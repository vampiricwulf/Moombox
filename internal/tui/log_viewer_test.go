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
