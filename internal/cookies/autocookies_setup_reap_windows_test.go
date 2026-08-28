//go:build windows

package cookies

import (
	"strings"
	"testing"
	"time"
)

// TestReapClosesTheFirstAttemptsJobObject is the Job Object half of A1, and it
// is Windows-only because the handle it inspects only exists there (Linux and
// the fallback build both use an empty no-op struct).
//
// WHAT IT PINS, EXACTLY: the REAP closes the abandoned attempt's Job Object
// handle, so that by the time a second StartSetup reaches the launcher there is
// no live handle left to overwrite. Before the reap existed, the only thing
// stopping a second attempt from dropping a live handle was that a second
// attempt could never start — the wedge was holding the leak shut. A dropped
// handle leaks the browser with it: nothing else holds a reference, so
// KILL_ON_JOB_CLOSE never fires and the orphan survives until Moombox exits.
//
// WHAT IT DOES NOT PIN, stated because an earlier version of this comment
// claimed otherwise: the guard inside adoptSetupJobLocked, which closes a
// pre-existing s.setupJob before installing a new one. Deleting that guard
// leaves this test PASSING — verified — because the reap has already closed and
// nil'd the handle before the launcher gets there. The assertion would sit
// downstream of a junction two mechanisms satisfy. Same for
// TestASecondFirefoxSetupDoesNotLeakTheFirstsJobObject below, which reaches the
// same junction from the other family.
//
// That guard has its own test now — TestAdoptSetupJobLockedClosesTheHandleItReplaces
// — which became possible when the two launchers stopped carrying a copy each
// and started sharing one. Before that it sat after cmd.Start() succeeded, so
// reaching it needed a process that actually launches.
//
// The assertion is on the first job's handle, which close() zeroes. Against the
// unfixed code StartSetup returns ErrSetupInProgress and the handle is still
// open.
func TestReapClosesTheFirstAttemptsJobObject(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = unlaunchableChromium(t)

	first, err := newProcessJob()
	if err != nil {
		t.Fatalf("create a Job Object to stand in for the first attempt's: %v", err)
	}
	// No process is ever assigned to it, so closing it kills nothing — the
	// handle is the only thing under test.
	t.Cleanup(first.close)

	abandonedSetup(t, s, setupAbandonGrace+time.Second)
	s.mu.Lock()
	s.setupJob = first
	s.mu.Unlock()

	startErr := s.StartSetup("youtube")
	if startErr == nil || !strings.Contains(startErr.Error(), "start browser") {
		t.Fatalf("expected the second attempt to reach its impossible launch, got %v", startErr)
	}

	s.mu.Lock()
	handle := first.handle
	s.mu.Unlock()
	if handle != 0 {
		t.Fatal("the second attempt replaced the first attempt's Job Object without closing it — " +
			"the handle leaks and the browser it holds outlives every cleanup")
	}
}

// TestSetupBrowserGoneReadsARealJobObject exercises the probe's syscall path,
// which every other test in this arc replaces with jobReports.
//
// An empty job is the only half that can be tested without launching
// something: a job holding live processes needs a real process in it, and
// nothing in this package may start one. So this pins that the query runs, does
// not error, and reports `known` — the half that decides whether the reap is
// allowed to act at all.
func TestSetupBrowserGoneReadsARealJobObject(t *testing.T) {
	job, err := newProcessJob()
	if err != nil {
		t.Fatalf("create a Job Object: %v", err)
	}
	t.Cleanup(job.close)

	gone, known := setupBrowserGone(job)
	if !known {
		t.Fatal("QueryInformationJobObject could not answer for a job this process just " +
			"created — the reap will never fire on Windows if this is right")
	}
	if !gone {
		t.Fatal("a Job Object with nothing assigned to it reported live processes")
	}

	// A closed handle degrades to "no answer" rather than lying: activeProcesses
	// short-circuits on handle == 0, which would read as an empty job if the
	// probe did not distinguish. It does not, because a zeroed handle is a job
	// nobody can ask.
	job.close()
	if gone, known = setupBrowserGone(job); known && gone {
		t.Fatal("a closed Job Object reported the browser gone — that is a handle nobody " +
			"can query, not evidence of an empty job")
	}
}

