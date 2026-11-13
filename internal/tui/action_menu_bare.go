package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The bare form of the action menu, drawn below the normal layout
// (overlay_bare.go): a title row, a page of rows, a last row.
//
//	main     one action a row, its chord and its label, with no category
//	         rows: the selected action's category is in the title, beside
//	         the page. The last row is the keys, or why the selected action
//	         is dimmed: no job to act on, or an overlay that needs a larger
//	         terminal ("needs 60x20").
//	jobs     the action's short name and how many jobs it can take, then one
//	         job a row
//	confirm  a page of its own in place of the list: the action, the job,
//	         and what the action deletes or cancels (ActionMenuItem's
//	         ConfirmDetail) in full, wrapped
//
// A confirm whose detail does not fit the page whole is not armed (owner
// rulings PR-M1, R-D2 and R-C11: Reinitialize names everything it deletes or
// it does not ask), and the last row names the size that shows it. The same
// holds for one armed in a larger terminal that then shrank: the page says
// so, and Enter does not confirm.
//
// The lists' cursors are the normal form's (mainList, jobList). The main
// list is drawn here from its action items alone, in pages of its own; the
// job list is given the bare page's height in SetSize, so its own paging is
// the page drawn.

// bareMenuSelected is the cursor row's style, the normal form's.
func bareMenuSelected() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(ColorWhite).Background(lipgloss.Color("#333355"))
}

