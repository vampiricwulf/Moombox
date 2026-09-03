package cookies

import (
	"errors"
	"os"
	"testing"
)

// trackedKillCall is what captureTrackedKills records for one
// killTrackedProcesses call: the job it was asked about, AND whether that job
// was still queryable() at the moment of the call.
//
// The second field is the whole fix. A recorder that keeps only the pointer
// cannot tell "asked, then closed" from "closed, then asked" — job.close() on
// Windows zeroes the handle in place, so the SAME pointer reads queryable()
// true before the close and false after it. Capturing queryable() AT CALL TIME
// is what makes a close-before-ask reorder observable.
type trackedKillCall struct {
	job       *processJob
	queryable bool
}

// captureTrackedKills swaps the "finish off what the job still tracks" hook for
// a recorder. On Windows the real hook is a no-op — close() there is
// KILL_ON_JOB_CLOSE — so these tests are about WHO ASKS, which is the half that
// has to be right before job_linux.go binds anything to it.
func captureTrackedKills(t *testing.T) *[]trackedKillCall {
	t.Helper()
	prev := killTrackedProcesses
	asked := []trackedKillCall{}
	killTrackedProcesses = func(j *processJob) error {
		asked = append(asked, trackedKillCall{job: j, queryable: j.queryable()})
		return nil
	}
	t.Cleanup(func() { killTrackedProcesses = prev })
	return &asked
}

