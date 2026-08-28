package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// abandonedSetupPid and latePublishPid are the PIDs the fakes in this file
// carry. Nothing ever kills them: every test here installs captureKills first,
// which replaces the package's kill hook, so no signal and no taskkill reaches
// the host. They are odd numbers on purpose — a live Windows PID is always a
// multiple of four — so even a future test that forgot the hook could not hit a
// real process on the developer machine this runs on.
const (
	abandonedSetupPid = 424243
	latePublishPid    = 424245
)

// captureKills swaps the package's process-kill hook for a recorder and
// restores it when the test ends. It returns the PIDs the code under test
// decided to kill, in order.
//
// Two jobs, and the second matters more: it is the ONLY thing that makes it
// safe to put a synthetic *os.Process into the setup slot. Without it,
// killProcessTree would hand a fabricated PID to taskkill /F /T (or Kill) on
// the machine running the tests.
func captureKills(t *testing.T) *[]int {
	t.Helper()
	prev := killProcessTree
	killed := []int{}
	killProcessTree = func(p *os.Process) {
		if p != nil {
			killed = append(killed, p.Pid)
		}
	}
	t.Cleanup(func() { killProcessTree = prev })
	return &killed
}

// abandonedSetup puts the service into the exact state a walked-away-from setup
// leaves behind: a browser process registered in the slot, the wait goroutine
// having recorded its exit `exitedAgo` in the past, and nothing at all having
// cleared setupProcess.
//
// That last part is the defect. The wait goroutines have always set
// browserExited; no path ever acted on it, so the slot stayed occupied for the
// life of the process and SetupInProgress stayed true with it.
func abandonedSetup(t *testing.T, s *AutoCookieService, exitedAgo time.Duration) *os.Process {
	t.Helper()
	proc := &os.Process{Pid: abandonedSetupPid}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setupProcess = proc
	s.setupBrowser = &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	s.browserExited = true
	s.setupRetainedSince = time.Now().Add(-exitedAgo)
	s.targetPlatform = "youtube"
	return proc
}

// unlaunchableChromium is a detector returning a Chromium-family browser at a
// path inside a fresh temp dir, so it provably does not exist.
//
// Reaching the launch is the POINT here: these tests assert that StartSetup got
// past the in-progress gate, and "start browser: …" is the proof it did without
// a browser window ever opening on the host.
func unlaunchableChromium(t *testing.T) func() *DetectedBrowser {
	t.Helper()
	path := filepath.Join(t.TempDir(), "not-a-browser.exe")
	return func() *DetectedBrowser {
		return &DetectedBrowser{Type: "chrome", Path: path, Name: "unlaunchable test browser"}
	}
}

// TestAbandonedSetupStopsWedgingStartSetup is A1 at the site it was reported.
//
// The user clicks "set up cookies", a browser opens, and they close it or walk
// away. Nothing cleared setupProcess, so the next StartSetup — and every one
// after it, until the process restarted — was refused with ErrSetupInProgress
// for a setup whose browser had been gone for hours.
//
// The assertion is on the error StartSetup returns, not on any field: against
// the unfixed code this is ErrSetupInProgress, and "start browser: …" is the
// launch attempt that could only be reached by passing the gate.
func TestAbandonedSetupStopsWedgingStartSetup(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = unlaunchableChromium(t)
	abandonedSetup(t, s, setupAbandonGrace+time.Second)

	err := s.StartSetup("youtube")

	if errors.Is(err, ErrSetupInProgress) {
		t.Fatal("a setup whose browser exited before the grace window still blocks a new one — " +
			"this is the wedge: no setup, no refresh, nothing, until Moombox restarts")
	}
	if err == nil || !strings.Contains(err.Error(), "start browser") {
		t.Fatalf("expected the attempt to reach its (deliberately impossible) launch, got %v", err)
	}
}