// --- the Firefox setup path's own Job Object -------------------------------
//
// Everything below drives a REAL StartSetup down the Firefox branch with the
// test binary standing in for the browser (see TestMain), and reads a REAL Job
// Object afterwards. That is deliberate and it is the only way these claims can
// be made: the arc's other reap tests replace setupBrowserGone with jobReports,
// which short-circuits the exact mechanism this task adds. A jobReports-based
// test of the Firefox path passes identically with and without the fix.
//
// No browser is ever launched. The stand-in is this test binary, and the
// process it hands off to is killed by closing the job handle — never by name,
// and never by a PID typed into taskkill.

// startFakeFirefoxSetup runs StartSetup down the Firefox path and returns once
// the launcher has exited, which is the state every assertion here is about.
//
// The t.Cleanup it registers closes the setup Job Object, and on the handoff
// mode that is what terminates the stand-in browser.
func startFakeFirefoxSetup(t *testing.T, s *AutoCookieService, mode string) {
	t.Helper()
	t.Setenv(fakeBrowserModeEnv, mode)
	s.detectBrowser = func() *DetectedBrowser { return fakeBrowser(t) }
	if err := s.StartSetup("youtube"); err != nil {
		t.Fatalf("StartSetup down the Firefox path: %v", err)
	}
	t.Cleanup(s.cleanup)
	waitForLauncherExit(t, s)
}

// waitForLauncherExit blocks until the wait goroutine has recorded the spawned
// process's exit. The generous deadline is for a cold process launch on a busy
// machine; the stub itself exits in milliseconds.
func waitForLauncherExit(t *testing.T, s *AutoCookieService) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		exited := s.browserExited
		s.mu.Unlock()
		if exited {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the stand-in launcher never exited — the fixture is not in the state these tests need")
}

// setupJobActiveProcesses reads the live count out of the setup slot's job.
func setupJobActiveProcesses(t *testing.T, s *AutoCookieService) int {
	t.Helper()
	s.mu.Lock()
	job := s.setupJob
	s.mu.Unlock()
	if job == nil {
		t.Fatal("the setup slot holds no Job Object")
	}
	active, err := job.activeProcesses()
	if err != nil {
		t.Fatalf("query the setup Job Object: %v", err)
	}
	return active
}

// expireGrace winds setupRetainedSince back past setupAbandonGrace.
//
// Every test below needs it, and for the same reason: with the grace window
// still open, setupRetainedLocked holds the slot on its own and the Job
// Object's answer is never the thing being observed. That is the junction this
// arc keeps tripping over, so it is closed in the fixture rather than argued
// about in a comment.
func expireGrace(s *AutoCookieService) {
	s.mu.Lock()
	s.setupRetainedSince = time.Now().Add(-2 * setupAbandonGrace)
	s.mu.Unlock()
}

// TestFirefoxSetupIsTrackedByAJobObject is S5 itself.
//
// startFirefoxSetup used to create no Job Object at all, which left
// setupBrowserGone with nothing to interrogate on the most common Windows path
// — knownBrowsers puts the whole Firefox family ahead of every Chromium entry,
// and Edge is excluded from default-browser promotion, so this is the path for
// anyone who merely has Firefox installed. The abandoned-setup reap therefore
// could not fire there, whatever the reap itself did.
//
// A witness: against b9a23a4 s.setupJob is nil after a Firefox setup, and the
// first assertion below is the one that fires.
func TestFirefoxSetupIsTrackedByAJobObject(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	startFakeFirefoxSetup(t, s, fakeBrowserHandoff)

	s.mu.Lock()
	job := s.setupJob
	s.mu.Unlock()

	if job == nil {
		t.Fatal("a Firefox setup stored no Job Object — nothing can say whether its browser " +
			"is still running, so the abandoned-setup reap can never fire on this path")
	}
	if !job.queryable() {
		t.Fatal("the Firefox setup's Job Object cannot be queried, which reads to the probe " +
			"as no job at all")
	}
	if _, known := setupBrowserGone(job); !known {
		t.Fatal("the probe still refuses to answer for a Firefox setup — the reap stays dead")
	}
}

