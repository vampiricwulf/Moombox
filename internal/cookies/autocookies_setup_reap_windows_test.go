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
// startChromiumSetup assigns `s.setupJob = job` unconditionally. Before the
// reap existed, the only thing stopping a second attempt from overwriting a
// live handle was that a second attempt could never start — the wedge was
// holding the leak shut. Now that StartSetup can proceed past an abandoned
// setup, the first attempt's handle has to be CLOSED rather than dropped:
// nothing else holds a reference, so a dropped handle means KILL_ON_JOB_CLOSE
// never fires and the orphaned browser survives until Moombox exits.
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
