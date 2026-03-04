package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/vampiricwulf/Moombox/internal/database"
)

type menuMode int

const (
	menuMain      menuMode = iota // browsing action list
	menuJobSelect                 // picking a job for a job-specific action
	menuConfirm                   // Enter to confirm, Esc to cancel
)

// ActionMenuItem describes a single entry in the action menu.
type ActionMenuItem struct {
	Chord        string                     // e.g. "A A", "R C", "F"
	Label        string                     // e.g. "Add Video"
	Category     string                     // "Action", "Request", "Open", "Filter", "Other"
	NeedsJob     bool                       // true → transitions to job selector
	NeedsConfirm bool                       // true → transitions to confirm prompt
	JobFilter    func(*database.Job) bool   // filter for job selector (nil = show all)
}

// ActionMenuModel manages the command palette overlay.
type ActionMenuModel struct {
	visible      bool
	width, height int
	mode         menuMode

	// Main list
	items          []ActionMenuItem
	selectedIdx    int
	scrollOffset   int
	visibleEntries []int // maps visible content line → itemIdx (-1 for headers/blanks)

	// Job selector
	pendingAction string           // chord of action being executed
	pendingLabel  string           // label for display
	jobs          []*database.Job  // all jobs (unfiltered source)
	filtered      []*database.Job  // filtered job list for current action
	jobIdx        int              // selected job index
	jobScroll     int              // job list scroll offset
	jobConfirm    bool             // true after first Enter (waiting for second)

	// Confirm prompt (for R P etc.)
	confirmLabel string
}

// NewActionMenuModel creates a new action menu.
func NewActionMenuModel() *ActionMenuModel {
	return &ActionMenuModel{}
}

// Open shows the menu with the given items.
func (m *ActionMenuModel) Open(items []ActionMenuItem) {
	m.visible = true
	m.mode = menuMain
	m.items = items
	m.selectedIdx = 0
	m.scrollOffset = 0
	m.pendingAction = ""
	m.pendingLabel = ""
	m.filtered = nil
	m.jobIdx = 0
	m.jobScroll = 0
	m.jobConfirm = false
	m.confirmLabel = ""
}

// Close hides the menu.
func (m *ActionMenuModel) Close() {
	m.visible = false
}

// IsVisible returns true if the menu is shown.
func (m *ActionMenuModel) IsVisible() bool {
	return m.visible
}

// SetSize updates the overlay dimensions.
func (m *ActionMenuModel) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// SetJobs provides the full job list for job selector filtering.
func (m *ActionMenuModel) SetJobs(jobs []*database.Job) {
	m.jobs = jobs
}

// contentHeight returns the visible list height inside the box.
func (m *ActionMenuModel) contentHeight() int {
	h := m.height - 8 // borders + header + footer
	if h < 3 {
		h = 3
	}
	return h
}

// HandleKey processes a key press and returns an action string.
// Returns "" for internal navigation, "close" to dismiss, or an action chord
// like "A A" or "A D:jobID123".
func (m *ActionMenuModel) HandleKey(key string) string {
	switch m.mode {
	case menuMain:
		return m.handleMainKey(key)
	case menuJobSelect:
		return m.handleJobSelectKey(key)
	case menuConfirm:
		return m.handleConfirmKey(key)
	}
	return ""
}

func (m *ActionMenuModel) handleMainKey(key string) string {
	switch key {
	case keyEsc, "m", "M":
		m.Close()
		return "close"
	case keyUp:
		if m.selectedIdx > 0 {
			m.selectedIdx--
			m.ensureMainVisible()
		}
	case keyDown:
		if m.selectedIdx < len(m.items)-1 {
			m.selectedIdx++
			m.ensureMainVisible()
		}
	case keyEnter:
		if m.selectedIdx >= 0 && m.selectedIdx < len(m.items) {
			item := m.items[m.selectedIdx]
			if item.NeedsJob {
				// Transition to job selector
				m.pendingAction = item.Chord
				m.pendingLabel = item.Label
				m.filtered = m.filterJobs(item.JobFilter)
				m.jobIdx = 0
				m.jobScroll = 0
				m.jobConfirm = false
				if len(m.filtered) == 0 {
					// No matching jobs — stay in main
					return ""
				}
				m.mode = menuJobSelect
				return ""
			}
			if item.NeedsConfirm {
				m.confirmLabel = item.Label
				m.pendingAction = item.Chord
				m.mode = menuConfirm
				return ""
			}
			// Direct action
			m.Close()
			return item.Chord
		}
	}
	return ""
}

