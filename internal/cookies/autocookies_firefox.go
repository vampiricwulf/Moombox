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
		err := runWithTimeout(ctx, cmd, processTimeout, s.restoreRefreshSlot, s.logger)
		elapsed := time.Since(startTime).Round(time.Millisecond)
		switch {
		case errors.Is(err, errBrowserDrainTimeout):
			// Degrade, do not abort. The browser was alive and has just been
			// killed, so the profile may be half-written — but the existing
			// cookies.txt is untouched and readFirefoxCookies below may still
			// find a usable set. Same shape as the ErrNoCookiesInProfile
			// handling in RefreshCookies: say what happened, keep going.
			s.logger.Warn("firefox "+platform+" refresh did not finish in time; the browser was killed mid-load",
				"elapsed", elapsed, "budget", processTimeout)
		case err != nil:
			s.logger.Warn("firefox "+platform+" refresh failed", "err", err, "elapsed", elapsed)
		default:
			s.logger.Info("firefox "+platform+" refresh completed", "elapsed", elapsed)
		}
	}

	netscape, stats, err := readFirefoxCookies(s.profileDir)
	s.logFirefoxReadStats(stats)
	return netscape, err
}

// restoreRefreshSlot puts the claim SENTINEL back once the launched browser
// process has been reaped.
//
// Not nil: RefreshCookies still has its critical tail to run (merge → atomic
// write → jar reload → auth verify → meta save), and clearing the slot here
// would let a concurrent RefreshCookies or StartSetup launch a second browser
// against the same profile mid-write. The outer defer in RefreshCookies
// releases the slot for real. Identical reasoning, and identical three lines,
// to refreshChromium's restore (autocookies_chromium.go).
//
// The restore matters MORE than it looks: while the real cmd sits in the slot
// with a reaped Process, killRefreshProcess would taskkill /F /T a PID
// Windows may already have recycled onto something else. Before the
// drain-wait that window was ~200ms; the drain stretches it to the whole
// launch budget.
func (s *AutoCookieService) restoreRefreshSlot() {
	s.mu.Lock()
	s.refreshCmd = &exec.Cmd{}
	s.mu.Unlock()
}

// logFirefoxReadStats reports what a moz_cookies read had to work around.
// The read itself has no logger by design (see firefoxReadStats), so this is
// where a degraded schema probe or a dropped row stops being invisible.
func (s *AutoCookieService) logFirefoxReadStats(stats firefoxReadStats) {
	if s.logger == nil || stats.rows == 0 {
		// Nothing was read; whatever error came back says why, and a
		// zero-value stats line would only add noise.
		return
	}
	switch {
	case !stats.schemaKnown:
		s.logger.Warn("firefox cookie database has no readable schema version — assuming pre-142 (seconds) expiry units",
			"rows", stats.rows)
	case stats.schemaVersion > firefoxMaxKnownSchema:
		s.logger.Warn("firefox cookie database is newer than this build has been checked against — expiry handling may be wrong",
			"schema_version", stats.schemaVersion, "max_known", firefoxMaxKnownSchema, "rows", stats.rows)
	default:
		s.logger.Debug("read firefox cookie database",
			"schema_version", stats.schemaVersion, "rows", stats.rows)
	}
	if stats.unusable() > 0 {
		s.logger.Warn("skipped unusable moz_cookies rows",
			"no_name", stats.droppedNoName, "no_host", stats.droppedNoHost, "scan_errors", stats.scanErrors, "rows", stats.rows)
	}
	if stats.defaulted > 0 {
		s.logger.Debug("filled in NULL moz_cookies columns", "rows_defaulted", stats.defaulted)
	}
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
// The read stats of the attempt that produced the result are returned
// alongside it so the caller — which owns a logger, unlike anything down
// this chain — can report what the read had to work around: a schema
// version it does not recognise, rows it could not use.
func readFirefoxCookies(profileDir string) (string, firefoxReadStats, error) {
	var stats firefoxReadStats

	dbPath := filepath.Join(profileDir, "cookies.sqlite")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return "", stats, fmt.Errorf("firefox %s not found in %q: %w", firefoxCookieDBName, profileDir, ErrCookieDBNotFound)
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

		lines, stats, lastErr = querySnapshotOrLive(profileDir, dbPath, attempt == maxRetries-1)
		if lastErr == nil || !isRetryableDBError(lastErr) {
			break
		}
	}

	if lastErr != nil {
		return "", stats, classifyCookieDBError(fmt.Errorf("after %d attempts: %w", maxRetries, lastErr))
	}

	// Zero relevant cookies is NEVER a success. It is what a dropped -wal
	// looks like (the database opens cleanly and simply has nothing in it),
	// what a profile snapshotted out from under a live Firefox looks like,
	// and what pointing at the wrong profile directory looks like. Returning
	// an empty jar here would merge as a no-op and let a broken read hide
	// behind whatever cookies.txt already held.
	if len(lines) == 0 {
		return "", stats, fmt.Errorf("%s in %q yielded no YouTube/Google/Twitch cookies — if the profile is in use, close the browser and try again: %w",
			firefoxCookieDBName, profileDir, ErrNoCookiesInProfile)
	}

	result := []string{
		"# Netscape HTTP Cookie File",
		"# Extracted by Moombox auto-cookie service",
		"",
	}
	result = append(result, lines...)

	return strings.Join(result, "\n") + "\n", stats, nil
}

