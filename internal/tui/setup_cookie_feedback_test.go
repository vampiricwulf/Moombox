package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestSetupCookieFinishFeedback pins the wizard's four outcomes.
//
// Two rows are load-bearing, and they are the two the bool pair could not tell
// apart from their neighbours.
//
// "accepted but never checked": FinishSetup takes a sign-in the user just
// completed even when the site could not answer — a 429 or a DNS blip is not
// evidence against a login that happened thirty seconds ago — and the wizard
// reported it with the same "cookies configured" line as a confirmed one. That
// states a verification that never ran.
//
// "extracted cookies that cannot sign a request": the mirror image. Also
// inconclusive, also no finding about the credentials, but not accepted, and
// the wizard said "no login detected" — which is the wrong problem. A login WAS
// detected; it just did not yield anything that can sign a request, and the
// user sent to look for a missing sign-in will find one.
//
// The verified and no-login rows are the premise: without them, a branch that
// hedged about everything would satisfy the two above by saying nothing.
//
// Wording rule the rows enforce, inherited from the R F chord's split: only a
// conclusive verdict may say "failed", and anything that did not find out stops
// at "could not establish".
func TestSetupCookieFinishFeedback(t *testing.T) {
	cases := []struct {
		name       string
		msg        setupCookieFinishMsg
		wantSaid   []string
		wantUnsaid []string
		wantErrBox bool // rendered as the wizard's error line rather than feedback
	}{
		{
			name: "verified",
			msg: setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
				YouTube: cookies.RefreshOK, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
			}},
			wantSaid:   []string{"YouTube cookies configured"},
			wantUnsaid: []string{"could not establish", "no login"},
		},
		{
			// THE FIX.
			name: "accepted but never checked",
			msg: setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
				YouTube: cookies.RefreshUnknown, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
			}},
			wantSaid:   []string{"YouTube cookies saved", "could not establish"},
			wantUnsaid: []string{"configured", "failed", "no login"},
		},
		{
			// The other half of the fix, on the other side of acceptance.
			name: "extracted cookies that cannot sign a request",
			msg: setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
				YouTube: cookies.RefreshUnknown, Twitch: cookies.RefreshFailed,
			}},
			wantSaid:   []string{"could not establish", "sign"},
			wantUnsaid: []string{"no login detected", "configured"},
			wantErrBox: true,
		},
		{
			// The premise: an empty profile still gets the original line.
			name: "no login detected",
			msg: setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
				YouTube: cookies.RefreshFailed, Twitch: cookies.RefreshFailed,
			}},
			wantSaid:   []string{"No login detected"},
			wantUnsaid: []string{"could not establish"},
			wantErrBox: true,
		},
		{
			name: "both platforms, one of them unchecked",
			msg: setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
				YouTube: cookies.RefreshOK, Twitch: cookies.RefreshUnknown,
				YouTubeAccepted: true, TwitchAccepted: true,
			}},
			wantSaid:   []string{"YouTube cookies configured", "Twitch cookies saved", "could not establish"},
			wantUnsaid: []string{"failed"},
		},
		{
			name:       "extraction errored",
			msg:        setupCookieFinishMsg{Platform: "youtube", Err: "cookies.txt could not be read"},
			wantSaid:   []string{"cookies.txt could not be read"},
			wantErrBox: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := NewApp()
			if _, cmd := app.Update(tc.msg); cmd != nil {
				t.Errorf("the finish branch should not schedule work, got %T", cmd)
			}

			got := app.feedbackMsg
			if tc.wantErrBox {
				got = app.setupWiz.errorMsg
				if app.feedbackMsg != "" {
					t.Errorf("a rejected finish also set the transient feedback line: %q", app.feedbackMsg)
				}
			} else if app.setupWiz.errorMsg != "" {
				t.Errorf("an accepted finish set the wizard's error line: %q", app.setupWiz.errorMsg)
			}
			if got == "" {
				t.Fatal("the user pressed the button and got nothing back")
			}

			lower := strings.ToLower(got)
			for _, want := range tc.wantSaid {
				if !strings.Contains(lower, strings.ToLower(want)) {
					t.Errorf("message does not say %q: %q", want, got)
				}
			}
			for _, unwanted := range tc.wantUnsaid {
				if strings.Contains(lower, strings.ToLower(unwanted)) {
					t.Errorf("message asserts %q, which this outcome does not establish: %q", unwanted, got)
				}
			}
		})
	}
}

// TestSetupCookieUncheckedOutcomeIsNotRed is the severity half of the fix.
//
// feedbackColor classifies by substring, so the wording split is only worth
// something if the colour follows it: a sign-in that was accepted, and merely
// not confirmed, rendered as a red line is an alarm the user then has to chase
// over cookies that are working.
//
// Driven through the same Update() that produced the message, so a rewording
// cannot quietly leave the two out of step.
func TestSetupCookieUncheckedOutcomeIsNotRed(t *testing.T) {
	app := NewApp()
	app.Update(setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
		YouTube: cookies.RefreshUnknown, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
	}})

	if got := feedbackColor(app.feedbackMsg, app.feedbackSev); got != ColorYellow {
		t.Errorf("an accepted-but-unverified sign-in renders %v, want %v (yellow): %q",
			got, ColorYellow, app.feedbackMsg)
	}

	// The premise: a verified sign-in still reads as an unqualified success.
	app = NewApp()
	app.Update(setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
		YouTube: cookies.RefreshOK, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
	}})
	if got := feedbackColor(app.feedbackMsg, app.feedbackSev); got != ColorGreen {
		t.Errorf("a confirmed sign-in renders %v, want %v (green): %q", got, ColorGreen, app.feedbackMsg)
	}
}
