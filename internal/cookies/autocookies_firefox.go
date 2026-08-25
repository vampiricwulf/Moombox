package cookies

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// Firefox user.js preferences to suppress first-run dialogs.
// Telemetry is explicitly disabled (healthreport upload off, toolkit
// telemetry off) in addition to bypassing the data-submission notification
// so we do not silently opt users into sending usage data just because
// Moombox launched their browser for cookie extraction.
const firefoxUserJS = `user_pref("browser.aboutwelcome.enabled", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.startup.homepage_override.mstone", "ignore");
user_pref("datareporting.policy.dataSubmissionPolicyBypassNotification", true);
user_pref("datareporting.healthreport.uploadEnabled", false);
user_pref("toolkit.telemetry.enabled", false);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
user_pref("browser.rights.3.shown", true);
`

// Firefox lock files that prevent launch when a previous session was force-killed.
var firefoxLockFiles = []string{"parent.lock", ".parentlock"}

// Firefox graceful-close timing. Pulled out of inline literals to make a tuning
// pass one place rather than scattered (audit reports/cookies.md #45).
const (
	firefoxGracefulCloseTimeout = 8 * time.Second        // overall budget for clean exit
	firefoxExitPollInterval     = 200 * time.Millisecond // poll cadence inside the wait loop
	// firefoxLaunchSpacing is the delay between consecutive Firefox
	// refresh launches so the previous instance has time to release
	// the profile directory's parent.lock. Audit reports/cookies.md #45.
	firefoxLaunchSpacing = 5 * time.Second
)

func (s *AutoCookieService) startFirefoxSetup(browser *DetectedBrowser, url string) error {
	if s.profileDirErr != nil {
		return s.profileDirErr
	}
	cleanFirefoxLockFiles(s.profileDir)

	// Write user.js to suppress first-run dialogs
	if err := os.WriteFile(filepath.Join(s.profileDir, "user.js"), []byte(firefoxUserJS), 0o644); err != nil {
		return fmt.Errorf("write user.js: %w", err)
	}

	cmd := exec.Command(browser.Path, "--new-instance", "--profile", s.profileDir, url)
	configureCmdSysProcAttr(cmd) // Linux: PR_SET_PDEATHSIG; Windows: no-op
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firefox: %w", err)
	}

	s.mu.Lock()
	s.setupProcess = cmd.Process
	s.mu.Unlock()

	// Monitor for exit. Compare against s.setupProcess so a stale wait from a
	// previous setup attempt cannot falsely flag the current one as exited
	// (audit reports/cookies.md #14).
	proc := cmd.Process
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in browser wait goroutine", "panic", r)
			}
		}()
		cmd.Wait()
		s.mu.Lock()
		if s.setupProcess == proc {
			s.browserExited = true
		}
		s.mu.Unlock()
	}()

	return nil
}