func (m *ActionMenuModel) handleJobSelectKey(key string) string {
	switch key {
	case keyEsc:
		if m.jobConfirm {
			m.jobConfirm = false
			return ""
		}
		// Back to main
		m.mode = menuMain
		return ""
	case keyUp:
		if m.jobIdx > 0 {
			m.jobIdx--
			m.jobConfirm = false
			m.ensureJobVisible()
		}
	case keyDown:
		if m.jobIdx < len(m.filtered)-1 {
			m.jobIdx++
			m.jobConfirm = false
			m.ensureJobVisible()
		}
	case keyEnter:
		if m.jobIdx >= 0 && m.jobIdx < len(m.filtered) {
			if m.jobConfirm {
				// Second Enter: confirmed
				job := m.filtered[m.jobIdx]
				action := m.pendingAction + ":" + job.ID
				m.Close()
				return action
			}
			// First Enter: show confirm
			m.jobConfirm = true
			return ""
		}
	}
	return ""
}

func (m *ActionMenuModel) handleConfirmKey(key string) string {
	switch key {
	case keyEsc:
		m.mode = menuMain
		m.confirmLabel = ""
		return ""
	case keyEnter:
		action := m.pendingAction
		m.Close()
		return action
	}
	return ""
}

func (m *ActionMenuModel) filterJobs(filter func(*database.Job) bool) []*database.Job {
	var result []*database.Job
	for _, j := range m.jobs {
		if filter == nil || filter(j) {
			result = append(result, j)
		}
	}
	return result
}

func (m *ActionMenuModel) ensureMainVisible() {
	ch := m.contentHeight()
	if m.selectedIdx < m.scrollOffset {
		m.scrollOffset = m.selectedIdx
	}
	if m.selectedIdx >= m.scrollOffset+ch {
		m.scrollOffset = m.selectedIdx - ch + 1
	}
}

func (m *ActionMenuModel) ensureJobVisible() {
	ch := m.contentHeight()
	if m.jobIdx < m.jobScroll {
		m.jobScroll = m.jobIdx
	}
	if m.jobIdx >= m.jobScroll+ch {
		m.jobScroll = m.jobIdx - ch + 1
	}
}

// overlayGeometry returns the overlay box position and content dimensions.
// Returns (padLeft, padTop, boxW, contentH).
func (m *ActionMenuModel) overlayGeometry() (int, int, int, int) {
	boxW := m.width - 8
	if boxW > 60 {
		boxW = 60
	}
	if boxW < 30 {
		boxW = 30
	}
	contentW := boxW - 2
	ch := m.contentHeight()

	// Box rendered size: contentW+2 wide (borders), ch+4 tall (borders+header+footer)
	renderedW := contentW + 2
	renderedH := ch + 4

	padLeft := (m.width - renderedW) / 2
	padTop := (m.height - renderedH) / 2
	if padLeft < 0 {
		padLeft = 0
	}
	if padTop < 0 {
		padTop = 0
	}
	return padLeft, padTop, renderedW, ch
}

