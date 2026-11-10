package tui

import (
	"errors"
	"testing"
)

// cookieStepWizard puts a wizard on its cookie step in the given flow, with
// recording start and cancel callbacks.
func cookieStepWizard(t *testing.T, flow string) (m *SetupWizardModel, started *[]string, cancels *int) {
	t.Helper()
	m = NewSetupWizardModel()
	m.Open()
	switch flow {
	case "simple":
		m.mode = setupModeSimple
		m.simpleStage = setupSimpleCookies
	case "advanced":
		m.mode = setupModeAdvanced
		m.advancedFormDone = true
		m.advancedCookieDone = false
	case "cookie-only":
		m = NewSetupWizardModel()
		m.OpenCookieLogin("youtube")
	}
	started, cancels = new([]string), new(int)
	m.OnStartAutoCookie = func(p string) error { *started = append(*started, p); return nil }
	m.OnCancelAutoCookie = func() { *cancels++ }
	return m, started, cancels
}

// The cookie step is one handler (handleCookieStepKey) behind the quick
// flow, the advanced flow and the E L overlay alike (K25): everything but
// its two ways out behaves the same in all three.
func TestCookieStepSharedBehaviour(t *testing.T) {
	for _, flow := range []string{"simple", "advanced", "cookie-only"} {
		t.Run(flow, func(t *testing.T) {
			// Start YouTube, finish.
			m, started, _ := cookieStepWizard(t, flow)
			m.cookieFocus = 0
			m.HandleKey(keyEnter)
			if len(*started) != 1 || (*started)[0] != "youtube" || !m.cookieActive ||
				m.cookiePlatform != "youtube" || m.cookieCountdown != cookieSetupCountdownSeconds || m.cookieTimedOut {
				t.Fatalf("start youtube: started=%v active=%v platform=%q countdown=%d", *started, m.cookieActive, m.cookiePlatform, m.cookieCountdown)
			}
			if got := m.HandleKey(keyEnter); got != "finish_cookie" || !m.cookieFinishing {
				t.Errorf("Enter with the browser open = %q finishing=%v", got, m.cookieFinishing)
			}
			if got := m.HandleKey(keyEsc); got != "" || !m.cookieActive {
				t.Errorf("keys during the extraction must be ignored: %q active=%v", got, m.cookieActive)
			}

			// Start Twitch, Esc cancels.
			m, started, cancels := cookieStepWizard(t, flow)
			m.cookieFocus = 1
			m.HandleKey(keyTab)
			if (*started)[0] != "twitch" || m.cookiePlatform != "twitch" {
				t.Fatalf("start twitch: %v %q", *started, m.cookiePlatform)
			}
			m.HandleKey(keyEsc)
			if *cancels != 1 || m.cookieActive || m.cookiePlatform != "" || !m.IsVisible() {
				t.Errorf("Esc with the browser open: cancels=%d active=%v platform=%q visible=%v", *cancels, m.cookieActive, m.cookiePlatform, m.IsVisible())
			}

			// A refused start names the platform.
			m, _, _ = cookieStepWizard(t, flow)
			m.OnStartAutoCookie = func(string) error { return errors.New("boom") }
			m.cookieFocus = 1
			m.HandleKey(keyEnter)
			if m.errorMsg != "Twitch cookies: boom" || m.cookieActive {
				t.Errorf("refused start: error %q active %v", m.errorMsg, m.cookieActive)
			}

			// Timed out: R retries the same platform, S skips.
			m, started, _ = cookieStepWizard(t, flow)
			m.cookieTimedOut, m.cookiePlatform = true, "twitch"
			m.HandleKey("r")
			if len(*started) != 1 || (*started)[0] != "twitch" || !m.cookieActive || m.cookieTimedOut {
				t.Errorf("retry: started=%v active=%v timedOut=%v", *started, m.cookieActive, m.cookieTimedOut)
			}
			m, _, _ = cookieStepWizard(t, flow)
			m.cookieTimedOut, m.cookiePlatform = true, "twitch"
			m.HandleKey("s")
			if m.cookieTimedOut || m.cookiePlatform != "" || !m.IsVisible() {
				t.Errorf("skip after a timeout: timedOut=%v platform=%q", m.cookieTimedOut, m.cookiePlatform)
			}

			// Up/Down stay within the three rows.
			m, _, _ = cookieStepWizard(t, flow)
			m.cookieFocus = 0
			m.HandleKey(keyUp)
			for range 5 {
				m.HandleKey(keyDown)
			}
			if m.cookieFocus != 2 {
				t.Errorf("focus = %d, want clamped to 2", m.cookieFocus)
			}
		})
	}
}

