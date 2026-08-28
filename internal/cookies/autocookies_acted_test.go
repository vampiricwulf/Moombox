package cookies

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// --- a browser the tests can drive -----------------------------------------

// fakeBrowserModeEnv selects what the stub browser does. It travels by
// environment because refreshFirefox builds the exec.Cmd itself and leaves Env
// nil, so the child inherits the test process's environment and t.Setenv is the
// only channel available.
const fakeBrowserModeEnv = "MOOMBOX_TEST_FAKE_BROWSER_MODE"

const (
	// fakeBrowserSilent exits 0 having rendered nothing.
	//
	// This is the shape of every NIL-returning failure — a job query that
	// failed, a job assign that failed, a launcher handoff nobody waited for —
	// and it is the hole this task exists to close. A launch that FAILS is a
	// different and much easier case: it is caught by the error alone, so a
	// test driving it would still pass with the screenshot check deleted.
	fakeBrowserSilent = "silent"
	// fakeBrowserRender writes a screenshot: a browser that really ran.
	fakeBrowserRender = "render"
	// fakeBrowserYouTubeOnly renders for YouTube and not for Twitch — a
	// two-platform refresh whose second launch did nothing.
	fakeBrowserYouTubeOnly = "youtube-only"
	// fakeBrowserHandoff is the SETUP path's mode, and it models the one fact
	// that path turns on: a Firefox launcher starts the real browser and then
	// exits (~170ms measured), so cmd.Wait() returning is evidence about the
	// launcher and nothing else. The stub starts a second process that keeps
	// running and returns immediately, which is the same shape.
	//
	// The refresh launches ignore it — they are judged by their screenshot —
	// so it only ever selects a behaviour on the setup argv.
	fakeBrowserHandoff = "handoff"
)

// fakeSetupChildArg marks the process fakeBrowserHandoff leaves behind: the
// stand-in for the browser window the user is signed into. An argv marker
// rather than a mode, because it has to be recognised before anything else and
// no `go test` flag can produce it.
const fakeSetupChildArg = "--moombox-fake-setup-browser-child"

// fakeSetupChildLifetime bounds that process. Normally the Job Object kills it
// — that is the whole point of the setup job — so this is the backstop for the
// case where it never got into one: a failed assign must not be able to leave a
// sleeping process behind on the machine running the tests. Chosen between two
// pressures: long enough that a slow host cannot make the stand-in browser
// "close" mid-test (the tests need it for under a second), short enough that an
// escapee cannot keep the test binary locked into the next `go test` run.
const fakeSetupChildLifetime = 15 * time.Second

// fakeSetupHandoffDelay holds the stand-in launcher open long enough for
// startFirefoxSetup's job.assign to land before the child exists. A child joins
// its parent's job AT CREATION, so one forked inside that window would never be
// tracked, and the tests would be measuring that race instead of the fix.
const fakeSetupHandoffDelay = 150 * time.Millisecond

// TestMain lets the test binary stand in for a Firefox-family browser, on both
// paths that launch one.
//
// refreshFirefox launches `<path> --new-instance --screenshot <file> --profile
// <dir> <url>` and judges the result by whether that screenshot appears, so a
// stub controlling exactly that is enough to drive the REAL launch path —
// runWithTimeout, the Job Object, the drain, the verdict — with no browser
// installed and no toolchain invoked at test time. No `go test` flag produces
// `--new-instance`, so an ordinary run can never trip the branch.
//
// startFirefoxSetup launches the same argv MINUS the `--screenshot` pair, and
// the setup stub covers that one. It exists because the setup path's Job Object
// cannot be tested any other way: the probe behind the abandoned-setup reap
// asks a real handle how many processes it holds, and faking the probe
// (jobReports) skips the very thing under test.
func TestMain(m *testing.M) {
	if isFakeSetupChild(os.Args) {
		// The stand-in browser: it does nothing but stay alive inside the job
		// until something closes the handle.
		time.Sleep(fakeSetupChildLifetime)
		os.Exit(0)
	}
	if screenshot, url, isBrowserLaunch := parseFakeBrowserArgs(os.Args); isBrowserLaunch {
		os.Exit(runFakeBrowser(os.Getenv(fakeBrowserModeEnv), screenshot, url))
	}
	if isFakeSetupLaunch(os.Args) {
		os.Exit(runFakeSetupBrowser(os.Getenv(fakeBrowserModeEnv)))
	}
	os.Exit(m.Run())
}

