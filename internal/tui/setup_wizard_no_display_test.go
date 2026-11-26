package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// On Linux with no display the browser login is refused with a sentence
// that names the way in (owner ruling R4-7). The E L cookie step shows it on
// its error line, and at the smallest normal size (60x20) the line wraps
// inside the box rather than being cut, so the remedy at its end is read.
//
// Mutant it kills: the wizard error line clipped to one row of the box.
func TestCookieStepShowsTheNoDisplayRefusalWhole(t *testing.T) {
	m, _, _ := cookieStepWizard(t, "cookie-only")
	m.OnStartAutoCookie = func(string) error { return cookies.ErrNoDisplay }
	m.SetSize(60, 20)
	m.cookieFocus = 0
	m.HandleKey(keyEnter)
	if m.cookieActive {
		t.Fatal("a refused start left the cookie step waiting on a browser")
	}
	squash := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '│' {
				return -1
			}
			return r
		}, s)
	}
	view := squash(ansi.Strip(m.View()))
	if want := squash("YouTube cookies: " + cookies.ErrNoDisplay.Error()); !strings.Contains(view, want) {
		t.Errorf("the refusal is not shown whole at 60x20:\n%s", ansi.Strip(m.View()))
	}
}
