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
	configureCmdSysProcAttr(cmd) // Linux: PR_SET_PDEATHSIG + Setpgid (the group the reap counts); Windows: no-op (Job Object below)

	// A job, created before the launch, for the two reasons the Chromium path
	// has one — the browser dies with a crashed Moombox (on Windows; on Linux
	// that is Pdeathsig's job for the direct child), and cleanupLocked can
	// finish off one the user left behind — plus a third that is specific to
	// this family and is why the omission mattered: the Firefox launcher hands
	// off to the real browser and EXITS IN ~170ms, so cmd.Wait() returning says
	// nothing about whether a browser is still on screen. The job is the only
	// thing that can tell an abandoned setup from a live one. Without it
	// setupBrowserGone answered "no idea" for every Firefox setup, and the
	// abandoned-setup reap never fired on the default path for anyone whose
	// browser is Firefox-family.
	//
	// WINDOWS AND LINUX. newProcessJob is a Job Object on Windows and a process
	// group on Linux, and the reap fires on both. It is still a no-op stub on
	// darwin and the fallback build, where the reap stays dead — see
	// setupBrowserGone's fourth case.
	job, jobErr := newProcessJob()
	if jobErr != nil {
		s.logger.Warn("failed to create job object for firefox setup", "err", jobErr)
	}

	if err := cmd.Start(); err != nil {
		if job != nil {
			job.close()
		}
		return fmt.Errorf("start firefox: %w", err)
	}

	// Assign immediately, before the launcher can hand off: a child created
	// before the assign lands is never tracked by the job. Returns nil if the
	// assign failed — see trackedSetupJob.
	job = s.trackedSetupJob(job, cmd.Process, "firefox")

	s.mu.Lock()
	s.setupProcess = cmd.Process
	// Closes any handle a previous attempt left behind; see the guard's doc.
	s.adoptSetupJobLocked(job)
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
			// Starts the retention grace. Until this stamp existed the exit
			// was recorded and then ignored: setupProcess stayed non-nil
			// forever and every consumer kept reporting a setup in progress.
			s.setupRetainedSince = time.Now()
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

