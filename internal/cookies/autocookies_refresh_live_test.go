package cookies

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

// TestLiveFirefoxRefreshWritesTheProfile launches a REAL Firefox-family
// browser against a throwaway profile and asserts the two things that prove
// the browser actually ran to completion: it rendered a page (the screenshot
// exists and is non-empty) and it wrote the profile (cookies.sqlite's
// fingerprint changed).
//
// This is the only test that can catch the failure it guards. Firefox uses a
// launcher-process model — the exe we start hands off and exits in ~170ms —
// so before runWithTimeout waited for the Job Object to drain, cmd.Wait()
// returned on the launcher and the deferred job.close() killed the real
// browser mid-page-load. Every Firefox-family cookie refresh silently did
// nothing: no screenshot, no profile write, and a cheerful "refresh
// completed" in the log. No fixture and no unit test can see that; only a
// real browser and a real profile can.
//
// Enable with MOOMBOX_LIVE_BROWSER_REFRESH=1. Skipped by default: it needs an
// installed browser, network access, and ~5s.
//
//	$env:MOOMBOX_LIVE_BROWSER_REFRESH="1"
//	go test -count=1 -v -timeout 120s -run TestLiveFirefoxRefreshWritesTheProfile ./internal/cookies/...
//
// It drives runWithTimeout with refreshFirefox's exact argv rather than
// calling refreshFirefox itself, for two reasons: refreshFirefox deletes the
// screenshot on the way out (so the strongest evidence would be unobservable
// from a test), and it only launches for platforms whose cookies are already
// in the jar (so it would need real credentials to do anything at all).
func TestLiveFirefoxRefreshWritesTheProfile(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_BROWSER_REFRESH") != "1" {
		t.Skip("set MOOMBOX_LIVE_BROWSER_REFRESH=1 to run the live browser refresh test")
	}

	browser := DetectBrowser()
	if browser == nil {
		t.Skip("no supported browser detected")
	}
	if !isFirefoxBased(browser.Type) {
		t.Skipf("detected browser %q is not Firefox-family; this test covers the launcher-handoff path", browser.Type)
	}
	t.Logf("using %s (%s) at %s", browser.Name, browser.Type, browser.Path)

	// Not t.TempDir(): its cleanup FAILS the test if Windows still holds a
	// handle on anything the browser left behind, which would turn a
	// cosmetic teardown race into a false regression report.
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

	dbPath := filepath.Join(profileDir, firefoxCookieDBName)
	before := fingerprintCookieDB(dbPath)
	screenshot := filepath.Join(profileDir, "refresh-screenshot.png")

	// Exactly refreshFirefox's argv.
	cmd := exec.Command(browser.Path, "--new-instance", "--screenshot", screenshot, "--profile", profileDir, refreshURL)
	configureCmdSysProcAttr(cmd)

	ctx, cancel := context.WithTimeout(context.Background(), processTimeout+30*time.Second)
	defer cancel()

	start := time.Now()
	runErr := runWithTimeout(ctx, cmd, processTimeout, nil, tLogger{t})
	elapsed := time.Since(start).Round(time.Millisecond)
	t.Logf("runWithTimeout returned after %s (err=%v)", elapsed, runErr)
	if runErr != nil {
		t.Fatalf("runWithTimeout: %v (after %s)", runErr, elapsed)
	}

	info, statErr := os.Stat(screenshot)
	if statErr != nil {
		t.Fatalf("no screenshot at %q after %s — the browser never rendered the page: %v", screenshot, elapsed, statErr)
	}
	if info.Size() == 0 {
		t.Fatalf("screenshot at %q is empty after %s — the browser was killed mid-render", screenshot, elapsed)
	}
	t.Logf("screenshot: %d bytes", info.Size())

	after := fingerprintCookieDB(dbPath)
	if !fingerprintsDiffer(before, after) {
		t.Fatalf("%s in %q is unchanged after %s — the browser never wrote the profile", firefoxCookieDBName, profileDir, elapsed)
	}
}