// isFakeSetupChild recognises the process fakeBrowserHandoff spawns. Checked
// first: it carries the inherited mode environment too, and must not be
// mistaken for another launcher.
func isFakeSetupChild(args []string) bool {
	return slices.Contains(args, fakeSetupChildArg)
}

// isFakeSetupLaunch recognises startFirefoxSetup's argv — the refresh argv
// without its `--screenshot` pair. The two are disjoint by that flag alone, so
// this is checked after parseFakeBrowserArgs and neither can claim the other's
// launch.
func isFakeSetupLaunch(args []string) bool {
	sawNewInstance := false
	for _, a := range args {
		switch a {
		case "--screenshot":
			return false
		case "--new-instance":
			sawNewInstance = true
		}
	}
	return sawNewInstance
}

// runFakeSetupBrowser is the setup stub's whole body.
//
// An unset or unknown mode exits at once and hands nothing off, leaving an
// EMPTY job behind — the abandoned setup. Only fakeBrowserHandoff leaves a live
// process in the job. That default is the same rule the refresh stub follows
// and matters more here: an accidental launch cannot manufacture the evidence
// that a browser is still running, which is the evidence that stops the reap.
func runFakeSetupBrowser(mode string) int {
	if mode != fakeBrowserHandoff {
		return 0
	}
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	time.Sleep(fakeSetupHandoffDelay)
	// Nil Stdout/Stderr, so the child gets NUL rather than a share of the pipe
	// `go test` is reading — an inherited pipe would keep the run open until the
	// child's lifetime expired.
	if err := exec.Command(exe, fakeSetupChildArg).Start(); err != nil {
		return 1
	}
	return 0
}

// parseFakeBrowserArgs recognises refreshFirefox's argv and pulls out the two
// values the stub needs. The target URL is last, which is how the stub tells
// the YouTube launch from the Twitch one.
func parseFakeBrowserArgs(args []string) (screenshot, url string, isBrowserLaunch bool) {
	sawNewInstance := false
	for i, a := range args {
		switch a {
		case "--new-instance":
			sawNewInstance = true
		case "--screenshot":
			if i+1 < len(args) {
				screenshot = args[i+1]
			}
		}
	}
	if !sawNewInstance || screenshot == "" {
		return "", "", false
	}
	return screenshot, args[len(args)-1], true
}

// runFakeBrowser is the stub's whole body. An unset or unknown mode is SILENT
// on purpose: the failure is the interesting case, and defaulting to it means
// an accidental launch can never fake proof it did not produce.
func runFakeBrowser(mode, screenshot, url string) int {
	render := false
	switch mode {
	case fakeBrowserRender:
		render = true
	case fakeBrowserYouTubeOnly:
		render = url == platformRefreshURLs["youtube"]
	}
	if render {
		if err := os.WriteFile(screenshot, []byte("\x89PNG fake screenshot"), 0o600); err != nil {
			return 1
		}
	}
	return 0
}

// fakeBrowser returns a DetectedBrowser pointing at this test binary.
func fakeBrowser(t *testing.T) *DetectedBrowser {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary to stand in for a browser: %v", err)
	}
	return &DetectedBrowser{Type: "firefox", Path: exe, Name: "Fake Firefox"}
}

// TestBrowserLaunchActed pins the acted/not-acted decision.
//
// Every row is a way a refresh used to report success while doing nothing.
// The two halves of the predicate are not interchangeable: the error covers
// browsers that were killed or never started, the screenshot covers the
// failures that return no error at all — and it was those that made the
// Firefox-family refresh a permanent silent no-op.
func TestBrowserLaunchActed(t *testing.T) {
	cases := []struct {
		name      string
		launchErr error
		rendered  bool
		want      bool
	}{
		{"clean launch that rendered", nil, true, true},

		// The bug this whole arc exists for. runWithTimeout returns nil when
		// the job-count query fails (drainJob stops waiting) and when
		// job.assign failed (the browser is outside the job, so the drain sees
		// an empty job on lap zero). Both reap the launcher, kill or outrun the
		// browser, and hand back nil — and both used to log "refresh
		// completed" at Info.
		{"no error but nothing rendered", nil, false, false},

		// A drain timeout means the browser was alive and has just been killed
		// mid-load. A screenshot from earlier in that same launch does not
		// redeem it: the profile is half-written at best.
		{"drain timed out even though a page rendered", errBrowserDrainTimeout, true, false},
		{"drain timed out with nothing rendered", errBrowserDrainTimeout, false, false},

		{"browser could not start", errors.New("exec: no such file"), false, false},
		{"cancelled mid-launch", context.Canceled, false, false},
		{"launch error with a stale screenshot present", errors.New("boom"), true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := browserLaunchActed(tc.launchErr, tc.rendered); got != tc.want {
				t.Errorf("browserLaunchActed(%v, %v) = %v, want %v", tc.launchErr, tc.rendered, got, tc.want)
			}
		})
	}
}

