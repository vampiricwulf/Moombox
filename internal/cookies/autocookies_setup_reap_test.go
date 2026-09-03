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

// jobReports fixes what the setup slot's Job Object says for one test, and
// restores the real probe afterwards.
//
// This is the seam that makes the reap testable at all. The real
// setupBrowserGone asks a Windows Job Object how many processes it still holds,
// which needs a launched browser — and no test here may launch one. The two
// booleans are the two axes that matter: whether the browser is gone, and
// whether anything is in a position to say.
func jobReports(t *testing.T, gone, known bool) {
	t.Helper()
	prev := setupBrowserGone
	setupBrowserGone = func(*processJob) (bool, bool) { return gone, known }
	t.Cleanup(func() { setupBrowserGone = prev })
}

// abandonedSetup puts the service into the exact state a walked-away-from setup
// leaves behind: a browser process registered in the slot, the wait goroutine
// having recorded its exit `exitedAgo` in the past, and nothing at all having
// cleared setupProcess.
//
// That last part is the defect. The wait goroutines have always set
// browserExited; no path ever acted on it, so the slot stayed occupied for the
// life of the process and SetupInProgress stayed true with it.
//
// It also declares the browser genuinely gone, because a stamped exit is NOT
// on its own enough for the reap to act — see setupBrowserGone. The Firefox
// browser record now describes a real Windows state: startFirefoxSetup creates
// and stores a job, so a Firefox slot can answer the probe. The record is here
// because the two FinishSetup tests need the Firefox read path to have a
// profile to read.
//
// IT INSTALLS NO JOB AT ALL — not a fake one, which is what this said before.
// jobReports replaces the probe outright, so s.setupJob stays nil and every
// answer about it comes from the stub; nothing here exercises a real handle.
// The tests that do are Windows-only and live in
// autocookies_setup_reap_windows_test.go. That matters to anyone adding a case
// here: code reading s.setupJob directly rather than going through the probe
// sees nil, and will behave as it does with no Job Object.
func abandonedSetup(t *testing.T, s *AutoCookieService, exitedAgo time.Duration) *os.Process {
	t.Helper()
	jobReports(t, true, true)
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

// TestReapWillNotCloseAJobThatStillHasLiveProcesses is the fix for the one way
// this change could destroy the thing it exists to protect.
//
// `browserExited` records that the process MOOMBOX SPAWNED exited. That is not
// the browser. Firefox and its forks hand off to a separate process and the
// launcher exits in ~170ms — measured, and written down in drainJob's doc,
// where closing the Job Object at that moment was found to kill the browser
// mid-load. A Chromium binary behind a shim, a `.bat`, msedge_proxy.exe, a snap
// wrapper, or any custom path accepted through Settings does the same.
//
// So the slot below is the realistic one: the launcher exited well over an hour
// ago, and the browser it started is still running with the user's login in it.
// A reap here closes the Job Object, KILL_ON_JOB_CLOSE fires, and the window
// they are typing into disappears. That is precisely the outcome the
// no-sleeping-reaper ruling exists to prevent, arriving through a different
// door.
//
// Asserted on the slot surviving a status poll — the most frequent reap
// trigger. Against the first cut of this change (346733d) the poll reaps.
func TestReapWillNotCloseAJobThatStillHasLiveProcesses(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	abandonedSetup(t, s, time.Hour)
	// Overrides the helper's default: the launcher is gone, but the job still
	// holds the real browser.
	jobReports(t, false, true)

	if !s.GetStatus().SetupInProgress {
		t.Fatal("the reap released a setup whose Job Object still holds live processes — " +
			"cleanupLocked closes that handle, and KILL_ON_JOB_CLOSE kills the browser " +
			"the user is signed into")
	}

	s.mu.Lock()
	proc, job := s.setupProcess, s.setupJob
	s.mu.Unlock()
	if proc == nil {
		t.Fatal("the setup slot was cleared out from under a running browser")
	}
	_ = job
}

// TestReapWillNotFireWhenNothingCanSayTheBrowserIsGone is the same rule for the
// case where there is no evidence at all rather than evidence of life.
//
// `known == false` is what the probe returns with no job (a launch where
// newProcessJob or its assign failed, on either family), on the platforms
// whose processJob cannot count — darwin and the fallback build — and on
// Linux where the group could not be adopted or /proc cannot be read.
// drainJob draws the identical line on the same syscall: a zero from something
// that cannot count means "nothing was waited on", not "the browser finished".
//
// Refusing to reap there costs the wedge staying put on those paths, which is
// exactly the pre-existing behaviour — and it was EVERY Linux and Docker path
// until the process-group arc gave job_linux.go a real count. Reaping on no
// evidence would instead destroy live setups 60 seconds in. The first is a
// gap; the second is a regression.
func TestReapWillNotFireWhenNothingCanSayTheBrowserIsGone(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	abandonedSetup(t, s, time.Hour)
	jobReports(t, false, false) // no job, or a job that cannot count

	if !s.GetStatus().SetupInProgress {
		t.Fatal("the reap released a setup on no evidence at all — a launcher exit is not " +
			"a browser exit, and where nothing can tell the two apart the reap must not act")
	}
}

// TestTheGraceIsMeasuredFromTheLastTimeTheSetupWasSeenAlive covers the reap's
// one write.
//
// For a browser that outlives its launcher, setupRetainedSince is stamped at
// the launcher's exit — seconds after the setup starts. Left alone, the browser
// would burn its whole window while still running, and the moment it finally
// closed the very next poll would reap with no grace left for the finish the
// user is about to ask for. The reap therefore re-arms the clock every time it
// finds the setup alive.
func TestTheGraceIsMeasuredFromTheLastTimeTheSetupWasSeenAlive(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	abandonedSetup(t, s, time.Hour)
	jobReports(t, false, true) // still running, an hour after the launcher went

	if !s.GetStatus().SetupInProgress {
		t.Fatal("fixture: a live setup must survive a poll")
	}

	// The browser closes. The stamp from an hour ago must not still be the one
	// the grace is measured from.
	jobReports(t, true, true)
	if !s.GetStatus().SetupInProgress {
		t.Fatal("the setup was reaped the instant its browser closed, with none of the grace " +
			"window left — an hour-old launcher exit is not when the browser went away")
	}
}

// TestSetupBrowserGoneRefusesToAnswerWithoutAJobObject pins the real probe's
// default, since every other test in this file replaces it.
//
// The `known` half is the whole contract: no job, or a job on a platform whose
// processJob cannot count, must answer "no idea" rather than "empty".
func TestSetupBrowserGoneRefusesToAnswerWithoutAJobObject(t *testing.T) {
	gone, known := setupBrowserGone(nil)
	if known {
		t.Error("setupBrowserGone claimed to know something with no Job Object to ask")
	}
	if gone {
		t.Error("setupBrowserGone reported a browser gone with no Job Object to ask")
	}
}

// TestCancelSetupGateIsDeliberatelyWiderThanSetupInProgress pins the divergence
// F-3 named: CancelSetup's gate is NOT setupInProgressLocked, and it must stay
// a STRICT superset of it.
//
// Two obligations, and only one of them is the obvious one:
//
//   - ⊇ : a cancel must never answer "nothing to cancel" while the dialog is
//     still showing the Cancel button that produced it. The first three rows.
//   - ≠ : and it must still succeed on a slot setupInProgressLocked has stopped
//     advertising — an expired setup nothing has reaped yet. THAT cancel is
//     what tears the dead slot down, and answering 404 while leaving state
//     behind would be worse. The last row.
//
// THE LAST ROW IS THE POINT. An earlier version of this test had only the first
// three, and every one of them is satisfied by BOTH predicates — so it passed
// with CancelSetup's gate mutated to setupInProgressLocked(), which is the exact
// change its own doc claimed to forbid. That is this plan's junction defect
// again: an assertion sited where two mechanisms agree. Mutation-checked now,
// and it fails.
//
// `advertised` is read straight from setupInProgressLocked rather than through
// GetStatus, because GetStatus reaps — on the last row that would clear the slot
// before CancelSetup ever saw it, and the row would prove nothing.
//
// A guard rather than a witness, and its evidence is the mutation above rather
// than a run against an earlier commit: setupInProgressLocked did not exist at
// 6b00559, so there is no version of this test that both compiles there and
// asserts what this one asserts.
func TestCancelSetupGateIsDeliberatelyWiderThanSetupInProgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		// exitedAgo/gone/known place the slot in one lifecycle state.
		exitedAgo time.Duration
		gone      bool
		known     bool
		// advertised is what setupInProgressLocked says about that state — the
		// predicate CancelSetup must NOT be narrowed to.
		advertised bool
	}{
		{"a browser still running", time.Hour, false, true, true},
		{"an exited browser inside its grace window", time.Second, true, true, true},
		{"a launcher exit nothing can corroborate", time.Hour, false, false, true},
		{"an expired slot nothing has reaped yet", time.Hour, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureKills(t)
			s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
			abandonedSetup(t, s, tc.exitedAgo)
			jobReports(t, tc.gone, tc.known)

			s.mu.Lock()
			advertised := s.setupInProgressLocked()
			s.mu.Unlock()
			if advertised != tc.advertised {
				t.Fatalf("fixture: setupInProgressLocked = %v, want %v — this row is not "+
					"exercising the state it is named for", advertised, tc.advertised)
			}

			if err := s.CancelSetup(); err != nil {
				if tc.advertised {
					t.Fatalf("the UI is showing a Cancel button for this state and the cancel "+
						"behind it reported nothing to cancel: %v", err)
				}
				t.Fatalf("CancelSetup was narrowed to setupInProgressLocked. An expired slot "+
					"nothing has reaped yet still holds state, and the cancel that would have "+
					"cleared it now declines instead: %v", err)
			}

			// The cancel is only useful if it actually emptied the slot.
			s.mu.Lock()
			proc := s.setupProcess
			s.mu.Unlock()
			if proc != nil {
				t.Fatal("CancelSetup reported success and left the setup slot occupied")
			}
		})
	}
}

