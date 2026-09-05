package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// typeSearch feeds each rune of s through the search textinput, the same
// path the app uses while IsSearching() is true (UpdateSearchInput is fed
// from routeComponentMsg; HandleSearchKey only consumes the keystroke).
func typeSearch(m *LogViewerModel, s string) {
	for _, r := range s {
		m.UpdateSearchInput(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// TestLogViewerClearEmptiesEverything: Clear drops the history, the filtered
// view, and any active search so the next AddLine starts a fresh view.
func TestLogViewerClearEmptiesEverything(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(80, 20)
	m.AddLines([]string{"[INFO] one", "[WARN] two", "[INFO] three"})
	m.StartSearch()
	typeSearch(m, "two")
	m.HandleSearchKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Clear()
	if len(m.lines) != 0 || len(m.filtered) != 0 || m.searchQuery != "" || m.searchRegex != nil || m.matchCount != 0 {
		t.Fatalf("Clear left state: lines=%d filtered=%d query=%q", len(m.lines), len(m.filtered), m.searchQuery)
	}
	if strings.Contains(m.View(), "two") {
		t.Fatal("cleared line still rendered")
	}
	m.AddLine("[INFO] four")
	if !strings.Contains(m.View(), "four") {
		t.Fatal("AddLine after Clear must render")
	}
}

// TestLogClearKeyRoutesThroughTheApp pins the guard, not just the method:
// c clears only while the log panel is focused, and the same keypress on
// the task panel must leave the log alone (there it is the A C chord's
// second key and the "Invalid Chord" fallback's business).
func TestLogClearKeyRoutesThroughTheApp(t *testing.T) {
	app := NewApp()
	app.logs.SetSize(80, 20)
	app.logs.AddLines([]string{"[INFO] one", "[WARN] two"})

	app.focusedPanel = PanelLogs
	app.handleKey(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if len(app.logs.lines) != 0 {
		t.Fatalf("c on the log panel left %d lines", len(app.logs.lines))
	}
	if !strings.Contains(app.feedback.msg, "Log view cleared") {
		t.Errorf("feedback = %q, want it to say the view was cleared", app.feedback.msg)
	}

	app.logs.AddLine("[INFO] three")
	app.focusedPanel = PanelTasks
	app.handleKey(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if len(app.logs.lines) != 1 {
		t.Errorf("c with the task panel focused cleared the log (%d lines left)", len(app.logs.lines))
	}
}
