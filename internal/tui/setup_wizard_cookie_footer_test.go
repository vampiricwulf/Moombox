package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestCookieStepFooterNamesTheKeysThatWork: the cookie step's footer showed
// "Esc: Back  Enter: Select" in every state. On the timed-out prompt both keys
// do nothing (only R and S are handled); while the browser is open Esc cancels
// the sign-in, which the body said and the footer contradicted. The timed-out
// line also blamed "extraction", which never runs on that path — the countdown
// only runs while waiting for the sign-in.
func TestCookieStepFooterNamesTheKeysThatWork(t *testing.T) {
	for _, tc := range []struct {
		name      string
		set       func(m *SetupWizardModel)
		want      []string
		wantNot   []string
		wantEmpty bool
	}{
		{"selecting", func(m *SetupWizardModel) {}, []string{"Esc: Back", "Enter: Select"}, nil, false},
		{"timed out", func(m *SetupWizardModel) { m.cookieTimedOut = true }, []string{"R: Try again", "S: Skip"}, []string{"Esc", "Enter"}, false},
		{"waiting for sign-in", func(m *SetupWizardModel) { m.cookieActive = true }, []string{"Esc: Cancel", "Enter: Extract cookies"}, []string{"Back", "Select"}, false},
		{"extracting", func(m *SetupWizardModel) { m.cookieActive = true; m.cookieFinishing = true }, nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewSetupWizardModel()
			tc.set(m)
			got := ansi.Strip(m.cookieStepFooter("Esc: Back", 60))
			if tc.wantEmpty && strings.TrimSpace(got) != "" {
				t.Errorf("footer = %q, want none while every key is ignored", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("footer %q lacks %q", got, w)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("footer %q offers %q, which this state ignores", got, w)
				}
			}
		})
	}

	m := NewSetupWizardModel()
	m.SetSize(100, 40)
	m.cookieTimedOut = true
	m.cookiePlatform = "youtube"
	for name, view := range map[string]string{"simple": m.viewSimpleCookies(), "advanced": m.viewAdvancedCookies(80, 84, 30)} {
		v := ansi.Strip(view)
		if !strings.Contains(v, "Timed out waiting for the browser sign-in.") || strings.Contains(v, "extraction timed out") {
			t.Errorf("%s timed-out view does not say the sign-in wait expired:\n%s", name, v)
		}
	}
}