// TestCleanupAsksForTheKillBeforeItDropsTheJob is R4 at the site that matters
// most. On Windows cleanupLocked's close IS the kill, which is why every caller
// of killSetupProcess calls cleanup immediately afterwards and why
// killSetupProcess is allowed to decline a reaped PID. On Linux the close
// forgets an integer, so unless this call is here — and BEFORE the field is
// nilled — a cancelled setup leaves the browser running forever.
//
// Mutation: move the call after `s.setupJob = nil` and it receives nil; delete
// it and the recorder is empty, which is the wedge on Linux.
func TestCleanupAsksForTheKillBeforeItDropsTheJob(t *testing.T) {
	asked := captureTrackedKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	// A real job, not a bare &processJob{}: a zero-value job's queryable() is
	// already false, so it cannot tell a correct "ask before close" from a
	// mutated "close before ask" apart. A real handle starts queryable() true
	// and only close() can turn that false.
	job, err := newProcessJob()
	if err != nil {
		t.Fatalf("newProcessJob: %v", err)
	}
	t.Cleanup(job.close)
	s.mu.Lock()
	s.setupJob = job
	s.mu.Unlock()

	s.cleanup()

	if len(*asked) != 1 || (*asked)[0].job != job {
		t.Fatalf("cleanup asked to kill %v, want exactly the job it was about to drop", *asked)
	}
	if !(*asked)[0].queryable {
		t.Fatal("cleanup asked for the kill AFTER the job was already closed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setupJob != nil {
		t.Fatal("cleanup left the job in the slot")
	}
}

// TestAdoptClosingAStaleJobAsksForTheKill covers the other close that is
// deliberately a kill on Windows: a handle an earlier attempt left behind, held
// by nothing else, whose only possible occupant is a setup browser the gate has
// already declared over.
//
// Mutation: delete the call and a Linux install that somehow reached adopt with
// a live stale group leaks that browser for the life of the process.
func TestAdoptClosingAStaleJobAsksForTheKill(t *testing.T) {
	asked := captureTrackedKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	// Real, for the same reason as TestCleanupAsksForTheKillBeforeItDropsTheJob:
	// only a live handle can distinguish "asked before close" from "asked
	// after". fresh is never closed or asked about, so it stays a bare value.
	stale, err := newProcessJob()
	if err != nil {
		t.Fatalf("newProcessJob: %v", err)
	}
	t.Cleanup(stale.close)
	fresh := &processJob{}
	s.mu.Lock()
	s.setupJob = stale
	s.adoptSetupJobLocked(fresh)
	got := s.setupJob
	s.mu.Unlock()

	if len(*asked) != 1 || (*asked)[0].job != stale {
		t.Fatalf("adopt asked to kill %v, want exactly the stale job", *asked)
	}
	if !(*asked)[0].queryable {
		t.Fatal("adopt asked for the kill AFTER the job was already closed")
	}
	if got != fresh {
		t.Fatal("adopt did not install the new job")
	}
}

// TestTrackedSetupJobDoesNotAskForAKillWhenTheAssignFailed is the site that
// must NOT kill, and it is the reason a Linux close() forgets instead of
// killing.
//
// trackedSetupJob drops a job whose assign failed, because a live handle
// tracking nothing reads as an EMPTY job and would license a premature release.
// On Windows that drop is free — the job holds nothing. If close() killed on
// Linux, or if this site asked for a kill, the group it would SIGKILL is the
// browser window the user is signed into, lost because a bookkeeping call went
// wrong.
//
// Mutation: add killTrackedProcesses to trackedSetupJob's drop path and this
// test fails, which is exactly what it is for.
func TestTrackedSetupJobDoesNotAskForAKillWhenTheAssignFailed(t *testing.T) {
	asked := captureTrackedKills(t)
	prevAssign := assignProcessToJob
	assignProcessToJob = func(*processJob, *os.Process) error {
		return errors.New("assign refused")
	}
	t.Cleanup(func() { assignProcessToJob = prevAssign })

	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	got := s.trackedSetupJob(&processJob{}, &os.Process{Pid: abandonedSetupPid}, "firefox")

	if got != nil {
		t.Fatal("a job whose assign failed was kept; an empty job reads as a closed browser")
	}
	if len(*asked) != 0 {
		t.Fatalf("the failed-assign drop asked to kill %v — that group is the user's live browser", *asked)
	}
}

// TestCloseLaunchJobAsksForTheKill covers runWithTimeout's teardown without
// launching anything. Two of its exits reach the deferred close with a browser
// still alive: the drain timing out with processes left, and the caller's
// context being cancelled. On Windows the close kills them; on Linux this call
// is the only thing that does.
//
// Mutation: swap the order so close() runs first and the kill is asked for a
// job that has already forgotten its group — a silent no-op.
func TestCloseLaunchJobAsksForTheKill(t *testing.T) {
	asked := captureTrackedKills(t)

	closeLaunchJob(nil, nopLogger{})
	if len(*asked) != 0 {
		t.Fatalf("a nil job asked to kill %v", *asked)
	}

	// Real, for the same reason as the two tests above: a bare &processJob{}
	// reads queryable() == false whether or not the reorder mutant is present,
	// which would make this assertion vacuous.
	job, err := newProcessJob()
	if err != nil {
		t.Fatalf("newProcessJob: %v", err)
	}
	t.Cleanup(job.close)
	closeLaunchJob(job, nopLogger{})
	if len(*asked) != 1 || (*asked)[0].job != job {
		t.Fatalf("closeLaunchJob asked to kill %v, want exactly the job", *asked)
	}
	if !(*asked)[0].queryable {
		t.Fatal("closeLaunchJob asked for the kill AFTER the job was already closed")
	}
}

// TestAskThenCloseStillClosesWhenTheKillErrors is Finding 4 from the Task 3
// review: a killTrackedProcesses failure is logged, never a reason to skip
// the close. On Linux the close is job_linux.go's forget() — the only way
// the failed group ever stops being tracked — so a site that returns early
// on a kill error would leave a group Moombox believes it can still act on,
// forever.
//
// One subtest per ask site, table-driven per the brief's suggestion. Each
// makes killTrackedProcesses fail, runs the site, then checks the SAME two
// things: the real job is no longer queryable() (the close still ran) and a
// Warn was logged (the error was not swallowed).
//
// The Warn check goes through containsAtWarn, not contains: a bare
// logger.contains() reads the merged, level-blind msgs slice, so a site that
// quietly downgraded its kill-error line to Debug would still satisfy it.
// containsAtWarn reads capturingLogger's warns slice instead, which only Warn
// populates.
//
// Mutant: make any site return/skip job.close() when the kill errors, and its
// case fails — the job stays queryable() and/or no Warn appears. Mutant:
// downgrade any one site's kill-error log from Warn to Debug, and that site's
// case fails — the line is still in msgs, but containsAtWarn only reads warns.
func TestAskThenCloseStillClosesWhenTheKillErrors(t *testing.T) {
	killErr := errors.New("kill refused")

	cases := []struct {
		name string
		run  func(t *testing.T) (job *processJob, warned bool)
	}{
		{
			name: "cleanupLocked",
			run: func(t *testing.T) (*processJob, bool) {
				prev := killTrackedProcesses
				killTrackedProcesses = func(*processJob) error { return killErr }
				t.Cleanup(func() { killTrackedProcesses = prev })

				logger := &capturingLogger{}
				s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), logger)
				job, err := newProcessJob()
				if err != nil {
					t.Fatalf("newProcessJob: %v", err)
				}
				s.mu.Lock()
				s.setupJob = job
				s.mu.Unlock()

				s.cleanup()
				return job, logger.containsAtWarn("could not kill the setup browser's process group")
			},
		},
		{
			name: "adoptSetupJobLocked",
			run: func(t *testing.T) (*processJob, bool) {
				prev := killTrackedProcesses
				killTrackedProcesses = func(*processJob) error { return killErr }
				t.Cleanup(func() { killTrackedProcesses = prev })

				logger := &capturingLogger{}
				s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), logger)
				stale, err := newProcessJob()
				if err != nil {
					t.Fatalf("newProcessJob: %v", err)
				}
				fresh := &processJob{}
				s.mu.Lock()
				s.setupJob = stale
				s.adoptSetupJobLocked(fresh)
				s.mu.Unlock()

				return stale, logger.containsAtWarn("could not kill the stale setup browser's process group")
			},
		},
		{
			name: "closeLaunchJob",
			run: func(t *testing.T) (*processJob, bool) {
				prev := killTrackedProcesses
				killTrackedProcesses = func(*processJob) error { return killErr }
				t.Cleanup(func() { killTrackedProcesses = prev })

				logger := &capturingLogger{}
				job, err := newProcessJob()
				if err != nil {
					t.Fatalf("newProcessJob: %v", err)
				}
				closeLaunchJob(job, logger)
				return job, logger.containsAtWarn("could not kill the refresh browser's process group")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, warned := tc.run(t)
			if job.queryable() {
				t.Fatal("the close was skipped after the kill errored; the job is still queryable")
			}
			if !warned {
				t.Fatal("no Warn was logged for the failed kill")
			}
		})
	}
}