// --- AbandonSetup: the unload beacon's endpoint ---

// TestAbandonLeavesTheBrowserAloneWhereTheReapCanJudgeIt is the finding the arc
// review named, at the site that produced it.
//
// The Web beacon fires the instant the dashboard tab unloads — and the setup
// flow's own instructions send the user away from that tab to go and sign in,
// so closing the now-idle tab mid-login is an entirely natural act. While the
// beacon posted /cancel that closed the browser they were typing into: harmless
// when it was written, because a Firefox setup had no Job Object for a cancel to
// reach, and a remote kill on the default Windows path the moment S5 gave it
// one.
//
// The slot below is that exact moment: a launcher long gone (Firefox exits
// ~170ms in) and a job that says the real browser is still running. Nothing may
// be killed and the slot must survive — the reap owns this one, and it cannot
// fire while a login is in progress because it is keyed on the browser actually
// being gone.
func TestAbandonLeavesTheBrowserAloneWhereTheReapCanJudgeIt(t *testing.T) {
	killed := captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	abandonedSetup(t, s, time.Hour)
	jobReports(t, false, true) // queryable, and the browser is still in the job

	released, err := s.AbandonSetup()
	if err != nil {
		t.Fatalf("AbandonSetup: %v", err)
	}
	if released {
		t.Fatal("the beacon released a slot whose Job Object still holds a live browser — " +
			"cleanupLocked closes that handle and KILL_ON_JOB_CLOSE takes the window the " +
			"user is signing into")
	}
	if len(*killed) != 0 {
		t.Fatalf("a tab unload killed something: %v. A click is consent; an unload is not", *killed)
	}

	s.mu.Lock()
	proc := s.setupProcess
	s.mu.Unlock()
	if proc == nil {
		t.Fatal("the setup slot was torn down out from under a running browser")
	}
}

