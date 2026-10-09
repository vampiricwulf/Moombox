package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// searchedPanel is a focused 100-line log panel whose lines named in hits
// carry "needle", with that search applied by / and Enter — which, from the
// following view, jumps to the first match.
func searchedPanel(t *testing.T, hits ...int) *LogViewerModel {
	t.Helper()
	m := NewLogViewerModel()
	m.SetSize(120, 15) // 12 viewport rows, 11 once the pause hint shows
	m.SetFocused(true)
	batch := make([]string, 0, 100)
	for i := range 100 {
		l := fmt.Sprintf("2026-10-09 12:00:00 INFO worker: line %03d", i)
		if slices.Contains(hits, i) {
			l += " needle"
		}
		batch = append(batch, l)
	}
	m.AddLines(batch)
	m.StartSearch()
	typeSearch(m, "needle")
	m.HandleSearchKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.matchCount != len(hits) {
		t.Fatalf("setup: matches = %d, want %d", m.matchCount, len(hits))
	}
	return m
}

// scrollTo moves the reader's view to row with the arrow keys' own path, one
// row at a time, as a reader does: each step lets the viewport re-select its
// match the way it does under ↓ and ↑.
func scrollTo(t *testing.T, m *LogViewerModel, row int) {
	t.Helper()
	for m.viewport.YOffset() < row {
		m.ScrollDown()
	}
	for m.viewport.YOffset() > row {
		m.ScrollUp()
	}
	if m.autoScroll {
		t.Fatal("setup: the reader must be paused")
	}
	if strings.Contains(stripANSI(m.viewport.View()), "needle") {
		t.Fatalf("setup: no match may be on screen at row %d", row)
	}
}

// TestSearchStepFromAViewWithNoMatchOnScreen: n and N from a view with no
// match on screen go to the nearest match below and above it. The viewport
// selects, on every scroll and every re-applied search, the first match at
// or below the top row whether it is on screen or not, and steps from there;
// the paused-view fix (W24-12) made a new line re-apply the search WITHOUT
// the scroll that used to show that match, so n stepped past a match the
// reader never saw: from line 20 with matches at 10, 50, 60 and 90 it went
// to 60, and with only 10 and 50 it wrapped up to 10. The same skip followed
// plain ↓ scrolling with no new line at all. N with nothing below the view
// stepped from "none selected" to the second-to-last match.
//
// Mutants: drop the off-screen branch of searchStep (always HighlightNext /
// HighlightPrevious) — "n after ↓", "n after a new line" and "only match
// below" go to 60 / 10, "N with nothing below" to line 010; drop the forward
// HighlightNext for nothing below — "n wraps" stays put; drop the backward
// HighlightNext, or the SetYOffset(top) after it — "N with nothing below"
// goes to line 010; drop N's first SetYOffset(top) — "N wraps to the last"
// stops on line 050 with 060 below it.
func TestSearchStepFromAViewWithNoMatchOnScreen(t *testing.T) {
	cases := []struct {
		name    string
		hits    []int
		at      int  // the reader's top row
		newLine bool // a line arrives while the reader is there
		key     rune
		wantTop string
	}{
		{name: "n after ↓", hits: []int{10, 50, 60, 90}, at: 20, key: 'n', wantTop: "line 050 needle"},
		{name: "n after a new line", hits: []int{10, 50, 60, 90}, at: 20, newLine: true, key: 'n', wantTop: "line 050 needle"},
		{name: "n, only match below", hits: []int{10, 50}, at: 20, newLine: true, key: 'n', wantTop: "line 050 needle"},
		{name: "N after a new line", hits: []int{10, 15, 50}, at: 30, newLine: true, key: 'N', wantTop: "line 015 needle"},
		{name: "N with nothing below", hits: []int{10, 15}, at: 30, newLine: true, key: 'N', wantTop: "line 015 needle"},
		{name: "n wraps to the first", hits: []int{10, 15}, at: 30, newLine: true, key: 'n', wantTop: "line 010 needle"},
		{name: "N wraps to the last", hits: []int{50, 60}, at: 20, newLine: true, key: 'N', wantTop: "line 060 needle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := searchedPanel(t, tc.hits...)
			scrollTo(t, m, tc.at)
			if tc.newLine {
				before := topRow(m)
				m.AddLine("2026-10-09 12:00:01 INFO worker: line 100")
				if topRow(m) != before {
					t.Fatalf("setup: the new line moved the paused view to %q", topRow(m))
				}
			}
			m.HandleSearchKey(tea.KeyPressMsg{Code: tc.key, Text: string(tc.key)})
			if got := topRow(m); !strings.HasSuffix(got, tc.wantTop) {
				t.Errorf("%c from line %03d went to %q, want the view on %q", tc.key, tc.at, got, tc.wantTop)
			}
		})
	}
}

