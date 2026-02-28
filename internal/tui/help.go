package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type helpSection struct {
	title string
	keys  []helpKey
}

type helpKey struct {
	key  string
	desc string
}

var globalKeys = helpSection{
	title: "Global",
	keys: []helpKey{
		{"Tab", "Cycle focus: Tasks → Details → Logs"},
		{"`", "Open settings panel"},
		{"W", "Open web dashboard in browser"},
		{"?", "Toggle this help overlay"},
		{"Q / Ctrl+C", "Quit"},
	},
}

var taskKeys = helpSection{
	title: "Tasks Panel",
	keys: []helpKey{
		{"↑/↓", "Select previous/next task"},
		{"A", "Add video by URL or ID (Tab: cycle mode)"},
		{"T", "Trim finished video"},
		{"C", "Cancel selected job"},
		{"R", "Retry failed/cancelled job"},
		{"D", "Delete job (press twice to confirm)"},
		{"F", "Cycle status filter"},
		{"O", "Open output folder (finished jobs)"},
		{"Enter", "Expand/collapse archived jobs"},
	},
}

var detailKeys = helpSection{
	title: "Details Panel",
	keys: []helpKey{
		{"↑/↓", "Scroll details up/down"},
	},
}

var logKeys = helpSection{
	title: "Logs Panel",
	keys: []helpKey{
		{"↑/↓", "Scroll logs up/down"},
		{"PgUp/PgDn", "Page up/down"},
		{"L", "Cycle log level filter"},
		{"", "Auto-scroll pauses when scrolling up"},
		{"", "Resume by scrolling to bottom"},
	},
}

var mouseKeys = helpSection{
	title: "Mouse",
	keys: []helpKey{
		{"Click", "Select task / focus panel"},
		{"Scroll", "Scroll focused panel"},
	},
}

// HelpModel renders the help overlay.
type HelpModel struct {
	visible      bool
	scrollOffset int
	width        int
	height       int
	activePanel  FocusPanel
}

// NewHelpModel creates a new help model.
func NewHelpModel() *HelpModel {
	return &HelpModel{}
}

// Toggle toggles the help overlay visibility.
func (m *HelpModel) Toggle() {
	m.visible = !m.visible
	m.scrollOffset = 0
}

// IsVisible returns true if the help overlay is shown.
func (m *HelpModel) IsVisible() bool {
	return m.visible
}

// SetSize updates the overlay dimensions.
func (m *HelpModel) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// SetActivePanel sets which panel is active (for ordering sections).
func (m *HelpModel) SetActivePanel(panel FocusPanel) {
	m.activePanel = panel
}

// ScrollUp scrolls the help overlay up.
func (m *HelpModel) ScrollUp() {
	if m.scrollOffset > 0 {
		m.scrollOffset--
	}
}

// ScrollDown scrolls the help overlay down.
func (m *HelpModel) ScrollDown() {
	m.scrollOffset++
}

// View renders the help overlay.
func (m *HelpModel) View() string {
	if !m.visible {
		return ""
	}

	w := m.width - 4
	h := m.height - 4
	if w < 20 {
		w = 20
	}
	if h < 5 {
		h = 5
	}

	sections := m.orderedSections()

	var allLines []string
	for i, sec := range sections {
		if i > 0 {
			allLines = append(allLines, "")
		}
		title := sec.title
		if m.isActiveSection(sec.title) {
			title += " [active]"
		}
		allLines = append(allLines, HeaderStyle.Render(title))
		for _, k := range sec.keys {
			// Yellow keys (match TS), 14-char padding (match TS padEnd(14))
			keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Width(14)
			allLines = append(allLines, "  "+keyStyle.Render(k.key)+k.desc)
		}
	}

	// Apply scroll
	maxScroll := len(allLines) - h
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scrollOffset > maxScroll {
		m.scrollOffset = maxScroll
	}

	end := m.scrollOffset + h
	if end > len(allLines) {
		end = len(allLines)
	}
	start := m.scrollOffset
	if start > len(allLines) {
		start = len(allLines)
	}
	visible := allLines[start:end]

	for len(visible) < h {
		visible = append(visible, "")
	}

	// Header with scroll percentage (H1) - brackets match TS [XX%]
	headerParts := TitleStyle.Render("Help")
	if maxScroll > 0 {
		pct := 0
		if maxScroll > 0 {
			pct = m.scrollOffset * 100 / maxScroll
		}
		headerParts += " " + DimStyle.Render(fmt.Sprintf("[%d%%]", pct))
	}
	headerParts += strings.Repeat(" ", max(0, w-20)) + DimStyle.Render("(press ? or Esc to close)")

	content := headerParts + "\n" + strings.Join(visible, "\n")

	box := FocusedBorder.Width(w).Height(h).Render(content)

	padLeft := (m.width - w - 2) / 2
	padTop := (m.height - h - 2) / 2
	if padLeft < 0 {
		padLeft = 0
	}
	if padTop < 0 {
		padTop = 0
	}

	var result strings.Builder
	for range padTop {
		result.WriteString(strings.Repeat(" ", m.width) + "\n")
	}
	for _, line := range strings.Split(box, "\n") {
		result.WriteString(strings.Repeat(" ", padLeft) + line + "\n")
	}

	return result.String()
}

func (m *HelpModel) orderedSections() []helpSection {
	// Match TS: Global first, then active panel's section moved before Global
	var sections []helpSection

	switch m.activePanel {
	case PanelTasks:
		sections = append(sections, taskKeys, globalKeys, detailKeys, logKeys)
	case PanelDetails:
		sections = append(sections, detailKeys, globalKeys, taskKeys, logKeys)
	case PanelLogs:
		sections = append(sections, logKeys, globalKeys, taskKeys, detailKeys)
	default:
		sections = append(sections, globalKeys, taskKeys, detailKeys, logKeys)
	}

	sections = append(sections, mouseKeys)
	return sections
}

func (m *HelpModel) isActiveSection(title string) bool {
	switch m.activePanel {
	case PanelTasks:
		return title == "Tasks Panel"
	case PanelDetails:
		return title == "Details Panel"
	case PanelLogs:
		return title == "Logs Panel"
	}
	return false
}