// refreshFirefox drives one browser launch per platform and reads the profile
// afterwards.
//
// The middle return is the ACTED verdict: did the browser this function
// launched actually run? It exists because the caller's notion of success is
// "cookies.txt authenticates something", and cookies.txt is kept alive by the
// independent 30-minute RefreshService whether or not a browser ever ran — so
// a refresh that did nothing at all still verified, and still logged "cookie
// refresh succeeded". See browserLaunchActed for what the verdict is made of.
//
// It is an AND across every launch this pass makes: one launch that could not
// be confirmed leaves the pass unable to claim it renewed that platform, and
// the merge downstream cannot tell the difference afterwards. Note the shape of
// that — it says the pass has no proof, not that the browser did nothing.
func (s *AutoCookieService) refreshFirefox(ctx context.Context, browser *DetectedBrowser) (string, bool, error) {
	if s.profileDirErr != nil {
		return "", false, s.profileDirErr
	}
	tempScreenshot := filepath.Join(s.profileDir, "refresh-screenshot.png")
	defer os.Remove(tempScreenshot)
	cookieDB := filepath.Join(s.profileDir, firefoxCookieDBName)

	platforms := s.refreshPlatforms()
	// Vacuous truth is the wrong default here: no launches means nothing was
	// refreshed, not "every launch succeeded". RefreshCookies gates this call
	// on there being at least one platform, so this only guards a future
	// caller that does not.
	allActed := len(platforms) > 0
	for i, platform := range platforms {
		url := platformRefreshURLs[platform]

		// Wait between launches so Firefox fully releases the profile.
		// Ctx-aware so a shutdown during a multi-platform refresh doesn't
		// have to wait the spacing out — the ctx check below observes it.
		if i > 0 {
			// s.firefoxLaunchSpacing is the injection seam (Arc 8 7(d)): zero
			// (services built via struct literal, not NewAutoCookieService)
			// falls back to the production const, same convention as
			// detectBrowser's nil fallback.
			spacing := s.firefoxLaunchSpacing
			if spacing <= 0 {
				spacing = firefoxLaunchSpacing
			}
			s.logger.Info("waiting before next Firefox launch", "platform", platform, "spacing", spacing)
			utils.Sleep(ctx, spacing)
		}

		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}

		// Clean lock files right before launch — Firefox leaves parent.lock on exit
		cleanFirefoxLockFiles(s.profileDir)

		// Clear the PREVIOUS launch's proof before this one starts. The defer
		// above is function-scoped, so without this the YouTube screenshot
		// survives into the Twitch launch and every platform after the first
		// reads as "acted" no matter what its browser did.
		clearBrowserRenderProof(tempScreenshot)
		// Corroboration only — deliberately NOT a second gate. A browser set
		// to clear cookies on close renders the page and writes nothing, so
		// gating on the profile moving would report a working refresh as a
		// failure. It is worth SAYING alongside the verdict, nothing more.
		beforeDB := fingerprintCookieDB(cookieDB)

		s.logger.Info("launching Firefox for cookie refresh", "platform", platform, "url", url)
		cmd := exec.Command(browser.Path, "--new-instance", "--screenshot", tempScreenshot, "--profile", s.profileDir, url)
		configureCmdSysProcAttr(cmd) // Linux: PR_SET_PDEATHSIG + Setpgid (the group the reap counts); Windows: no-op (Job Object in runWithTimeout)
		s.mu.Lock()
		s.refreshCmd = cmd
		s.mu.Unlock()
		startTime := time.Now()
		err := runWithTimeout(ctx, cmd, processTimeout, s.restoreRefreshSlot, s.logger)
		elapsed := time.Since(startTime).Round(time.Millisecond)

		rendered := browserRendered(tempScreenshot)
		acted := browserLaunchActed(err, rendered)
		if !acted {
			allActed = false
		}
		profileWritten := fingerprintsDiffer(beforeDB, fingerprintCookieDB(cookieDB))

		switch {
		case errors.Is(err, errBrowserDrainTimeout):
			// Degrade, do not abort. The browser was alive and has just been
			// killed, so the profile may be half-written — but the existing
			// cookies.txt is untouched and readFirefoxCookies below may still
			// find a usable set. Same shape as the ErrNoCookiesInProfile
			// handling in RefreshCookies: say what happened, keep going.
			s.logger.Warn("firefox "+platform+" refresh did not finish in time; the browser was killed mid-load",
				"elapsed", elapsed, "budget", processTimeout, "profile_written", profileWritten)
		case err != nil:
			s.logger.Warn("firefox "+platform+" refresh failed", "err", err, "elapsed", elapsed, "profile_written", profileWritten)
		case !rendered:
			// Nothing reported an error and the launcher was reaped, but no
			// screenshot had appeared by the time the launch returned.
			//
			// The message says exactly that and no more. What the browser
			// actually did is not observable from here: with no count to drain
			// (darwin and the fallback build; a job we failed to create or
			// assign; a Linux group that could not be adopted) a detached
			// browser may render moments after this stat, and profile_written can come
			// back true off a flush that landed in between. Asserting "it never
			// rendered" or "the profile was not refreshed" would be the same
			// kind of unearned claim this whole change exists to remove — one
			// that happens to point the other way.
			//
			// This arm is where the two NIL-returning failures land, and both
			// used to log "refresh completed" at Info:
			//   - the job-count query failed, so drainJob stopped waiting
			//     (autocookies_firefox.go, drainJob) and the deferred
			//     job.close() killed the browser mid-load — one line after a
			//     Warn saying the query failed;
			//   - job.assign failed, so the browser is outside the job, the
			//     drain sees an empty job on its first lap, and we read the
			//     profile while the page is still loading.
			// Keying the verdict off errBrowserDrainTimeout alone would have
			// reopened the silent-success hole through both of these.
			s.logger.Warn("firefox "+platform+" refresh could not be confirmed — no screenshot was written by the time the launch returned",
				"elapsed", elapsed, "profile_written", profileWritten)
		default:
			s.logger.Info("firefox "+platform+" refresh completed", "elapsed", elapsed, "profile_written", profileWritten)
			// Observability only: does not touch acted, allActed, or any
			// return value. See refreshLooksImplausiblyFast.
			if refreshLooksImplausiblyFast(elapsed, acted) {
				s.logger.Debug("firefox "+platform+" refresh rendered unusually fast; the page may not have fully loaded",
					"elapsed", elapsed, "floor", minPlausibleBrowserRefresh)
			}
		}
	}

	netscape, stats, err := readFirefoxCookies(s.profileDir)
	s.logFirefoxReadStats(stats)
	return netscape, allActed, err
}

