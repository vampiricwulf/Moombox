package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
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
		{"A I", "Import archive"},
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

// helpViewportKeyMap returns a KeyMap with only arrow keys and pgup/pgdn,
// disabling letter keys (f, b, u, d, k, j, h, l) that conflict with app chords.
func helpViewportKeyMap() viewport.KeyMap {
	return viewport.KeyMap{
		PageDown: key.NewBinding(
			key.WithKeys("pgdown"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup"),
		),
		HalfPageUp: key.NewBinding(
			key.WithKeys("ctrl+u"),
		),
		HalfPageDown: key.NewBinding(
			key.WithKeys("ctrl+d"),
		),
		Up: key.NewBinding(
			key.WithKeys("up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down"),
		),
		Left: key.NewBinding(
			key.WithDisabled(),
		),
		Right: key.NewBinding(
			key.WithDisabled(),
		),
	}
}

// HelpModel renders the help overlay.
type HelpModel struct {
	visible  bool
	viewport viewport.Model
	width    int
	height   int
}

// NewHelpModel creates a new help model.
func NewHelpModel() *HelpModel {
	vp := viewport.New(0, 0)
	vp.KeyMap = helpViewportKeyMap()
	return &HelpModel{
		viewport: vp,
	}
}

// Toggle toggles the help overlay visibility.
func (m *HelpModel) Toggle() {
	m.visible = !m.visible
	if m.visible {
		m.buildContent()
	}
}

// IsVisible returns true if the help overlay is shown.
func (m *HelpModel) IsVisible() bool {
	return m.visible
}

// SetSize updates the overlay dimensions.
func (m *HelpModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	if m.visible {
		m.buildContent()
	}
}

// UpdateViewport delegates messages to the viewport for scrolling.
func (m *HelpModel) UpdateViewport(msg tea.Msg) tea.Cmd {
	if !m.visible {
		return nil
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return cmd
}

// buildContent constructs the help text and sets it into the viewport.
func (m *HelpModel) buildContent() {
	sections := m.orderedSections()
	var allLines []string
	for i, sec := range sections {
		if i > 0 {
			allLines = append(allLines, "")
		}
		allLines = append(allLines, HeaderStyle.Render(sec.title))
		for _, k := range sec.keys {
			keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Width(14)
			allLines = append(allLines, "  "+keyStyle.Render(k.key)+k.desc)
		}
	}

	w := m.width - 4
	h := m.height - 4
	if w < 20 {
		w = 20
	}
	if h < 5 {
		h = 5
	}

	m.viewport.Width = w
	m.viewport.Height = h - 1 // -1 for header line
	m.viewport.SetContent(strings.Join(allLines, "\n"))
	m.viewport.GotoTop()
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

	// Header with scroll percentage
	headerParts := TitleStyle.Render("Help")
	if m.viewport.TotalLineCount() > m.viewport.Height {
		pct := int(m.viewport.ScrollPercent() * 100)
		headerParts += " " + DimStyle.Render(fmt.Sprintf("[%d%%]", pct))
	}
	headerParts += strings.Repeat(" ", max(0, w-20)) + DimStyle.Render("(press ? or Esc to close)")

	content := headerParts + "\n" + m.viewport.View()

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