func (s *AutoCookieService) closeFirefoxGracefully() {
	s.mu.Lock()
	proc := s.setupProcess
	exited := s.browserExited
	s.mu.Unlock()

	if proc == nil || exited {
		time.Sleep(taskkillDrainDelay)
		return
	}

	s.logger.Debug("sending graceful close to Firefox")

	// Graceful close
	if isWindows() {
		exec.Command("taskkill", "/T", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		proc.Signal(os.Interrupt)
	}

	// Wait for clean exit, polling the wait-goroutine's browserExited flag.
	deadline := time.Now().Add(firefoxGracefulCloseTimeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		exited = s.browserExited
		s.mu.Unlock()
		if exited {
			s.logger.Debug("Firefox exited cleanly")
			time.Sleep(taskkillDrainDelay)
			return
		}
		time.Sleep(firefoxExitPollInterval)
	}

	// Force kill, then pause briefly so cookies.sqlite write-aheads land
	// before the next read attempt picks up the lock-released state.
	s.logger.Warn("Firefox did not exit gracefully, force killing")
	killProcessTree(proc)
	time.Sleep(cdpCloseFlushDelay)
}

func (s *AutoCookieService) refreshFirefox(ctx context.Context, browser *DetectedBrowser) (string, error) {
	if s.profileDirErr != nil {
		return "", s.profileDirErr
	}
	tempScreenshot := filepath.Join(s.profileDir, "refresh-screenshot.png")
	defer os.Remove(tempScreenshot)

	platforms := s.refreshPlatforms()
	for i, platform := range platforms {
		url := platformRefreshURLs[platform]

		// Wait between launches so Firefox fully releases the profile.
		// Ctx-aware so a shutdown during a multi-platform refresh doesn't
		// have to wait the spacing out — the ctx check below observes it.
		if i > 0 {
			s.logger.Info("waiting before next Firefox launch", "platform", platform, "spacing", firefoxLaunchSpacing)
			utils.Sleep(ctx, firefoxLaunchSpacing)
		}

		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		// Clean lock files right before launch — Firefox leaves parent.lock on exit
		cleanFirefoxLockFiles(s.profileDir)

		s.logger.Info("launching Firefox for cookie refresh", "platform", platform, "url", url)
		cmd := exec.Command(browser.Path, "--new-instance", "--screenshot", tempScreenshot, "--profile", s.profileDir, url)
		configureCmdSysProcAttr(cmd) // Linux: PR_SET_PDEATHSIG; Windows: no-op (Job Object in runWithTimeout)
		s.mu.Lock()
		s.refreshCmd = cmd
		s.mu.Unlock()
		startTime := time.Now()
		if err := runWithTimeout(cmd, processTimeout, s.logger); err != nil {
			s.logger.Warn("firefox "+platform+" refresh failed", "err", err, "elapsed", time.Since(startTime).Round(time.Millisecond))
		} else {
			s.logger.Info("firefox "+platform+" refresh completed", "elapsed", time.Since(startTime).Round(time.Millisecond))
		}
	}

	return readFirefoxCookies(s.profileDir)
}

// readFirefoxCookies extracts the relevant cookies from a Firefox profile
// directory and returns them in Netscape format.
//
// Each attempt SNAPSHOTS cookies.sqlite together with its -wal sidecar into a
// temp dir and queries the copy (see snapshotFirefoxCookieDB for why the
// sidecar is mandatory, why -shm is not, and why copying beats opening in
// place). If the snapshot itself fails — no temp space, an AV holding the
// file — the attempt falls back to querying the profile's database directly,
// which is exactly what this function did before, so a snapshot problem can
// never make this path worse than it was.
//
// Retrying the snapshot as well as the query is deliberate: a copy taken
// while Firefox is mid-flush can pair a main file and a -wal that disagree,
// and the fix for that is another copy, not another query of the same one.
func readFirefoxCookies(profileDir string) (string, error) {
	dbPath := filepath.Join(profileDir, "cookies.sqlite")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return "", fmt.Errorf("firefox %s not found in %q: %w", firefoxCookieDBName, profileDir, ErrCookieDBNotFound)
	}

	// Retry loop for SQLite WAL lock contention (Firefox may not have fully
	// released the lock) and for torn snapshots. Both are transient. A corrupt
	// or non-database file is permanent, and retrying it five times at 500ms
	// only delays the error the operator needs — so the loop breaks on
	// anything that is not retryable.
	const maxRetries = 5
	const retryBackoff = 500 * time.Millisecond

	var lines []string
	var lastErr error

	for attempt := range maxRetries {
		if attempt > 0 {
			time.Sleep(retryBackoff)
		}

		lines, lastErr = querySnapshotOrLive(profileDir, dbPath, attempt == maxRetries-1)
		if lastErr == nil || !isRetryableDBError(lastErr) {
			break
		}
	}

	if lastErr != nil {
		return "", classifyCookieDBError(fmt.Errorf("after %d attempts: %w", maxRetries, lastErr))
	}

	// Zero relevant cookies is NEVER a success. It is what a dropped -wal
	// looks like (the database opens cleanly and simply has nothing in it),
	// what a profile snapshotted out from under a live Firefox looks like,
	// and what pointing at the wrong profile directory looks like. Returning
	// an empty jar here would merge as a no-op and let a broken read hide
	// behind whatever cookies.txt already held.
	if len(lines) == 0 {
		return "", fmt.Errorf("%s in %q yielded no YouTube/Google/Twitch cookies — if the profile is in use, close the browser and try again: %w",
			firefoxCookieDBName, profileDir, ErrNoCookiesInProfile)
	}

	result := []string{
		"# Netscape HTTP Cookie File",
		"# Extracted by Moombox auto-cookie service",
		"",
	}
	result = append(result, lines...)

	return strings.Join(result, "\n") + "\n", nil
}

