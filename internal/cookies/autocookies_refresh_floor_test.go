package cookies

import (
	"testing"
	"time"
)

// TestRefreshLooksImplausiblyFast pins the Debug-only sanity predicate.
//
// This is NOT the acted/not-acted verdict — browserLaunchActed (the
// screenshot) already owns that, and refreshLooksImplausiblyFast cannot
// override it. The table below exists to keep the threshold table-testable
// without launching a browser, per the file's existing habit of pulling
// decisions out of unstubbable I/O.
func TestRefreshLooksImplausiblyFast(t *testing.T) {
	cases := []struct {
		name    string
		elapsed time.Duration
		acted   bool
		want    bool
	}{
		// The observed no-op shape (160-211 ms) alongside acted=true: this is
		// the case the Debug note exists for.
		{"fast and acted", 200 * time.Millisecond, true, true},

		// The observed working-launch shape (3.08 s): comfortably clear of the
		// floor, no note.
		{"slow and acted", 3 * time.Second, true, false},

		// Already reported not-acted by browserLaunchActed — no second
		// complaint. Layering a heuristic warning on top of a fact already
		// surfaced would just be noise.
		{"fast but not acted", 200 * time.Millisecond, false, false},

		// Exactly at the floor is not below it.
		{"exactly at the floor", minPlausibleBrowserRefresh, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refreshLooksImplausiblyFast(tc.elapsed, tc.acted); got != tc.want {
				t.Errorf("refreshLooksImplausiblyFast(%v, %v) = %v, want %v", tc.elapsed, tc.acted, got, tc.want)
			}
		})
	}
}
