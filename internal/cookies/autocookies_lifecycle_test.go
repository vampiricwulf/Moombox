package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// cancellingDetector is the injection seam these tests use to land a cancel
// INSIDE StartSetup's preparation window.
//
// StartSetup claims the slot, then does browser detection, MkdirAll and an
// icacls shell-out before it re-checks the cancelled flag. detectBrowser is
// the first thing in that window and is already a test seam, so calling
// CancelSetup from inside it puts the cancel exactly where the real race puts
// it — after the claim, before the check — with no goroutines and no sleeps.
//
// The browser it hands back is a path inside a fresh temp dir, so it provably
// does not exist. That is deliberate: if the cancel were ever missed, the only
// thing downstream is a launch, and this makes that launch fail fast and
// loudly (as "start browser: …", which no assertion here accepts) instead of
// opening a real browser window on the machine running the tests.
//
// Each test using this costs ~2s, and the cost is an artifact of the fixture
// rather than a slow code path. CancelSetup's killSetupProcess polls the setup
// slot for launchWindowKillBudget so a cancel landing in the launch window
// still catches the browser the launcher is about to publish — and here the
// call is made from INSIDE StartSetup's own goroutine, so the claim it is
// waiting on can never resolve and the poll always runs to its cap. A real
// cancel arrives on another goroutine and returns as soon as the launcher
// publishes or abandons the slot.
func cancellingDetector(t *testing.T, s *AutoCookieService, cancelErr *error, calls *int) func() *DetectedBrowser {
	t.Helper()
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser.exe")
	return func() *DetectedBrowser {
		*calls++
		*cancelErr = s.CancelSetup()
		return &DetectedBrowser{
			Type: "chrome",
			Path: unlaunchable,
			Name: "unlaunchable test browser",
		}
	}
}

// TestCancelDuringStartSetupPreparationIsHonoured is S1, at the site the
// defect actually reaches a user.
//
// CancelSetup raised `cancelled` and then called cleanup(), and cleanup()
// reset `cancelled` to false. A COMPLETE cancel therefore erased its own flag
// microseconds after raising it, so StartSetup's mid-preparation check could
// only ever observe a cancel caught mid-flight — never one that had finished.
// The user pressed Cancel while the wizard said "starting browser…", the flag
// was wiped, and a browser they had just dismissed opened anyway.
//
// The assertion is on StartSetup's own return value rather than on the flag,
// because the flag is the mechanism and ErrSetupCancelled is the behaviour.
// Against the unfixed code this returns a "start browser: …" error from the
// launch that should never have been attempted.
func TestCancelDuringStartSetupPreparationIsHonoured(t *testing.T) {
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})

	var cancelErr error
	var detectCalls int
	s.detectBrowser = cancellingDetector(t, s, &cancelErr, &detectCalls)

	err := s.StartSetup("youtube")

	if detectCalls != 1 {
		t.Fatalf("fixture is broken — the cancel must be injected exactly once inside the "+
			"preparation window, detectBrowser ran %d times", detectCalls)
	}
	// The cancel landed while only the CLAIM was held: no setupProcess exists
	// yet, and S18's "nothing to cancel" must not swallow it.
	if cancelErr != nil {
		t.Fatalf("a cancel arriving while StartSetup holds the slot claim has a real setup to "+
			"abort and must succeed, got %v", cancelErr)
	}
	if !errors.Is(err, ErrSetupCancelled) {
		t.Fatalf("StartSetup ignored a completed cancel and carried on: got %v, want ErrSetupCancelled", err)
	}

	s.mu.Lock()
	proc, claimed := s.setupProcess, s.setupClaimed
	s.mu.Unlock()
	if proc != nil {
		t.Error("a cancelled StartSetup registered a browser process")
	}
	if claimed {
		t.Error("StartSetup's deferred claim release did not run — the slot is wedged and no " +
			"later setup can ever start")
	}
}

// TestCleanupDoesNotEraseTheCancelFlag pins the mechanism the test above
// exercises through behaviour, at the one line that used to break it.
//
// Kept separate and white-box on purpose: cleanup() runs on every setup exit
// path and Arc 3's remaining tasks restructure it, so the next
// implementer needs a failure that names the flag rather than one that reports
// a browser opening.
func TestCleanupDoesNotEraseTheCancelFlag(t *testing.T) {
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	s.setupClaimed = true // a StartSetup mid-preparation: something to cancel

	if err := s.CancelSetup(); err != nil {
		t.Fatalf("CancelSetup with a claim in flight: %v", err)
	}

	s.mu.Lock()
	cancelled := s.cancelled
	s.mu.Unlock()
	if !cancelled {
		t.Fatal("CancelSetup's own cleanup() erased the flag it had just raised — the cancel is " +
			"invisible to StartSetup by the time CancelSetup returns")
	}

	// And a cleanup() from any OTHER exit path must not clear it either: the
	// only place a cancel is consumed is StartSetup's slot claim.
	s.cleanup()
	s.mu.Lock()
	cancelled = s.cancelled
	s.mu.Unlock()
	if !cancelled {
		t.Error("a later cleanup() cleared the pending cancel; it must only be consumed at claim time")
	}
}

