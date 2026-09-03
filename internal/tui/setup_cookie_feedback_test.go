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

			got := app.feedback.msg
			if tc.wantErrBox {
				got = app.setupWiz.errorMsg
				if app.feedback.msg != "" {
					t.Errorf("a rejected finish also set the transient feedback line: %q", app.feedback.msg)
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

	if got := feedbackColor(app.feedback.msg, app.feedback.sev); got != ColorYellow {
		t.Errorf("an accepted-but-unverified sign-in renders %v, want %v (yellow): %q",
			got, ColorYellow, app.feedback.msg)
	}

	// The premise: a verified sign-in still reads as an unqualified success.
	app = NewApp()
	app.Update(setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
		YouTube: cookies.RefreshOK, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
	}})
	if got := feedbackColor(app.feedback.msg, app.feedback.sev); got != ColorGreen {
		t.Errorf("a confirmed sign-in renders %v, want %v (green): %q", got, ColorGreen, app.feedback.msg)
	}
}

// TestSetupCookieAcceptedVerdictRendersInsideTheOverlay is the 12a arc-close
// finding F4.
//
// The accepted arm reported through the App's transient feedback line, which
// app_layout.go never draws while the setup wizard is visible — the overlay
// takes the whole view. So an operator who signed in and pressed Enter saw the
// green tick on the platform row and nothing else: not which platforms were
// accepted, and not the "saved, but could not establish" hedge that is the
// point of the four-arm split beside it.
//
// The verdict now renders INSIDE the wizard, where errorMsg already does. It
// ALSO still reaches the feedback line, deliberately: the alternative is
// holding it in new state until the overlay closes and draining it from a
// close hook — a mechanism built to contain a mechanism. Together they cost
// nothing: the wizard's line is read while the overlay stands, the feedback
// line if it is closed at once.
//
// goja-free: NewApp() plus one Update, like TestSetupCookieFinishFeedback.
//
// Mutations: drop the successMsg write from the accepted arm; render
// successMsg with ErrorStyle.
func TestSetupCookieAcceptedVerdictRendersInsideTheOverlay(t *testing.T) {
	app := NewApp()
	app.setupWiz.OpenCookieLogin("youtube")
	app.setupWiz.SetSize(100, 30)

	app.Update(setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
		YouTube: cookies.RefreshOK, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
	}})

	if !app.setupWiz.IsVisible() {
		t.Fatal("premise broken: the finish arm closed the overlay, so there is nothing to render into")
	}

	view := app.setupWiz.View()
	if !strings.Contains(view, "YouTube cookies configured") {
		t.Errorf("the accepted verdict is not reachable through the overlay the operator is looking at:\n%s", view)
	}
	if !strings.Contains(view, SuccessStyle.Render("YouTube cookies configured")) {
		t.Error("the accepted verdict is not rendered with SuccessStyle — a confirmed sign-in must not read as an error")
	}
	if !strings.Contains(app.feedback.msg, "YouTube cookies configured") {
		t.Errorf("the App feedback line no longer carries the verdict: %q", app.feedback.msg)
	}

	// The hedged verdict travels the same way — the arm the ✓ alone cannot express.
	hedged := NewApp()
	hedged.setupWiz.OpenCookieLogin("youtube")
	hedged.setupWiz.SetSize(100, 30)
	hedged.Update(setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
		YouTube: cookies.RefreshUnknown, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
	}})
	if v := hedged.setupWiz.View(); !strings.Contains(v, "could not establish") {
		t.Errorf("the accepted-but-unverified hedge does not reach the overlay:\n%s", v)
	}

	// A rejected finish still uses the error line and sets no success line.
	rejected := NewApp()
	rejected.setupWiz.OpenCookieLogin("youtube")
	rejected.setupWiz.SetSize(100, 30)
	rejected.Update(setupCookieFinishMsg{Platform: "youtube", Err: "cookies.txt could not be read"})
	if rejected.setupWiz.successMsg != "" {
		t.Errorf("a failed finish set the wizard's success line: %q", rejected.setupWiz.successMsg)
	}
}
