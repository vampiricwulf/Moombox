package tui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/list"
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
	Chord        string                   // e.g. "A A", "R C", "F"
	Label        string                   // e.g. "Add Video"
	HintLabel    string                   // short label for chord feedback, e.g. "Add", "Retry", "Tokens"
	Category     string                   // "Action", "Request", "Open", "Filter", "Other"
	NeedsJob     bool                     // true → transitions to job selector
	NeedsConfirm bool                     // true → transitions to confirm prompt
	JobFilter    func(*database.Job) bool // filter for job selector (nil = show all)
}

// menuActionItem is a list.Item for the main action menu.
type menuActionItem struct {
	isHeader  bool
	isBlank   bool
	category  string
	action    *ActionMenuItem
	actionIdx int  // index in m.items (-1 for header/blank)
	noJobs    bool // pre-computed: NeedsJob && count == 0
}

func (i menuActionItem) FilterValue() string {
	if i.action != nil {
		return i.action.Label
	}
	return ""
}

// menuJobItem is a list.Item for the job selector.
type menuJobItem struct {
	job *database.Job
}

func (i menuJobItem) FilterValue() string { return i.job.Title }

// menuActionDelegate renders action menu items.
type menuActionDelegate struct{}

func (d menuActionDelegate) Height() int                             { return 1 }
func (d menuActionDelegate) Spacing() int                            { return 0 }
func (d menuActionDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

func (d menuActionDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	mi, ok := item.(menuActionItem)
	if !ok {
		return
	}

	if mi.isBlank {
		return
	}
	if mi.isHeader {
		fmt.Fprint(w, HeaderStyle.Render(mi.category))
		return
	}

	contentW := m.Width()
	chord := lipgloss.NewStyle().Foreground(ColorCyan).Width(6).Render(mi.action.Chord)
	label := mi.action.Label
	if mi.noJobs {
		label += DimStyle.Render(" (none)")
	}
	text := "  " + chord + " " + label
	if index == m.Index() {
		text = lipgloss.NewStyle().Bold(true).Foreground(ColorWhite).Background(lipgloss.Color("#333355")).Render(
			padToWidth(text, contentW),
		)
	}
	fmt.Fprint(w, text)
}

// menuJobDelegate renders job selector items.
type menuJobDelegate struct{}

func (d menuJobDelegate) Height() int                             { return 1 }
func (d menuJobDelegate) Spacing() int                            { return 0 }
func (d menuJobDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

func (d menuJobDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	ji, ok := item.(menuJobItem)
	if !ok {
		return
	}
	j := ji.job
	contentW := m.Width()
	statusStyle := lipgloss.NewStyle().Foreground(StatusColor(string(j.Status)))
	icon := StatusIcon(string(j.Status))
	title := truncateString(j.Title, contentW-17)
	line := fmt.Sprintf("  %s %s %s", icon, statusStyle.Render(padRight(string(j.Status), 11)), title)
	if index == m.Index() {
		line = lipgloss.NewStyle().Bold(true).Foreground(ColorWhite).Background(lipgloss.Color("#333355")).Render(
			padToWidth(line, contentW),
		)
	}
	fmt.Fprint(w, line)
}

// ActionMenuModel manages the command palette overlay.
type ActionMenuModel struct {
	visible       bool
	width, height int
	mode          menuMode

	// Main list
	items    []ActionMenuItem
	mainList list.Model

	// Job selector
	pendingAction string          // chord of action being executed
	pendingLabel  string          // label for display
	jobs          []*database.Job // all jobs (unfiltered source)
	filtered      []*database.Job // filtered job list for current action
	jobList       list.Model
	jobConfirm    bool // true after first Enter (waiting for second)

	// Confirm prompt (for R P etc.)
	confirmLabel string
}

// NewActionMenuModel creates a new action menu.
func NewActionMenuModel() *ActionMenuModel {
	m := &ActionMenuModel{}
	m.mainList = m.newMenuList(menuActionDelegate{})
	m.jobList = m.newMenuList(menuJobDelegate{})
	return m
}

func (m *ActionMenuModel) newMenuList(delegate list.ItemDelegate) list.Model {
	l := list.New(nil, delegate, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowFilter(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()

	km := l.KeyMap
	km.CursorUp.SetKeys("up")
	km.CursorDown.SetKeys("down")
	km.NextPage.SetKeys("pgdown")
	km.PrevPage.SetKeys("pgup")
	km.GoToStart.SetKeys("home")
	km.GoToEnd.SetKeys("end")
	l.KeyMap = km

	return l
}

// Open shows the menu with the given items.
func (m *ActionMenuModel) Open(items []ActionMenuItem) {
	m.visible = true
	m.mode = menuMain
	m.items = items
	m.pendingAction = ""
	m.pendingLabel = ""
	m.filtered = nil
	m.jobConfirm = false
	m.confirmLabel = ""

	// Build list items with category headers
	var listItems []list.Item
	lastCat := ""
	for i := range items {
		if items[i].Category != lastCat {
			if lastCat != "" {
				listItems = append(listItems, menuActionItem{isBlank: true, actionIdx: -1})
			}
			listItems = append(listItems, menuActionItem{
				isHeader: true,
				category: items[i].Category,
				actionIdx: -1,
			})
			lastCat = items[i].Category
		}
		noJobs := false
		if items[i].NeedsJob {
			count := 0
			for _, j := range m.jobs {
				if items[i].JobFilter == nil || items[i].JobFilter(j) {
					count++
				}
			}
			noJobs = count == 0
		}
		listItems = append(listItems, menuActionItem{
			action:    &items[i],
			actionIdx: i,
			noJobs:    noJobs,
		})
	}
	m.mainList.SetItems(listItems)
	m.updateListSizes()

	// Skip cursor to first selectable action item
	m.mainList.Select(0)
	m.skipMainForward()
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
	m.updateListSizes()
}

func (m *ActionMenuModel) updateListSizes() {
	boxW := m.width - 8
	if boxW > 60 {
		boxW = 60
	}
	if boxW < 30 {
		boxW = 30
	}
	contentW := boxW - 2
	ch := m.contentHeight()
	m.mainList.SetSize(contentW, ch)
	m.jobList.SetSize(contentW, ch)
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

// isSelectable returns true if the item at the given index is an action (not header/blank).
func (m *ActionMenuModel) isMainSelectable(idx int) bool {
	items := m.mainList.Items()
	if idx < 0 || idx >= len(items) {
		return false
	}
	mi, ok := items[idx].(menuActionItem)
	return ok && !mi.isHeader && !mi.isBlank
}

// skipMainForward moves the cursor forward until it lands on a selectable item.
func (m *ActionMenuModel) skipMainForward() {
	for !m.isMainSelectable(m.mainList.Index()) {
		if m.mainList.Index() >= len(m.mainList.Items())-1 {
			return // at end, can't skip further
		}
		m.mainList.CursorDown()
	}
}

// mainCursorDown moves cursor down, skipping non-selectable items.
func (m *ActionMenuModel) mainCursorDown() {
	prev := m.mainList.Index()
	m.mainList.CursorDown()
	for !m.isMainSelectable(m.mainList.Index()) {
		if m.mainList.Index() >= len(m.mainList.Items())-1 {
			m.mainList.Select(prev) // can't skip — revert
			return
		}
		m.mainList.CursorDown()
	}
}

// mainCursorUp moves cursor up, skipping non-selectable items.
func (m *ActionMenuModel) mainCursorUp() {
	prev := m.mainList.Index()
	m.mainList.CursorUp()
	for !m.isMainSelectable(m.mainList.Index()) {
		if m.mainList.Index() <= 0 {
			m.mainList.Select(prev) // can't skip — revert
			return
		}
		m.mainList.CursorUp()
	}
}

// selectedAction returns the ActionMenuItem at the current cursor, or nil.
func (m *ActionMenuModel) selectedAction() *ActionMenuItem {
	sel := m.mainList.SelectedItem()
	if sel == nil {
		return nil
	}
	mi, ok := sel.(menuActionItem)
	if !ok || mi.action == nil {
		return nil
	}
	return mi.action
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
		m.mainCursorUp()
	case keyDown:
		m.mainCursorDown()
	case keyEnter:
		item := m.selectedAction()
		if item == nil {
			return ""
		}
		if item.NeedsJob {
			m.pendingAction = item.Chord
			m.pendingLabel = item.Label
			m.filtered = m.filterJobs(item.JobFilter)
			m.jobConfirm = false
			if len(m.filtered) == 0 {
				return ""
			}
			// Build job list items
			jobItems := make([]list.Item, len(m.filtered))
			for i, j := range m.filtered {
				jobItems[i] = menuJobItem{job: j}
			}
			m.jobList.SetItems(jobItems)
			m.jobList.Select(0)
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
	return ""
}

func (m *ActionMenuModel) handleJobSelectKey(key string) string {
	switch key {
	case keyEsc:
		if m.jobConfirm {
			m.jobConfirm = false
			return ""
		}
		m.mode = menuMain
		return ""
	case keyUp:
		m.jobList.CursorUp()
		m.jobConfirm = false
	case keyDown:
		m.jobList.CursorDown()
		m.jobConfirm = false
	case keyEnter:
		sel := m.jobList.SelectedItem()
		if sel == nil {
			return ""
		}
		ji, ok := sel.(menuJobItem)
		if !ok {
			return ""
		}
		if m.jobConfirm {
			action := m.pendingAction + ":" + ji.job.ID
			m.Close()
			return action
		}
		m.jobConfirm = true
		return ""
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
	if m.mode == menuConfirm {
		ch = 3
	}

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
			perPage := m.mainList.Paginator.PerPage
			if perPage <= 0 {
				return
			}
			globalIdx := m.mainList.Paginator.Page*perPage + lineIdx
			if globalIdx >= 0 && globalIdx < len(m.mainList.Items()) && m.isMainSelectable(globalIdx) {
				m.mainList.Select(globalIdx)
			}
		case menuJobSelect:
			perPage := m.jobList.Paginator.PerPage
			if perPage <= 0 {
				return
			}
			globalIdx := m.jobList.Paginator.Page*perPage + lineIdx
			if globalIdx >= 0 && globalIdx < len(m.jobList.Items()) {
				if globalIdx != m.jobList.Index() {
					m.jobConfirm = false
				}
				m.jobList.Select(globalIdx)
			}
		}
		return
	}

	if isScrollUp(msg) || isScrollDown(msg) {
		switch m.mode {
		case menuMain:
			if isScrollUp(msg) {
				m.mainCursorUp()
			} else {
				m.mainCursorDown()
			}
		case menuJobSelect:
			if isScrollUp(msg) {
				m.jobList.CursorUp()
			} else {
				m.jobList.CursorDown()
			}
			m.jobConfirm = false
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
		return m.renderMain(contentW)
	case menuJobSelect:
		return m.renderJobSelect(contentW)
	case menuConfirm:
		return m.renderConfirm(contentW)
	}
	return ""
}

func (m *ActionMenuModel) renderMain(contentW int) string {
	header := TitleStyle.Render("Action Menu") +
		strings.Repeat(" ", max(0, contentW-36)) +
		DimStyle.Render("↑↓ navigate | Enter | Esc")

	footer := DimStyle.Render("M to close")

	content := header + "\n" + m.mainList.View() + "\n" + footer
	box := FocusedBorder.Width(contentW).Render(content)
	return centerBox(box, m.width, m.height)
}

func (m *ActionMenuModel) renderJobSelect(contentW int) string {
	header := TitleStyle.Render(m.pendingLabel) +
		DimStyle.Render(fmt.Sprintf(" — Select job (%d)", len(m.filtered)))

	var footer string
	if m.jobConfirm {
		footer = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Bold(true).Render(
			fmt.Sprintf("Enter: confirm %s | Esc: cancel", strings.ToLower(m.pendingLabel)))
	} else {
		footer = DimStyle.Render("Enter: select | Esc: back")
	}

	content := header + "\n" + m.jobList.View() + "\n" + footer
	box := FocusedBorder.Width(contentW).Render(content)
	return centerBox(box, m.width, m.height)
}

func (m *ActionMenuModel) renderConfirm(contentW int) string {
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
	return centerBox(box, m.width, m.height)
}

// padToWidth pads a string with spaces to reach at least width w (by visual width).
func padToWidth(s string, w int) string {
	sw := lipgloss.Width(s)
	if sw >= w {
		return s
	}
	return s + strings.Repeat(" ", w-sw)
}
