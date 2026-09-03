package tui

import "testing"

// The wizard has always owned the whole interactive cookie login — start,
// countdown, finish, cancel, the three-state verdict — and has always been
// reachable from one place: app.go's `if a.IsFirstRun`. These tests pin the
// second entrance, and what they are NOT allowed to pin is a second
// implementation: every assertion runs through the same handleSimpleCookieKey
// the first-run flow runs through.

// TestOpenCookieLoginLandsOnTheCookieStep pins where the overlay opens and what
// it preselects.
//
// MUTANT: have OpenCookieLogin call Open() instead. Open() is the first-run
// entrance — it drops to setupModeSelect, so a chord that promised a login
// serves the "Quick or Advanced" screen, and it clears the config values and
// channel list on the way.
func TestOpenCookieLoginLandsOnTheCookieStep(t *testing.T) {
	for _, tc := range []struct {
		platform  string
		wantFocus int
		wantStart string
	}{
		{"youtube", 0, "youtube"},
		{"twitch", 1, "twitch"},
		{"", 0, "youtube"},         // no platform flagged — YouTube is the default row
		{"nonsense", 0, "youtube"}, // an unknown value costs a keystroke, never a wrong browser
	} {
		t.Run("platform="+tc.platform, func(t *testing.T) {
			m := NewSetupWizardModel()
			started := ""
			m.OnStartAutoCookie = func(p string) error { started = p; return nil }

			m.OpenCookieLogin(tc.platform)

			if !m.IsVisible() {
				t.Fatal("OpenCookieLogin left the wizard hidden")
			}
			if !m.cookieOnly {
				t.Error("the overlay is not marked cookie-only, so Esc and the third row will " +
					"walk the operator into the first-run channel editor")
			}
			if m.mode != setupModeSimple {
				t.Errorf("mode = %v, want setupModeSimple — a chord that promised a login opened "+
					"the mode-selection screen", m.mode)
			}
			if m.simpleStage != setupSimpleCookies {
				t.Errorf("stage = %v, want setupSimpleCookies", m.simpleStage)
			}
			if m.cookieFocus != tc.wantFocus {
				t.Errorf("cookieFocus = %d, want %d", m.cookieFocus, tc.wantFocus)
			}

			// The step is not a picture of one: pressing Enter must reach the
			// SAME callback the first-run step reaches, and arm the SAME
			// countdown constant.
			m.HandleKey(keyEnter)
			if started != tc.wantStart {
				t.Errorf("Enter started %q, want %q", started, tc.wantStart)
			}
			if m.cookieCountdown != cookieSetupCountdownSeconds {
				t.Errorf("armed %d seconds, want cookieSetupCountdownSeconds (%d) — the overlay is "+
					"running its own timer instead of the wizard's", m.cookieCountdown,
					cookieSetupCountdownSeconds)
			}
		})
	}
}

// TestCookieLoginOverlayCancelsWhateverItLeavesBehind pins the abandon rule.
// AutoCookieService holds the setup slot until someone cancels, finishes, or
// the server-side reap notices the browser is gone, so walking out with a
// browser open must release it — otherwise the next R L, and the next periodic
// refresh, meet ErrSetupInProgress for the whole grace window.
//
// MUTANT: make closeCookieLogin call m.Close() directly. The third subtest then
// leaves a live setup behind with no cancel.
func TestCookieLoginOverlayCancelsWhateverItLeavesBehind(t *testing.T) {
	t.Run("esc while the browser is open cancels and stays", func(t *testing.T) {
		m := NewSetupWizardModel()
		cancels := 0
		m.OnStartAutoCookie = func(string) error { return nil }
		m.OnCancelAutoCookie = func() { cancels++ }
		m.OpenCookieLogin("youtube")
		m.HandleKey(keyEnter) // browser open, countdown armed

		m.HandleKey(keyEsc)

		if cancels != 1 {
			t.Fatalf("Esc over a live setup cancelled %d times, want 1", cancels)
		}
		if m.cookieActive {
			t.Error("the overlay still advertises a live cookie flow after Esc")
		}
		if !m.IsVisible() {
			t.Error("Esc over a live setup closed the overlay; it should cancel the browser and " +
				"leave the picker up so the operator can try the other platform")
		}
	})

	t.Run("esc at the picker closes and cancels nothing", func(t *testing.T) {
		m := NewSetupWizardModel()
		m.OnCancelAutoCookie = func() { t.Error("cancelled a setup that was never started") }
		m.OpenCookieLogin("youtube")

		m.HandleKey(keyEsc)

		if m.IsVisible() {
			t.Error("Esc at the picker left the cookie-login overlay open")
		}
		if m.cookieOnly {
			t.Error("cookieOnly survived the close, so the next first-run wizard would inherit it")
		}
	})

	t.Run("the close funnel cancels a live setup", func(t *testing.T) {
		m := NewSetupWizardModel()
		cancels := 0
		m.OnStartAutoCookie = func(string) error { return nil }
		m.OnCancelAutoCookie = func() { cancels++ }
		m.OpenCookieLogin("twitch")
		m.HandleKey(keyEnter)

		m.closeCookieLogin()

		if cancels != 1 {
			t.Fatalf("closeCookieLogin cancelled %d times, want 1 — a browser was left holding "+
				"the acquisition slot", cancels)
		}
		if m.IsVisible() || m.cookieActive {
			t.Error("closeCookieLogin left the overlay or the flow alive")
		}
	})
}