// TestBrowserGoneFromAProcessGroup runs the reap's actual predicate over the
// actual Linux liveness type, on Windows, with a fake process table. This is
// what "unit-tested with a fake process table, no Linux box" means: the four
// answers below are the four the reap can receive, and three of them are new
// on Linux.
//
// Mutations, one per case: return (true, true) for an unadopted group and every
// launch that could not be tracked is reaped immediately; return (false, true)
// for the unreadable table and a hardened container reaps a setup whose browser
// is on screen; return `active >= 0` and a live browser reads as gone; return
// known=false for the empty group and the reap never fires at all, which is
// today's bug.
func TestBrowserGoneFromAProcessGroup(t *testing.T) {
	cases := []struct {
		name       string
		table      map[int]int
		unreadable bool
		job        *pgroupJob
		wantGone   bool
		wantKnown  bool
	}{
		{
			name:      "no group adopted — nothing can say",
			table:     map[int]int{setupGroupPid: setupGroupPid},
			job:       &pgroupJob{},
			wantGone:  false,
			wantKnown: false,
		},
		{
			name:       "process table unreadable — nothing can say",
			unreadable: true,
			job:        &pgroupJob{pgid: setupGroupPid},
			wantGone:   false,
			wantKnown:  false,
		},
		{
			name:      "the browser outlived its launcher",
			table:     map[int]int{setupChildPid: setupGroupPid, strangerPid: strangerGroup},
			job:       &pgroupJob{pgid: setupGroupPid},
			wantGone:  false,
			wantKnown: true,
		},
		{
			name:      "the group is empty — gone, and we can say so",
			table:     map[int]int{strangerPid: strangerGroup},
			job:       &pgroupJob{pgid: setupGroupPid},
			wantGone:  true,
			wantKnown: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unreadable {
				unreadableProcessTable(t)
			} else {
				fakeProcessTable(t, tc.table)
			}
			gone, known := browserGoneFrom(tc.job)
			if gone != tc.wantGone || known != tc.wantKnown {
				t.Fatalf("browserGoneFrom = (gone %v, known %v), want (%v, %v)",
					gone, known, tc.wantGone, tc.wantKnown)
			}
		})
	}
}

// TestBrowserGoneFromANilJobStillAnswers keeps the typed-nil path honest. Every
// platform's processJob nil-checks its receiver, which is why the question asked
// is queryable() and not `job != nil` — and why handing this function a nil
// *processJob through an interface parameter is safe rather than a panic.
//
// Mutation: change queryable() on any platform to dereference before checking
// and this panics, which is the failure the interface hides if nobody looks.
func TestBrowserGoneFromANilJobStillAnswers(t *testing.T) {
	gone, known := browserGoneFrom((*processJob)(nil))
	if gone || known {
		t.Fatalf("a nil job answered (gone %v, known %v), want (false, false)", gone, known)
	}
}
