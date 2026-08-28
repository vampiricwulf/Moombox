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
// claimed otherwise: the guard at startChromiumSetup's assignment site, which
// closes a pre-existing s.setupJob before installing a new one. Deleting that
// guard leaves this test PASSING — verified — because the reap has already
// closed and nil'd the handle before the launcher gets there. The assertion
// would sit downstream of a junction two mechanisms satisfy.
//
// That guard has no test and cannot easily get one. It sits after cmd.Start()
// succeeds, so reaching it needs a process that actually launches, and nothing
// in this package may launch one. It is defence-in-depth against an invariant
// violation that is unreachable by construction — hence the Warn it logs — and
// it is documented as such at the site rather than pretended to be covered.
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