// TestAbandonedSetupStopsWedgingRefreshCookies is the same wedge on the path
// the user never sees. The periodic refresh declined on every tick for the rest
// of the run, so credentials expired with a perfectly capable refresher sitting
// idle behind a browser nobody was using.
//
// Asserted on RefreshResult.Ran, which is what separates the two outcomes that
// otherwise look alike: a DECLINE never looked at anything (Ran false), while a
// pass that ran and could not launch its browser still ran (Ran true). Checking
// err or AnyVerified instead would put the assertion downstream of a junction
// several mechanisms satisfy.
func TestAbandonedSetupStopsWedgingRefreshCookies(t *testing.T) {
	captureKills(t)
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	abandonedSetup(t, s, setupAbandonGrace+time.Second)

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookiesDetailed: %v", err)
	}
	if !result.Ran {
		t.Fatal("the refresh still declined — an abandoned setup keeps the periodic refresh " +
			"from ever running again")
	}
}

// TestAbandonedSetupStopsWedgingGetStatus covers the third consumer, and the
// one that decides what both UIs draw: SetupInProgress stayed true forever, so
// Settings kept showing a setup in flight with no browser anywhere.
func TestAbandonedSetupStopsWedgingGetStatus(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	abandonedSetup(t, s, setupAbandonGrace+time.Second)

	if s.GetStatus().SetupInProgress {
		t.Fatal("GetStatus still reports a setup in progress for a browser that exited " +
			"before the grace window — this is what leaves the UI stuck")
	}
}

// TestRefreshIsRefusedWhileAnExitedSetupIsStillRetained is trap 1, and it is a
// GUARD rather than a witness: the unfixed code refuses here too (it refuses
// forever). What it protects is the direction of the fix.
//
// The tempting simplification is to gate the refresh on "is a browser actually
// running", since the browser is demonstrably gone. It is gone during a normal
// Firefox finish too — FinishSetup closes the browser ITSELF and then reads the
// profile — so a live-gated refresh would launch a second headless browser at
// the same profile directory while that read and its merge into cookies.txt
// were still in flight. Two writers into one cookie store is the class of bug
// the previous arc was entirely about.
//
// Mutation-checked: replacing setupInProgressLocked() with
// setupBrowserLiveLocked() in RefreshCookiesDetailed makes this fail.
func TestRefreshIsRefusedWhileAnExitedSetupIsStillRetained(t *testing.T) {
	captureKills(t)
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	// Exited a second ago: a FinishSetup started just before that exit is very
	// plausibly still reading the profile right now.
	abandonedSetup(t, s, time.Second)

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookiesDetailed: %v", err)
	}
	if result.Ran {
		t.Fatal("a headless refresh launched into the profile a FinishSetup could still be " +
			"reading — the grace window exists precisely to prevent that second writer")
	}
	if !s.GetStatus().SetupInProgress {
		t.Fatal("the retained setup was reaped inside its own grace window")
	}
}

// TestFinishSetupCompletesInsideTheGraceWindow is the other half of the same
// guard, on the caller the window exists for.
//
// A status poll runs first, deliberately: GetStatus is the most frequent
// visitor to the reap, so if the reap ever fired on "the browser exited" alone
// this call would 404 instead of extracting the cookies the user just signed in
// for. Also a guard, not a witness — the unfixed code has no reap to get wrong.
func TestFinishSetupCompletesInsideTheGraceWindow(t *testing.T) {
	captureKills(t)
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	abandonedSetup(t, s, time.Second)

	if !s.GetStatus().SetupInProgress {
		t.Fatal("a status poll inside the grace window reaped a setup that is still finishable")
	}

	ytAuth, _, err := s.FinishSetup(context.Background())
	if err != nil {
		t.Fatalf("FinishSetup inside the grace window: %v", err)
	}
	if !ytAuth {
		t.Fatal("FinishSetup ran but reported no YouTube auth from a profile holding SAPISID + LOGIN_INFO")
	}
}

