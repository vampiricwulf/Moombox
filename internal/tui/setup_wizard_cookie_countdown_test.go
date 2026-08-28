package tui

import (
	"testing"
)

// The cookie-setup countdown used to be the bare literal 60 at six separate
// sites in setup_wizard.go. Two things were wrong with that and this file pins
// both.
//
// First, the number. Sixty seconds cannot cover an email, a password and a 2FA
// code, and since cookies.cleanupLocked started closing the setup Job Object on
// the Firefox family, an expiry no longer merely abandons the setup — it closes
// the window the user is typing into. The countdown's JOB changed underneath
// it: it used to be the only backstop against an abandoned setup wedging every
// form of cookie acquisition, and the server-side reap is that backstop now.
// What is left for it to do is bound a human who walked away, which does not
// need to be tight.
//
// Second, the six copies. A value that appears six times is a value that gets
// changed in five places.

// TestEveryCookieSetupEntryPointArmsTheSameCountdown walks all six sites that
// start the countdown and requires each to use the shared constant.
//
// This is the test that would have caught the original defect, and it is the
// one that catches its return: a seventh entry point, or a sixth that quietly
// keeps a literal, fails here rather than in the field.
//
// Note what it does NOT assert — that the constant equals any particular
// number. Pinning 300 against 300 would only restate the declaration. The
// duration itself is asserted once, separately, as the property that actually
// matters.
func TestEveryCookieSetupEntryPointArmsTheSameCountdown(t *testing.T) {
	// arm puts the wizard in the state one entry point starts from, presses the
	// key that reaches it, and hands back the countdown that was armed.
	for _, tc := range []struct {
		name  string
		setup func(m *SetupWizardModel)
		press func(m *SetupWizardModel, key string) string
		key   string
	}{
		{
			"simple wizard, YouTube",
			func(m *SetupWizardModel) { m.cookieFocus = 0 },
			(*SetupWizardModel).handleSimpleCookieKey, keyEnter,
		},
		{
			"simple wizard, Twitch",
			func(m *SetupWizardModel) { m.cookieFocus = 1 },
			(*SetupWizardModel).handleSimpleCookieKey, keyEnter,
		},
		{
			"simple wizard, retry after a timeout",
			func(m *SetupWizardModel) { m.cookieTimedOut = true; m.cookiePlatform = "youtube" },
			(*SetupWizardModel).handleSimpleCookieKey, "r",
		},
		{
			"advanced wizard, YouTube",
			func(m *SetupWizardModel) { m.cookieFocus = 0 },
			(*SetupWizardModel).handleAdvancedCookieKey, keyEnter,
		},
		{
			"advanced wizard, Twitch",
			func(m *SetupWizardModel) { m.cookieFocus = 1 },
			(*SetupWizardModel).handleAdvancedCookieKey, keyEnter,
		},
		{
			"advanced wizard, retry after a timeout",
			func(m *SetupWizardModel) { m.cookieTimedOut = true; m.cookiePlatform = "twitch" },
			(*SetupWizardModel).handleAdvancedCookieKey, "r",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSetupWizardModel()
			started := false
			// No service, no browser: the wizard only needs the callback to
			// report success for it to arm the countdown.
			m.OnStartAutoCookie = func(string) error { started = true; return nil }
			tc.setup(m)

			tc.press(m, tc.key)

			if !started {
				t.Fatal("fixture: this row never reached OnStartAutoCookie, so it is not " +
					"exercising the entry point it is named for")
			}
			if m.cookieCountdown != cookieSetupCountdownSeconds {
				t.Fatalf("armed %d seconds, want cookieSetupCountdownSeconds (%d) — this site "+
					"is not using the shared constant, so the six will drift apart again",
					m.cookieCountdown, cookieSetupCountdownSeconds)
			}
		})
	}
}

// TestTheCookieCountdownOutlastsARealLogin is the claim behind the number,
// stated as a lower bound rather than an equality so a future increase does not
// have to edit a test to stay honest.
//
// Five minutes is the floor because the countdown's expiry is DESTRUCTIVE on
// Windows: OnCancelAutoCookie reaches cookies.CancelSetup, whose cleanup closes
// the Job Object, and KILL_ON_JOB_CLOSE takes the browser window with it. An
// email, a password, a 2FA prompt and a slow phone do not fit in one minute,
// and nothing bad happens if the countdown is generous — a deliberate cancel is
// one Esc away, and the server-side reap collects a setup this never catches.
func TestTheCookieCountdownOutlastsARealLogin(t *testing.T) {
	const enoughForAPasswordAndA2FACode = 300 // seconds
	if cookieSetupCountdownSeconds < enoughForAPasswordAndA2FACode {
		t.Fatalf("the wizard gives a user %ds to sign in, and on Windows it destroys their "+
			"browser window when that runs out; want at least %ds",
			cookieSetupCountdownSeconds, enoughForAPasswordAndA2FACode)
	}
}

// TestTheCookieCountdownExpiryCancelsTheSetup pins what expiry DOES, since the
// test above turns on it being destructive.
//
// If this ever stops holding — if expiry became non-destructive — the lower
// bound above would be arguing for a duration on a premise that no longer
// applies, and the two tests should be revisited together.
func TestTheCookieCountdownExpiryCancelsTheSetup(t *testing.T) {
	m := NewSetupWizardModel()
	cancelled := false
	m.OnCancelAutoCookie = func() { cancelled = true }
	m.visible = true
	m.cookieActive = true
	m.cookieCountdown = 1
	m.cookieTickGen = 7

	m.UpdateComponents(cookieCountdownTickMsg{gen: m.cookieTickGen})

	if !cancelled {
		t.Fatal("the countdown ran out without cancelling the setup — then its duration " +
			"would not be the thing protecting a user's login window")
	}
	if !m.cookieTimedOut || m.cookieActive {
		t.Fatal("expiry left the wizard advertising a live cookie flow")
	}
}
