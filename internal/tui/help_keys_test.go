package tui

import (
	"strings"
	"testing"
)

// The help overlay is the only discoverable path to batch mode, and it listed
// neither Space nor the keys that leave it. Mutant: dropping any of the four
// rows — the operator has no way to learn the key exists.
func TestHelpDocumentsTheBatchAndScrollKeys(t *testing.T) {
	app := NewApp()
	h := NewHelpModel()
	h.SetMenuItems(app.buildMenuItems())
	h.SetSize(100, 200)
	h.Toggle()

	view := stripANSI(h.viewport.View())
	for _, want := range []string{
		"Space", "Select task for batch actions (Tasks)",
		"Esc", "Clear batch selection",
		"Ctrl+U/Ctrl+D", "Half-page scroll",
		"End", "Resume auto-scroll (Logs)",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("help view lacks %q:\n%s", want, view)
		}
	}
}

// Only keys that do something are documented. Home and PgUp/PgDn are no-ops
// on the Tasks panel (handleTaskKey handles up/down/enter only, and
// routeComponentMsg does not feed the list's KeyMap), so the help must not
// claim otherwise — PgUp/PgDn is already marked (Logs).
func TestHelpDoesNotClaimTaskPanelPaging(t *testing.T) {
	app := NewApp()
	h := NewHelpModel()
	h.SetMenuItems(app.buildMenuItems())
	h.SetSize(100, 200)
	h.Toggle()

	for _, line := range strings.Split(stripANSI(h.viewport.View()), "\n") {
		if strings.Contains(line, "Home") {
			t.Errorf("help documents Home, which no panel handles: %q", line)
		}
		if strings.Contains(line, "PgUp") && !strings.Contains(line, "(Logs)") {
			t.Errorf("PgUp/PgDn works on the log viewport only: %q", line)
		}
	}
}
