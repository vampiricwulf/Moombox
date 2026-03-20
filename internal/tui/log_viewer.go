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
	lines      []string
	filtered   []string
	viewport   viewport.Model
	autoScroll bool
	width      int
	height     int
	focused    bool
	level      LogLevel

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
	vp.SoftWrap = true
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

// AddLine appends a single log line.
func (m *LogViewerModel) AddLine(line string) {
	m.lines = append(m.lines, line)
	if len(m.lines) > maxLogLines {
		m.lines = m.lines[len(m.lines)-maxLogLines:]
	}
	m.rebuildFiltered()
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}

// AddLines appends a batch of log lines efficiently (single rebuildFiltered call).
// Matches TS behavior where batch is concat'd and capped in one operation.
func (m *LogViewerModel) AddLines(batch []string) {
	m.lines = append(m.lines, batch...)
	if len(m.lines) > maxLogLines {
		m.lines = m.lines[len(m.lines)-maxLogLines:]
	}
	m.rebuildFiltered()
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}

// SetSize updates the panel dimensions.
func (m *LogViewerModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.SetWidth(w - 2) // account for borders
	m.resizeViewport()
	m.updateViewportContent()
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
		m.resizeViewport()
	}
}

// ScrollUp scrolls up by one line via the viewport.
func (m *LogViewerModel) ScrollUp() {
	m.viewport.ScrollUp(1)
	m.autoScroll = m.viewport.AtBottom()
}

// ScrollDown scrolls down by one line via the viewport.
func (m *LogViewerModel) ScrollDown() {
	m.viewport.ScrollDown(1)
	m.autoScroll = m.viewport.AtBottom()
}

// PageUp scrolls up by a page via the viewport.
func (m *LogViewerModel) PageUp() {
	m.viewport.HalfPageUp()
	m.autoScroll = m.viewport.AtBottom()
}

// PageDown scrolls down by a page via the viewport.
func (m *LogViewerModel) PageDown() {
	m.viewport.HalfPageDown()
	m.autoScroll = m.viewport.AtBottom()
}

// ReEnableAutoScroll re-enables auto-scroll (called when clicking away from logs panel).
func (m *LogViewerModel) ReEnableAutoScroll() {
	m.autoScroll = true
	m.viewport.GotoBottom()
}

// UpdateViewport delegates a tea.Msg to the viewport and syncs autoScroll state.
func (m *LogViewerModel) UpdateViewport(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	m.autoScroll = m.viewport.AtBottom()
	return cmd
}

// CycleLevel cycles the log level filter.
func (m *LogViewerModel) CycleLevel() {
	m.level = m.level.Next()
	m.autoScroll = true
	m.rebuildFiltered()
	m.viewport.GotoBottom()
}

func (m *LogViewerModel) rebuildFiltered() {
	if m.level == LogLevelAll {
		m.filtered = append(m.filtered[:0], m.lines...)
		m.updateViewportContent()
		return
	}

	m.filtered = m.filtered[:0]
	for _, line := range m.lines {
		if m.matchLevel(line) {
			m.filtered = append(m.filtered, line)
		}
	}
	m.updateViewportContent()
}

func (m *LogViewerModel) updateViewportContent() {
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
	return lipgloss.NewStyle().Foreground(logLineColor(m.filtered[idx]))
}

func (m *LogViewerModel) matchLevel(line string) bool {
	// Level threshold: show logs at or above the selected level
	// Lines without a level marker always pass (match TS: unmatched lines always shown)
	lineLevel := extractLogLevel(line)
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
			m.autoScroll = m.viewport.AtBottom()
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
			m.autoScroll = m.viewport.AtBottom()
			return nil, true
		case "N":
			m.viewport.HighlightPrevious()
			m.autoScroll = m.viewport.AtBottom()
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
	content := m.viewport.GetContent()
	matches := m.searchRegex.FindAllStringIndex(content, -1)
	m.matchCount = len(matches)
	if len(matches) > 0 {
		m.viewport.SetHighlights(matches)
	} else {
		m.viewport.ClearHighlights()
	}
}

// resizeViewport recalculates viewport height accounting for the search bar.
func (m *LogViewerModel) resizeViewport() {
	contentH := max(m.height-3, 1)
	if m.searching {
		contentH = max(contentH-1, 1) // search bar takes 1 line
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

func logLineColor(line string) color.Color {
	level := extractLogLevel(line)
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

// View renders the log viewer panel.
func (m *LogViewerModel) View() string {
	contentW := max(m.width-2, 1)

	// Title color: cyan when focused, white when not (match TS titleColor)
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
	if !m.focused {
		titleStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorWhite)
	}

	// Header: "Logs (150)" with count (L2)
	// Suffixes are only appended if they fit within contentW to prevent
	// header wrapping (which adds an extra line and causes vertical shifting).
	header := titleStyle.Render(fmt.Sprintf("Logs (%d)", len(m.filtered)))
	// Search query indicator (when search is active but not typing)
	if !m.searching && m.searchQuery != "" {
		queryDisplay := m.searchQuery
		if len(queryDisplay) > 20 {
			queryDisplay = queryDisplay[:20] + "..."
		}
		matchSuffix := fmt.Sprintf(" [/%s] (%d matches)", queryDisplay, m.matchCount)
		suffix := " " + lipgloss.NewStyle().Foreground(lipgloss.Color("#aaaa00")).Render(matchSuffix)
		if lipgloss.Width(header)+lipgloss.Width(suffix) <= contentW {
			header += suffix
		}
	}
	// Level filter suffix (L3)
	if m.level != LogLevelAll {
		suffix := " " + lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("["+m.level.String()+"+]")
		if lipgloss.Width(header)+lipgloss.Width(suffix) <= contentW {
			header += suffix
		}
	}
	// PAUSED indicator when not auto-scrolling and focused (L4)
	if !m.autoScroll && m.focused {
		suffix := " " + lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render("[PAUSED]")
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

	return style.Width(m.width).Height(m.height).Render(content)
}
