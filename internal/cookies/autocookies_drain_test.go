package cookies

import (
	"context"
	"testing"
	"time"
)

// TestShouldKeepWaiting pins the drain loop's exit conditions.
//
// The predicate itself was never the bug — the defect lived in the syscall
// wiring and in WHERE the wait happened — but it is the one piece of the
// drain that can be checked without a Job Object, and getting either half of
// it backwards resurrects the failure it exists to prevent: drop the
// `active > 0` half and the browser is killed mid-load again; drop the
// budget half and a hung browser pins a refresh forever.
func TestShouldKeepWaiting(t *testing.T) {
	cases := []struct {
		name    string
		active  int
		elapsed time.Duration
		budget  time.Duration
		want    bool
	}{
		{"browser still running, budget left", 2, time.Second, 30 * time.Second, true},
		{"job empty", 0, time.Second, 30 * time.Second, false},
		{"budget blown with browser alive", 2, 31 * time.Second, 30 * time.Second, false},
		{"budget blown and job empty", 0, 31 * time.Second, 30 * time.Second, false},
		{"exactly at the budget", 2, 30 * time.Second, 30 * time.Second, false},
		{"one lap short of the budget", 1, 29950 * time.Millisecond, 30 * time.Second, true},
		// A negative count is not something QueryInformationJobObject can
		// produce (the field is a DWORD), but a future caller passing a
		// sentinel must not be read as "keep waiting forever".
		{"nonsense negative count", -1, time.Second, 30 * time.Second, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldKeepWaiting(tc.active, tc.elapsed, tc.budget); got != tc.want {
				t.Errorf("shouldKeepWaiting(%d, %s, %s) = %v, want %v",
					tc.active, tc.elapsed, tc.budget, got, tc.want)
			}
		})
	}
}

// TestDrainJobReturnsImmediatelyWithoutAJob covers the two ways there is
// nothing to drain: newProcessJob failed (runWithTimeout carries on with a
// nil job) and a job with no handle — which is also what every non-Windows
// build looks like, since their activeProcesses always reports zero. Both
// must return nil instantly rather than erroring or burning the budget.
func TestDrainJobReturnsImmediatelyWithoutAJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  *processJob
	}{
		{"nil job", nil},
		{"zero-value job", &processJob{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			if err := drainJob(context.Background(), tc.job, start, 30*time.Second, nopLogger{}); err != nil {
				t.Fatalf("drainJob = %v, want nil", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("drainJob took %s; it should not have waited at all", elapsed)
			}
		})
	}
}