// TestANormalFirefoxSetupSurvivesItsLaunchersExit is the trap this task creates
// and must not fall into.
//
// A Firefox launcher hands off to the real browser and exits in ~170ms —
// measured, and recorded in drainJob's doc. Giving the setup path a job makes
// the reap live on that path for the first time, so a reap that keyed on the
// spawned process exiting would now close the Job Object of every normal
// Firefox setup a minute after the user started typing their password, and
// KILL_ON_JOB_CLOSE would take the window with it. This turns "the reap is
// dead on Firefox" into "the reap kills Firefox setups", which is far worse
// than the bug.
//
// The grace window is expired first, so the ONLY thing that can hold the slot
// here is the job reporting a live process. The launcher is gone; the stand-in
// browser it handed off to is not.
//
// WHAT ITS EVIDENCE IS, since the reap assertion alone would be satisfied by
// two different mechanisms. Against b9a23a4 the assertion passes for the wrong
// reason — no job, the probe answers "no idea", and the slot survives on that.
// Verified, by deleting the fixture check below and running it there. So the
// fixture check is the load-bearing half: it fails at b9a23a4 ("the setup slot
// holds no Job Object"), and it fails again under the mutation that matters
// here — deleting the job.assign call while keeping the job, which is the
// half-done version of this commit and leaves a live handle tracking nothing.
// A handle that tracks nothing reports an EMPTY job, and an empty job is
// exactly the licence the reap acts on.
func TestANormalFirefoxSetupSurvivesItsLaunchersExit(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	startFakeFirefoxSetup(t, s, fakeBrowserHandoff)
	expireGrace(s)

	if active := setupJobActiveProcesses(t, s); active == 0 {
		t.Fatal("fixture: the stand-in browser is not in the job, so this run cannot tell a " +
			"reap that respects a live browser from one that does not")
	}

	// GetStatus is the reap's most frequent caller — both UIs poll it while the
	// cookie dialog is open, and the TUI polls it with no dialog at all.
	if !s.GetStatus().SetupInProgress {
		t.Fatal("a status poll released a Firefox setup whose browser is still running — " +
			"cleanupLocked closes the Job Object and KILL_ON_JOB_CLOSE kills the window the " +
			"user is signing in to")
	}

	s.mu.Lock()
	proc := s.setupProcess
	s.mu.Unlock()
	if proc == nil {
		t.Fatal("the setup slot was cleared out from under a running browser")
	}
	if active := setupJobActiveProcesses(t, s); active == 0 {
		t.Fatal("the stand-in browser was killed by a poll that should have left it alone")
	}
}

// TestAFirefoxSetupIsReapedOnceItsBrowserIsGone is the payoff: with a job in the
// slot, an abandoned Firefox setup finally releases it.
//
// The stub exits without handing anything off, which is a user who closed the
// browser (or never had one open past the launcher) — the job genuinely empties
// rather than merely losing its launcher. Grace expired, so the reap is free to
// act on that.
//
// A witness: against b9a23a4 there is no job, setupBrowserGone answers "no
// idea", and SetupInProgress stays true forever. That is the wedge A1 was
// reported as, still fully intact on the Firefox path after Task 2.
func TestAFirefoxSetupIsReapedOnceItsBrowserIsGone(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	startFakeFirefoxSetup(t, s, fakeBrowserSilent)

	if active := setupJobActiveProcesses(t, s); active != 0 {
		t.Fatalf("fixture: the job still holds %d process(es), so this is not the "+
			"abandoned case", active)
	}
	expireGrace(s)

	if s.GetStatus().SetupInProgress {
		t.Fatal("an abandoned Firefox setup still reports a setup in progress — this is the " +
			"wedge: no setup, no periodic refresh, nothing, until Moombox restarts")
	}

	s.mu.Lock()
	proc, job := s.setupProcess, s.setupJob
	s.mu.Unlock()
	if proc != nil || job != nil {
		t.Fatal("the reap stopped advertising the setup but left its state in the slot")
	}
}