// TestCookieLoginOverlayDoesNotWalkIntoTheFirstRunFlow pins the third list row.
// In the first-run wizard that row is "Skip / Next" and leads to the channel
// editor, whose Tab finishes setup and REWRITES config.toml — not something a
// configured install should reach from a cookie chord.
//
// MUTANT 1: drop the cookieOnly branch from case 2. The first subtest then
// lands on setupSimpleChannels.
// MUTANT 2: drop the `if m.cookieOnly` guard from the keyEsc case so Esc
// always closes. The cookie-only Esc subtest in
// TestCookieLoginOverlayCancelsWhateverItLeavesBehind still passes; the third
// subtest here is what catches it.
func TestCookieLoginOverlayDoesNotWalkIntoTheFirstRunFlow(t *testing.T) {
	t.Run("cookie-only: the third row closes", func(t *testing.T) {
		m := NewSetupWizardModel()
		m.OpenCookieLogin("youtube")
		m.cookieFocus = 2

		m.HandleKey(keyEnter)

		if m.IsVisible() {
			t.Error("the third row did not close the cookie-login overlay")
		}
		if m.simpleStage == setupSimpleChannels {
			t.Error("the cookie-login overlay walked into the first-run channel editor")
		}
	})

	// THE PREMISE. Without it the subtest above is satisfied by a build where
	// the third row closes the wizard for everyone, which would break first run.
	t.Run("first run: the third row still advances to channels", func(t *testing.T) {
		m := NewSetupWizardModel()
		m.Open()
		m.mode = setupModeSimple
		m.simpleStage = setupSimpleCookies
		m.cookieFocus = 2

		m.HandleKey(keyEnter)

		if !m.IsVisible() {
			t.Fatal("premise lost: Skip / Next closed the first-run wizard")
		}
		if m.simpleStage != setupSimpleChannels {
			t.Fatalf("premise lost: Skip / Next left the first-run wizard at stage %v, so the "+
				"cookie-only subtest above proves nothing about the branch", m.simpleStage)
		}
	})

	// The Esc branch has the same shape and needs its own premise: a build
	// where Esc closes the wizard for EVERYONE passes the cookie-only Esc
	// subtest and strands a first-run operator who pressed Esc to go back a
	// screen — the wizard is gone and the process is still unconfigured.
	t.Run("first run: Esc at the picker still returns to mode selection", func(t *testing.T) {
		m := NewSetupWizardModel()
		m.Open()
		m.mode = setupModeSimple
		m.simpleStage = setupSimpleCookies

		m.HandleKey(keyEsc)

		if !m.IsVisible() {
			t.Fatal("premise lost: Esc closed the first-run wizard at its cookie step")
		}
		if m.mode != setupModeSelect {
			t.Fatalf("premise lost: Esc left the first-run wizard in mode %v, want setupModeSelect", m.mode)
		}
	})
}

// TestCookieLoginRefusesToReopenOverAVisibleWizard: a chord must not throw away
// a first-run setup in progress, nor stack a second cookie overlay on top of a
// countdown that has a real browser behind it.
//
// MUTANT: drop the `if m.visible { return }` guard. The wizard then jumps from
// the mode-selection screen straight to a cookie-only picker mid-setup.
func TestCookieLoginRefusesToReopenOverAVisibleWizard(t *testing.T) {
	m := NewSetupWizardModel()
	m.Open() // first run, sitting on the mode-selection screen

	m.OpenCookieLogin("twitch")

	if m.cookieOnly {
		t.Error("OpenCookieLogin converted a first-run wizard into a cookie-only overlay")
	}
	if m.mode != setupModeSelect {
		t.Errorf("mode = %v, want setupModeSelect — the first-run wizard was moved out from under "+
			"the operator", m.mode)
	}
}

// TestOpenClearsTheCookieOnlyFlag: Open() is the first-run entrance and must
// leave no trace of a previous cookie-login overlay.
//
// MUTANT: omit `m.cookieOnly = false` from Open(). The first-run wizard's cookie
// step then closes on Esc and never reaches the channel editor — first run
// breaks, and only after someone used the chord first.
func TestOpenClearsTheCookieOnlyFlag(t *testing.T) {
	m := NewSetupWizardModel()
	m.OpenCookieLogin("youtube")
	m.closeCookieLogin()

	m.Open()

	if m.cookieOnly {
		t.Error("Open() inherited cookieOnly from an earlier cookie-login overlay")
	}
}
