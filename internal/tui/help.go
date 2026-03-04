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

var actionKeys = helpSection{
	title: "Action (A)",
	keys: []helpKey{
		{"A A", "Add video"},
		{"A R", "Retry failed/cancelled job"},
		{"A C C", "Cancel active job (confirm)"},
		{"A D D", "Delete job (confirm)"},
		{"A T", "Trim finished video"},
		{"A O", "Browse orphaned files"},
	},
}

var requestKeys = helpSection{
	title: "Request (R)",
	keys: []helpKey{
		{"R C", "Recheck cookie authentication"},
		{"R F", "Force browser cookie refresh"},
		{"R V", "Check for updates"},
		{"R U", "Apply pending update"},
		{"R P P", "Restart program (confirm)"},
	},
}

var openKeys = helpSection{
	title: "Open (O)",
	keys: []helpKey{
		{"O F", "Open output/staging folder"},
		{"O S", "Open stream page in browser"},
		{"O W", "Open web dashboard"},
	},
}

var singleKeys = helpSection{
	title: "Quick Keys",
	keys: []helpKey{
		{"F", "Filter (panel-sensitive)"},
		{"M", "Action menu"},
		{"Q Q", "Quit program"},
		{"Ctrl+C", "Quit immediately"},
		{"Tab", "Cycle panel focus"},
		{"`", "Open settings"},
		{"?", "Toggle help"},
	},
}

var navigationKeys = helpSection{
	title: "Navigation",
	keys: []helpKey{
		{"↑/↓", "Select / Scroll"},
		{"PgUp/PgDn", "Page scroll (Logs)"},
		{"Enter", "Expand/collapse archives"},
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
		allLines = append(allLines, HeaderStyle.Render(sec.title))
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
	return []helpSection{
		actionKeys,
		requestKeys,
		openKeys,
		singleKeys,
		navigationKeys,
		mouseKeys,
	}
}
