package cookies

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// tLogger routes the refresh path's logging into the test log so a live run
// shows the drain polls, the elapsed time, and any degradation reason.
// Deliberately not nopLogger: when this test fails, the log IS the diagnosis.
type tLogger struct{ t *testing.T }

func (l tLogger) Debug(msg string, args ...any) { l.t.Log(append([]any{"DEBUG", msg}, args...)...) }
func (l tLogger) Info(msg string, args ...any)  { l.t.Log(append([]any{"INFO ", msg}, args...)...) }
func (l tLogger) Warn(msg string, args ...any)  { l.t.Log(append([]any{"WARN ", msg}, args...)...) }
func (l tLogger) Error(msg string, args ...any) { l.t.Log(append([]any{"ERROR", msg}, args...)...) }

// Environment variables that drive this gate. Both are TEST-ONLY; neither is
// read by any production path.
const (
	// liveRefreshEnv=1 enables the gate at all.
	liveRefreshEnv = "MOOMBOX_LIVE_BROWSER_REFRESH"
	// liveBrowserPathEnv names the browser executable directly, bypassing
	// DetectBrowser. See liveTestBrowser for why this exists.
	liveBrowserPathEnv = "MOOMBOX_LIVE_BROWSER_PATH"
)

// TestLiveFirefoxRefreshWritesTheProfile launches a REAL Firefox-family
// browser against a throwaway profile and asserts that the browser ran far
// enough to LOAD https://www.youtube.com — not merely that a process started
// and was reaped.
//
// It asserts that by counting rows in the profile's cookies.sqlite. YouTube
// sets cookies on an anonymous visit (VISITOR_INFO1_LIVE, YSC and friends —
// no login involved), so a browser that genuinely reached the page leaves
// moz_cookies non-empty with at least one youtube host, while a browser killed
// at launcher-exit time leaves the database absent or empty. Only row COUNTS
// and the host column are read; cookie values are never selected, logged, or
// compared, here or anywhere else in this package's tests.
//
// Two subtests run the A/B directly, so the gate cannot silently degrade into
// a test that always passes:
//
//   - "killed at launcher exit" reproduces the pre-f6e5c59 bug (close the Job
//     Object the moment cmd.Wait() returns) and requires that it wrote no
//     YouTube cookies. Windows only — the Linux/darwin processJob stubs have
//     no KILL_ON_JOB_CLOSE, so nothing would be killed and the control would
//     be meaningless.
//   - "drained launch" runs the real runWithTimeout and requires that it did.
//
// This is the only test that can catch the failure it guards. Firefox uses a
// launcher-process model — the exe we start hands off and exits in ~170ms —
// so before runWithTimeout waited for the Job Object to drain, cmd.Wait()
// returned on the launcher and the deferred job.close() killed the real
// browser mid-page-load. Every Firefox-family cookie refresh silently did
// nothing, with a cheerful "refresh completed" in the log. No fixture and no
// unit test can see that; only a real browser and a real profile can.
//
// # Why the screenshot is logged and not asserted
//
// refreshFirefox's own verdict (browserRendered) keys off --screenshot, and an
// earlier version of this test asserted the same thing. It cannot: measured on
// 2026-08-25, --screenshot does NOT reliably produce a file when the profile
// was created fresh by os.MkdirTemp. Reproduced against both Waterfox and
// Mozilla Firefox, against both https://example.com and https://www.youtube.com,
// against a bare fresh profile / one warmed by a real 15s GUI session with the
// lock files cleared / one seeded with the real install's prefs.js, and with no
// Job Object at all (plain exec.Command) — so it is unrelated to the drain
// machinery. Waterfox emits "RenderCompositorSWGL failed mapping default
// framebuffer, no dt" on those runs. The same screenshot check DID discriminate
// correctly against copies of a real, setup-created profile, so the signal is
// sound in production; it just cannot be manufactured from a fresh profile,
// which is what this test builds. Do not re-add the assertion — log it, as
// below, because it is useful diagnostic output on a machine where it works.
//
// The old fingerprintCookieDB before/after check is gone for a related reason:
// on a fresh profile cookies.sqlite does not exist beforehand, so ANY file
// creation makes the fingerprints differ. That is not hypothetical — the
// killed-launch control below creates cookies.sqlite with ZERO rows on both
// Waterfox and Firefox, so the fingerprint check passes on exactly the launch
// this whole test exists to reject. Row counts see the difference; file stamps
// cannot.
//
// # Reading the drain output
//
// The one signal that MEANS something is errBrowserDrainTimeout: the job still
// had a live process when the budget expired, which is the failure mode
// drainJob's note describes — a browser that leaves an updater, a crash
// reporter or a content process behind never empties the job, burns the whole
// processTimeout on every refresh, and turns working refreshes into reported
// failures. A run that drains and finds YouTube cookies has passed, however
// long it took.
//
// Elapsed time and poll count are an OBSERVATION, not a threshold, and the
// spread is wide enough that treating them as one produces a false "this
// browser is broken" verdict. Measured on throwaway profiles, 6 YouTube
// cookies each:
//
//	Waterfox  2.848s /  53 polls   (2.689-3.595s over 51-67 polls, reference machine, 2026-08-25)
//	Firefox   1.734s /  32 polls   (reference machine, 2026-08-25)
//	          13.96s / 276 polls   (a clean pass on different hardware, 2026-08-26)
//
// That last row is the same successful outcome as the first two, four to five
// times slower, with no error — so a slower machine, a colder page cache, or a
// browser that simply takes longer to settle all land well outside anything the
// first two rows would suggest. The launcher itself exits at ~150-170ms in
// every case observed.
//
// # Running it
//
// Enable with MOOMBOX_LIVE_BROWSER_REFRESH=1. Skipped by default: it needs an
// installed browser, network access, and ~10s.
//
//	$env:MOOMBOX_LIVE_BROWSER_REFRESH="1"
//	go test -count=1 -v -timeout 180s -run TestLiveFirefoxRefreshWritesTheProfile ./internal/cookies/
//
// MOOMBOX_LIVE_BROWSER_PATH points the gate at a specific browser, bypassing
// DetectBrowser entirely. DetectBrowser ranks Waterfox above Firefox and there
// is no supported way to steer it (redirecting PROGRAMFILES does not work, and
// these browsers are not on PATH), so without this override the gate can only
// ever certify one browser — which defeats the point of a cross-browser gate.
// When it is set, the Firefox-family detection check is skipped: the operator
// asserted the family by pointing at the executable.
//
//	$env:MOOMBOX_LIVE_BROWSER_REFRESH="1"
//	$env:MOOMBOX_LIVE_BROWSER_PATH="$env:LOCALAPPDATA\Mozilla Firefox\firefox.exe"
//	go test -count=1 -v -timeout 180s -run TestLiveFirefoxRefreshWritesTheProfile ./internal/cookies/
//
// It drives runWithTimeout with refreshFirefox's exact argv rather than
// calling refreshFirefox itself, for two reasons: refreshFirefox deletes the
// screenshot on the way out (so that diagnostic would be unobservable from a
// test), and it only launches for platforms whose cookies are already in the
// jar (so it would need real credentials to do anything at all).
func TestLiveFirefoxRefreshWritesTheProfile(t *testing.T) {
	if os.Getenv(liveRefreshEnv) != "1" {
		t.Skip("set " + liveRefreshEnv + "=1 to run the live browser refresh test")
	}
	browser := liveTestBrowser(t)

	// The negative control runs FIRST: if it fails, the positive assertion
	// below is not worth trusting, and knowing that early is cheaper than
	// waiting out a full drained launch to learn it.
	t.Run("killed at launcher exit writes no cookies", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("the killed-launch control needs KILL_ON_JOB_CLOSE; the processJob stubs on this platform kill nothing")
		}
		profileDir := newThrowawayProfile(t)
		cmd := refreshArgv(browser, profileDir)

		start := time.Now()
		runErr := runKilledAtLauncherExit(cmd)
		elapsed := time.Since(start).Round(time.Millisecond)
		t.Logf("killed-at-launcher-exit launch returned after %s (err=%v)", elapsed, runErr)
		if runErr != nil {
			t.Fatalf("could not reproduce the pre-fix launch: %v", runErr)
		}

		census := censusProfileCookies(profileDir)
		t.Logf("killed launch: %s", census)
		if census.youtubeRows > 0 {
			t.Fatalf("a browser killed at launcher exit (%s) still wrote %d youtube rows into %s — "+
				"the cookie census does not discriminate and this gate cannot fail",
				elapsed, census.youtubeRows, firefoxCookieDBName)
		}
	})

	t.Run("drained launch writes cookies", func(t *testing.T) {
		profileDir := newThrowawayProfile(t)
		screenshot := filepath.Join(profileDir, "refresh-screenshot.png")
		cmd := refreshArgv(browser, profileDir, screenshot)

		ctx, cancel := context.WithTimeout(context.Background(), processTimeout+30*time.Second)
		defer cancel()

		start := time.Now()
		runErr := runWithTimeout(ctx, cmd, processTimeout, nil, tLogger{t})
		elapsed := time.Since(start).Round(time.Millisecond)
		t.Logf("runWithTimeout returned after %s (err=%v)", elapsed, runErr)
		if runErr != nil {
			t.Fatalf("runWithTimeout: %v (after %s)", runErr, elapsed)
		}

		// Informational ONLY — see the "Why the screenshot is logged and not
		// asserted" section above. Never turn this into a t.Fatalf.
		if info, statErr := os.Stat(screenshot); statErr == nil {
			t.Logf("screenshot: %d bytes (informational; --screenshot is unreliable on fresh profiles)", info.Size())
		} else {
			t.Logf("screenshot: none (informational; --screenshot is unreliable on fresh profiles): %v", statErr)
		}

		census := censusProfileCookies(profileDir)
		t.Logf("drained launch: %s", census)
		if census.err != nil {
			t.Fatalf("could not read %s in %q after %s — the browser never wrote the profile: %v",
				firefoxCookieDBName, profileDir, elapsed, census.err)
		}
		if census.rows == 0 {
			t.Fatalf("%s in %q holds zero rows after %s — the browser was killed before it loaded %s",
				firefoxCookieDBName, profileDir, elapsed, refreshURL)
		}
		if census.youtubeRows == 0 {
			t.Fatalf("%s in %q holds %d rows after %s but none from a youtube host — the browser started but never reached %s",
				firefoxCookieDBName, profileDir, census.rows, elapsed, refreshURL)
		}
	})
}

