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

// The help must claim exactly the panels that page. Arc 6 pinned "(Details ·
// Logs)" because the Tasks panel did not page; O-X adds the paging, so the
// claim widens and Home/End become documented rather than forbidden
// (CORE-16).
//
// Mutant: adding the four cases to handleTaskKey without widening the help
// row — the PgUp line still reads "(Details · Logs)" and fails here.
func TestHelpClaimsTaskPanelPaging(t *testing.T) {
	app := NewApp()
	h := NewHelpModel()
	h.SetMenuItems(app.buildMenuItems())
	h.SetSize(100, 200)
	h.Toggle()

	view := stripANSI(h.viewport.View())
	sawPaging, sawHomeEnd := false, false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "PgUp") {
			sawPaging = true
			if !strings.Contains(line, "(Tasks · Details · Logs)") {
				t.Errorf("PgUp/PgDn pages all three panels now: %q", line)
			}
		}
		if strings.Contains(line, "Home/End") {
			sawHomeEnd = true
			if !strings.Contains(line, "(Tasks)") {
				t.Errorf("Home/End is the task list's jump: %q", line)
			}
		}
	}
	if !sawPaging {
		t.Error("the help must document PgUp/PgDn")
	}
	if !sawHomeEnd {
		t.Error("the help must document Home/End now that the task list handles them")
	}
}

// O C hands the URL to the terminal over OSC 52 and, on Windows outside
// Windows Terminal, to clip.exe — neither of which is a copy the TUI can
// promise in advance. The menu label is the one place the chord announces
// itself before it runs, so it must not promise one either (CORE-15, O-W).
//
// Mutant: restoring the bare "Copy Stream URL" label.
func TestCopyChordLabelNamesTheMechanism(t *testing.T) {
	app := NewApp()
	var label string
	for _, item := range app.buildMenuItems() {
		if item.Chord == "O C" {
			label = item.Label
		}
	}
	if label == "" {
		t.Fatal("no O C item in buildMenuItems()")
	}
	if !strings.Contains(label, "OSC 52") {
		t.Errorf("O C label = %q, want it to name the mechanism it actually uses", label)
	}
}
