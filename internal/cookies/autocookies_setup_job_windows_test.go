//go:build windows

package cookies

import (
	"errors"
	"os"
	"testing"
)

// Two things this file pins, both Windows-only because both turn on a real Job
// Object handle:
//
//   - trackedSetupJob drops a job it could not assign to, rather than storing a
//     live handle that tracks nothing; and
//   - killSetupProcess does not shell a taskkill at a PID the OS has already
//     reaped and recycled, when the job is the thing that will do the killing.
//
// The fixtures here launch nothing and kill nothing on the host: captureKills
// intercepts the kill hook, the jobs created below never have a process
// assigned to them, and the one test that drives a real launcher reuses the
// stand-in browser from autocookies_setup_reap_windows_test.go.

// assignFails forces the job assign to fail for one test and restores the real
// one afterwards.
//
// The seam exists because the failure cannot be arranged any other way:
// AssignProcessToJobObject refuses for reasons a test cannot manufacture
// without a hostile process state, and nothing in this package may launch one.
func assignFails(t *testing.T) {
	t.Helper()
	prev := assignProcessToJob
	assignProcessToJob = func(*processJob, *os.Process) error {
		return errors.New("AssignProcessToJobObject: access is denied")
	}
	t.Cleanup(func() { assignProcessToJob = prev })
}

// TestTrackedSetupJobDropsAJobItCouldNotAssignTo covers the decision both
// launchers share, at the only place it can be reached without a process.
//
// The failure mode is subtle enough to restate. A job whose assign failed still
// has a LIVE handle, so queryable() is true and activeProcesses() answers 0 —
// and setupBrowserGone reads that pair as a positive "the browser is gone". The
// reap then releases a setup whose browser is still on screen. It cannot kill
// that browser, because nothing is in the job for KILL_ON_JOB_CLOSE to take;
// the cost is a premature release, and an ErrNoSetupInProgress on the user's
// next "I'm logged in".
//
// BOTH ROWS MATTER. Without the second, the guard could `return nil` always and
// still pass — and that mutation switches the reap off on every path, silently,
// which is a worse bug than the one being fixed.
//
// Both rows go through the seam. The REAL AssignProcessToJobObject is exercised
// end-to-end by TestFirefoxSetupIsTrackedByAJobObject instead, because the only
// process this test could offer the real call is itself, and closing a
// KILL_ON_JOB_CLOSE job that the test binary had been assigned to would
// terminate the test run.
func TestTrackedSetupJobDropsAJobItCouldNotAssignTo(t *testing.T) {
	// Never touched: the seam is stubbed in both rows, so nothing opens a
	// handle to this PID. Odd on purpose — a live Windows PID is a multiple of
	// four — so even a future edit that dropped the stub could not reach a real
	// process on the machine running the tests.
	unusedProc := &os.Process{Pid: abandonedSetupPid}

	t.Run("a failed assign is dropped, not stored", func(t *testing.T) {
		s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
		assignFails(t)

		job, err := newProcessJob()
		if err != nil {
			t.Fatalf("create a Job Object: %v", err)
		}
		t.Cleanup(job.close)

		got := s.trackedSetupJob(job, unusedProc, "firefox")

		if got != nil {
			t.Fatal("a job whose assign failed was kept — it tracks nothing, reports an " +
				"empty job from a live handle, and the reap reads that as a browser that closed")
		}
		if job.handle != 0 {
			t.Fatal("the dropped job's handle was leaked rather than closed")
		}
		if _, known := setupBrowserGone(got); known {
			t.Fatal("the probe claims to know something about a setup nothing is tracking")
		}
	})

	t.Run("a successful assign is kept", func(t *testing.T) {
		s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
		prev := assignProcessToJob
		assignProcessToJob = func(*processJob, *os.Process) error { return nil }
		t.Cleanup(func() { assignProcessToJob = prev })

		job, err := newProcessJob()
		if err != nil {
			t.Fatalf("create a Job Object: %v", err)
		}
		t.Cleanup(job.close)

		got := s.trackedSetupJob(job, unusedProc, "chromium")

		if got != job {
			t.Fatal("a job with a working assign was dropped — no setup would ever be " +
				"trackable, and the reap would be dead on every path again")
		}
		if !got.queryable() {
			t.Fatal("the kept job cannot be queried, which reads to the probe as no job at all")
		}
	})
}

