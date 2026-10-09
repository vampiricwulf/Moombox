package tui

import (
	"fmt"
	"image/color"
	"regexp"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const maxLogLines = 1000

// LogLevel represents a log filter level.
type LogLevel int

const (
	LogLevelAll LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

func (l LogLevel) String() string {
	switch l {
	case LogLevelError:
		return "ERROR"
	case LogLevelWarn:
		return "WARN"
	case LogLevelInfo:
		return "INFO"
	default:
		return "ALL"
	}
}

// Next cycles to the next log level filter.
func (l LogLevel) Next() LogLevel {
	return (l + 1) % 4
}

// LogViewerModel manages the log panel.
type LogViewerModel struct {
	lines    []string
	filtered []string
	// levels[i] is extractLogLevel(lines[i]) and filteredLevels[i] is the
	// level of filtered[i]. A line's level cannot change, so it is parsed
	// exactly once — when the line arrives — instead of once per visible line
	// per frame in styleLogLine (the viewport's StyleLineFunc, which runs for
	// every visible row on every render) and once per line per rebuild in the
	// level filter. appendLine/dropOldest are the only writers of
	// lines+levels, so the two cannot fall out of step.
	levels         []string
	filteredLevels []string
	// wrapped[i] is lines[i] cut to wrapWidth, nil until it is first needed.
	// Parallel to lines and levels (appendLine, dropOldest and Clear are its
	// only writers), so a source line is cut EXACTLY ONCE in its life instead
	// of once per rebuild. At the 1,000-line cap capLines trims on every
	// insertion, so a 24/7 process re-cut every wrapping line ~10 times a
	// second — 18 ms and 1.25 MB of garbage per flush at a 60-column panel,
	// and structured log lines exceed 118 columns routinely, so a 120-column
	// terminal pays the same shape. rebuildFiltered drops the whole slice
	// when wrapWidth moves; the rows are immutable and are shared with
	// filtered rather than copied.
	wrapped [][]string
	// rawCount is how many SOURCE log lines survive the level filter. The
	// header reports this, not len(filtered) — filtered holds WRAPPED
	// display lines, so a single long line would otherwise read as ten.
	rawCount int
	// wrapWidth is the content width filtered was wrapped to. SetSize
	// re-wraps when it moves (Tab changes every panel's share).
	wrapWidth  int
	viewport   viewport.Model
	autoScroll bool
	width      int
	height     int
	focused    bool
	level      LogLevel

	// title heads the panel ("Logs" when empty) and emptyText is what an
	// empty buffer says ("No logs yet." when empty). The O L overlay is the
	// one viewer that sets them: it shows ONE job's log, and says so.
	title     string
	emptyText string

	// renderCache / cacheKey memoise View(). bubbletea calls View() after
	// EVERY message (~180/s with one active download at the defaults: 60
	// progress updates plus the 120 Hz tick), and the vast majority of those
	// carry no log change at all. The key is every input View() reads; the cache is
	// dropped outright while the search box is open, because the textinput
	// renders a blinking cursor whose state is not in the key.
	renderCache string
	cacheKey    logViewerKey
	// contentSeq counts content changes. updateViewportContent is the single
	// funnel for filtered/filteredLevels, so bumping it there means no
	// mutator can change what is displayed without moving the cache key.
	contentSeq uint64

	// Search state
	searching   bool            // true when search input is visible
	searchInput textinput.Model // the search text input
	searchQuery string          // current active search query (empty = no highlights)
	searchRegex *regexp.Regexp  // compiled search pattern (cached, recompiled only on query change)
	matchCount  int             // number of matches for the current query
	// matches is the byte ranges applySearchHighlights last handed the
	// viewport, and matchRows[i] the display row matches[i] starts on — the
	// row bubbles files it under (one per "\n" before it). searchStep needs
	// both: the viewport keeps its selected match private, so whether any
	// match is on screen has to be worked out here, and re-anchoring the
	// selection on the view means handing the same ranges back.
	matches   [][]int
	matchRows []int
}

// NewLogViewerModel creates a new log viewer model.
func NewLogViewerModel() *LogViewerModel {
	vp := viewport.New(viewport.WithWidth(0), viewport.WithHeight(1))
	// Use helpViewportKeyMap to prevent letter keys (j/k/d/u/f/b) from
	// conflicting with app chord bindings. Mouse scroll is handled
	// explicitly in app.go handleMouse.
	vp.KeyMap = helpViewportKeyMap()
	// SoftWrap = false is load-bearing for the frame budget. Under SoftWrap
	// the viewport's calculateLine walks EVERY buffered line calling
	// ansi.StringWidth on each, on every render, and visibleLines /
	// maxYOffset / ScrollPercent each call it — 2.35 ms and 6,192 allocs at
	// the 1,000-line cap, 60% of a whole TUI frame (CORE-2). With it off the
	// same function is a length and a min(). Lines are hard-wrapped once at
	// insertion instead (rebuildFiltered / wrapLogLine), which moves that
	// width pass to the ~10/s insertion path where SetContentLines already
	// pays an O(n) maxLineWidth pass.
	vp.SoftWrap = false
	vp.FillHeight = true

	// Highlight styles for search matches
	vp.HighlightStyle = lipgloss.NewStyle().
		Background(lipgloss.Color("#555500")).
		Foreground(lipgloss.Color("#ffffff"))
	vp.SelectedHighlightStyle = lipgloss.NewStyle().
		Background(lipgloss.Color("#aaaa00")).
		Foreground(lipgloss.Color("#000000")).
		Bold(true)

	ti := newTextInput()
	ti.Prompt = "/"
	ti.Placeholder = ""
	ti.CharLimit = 200

	m := &LogViewerModel{
		autoScroll:  true,
		viewport:    vp,
		searchInput: ti,
	}

	// Use StyleLineFunc instead of pre-rendered ANSI to keep viewport
	// content as plain text. This allows SetHighlights to work correctly
	// (its byte-offset parser operates on ANSI-stripped content).
	m.viewport.StyleLineFunc = m.styleLogLine

	return m
}

// appendLine records one line, its parsed level and an empty wrap slot. The
// only writer of the three slices, together with capLines.
func (m *LogViewerModel) appendLine(line string) {
	m.lines = append(m.lines, line)
	m.levels = append(m.levels, extractLogLevel(line))
	m.wrapped = append(m.wrapped, nil)
}

// capLines trims all three slices to maxLogLines, identically, and returns
// how many display rows left the top of the view with them (see dropOldest).
func (m *LogViewerModel) capLines() int {
	return m.dropOldest(len(m.lines) - maxLogLines)
}

// dropOldest removes the n oldest lines from all three slices, identically,
// and returns how many DISPLAY rows they took with them — the rows the
// viewport was showing above everything that survives. redisplay needs that
// count to keep a paused view where it is.
//
// A dropped line counts only if it is on screen: it passes the level filter
// (a hidden line keeps the rows it was cut into before F hid it), and it has
// been cut — wrapped[i] is still nil for a line that arrived in the same
// batch and never reached the display, and contributes nothing. Its rows are
// counted the way the viewport counts them: SetContentLines splits a row at
// an embedded "\n", and wrapLogLine passes a line that already fits through
// uncut, so a multi-line entry (an ffmpeg stderr tail) moves the offset by
// every row it occupied, not by one.
func (m *LogViewerModel) dropOldest(n int) int {
	if n <= 0 {
		return 0
	}
	n = min(n, len(m.lines))
	rows := 0
	for i := range min(n, len(m.wrapped)) {
		if m.level != LogLevelAll && !m.matchLevel(m.levels[i]) {
			continue
		}
		for _, r := range m.wrapped[i] {
			rows += 1 + strings.Count(r, "\n")
		}
	}
	// slices.Clone prevents the re-slice from aliasing the old backing
	// array, which would otherwise retain MBs of string headers over the
	// 24/7 runtime target.
	m.lines = slices.Clone(m.lines[n:])
	m.levels = slices.Clone(m.levels[n:])
	// The surviving lines keep the rows they were already cut into — that
	// is the whole point of the cache, since this runs on every insertion
	// once the buffer is full.
	if len(m.wrapped) >= n {
		m.wrapped = slices.Clone(m.wrapped[n:])
	}
	return rows
}

// AddLine appends a single log line.
func (m *LogViewerModel) AddLine(line string) {
	m.appendLine(line)
	m.redisplay(m.capLines())
}

// AddLines appends a batch of log lines efficiently (single rebuildFiltered call).
// Matches TS behavior where batch is concat'd and capped in one operation.
func (m *LogViewerModel) AddLines(batch []string) {
	for _, line := range batch {
		m.appendLine(line)
	}
	m.redisplay(m.capLines())
}

// SyncLines brings the buffer in line with snapshot, a fresh read of a ring
// this viewer mirrors rather than owns — the O L overlay's per-job log
// (db.GetJobLogs). It applies only the difference, as the ring itself moved:
// the lines evicted from the front go through dropOldest and the new ones are
// appended, so a paused view stays on its lines across a read exactly as the
// log panel's does across an insertion (W24-12). Reports whether anything
// changed; an identical read leaves the display, and its render cache, alone.
func (m *LogViewerModel) SyncLines(snapshot []string) bool {
	drop, tail := ringDelta(m.lines, snapshot)
	if drop == 0 && len(tail) == 0 {
		return false
	}
	trimmed := m.dropOldest(drop)
	for _, line := range tail {
		m.appendLine(line)
	}
	m.redisplay(trimmed + m.capLines())
	return true
}

// ringDelta works out how a ring buffer moved between two reads of it: drop
// lines left the front of prev, and tail was appended after what survived.
// The per-job log buffer only ever appends and trims its front (capLogLines,
// internal/database), so cur is prev[drop:] followed by tail for the smallest
// drop that lines up. Taking the SMALLEST is what keeps a run of identical
// lines (one message repeated within the same second) from reading as an
// eviction. A cur that matches no suffix of prev — the buffer was cleared,
// or replaced outright — comes back as every line of prev dropped and every
// line of cur new, which is a full replace.
func ringDelta(prev, cur []string) (drop int, tail []string) {
	for drop = 0; drop < len(prev); drop++ {
		kept := prev[drop:]
		if len(kept) <= len(cur) && slices.Equal(kept, cur[:len(kept)]) {
			return drop, cur[len(kept):]
		}
	}
	return len(prev), cur
}

// redisplay rebuilds the display after lines were added and trimmedRows
// display rows were dropped off the front of the buffer. Following, the view
// sticks to the bottom. Paused, it stays on the lines it is showing: the
// viewport keeps its YOffset across SetContentLines (it only clamps), so
// once the buffer is full and every insertion trims the oldest line, the same
// offset over the shortened buffer pointed at later lines — the "Auto-scroll
// paused" view crept up a row per new line (more for wrapped ones), and at
// DEBUG or with several jobs running it could not be read (W24-12). The
// offset moves up by exactly the rows that left above it, clamped at the top
// once the lines on screen are themselves the ones evicted.
//
// Moved BEFORE the rebuild, on the old content: the rebuild re-applies an
// active search's highlights, and SetHighlights picks the selected match from
// the offset it finds. The new offset is never past the new bottom — the
// buffer lost trimmedRows rows and gained at least none.
func (m *LogViewerModel) redisplay(trimmedRows int) {
	if !m.autoScroll && trimmedRows > 0 {
		m.viewport.SetYOffset(m.viewport.YOffset() - trimmedRows)
	}
	m.rebuildFiltered()
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}

// Clear empties the view: history, filtered lines, and any search. The
// level filter is kept — it is a preference, not content.
func (m *LogViewerModel) Clear() {
	m.lines = nil
	m.levels = nil
	m.wrapped = nil
	m.rawCount = 0
	m.searching = false
	m.searchInput.SetValue("")
	m.searchQuery = ""
	m.searchRegex = nil
	m.clearSearchMatches()
	m.rebuildFiltered()
	// setAutoScroll (not a direct field assignment) so the viewport height
	// is recalculated when this un-pauses — it owns the pause-hint row (see
	// resizeViewport), and skipping that leaves a stale short viewport if
	// the log was paused (scrolled up) when cleared.
	m.setAutoScroll(true)
	m.viewport.GotoTop()
}

// SetSize updates the panel dimensions.
func (m *LogViewerModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.SetWidth(w - 2) // account for borders
	m.resizeViewport()
	// A width change invalidates the WRAP, not just the layout: filtered
	// holds lines already cut to the old content width, so a re-render alone
	// would leave them short (or overflowing, after a widen). Tab gives each
	// panel a different share of the terminal, so this runs on every press.
	if m.wrapWidth != m.contentWidth() {
		m.rebuildFiltered()
	} else {
		m.updateViewportContent()
	}
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}

// SetFocused sets the focus state.
func (m *LogViewerModel) SetFocused(f bool) {
	m.focused = f
	// Cancel search input when losing focus (keep existing results)
	if !f && m.searching {
		m.searching = false
		m.searchInput.SetValue("")
	}
	// Focus affects whether the pause hint is rendered — resize always.
	m.resizeViewport()
}

// setAutoScroll updates the auto-scroll flag, resizing the viewport when it
// flips — the pause hint occupies a content row while paused and focused.
func (m *LogViewerModel) setAutoScroll(v bool) {
	if m.autoScroll == v {
		return
	}
	m.autoScroll = v
	m.resizeViewport()
}

// ScrollUp scrolls up by one line via the viewport.
func (m *LogViewerModel) ScrollUp() {
	m.viewport.ScrollUp(1)
	m.setAutoScroll(m.viewport.AtBottom())
}

// ScrollDown scrolls down by one line via the viewport.
func (m *LogViewerModel) ScrollDown() {
	m.viewport.ScrollDown(1)
	m.setAutoScroll(m.viewport.AtBottom())
}

// PageUp scrolls up by a page via the viewport.
func (m *LogViewerModel) PageUp() {
	m.viewport.HalfPageUp()
	m.setAutoScroll(m.viewport.AtBottom())
}

// PageDown scrolls down by a page via the viewport.
func (m *LogViewerModel) PageDown() {
	m.viewport.HalfPageDown()
	m.setAutoScroll(m.viewport.AtBottom())
}

// ReEnableAutoScroll re-enables auto-scroll (called when clicking away from logs panel).
func (m *LogViewerModel) ReEnableAutoScroll() {
	m.setAutoScroll(true)
	m.viewport.GotoBottom()
}

// UpdateViewport delegates a tea.Msg to the viewport and syncs autoScroll state.
func (m *LogViewerModel) UpdateViewport(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	m.setAutoScroll(m.viewport.AtBottom())
	return cmd
}

// CycleLevel cycles the log level filter.
func (m *LogViewerModel) CycleLevel() {
	m.level = m.level.Next()
	m.setAutoScroll(true)
	m.rebuildFiltered()
	m.viewport.GotoBottom()
}

// wrapLogLine wraps one plain-text log line to width columns at its spaces,
// breaking only a word longer than a whole row. It used to cut at exact
// columns — the viewport's own softWrap rule — which split the very tokens a
// log line is read for: "addr=127.0.0." on one row and "1:7743" on the next,
// a URL or a video ID across two. Lines that already fit are returned as a
// one-element slice sharing the original string, and a width of 0 (the panel
// has not been sized yet) wraps nothing.
func wrapLogLine(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}
	if ansi.StringWidth(line) <= width {
		return []string{line}
	}
	return strings.Split(ansi.Wrap(line, width, ""), "\n")
}

// rebuildFiltered rebuilds the DISPLAY buffer: level-filter the raw lines,
// then hard-wrap each survivor to the panel's content width. filtered and
// filteredLevels hold WRAPPED lines (a source line contributes one entry per
// display row, repeating its level so styleLogLine still colours
// continuations); rawCount holds the SOURCE count the header reports.
func (m *LogViewerModel) rebuildFiltered() {
	m.filtered = m.filtered[:0]
	m.filteredLevels = m.filteredLevels[:0]
	m.rawCount = 0
	w := m.contentWidth()
	// The cache holds rows cut to the OLD wrapWidth, so a width change (Tab
	// gives every panel a different share of the terminal) drops all of it.
	// Done here, at the one place wrapWidth is assigned, so no caller can
	// leave rows cut to a width that is no longer the panel's — and a length
	// that somehow fell out of step with lines self-heals in the same test.
	if w != m.wrapWidth || len(m.wrapped) != len(m.lines) {
		m.wrapped = make([][]string, len(m.lines))
	}
	m.wrapWidth = w
	for i, line := range m.lines {
		if m.level != LogLevelAll && !m.matchLevel(m.levels[i]) {
			continue
		}
		m.rawCount++
		// Cut once per SOURCE line, not once per rebuild: at the cap
		// capLines trims on every insertion, so before this every wrapping
		// line was re-cut ~10 times a second for the life of the process.
		// wrapLogLine returns the line itself as a one-element slice when it
		// already fits (and when the panel is unsized), so this one branch
		// covers every case and nil stays an unambiguous "not cut yet".
		if m.wrapped[i] == nil {
			m.wrapped[i] = wrapLogLine(line, w)
		}
		m.filtered = append(m.filtered, m.wrapped[i]...)
		for range m.wrapped[i] {
			m.filteredLevels = append(m.filteredLevels, m.levels[i])
		}
	}
	m.updateViewportContent()
}

// contentWidth is the viewport's usable width — the same value SetSize hands
// the viewport, and therefore the width wrapLogLine must target. Zero until
// the panel is sized: BackfillLogs seeds the ring buffer before the first
// WindowSizeMsg, and wrapping those lines to the max(…, 1) floor would cut
// the whole backlog into one-column rows for one frame.
func (m *LogViewerModel) contentWidth() int {
	if m.width <= 0 {
		return 0
	}
	return max(m.width-2, 1)
}

func (m *LogViewerModel) updateViewportContent() {
	// One funnel for every content change — the render cache keys on this.
	m.contentSeq++
	if len(m.filtered) == 0 {
		// A placeholder is not searched: an active query matches nothing in
		// it, and the ranges found in the content it replaced would point
		// past its end (searchStep hands them back to the viewport).
		m.clearSearchMatches()
		// Lines exist but the level filter hides them all: say so, rather
		// than "No logs yet." under a header reading "Logs (0) [WARN+]".
		if len(m.lines) > 0 && m.level != LogLevelAll {
			m.viewport.SetContent("No " + m.level.String() + "+ lines. F cycles the level.")
			return
		}
		empty := m.emptyText
		if empty == "" {
			empty = "No logs yet."
		}
		m.viewport.SetContent(empty)
		return
	}

	// Set plain text content — coloring is handled by StyleLineFunc.
	// This keeps the viewport content free of ANSI codes so that
	// SetHighlights byte offsets work correctly.
	// Clone to avoid shared backing array — SetContentLines stores the
	// slice reference, and m.filtered may be mutated by rebuildFiltered().
	m.viewport.SetContentLines(slices.Clone(m.filtered))

	// Re-apply search highlights if a query is active (SetContent clears them).
	// SetHighlights also SCROLLS: it selects the first match at or below the
	// top row and, when that match is off screen, moves the view to it. That
	// is the jump Enter wants (it calls applySearchHighlights directly), but
	// here the content merely changed, and a reader paused above the latest
	// lines was carried down to the next match on every new line. The offset
	// is the reader's, so it is put back — which leaves the viewport's
	// selection on that unseen match below, and searchStep is what keeps n
	// from stepping past it.
	if m.searchQuery != "" {
		top := m.viewport.YOffset()
		m.applySearchHighlights()
		m.viewport.SetYOffset(top)
	}
}

// styleLogLine returns the lipgloss style for a given viewport line index.
// Used as viewport.StyleLineFunc to color log lines without embedding ANSI
// in the content (which would break SetHighlights byte offset parsing).
func (m *LogViewerModel) styleLogLine(idx int) lipgloss.Style {
	if idx < 0 || idx >= len(m.filtered) {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(logLevelColor(m.filteredLevels[idx]))
}

// matchLevel reports whether a line at the given (already parsed) level passes
// the threshold. Lines without a level marker always pass (match TS:
// unmatched lines always shown).
func (m *LogViewerModel) matchLevel(lineLevel string) bool {
	if lineLevel == "" {
		return true // no level marker → always show
	}
	switch m.level {
	case LogLevelError:
		return lineLevel == "ERROR"
	case LogLevelWarn:
		return lineLevel == "ERROR" || lineLevel == "WARN"
	case LogLevelInfo:
		return lineLevel == "ERROR" || lineLevel == "WARN" || lineLevel == "INFO"
	default:
		return true
	}
}

func extractLogLevel(line string) string {
	// Log format: "2006-01-02 15:04:05 LEVEL msg..."
	// The level token is the third space-delimited field (index 2).
	// Try positional parsing first, fall back to substring matching for non-standard lines.
	if fields := strings.SplitN(line, " ", 4); len(fields) >= 3 {
		switch strings.ToUpper(fields[2]) {
		case "ERROR":
			return "ERROR"
		case "WARN", "WARNING":
			return "WARN"
		case "INFO":
			return "INFO"
		case "DEBUG":
			return "DEBUG"
		}
	}

	// Fallback: substring matching for non-standard log formats (e.g. bracketed levels)
	upper := strings.ToUpper(line)
	if strings.Contains(upper, "[ERROR]") {
		return "ERROR"
	}
	if strings.Contains(upper, "[WARN]") || strings.Contains(upper, "[WARNING]") {
		return "WARN"
	}
	if strings.Contains(upper, "[INFO]") {
		return "INFO"
	}
	if strings.Contains(upper, "[DEBUG]") {
		return "DEBUG"
	}
	return "" // no level marker found
}

// IsSearching returns true when the search input is visible.
func (m *LogViewerModel) IsSearching() bool {
	return m.searching
}

// HandleSearchKey processes a key press during search mode or with active
// search results. Returns a tea.Cmd if the textinput produced one.
// The second return value indicates whether the key was consumed.
func (m *LogViewerModel) HandleSearchKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	key := msg.String()

	// When the search input is visible (typing mode)
	if m.searching {
		switch key {
		case keyCtrlC:
			// Let Ctrl+C pass through to the app for quit handling
			return nil, false

		case keyEnter:
			query := m.searchInput.Value()
			m.searching = false
			if query == "" {
				// Empty query — clear search
				m.searchQuery = ""
				m.searchRegex = nil
				m.clearSearchMatches()
				m.resizeViewport()
				return nil, true
			}
			m.searchQuery = query
			m.searchRegex, _ = regexp.Compile("(?i)" + regexp.QuoteMeta(query))
			m.applySearchHighlights()
			m.resizeViewport()
			// Jump to first match
			m.viewport.HighlightNext()
			m.invalidate() // the selected-highlight index is bubbles-private
			m.setAutoScroll(m.viewport.AtBottom())
			return nil, true

		case keyEsc:
			// Cancel search input without clearing existing results
			m.searching = false
			m.searchInput.SetValue("")
			m.resizeViewport()
			return nil, true

		default:
			// Consume the key so it doesn't reach the chord system.
			// The textinput is updated via UpdateSearchInput in routeComponentMsg.
			return nil, true
		}
	}

	// When search results are active (not typing)
	if m.searchQuery != "" {
		// n/N for next/previous — intercept before chord normalization
		// to distinguish lowercase n from uppercase N.
		switch key {
		case keyEsc:
			m.searchQuery = ""
			m.searchRegex = nil
			m.clearSearchMatches()
			return nil, true
		case "n":
			m.searchStep(true)
			m.invalidate() // the selected-highlight index is bubbles-private
			m.setAutoScroll(m.viewport.AtBottom())
			return nil, true
		case "N":
			m.searchStep(false)
			m.invalidate() // the selected-highlight index is bubbles-private
			m.setAutoScroll(m.viewport.AtBottom())
			return nil, true
		}
	}

	return nil, false
}

// StartSearch activates the search input. Returns a tea.Cmd for the textinput focus.
func (m *LogViewerModel) StartSearch() tea.Cmd {
	m.searching = true
	m.searchInput.SetValue("")
	m.resizeViewport()
	return m.searchInput.Focus()
}

// applySearchHighlights runs the search regex against the viewport content
// and sets highlight ranges.
func (m *LogViewerModel) applySearchHighlights() {
	if m.searchRegex == nil {
		m.clearSearchMatches()
		return
	}
	// The content is the buffer AFTER hard-wrapping, so a match that straddles
	// a wrap boundary is not found — the row break is a real "\n" in this
	// string. Accepted: searching the unwrapped source would need a second
	// offset mapping back onto display rows for SetHighlights, and the miss
	// only affects a query long enough to span the panel's own width.
	content := m.viewport.GetContent()
	matches := m.searchRegex.FindAllStringIndex(content, -1)
	if len(matches) == 0 {
		m.clearSearchMatches()
		return
	}
	m.matchCount = len(matches)
	m.matches = matches
	m.matchRows = m.matchRows[:0]
	row, from := 0, 0
	for _, mt := range matches {
		row += strings.Count(content[from:mt[0]], "\n")
		from = mt[0]
		m.matchRows = append(m.matchRows, row)
	}
	m.viewport.SetHighlights(matches)
}

// clearSearchMatches forgets the matches of a search that no longer applies
// (Esc, an empty Enter, Clear) or of content that cannot be searched (the
// empty and filtered-empty placeholders).
func (m *LogViewerModel) clearSearchMatches() {
	m.matchCount = 0
	m.matches = nil
	m.matchRows = m.matchRows[:0]
	m.viewport.ClearHighlights()
}

// searchStep is n (forward) and N: select the next or previous match and
// bring it on screen.
//
// The viewport's own HighlightNext/HighlightPrevious step from the match it
// has selected, and it re-selects on every scroll and every SetHighlights:
// the first match at or below the top row, ON SCREEN OR NOT. A reader who
// scrolled to a stretch with no match on screen — with ↓/PgDn, or simply
// paused there while a new line re-applied the highlights (the rebuild puts
// the reader's offset back, W24-12) — therefore had the first match BELOW
// the view selected without ever seeing it, and n stepped past it: from
// line 20 with matches at 10, 50, 60 and 90, n went to 60, and with only 10
// and 50 it wrapped up to 10, skipping the one match below. N, with nothing
// below the view, stepped from "none selected" to the second-to-last match
// and skipped the last one above.
//
// So the selection is used only while a match is on screen — the one bubbles
// highlights as selected is then one the reader can see. With none on screen
// n goes to the first match below the view (wrapping to the first of all)
// and N to the last match above it (wrapping to the last of all), whatever
// the viewport had selected.
func (m *LogViewerModel) searchStep(forward bool) {
	if len(m.matchRows) == 0 {
		return
	}
	top := m.viewport.YOffset()
	// below is the index of the first match at or below the top row — the
	// one SetHighlights selects at this offset — or len when there is none.
	below, _ := slices.BinarySearch(m.matchRows, top)
	onScreen := below < len(m.matchRows) && m.matchRows[below] < top+m.viewport.Height()
	if onScreen {
		if forward {
			m.viewport.HighlightNext()
		} else {
			m.viewport.HighlightPrevious()
		}
		return
	}
	// Re-anchor: SetHighlights selects the first match below the view and
	// scrolls to it, which is n's answer outright. With nothing below it
	// selects none, and stepping from none reaches the first match (n's wrap)
	// and, back from there, the last (N's).
	m.viewport.SetHighlights(m.matches)
	if forward {
		if below == len(m.matchRows) {
			m.viewport.HighlightNext()
		}
		return
	}
	m.viewport.SetYOffset(top) // N scrolls from the reader's place, not the match below it
	if below == len(m.matchRows) {
		m.viewport.HighlightNext()
		m.viewport.SetYOffset(top)
	}
	m.viewport.HighlightPrevious()
}

// resizeViewport recalculates viewport height accounting for the search bar
// and the auto-scroll pause hint.
func (m *LogViewerModel) resizeViewport() {
	contentH := max(m.height-3, 1)
	if m.searching {
		contentH = max(contentH-1, 1) // search bar takes 1 line
	}
	if m.focused && !m.autoScroll {
		contentH = max(contentH-1, 1) // pause hint takes 1 line (see View)
	}
	m.viewport.SetHeight(contentH)
}

// UpdateSearchInput delegates a tea.Msg to the search textinput when searching.
func (m *LogViewerModel) UpdateSearchInput(msg tea.Msg) tea.Cmd {
	if !m.searching {
		return nil
	}
	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	return cmd
}

func logLevelColor(level string) color.Color {
	switch level {
	case "ERROR":
		return ColorLogError
	case "WARN":
		return ColorLogWarn
	case "DEBUG":
		return ColorLogDebug
	default:
		return ColorLogInfo // includes empty (no level marker) and INFO
	}
}

// logViewerKey is every input LogViewerModel.View() reads. It is a
// comparable struct so a cache hit is one ==. Content changes are covered by
// contentSeq, which updateViewportContent (the single funnel for
// filtered/filteredLevels) bumps; everything else here is read straight off
// the model or the viewport, so no mutator can forget to invalidate it.
//
// `searching` is deliberately ABSENT rather than forgotten: while the search
// box is open View() neither reads nor writes the cache at all (the textinput
// draws a blinking cursor whose state is not keyable), so a field for it
// would key a lookup that never happens.
type logViewerKey struct {
	contentSeq  uint64
	width       int
	height      int
	focused     bool
	autoScroll  bool
	level       LogLevel
	searchQuery string
	matchCount  int
	rawCount    int
	displayed   int
	yOffset     int
	vpHeight    int
}

func (m *LogViewerModel) key() logViewerKey {
	return logViewerKey{
		contentSeq:  m.contentSeq,
		width:       m.width,
		height:      m.height,
		focused:     m.focused,
		autoScroll:  m.autoScroll,
		level:       m.level,
		searchQuery: m.searchQuery,
		matchCount:  m.matchCount,
		rawCount:    m.rawCount,
		displayed:   len(m.filtered),
		yOffset:     m.viewport.YOffset(),
		vpHeight:    m.viewport.Height(),
	}
}

// invalidate drops the memoised frame. Called where the rendered output
// changes without any keyed field moving — the only such case is the
// viewport's own highlight cursor (HighlightNext/HighlightPrevious), which
// bubbles keeps private.
func (m *LogViewerModel) invalidate() {
	m.renderCache = ""
}

// View renders the log viewer panel.
func (m *LogViewerModel) View() string {
	// The search box renders a blinking textinput cursor whose state is not
	// in the key, so while it is open the panel is rendered every frame.
	if !m.searching {
		if k := m.key(); m.renderCache != "" && k == m.cacheKey {
			return m.renderCache
		}
	}
	contentW := max(m.width-2, 1)

	// Title color: cyan when focused, white when not (match TS titleColor)
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
	if !m.focused {
		titleStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorWhite)
	}

	// Header: "Logs (150)" with count (L2)
	// Suffixes are only appended if they fit within contentW to prevent
	// header wrapping (which adds an extra line and causes vertical shifting).
	// rawCount, not len(m.filtered): filtered holds wrapped DISPLAY lines, so
	// one long line would otherwise be counted as several.
	//
	// The title is cut, never the count, when the two do not fit: the O L
	// overlay's names a job, and a long one would wrap the header.
	title := "Logs"
	count := fmt.Sprintf(" (%d)", m.rawCount)
	if m.title != "" {
		title = truncateString(m.title, max(contentW-lipgloss.Width(count), 1))
	}
	header := titleStyle.Render(title + count)
	// Search query indicator (when search is active but not typing).
	// truncateString is rune/width-aware — byte-slicing would split
	// multi-byte runes in the user's query.
	if !m.searching && m.searchQuery != "" {
		queryDisplay := truncateString(m.searchQuery, 20)
		matchSuffix := fmt.Sprintf(" [/%s] (%d matches)", queryDisplay, m.matchCount)
		suffix := " " + lipgloss.NewStyle().Foreground(lipgloss.Color("#aaaa00")).Render(matchSuffix)
		if lipgloss.Width(header)+lipgloss.Width(suffix) <= contentW {
			header += suffix
		}
	}
	// Level filter suffix (L3)
	if m.level != LogLevelAll {
		suffix := " " + YellowStyle.Render("["+m.level.String()+"+]")
		if lipgloss.Width(header)+lipgloss.Width(suffix) <= contentW {
			header += suffix
		}
	}
	// PAUSED indicator when not auto-scrolling and focused (L4)
	if !m.autoScroll && m.focused {
		suffix := " " + YellowStyle.Render("[PAUSED]")
		if lipgloss.Width(header)+lipgloss.Width(suffix) <= contentW {
			header += suffix
		}
	}
	// Scroll percentage with brackets (L1 - match TS format [XX%])
	if len(m.filtered) > m.viewport.Height() {
		pct := int(m.viewport.ScrollPercent() * 100)
		suffix := " " + DimStyle.Render(fmt.Sprintf("[%d%%]", pct))
		if lipgloss.Width(header)+lipgloss.Width(suffix) <= contentW {
			header += suffix
		}
	}

	content := header + "\n"

	// Search bar (when actively typing)
	if m.searching {
		m.searchInput.SetWidth(contentW - 1) // -1 for "/" prompt
		content += m.searchInput.View() + "\n"
	}

	content += m.viewport.View()

	if m.focused && !m.autoScroll {
		pauseHint := DimStyle.Render("↓ Auto-scroll paused (End to resume)")
		content += "\n" + pauseHint
	}

	style := UnfocusedBorder
	if m.focused {
		style = FocusedBorder
	}

	out := style.Width(m.width).Height(m.height).Render(content)
	if !m.searching {
		m.renderCache = out
		m.cacheKey = m.key()
	} else {
		m.renderCache = ""
	}
	return out
}