// firefoxMillisecondExpirySchema is the moz_cookies schema version
// (`PRAGMA user_version`) at which Firefox switched the expiry column from
// SECONDS to MILLISECONDS. Firefox 142 shipped the change; yt-dlp mirrors it
// in _extract_firefox_cookies.
//
// Ref: https://github.com/mozilla-firefox/firefox/commit/5869af852cd20425165837f6c2d9971f3efba83d
const firefoxMillisecondExpirySchema = 16

// firefoxSchemaVersion reads `PRAGMA user_version` from an open Firefox
// cookie database, which is how Firefox stamps the moz_cookies schema
// generation.
//
// A missing, unreadable, non-integer, or negative pragma degrades to 0 —
// i.e. "the pre-Firefox-142 seconds interpretation". The asymmetry is
// deliberate: guessing SECONDS on a milliseconds database inflates expiries
// by 1000x, which is exactly today's (pre-fix) behavior and merely keeps
// stale rows around; guessing MILLISECONDS on a seconds database divides
// every real expiry by 1000, throwing every cookie back to the 1970s so the
// merge pruner deletes the entire authenticated set. Only a positively-read
// version >= 16 may enable the conversion.
func firefoxSchemaVersion(db *sql.DB) int64 {
	var version sql.NullInt64
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0
	}
	if !version.Valid || version.Int64 < 0 {
		return 0
	}
	return version.Int64
}

// querySnapshotOrLive runs one read attempt against a private copy of the
// cookie database.
//
// The three snapshot outcomes are handled differently on purpose:
//
//   - TORN (a live Firefox wrote mid-copy): retry for a clean copy. On the
//     final attempt read the live database instead, because SQLite resolves
//     WAL consistency itself — a profile under constant write must not fail
//     where the pre-snapshot implementation succeeded.
//   - UNREADABLE SIDECAR (the SOURCE -wal cannot be stat'd or read):
//     propagate. Falling back to the live database would hit the same
//     unreadable -wal and could return a stale checkpointed set as if it
//     were current.
//   - ANYTHING ELSE (no temp space or an I/O error on OUR side of the copy,
//     an AV holding the file): fall back to the live database, which is
//     exactly what this code did before snapshots. Note that this covers a
//     temp-side failure on EITHER file — the profile is fine in that case,
//     so there is nothing to refuse.
func querySnapshotOrLive(profileDir, livePath string, finalAttempt bool) ([]string, error) {
	snapDir, cleanup, err := snapshotFirefoxCookieDB(profileDir)
	switch {
	case err == nil:
		defer cleanup()
		return queryFirefoxCookieDB(filepath.Join(snapDir, firefoxCookieDBName))
	case errors.Is(err, errSnapshotTorn) && !finalAttempt:
		return nil, err
	case errors.Is(err, ErrCookieDBUnreadable):
		return nil, err
	default:
		return queryFirefoxCookieDB(livePath)
	}
}

// isRetryableDBError reports whether another attempt could plausibly succeed.
// Lock contention clears and torn copies are a race; a corrupt file is not
// going to fix itself.
func isRetryableDBError(err error) bool {
	return isLockedDBError(err) || errors.Is(err, errSnapshotTorn)
}