// TestAbandonReleasesTheSlotWhereTheReapNeverFires is the other arm, and the
// reason the beacon still exists at all.
//
// Where setupBrowserGone cannot answer — no job, a platform with no primitive
// at all (darwin, the fallback build), or a Linux group that could not be
// adopted or whose /proc cannot be read — the reap can NEVER release the
// slot, so this beacon is the only thing that does. Releasing is also safe
// exactly there: with nothing tracking the browser, clearing the slot closes
// no handle and kills nothing.
//
// So the call is redundant wherever a group or a Job Object was adopted —
// Windows, and Linux since the process-group reap — and load-bearing wherever
// nothing was. Both halves are asserted, here and above, because deleting
// either one is a live regression: drop the deferral and the kill comes back,
// drop this and an unanswerable platform wedges until restart.
func TestAbandonReleasesTheSlotWhereTheReapNeverFires(t *testing.T) {
	killed := captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	abandonedSetup(t, s, time.Hour)
	jobReports(t, false, false) // nothing can say — no job, or no job primitive

	released, err := s.AbandonSetup()
	if err != nil {
		t.Fatalf("AbandonSetup: %v", err)
	}
	if !released {
		t.Fatal("the beacon released nothing on a platform whose reap can never fire — " +
			"the setup slot wedges every later setup and every periodic refresh until restart")
	}
	if len(*killed) != 0 {
		t.Fatalf("releasing the slot killed %v. Releasing is only safe here BECAUSE "+
			"nothing is tracking the browser; the release must not go looking for it", *killed)
	}

	s.mu.Lock()
	proc := s.setupProcess
	s.mu.Unlock()
	if proc != nil {
		t.Fatal("AbandonSetup reported a release and left the slot occupied")
	}
}

// TestAbandonAnswersAMissingSetupLikeCancelDoes keeps the two endpoints
// agreeing about what "there was nothing here" looks like, so the route can
// answer both with one arm.
func TestAbandonAnswersAMissingSetupLikeCancelDoes(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})

	released, err := s.AbandonSetup()
	if !errors.Is(err, ErrNoSetupInProgress) {
		t.Fatalf("AbandonSetup with no setup: got %v, want ErrNoSetupInProgress", err)
	}
	if released {
		t.Fatal("AbandonSetup reported releasing a setup that never existed")
	}
	if cancelErr := s.CancelSetup(); !errors.Is(cancelErr, ErrNoSetupInProgress) {
		t.Fatalf("the two endpoints disagree about a missing setup: abandon %v, cancel %v",
			err, cancelErr)
	}
}

// restoreRealProbe puts the genuine setupBrowserGone back for one test, undoing
// a jobReports stub installed earlier in the same test (abandonedSetup installs
// one). Used by the Windows tests that want the real syscall.
func restoreRealProbe(t *testing.T) {
	t.Helper()
	stub := setupBrowserGone
	setupBrowserGone = realSetupBrowserGone
	t.Cleanup(func() { setupBrowserGone = stub })
}
