package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type helpSection struct {
	title string
	keys  []helpKey
}

type helpKey struct {
	key  string
	desc string
}

var quickKeys = helpSection{
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
		{"/", "Search logs (when Logs focused)"},
		{"n / N", "Next / previous search match"},
	},
}

var mouseKeys = helpSection{
	title: "Mouse",
	keys: []helpKey{
		{"Click", "Select task / focus panel"},
		{"Scroll", "Scroll focused panel"},
	},
}

// Category display titles for help sections.
var categoryHelpTitles = map[string]string{
	"Action":  "Action (A)",
	"Request": "Request (R)",
	"Open":    "Open (O)",
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
	visible   bool
	viewport  viewport.Model
	width     int
	height    int
	menuItems []ActionMenuItem
}

// NewHelpModel creates a new help model.
func NewHelpModel() *HelpModel {
	vp := viewport.New(viewport.WithWidth(0), viewport.WithHeight(0))
	vp.KeyMap = helpViewportKeyMap()
	return &HelpModel{
		viewport: vp,
	}
}

// SetMenuItems updates the menu items used to generate help sections.
func (m *HelpModel) SetMenuItems(items []ActionMenuItem) {
	m.menuItems = items
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

	w := max(m.width-4, 20)
	h := max(m.height-4, 5)

	m.viewport.SetWidth(w)
	m.viewport.SetHeight(h - 1) // -1 for header line
	m.viewport.SetContent(strings.Join(allLines, "\n"))
	m.viewport.GotoTop()
}

// View renders the help overlay.
func (m *HelpModel) View() string {
	if !m.visible {
		return ""
	}

	w := max(m.width-4, 20)
	h := max(m.height-4, 5)

	// Header with scroll percentage
	leftPart := TitleStyle.Render("Help")
	if m.viewport.TotalLineCount() > m.viewport.Height() {
		pct := int(m.viewport.ScrollPercent() * 100)
		leftPart += " " + DimStyle.Render(fmt.Sprintf("[%d%%]", pct))
	}
	rightPart := DimStyle.Render("(press ? or Esc to close)")
	leftW := lipgloss.Width(leftPart)
	rightW := lipgloss.Width(rightPart)
	gap := max(1, w-leftW-rightW)
	header := leftPart + strings.Repeat(" ", gap) + rightPart

	content := header + "\n" + m.viewport.View()

	box := FocusedBorder.Width(w + 2).Height(h + 2).Render(content)

	return centerBox(box, m.width, m.height)
}

// sectionsFromMenu generates help sections for Action/Request/Open from menu items.
func (m *HelpModel) sectionsFromMenu() []helpSection {
	// Preserve category order
	categoryOrder := []string{"Action", "Request", "Open"}
	grouped := make(map[string][]helpKey)

	for _, item := range m.menuItems {
		if _, ok := categoryHelpTitles[item.Category]; !ok {
			continue // Skip Filter/Other — handled as static sections
		}

		chord := item.Chord
		if item.NeedsConfirm {
			// Show confirm chord: "A C" → "A C C", "R P" → "R P P"
			parts := strings.Fields(chord)
			if len(parts) == 2 {
				chord = chord + " " + parts[1]
			}
		}
		grouped[item.Category] = append(grouped[item.Category], helpKey{
			key:  chord,
			desc: item.Label,
		})
	}

	var sections []helpSection
	for _, cat := range categoryOrder {
		keys, ok := grouped[cat]
		if !ok || len(keys) == 0 {
			continue
		}
		sections = append(sections, helpSection{
			title: categoryHelpTitles[cat],
			keys:  keys,
		})
	}
	return sections
}

func (m *HelpModel) orderedSections() []helpSection {
	var sections []helpSection

	// Dynamic sections from menu items (Action, Request, Open)
	if len(m.menuItems) > 0 {
		sections = append(sections, m.sectionsFromMenu()...)
	}

	// Static sections
	sections = append(sections, quickKeys, navigationKeys, mouseKeys)
	return sections
}