// TestStartSetupClaimConsumesAPendingCancel is the necessary other half of the
// test above, and the reason cleanup() is the wrong place for the reset rather
// than merely a place that also needs one.
//
// A flag that is never cleared is not a fix: the NEXT setup would inherit the
// previous one's cancel and refuse to start, forever. Claim time is where it
// is consumed, so a fresh StartSetup after a cancelled one must run.
func TestStartSetupClaimConsumesAPendingCancel(t *testing.T) {
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	s.setupClaimed = true
	if err := s.CancelSetup(); err != nil {
		t.Fatalf("CancelSetup with a claim in flight: %v", err)
	}
	s.setupClaimed = false // the cancelled StartSetup's defer would have done this

	// No browser on this host, so StartSetup stops at the detection step. That
	// is one step PAST the claim, which is all this needs to prove: reaching
	// ErrNoBrowserFound means the stale cancel did not turn the next setup away.
	s.detectBrowser = func() *DetectedBrowser { return nil }

	err := s.StartSetup("youtube")
	if errors.Is(err, ErrSetupCancelled) {
		t.Fatal("a consumed cancel leaked into the next setup — StartSetup's claim must clear it")
	}
	if !errors.Is(err, ErrNoBrowserFound) {
		t.Fatalf("StartSetup got past the claim but stopped somewhere unexpected: %v", err)
	}
	s.mu.Lock()
	cancelled := s.cancelled
	s.mu.Unlock()
	if cancelled {
		t.Error("the claim did not clear the previous attempt's cancel flag")
	}
}

// TestStopLatchesAgainstStartSetupAndRefresh is the other half of S1.
//
// Stop() set the same `cancelled` flag CancelSetup did, and cleanup() — which
// Stop itself calls, as its last act — wiped it. Shutdown therefore left no
// trace at all: a StartSetup or a periodic refresh arriving afterwards would
// happily launch a browser for a service that had already been torn down.
//
// `stopped` is a separate field precisely so this cannot happen again. It is a
// statement about the SERVICE, not about one setup attempt, so nothing may
// lower it — including the cleanup() inside Stop and any later one.
//
// detectBrowser is stubbed to nil throughout so that a latch failure surfaces
// as ErrNoBrowserFound rather than as a real browser window: the two outcomes
// are distinguishable, and only one of them opens Firefox on the test host.
func TestStopLatchesAgainstStartSetupAndRefresh(t *testing.T) {
	profileDir := t.TempDir()
	s := NewAutoCookieService(profileDir, "", NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }

	s.Stop()

	if err := s.StartSetup("youtube"); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("StartSetup after Stop: got %v, want ErrServiceStopped — a stopped service must "+
			"not begin a setup", err)
	}

	// The refresh gate declines rather than errors, matching the two gates
	// beside it: nothing was examined, so the pass has no verdict to report
	// and nothing to blame on the credentials. `Ran` is what separates the two
	// against the unfixed code — the profile dir exists, so an unlatched
	// service reaches importProfileCookies and comes back Ran=true with an
	// error.
	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Errorf("a declined refresh must not report an error, got %v", err)
	}
	if result.Ran {
		t.Error("RefreshCookiesDetailed did real work after Stop — the service launched or read " +
			"something it had been told to stop touching")
	}

	// The wrapper every whole-service caller actually uses (the periodic tick,
	// the startup seed, the Settings button, runCookieRecovery).
	if ok, wrapErr := s.RefreshCookies(context.Background()); ok || wrapErr != nil {
		t.Errorf("RefreshCookies after Stop = (%v, %v), want (false, nil)", ok, wrapErr)
	}

	// The heart of it: cleanup() runs on every setup exit path and Stop calls
	// one itself. It must not resurrect the service.
	s.cleanup()
	if err := s.StartSetup("youtube"); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("a cleanup() after Stop un-stopped the service: StartSetup got %v, want "+
			"ErrServiceStopped", err)
	}
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if !stopped {
		t.Error("the stopped latch was lowered by cleanup()")
	}
}