// firefoxMillisecondExpirySchema is the moz_cookies schema version
// (`PRAGMA user_version`) at which Firefox switched the expiry column from
// SECONDS to MILLISECONDS. Firefox 142 shipped the change; yt-dlp mirrors it
// in _extract_firefox_cookies.
//
// Ref: https://github.com/mozilla-firefox/firefox/commit/5869af852cd20425165837f6c2d9971f3efba83d
const firefoxMillisecondExpirySchema = 16

// firefoxMaxKnownSchema is the highest moz_cookies schema generation this
// build has been checked against. Anything above it may have moved a column
// or changed a unit the way Firefox 142 changed expiry, so it is worth
// saying out loud. yt-dlp warns at the same boundary
// (MAX_SUPPORTED_DB_SCHEMA_VERSION in _extract_firefox_cookies).
const firefoxMaxKnownSchema = 17

// firefoxReadStats is what one read of moz_cookies had to work around.
//
// It is returned rather than logged because nothing down this call chain
// owns a logger, and threading one into four pure functions to say
// "user_version came back empty" buys less than handing the facts back to
// the layer that already has a logger — which is also the only form a test
// can assert on.
type firefoxReadStats struct {
	schemaVersion int64 // PRAGMA user_version, or 0 when the probe failed
	schemaKnown   bool  // the probe actually read a version
	rows          int   // rows moz_cookies handed back
	scanErrors    int   // rows whose Scan failed for a reason other than a NULL we handle
	droppedNoName int   // NULL/empty name — nothing to send, nothing to match
	droppedNoHost int   // NULL/empty host — no domain to attach the cookie to
	defaulted     int   // rows where a NULL non-identity column was filled in
}

// unusable is the count of rows this read could not turn into a cookie.
func (s firefoxReadStats) unusable() int {
	return s.scanErrors + s.droppedNoName + s.droppedNoHost
}

