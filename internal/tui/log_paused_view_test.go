package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// pausedLine is one structured log line at the given level, numbered so a
// row names the line it came from.
func pausedLine(level string, i int) string {
	return fmt.Sprintf("2026-10-09 12:00:00 %s worker: segment fetched line %d", level, i)
}

// topRow is the first row the viewport shows, ANSI stripped — what a reader
// looking at the paused panel sees at the top of it.
func topRow(m *LogViewerModel) string {
	first, _, _ := strings.Cut(stripANSI(m.viewport.View()), "\n")
	return strings.TrimRight(first, " ")
}

// fullPausedPanel is a log panel holding maxLogLines lines (every one at the
// given level, wrapped to the given width) and scrolled up until it pauses.
func fullPausedPanel(t *testing.T, width int, line func(i int) string) *LogViewerModel {
	t.Helper()
	m := NewLogViewerModel()
	m.SetSize(width, 20)
	m.SetFocused(true)
	batch := make([]string, 0, maxLogLines)
	for i := range maxLogLines {
		batch = append(batch, line(i))
	}
	m.AddLines(batch)
	for range 10 {
		m.PageUp()
	}
	if m.autoScroll {
		t.Fatal("setup: ten PgUp presses must pause the panel")
	}
	return m
}

// TestPausedLogViewStaysOnItsLinesAtTheCap is W24-12: once the panel holds
// maxLogLines, every insertion trims the oldest line, and the viewport kept
// its YOffset over the shortened buffer — so the "Auto-scroll paused" view
// crept up a row per new line, more for wrapped lines, and could not be read
// at DEBUG or with several jobs running. A paused view must stay on the lines
// it shows while the ring evicts older ones; one it shows that are themselves
// evicted leave it at the top.
//
// Mutants: drop the SetYOffset in redisplay (every case moves); count one row
// per dropped line instead of its wrapped rows ("wrapped" moves by two rows
// per line); count one row per wrapped row ignoring an embedded "\n"
// ("multi-line" moves); drop the level-filter test in dropOldest ("hidden by
// F" jumps up by the rows of lines nobody could see).
func TestPausedLogViewStaysOnItsLinesAtTheCap(t *testing.T) {
	cases := []struct {
		name  string
		width int
		line  func(i int) string
		setup func(m *LogViewerModel)
		add   func(m *LogViewerModel)
	}{
		{
			name:  "one line at a time",
			width: 200,
			line:  func(i int) string { return pausedLine("INFO", i) },
			add: func(m *LogViewerModel) {
				for i := range 5 {
					m.AddLine(pausedLine("INFO", maxLogLines+i))
				}
			},
		},
		{
			name:  "a batch",
			width: 200,
			line:  func(i int) string { return pausedLine("INFO", i) },
			add: func(m *LogViewerModel) {
				m.AddLines([]string{pausedLine("INFO", maxLogLines), pausedLine("INFO", maxLogLines+1), pausedLine("INFO", maxLogLines+2)})
			},
		},
		{
			// Every source line cuts into three rows at a 60-column panel.
			name:  "wrapped",
			width: 62,
			line: func(i int) string {
				return pausedLine("INFO", i) + " " + strings.Repeat(fmt.Sprintf("p%04d ", i), 12)
			},
			add: func(m *LogViewerModel) {
				m.AddLine(pausedLine("INFO", maxLogLines) + " x")
				m.AddLine(pausedLine("INFO", maxLogLines+1) + " y")
			},
		},
		{
			// A line that fits the width but carries a stderr tail: one
			// wrapped row, two viewport rows.
			name:  "multi-line",
			width: 200,
			line: func(i int) string {
				if i < 5 {
					return pausedLine("ERROR", i) + " stderr=No such file\nConversion failed!"
				}
				return pausedLine("INFO", i)
			},
			add: func(m *LogViewerModel) {
				for i := range 3 {
					m.AddLine(pausedLine("INFO", maxLogLines+i))
				}
			},
		},
		{
			// The evicted INFO lines were cut while F showed everything and
			// then hidden by WARN+: they leave no rows above the view.
			name:  "hidden by F",
			width: 200,
			line: func(i int) string {
				if i%2 == 0 {
					return pausedLine("INFO", i)
				}
				return pausedLine("WARN", i)
			},
			setup: func(m *LogViewerModel) {
				m.CycleLevel() // INFO+
				m.CycleLevel() // WARN+
				for range 10 {
					m.PageUp()
				}
				if m.autoScroll {
					t.Fatal("setup: the WARN+ view must pause too")
				}
			},
			add: func(m *LogViewerModel) {
				m.AddLines([]string{pausedLine("INFO", maxLogLines), pausedLine("INFO", maxLogLines+2)})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := fullPausedPanel(t, tc.width, tc.line)
			if tc.setup != nil {
				tc.setup(m)
			}
			before := topRow(m)
			tc.add(m)
			if after := topRow(m); after != before {
				t.Errorf("the paused view moved: its top row was %q, now %q", before, after)
			}
			if m.autoScroll {
				t.Error("an insertion must not un-pause the panel")
			}
		})
	}
}