// TestAFirefoxSetupWithAFailedAssignStoresNoJob is the routing half: it shows
// startFirefoxSetup goes THROUGH trackedSetupJob, rather than only that
// trackedSetupJob is correct in isolation.
//
// It also pins the consequence, which is the part worth having. A dropped job
// means the probe answers "no idea", so the reap declines even with the grace
// window long gone. That is the pre-existing wedge, and it is the safe
// direction: the alternative is the reap firing on a setup whose browser is
// still on screen.
//
// The equivalent line in startChromiumSetup is NOT covered by any test; the
// reason is recorded at that site rather than left to be discovered.
//
// Deliberately the SILENT stand-in and not the handoff one. With the assign
// failing there is no job, so a handed-off child would escape into the wild —
// it would hold the test binary open for fakeSetupChildLifetime and break the
// next `go test` run's link step. Nothing here needs it: the job is nil either
// way, and nil is the whole subject.
func TestAFirefoxSetupWithAFailedAssignStoresNoJob(t *testing.T) {
	captureKills(t)
	assignFails(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	startFakeFirefoxSetup(t, s, fakeBrowserSilent)

	s.mu.Lock()
	job, proc := s.setupJob, s.setupProcess
	s.mu.Unlock()

	if job != nil {
		t.Fatal("a setup whose assign failed kept its Job Object — a live handle tracking " +
			"nothing, which the probe reads as a browser that closed")
	}
	if proc == nil {
		t.Fatal("fixture: the setup never registered, so nothing here is about the assign")
	}

	expireGrace(s)
	if !s.GetStatus().SetupInProgress {
		t.Fatal("the reap released a setup on the word of a job that never tracked anything — " +
			"where nothing can say whether the browser is gone, the slot must be left alone")
	}
}

// TestKillSetupProcessSkipsAReapedPID is F11. With the Job Object doing the
// killing, shelling `taskkill /F /T /PID` at the launcher's PID can no longer
// accomplish anything, and can only ever land on whatever Windows recycled that
// PID onto.
//
// That state is the NORMAL one on the Firefox family, where the launcher hands
// off and exits in ~170ms, so before the setup job existed this kill fired on
// every cancel and every shutdown of a Firefox setup. It is the same rule
// runWithTimeout applies with onLauncherReaped.
//
// THE LAST THREE ROWS ARE WHY THIS IS A TABLE. Skipping is only correct where
// something else will do the killing, so the guard has to be narrow: a live
// launcher must still be killed, and so must a reaped one whose job cannot
// vouch for anything — which includes every non-Windows target, where
// queryable() is always false and this guard therefore never engages.
//
// Mutation-checked: widening the guard to `s.browserExited` alone fails row 2;
// dropping the guard entirely fails row 1.
func TestKillSetupProcessSkipsAReapedPID(t *testing.T) {
	for _, tc := range []struct {
		name string
		// exited is the wait goroutine's record: has the process MOOMBOX
		// SPAWNED gone? On Firefox that is true within ~170ms of every launch.
		exited bool
		// job says whether a live, queryable Job Object sits in the slot.
		job      bool
		wantKill bool
	}{
		{"a reaped launcher whose job holds the browser", true, true, false},
		{"a reaped launcher with no job to finish the job", true, false, true},
		{"a live process with a job", false, true, true},
		{"a live process with no job", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			killed := captureKills(t)
			s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})

			s.mu.Lock()
			s.setupProcess = &os.Process{Pid: abandonedSetupPid}
			s.browserExited = tc.exited
			if tc.job {
				job, err := newProcessJob()
				if err != nil {
					s.mu.Unlock()
					t.Fatalf("create a Job Object: %v", err)
				}
				// Nothing is ever assigned to it, so closing it kills nothing.
				t.Cleanup(job.close)
				s.setupJob = job
			}
			s.mu.Unlock()

			s.killSetupProcess()

			switch {
			case tc.wantKill && len(*killed) == 0:
				t.Fatal("nothing was killed, and nothing else is going to — the setup browser " +
					"survives the cancel that was supposed to end it")
			case !tc.wantKill && len(*killed) != 0:
				t.Fatalf("taskkill was issued for PID %v, which was reaped ~170ms into the "+
					"setup and now names whatever Windows handed it to next; the job holding "+
					"the real browser is closed by cleanup a moment later", *killed)
			}
		})
	}
}
