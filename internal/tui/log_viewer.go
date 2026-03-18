package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
}

// NewLogViewerModel creates a new log viewer model.
func NewLogViewerModel() *LogViewerModel {
	vp := viewport.New(0, 1)
	// Use helpViewportKeyMap to prevent letter keys (j/k/d/u/f/b) from
	// conflicting with app chord bindings. Mouse scroll is handled
	// explicitly in app.go handleMouse.
	vp.KeyMap = helpViewportKeyMap()
	return &LogViewerModel{
		autoScroll: true,
		viewport:   vp,
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
	contentH := max(h-3, 1)
	m.viewport.Width = w - 2 // account for borders
	m.viewport.Height = contentH
	m.updateViewportContent()
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}

// SetFocused sets the focus state.
func (m *LogViewerModel) SetFocused(f bool) {
	m.focused = f
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
	contentW := max(m.width-2, 1)

	if len(m.filtered) == 0 {
		m.viewport.SetContent(DimStyle.Render("No logs yet."))
		return
	}

	var rendered []string
	for _, line := range m.filtered {
		display := truncateString(line, contentW)
		color := logLineColor(line)
		rendered = append(rendered, lipgloss.NewStyle().Foreground(color).Render(display))
	}
	m.viewport.SetContent(strings.Join(rendered, "\n"))
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
	if len(m.filtered) > m.viewport.Height {
		pct := int(m.viewport.ScrollPercent() * 100)
		suffix := " " + DimStyle.Render(fmt.Sprintf("[%d%%]", pct))
		if lipgloss.Width(header)+lipgloss.Width(suffix) <= contentW {
			header += suffix
		}
	}

	content := header + "\n" + m.viewport.View()
	if m.focused && !m.autoScroll {
		pauseHint := DimStyle.Render("↓ Auto-scroll paused (End to resume)")
		content += "\n" + pauseHint
	}

	style := UnfocusedBorder
	if m.focused {
		style = FocusedBorder
	}

	return style.Width(contentW).Height(m.height - 2).Render(content)
}
