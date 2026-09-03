package cookies

import (
	"errors"
	"os"
	"testing"
)

// captureTrackedKills swaps the "finish off what the job still tracks" hook for
// a recorder. On Windows the real hook is a no-op — close() there is
// KILL_ON_JOB_CLOSE — so these tests are about WHO ASKS, which is the half that
// has to be right before job_linux.go binds anything to it.
func captureTrackedKills(t *testing.T) *[]*processJob {
	t.Helper()
	prev := killTrackedProcesses
	asked := []*processJob{}
	killTrackedProcesses = func(j *processJob) error {
		asked = append(asked, j)
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
	job := &processJob{}
	s.mu.Lock()
	s.setupJob = job
	s.mu.Unlock()

	s.cleanup()

	if len(*asked) != 1 || (*asked)[0] != job {
		t.Fatalf("cleanup asked to kill %v, want exactly the job it was about to drop", *asked)
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
	stale := &processJob{}
	fresh := &processJob{}
	s.mu.Lock()
	s.setupJob = stale
	s.adoptSetupJobLocked(fresh)
	got := s.setupJob
	s.mu.Unlock()

	if len(*asked) != 1 || (*asked)[0] != stale {
		t.Fatalf("adopt asked to kill %v, want exactly the stale job", *asked)
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

	job := &processJob{}
	closeLaunchJob(job, nopLogger{})
	if len(*asked) != 1 || (*asked)[0] != job {
		t.Fatalf("closeLaunchJob asked to kill %v, want exactly the job", *asked)
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