// TestASecondFirefoxSetupDoesNotLeakTheFirstsJobObject is the Firefox half of
// what TestReapClosesTheFirstAttemptsJobObject pins for Chromium, and it is
// worth having separately because the first attempt's handle here is created by
// the code under test rather than injected by the test.
//
// A dropped handle leaks the browser with it: nothing else holds a reference,
// so KILL_ON_JOB_CLOSE never fires and the orphan survives until Moombox exits.
//
// Note which mechanism actually closes it — the reap, via cleanupLocked, before
// the second launcher runs. adoptSetupJobLocked's own guard is downstream of
// that junction and is covered by its own test below.
func TestASecondFirefoxSetupDoesNotLeakTheFirstsJobObject(t *testing.T) {
	captureKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	startFakeFirefoxSetup(t, s, fakeBrowserSilent)

	s.mu.Lock()
	first := s.setupJob
	s.mu.Unlock()
	if first == nil || !first.queryable() {
		t.Fatal("fixture: the first attempt stored no queryable Job Object")
	}
	expireGrace(s)

	// The cleanup registered by startFakeFirefoxSetup closes whatever is in the
	// slot when the test ends, which is the second attempt's job by then.
	if err := s.StartSetup("youtube"); err != nil {
		t.Fatalf("the second Firefox setup: %v", err)
	}
	waitForLauncherExit(t, s)

	if first.handle != 0 {
		t.Fatal("the second attempt replaced the first attempt's Job Object without closing " +
			"it — the handle leaks and the browser it holds outlives every cleanup")
	}
	s.mu.Lock()
	second := s.setupJob
	s.mu.Unlock()
	if second == nil || second == first || !second.queryable() {
		t.Fatal("the second Firefox setup did not install a Job Object of its own")
	}
}

// TestAdoptSetupJobLockedClosesTheHandleItReplaces covers the guard both
// launchers now share, at the only place it can be reached deterministically.
//
// Reaching it through a launcher is not possible: StartSetup's reap clears the
// slot first, so the guard is defence-in-depth against an invariant violation
// that is unreachable by construction — hence the Warn it logs. Sharing one
// implementation between the two families is what makes it directly testable,
// and it is why the two copies can no longer drift apart.
//
// Mutation-checked: dropping the close() inside adoptSetupJobLocked fails this
// test and nothing else in the package.
func TestAdoptSetupJobLockedClosesTheHandleItReplaces(t *testing.T) {
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})

	first, err := newProcessJob()
	if err != nil {
		t.Fatalf("create the first Job Object: %v", err)
	}
	second, err := newProcessJob()
	if err != nil {
		t.Fatalf("create the second Job Object: %v", err)
	}
	// Nothing is ever assigned to either, so closing them kills nothing.
	t.Cleanup(first.close)
	t.Cleanup(second.close)

	s.mu.Lock()
	s.adoptSetupJobLocked(first)
	s.adoptSetupJobLocked(second)
	stored := s.setupJob
	s.mu.Unlock()

	if first.handle != 0 {
		t.Fatal("adoptSetupJobLocked installed a new Job Object over a live handle — that " +
			"handle, and the browser it holds, are now unreachable")
	}
	if stored != second {
		t.Fatal("adoptSetupJobLocked did not store the job it was given")
	}
}