// TestFailedSetupExitDoesNotPoisonTheNextSetup is the hazard created by this
// commit rather than fixed by it, and it is aimed squarely at Arc 2's S9
// abort — cleanup()'s newest caller.
//
// cleanup() used to reset `cancelled` unconditionally, so every exit path got
// a free wipe. Removing that reset is the S1 fix, and it also removes the
// safety net. The invariant that replaces it is narrow and worth stating
// plainly, because the rest of Arc 3 restructures this same function: NO exit
// path other than a genuine CancelSetup or Stop may leave `cancelled` raised.
// StartSetup's claim would swallow a stray one today, which is exactly why a
// leak here would go unnoticed until a later task added a second reader of the
// flag and inherited a state machine that lies.
//
// The S9 abort is the case to check. It fires setError → cleanup() → error
// return when the existing cookies.txt
// cannot be read, and TestFinishSetupAbortsOnUnreadableExistingCookieFile pins
// its three guarantees (error returned, both platforms false, file untouched).
// Those are restated below so a break fails here too, then the invariant above
// is checked on top of them.
func TestFailedSetupExitDoesNotPoisonTheNextSetup(t *testing.T) {
	s := finishSetupService(t, youtubeAuthRows(), nopAutoCookieLogger{})
	if err := os.WriteFile(s.cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	failCookieRead(t, errors.New("permission denied (simulated)"))

	ytAuth, twAuth, err := s.FinishSetup(context.Background())

	// Arc 2's contract, restated here so a break in it fails at this test too
	// rather than only in the file that owns it.
	if !errors.Is(err, ErrCookieFileUnreadable) {
		t.Fatalf("the S9 abort no longer returns its discriminating error: %v", err)
	}
	if ytAuth || twAuth {
		t.Errorf("an aborted setup reported a platform authenticated: yt=%v tw=%v", ytAuth, twAuth)
	}
	if data, readErr := os.ReadFile(s.cookiePath); readErr != nil || string(data) != previousCookieFile {
		t.Errorf("the aborted setup touched cookies.txt (err %v):\ngot:  %q\nwant: %q",
			readErr, data, previousCookieFile)
	}

	// The new part.
	s.mu.Lock()
	cancelled, stopped := s.cancelled, s.stopped
	s.mu.Unlock()
	if cancelled {
		t.Error("the S9 abort left `cancelled` raised. Nothing consumed it and no user asked for " +
			"it; only CancelSetup and Stop may raise this flag")
	}
	if stopped {
		t.Error("a failed setup stopped the whole service")
	}

	// And the user-visible consequence: setup is still reachable afterwards.
	// Reaching browser detection is one step past the claim, which is all this
	// needs to show.
	s.detectBrowser = func() *DetectedBrowser { return nil }
	if err := s.StartSetup("youtube"); !errors.Is(err, ErrNoBrowserFound) {
		t.Fatalf("a setup after the abort could not get past the claim: %v", err)
	}
}

// TestCancelSetupReportsNothingToCancel is S18.
//
// CancelSetup returned nothing and the route answered {"success": true}
// unconditionally, so cancelling twice — or cancelling with no setup ever
// started — reported a cancel that never happened. errors.go had already
// documented ErrNoSetupInProgress as "returned by FinishSetup or CancelSetup";
// only the FinishSetup half was ever built.
func TestCancelSetupReportsNothingToCancel(t *testing.T) {
	t.Run("no setup ever started", func(t *testing.T) {
		s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
		if err := s.CancelSetup(); !errors.Is(err, ErrNoSetupInProgress) {
			t.Fatalf("CancelSetup with no setup: got %v, want ErrNoSetupInProgress", err)
		}
	})

	t.Run("second cancel", func(t *testing.T) {
		s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
		// Pid -1 is not a live process: killProcessTree's taskkill answers
		// "The process \"-1\" not found" and nothing on the host is touched.
		s.setupProcess = &os.Process{Pid: -1}

		if err := s.CancelSetup(); err != nil {
			t.Fatalf("the first cancel had a running setup to kill and must succeed, got %v", err)
		}
		if err := s.CancelSetup(); !errors.Is(err, ErrNoSetupInProgress) {
			t.Fatalf("the second cancel: got %v, want ErrNoSetupInProgress", err)
		}
	})

	t.Run("a claim in flight is something to cancel", func(t *testing.T) {
		// The distinction S18 turns on: no browser process exists between
		// StartSetup's gate and the launch, but there IS a setup to abort, and
		// treating it as "nothing to cancel" would drop the very cancel S1
		// exists to make observable.
		s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
		s.setupClaimed = true
		if err := s.CancelSetup(); err != nil {
			t.Fatalf("CancelSetup with a claim in flight: got %v, want nil", err)
		}
	})
}

// TestCancelSetupAgreesWithSetupInProgress pins CancelSetup's "nothing to
// cancel" test to GetStatus's SetupInProgress, which is what put the Cancel
// button on screen in the first place. If the two ever disagree, the UI offers
// a cancel the service answers 404 to, or hides one that would have worked.
func TestCancelSetupAgreesWithSetupInProgress(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*AutoCookieService)
	}{
		{"idle", func(*AutoCookieService) {}},
		{"claim held", func(s *AutoCookieService) { s.setupClaimed = true }},
		{"process running", func(s *AutoCookieService) { s.setupProcess = &os.Process{Pid: -1} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
			tc.setup(s)

			inProgress := s.GetStatus().SetupInProgress
			err := s.CancelSetup()
			cancellable := !errors.Is(err, ErrNoSetupInProgress)

			if inProgress != cancellable {
				t.Errorf("GetStatus reports SetupInProgress=%v but CancelSetup says cancellable=%v (err %v)",
					inProgress, cancellable, err)
			}
		})
	}
}