// TestPausedLogViewAtTheTopKeepsTheOldestSurvivor: a reader paused on the
// very oldest lines watches them be evicted. The offset cannot go above the
// top, so the view shows whatever is oldest now rather than an error or a
// later page.
func TestPausedLogViewAtTheTopKeepsTheOldestSurvivor(t *testing.T) {
	m := fullPausedPanel(t, 200, func(i int) string { return pausedLine("INFO", i) })
	m.viewport.GotoTop()
	m.AddLines([]string{pausedLine("INFO", maxLogLines), pausedLine("INFO", maxLogLines+1)})
	if got, want := topRow(m), pausedLine("INFO", 2); got != want {
		t.Errorf("top row = %q, want the oldest surviving line %q", got, want)
	}
	if m.viewport.YOffset() != 0 {
		t.Errorf("YOffset = %d, want 0", m.viewport.YOffset())
	}
}

// TestPausedLogViewWithASearchDoesNotJumpToTheNextMatch: re-applying an
// active search after a content change went through SetHighlights, which
// SCROLLS to the first match at or below the top row when none is on screen
// — so a reader paused between matches was carried down to the next one by
// every new line, below the cap as well as at it. Enter still jumps (it calls
// applySearchHighlights itself); a content change keeps the reader's place.
//
// Mutant: drop the SetYOffset(top) after applySearchHighlights in
// updateViewportContent — the view jumps to line 300's match.
func TestPausedLogViewWithASearchDoesNotJumpToTheNextMatch(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(200, 20)
	m.SetFocused(true)
	batch := make([]string, 0, 400)
	for i := range 400 {
		l := pausedLine("INFO", i)
		if i == 50 || i == 300 {
			l += " needle"
		}
		batch = append(batch, l)
	}
	m.AddLines(batch)
	m.StartSearch()
	typeSearch(m, "needle")
	m.HandleSearchKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.matchCount != 2 {
		t.Fatalf("setup: matches = %d, want 2", m.matchCount)
	}
	// Park between the two matches, with neither on screen.
	m.viewport.SetYOffset(150)
	m.setAutoScroll(m.viewport.AtBottom())
	if m.autoScroll {
		t.Fatal("setup: the panel must be paused")
	}
	before := topRow(m)
	m.AddLine(pausedLine("INFO", 400))
	if after := topRow(m); after != before {
		t.Errorf("a new line moved the paused view from %q to %q", before, after)
	}
	if m.matchCount != 2 {
		t.Errorf("matches = %d after the insertion, want 2 — the highlights must still be applied", m.matchCount)
	}
}

// TestPausedLogPanelStaysPutThroughTheApp drives the production path: PgUp
// through App.Update pauses the focused panel, and the log channel's
// LogBatchMsg plus the flush that follows it land on a full buffer.
func TestPausedLogPanelStaysPutThroughTheApp(t *testing.T) {
	a := NewApp()
	a.width, a.height = 200, 60
	a.recalcLayout()
	batch := make([]string, 0, maxLogLines)
	for i := range maxLogLines {
		batch = append(batch, pausedLine("INFO", i))
	}
	a.BackfillLogs(batch)
	a.setFocus(PanelLogs)
	for range 10 {
		a.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	if a.logs.autoScroll {
		t.Fatal("setup: PgUp must pause the panel")
	}
	before := topRow(a.logs)
	for i := range 5 {
		a.Update(LogBatchMsg{Lines: []string{pausedLine("INFO", maxLogLines+i)}})
		a.Update(logFlushMsg{})
	}
	if after := topRow(a.logs); after != before {
		t.Errorf("the paused panel moved from %q to %q", before, after)
	}
	if !strings.Contains(stripANSI(a.View().Content), "Auto-scroll paused") {
		t.Error("the panel must still say it is paused")
	}
}