// bare reports whether the menu draws its bare form: its own size is below
// the normal layout. Derived on every call, never stored.
func (m *ActionMenuModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// bareRows is how many list rows a bare page has.
func (m *ActionMenuModel) bareRows() int {
	return max(m.height-2, 1)
}

// sizeRefusal is why item cannot run at the menu's size, "" when it can: the
// overlay it opens needs a larger terminal (overlayOpensAt, the one
// predicate; always "" in the normal layout).
func (m *ActionMenuModel) sizeRefusal(item *ActionMenuItem) string {
	if item == nil || item.Opens == "" {
		return ""
	}
	if ok, w, h := overlayOpensAt(item.Opens, m.width, m.height); !ok {
		return fmt.Sprintf("needs %dx%d", w, h)
	}
	return ""
}

// bareActions is the main list's action items, in order, with the position
// of the selected one among them.
func (m *ActionMenuModel) bareActions() (acts []menuActionItem, listIdx []int, sel int) {
	for i, it := range m.mainList.Items() {
		mi, ok := it.(menuActionItem)
		if !ok || mi.action == nil {
			continue
		}
		if i == m.mainList.Index() {
			sel = len(acts)
		}
		acts = append(acts, mi)
		listIdx = append(listIdx, i)
	}
	return acts, listIdx, sel
}

// bareWindow is the first row drawn of a list of n rows whose cursor is on
// sel, in pages of the bare page's height. covered is how many rows at the
// foot of the page a message takes: the cursor's row is kept above them.
func (m *ActionMenuModel) bareWindow(sel, covered int) (top int) {
	rows := m.bareRows()
	top = sel / rows * rows
	if visible := max(rows-covered, 1); sel-top >= visible {
		top = sel - visible + 1
	}
	return top
}

// bareMainReason is why the selected action is dimmed, "" when it is not.
func (m *ActionMenuModel) bareMainReason(mi menuActionItem) string {
	if why := m.sizeRefusal(mi.action); why != "" {
		return why
	}
	if !mi.noJobs {
		return ""
	}
	if mi.action.DisabledReason != "" {
		return mi.action.DisabledReason
	}
	return "none"
}

// bareFoot is the message at the foot of a list page as wrapped rows, nil
// for none: all but the first take list rows.
func (m *ActionMenuModel) bareFoot(msg string) []string {
	if msg == "" {
		return nil
	}
	return bareWrap(msg, m.width, max(m.bareRows(), 1))
}

// bareMainFoot is the main page's foot message: why the selected action is
// dimmed.
func (m *ActionMenuModel) bareMainFoot() string {
	acts, _, sel := m.bareActions()
	if len(acts) == 0 {
		return ""
	}
	return m.bareMainReason(acts[sel])
}

// bareMainKey is a key of the main list that the bare form answers itself:
// the page keys, which move by a bare page of actions. It reports whether
// key was one.
func (m *ActionMenuModel) bareMainKey(key string) bool {
	if key != keyPgDown && key != keyPgUp {
		return false
	}
	_, listIdx, sel := m.bareActions()
	if len(listIdx) == 0 {
		return true
	}
	step := m.bareRows()
	if key == keyPgUp {
		step = -step
	}
	m.mainList.Select(listIdx[min(max(sel+step, 0), len(listIdx)-1)])
	return true
}

// bareConfirmDetailRows is the armed confirm's detail, wrapped whole.
func (m *ActionMenuModel) bareConfirmDetailRows() []string {
	if m.confirmDetail == "" {
		return nil
	}
	return bareWrap(m.confirmDetail, m.width, len(m.confirmDetail)+1)
}

// bareConfirmFits reports whether the confirm page shows the detail whole:
// the page has every row but the action, the job and the keys for it.
func (m *ActionMenuModel) bareConfirmFits() bool {
	return len(m.bareConfirmDetailRows()) <= m.height-3
}

// bareConfirmRefusal is the line a confirm that does not fit is answered
// with: the size that shows it, at the menu's own width where a taller
// compact terminal is enough, else the normal layout's.
func (m *ActionMenuModel) bareConfirmRefusal() string {
	if need := len(m.bareConfirmDetailRows()) + 3; need < minTermHeight {
		return compactRefusal(m.width, need)
	}
	return compactRefusal(minTermWidth, minTermHeight)
}

// bareRefusal is the foot of the job page after a confirm was refused: the
// size that shows it, worked out for the terminal as it is now, and nothing
// once the terminal has grown enough to show it.
func (m *ActionMenuModel) bareRefusal() string {
	if !m.bareRefused || m.bareConfirmFits() {
		return ""
	}
	return m.bareConfirmRefusal()
}

// bareRefuseConfirm is the first Enter on a job in the bare form, after the
// confirm was armed: a confirm that does not fit whole is disarmed and the
// refusal noted for the foot of the page (bareRefusal). It reports whether it
// refused.
func (m *ActionMenuModel) bareRefuseConfirm() bool {
	if !m.bare() || m.bareConfirmFits() {
		return false
	}
	m.bareRefused = true
	m.jobConfirm = false
	return true
}

// bareMouse is the mouse over the bare form: the wheel moves the cursor a
// row, a click on a list row selects it. Nothing runs without Enter.
func (m *ActionMenuModel) bareMouse(msg tea.MouseMsg) {
	if isScrollUp(msg) || isScrollDown(msg) {
		m.bareRefused = false
		switch m.mode {
		case menuMain:
			if isScrollUp(msg) {
				m.mainCursorUp()
			} else {
				m.mainCursorDown()
			}
		case menuJobSelect:
			if m.jobConfirm {
				return // the confirm page has no list
			}
			if isScrollUp(msg) {
				m.jobList.CursorUp()
			} else {
				m.jobList.CursorDown()
			}
		}
		return
	}
	if !isLeftClick(msg) {
		return
	}
	row := msg.Mouse().Y - 1 // under the title
	if row < 0 || row >= m.bareRows() {
		return
	}
	switch m.mode {
	case menuMain:
		_, listIdx, sel := m.bareActions()
		foot := m.bareFoot(m.bareMainFoot())
		covered := max(len(foot)-1, 0)
		if at := m.bareWindow(sel, covered) + row; at < len(listIdx) && row < m.bareRows()-covered {
			m.mainList.Select(listIdx[at])
		}
	case menuJobSelect:
		if m.jobConfirm {
			return
		}
		foot := m.bareFoot(m.bareRefusal())
		covered := max(len(foot)-1, 0)
		if at := m.bareWindow(m.jobList.Index(), covered) + row; at < len(m.jobList.Items()) && row < m.bareRows()-covered {
			if at != m.jobList.Index() {
				m.bareRefused = false
			}
			m.jobList.Select(at)
		}
	}
}

// barePage lays a list page out: rows drawn from top, and the foot message,
// whose first row is the last row of the sheet and whose other rows take
// list rows from the bottom.
func (m *ActionMenuModel) barePage(sheet bareSheet, n int, row func(i int) string, sel int, foot []string, footStyle lipgloss.Style, keys string) string {
	rows := m.bareRows()
	covered := max(len(foot)-1, 0)
	top := m.bareWindow(sel, covered)
	pages := (n + rows - 1) / rows
	if pages > 1 {
		sheet.right = DimStyle.Render(fmt.Sprintf("%d/%d", sel/rows+1, pages))
	}
	body := make([]string, 0, rows)
	for i := top; i < n && len(body) < rows-covered; i++ {
		body = append(body, row(i))
	}
	body = bareRows(body, rows-covered)
	sheet.keys = DimStyle.Render(keys)
	if len(foot) > 0 {
		body = append(body, bareStyled(foot[:len(foot)-1], footStyle)...)
		sheet.keys = footStyle.Render(foot[len(foot)-1])
	}
	sheet.body = body
	return sheet.render(m.width, m.height)
}

// bareView draws the bare form: exactly width x height cells.
func (m *ActionMenuModel) bareView() string {
	w, h := m.width, m.height
	switch m.mode {
	case menuConfirm:
		sheet := bareSheet{keys: DimStyle.Render("Enter confirm  Esc cancel")}
		question := bareWrap(m.confirmLabel+"?", w, max(h-1, 1))
		sheet.left = YellowBoldStyle.Render(question[0])
		sheet.body = bareStyled(question[1:], YellowBoldStyle)
		if m.confirmDetail != "" {
			sheet.body = append(sheet.body, bareStyled(bareWrap(m.confirmDetail, w, len(m.confirmDetail)+1), YellowStyle)...)
		}
		return sheet.render(w, h)

	case menuJobSelect:
		if m.jobConfirm {
			return m.bareConfirmView()
		}
		label := m.pendingLabel
		for i := range m.items {
			if m.items[i].Chord == m.pendingAction && m.items[i].HintLabel != "" {
				label = m.items[i].HintLabel
			}
		}
		sheet := bareSheet{left: TitleStyle.Render(label) + DimStyle.Render(fmt.Sprintf(": pick (%d)", len(m.filtered)))}
		items := m.jobList.Items()
		sel := m.jobList.Index()
		row := func(i int) string {
			ji, ok := items[i].(menuJobItem)
			if !ok {
				return ""
			}
			icon := StatusIcon(string(ji.job.Status))
			title := truncateString(ji.job.Title, max(w-ansi.StringWidth(icon)-1, 1))
			if i == sel {
				return bareMenuSelected().Render(padToWidth(icon+" "+title, w))
			}
			return lipgloss.NewStyle().Foreground(StatusColor(string(ji.job.Status))).Render(icon) + " " + title
		}
		return m.barePage(sheet, len(items), row, sel, m.bareFoot(m.bareRefusal()), YellowStyle, "Enter: Select  Esc: Back")
	}

	acts, _, sel := m.bareActions()
	sheet := bareSheet{left: TitleStyle.Render("Menu")}
	if len(acts) > 0 {
		sheet.left += DimStyle.Render(" · " + acts[sel].action.Category)
	}
	row := func(i int) string {
		mi := acts[i]
		chord := fmt.Sprintf("%-3s ", mi.action.Chord)
		label := truncateString(mi.action.Label, max(w-len(chord), 1))
		switch {
		case i == sel:
			return bareMenuSelected().Render(padToWidth(chord+label, w))
		case m.bareMainReason(mi) != "":
			return DimStyle.Render(chord + label)
		}
		return lipgloss.NewStyle().Foreground(ColorCyan).Render(chord) + label
	}
	return m.barePage(sheet, len(acts), row, sel, m.bareFoot(m.bareMainFoot()), DimStyle, "Enter  PgUp/PgDn  Esc")
}

// bareConfirmView is the armed confirm's page.
func (m *ActionMenuModel) bareConfirmView() string {
	w, h := m.width, m.height
	sheet := bareSheet{left: YellowBoldStyle.Render(truncateString(m.pendingLabel+"?", w))}
	if ji, ok := m.jobList.SelectedItem().(menuJobItem); ok {
		icon := StatusIcon(string(ji.job.Status))
		sheet.body = []string{icon + " " + truncateString(ji.job.Title, max(w-ansi.StringWidth(icon)-1, 1))}
	}
	if !m.bareConfirmFits() {
		// Armed in a larger terminal: what it deletes cannot be shown
		// whole here, so it is not shown cut, and Enter does not confirm.
		sheet.keys = DimStyle.Render("Esc cancel")
		sheet.errorUnder(m.bareConfirmRefusal(), YellowStyle, w, h)
		return sheet.render(w, h)
	}
	sheet.keys = DimStyle.Render("Enter confirm  Esc cancel")
	sheet.body = append(sheet.body, bareStyled(m.bareConfirmDetailRows(), YellowStyle)...)
	return sheet.render(w, h)
}

// bareSetSize sizes the lists for the bare form: the job list pages by the
// rows a bare page draws.
func (m *ActionMenuModel) bareSetSize() {
	m.mainList.SetSize(m.width, m.bareRows())
	m.jobList.SetSize(m.width, m.bareRows())
}