// liveTestBrowser resolves which browser this gate drives: the operator's
// MOOMBOX_LIVE_BROWSER_PATH if set, otherwise DetectBrowser's pick. Skips (or
// fails, for a bad override) rather than returning nil.
func liveTestBrowser(t *testing.T) *DetectedBrowser {
	t.Helper()

	if override := os.Getenv(liveBrowserPathEnv); override != "" {
		info, err := os.Stat(override)
		if err != nil {
			t.Fatalf("%s=%q cannot be used: %v", liveBrowserPathEnv, override, err)
		}
		if info.IsDir() {
			t.Fatalf("%s=%q is a directory, not a browser executable", liveBrowserPathEnv, override)
		}
		// The family check is deliberately skipped: DetectBrowser is what
		// classifies a browser, and it is exactly what the operator just
		// bypassed. Pointing this at a Chromium browser will simply fail,
		// loudly, on the --profile/--screenshot argv.
		t.Logf("using operator-supplied browser at %s (%s override; Firefox-family check skipped)", override, liveBrowserPathEnv)
		return &DetectedBrowser{Type: "override", Path: override, Name: filepath.Base(override)}
	}

	browser := DetectBrowser()
	if browser == nil {
		t.Skip("no supported browser detected")
	}
	if !isFirefoxBased(browser.Type) {
		t.Skipf("detected browser %q is not Firefox-family; this test covers the launcher-handoff path (set %s to choose one)",
			browser.Type, liveBrowserPathEnv)
	}
	t.Logf("using %s (%s) at %s", browser.Name, browser.Type, browser.Path)
	return browser
}

