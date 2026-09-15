package tui

import (
	"fmt"
	"testing"
)

// The level cache must stay index-aligned with the lines it describes,
// including across the 1000-line cap. Mutant: trimming m.lines without
// trimming m.levels — every line would then be coloured as its predecessor.
func TestLogLevelsStayInStepWithLines(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(80, 20)
	levels := []string{"INFO", "WARN", "ERROR", "DEBUG"}
	batch := make([]string, 0, 1500)
	for i := range 1500 {
		batch = append(batch, fmt.Sprintf("2026-01-01 00:00:00 %s line %d", levels[i%len(levels)], i))
	}
	m.AddLines(batch)

	if len(m.lines) != maxLogLines || len(m.levels) != len(m.lines) {
		t.Fatalf("lines=%d levels=%d, want %d each", len(m.lines), len(m.levels), maxLogLines)
	}
	for i, line := range m.lines {
		if want := extractLogLevel(line); m.levels[i] != want {
			t.Fatalf("levels[%d] = %q, want %q (line %q)", i, m.levels[i], want, line)
		}
	}

	// The single-line path caps through the same helper.
	m.AddLine("2026-01-01 00:00:00 ERROR one more")
	if len(m.levels) != len(m.lines) || m.levels[len(m.levels)-1] != "ERROR" {
		t.Fatalf("AddLine broke the pairing: lines=%d levels=%d last=%q",
			len(m.lines), len(m.levels), m.levels[len(m.levels)-1])
	}
}

// styleLogLine reads the cached level rather than re-parsing the text.
// Mutant: leaving the parse in styleLogLine — the corrupted cache entry would
// then be ignored and the colour would follow the text.
func TestStyleLogLineReadsTheCachedLevel(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(80, 20)
	m.AddLines([]string{"2026-01-01 00:00:00 ERROR boom"})

	if got := m.styleLogLine(0).GetForeground(); got != ColorLogError {
		t.Fatalf("an ERROR line must render in ColorLogError, got %v", got)
	}
	m.filteredLevels[0] = "DEBUG"
	if got := m.styleLogLine(0).GetForeground(); got != ColorLogDebug {
		t.Errorf("styleLogLine re-parsed the line text instead of reading the cached level (got %v)", got)
	}
}

// The filtered view carries its own aligned level slice, and the filter reads
// the cache instead of re-parsing.
func TestFilteredLevelsFollowTheFilter(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(80, 20)
	m.AddLines([]string{
		"2026-01-01 00:00:00 INFO i",
		"2026-01-01 00:00:00 ERROR e",
		"2026-01-01 00:00:00 WARN w",
		"no level marker here",
	})

	if len(m.filteredLevels) != len(m.filtered) {
		t.Fatalf("ALL: filtered=%d filteredLevels=%d", len(m.filtered), len(m.filteredLevels))
	}

	m.level = LogLevelWarn
	m.rebuildFiltered()
	if len(m.filtered) != 3 || len(m.filteredLevels) != 3 {
		t.Fatalf("WARN: filtered=%v levels=%v, want the ERROR, WARN and unmarked lines", m.filtered, m.filteredLevels)
	}
	if m.filteredLevels[0] != "ERROR" || m.filteredLevels[1] != "WARN" || m.filteredLevels[2] != "" {
		t.Errorf("filteredLevels = %v, want [ERROR WARN \"\"]", m.filteredLevels)
	}
}

// Clear drops the cache with the history; a stale level slice would outlive
// the lines it described and mis-colour the next batch.
func TestClearDropsTheLevelCache(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(80, 20)
	m.AddLines([]string{"2026-01-01 00:00:00 ERROR boom"})
	m.Clear()
	if len(m.levels) != 0 || len(m.filteredLevels) != 0 {
		t.Fatalf("Clear left levels=%v filteredLevels=%v", m.levels, m.filteredLevels)
	}
	m.AddLine("2026-01-01 00:00:00 DEBUG quiet")
	if got := m.styleLogLine(0).GetForeground(); got != ColorLogDebug {
		t.Errorf("the first line after Clear rendered as %v, want ColorLogDebug", got)
	}
}
