package tui

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// benchLogPanel builds a log panel holding maxLogLines lines at the size the
// panel gets in the 200x60 terminal the frame budget was measured in (the log
// panel is the full terminal width and, unfocused, the lower ~30% of it).
func benchLogPanel() *LogViewerModel {
	m := NewLogViewerModel()
	m.SetSize(200, 18)
	batch := make([]string, 0, maxLogLines)
	for i := range maxLogLines {
		batch = append(batch, fmt.Sprintf(
			"2026-09-17 12:%02d:%02d INFO worker: job dQw4w9WgXcQ segment %d fetched (%d bytes)",
			i/60%60, i%60, i, 1024*(i%97)))
	}
	m.AddLines(batch)
	return m
}

// BenchmarkLogViewerView is the frame-budget number: bubbletea calls View()
// after every message (~120/s) and the vast majority carry no log change.
func BenchmarkLogViewerView(b *testing.B) {
	m := benchLogPanel()
	b.ReportAllocs()
	for b.Loop() {
		_ = m.View()
	}
}

// BenchmarkLogViewerViewScrolling measures the render itself rather than any
// memo of it: the scroll position moves every iteration, so nothing can be
// reused between frames.
func BenchmarkLogViewerViewScrolling(b *testing.B) {
	m := benchLogPanel()
	off := m.viewport.YOffset()
	b.ReportAllocs()
	var i int
	for b.Loop() {
		i ^= 1
		m.viewport.SetYOffset(off - i)
		_ = m.View()
	}
}

// BenchmarkLogViewerAddLine is the other side of the CORE-2 trade: wrapping
// at insertion makes this path do the width pass the render used to do. It
// runs ~10 times a second against View()'s ~120.
func BenchmarkLogViewerAddLine(b *testing.B) {
	m := benchLogPanel()
	b.ReportAllocs()
	for b.Loop() {
		m.AddLine("2026-09-17 12:00:00 INFO worker: job dQw4w9WgXcQ segment 0 fetched (1024 bytes)")
	}
}

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

// The viewport must never soft-wrap: SoftWrap makes calculateLine walk every
// buffered line on EVERY render (2.35 ms at the 1,000-line cap, 60% of a
// whole frame), so lines are hard-wrapped once at insertion instead
// (CORE-2). Every display line is therefore at or under the content width.
//
// Mutant: restoring vp.SoftWrap = true and feeding raw lines — the long line
// stays one 400-column entry and the width assertion fails.
func TestLogLinesAreWrappedAtInsertion(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(42, 12) // content width 40
	m.AddLine("2026-09-17 12:00:00 INFO " + strings.Repeat("x", 400))

	if m.viewport.SoftWrap {
		t.Error("the viewport must not soft-wrap; lines are pre-wrapped at insertion")
	}
	if len(m.filtered) < 2 {
		t.Fatalf("a 425-column line at content width 40 must wrap into several display lines, got %d", len(m.filtered))
	}
	for i, line := range m.filtered {
		if w := ansi.StringWidth(line); w > 40 {
			t.Errorf("display line %d is %d columns wide, want <= 40", i, w)
		}
	}
	if len(m.filtered) != len(m.filteredLevels) {
		t.Fatalf("levels must stay in lock-step with display lines: %d vs %d", len(m.filtered), len(m.filteredLevels))
	}
	for i, lvl := range m.filteredLevels {
		if lvl != "INFO" {
			t.Errorf("continuation line %d carries level %q, want INFO from its source line", i, lvl)
		}
	}
}

// The header counts LOG LINES, not display lines. Wrapping must not inflate
// it — "Logs (1)" for one wrapped line, never "Logs (11)".
//
// Mutant: rendering len(m.filtered) in the header again.
func TestLogHeaderCountsSourceLinesNotWrappedOnes(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(42, 12)
	m.AddLine("2026-09-17 12:00:00 INFO " + strings.Repeat("y", 400))

	if got := stripANSI(m.View()); !strings.Contains(got, "Logs (1)") {
		t.Errorf("header must read \"Logs (1)\" for one wrapped line, got:\n%s", got)
	}
}

// A width change re-wraps the buffer: without it the panel keeps rows wrapped
// to the OLD width, which Tab (each panel gets a different share of the
// terminal) changes on every press.
//
// Mutant: dropping the width comparison in SetSize so rebuildFiltered is not
// re-run — the display lines stay 40 columns wide after the shrink.
func TestLogSetSizeRewrapsTheBuffer(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(42, 12)
	m.AddLine("2026-09-17 12:00:00 INFO " + strings.Repeat("z", 400))
	wide := len(m.filtered)

	m.SetSize(22, 12) // content width 20
	if len(m.filtered) <= wide {
		t.Fatalf("halving the width must produce more display lines: %d -> %d", wide, len(m.filtered))
	}
	for i, line := range m.filtered {
		if w := ansi.StringWidth(line); w > 20 {
			t.Errorf("display line %d is %d columns wide after the resize, want <= 20", i, w)
		}
	}
}