// newThrowawayProfile creates a fresh profile directory carrying only the
// first-run-suppressing user.js that refreshFirefox writes.
//
// Not t.TempDir(): its cleanup FAILS the test if Windows still holds a handle
// on anything the browser left behind, which would turn a cosmetic teardown
// race into a false regression report.
func newThrowawayProfile(t *testing.T) string {
	t.Helper()

	profileDir, err := os.MkdirTemp("", "moombox-live-refresh-")
	if err != nil {
		t.Fatalf("create throwaway profile dir: %v", err)
	}
	t.Cleanup(func() {
		if rmErr := os.RemoveAll(profileDir); rmErr != nil {
			t.Logf("could not remove throwaway profile %q: %v", profileDir, rmErr)
		}
	})
	if err := os.WriteFile(filepath.Join(profileDir, "user.js"), []byte(firefoxUserJS), 0o644); err != nil {
		t.Fatalf("write user.js: %v", err)
	}
	return profileDir
}

// refreshArgv builds exactly refreshFirefox's command line. Passing no
// screenshot path omits the flag, which is what the killed-launch control
// wants: it has nothing to say about rendering.
func refreshArgv(browser *DetectedBrowser, profileDir string, screenshot ...string) *exec.Cmd {
	args := []string{"--new-instance"}
	if len(screenshot) > 0 {
		args = append(args, "--screenshot", screenshot[0])
	}
	args = append(args, "--profile", profileDir, refreshURL)
	cmd := exec.Command(browser.Path, args...)
	configureCmdSysProcAttr(cmd)
	return cmd
}

