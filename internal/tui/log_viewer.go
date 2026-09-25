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
	// level filter. appendLine/capLines are the only writers of lines+levels,
	// so the two cannot fall out of step.
	levels         []string
	filteredLevels []string
	// wrapped[i] is lines[i] cut to wrapWidth, nil until it is first needed.
	// Parallel to lines and levels (appendLine, capLines and Clear are its
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

// capLines trims all three slices to maxLogLines, identically.
func (m *LogViewerModel) capLines() {
	if len(m.lines) <= maxLogLines {
		return
	}
	// slices.Clone prevents the re-slice from aliasing the old backing
	// array, which would otherwise retain MBs of string headers over the
	// 24/7 runtime target.
	m.lines = slices.Clone(m.lines[len(m.lines)-maxLogLines:])
	m.levels = slices.Clone(m.levels[len(m.levels)-maxLogLines:])
	// The surviving lines keep the rows they were already cut into — that
	// is the whole point of the cache, since this runs on every insertion
	// once the buffer is full.
	if len(m.wrapped) > maxLogLines {
		m.wrapped = slices.Clone(m.wrapped[len(m.wrapped)-maxLogLines:])
	}
}

// AddLine appends a single log line.
func (m *LogViewerModel) AddLine(line string) {
	m.appendLine(line)
	m.capLines()
	m.rebuildFiltered()
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}

// AddLines appends a batch of log lines efficiently (single rebuildFiltered call).
// Matches TS behavior where batch is concat'd and capped in one operation.
func (m *LogViewerModel) AddLines(batch []string) {
	for _, line := range batch {
		m.appendLine(line)
	}
	m.capLines()
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
	m.matchCount = 0
	m.viewport.ClearHighlights()
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

// wrapLogLine hard-wraps one plain-text log line to width columns, using the
// same character-wrap rule the viewport's own softWrap applies
// (ansi.Cut(line, idx, idx+width)) so nothing about the rendered result
// changes — only WHEN the work is done. Lines that already fit are returned
// as a one-element slice sharing the original string, and a width of 0 (the
// panel has not been sized yet) wraps nothing.
func wrapLogLine(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}
	total := ansi.StringWidth(line)
	if total <= width {
		return []string{line}
	}
	out := make([]string, 0, (total+width-1)/width)
	for idx := 0; idx < total; idx += width {
		out = append(out, ansi.Cut(line, idx, idx+width))
	}
	return out
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
		m.viewport.SetContent("No logs yet.")
		return
	}

	// Set plain text content — coloring is handled by StyleLineFunc.
	// This keeps the viewport content free of ANSI codes so that
	// SetHighlights byte offsets work correctly.
	// Clone to avoid shared backing array — SetContentLines stores the
	// slice reference, and m.filtered may be mutated by rebuildFiltered().
	m.viewport.SetContentLines(slices.Clone(m.filtered))

	// Re-apply search highlights if a query is active (SetContent clears them).
	if m.searchQuery != "" {
		m.applySearchHighlights()
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
				m.matchCount = 0
				m.viewport.ClearHighlights()
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
			m.matchCount = 0
			m.viewport.ClearHighlights()
			return nil, true
		case "n":
			m.viewport.HighlightNext()
			m.invalidate() // the selected-highlight index is bubbles-private
			m.setAutoScroll(m.viewport.AtBottom())
			return nil, true
		case "N":
			m.viewport.HighlightPrevious()
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
		m.matchCount = 0
		return
	}
	// The content is the buffer AFTER hard-wrapping, so a match that straddles
	// a wrap boundary is not found — the row break is a real "\n" in this
	// string. Accepted: searching the unwrapped source would need a second
	// offset mapping back onto display rows for SetHighlights, and the miss
	// only affects a query long enough to span the panel's own width.
	content := m.viewport.GetContent()
	matches := m.searchRegex.FindAllStringIndex(content, -1)
	m.matchCount = len(matches)
	if len(matches) > 0 {
		m.viewport.SetHighlights(matches)
	} else {
		m.viewport.ClearHighlights()
	}
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
	header := titleStyle.Render(fmt.Sprintf("Logs (%d)", m.rawCount))
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