// Lines arrive before the first WindowSizeMsg — App.BackfillLogs seeds the
// logger's ring buffer into the panel straight after NewApp, while the panel
// still has no width. There is nothing to wrap to yet, so the backlog is
// left alone rather than cut to a one-column floor (1,000 lines of ~100
// columns would become ~100,000 display rows for one frame); the first
// SetSize wraps it properly.
//
// Mutant: dropping the m.width <= 0 branch from contentWidth so it returns
// max(m.width-2, 1) — the unsized panel shreds the backlog into single
// columns and the first assertion fails.
func TestLogBacklogIsNotWrappedBeforeTheFirstSize(t *testing.T) {
	m := NewLogViewerModel()
	m.AddLines([]string{
		"2026-09-17 12:00:00 INFO " + strings.Repeat("q", 200),
		"2026-09-17 12:00:01 WARN short",
	})
	if len(m.filtered) != 2 {
		t.Fatalf("an unsized panel must not wrap: %d display lines for 2 log lines", len(m.filtered))
	}

	m.SetSize(42, 12) // content width 40
	if len(m.filtered) != 7 {
		t.Fatalf("the first SetSize must wrap the backlog: got %d display lines, want 6 + 1", len(m.filtered))
	}
	for i, line := range m.filtered {
		if w := ansi.StringWidth(line); w > 40 {
			t.Errorf("display line %d is %d columns wide, want <= 40", i, w)
		}
	}
}

// The rendered panel is cached: a frame that changes nothing about the log
// panel must not re-render it. The probe writes straight into the viewport,
// bypassing updateViewportContent (the only thing that bumps contentSeq) and
// keeping the line COUNT identical, so nothing in the key moves — exactly
// the "message that changed nothing here" case bubbletea delivers ~120 times
// a second.
//
// Mutant: deleting the cache lookup at the top of View() — the injected text
// appears in the second frame.
func TestLogViewIsCachedBetweenIdenticalFrames(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(60, 12)
	m.AddLine("2026-09-17 12:00:00 INFO original")
	first := m.View()

	inject := make([]string, len(m.filtered))
	for i := range inject {
		inject[i] = "MUTATED"
	}
	m.viewport.SetContentLines(inject)

	if second := m.View(); second != first {
		t.Errorf("an unchanged log panel must return the cached frame; got a re-render:\n%s", stripANSI(second))
	}

	// Not a vacuous assertion: the injection IS visible once the cache is
	// dropped, so the previous check proved the cache and not an inert probe.
	m.invalidate()
	if got := stripANSI(m.View()); !strings.Contains(got, "MUTATED") {
		t.Fatalf("the probe must be visible without the cache, got:\n%s", got)
	}

	// Any real content change invalidates it through contentSeq.
	m.AddLine("2026-09-17 12:00:01 INFO second")
	if got := stripANSI(m.View()); !strings.Contains(got, "second") {
		t.Errorf("a new line must invalidate the cache, got:\n%s", got)
	}
}