// firefoxSchemaVersion reads `PRAGMA user_version` from an open Firefox
// cookie database, which is how Firefox stamps the moz_cookies schema
// generation. The second return says whether a version was actually read.
//
// A missing, unreadable, non-integer, or negative pragma degrades to 0 —
// i.e. "the pre-Firefox-142 seconds interpretation". The asymmetry is
// deliberate: guessing SECONDS on a milliseconds database inflates expiries
// by 1000x, which is exactly today's (pre-fix) behavior and merely keeps
// stale rows around; guessing MILLISECONDS on a seconds database divides
// every real expiry by 1000, throwing every cookie back to the 1970s so the
// merge pruner deletes the entire authenticated set. Only a positively-read
// version >= 16 may enable the conversion.
//
// The degrade stays silent HERE and is reported by the caller: this
// function has no logger and should not grow one just to say that a
// one-line pragma came back empty.
func firefoxSchemaVersion(db *sql.DB) (int64, bool) {
	var version sql.NullInt64
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, false
	}
	if !version.Valid || version.Int64 < 0 {
		return 0, false
	}
	return version.Int64, true
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
func querySnapshotOrLive(profileDir, livePath string, finalAttempt bool) ([]string, firefoxReadStats, error) {
	snapDir, cleanup, err := snapshotFirefoxCookieDB(profileDir)
	switch {
	case err == nil:
		defer cleanup()
		return queryFirefoxCookieDB(filepath.Join(snapDir, firefoxCookieDBName))
	case errors.Is(err, errSnapshotTorn) && !finalAttempt:
		return nil, firefoxReadStats{}, err
	case errors.Is(err, ErrCookieDBUnreadable):
		return nil, firefoxReadStats{}, err
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
func queryFirefoxCookieDB(dbPath string) ([]string, firefoxReadStats, error) {
	var stats firefoxReadStats

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, stats, fmt.Errorf("open cookies.sqlite: %w", err)
	}
	defer db.Close()

	// Read the schema generation BEFORE the rows: it decides whether the
	// expiry column is seconds or milliseconds (Firefox 142 / schema 16).
	schemaVersion, schemaKnown := firefoxSchemaVersion(db)
	stats.schemaVersion, stats.schemaKnown = schemaVersion, schemaKnown

	rows, err := db.Query("SELECT name, value, host, path, expiry, isHttpOnly, isSecure FROM moz_cookies")
	if err != nil {
		return nil, stats, fmt.Errorf("query cookies: %w", err)
	}
	defer rows.Close()

	var collected []extractedCookie
	for rows.Next() {
		// EVERY column here is nullable — moz_cookies declares none of them
		// NOT NULL — and a bare Go destination turns any NULL into a scan
		// error, which the `continue` below swallows: the whole cookie
		// disappears with nothing said. That is how a NULL expiry used to
		// drop rows, and name/value/host/path/isHttpOnly/isSecure had the
		// identical hole. Scanning through the Null* types means a NULL is
		// a value we decide about, per column, instead of a row we lose.
		//
		// Upstream (yt-dlp _extract_firefox_cookies) passes the raw values
		// straight into http.cookiejar.Cookie, so it does not guard these
		// either; parity is not the argument here, not silently losing
		// credentials is.
		var name, value, host, cookiePath sql.NullString
		var expiry, isHttpOnly, isSecure sql.NullInt64
		stats.rows++
		if err := rows.Scan(&name, &value, &host, &cookiePath, &expiry, &isHttpOnly, &isSecure); err != nil {
			stats.scanErrors++
			continue
		}

		// A cookie with no NAME cannot be sent and cannot be matched; one
		// with no HOST has no domain to attach to and would be written as a
		// row with an empty domain field. Both are genuinely unusable, so
		// they are dropped — but counted, which is the whole difference from
		// before.
		if !name.Valid || name.String == "" {
			stats.droppedNoName++
			continue
		}
		if !host.Valid || host.String == "" {
			stats.droppedNoHost++
			continue
		}

		// The rest have a faithful default and must NOT cost the row:
		//   value      NULL -> "" (an empty cookie value is legal)
		//   path       NULL -> "/" (upstream's path_specified=False, i.e. no
		//                           path restriction; "/" is how that is
		//                           spelled in a Netscape file)
		//   expiry     NULL -> 0, the Netscape session-cookie sentinel that
		//                      rowExpired never prunes (upstream's
		//                      `expiry is not None`)
		//   isHttpOnly NULL -> false; it only decides the #HttpOnly_ prefix
		//   isSecure   NULL -> TRUE, deliberately not false. The field is
		//                      unknown, and the two guesses are not
		//                      symmetric: marking a cookie secure withholds
		//                      it from a plaintext request, marking it
		//                      insecure would send a session credential over
		//                      one. Our own jar ignores the field and all
		//                      our traffic is HTTPS, so the safe guess is
		//                      free here and only ever helps another
		//                      consumer of the file.
		if !value.Valid || !cookiePath.Valid || !expiry.Valid || !isHttpOnly.Valid || !isSecure.Valid {
			stats.defaulted++
		}
		rowPath := cookiePath.String
		if !cookiePath.Valid || rowPath == "" {
			rowPath = "/"
		}
		secure := true
		if isSecure.Valid {
			secure = isSecure.Int64 != 0
		}

		expirySeconds := int64(0)
		if expiry.Valid {
			expirySeconds = expiry.Int64
			if schemaVersion >= firefoxMillisecondExpirySchema {
				expirySeconds /= 1000
			}
		}

		collected = append(collected, extractedCookie{
			domain:   host.String,
			httpOnly: isHttpOnly.Int64 != 0,
			path:     rowPath,
			secure:   secure,
			expiry:   expirySeconds,
			name:     name.String,
			value:    value.String,
		})
	}
	// A mid-iteration failure (lock contention while Firefox flushes — the
	// exact case the caller's retry loop exists for) ends the loop with
	// Next()==false; without this check a PARTIAL cookie set would be
	// returned as success and merged over the full file.
	if err := rows.Err(); err != nil {
		return nil, stats, fmt.Errorf("iterate cookies: %w", err)
	}

	return deduplicateAndFormat(collected), stats, nil
}

func cleanFirefoxLockFiles(profileDir string) {
	// Skip recently-touched locks so we don't yank a parent.lock out from
	// under a live Firefox instance (audit reports/cookies.md #9).
	for _, name := range firefoxLockFiles {
		removeStaleLock(filepath.Join(profileDir, name))
	}
}

// errBrowserDrainTimeout is returned by runWithTimeout when the launcher
// process was reaped but the Job Object still held live processes once the
// launch budget ran out — the browser was still working, or hung, and the
// deferred job.close() is about to kill it.
//
// It is deliberately DISTINCT from a nil return. Returning nil would report a
// hung browser as a refresh that merely took the whole budget: the exact
// silent failure the drain-wait exists to fix, only slower and harder to see.
var errBrowserDrainTimeout = errors.New("browser did not finish within the launch budget")

// shouldKeepWaiting decides whether the job-drain loop takes another lap:
// processes are still alive in the job AND the launch budget has not run out.
//
// Extracted so the decision is testable without a Job Object — the syscall
// that produces `active` is the part this package cannot exercise in a unit
// test, and the part that was actually wrong.
func shouldKeepWaiting(active int, elapsed, budget time.Duration) bool {
	return active > 0 && elapsed < budget
}

// drainJob waits for every process in the job to exit.
//
// This is the whole point of the Firefox fix. cmd.Wait() returning tells us
// only that the LAUNCHER exited: Firefox (and Waterfox / LibreWolf / Zen)
// hand off to a separate browser process and the launcher exits in ~170ms.
// Returning at that moment runs the caller's deferred job.close(), whose
// KILL_ON_JOB_CLOSE kills the real browser mid-page-load — measured, and the
// reason every Firefox-family cookie refresh silently did nothing.
//
// The budget is shared with the launch (startedAt is stamped before
// cmd.Start), not restarted here, so a slow start eats into the drain rather
// than granting a second full timeout.
//
// Three ways out:
//   - the job empties → nil, the browser finished on its own;
//   - the budget expires with processes alive → errBrowserDrainTimeout, and
//     the caller's job.close() kills them;
//   - the query fails → nil, degrading to the pre-drain behaviour. That is
//     bad but known; spinning on a failing syscall for the whole budget is
//     worse.
func drainJob(ctx context.Context, job *processJob, startedAt time.Time, budget time.Duration, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) error {
	if job == nil {
		return nil
	}
	for polls := 0; ; polls++ {
		active, qErr := job.activeProcesses()
		if qErr != nil {
			logger.Warn("could not query job process count; not waiting for the browser to finish", "err", qErr, "polls", polls)
			return nil
		}
		elapsed := time.Since(startedAt)
		if !shouldKeepWaiting(active, elapsed, budget) {
			if active > 0 {
				logger.Warn("browser still running when the launch budget expired; killing it",
					"active", active, "budget", budget, "polls", polls)
				return errBrowserDrainTimeout
			}
			logger.Debug("browser finished; job drained",
				"elapsed", elapsed.Round(time.Millisecond), "polls", polls)
			return nil
		}
		select {
		case <-ctx.Done():
			// The caller (refreshOverallBudget, or shutdown) gave up. Without
			// this the drain is up to a full budget of un-cancellable wait
			// per launch.
			logger.Warn("cancelled while waiting for the browser to finish",
				"err", ctx.Err(), "active", active, "polls", polls)
			return ctx.Err()
		case <-time.After(killProcessTreePollDelay):
		}
	}
}

// runWithTimeout starts cmd inside a Job Object, waits for the launched
// process AND for the job to empty, and kills the tree on the way out.
//
// onLauncherReaped, when non-nil, is called the instant cmd.Wait() returns —
// before the drain, which can run for the rest of the budget. The caller uses
// it to stop advertising a PID that no longer exists (see refreshFirefox).
func runWithTimeout(ctx context.Context, cmd *exec.Cmd, timeout time.Duration, onLauncherReaped func(), logger interface {
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

	// Stamped before Start so the launch and the drain share ONE budget
	// rather than the drain quietly starting a second one.
	startedAt := time.Now()
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
		logger.Debug("launcher process exited", "pid", cmd.Process.Pid, "err", err)
		// The PID is reaped as of now, so stop advertising it as something to
		// kill: killProcessTree on a reaped PID can land on whatever Windows
		// recycled it onto, and the drain below holds that window open for up
		// to the whole budget instead of ~200ms.
		if onLauncherReaped != nil {
			onLauncherReaped()
		}
		// Do NOT return here. cmd.Wait() only says the LAUNCHER exited;
		// returning closes the job and kills the browser it handed off to.
		drainErr := drainJob(ctx, job, startedAt, timeout, logger)
		if err != nil {
			// A launcher that itself failed is the more direct diagnosis;
			// the drain outcome after it is noise.
			return err
		}
		return drainErr
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