// The two ways out are each flow's own.
func TestCookieStepExitsPerFlow(t *testing.T) {
	// Quick flow: Esc → the mode choice; row 2 → the channels, from the top.
	m, _, _ := cookieStepWizard(t, "simple")
	m.HandleKey(keyEsc)
	if m.mode != setupModeSelect || !m.IsVisible() {
		t.Errorf("simple Esc: mode %v visible %v", m.mode, m.IsVisible())
	}
	m, _, _ = cookieStepWizard(t, "simple")
	m.channelIndex = 3
	m.cookieFocus = 2
	m.HandleKey(keyEnter)
	if m.simpleStage != setupSimpleChannels || m.channelIndex != 0 || m.channelMode != "list" {
		t.Errorf("simple row 2: stage %v index %d mode %q", m.simpleStage, m.channelIndex, m.channelMode)
	}

	// Advanced flow: Esc → the form rebuilt, on its last group; row 2 → the
	// channels, the list kept.
	m, _, _ = cookieStepWizard(t, "advanced")
	m.HandleKey(keyEsc)
	if m.advancedFormDone || m.advancedForm == nil || m.mode != setupModeAdvanced {
		t.Errorf("advanced Esc: formDone %v form %v mode %v", m.advancedFormDone, m.advancedForm != nil, m.mode)
	}
	m, _, _ = cookieStepWizard(t, "advanced")
	m.channelIndex = 3
	m.cookieFocus = 2
	m.HandleKey(keyEnter)
	if !m.advancedCookieDone || m.channelMode != "list" || m.channelIndex != 3 {
		t.Errorf("advanced row 2: cookieDone %v mode %q index %d", m.advancedCookieDone, m.channelMode, m.channelIndex)
	}

	// E L overlay: both close it, cancelling nothing when no browser is open.
	for _, key := range []string{keyEsc, keyEnter} {
		m, _, cancels := cookieStepWizard(t, "cookie-only")
		m.cookieFocus = 2
		m.HandleKey(key)
		if m.IsVisible() || m.cookieOnly || *cancels != 0 {
			t.Errorf("cookie-only %s: visible %v cookieOnly %v cancels %d", key, m.IsVisible(), m.cookieOnly, *cancels)
		}
	}
}

// resetCookieFlow is the one reset of the cookie step, and it never touches
// the tick generation (a reset generation would let a stale tick of an
// earlier flow drain the next one's countdown) nor the platform ticks a
// finish earned.
func TestResetCookieFlowKeepsTheTickGeneration(t *testing.T) {
	m := NewSetupWizardModel()
	m.cookieActive, m.cookieFinishing, m.cookieTimedOut, m.cookieTickActive = true, true, true, true
	m.cookiePlatform, m.cookieCountdown = "twitch", 42
	m.cookieTickGen = 7
	m.cookieYTDone, m.cookieTWDone = true, true
	m.resetCookieFlow()
	if m.cookieActive || m.cookieFinishing || m.cookieTimedOut || m.cookieTickActive ||
		m.cookiePlatform != "" || m.cookieCountdown != 0 {
		t.Errorf("reset left the flow running: %+v", m)
	}
	if m.cookieTickGen != 7 {
		t.Errorf("cookieTickGen = %d, must never be reset", m.cookieTickGen)
	}
	if !m.cookieYTDone || !m.cookieTWDone {
		t.Error("the platform ticks a finish earned must survive the reset")
	}

	// And the stale chain stays stale: a tick of the old generation after a
	// reset and a new start does not count down the new flow.
	m.OnStartAutoCookie = func(string) error { return nil }
	m.visible, m.mode, m.simpleStage = true, setupModeSimple, setupSimpleCookies
	m.cookieFocus = 0
	m.HandleKey(keyEnter)
	if m.armCookieTick() == nil {
		t.Fatal("the new flow armed no countdown")
	}
	before := m.cookieCountdown
	m.UpdateComponents(cookieCountdownTickMsg{gen: 7})
	if m.cookieCountdown != before {
		t.Errorf("a stale tick drained the new countdown: %d → %d", before, m.cookieCountdown)
	}
}