// runKilledAtLauncherExit reproduces the pre-f6e5c59 launch: register the Job
// Object's KILL_ON_JOB_CLOSE teardown, then return the moment cmd.Wait()
// reports the LAUNCHER exited — which kills the real browser mid-page-load.
//
// Kept in the test rather than restored to production code so the bug has a
// live specimen without the shipped path ever being able to take it. It only
// ever kills processes started by this function, via the job it created.
func runKilledAtLauncherExit(cmd *exec.Cmd) error {
	job, err := newProcessJob()
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}
	defer job.close()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser: %w", err)
	}
	if err := job.assign(cmd.Process); err != nil {
		return fmt.Errorf("assign to job object: %w", err)
	}
	return cmd.Wait()
}

// profileCookieCensus is what one read of a throwaway profile's cookies.sqlite
// found. COUNTS ONLY. No cookie value is ever selected, stored, logged, or
// compared — see censusProfileCookies.
type profileCookieCensus struct {
	rows        int
	youtubeRows int
	err         error
}

func (c profileCookieCensus) String() string {
	if c.err != nil {
		return fmt.Sprintf("%s unreadable: %v", firefoxCookieDBName, c.err)
	}
	return fmt.Sprintf("%d moz_cookies rows, %d from a youtube host", c.rows, c.youtubeRows)
}

// censusProfileCookies counts the rows a Firefox-family browser wrote into a
// throwaway profile's cookies.sqlite.
//
// It goes through snapshotFirefoxCookieDB rather than copying the database
// itself because of the WAL trap this package already documents: cookies.sqlite
// taken WITHOUT its -wal sidecar opens cleanly and returns ZERO rows with no
// error at all. A discriminator built on that would report "no cookies" on
// every run and could never fail. The snapshot helper copies the pair. When the
// snapshot cannot be taken the live database is read in place instead — SQLite
// resolves the -wal itself when the sidecar is sitting right next to it — which
// mirrors querySnapshotOrLive's fallback.
//
// The SELECT names `host` and nothing else. Do not add `value` or `name` to it.
func censusProfileCookies(profileDir string) profileCookieCensus {
	dbPath := filepath.Join(profileDir, firefoxCookieDBName)
	if _, err := os.Stat(dbPath); err != nil {
		return profileCookieCensus{err: fmt.Errorf("stat %s: %w", firefoxCookieDBName, err)}
	}

	readPath := dbPath
	if snapDir, cleanup, err := snapshotFirefoxCookieDB(profileDir); err == nil {
		defer cleanup()
		readPath = filepath.Join(snapDir, firefoxCookieDBName)
	}
	return countMozCookieRows(readPath)
}

// countMozCookieRows opens the database read-only and counts rows, plus how
// many carry a youtube host. Same DSN as queryFirefoxCookieDB, and for the
// same reasons: the `file:` prefix keeps modernc/sqlite from dropping the query
// string, mode=ro guarantees no write into the database, and the busy timeout
// gives WAL lock contention somewhere to wait.
func countMozCookieRows(dbPath string) profileCookieCensus {
	var out profileCookieCensus

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		out.err = fmt.Errorf("open %s: %w", firefoxCookieDBName, err)
		return out
	}
	defer db.Close()

	rows, err := db.Query("SELECT host FROM moz_cookies")
	if err != nil {
		out.err = fmt.Errorf("query moz_cookies: %w", err)
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			// A NULL host is a row queryFirefoxCookieDB would drop; it still
			// counts as a row the browser wrote, which is all this needs.
			out.rows++
			continue
		}
		out.rows++
		if strings.Contains(strings.ToLower(host), "youtube") {
			out.youtubeRows++
		}
	}
	if err := rows.Err(); err != nil {
		out.err = fmt.Errorf("iterate moz_cookies: %w", err)
	}
	return out
}