// clearBrowserRenderProof deletes the screenshot from the previous launch.
//
// MUST run before every launch, not once per refresh: the artifact lives at a
// fixed path inside the profile, so a surviving file from the YouTube launch is
// indistinguishable from one the Twitch launch just wrote.
//
// Errors are ignored on purpose. "Not there" is the normal case, and a removal
// that genuinely fails leaves a stale file that browserRendered will read as
// proof — which is the pre-existing behaviour, not a new failure, and refusing
// the refresh over it would be worse.
func clearBrowserRenderProof(path string) {
	os.Remove(path)
}

// browserRendered reports whether the launch that just finished wrote a
// screenshot.
//
// This is the decisive signal, and it is the one the isolation experiment used:
// under a Job Object the identical argv produced no screenshot and no profile
// write, while a plain exec produced both. It is per-launch, needs no WAL
// reasoning, and — unlike anything derived from cookies.txt — cannot be
// satisfied by the independent 30-minute RefreshService.
//
// A zero-length file does not count: that is what a browser killed part-way
// through writing leaves behind, and it is not evidence that anything rendered.
//
// Only meaningful directly after clearBrowserRenderProof; see its comment.
func browserRendered(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

// browserLaunchActed folds one launch's outcome into the single verdict the
// refresh tail needs: did this browser do the work the refresh is about to take
// credit for?
//
// Both halves are required, and each covers failures the other cannot see:
//
//   - launchErr != nil is the browser that could not start, that the launch
//     budget killed mid-load (errBrowserDrainTimeout), or that the caller
//     cancelled. A screenshot from earlier in the same launch does not redeem
//     any of those — the profile is half-written at best.
//   - rendered == false is every failure that returns NO error: a job query
//     that failed, a job assign that failed, and on platforms with no Job
//     Object at all a launcher handoff we never waited for. These are exactly
//     the paths that logged "refresh completed" while doing nothing.
func browserLaunchActed(launchErr error, rendered bool) bool {
	return launchErr == nil && rendered
}

// minPlausibleBrowserRefresh is a Debug-level sanity threshold, not a second
// success gate. browserLaunchActed's screenshot check is the decisive verdict
// on whether a launch acted, and this constant plays no part in it — see
// refreshLooksImplausiblyFast, which never changes browserActed, a return
// value, or control flow. It only decides whether a launch that already
// reported "acted" is worth a Debug note.
//
// Measured against the owner's Waterfox, run against a copy of the real
// profile, with A0.3's drain-wait in place: a no-op launch (the browser
// killed before it could paint anything) completed in 160-211 ms; a working
// launch that actually rendered the page completed in 3.08 s (the drain
// finishing in 59 polls). 1 s sits ~5x above the no-op and ~3x below the
// working case — margin on both sides, so do not retune it toward either
// observed number without new measurements of your own.
const minPlausibleBrowserRefresh = 1 * time.Second

// refreshLooksImplausiblyFast is a pure, table-testable predicate behind the
// Debug note logged when a launch that already reported ACTED (a screenshot
// exists, no launch error) finished suspiciously quickly.
//
// acted is a parameter rather than re-derived here on purpose: a launch that
// was already reported not-acted gets no second complaint — that failure is
// already loud through browserLaunchActed, and layering a heuristic warning
// on top of a fact would just be noise. This function only has something to
// say about the launches the fact already credited.
//
// On darwin and the fallback build there is no count at all, so drainJob
// returns on lap zero (see its doc comment) and a launch that genuinely worked
// can still read as fast here. Linux has a count since the process-group reap,
// but no timing has been recorded there, so the threshold is a Windows number.
// The Debug message at the call site is worded as an observation, not
// a conclusion, so that routine case does not read as an assertion of
// failure.
func refreshLooksImplausiblyFast(elapsed time.Duration, acted bool) bool {
	return acted && elapsed < minPlausibleBrowserRefresh
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
//
// Called on every runWithTimeout exit that OBSERVES a reap, which is not quite
// every exit — see the note on runWithTimeout for the one that deliberately
// leaves the PID published.
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
// deferred closeLaunchJob is about to kill it.
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
// Returning at that moment runs the caller's deferred closeLaunchJob, whose
// kill (KILL_ON_JOB_CLOSE on Windows, the group kill on Linux) lands on the
// real browser mid-page-load — measured on Windows, and the
// reason every Firefox-family cookie refresh silently did nothing.
//
// The budget is shared with the launch (startedAt is stamped before
// cmd.Start), not restarted here, so a slow start eats into the drain rather
// than granting a second full timeout.
//
// Three ways out:
//   - the job empties → nil, the browser finished on its own;
//   - the budget expires with processes alive → errBrowserDrainTimeout, and
//     the caller's closeLaunchJob kills them;
//   - the query fails → nil, degrading to the pre-drain behaviour. That is
//     bad but known; spinning on a failing syscall for the whole budget is
//     worse.
//
// VERIFICATION STATUS — two browsers, one platform, both on Windows.
//
//	Waterfox  2026-08-25  drained in 2.848s over 53 polls, 6 YouTube cookies
//	Firefox   2026-08-25  drained in 1.734s over 32 polls, 6 YouTube cookies
//
// Both ran against a throwaway profile via the live gate below; the killed
// control (job closed the instant cmd.Wait() returned) came back in 146-167ms
// having written a cookies.sqlite with ZERO rows, on both. The earlier
// Waterfox figure — 3.082s over 59 polls against a copy of a real profile —
// still holds. LibreWolf and Zen remain UNVERIFIED, as does every non-Windows
// platform: darwin and the fallback build have no job to drain at all, and
// Linux has had a process group to drain since the process-group arc with no
// timing recorded there.
//
// Those elapsed times and poll counts are observations of these machines, NOT
// a healthy band: a clean pass on different hardware on 2026-08-26 drained in
// 13.96s over 276 polls, same successful outcome, no error. The signal that
// something is actually wrong is errBrowserDrainTimeout below — nothing about
// the poll count on its own.
//
// The risk if one behaves differently: this waits for the job to become EMPTY,
// which is a stronger condition than "the page finished loading". A browser
// that leaves any process alive in the job — a background updater, a crash
// reporter, a lingering content process — would burn the full processTimeout
// budget on every refresh and return errBrowserDrainTimeout, turning working
// refreshes into reported failures. Observed on neither Waterfox nor Firefox,
// which is the point of having run two: the empty-job condition is not a
// Waterfox quirk.
//
// Accepted rather than defended against, on three grounds: it is bounded by the
// budget, it degrades rather than aborts, and the poll count and elapsed time
// are logged below, so the symptom is legible rather than silent. Guessing at a
// weaker stop condition without a browser that actually needs one would trade a
// proven fix for a speculative one.
//
// To extend this to LibreWolf or Zen, run the live gate against one.
// DetectBrowser cannot be steered, so name the executable directly —
// MOOMBOX_LIVE_BROWSER_PATH exists for exactly this:
//
//	$env:MOOMBOX_LIVE_BROWSER_REFRESH="1"
//	$env:MOOMBOX_LIVE_BROWSER_PATH="$env:LOCALAPPDATA\Mozilla Firefox\firefox.exe"
//	go test -count=1 -v -run TestLiveFirefoxRefreshWritesTheProfile ./internal/cookies/
//
// A drain that empties the job in a few seconds and leaves YouTube cookies in
// the throwaway profile confirms the condition generalises. Do NOT look for a
// screenshot there: --screenshot is unreliable against freshly-created
// profiles, which is why the gate only logs it — see the test's doc comment.
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
			if polls == 0 {
				// Nothing was ever seen alive in the job, so nothing was
				// waited on — which is a different statement from "the
				// browser finished", and the only one this observation
				// supports.
				//
				// It is the norm on darwin and the fallback build, where
				// processJob is still a no-op stub returning 0 from
				// activeProcesses unconditionally, so every launch there lands
				// here on lap zero having drained nothing. Linux is different
				// in kind: its process group gives a real count, so landing
				// here means the group really was empty when the direct child
				// was reaped — the usual case there, since without a launcher
				// the direct child IS the browser — and anything that had
				// outlived it would have been waited for. Claiming a finish
				// would still assert something a stub platform cannot observe,
				// the same distinction the !rendered branch in refreshFirefox
				// holds.
				logger.Debug("no tracked processes to wait for; the browser was not waited on",
					"elapsed", elapsed.Round(time.Millisecond), "polls", polls)
				return nil
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

// closeLaunchJob is runWithTimeout's teardown: finish off whatever the job
// still tracks, then release it.
//
// Named rather than inlined so it can be tested without launching a process.
// Two of runWithTimeout's exits arrive here with a browser still alive — the
// drain timing out with processes left in the job, and the caller's context
// being cancelled — and on Windows the close is what kills them. On Linux the
// close forgets a process-group id, so the kill has to be asked for first; the
// order matters, because a job that has already forgotten its group has nothing
// left to name.
func closeLaunchJob(job *processJob, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) {
	if job == nil {
		return
	}
	logger.Debug("closing job object (killing all tracked processes)")
	if err := killTrackedProcesses(job); err != nil {
		logger.Warn("could not kill the refresh browser's process group; it may still be running",
			"err", err)
	}
	job.close()
}

// runWithTimeout starts cmd inside a Job Object, waits for the launched
// process AND for the job to empty, and kills the tree on the way out.
//
// onLauncherReaped, when non-nil, is called the instant cmd.Wait() returns —
// before the drain, which can run for the rest of the budget. The caller uses
// it to stop advertising a PID that no longer exists (see refreshFirefox).
//
// It fires on both exits that actually observe a reap: the normal one, and the
// timeout path once the post-kill wait sees cmd.Wait() return. It does NOT
// fire when that wait times out in turn, because then the process may still be
// alive and the published PID is the only handle on it.
func runWithTimeout(ctx context.Context, cmd *exec.Cmd, timeout time.Duration, onLauncherReaped func(), logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) error {
	// Create a job — a Job Object on Windows, a process group on Linux — so
	// every child process (reparented ones included) is finished off by the
	// deferred closeLaunchJob on the way out: on Windows the close itself is
	// the kill, on Linux closeLaunchJob asks for it first because a close
	// there only forgets the group.
	job, jobErr := newProcessJob()
	if jobErr != nil {
		logger.Warn("failed to create job object", "err", jobErr)
	} else if job != nil {
		logger.Debug("created job object for process tracking")
	}
	defer closeLaunchJob(job, logger)

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
		// The deferred closeLaunchJob finishes off the job on the way out
		// (KILL_ON_JOB_CLOSE on Windows, the group kill on Linux). Also kill
		// the tree directly as a belt-and-suspenders approach.
		killProcessTree(cmd.Process)
		// Wait briefly for reap, but don't block forever if kill failed
		select {
		case <-done:
			logger.Debug("process reaped after kill", "pid", cmd.Process.Pid)
			// Same reason as the <-done branch above, and previously missing
			// here: cmd.Wait() has returned, so this PID is reaped and
			// Windows may recycle it onto something unrelated at any moment.
			// The caller then carries a dead PID through the launch spacing
			// and the rest of the refresh with killRefreshProcess still
			// willing to taskkill /F /T it.
			//
			// Only in this arm. The 5s fallback below reaches its deadline
			// WITHOUT a reap, so the process may still be alive and the
			// caller's ability to kill it is the last line of defence —
			// clearing the slot there would trade a recycled-PID risk for an
			// orphaned-browser one, which is the worse of the two because it
			// holds the profile lock and breaks every later refresh.
			if onLauncherReaped != nil {
				onLauncherReaped()
			}
		case <-time.After(5 * time.Second):
			logger.Warn("process did not exit after kill, forcing", "pid", cmd.Process.Pid)
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
		return fmt.Errorf("process timed out after %s", timeout)
	}
}