// TestBrowserRenderProofIsPerLaunch is the trap the brief calls out: the
// screenshot lives at a FIXED path inside the profile and refreshFirefox's
// os.Remove is function-scoped, so without a per-launch clear the YouTube
// screenshot survives into the Twitch launch and every platform after the
// first reads as "acted" no matter what its browser did.
//
// The clear-then-check ordering is the contract; this pins it end to end.
func TestBrowserRenderProofIsPerLaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refresh-screenshot.png")

	// A screenshot left by the previous platform's launch.
	if err := os.WriteFile(path, []byte("stale png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !browserRendered(path) {
		t.Fatal("fixture is broken — the stale artifact should look like proof before it is cleared")
	}

	clearBrowserRenderProof(path)
	if browserRendered(path) {
		t.Fatal("a screenshot from the PREVIOUS launch was counted as this launch's proof")
	}

	// Clearing a path that is already gone is the normal case, not an error.
	clearBrowserRenderProof(path)

	if err := os.WriteFile(path, []byte("fresh png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !browserRendered(path) {
		t.Error("a screenshot written by this launch is the proof the verdict is made of")
	}
}

// TestBrowserRenderedRejectsNonProof covers the artifacts that exist but prove
// nothing. A zero-length file is what a browser killed part-way through
// writing leaves behind; a directory at that path is not a screenshot at all.
func TestBrowserRenderedRejectsNonProof(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "never-written.png")
	if browserRendered(missing) {
		t.Error("a screenshot that does not exist is not proof")
	}

	empty := filepath.Join(dir, "empty.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if browserRendered(empty) {
		t.Error("a zero-length screenshot is what a killed browser leaves behind, not proof it rendered")
	}

	asDir := filepath.Join(dir, "adirectory.png")
	if err := os.Mkdir(asDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if browserRendered(asDir) {
		t.Error("a directory at the screenshot path is not a screenshot")
	}
}

// TestRefreshThatRenewedNothingDoesNotClaimCredit is F2 at the level that
// matters.
//
// A browser refresh whose browser never ran still finds the previous
// credentials in cookies.txt — the independent 30-minute RefreshService keeps
// them alive — so verification passes and the call still returns true, which is
// the honest answer to "will authenticated requests work?". What must NOT
// happen is this pass taking credit for it: stamping lastRefresh would suppress
// the next attempt (shouldSkipPeriodicRefresh skips inside interval/2) and tell
// the user in Settings that their cookies are fresher than they are, and the
// meta sidecar would make that durable across a restart.
func TestRefreshThatRenewedNothingDoesNotClaimCredit(t *testing.T) {
	// A profile with nothing relevant in it, so the read contributes nothing
	// and the existing cookies.txt is what verifies.
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "sessionid", value: "x", host: ".example.com", path: "/"},
	})
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	// A browser that cannot launch is the simplest not-acted case: no
	// screenshot, and refreshFirefox degrades rather than failing the refresh.
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if !ok {
		t.Fatal("the cookies on disk still authenticate, so the caller's question is still answered yes")
	}

	if lr := s.GetStatus().LastRefresh; lr != nil {
		t.Errorf("lastRefresh was stamped for a refresh whose browser never ran: %q", *lr)
	}
	if _, statErr := os.Stat(MetaPath(cookiePath)); !os.IsNotExist(statErr) {
		t.Error("the meta sidecar records a refresh that never happened, making the claim survive a restart")
	}
}

// TestBrowserThatRenderedIsCredited is the positive control for the two tests
// below: it proves the stub really can produce proof, so their not-acted
// verdicts mean "the browser did nothing" rather than "the harness is broken".
// It is also the case that must keep working — a browser that ran gets credit.
func TestBrowserThatRenderedIsCredited(t *testing.T) {
	t.Setenv(fakeBrowserModeEnv, fakeBrowserRender)

	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	_, acted, err := s.refreshFirefox(context.Background(), fakeBrowser(t))
	if err != nil {
		t.Fatalf("refreshFirefox: %v", err)
	}
	if !acted {
		t.Error("a browser that wrote its screenshot did the work and must be credited for it")
	}
}

