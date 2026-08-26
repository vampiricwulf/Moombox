package tui

import (
	"errors"
	"strings"
	"testing"
)

// TestCookieForceRefreshFeedback pins the R F chord's four outcomes.
//
// The load-bearing row is the third. A browser refresh that did nothing still
// leaves a working cookies.txt behind — the independent 30-minute session
// refresh keeps it alive — so "did the cookies verify" comes back true while
// "did this pass refresh them" comes back false. Reporting only the first
// toasted "Browser cookie refresh successful" for a refresh that had just
// logged that it could not confirm it had done anything, and did so beside a
// Last-refresh time that deliberately refuses to advance.
//
// On Linux there is no Job Object to drain, so a launch can never be
// confirmed to have acted: that row is not an edge case there, it is every
// press of the key.
//
// The wording must stop at "could not confirm". A browser that finished after
// we looked and one that never started are indistinguishable from here, so
// asserting failure would swap one wrong claim for its mirror image.
func TestCookieForceRefreshFeedback(t *testing.T) {
	cases := []struct {
		name       string
		msg        cookieForceRefreshResultMsg
		wantSaid   []string
		wantUnsaid []string
	}{
		{
			name:     "error",
			msg:      cookieForceRefreshResultMsg{Err: errors.New("browser exploded")},
			wantSaid: []string{"failed", "browser exploded"},
		},
		{
			name:       "nothing verified",
			msg:        cookieForceRefreshResultMsg{Success: false},
			wantSaid:   []string{"no cookies acquired"},
			wantUnsaid: []string{"successful"},
		},
		{
			// THE FIX.
			name:       "verified but the refresh could not be confirmed",
			msg:        cookieForceRefreshResultMsg{Success: true, Renewed: false},
			wantSaid:   []string{"still work", "could not confirm"},
			wantUnsaid: []string{"successful", "failed"},
		},
		{
			name:       "verified and renewed",
			msg:        cookieForceRefreshResultMsg{Success: true, Renewed: true},
			wantSaid:   []string{"successful"},
			wantUnsaid: []string{"could not confirm"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := NewApp()
			if _, cmd := app.Update(tc.msg); cmd != nil {
				t.Errorf("the feedback branch should not schedule work, got %T", cmd)
			}
			got := app.feedbackMsg
			if got == "" {
				t.Fatal("no feedback was set — the operator pressed a key and got nothing")
			}
			for _, want := range tc.wantSaid {
				if !strings.Contains(got, want) {
					t.Errorf("feedback does not say %q: %q", want, got)
				}
			}
			for _, unwanted := range tc.wantUnsaid {
				if strings.Contains(got, unwanted) {
					t.Errorf("feedback asserts %q, which this outcome does not establish: %q", unwanted, got)
				}
			}
		})
	}
}