// queryFirefoxCookieDB opens the Firefox cookie database and reads all cookies.
//
// The DSN needs the `file:` prefix — without it modernc/sqlite strips the
// entire query string and opens read-write with no busy timeout. mode=ro
// guarantees we never write into the browser's live database (a read-write
// open can perform WAL-index recovery writes), and
// `_pragma=busy_timeout(2000)` (modernc's parameter syntax) hands SQLite the
// wait itself: WAL-mode locks from a mid-flush Firefox return "database is
// locked" almost immediately under the default 0ms busy timeout, and the
// caller's 5×500ms retry loop doesn't help if each open errors before even
// waiting.
func queryFirefoxCookieDB(dbPath string) ([]string, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, fmt.Errorf("open cookies.sqlite: %w", err)
	}
	defer db.Close()

	// Read the schema generation BEFORE the rows: it decides whether the
	// expiry column is seconds or milliseconds (Firefox 142 / schema 16).
	schemaVersion := firefoxSchemaVersion(db)

	rows, err := db.Query("SELECT name, value, host, path, expiry, isHttpOnly, isSecure FROM moz_cookies")
	if err != nil {
		return nil, fmt.Errorf("query cookies: %w", err)
	}
	defer rows.Close()

	var collected []extractedCookie
	for rows.Next() {
		var name, value, host, cookiePath string
		var isHttpOnly, isSecure int64
		// expiry is nullable: Firefox leaves it NULL for some rows, and a
		// plain int64 destination turns that into a scan error, which the
		// `continue` below would silently swallow — dropping the cookie
		// entirely. Upstream keeps such rows with no expiry at all
		// (`expiry is not None`), so 0 (the Netscape session-cookie
		// sentinel, which rowExpired never prunes) is the faithful mapping.
		var expiry sql.NullInt64
		if err := rows.Scan(&name, &value, &host, &cookiePath, &expiry, &isHttpOnly, &isSecure); err != nil {
			continue
		}

		expirySeconds := int64(0)
		if expiry.Valid {
			expirySeconds = expiry.Int64
			if schemaVersion >= firefoxMillisecondExpirySchema {
				expirySeconds /= 1000
			}
		}

		collected = append(collected, extractedCookie{
			domain:   host,
			httpOnly: isHttpOnly != 0,
			path:     cookiePath,
			secure:   isSecure != 0,
			expiry:   expirySeconds,
			name:     name,
			value:    value,
		})
	}
	// A mid-iteration failure (lock contention while Firefox flushes — the
	// exact case the caller's retry loop exists for) ends the loop with
	// Next()==false; without this check a PARTIAL cookie set would be
	// returned as success and merged over the full file.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cookies: %w", err)
	}

	return deduplicateAndFormat(collected), nil
}

func cleanFirefoxLockFiles(profileDir string) {
	// Skip recently-touched locks so we don't yank a parent.lock out from
	// under a live Firefox instance (audit reports/cookies.md #9).
	for _, name := range firefoxLockFiles {
		removeStaleLock(filepath.Join(profileDir, name))
	}
}

func runWithTimeout(cmd *exec.Cmd, timeout time.Duration, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) error {
	// Create a Job Object so all child processes (including reparented ones)
	// are killed when we close the job handle.
	job, jobErr := newProcessJob()
	if jobErr != nil {
		logger.Warn("failed to create job object", "err", jobErr)
	} else if job != nil {
		logger.Debug("created job object for process tracking")
	}
	defer func() {
		if job != nil {
			logger.Debug("closing job object (killing all tracked processes)")
			job.close()
		}
	}()

	if err := cmd.Start(); err != nil {
		return err
	}
	logger.Debug("process started", "pid", cmd.Process.Pid)

	// Assign immediately after start so children are tracked from the beginning
	if job != nil {
		if err := job.assign(cmd.Process); err != nil {
			logger.Warn("failed to assign process to job object", "pid", cmd.Process.Pid, "err", err)
		} else {
			logger.Debug("assigned process to job object", "pid", cmd.Process.Pid)
		}
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("panic in firefox wait goroutine", "panic", r)
			}
		}()
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		logger.Debug("process exited normally", "pid", cmd.Process.Pid, "err", err)
		return err
	case <-time.After(timeout):
		logger.Warn("process timed out, killing", "pid", cmd.Process.Pid, "timeout", timeout)
		// Closing the job handle kills all processes in the job.
		// Also try direct kill as a belt-and-suspenders approach.
		killProcessTree(cmd.Process)
		// Wait briefly for reap, but don't block forever if kill failed
		select {
		case <-done:
			logger.Debug("process reaped after kill", "pid", cmd.Process.Pid)
		case <-time.After(5 * time.Second):
			logger.Warn("process did not exit after kill, forcing", "pid", cmd.Process.Pid)
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
		return fmt.Errorf("process timed out after %s", timeout)
	}
}