// TestRefreshWithNoErrorAndNothingDoneClaimsNoCredit is F2's actual hole, end
// to end.
//
// The launch SUCCEEDS — exit 0, nothing reported — and the browser does
// nothing. That is what a failed job query, a failed job assign, and an
// unwaited launcher handoff all look like from here, and it is the shape that
// logged "refresh completed" and took full credit for cookies the 30-minute
// RefreshService had kept alive.
//
// Deliberately NOT the `launchErr != nil` shape: reducing the verdict to
// `launchErr == nil` would still satisfy a test driven by a browser that cannot
// start, so such a test cannot detect a regression of this fix. Delete the
// screenshot half of browserLaunchActed and this test fails.
func TestRefreshWithNoErrorAndNothingDoneClaimsNoCredit(t *testing.T) {
	t.Setenv(fakeBrowserModeEnv, fakeBrowserSilent)

	// Nothing relevant in the profile, so the read contributes nothing and the
	// existing cookies.txt is what verifies — exactly the situation where the
	// old code congratulated itself.
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "sessionid", value: "x", host: ".example.com", path: "/"},
	})
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	browser := fakeBrowser(t)
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return browser }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if !ok {
		t.Fatal("the cookies on disk still authenticate, so the caller's question is still answered yes")
	}

	if lr := s.GetStatus().LastRefresh; lr != nil {
		t.Errorf("lastRefresh was stamped for a launch that exited cleanly having done nothing: %q", *lr)
	}
	if _, statErr := os.Stat(MetaPath(cookiePath)); !os.IsNotExist(statErr) {
		t.Error("the meta sidecar records a refresh that never happened, making the claim survive a restart")
	}
}

// TestScreenshotIsClearedBeforeEachLaunch pins the CALL SITE, not the helper.
//
// The screenshot lives at a fixed path inside the profile and refreshFirefox's
// os.Remove is function-scoped, so hoisting clearBrowserRenderProof out of the
// loop — the exact trap the brief called out — lets the YouTube screenshot
// survive into the Twitch launch. Twitch then reads as "acted" while its
// browser did nothing, and the AND across launches comes out true.
//
// So: YouTube renders, Twitch does not, and the pass must read not-renewed.
// TestBrowserThatRenderedIsCredited above is the control proving the YouTube
// half really did render, so a pass here cannot come from a stub that never
// writes anything.
//
// Costs firefoxLaunchSpacing (5s) — the wait between launches is what makes the
// two launches distinct, and shortening it via ctx would abort the second one
// and make the test vacuous.
func TestScreenshotIsClearedBeforeEachLaunch(t *testing.T) {
	t.Setenv(fakeBrowserModeEnv, fakeBrowserYouTubeOnly)

	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(goodTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	if len(s.refreshPlatforms()) != 2 {
		t.Fatalf("fixture is broken — this test needs two launches, got %v", s.refreshPlatforms())
	}

	_, acted, err := s.refreshFirefox(context.Background(), fakeBrowser(t))
	if err != nil {
		t.Fatalf("refreshFirefox: %v", err)
	}
	if acted {
		t.Error("the Twitch launch rendered nothing and was credited anyway — the YouTube screenshot survived into it, " +
			"so clearBrowserRenderProof is no longer running inside the loop")
	}
}

// TestProfileImportStillClaimsCredit is the guard on the other side, and the
// reason the mtime-based version of this check was withdrawn.
//
// The browserless import (Docker: no browser installed, a mounted profile) has
// no browser that could have acted, so anything that demands proof of one makes
// every containerised import report a refresh that never renews — permanently,
// on every restart. Reading a profile IS renewal; this must stay indistinguishable
// from a successful browser refresh.
func TestProfileImportStillClaimsCredit(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if !ok {
		t.Fatal("the import read a signed-in profile and YouTube verified — that is a success")
	}

	if s.GetStatus().LastRefresh == nil {
		t.Error("a browserless import must still stamp lastRefresh; withholding it re-fires the import on every tick forever")
	}
	meta, err := LoadMeta(cookiePath)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if meta == nil || meta.LastRefresh.IsZero() {
		t.Fatal("a browserless import must still persist the meta sidecar")
	}
}
