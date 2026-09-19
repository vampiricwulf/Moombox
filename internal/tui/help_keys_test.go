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

// O C hands the URL to the terminal over OSC 52 and, on a local Windows
// console, to clip.exe as well — neither of which is a copy the TUI can
// promise in advance. The menu label is the one place the chord announces
// itself BEFORE it runs, so it must hedge; and it must hedge without naming
// a mechanism, because the label renders identically on every platform and
// each mechanism is absent on some of them — naming OSC 52 is wrong on the
// local Windows console the fallback was written for, and naming clip.exe
// is wrong everywhere else (CORE-15, O-W; B6).
//
// Mutant: restoring the bare "Copy Stream URL" label.
// Mutant: putting "(OSC 52)" (or "clip.exe") back in the label.
func TestCopyChordLabelIsHedged(t *testing.T) {
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
	if label == "Copy Stream URL" {
		t.Errorf("O C label = %q, want a hedge — the press cannot promise a completed copy", label)
	}
	for _, mechanism := range []string{"OSC 52", "clip.exe"} {
		if strings.Contains(label, mechanism) {
			t.Errorf("O C label = %q, must not name %q — it is not the mechanism on every platform", label, mechanism)
		}
	}
}