// TestSearchStepThroughTheAppAfterANewLine drives both readers the way an
// operator does: / and Enter, ↓ to a stretch between matches, then a new line
// — the log channel's batch and flush on the panel, a poll of the job's
// buffer in O L — and n. Either way n must land on the next match below the
// reader, not the one after it.
//
// Mutant: drop the off-screen branch of searchStep — the panel goes to line
// 350 and the overlay to segment 120.
func TestSearchStepThroughTheAppAfterANewLine(t *testing.T) {
	t.Run("log panel", func(t *testing.T) {
		a := NewApp()
		a.width, a.height = 200, 60
		a.recalcLayout()
		batch := make([]string, 0, 400)
		for i := range 400 {
			l := pausedLine("INFO", i)
			if i == 50 || i == 300 || i == 350 {
				l += " needle"
			}
			batch = append(batch, l)
		}
		a.BackfillLogs(batch)
		a.setFocus(PanelLogs)
		for _, r := range "/needle" {
			a.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		for range 100 {
			a.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		}
		if a.logs.autoScroll || strings.Contains(stripANSI(a.logs.viewport.View()), "needle") {
			t.Fatalf("setup: the reader must be paused between matches, top %q", topRow(a.logs))
		}
		a.Update(LogBatchMsg{Lines: []string{pausedLine("INFO", 400)}})
		a.Update(logFlushMsg{})
		a.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
		if got := topRow(a.logs); !strings.HasSuffix(got, "line 300 needle") {
			t.Errorf("n went to %q, want line 300's match", got)
		}
	})
	t.Run("O L", func(t *testing.T) {
		a, logs := jobLogApp(t)
		for i := range 150 {
			line := jobLine("job1", i)
			if i == 20 || i == 100 || i == 120 {
				line += " needle"
			}
			logs.append("job1", line)
		}
		openJobLog(t, a, "job1")
		for _, r := range "/needle" {
			a.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		for range 15 {
			a.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		}
		if a.jobLog.log.autoScroll || strings.Contains(stripANSI(a.jobLog.log.viewport.View()), "needle") {
			t.Fatalf("setup: the reader must be paused between matches, top %q", topRow(a.jobLog.log))
		}
		logs.append("job1", jobLine("job1", 150))
		_, cmd := a.Update(jobLogRefreshTickMsg{Epoch: a.jobLogEpoch})
		applyStatsCmd(a, cmd)
		a.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
		if got := topRow(a.jobLog.log); !strings.HasSuffix(got, "segment 100 fetched needle") {
			t.Errorf("n went to %q, want segment 100's match", got)
		}
	})
}

// TestLogSearchOverAFilteredOutBufferFindsNothing: when F hides every line,
// the panel shows a placeholder, and an active search matches nothing in it.
// The header said "(2 matches)" over "No WARN+ lines", and the ranges found
// in the hidden lines stayed behind for n to hand back to the viewport,
// pointing past the end of the placeholder. Cycling back to every level finds
// the matches again.
//
// Mutant: drop clearSearchMatches from updateViewportContent's placeholder
// arm — the header keeps "(2 matches)".
func TestLogSearchOverAFilteredOutBufferFindsNothing(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(120, 15)
	m.SetFocused(true)
	batch := make([]string, 0, 60)
	for i := range 60 {
		l := pausedLine("INFO", i)
		if i == 5 || i == 40 {
			l += " needle"
		}
		batch = append(batch, l)
	}
	m.AddLines(batch)
	m.StartSearch()
	typeSearch(m, "needle")
	m.HandleSearchKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.CycleLevel() // INFO+
	m.CycleLevel() // WARN+
	view := stripANSI(m.View())
	if !strings.Contains(view, "No WARN+ lines") || !strings.Contains(view, "(0 matches)") {
		t.Errorf("a search over a buffer F hides entirely must find nothing:\n%s", view)
	}
	for _, k := range []rune{'n', 'N'} {
		m.HandleSearchKey(tea.KeyPressMsg{Code: k, Text: string(k)})
		if got := stripANSI(m.viewport.View()); !strings.Contains(got, "No WARN+ lines") || strings.Contains(got, "needle") {
			t.Errorf("%c over the placeholder changed it:\n%s", k, got)
		}
	}
	m.CycleLevel() // ERROR+
	m.CycleLevel() // every level
	if m.matchCount != 2 || !strings.Contains(stripANSI(m.View()), "(2 matches)") {
		t.Errorf("back at every level the search must find both matches again, got %d", m.matchCount)
	}
}

// TestSearchStepWalksTheMatchesOnScreen: with the selected match on screen n
// and N step from it, through matches that share the screen, without moving
// the view — searchStep re-anchors only when no match is visible.
//
// Mutant: re-anchor whatever is on screen (drop searchStep's on-screen
// branch) — every n re-selects the first match at the top and never moves.
func TestSearchStepWalksTheMatchesOnScreen(t *testing.T) {
	m := searchedPanel(t, 10, 12, 14)
	frames := []string{m.viewport.View()}
	for range 2 {
		m.HandleSearchKey(tea.KeyPressMsg{Code: 'n', Text: "n"})
		frames = append(frames, m.viewport.View())
	}
	if !strings.HasSuffix(topRow(m), "line 010 needle") {
		t.Errorf("n between matches on one screen scrolled the view to %q", topRow(m))
	}
	if frames[0] == frames[1] || frames[1] == frames[2] || frames[0] == frames[2] {
		t.Error("n must select the next match on screen each time")
	}
	m.HandleSearchKey(tea.KeyPressMsg{Code: 'N', Text: "N"})
	if m.viewport.View() != frames[1] {
		t.Error("N must step back to the match before")
	}
	m.HandleSearchKey(tea.KeyPressMsg{Code: 'N', Text: "N"})
	if m.viewport.View() != frames[0] {
		t.Error("a second N must step back to the first match")
	}
}