// HandleMouse processes a mouse event for selection (click/scroll).
// Mouse only changes selection — never triggers actions (Enter required).
func (m *ActionMenuModel) HandleMouse(msg tea.MouseMsg) {
	if !m.visible {
		return
	}

	padLeft, padTop, renderedW, ch := m.overlayGeometry()
	x, y := msg.X, msg.Y

	// Check X bounds (within the overlay box)
	inBox := x >= padLeft && x < padLeft+renderedW

	if isLeftClick(msg) && inBox {
		// Content lines start at padTop + 2 (top border + header)
		lineIdx := y - padTop - 2
		if lineIdx < 0 || lineIdx >= ch {
			return
		}

		switch m.mode {
		case menuMain:
			if lineIdx < len(m.visibleEntries) {
				itemIdx := m.visibleEntries[lineIdx]
				if itemIdx >= 0 && itemIdx < len(m.items) {
					m.selectedIdx = itemIdx
				}
			}
		case menuJobSelect:
			jobIdx := m.jobScroll + lineIdx
			if jobIdx >= 0 && jobIdx < len(m.filtered) {
				if jobIdx != m.jobIdx {
					m.jobConfirm = false // reset confirm on re-select
				}
				m.jobIdx = jobIdx
			}
		}
		return
	}

	if isScrollUp(msg) || isScrollDown(msg) {
		switch m.mode {
		case menuMain:
			if isScrollUp(msg) {
				if m.selectedIdx > 0 {
					m.selectedIdx--
					m.ensureMainVisible()
				}
			} else {
				if m.selectedIdx < len(m.items)-1 {
					m.selectedIdx++
					m.ensureMainVisible()
				}
			}
		case menuJobSelect:
			if isScrollUp(msg) {
				if m.jobIdx > 0 {
					m.jobIdx--
					m.jobConfirm = false
					m.ensureJobVisible()
				}
			} else {
				if m.jobIdx < len(m.filtered)-1 {
					m.jobIdx++
					m.jobConfirm = false
					m.ensureJobVisible()
				}
			}
		}
	}
}

// View renders the action menu overlay.
func (m *ActionMenuModel) View() string {
	if !m.visible {
		return ""
	}

	boxW := m.width - 8
	if boxW > 60 {
		boxW = 60
	}
	if boxW < 30 {
		boxW = 30
	}
	contentW := boxW - 2

	switch m.mode {
	case menuMain:
		return m.renderMain(boxW, contentW)
	case menuJobSelect:
		return m.renderJobSelect(boxW, contentW)
	case menuConfirm:
		return m.renderConfirm(boxW, contentW)
	}
	return ""
}

func (m *ActionMenuModel) renderMain(boxW, contentW int) string {
	ch := m.contentHeight()

	// Build lines with category headers
	type lineEntry struct {
		text     string
		isHeader bool
		itemIdx  int // index into m.items (-1 for headers)
	}
	var entries []lineEntry
	lastCat := ""
	for i, item := range m.items {
		if item.Category != lastCat {
			if lastCat != "" {
				entries = append(entries, lineEntry{text: "", isHeader: true, itemIdx: -1})
			}
			entries = append(entries, lineEntry{
				text:     HeaderStyle.Render(item.Category),
				isHeader: true,
				itemIdx:  -1,
			})
			lastCat = item.Category
		}
		chord := lipgloss.NewStyle().Foreground(ColorCyan).Width(6).Render(item.Chord)
		label := item.Label
		if item.NeedsJob {
			// Count matching jobs
			count := 0
			for _, j := range m.jobs {
				if item.JobFilter == nil || item.JobFilter(j) {
					count++
				}
			}
			if count == 0 {
				label += DimStyle.Render(" (none)")
			}
		}
		text := "  " + chord + " " + label
		if i == m.selectedIdx {
			text = lipgloss.NewStyle().Bold(true).Foreground(ColorWhite).Background(lipgloss.Color("#333355")).Render(
				padToWidth(text, contentW),
			)
		}
		entries = append(entries, lineEntry{text: text, itemIdx: i})
	}

	// Map selectedIdx to entry position for scroll
	selectedEntry := 0
	for ei, e := range entries {
		if e.itemIdx == m.selectedIdx {
			selectedEntry = ei
			break
		}
	}

	// Scroll to keep selected visible
	if selectedEntry < m.scrollOffset {
		m.scrollOffset = selectedEntry
	}
	if selectedEntry >= m.scrollOffset+ch {
		m.scrollOffset = selectedEntry - ch + 1
	}

	end := m.scrollOffset + ch
	if end > len(entries) {
		end = len(entries)
	}
	start := m.scrollOffset
	if start > len(entries) {
		start = len(entries)
	}
	visible := entries[start:end]

	// Build visible lines and track line→item mapping for mouse
	m.visibleEntries = make([]int, ch)
	for i := range ch {
		m.visibleEntries[i] = -1
	}
	var lines []string
	for i, e := range visible {
		lines = append(lines, e.text)
		if i < ch {
			m.visibleEntries[i] = e.itemIdx
		}
	}
	for len(lines) < ch {
		lines = append(lines, "")
	}

	header := TitleStyle.Render("Action Menu") +
		strings.Repeat(" ", max(0, contentW-28)) +
		DimStyle.Render("↑↓ navigate | Enter | Esc")

	footer := DimStyle.Render("M to close")

	content := header + "\n" + strings.Join(lines, "\n") + "\n" + footer
	box := FocusedBorder.Width(contentW).Render(content)
	return m.centerOverlay(box)
}