// TestFinishSetupIsRefusedOnceTheSetupHasBeenReaped pins trap 2: A DELIBERATE
// UX REGRESSION, NOT AN OVERSIGHT.
//
// Firefox's FinishSetup only reads the profile directory, so the browser being
// gone does not stop it — before this change, a user who closed the browser and
// came back an hour later could still click "I'm Logged In" and have it work.
// After the reap there is no slot to finish and the call is a 404.
//
// That trade is the whole point: the alternative is holding the slot open for
// that edge case, which is exactly the state that wedges every setup and every
// periodic refresh for the rest of the run. Do not "fix" this by widening
// FinishSetup's gate.
//
// A witness: against the unfixed code the status poll below still reports a
// setup in progress and the finish succeeds.
func TestFinishSetupIsRefusedOnceTheSetupHasBeenReaped(t *testing.T) {
	captureKills(t)
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	abandonedSetup(t, s, setupAbandonGrace+time.Second)

	if s.GetStatus().SetupInProgress {
		t.Fatal("the expired setup survived a status poll, so nothing was reaped")
	}

	_, _, err := s.FinishSetup(context.Background())
	if !errors.Is(err, ErrNoSetupInProgress) {
		t.Fatalf("a finish after the reap must answer ErrNoSetupInProgress, got %v", err)
	}
	if _, statErr := os.Stat(cookiePath); statErr == nil {
		t.Fatal("the refused finish still wrote cookies.txt")
	}
}

// TestCancelCatchesABrowserPublishedInsideTheLaunchWindow is the launch-window
// race, at the only place it can be observed.
//
// StartSetup claims the slot and only assigns setupProcess once the launcher
// has started the browser. A CancelSetup or Stop landing between the
// mid-preparation cancel check and that assignment used to sample a nil
// process, kill nothing and return — and the launcher then registered a browser
// into a slot nobody was watching, leaving the user a stuck wizard and an open
// browser window.
//
// The fix is the poll killRefreshProcess already carries for the refresh slot,
// so this stands where CancelSetup stands: claim held, no process yet, one
// published a few poll intervals later. Against the unfixed body the kill
// returns immediately and nothing is killed.
func TestCancelCatchesABrowserPublishedInsideTheLaunchWindow(t *testing.T) {
	killed := captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})

	s.mu.Lock()
	s.setupClaimed = true
	s.mu.Unlock()

	published := &os.Process{Pid: latePublishPid}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(3 * killProcessTreePollDelay)
		s.mu.Lock()
		s.setupProcess = published
		s.mu.Unlock()
	}()

	s.killSetupProcess()
	<-done

	if len(*killed) != 1 || (*killed)[0] != latePublishPid {
		t.Fatalf("the kill missed a browser the launcher published moments later: killed %v, want [%d]",
			*killed, latePublishPid)
	}
}

// TestKillSetupProcessCannotBlockOnALauncherThatNeverPublishes is the cap on
// the poll above. Stop() calls killSetupProcess on the way down, so a claim
// that never resolves — a launcher that errored before assigning — must not
// hold shutdown open indefinitely.
func TestKillSetupProcessCannotBlockOnALauncherThatNeverPublishes(t *testing.T) {
	killed := captureKills(t)

	t.Run("nothing claimed returns at once", func(t *testing.T) {
		s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
		start := time.Now()
		s.killSetupProcess()
		if elapsed := time.Since(start); elapsed > killProcessTreePollDelay {
			t.Fatalf("an empty slot must not be polled at all, took %s", elapsed)
		}
	})

	t.Run("an unresolved claim gives up at the cap", func(t *testing.T) {
		s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
		s.mu.Lock()
		s.setupClaimed = true
		s.mu.Unlock()

		start := time.Now()
		s.killSetupProcess()
		elapsed := time.Since(start)
		if elapsed < launchWindowKillBudget {
			t.Fatalf("gave up on the launch window after only %s", elapsed)
		}
		if elapsed > launchWindowKillBudget+2*time.Second {
			t.Fatalf("killSetupProcess blocked for %s — Stop() cannot wait that long", elapsed)
		}
	})

	if len(*killed) != 0 {
		t.Fatalf("nothing was ever published, yet a kill was issued for %v", *killed)
	}
}