// The log panel's chords keep working over WRAPPED content: / finds a term
// that sits on a continuation row, n/N step between matches, and c clears.
// Nothing else pinned n/N, and after CORE-2 the viewport holds display rows
// rather than source lines, so this walks the whole documented chord path
// (app.handleKey) at a width that actually wraps.
//
// Mutant: dropping m.invalidate() after HighlightNext/HighlightPrevious —
// n and N move a highlight index bubbles keeps private, so the memoised
// frame is served again and the selected match never appears to move.
func TestLogSearchAndClearWorkOverWrappedLines(t *testing.T) {
	app := NewApp()
	app.focusedPanel = PanelLogs
	app.logs.SetSize(42, 12) // content width 40
	// "needle" starts at column 45 of the first line, i.e. on its SECOND
	// display row once the line is wrapped at 40.
	app.logs.AddLines([]string{
		"2026-09-17 12:00:00 INFO " + strings.Repeat("a", 20) + "needle" + strings.Repeat("b", 40),
		"2026-09-17 12:00:01 WARN needle again",
	})
	if len(app.logs.filtered) < 4 {
		t.Fatalf("setup: want a buffer that wraps, got %d display lines", len(app.logs.filtered))
	}

	app.handleKey(tea.KeyPressMsg{Code: '/', Text: "/"})
	if !app.logs.IsSearching() {
		t.Fatal("/ on the log panel must open the search box")
	}
	typeSearch(app.logs, "needle")
	app.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	if app.logs.matchCount != 2 {
		t.Fatalf("matches = %d, want 2 — the match on a wrapped continuation row must still be found", app.logs.matchCount)
	}
	first := app.logs.View()
	if got := stripANSI(first); !strings.Contains(got, "[/needle] (2 matches)") {
		t.Errorf("header must report the active search, got:\n%s", got)
	}

	app.handleKey(tea.KeyPressMsg{Code: 'n', Text: "n"})
	next := app.logs.View()
	if next == first {
		t.Error("n must move the selected match — the frame is unchanged")
	}
	app.handleKey(tea.KeyPressMsg{Code: 'N', Text: "N"})
	if back := app.logs.View(); back != first {
		t.Error("N must step back to the match n left")
	}

	app.handleKey(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if len(app.logs.lines) != 0 || app.logs.rawCount != 0 || app.logs.matchCount != 0 || app.logs.searchQuery != "" {
		t.Fatalf("c must clear history and search: lines=%d rawCount=%d matches=%d query=%q",
			len(app.logs.lines), app.logs.rawCount, app.logs.matchCount, app.logs.searchQuery)
	}
	if got := stripANSI(app.logs.View()); !strings.Contains(got, "Logs (0)") || strings.Contains(got, "needle") {
		t.Errorf("the cleared panel still renders the old buffer:\n%s", got)
	}
}

// maxInsertionAllocsAtCap bounds ONE AddLines flush of a handful of lines
// into a full buffer at a geometry where every line wraps. Before the
// per-line wrap cache capLines trimmed on every insertion and rebuildFiltered
// re-cut the entire 1,000-line buffer, so a steady-state 24/7 process paid
// ~22,000 allocations (and 1.25 MB) roughly ten times a second for rows it
// had already produced. With the cache only the NEW lines are cut.
//
// Generous on purpose — the floor is the 10 new lines' own rows plus the
// filtered/filteredLevels growth, and the budget is about two orders of
// magnitude below the un-cached number, which is the only distinction worth
// asserting.
const maxInsertionAllocsAtCap = 2000

// insertionAllocsRuns is how many back-to-back insertions one probe averages
// over. Small on purpose: the buffer is already at the cap, so every call
// trims exactly what it adds and the path is in steady state from the first
// warm-up — and the mutant this guards against costs over a second per call.
const insertionAllocsRuns = 8

// insertionAllocs reports the heap allocations and bytes one insertion costs
// in steady state. Deliberately NOT steadyStateAllocs: that probe's
// wall-clock-second guard exists for the render caches that key on the
// second, and nothing here renders — while the un-cached shape it is meant to
// catch is slow enough that the guard would report a straddle instead of the
// budget.
func insertionAllocs(body func()) (allocs, bytes uint64) {
	var before, after runtime.MemStats
	for range 2 {
		body()
	}
	runtime.ReadMemStats(&before)
	for range insertionAllocsRuns {
		body()
	}
	runtime.ReadMemStats(&after)
	return (after.Mallocs - before.Mallocs) / insertionAllocsRuns,
		(after.TotalAlloc - before.TotalAlloc) / insertionAllocsRuns
}

// A full buffer at a narrow panel is the shape B-7 measured: 60 columns of
// content, source lines ~225 columns wide, so every line cuts into four rows.
//
// Mutant: drop the `if m.wrapped[i] == nil` guard (cut unconditionally) or
// re-allocate m.wrapped on every rebuild — the flush costs ~22,000
// allocations again. The byte-identity assertion is the other half: the
// cached rows must equal what a from-scratch rebuild produces.
func TestInsertionAtTheCapDoesNotReWrapTheBuffer(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(62, 20) // content width 60
	wide := make([]string, maxLogLines)
	for i := range wide {
		wide[i] = fmt.Sprintf("2026-09-17 12:00:00 INFO worker: job dQw4w9WgXcQ segment %d %s",
			i, strings.Repeat("x", 160))
	}
	m.AddLines(wide)
	if len(m.lines) != maxLogLines {
		t.Fatalf("fixture holds %d lines, want the %d-line cap", len(m.lines), maxLogLines)
	}
	if len(m.filtered) < 3*maxLogLines {
		t.Fatalf("fixture produced %d display rows for %d source lines — the lines must wrap",
			len(m.filtered), maxLogLines)
	}

	batch := make([]string, 10)
	for i := range batch {
		batch[i] = fmt.Sprintf("2026-09-17 12:01:00 WARN worker: late segment %d %s",
			i, strings.Repeat("y", 170))
	}
	allocs, bytes := insertionAllocs(func() { m.AddLines(batch) })
	t.Logf("AddLines(10) at the cap, 60-column panel: %d allocs/op, %d B/op", allocs, bytes)
	if allocs > maxInsertionAllocsAtCap {
		t.Errorf("one insertion into a full buffer allocates %d times, budget %d — every wrapping "+
			"line in the buffer is being re-cut on a path that runs ~10 times a second",
			allocs, maxInsertionAllocsAtCap)
	}

	// The cache must be invisible: the same buffer rebuilt from scratch
	// produces byte-identical display rows.
	fresh := NewLogViewerModel()
	fresh.SetSize(62, 20)
	fresh.AddLines(m.lines)
	if len(fresh.filtered) != len(m.filtered) {
		t.Fatalf("cached rebuild has %d display rows, a fresh one %d", len(m.filtered), len(fresh.filtered))
	}
	for i := range fresh.filtered {
		if fresh.filtered[i] != m.filtered[i] {
			t.Fatalf("display row %d differs: cached %q, fresh %q", i, m.filtered[i], fresh.filtered[i])
		}
		if fresh.filteredLevels[i] != m.filteredLevels[i] {
			t.Fatalf("display row %d level differs: cached %q, fresh %q",
				i, m.filteredLevels[i], fresh.filteredLevels[i])
		}
	}
}

// An unsized panel (BackfillLogs seeds the ring buffer before the first
// WindowSizeMsg) wraps nothing: wrapLogLine's width guard returns the line
// whole. Nothing else calls it with 0 now that rebuildFiltered routes every
// line through it, so it gets its own assertion. (The guard once also kept the
// exact-column cut from dividing by zero; ansi.Wrap answers a zero width with
// the line as it is, so today it states the contract rather than averting a
// crash.)
func TestWrapLogLineAtZeroWidthReturnsTheLine(t *testing.T) {
	got := wrapLogLine("abc", 0)
	if len(got) != 1 || got[0] != "abc" {
		t.Errorf("wrapLogLine(%q, 0) = %#v, want one unwrapped element", "abc", got)
	}
}

// TestLogFilterHidingEveryLineSaysSo: with lines in the buffer and a level
// filter that hides them all, the panel read "No logs yet." under a header
// saying "Logs (0) [WARN+]" — as if nothing had been logged at all.
//
// Mutant: dropping the filtered-empty arm — "No logs yet." again.
func TestLogFilterHidingEveryLineSaysSo(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(80, 10)
	m.AddLine("2026-10-05 12:00:00 INFO  something ordinary happened")
	m.CycleLevel() // INFO+
	m.CycleLevel() // WARN+
	view := stripANSI(m.View())
	if strings.Contains(view, "No logs yet.") || !strings.Contains(view, "No WARN+ lines") {
		t.Errorf("a filter hiding every line is not named:\n%s", view)
	}

	empty := NewLogViewerModel()
	empty.SetSize(80, 10)
	empty.CycleLevel()
	if !strings.Contains(stripANSI(empty.View()), "No logs yet.") {
		t.Error("an empty buffer no longer says No logs yet.")
	}
}

// Log lines wrap at their spaces: the exact-column cut split the very tokens
// a line is read for ("addr=127.0.0." on one row, "1:7743" on the next). A
// word longer than a whole row still breaks, and nothing is lost either way.
//
// Mutant: wrapLogLine cutting at exact columns again.
func TestWrapLogLineKeepsTokensWhole(t *testing.T) {
	line := "2026-10-05 04:15:51 INFO web server starting addr=127.0.0.1:7743 scheme=http"
	rows := wrapLogLine(line, 58)
	if len(rows) < 2 {
		t.Fatalf("the fixture must wrap at 58: %q", rows)
	}
	whole := false
	for _, r := range rows {
		if w := ansi.StringWidth(r); w > 58 {
			t.Errorf("row is %d columns: %q", w, r)
		}
		whole = whole || strings.Contains(r, "addr=127.0.0.1:7743")
	}
	if !whole {
		t.Errorf("addr=127.0.0.1:7743 was split across rows: %q", rows)
	}
	if got := strings.Join(rows, " "); got != line {
		t.Errorf("rejoined %q, want %q", got, line)
	}

	long := strings.Repeat("x", 100)
	rows = wrapLogLine("a "+long, 40)
	for _, r := range rows {
		if ansi.StringWidth(r) > 40 {
			t.Errorf("an over-long word was not broken: %q", r)
		}
	}
	if got := strings.ReplaceAll(strings.Join(rows, ""), " ", ""); got != "a"+long {
		t.Errorf("the over-long word lost characters: %q", rows)
	}
}
