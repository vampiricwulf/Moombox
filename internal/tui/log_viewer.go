package tui

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const maxLogLines = 1000

// LogLevel represents a log filter level.
type LogLevel int

const (
	LogLevelAll LogLevel = iota
	LogLevelError
	LogLevelWarn
	LogLevelInfo
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
	lines        []string
	filtered     []string
	scrollOffset int
	autoScroll   bool
	width        int
	height       int
	focused      bool
	level        LogLevel
}

// NewLogViewerModel creates a new log viewer model.
func NewLogViewerModel() *LogViewerModel {
	return &LogViewerModel{
		autoScroll: true,
	}
}

// AddLine appends a single log line.
func (m *LogViewerModel) AddLine(line string) {
	m.lines = append(m.lines, line)
	if len(m.lines) > maxLogLines {
		m.lines = m.lines[len(m.lines)-maxLogLines:]
	}
	m.rebuildFiltered()
	if m.autoScroll {
		m.scrollToBottom()
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
		m.scrollToBottom()
	}
}

// SetSize updates the panel dimensions.
func (m *LogViewerModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	if m.autoScroll {
		m.scrollToBottom()
	}
}

// SetFocused sets the focus state.
func (m *LogViewerModel) SetFocused(f bool) {
	m.focused = f
}

// ScrollUp scrolls up and disables auto-scroll.
func (m *LogViewerModel) ScrollUp() {
	if m.scrollOffset > 0 {
		m.scrollOffset--
		m.autoScroll = false
	}
}

// ScrollDown scrolls down, re-enabling auto-scroll at bottom.
func (m *LogViewerModel) ScrollDown() {
	ms := m.maxScroll()
	if m.scrollOffset < ms {
		m.scrollOffset++
	}
	if m.scrollOffset >= ms {
		m.autoScroll = true
	}
}

// PageUp scrolls up by a page.
func (m *LogViewerModel) PageUp() {
	// Page by contentHeight (match TS: logHeight - 3 where logHeight is panel height)
	ch := m.contentHeight()
	m.scrollOffset -= ch
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
	m.autoScroll = false
}

// PageDown scrolls down by a page.
func (m *LogViewerModel) PageDown() {
	ch := m.contentHeight()
	m.scrollOffset += ch
	ms := m.maxScroll()
	if m.scrollOffset > ms {
		m.scrollOffset = ms
	}
	if m.scrollOffset >= ms {
		m.autoScroll = true
	}
}

// ReEnableAutoScroll re-enables auto-scroll (called when clicking away from logs panel).
func (m *LogViewerModel) ReEnableAutoScroll() {
	m.autoScroll = true
	m.scrollToBottom()
}

// CycleLevel cycles the log level filter.
func (m *LogViewerModel) CycleLevel() {
	m.level = m.level.Next()
	m.rebuildFiltered()
	// Unconditionally re-enable auto-scroll and jump to bottom (match TS)
	m.autoScroll = true
	m.scrollToBottom()
}

func (m *LogViewerModel) contentHeight() int {
	h := m.height - 3
	if h < 1 {
		h = 1
	}
	return h
}

func (m *LogViewerModel) maxScroll() int {
	ms := len(m.filtered) - m.contentHeight()
	if ms < 0 {
		return 0
	}
	return ms
}

func (m *LogViewerModel) scrollToBottom() {
	m.scrollOffset = m.maxScroll()
}

func (m *LogViewerModel) rebuildFiltered() {
	if m.level == LogLevelAll {
		m.filtered = append(m.filtered[:0], m.lines...)
		return
	}

	m.filtered = m.filtered[:0]
	for _, line := range m.lines {
		if m.matchLevel(line) {
			m.filtered = append(m.filtered, line)
		}
	}
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
	// The level token starts at position 20 (after "YYYY-MM-DD HH:MM:SS ")
	upper := strings.ToUpper(line)
	if strings.Contains(upper, " ERROR ") || strings.Contains(upper, "[ERROR]") {
		return "ERROR"
	}
	if strings.Contains(upper, " WARN ") || strings.Contains(upper, "[WARN]") || strings.Contains(upper, "[WARNING]") {
		return "WARN"
	}
	if strings.Contains(upper, " INFO ") || strings.Contains(upper, "[INFO]") {
		return "INFO"
	}
	if strings.Contains(upper, " DEBUG ") || strings.Contains(upper, "[DEBUG]") {
		return "DEBUG"
	}
	return "" // no level marker found
}

func logLineColor(line string) lipgloss.Color {
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
	contentW := m.width - 2
	if contentW < 1 {
		contentW = 1
	}

	// Title color: cyan when focused, white when not (match TS titleColor)
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
	if !m.focused {
		titleStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorWhite)
	}

	// Header: "Logs (150)" with count (L2)
	// Suffixes are only appended if they fit within contentW to prevent
	// header wrapping (which adds an extra line and causes vertical shifting).
	header := titleStyle.Render(fmt.Sprintf("Logs (%d)", len(m.filtered)))
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
	if ms := m.maxScroll(); ms > 0 {
		pct := int(math.Round(float64(m.scrollOffset) * 100 / float64(ms)))
		suffix := " " + DimStyle.Render(fmt.Sprintf("[%d%%]", pct))
		if lipgloss.Width(header)+lipgloss.Width(suffix) <= contentW {
			header += suffix
		}
	}

	contentH := m.contentHeight()
	var lines []string

	// Empty state message (match TS)
	if len(m.filtered) == 0 {
		lines = append(lines, DimStyle.Render("No logs yet."))
	}

	end := m.scrollOffset + contentH
	if end > len(m.filtered) {
		end = len(m.filtered)
	}

	for i := m.scrollOffset; i < end; i++ {
		line := m.filtered[i]
		color := logLineColor(line)
		rendered := lipgloss.NewStyle().Foreground(color).Render(
			truncateString(line, contentW),
		)
		lines = append(lines, rendered)
	}

	// Pad
	for len(lines) < contentH {
		lines = append(lines, strings.Repeat(" ", contentW))
	}

	content := header + "\n" + strings.Join(lines, "\n")

	style := UnfocusedBorder
	if m.focused {
		style = FocusedBorder
	}

	return style.Width(contentW).Height(m.height - 2).Render(content)
}