func (m *ActionMenuModel) renderJobSelect(boxW, contentW int) string {
	ch := m.contentHeight()

	header := TitleStyle.Render(m.pendingLabel) +
		DimStyle.Render(fmt.Sprintf(" — Select job (%d)", len(m.filtered)))

	var lines []string
	end := m.jobScroll + ch
	if end > len(m.filtered) {
		end = len(m.filtered)
	}
	for i := m.jobScroll; i < end; i++ {
		j := m.filtered[i]
		statusStyle := lipgloss.NewStyle().Foreground(StatusColor(string(j.Status)))
		icon := StatusIcon(string(j.Status))
		title := truncateString(j.Title, contentW-12)
		line := fmt.Sprintf("  %s %s %s", icon, statusStyle.Render(padRight(string(j.Status), 6)), title)
		if i == m.jobIdx {
			line = lipgloss.NewStyle().Bold(true).Foreground(ColorWhite).Background(lipgloss.Color("#333355")).Render(
				padToWidth(line, contentW),
			)
		}
		lines = append(lines, line)
	}
	for len(lines) < ch {
		lines = append(lines, "")
	}

	var footer string
	if m.jobConfirm {
		footer = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Bold(true).Render(
			fmt.Sprintf("Enter: confirm %s | Esc: cancel", strings.ToLower(m.pendingLabel)))
	} else {
		footer = DimStyle.Render("Enter: select | Esc: back")
	}

	content := header + "\n" + strings.Join(lines, "\n") + "\n" + footer
	box := FocusedBorder.Width(contentW).Render(content)
	return m.centerOverlay(box)
}

func (m *ActionMenuModel) renderConfirm(boxW, contentW int) string {
	ch := 3
	prompt := lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Bold(true).Render(
		fmt.Sprintf("Confirm: %s?", m.confirmLabel))
	hint := DimStyle.Render("Enter: confirm | Esc: cancel")

	var lines []string
	lines = append(lines, "")
	lines = append(lines, "  "+prompt)
	lines = append(lines, "  "+hint)
	for len(lines) < ch+2 {
		lines = append(lines, "")
	}

	header := TitleStyle.Render("Confirm")
	content := header + "\n" + strings.Join(lines, "\n")
	box := FocusedBorder.Width(contentW).Render(content)
	return m.centerOverlay(box)
}

func (m *ActionMenuModel) centerOverlay(box string) string {
	boxLines := strings.Split(box, "\n")
	boxH := len(boxLines)
	boxW := 0
	for _, l := range boxLines {
		if w := lipgloss.Width(l); w > boxW {
			boxW = w
		}
	}

	padLeft := (m.width - boxW) / 2
	padTop := (m.height - boxH) / 2
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
	for _, line := range boxLines {
		result.WriteString(strings.Repeat(" ", padLeft) + line + "\n")
	}
	return result.String()
}

// padToWidth pads a string with spaces to reach at least width w (by visual width).
func padToWidth(s string, w int) string {
	sw := lipgloss.Width(s)
	if sw >= w {
		return s
	}
	return s + strings.Repeat(" ", w-sw)
}
