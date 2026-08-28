package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestCookieForceRefreshFeedback pins the R F chord's six outcomes.
//
// Two rows are load-bearing, for opposite reasons.
//
// "verified but the refresh could not be confirmed": a browser refresh that
// did nothing still leaves a working cookies.txt behind — the independent
// 30-minute session refresh keeps it alive — so "did the cookies verify" comes
// back true while "did this pass refresh them" comes back false. Reporting
// only the first toasted "Browser cookie refresh successful" for a refresh
// that had just logged that it could not confirm it had done anything, and did
// so beside a Last-refresh time that deliberately refuses to advance. On Linux
// there is no Job Object to drain, so a launch can never be confirmed to have
// acted: that row is not an edge case there, it is every press of the key.
//
// "declined": the reverse mistake, and the one that was live. The single
// refresh slot is held by the 30-minute periodic tick and by interactive
// setup, so pressing R F while either is in flight returns refreshDeclined() —
// a pass that never looked at anything. That reported "no cookies acquired",
// a conclusive-sounding verdict, for cookies the very same screen was
// simultaneously reporting as authenticated.
//
// The "conclusively unauthenticated" row is the PREMISE for the two above: it
// proves this branch still says so when the pass actually established it, so a
// silent row cannot pass by saying nothing at all.
//
// Wording rules the rows enforce: a pass that did not find out must stop at
// "could not establish" / "declined to run", because a browser that finished
// after we looked and one that never started are indistinguishable from here,
// and asserting failure would swap one wrong claim for its mirror image.
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
			// THE FIX. refreshDeclined() is the zero RefreshResult.
			name:       "declined — the slot was already held",
			msg:        cookieForceRefreshResultMsg{Result: cookies.RefreshResult{}},
			wantSaid:   []string{"declined to run", "nothing was learned"},
			wantUnsaid: []string{"failed", "not authenticated", "successful"},
		},
		{
			// refreshAborted()'s shape, and every pass whose verification
			// could not reach the service.
			name:       "ran but learned nothing",
			msg:        cookieForceRefreshResultMsg{Result: cookies.RefreshResult{Ran: true}},
			wantSaid:   []string{"could not establish"},
			wantUnsaid: []string{"failed", "declined", "successful"},
		},
		{
			// The premise: a conclusive negative is still reported as one.
			name: "conclusively unauthenticated",
			msg: cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
				Ran: true, YouTube: cookies.RefreshFailed, YouTubeStored: true,
			}},
			wantSaid:   []string{"auth verification failed"},
			wantUnsaid: []string{"declined", "could not establish", "successful"},
		},
		{
			name: "verified but the refresh could not be confirmed",
			msg: cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
				Ran: true, YouTube: cookies.RefreshOK, Renewed: false,
			}},
			wantSaid:   []string{"still work", "could not confirm"},
			wantUnsaid: []string{"successful", "failed"},
		},
		{
			name: "verified and renewed",
			msg: cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
				Ran: true, YouTube: cookies.RefreshOK, Renewed: true,
			}},
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
			lower := strings.ToLower(got)
			for _, want := range tc.wantSaid {
				if !strings.Contains(lower, strings.ToLower(want)) {
					t.Errorf("feedback does not say %q: %q", want, got)
				}
			}
			for _, unwanted := range tc.wantUnsaid {
				if strings.Contains(lower, strings.ToLower(unwanted)) {
					t.Errorf("feedback asserts %q, which this outcome does not establish: %q", unwanted, got)
				}
			}
		})
	}
}

// TestCookieForceRefreshFeedbackColour pins the severity each outcome renders
// at, because the wording split is only half the fix: a decline that reads as
// a red failure line is still an alarm the operator has to chase.
//
// feedbackColor classifies by substring, so it is exactly the kind of
// downstream junction that drifts when a message is reworded. Driving it from
// the same Update() that produced the message keeps the two in step.
func TestCookieForceRefreshFeedbackColour(t *testing.T) {
	cases := []struct {
		name string
		msg  cookieForceRefreshResultMsg
		want string // ColorRed / ColorYellow / ColorGreen, compared by value below
	}{
		{"declined", cookieForceRefreshResultMsg{Result: cookies.RefreshResult{}}, "yellow"},
		{"ran but learned nothing", cookieForceRefreshResultMsg{Result: cookies.RefreshResult{Ran: true}}, "yellow"},
		{"conclusively unauthenticated", cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, YouTube: cookies.RefreshFailed, YouTubeStored: true,
		}}, "red"},
	}
	want := map[string]any{"yellow": ColorYellow, "red": ColorRed}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := NewApp()
			app.Update(tc.msg)
			if got := feedbackColor(app.feedbackMsg); got != want[tc.want] {
				t.Errorf("feedbackColor(%q) = %v, want %s", app.feedbackMsg, got, tc.want)
			}
		})
	}
}
