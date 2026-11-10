package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// The bare form of Settings, drawn below the normal layout (overlay_bare.go;
// owner ruling R-C13: down to 30x6). No box, no tab strip, no button row.
// Every page is a bareSheet:
//
//	fields        "Settings n/13 Name" and the unsaved mark; the fields, one
//	              a row, the focused one on two (its label, then its value
//	              or its input at the full width) and a third for its
//	              preview; the focused field's explanation, one row at the
//	              smallest size and more where there is room; the keys
//	Channels      the list, one channel a row; the removal prompt takes the
//	              whole screen, one fact a row, its counts never cut
//	channel       the editor: the fields as above, in a window that follows
//	              the focus (settings_bare_lists.go)
//	Integrations  the list, a webhook shown by its id and never by a
//	              character of its token; the editor in a window that
//	              follows the focus
//	password      the sub-editor takes the whole screen: the dotted inputs,
//	              then the message, which never covers an input
//	restart       six rows that take the whole screen
//
// A save or validation error, a test result and the refusals are drawn
// under the rows of the page, which give way for them down to the focused
// item; past that the title and then the keys give way before any of the
// message is cut (bareSheet.errorUnder). "Save changes?" takes the keys row.
//
// Nothing here is stored but bareNote. The window of rows is derived from
// the focus on every draw, and the shared text input is drawn from a copy
// (bareInput), so the normal form finds its scroll offset and its input as
// it left them.
//
// Keys are the normal form's, with three differences (bareKey): Shift+Down
// does nothing on a field page, since the buttons it reaches are not drawn;
// button focus held from the normal form is dropped; and Ctrl+O on the
// FFmpeg path is refused, since the installer needs the normal layout
// (owner ruling R-C10).

// bareInstallerRefusal answers Ctrl+O on the FFmpeg path below the normal
// layout. 21 cells: whole at the compact floor.
const bareInstallerRefusal = "Installer needs 60x20"

// bareSavePrompt is "Save changes?" on the keys row. 27 cells.
const bareSavePrompt = "Save changes? Y/N  Esc:back"

// bareOuterSpaceWarning is config.PasswordOuterSpaceWarning in the words
// that fit a row of the compact floor.
const bareOuterSpaceWarning = "Space at start/end is kept"

// bare reports whether Settings draws its bare form: its own size is below
// the normal layout. Derived on every call, never stored.
func (m *SettingsModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// bareKey is HandleKey for the bare form.
func (m *SettingsModel) bareKey(key string) string {
	m.bareNote = ""
	if m.showRestartOverlay || m.closeConfirm {
		return m.handleKey(key)
	}
	// The buttons are not drawn: focus left on them from the normal form
	// would let Enter save or close unseen.
	m.bareDropButtonFocus()
	if m.isFieldSection() && m.secMode == securityStatus {
		sec := sections[m.sectionIndex]
		switch key {
		case "shift+down":
			return ""
		case keyCtrlO:
			if sec.name == "Paths" && sec.fields[m.fieldIndex].key == "ffmpeg_path" {
				// Before the save prompt: answering it would save, close and
				// only then find the installer cannot be drawn.
				m.bareNote, m.bareNoteStyle = bareInstallerRefusal, YellowStyle
				return ""
			}
		}
	}
	editing := m.secMode != securityStatus
	action := m.handleKey(key)
	if editing && m.secMode == securityStatus && key != keyEsc && m.secMessage != "" {
		// The password editor closed on a result. The normal form shows it
		// under the Network fields; here it is on the page until the next
		// key.
		m.bareNote, m.bareNoteStyle = m.secMessage, lipgloss.NewStyle().Foreground(m.secMessageColor)
	}
	return action
}

// bareDropButtonFocus takes the focus off the action buttons, which the
// bare form does not draw, and gives it back to the field.
func (m *SettingsModel) bareDropButtonFocus() {
	if m.buttonFocus >= 0 {
		m.buttonFocus = -1
		m.updateTextInputForField()
	}
}

// bareMouse is HandleMouse for the bare form: a wheel notch moves the
// cursor one row, as it does in the normal form; a click does nothing.
func (m *SettingsModel) bareMouse(msg tea.MouseMsg) {
	if m.showRestartOverlay || m.closeConfirm {
		return
	}
	if isScrollUp(msg) || isScrollDown(msg) {
		m.bareNote = ""
		m.bareDropButtonFocus()
		m.handleMouseScroll(isScrollDown(msg))
	}
}

// bareView draws the bare form: exactly width x height cells.
func (m *SettingsModel) bareView() string {
	if m.showRestartOverlay {
		return m.bareRestart()
	}
	sec := sections[m.sectionIndex]
	switch sec.name {
	case "Network":
		if m.secMode != securityStatus {
			return m.bareSecurity()
		}
	case "Channels":
		switch {
		case m.channelMode == "edit":
			return m.bareChannelEdit()
		case m.channelDeleteConf:
			return m.bareChannelRemoval()
		}
		return m.bareChannels()
	case "Integrations":
		if m.notifMode == "edit" {
			return m.bareNotifEdit()
		}
		return m.bareNotifications()
	}
	return m.bareFields(sec)
}

// ---- shared pieces ----

// bareInput draws a copy of a text input in width+1 cells, the last one the
// cursor's, with the cursor on screen. The input itself is not touched: the
// normal form sets its width on every draw and would find a narrow layout
// left behind.
func bareInput(ti textinput.Model, width int) string {
	value, pos := ti.Value(), ti.Position()
	ti.SetWidth(max(width, 1))
	ti.Reset() // forget the window the wider form scrolled to
	ti.SetValue(value)
	ti.SetCursor(pos)
	return ti.View()
}

// bareKeyRow is a keys row of the parts that fit w cells, two spaces apart
// where that fits and one where it does not. Parts are dropped from the
// end; a part is never cut.
func bareKeyRow(w int, parts ...string) string {
	for ; len(parts) > 1; parts = parts[:len(parts)-1] {
		if row := strings.Join(parts, "  "); ansi.StringWidth(row) <= w {
			return DimStyle.Render(row)
		}
		if row := strings.Join(parts, " "); ansi.StringWidth(row) <= w {
			return DimStyle.Render(row)
		}
	}
	return DimStyle.Render(truncateString(strings.Join(parts, ""), w))
}

// bareWindow picks the items [start, end) drawn in rows rows, heights[i]
// rows each: the focused one always, then as many before it as fit, then as
// many after it. Derived from the focus alone, so nothing is stored.
func bareWindow(heights []int, focus, rows int) (start, end int) {
	if len(heights) == 0 {
		return 0, 0
	}
	focus = min(max(focus, 0), len(heights)-1)
	start, end = focus, focus+1
	used := heights[focus]
	for start > 0 && used+heights[start-1] <= rows {
		start--
		used += heights[start]
	}
	for end < len(heights) && used+heights[end] <= rows {
		used += heights[end]
		end++
	}
	return start, end
}

// bareHelpRows is how many rows a page gives the focused item's explanation:
// what the longest one of the page needs at width w, so the window does not
// resize as the focus moves, and at most limit. At least one.
func bareHelpRows(helps []string, w, limit int) int {
	rows := 1
	for _, help := range helps {
		if rows >= limit {
			break
		}
		if help != "" {
			rows = max(rows, len(bareWrap(help, w, limit)))
		}
	}
	return max(min(rows, limit), 1)
}

// barePage lays a page out on sheet, which holds its header and keys:
// items (the rows each one draws) in a window that follows focus, then tail
// (the explanation, a count), then msg. The window fills the rows the tail
// and the message leave. A message that needs more makes the tail and then
// the window give way, down to the focused item; past that it is
// bareSheet.errorUnder's: the title, the keys and the header give way before
// any of it is cut. focus < 0: no item is focused and items are drawn from
// the first.
func (m *SettingsModel) barePage(sheet bareSheet, items [][]string, focus int, tail []string, msg string, style lipgloss.Style) string {
	w, h := m.width, m.height
	avail := max(h-2, 1)
	need := 0
	if strings.TrimSpace(msg) != "" {
		need = len(bareWrap(msg, w, len(msg)+1))
	}
	heights := make([]int, len(items))
	for i := range items {
		heights[i] = len(items[i])
	}
	minWin := 0
	if focus >= 0 && focus < len(items) {
		minWin = heights[focus]
	}
	if need > 0 {
		// Rows the tail keeps blank are the message's first.
		for len(tail) > 0 && strings.TrimSpace(ansi.Strip(tail[len(tail)-1])) == "" {
			tail = tail[:len(tail)-1]
		}
	}
	win := avail - need - len(tail)
	if win < minWin {
		tail = tail[:max(len(tail)-(minWin-win), 0)]
		win = minWin
	}
	win = min(win, avail)

	var rows []string
	start, end := bareWindow(heights, focus, win)
	for i := start; i < end; i++ {
		rows = append(rows, items[i]...)
	}
	sheet.body = append(bareRows(rows, win), tail...)
	sheet.errorUnder(msg, style, w, h)
	return sheet.render(w, h)
}

// bareShell is the sheet of a page of the shell: "Settings n/13 Name" (the
// word gives way before the section's name does) and the unsaved mark, and
// keys as the keys row. "Save changes?" replaces the keys.
func (m *SettingsModel) bareShell(keys ...string) bareSheet {
	w := m.width
	right := ""
	if m.dirty {
		right = "*"
		if w >= 40 {
			right = "*unsaved"
		}
	}
	name := fmt.Sprintf("%d/%d %s", m.sectionIndex+1, len(sections), sections[m.sectionIndex].name)
	left := "Settings " + name
	if room := w - ansi.StringWidth(right) - min(ansi.StringWidth(right), 1); ansi.StringWidth(left) > room {
		left = name
	}
	sheet := bareSheet{
		left:  TitleStyle.Render(left),
		right: YellowStyle.Render(right),
		keys:  bareKeyRow(w, keys...),
	}
	if m.closeConfirm {
		sheet.keys = YellowStyle.Render(bareSavePrompt)
	}
	return sheet
}

// bareEscKey is the Esc entry of a shell page's keys: with unsaved edits Esc
// leads to the save question, and the buttons that were the other way to
// save are not drawn.
func (m *SettingsModel) bareEscKey() string {
	if m.dirty {
		return "Esc:save/close"
	}
	return "Esc:close"
}

// bareMessage is the one message a shell page shows: the save or validation
// error, else a refusal or the password editor's result, else a notice (a
// test notification on its way or delivered).
func (m *SettingsModel) bareMessage() (string, lipgloss.Style) {
	switch {
	case m.status == saveError && m.errorMsg != "":
		return m.errorMsg, ErrorStyle
	case m.bareNote != "":
		return m.bareNote, m.bareNoteStyle
	case m.status == saveNotice && m.errorMsg != "":
		return m.errorMsg, lipgloss.NewStyle().Foreground(ColorGreen)
	}
	return "", lipgloss.Style{}
}

// bareEditorMessage is the one message the channel and the webhook editor
// show: the validation error. A notice left by the list (a test result) is
// not theirs and would cost them a row.
func (m *SettingsModel) bareEditorMessage() (string, lipgloss.Style) {
	if m.status == saveError && m.errorMsg != "" {
		return m.errorMsg, ErrorStyle
	}
	return "", lipgloss.Style{}
}

// bareField is one field of a field page or of the channel editor.
type bareField struct {
	label   string
	ftype   fieldType
	options []string
	value   string
	// mark is the two cells ahead of the label, styled: the cursor and the
	// changed mark.
	mark string
	// preview is the row under the focused field, "" for none.
	preview string
}

// bareChoice is a toggle's or a cycle's selected option as an unfocused row
// shows it: the selected half only. "Yes / [No]" beside a label does not fit
// the compact floor.
func bareChoice(options []string, value string) string {
	for _, opt := range options {
		if strings.EqualFold(opt, value) {
			return "[" + opt + "]"
		}
	}
	return "[" + value + "]"
}

// bareFieldRows draws a field in the width. Unfocused it is one row, the
// label and the value, the label padded to col cells and cut with an
// ellipsis only where the two do not fit. Focused it is two: the label,
// then the choice in full or the input at the full width, and a third for a
// preview.
func (m *SettingsModel) bareFieldRows(f bareField, focused bool, col int) []string {
	w := m.width
	room := max(w-2, 1)
	if focused {
		label := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(truncateString(f.label, room))
		rows := []string{f.mark + label}
		switch f.ftype {
		case fieldToggle:
			rows = append(rows, "  "+renderToggle(f.value))
		case fieldCycle:
			rows = append(rows, "  "+renderCycleOptions(f.options, f.value, true, max(room, 5)))
		default:
			rows = append(rows, "  "+bareInput(m.textInput, w-3))
		}
		if f.preview != "" {
			rows = append(rows, "  "+DimStyle.Render(truncateString(f.preview, room)))
		}
		return rows
	}

	value, floor := f.value, 6
	if f.ftype == fieldToggle || f.ftype == fieldCycle {
		value, floor = bareChoice(f.options, f.value), 12
	}
	floor = min(ansi.StringWidth(value), floor)
	label := truncateString(f.label, max(room-2-floor, 1))
	if lw := ansi.StringWidth(label); lw < col && col <= room-2-floor {
		label = padRight(label, col)
	}
	value = truncateString(value, max(room-ansi.StringWidth(label)-2, 1))
	return []string{f.mark + lipgloss.NewStyle().Foreground(ColorGray).Render(label) + "  " + value}
}

// bareLabelCol is the label column of a page whose longest label is longest
// cells wide: the labels line up while that leaves the values half the row.
func (m *SettingsModel) bareLabelCol(longest int) int {
	return min(longest, max(m.width-2, 1)/2-1)
}

// ---- the field sections ----

// bareFieldMark is the two cells ahead of a settings field: the cursor and
// the changed mark, coloured as the normal form colours them (yellow for a
// change that needs a restart).
func bareFieldMark(selected, changed, restart bool) string {
	switch {
	case changed && selected:
		if restart {
			return YellowStyle.Render("*>")
		}
		return lipgloss.NewStyle().Foreground(ColorCyan).Render("*>")
	case changed:
		if restart {
			return YellowFaintStyle.Render("* ")
		}
		return lipgloss.NewStyle().Foreground(ColorGreen).Faint(true).Render("* ")
	case selected:
		return lipgloss.NewStyle().Foreground(ColorCyan).Render("> ")
	}
	return "  "
}

// barePasswordLine is the Network page's password row: whether one is set
// and the keys that open its editor. They work from a toggle row only: on a
// text or number row they type themselves.
func (m *SettingsModel) barePasswordLine(field fieldDef) string {
	state, keys := "pw:none", " `:set"
	if m.hasPassword() {
		state, keys = "pw:set", " `:change ~:remove"
	}
	if field.ftype == fieldText || field.ftype == fieldNumber {
		keys = "(keys: toggle row)"
	}
	return DimStyle.Render(truncateString(state+" "+keys, m.width))
}

// bareFields draws a field section.
func (m *SettingsModel) bareFields(sec settingsSection) string {
	w, h := m.width, m.height
	if len(sec.fields) == 0 {
		sheet := m.bareShell("Tab:section", m.bareEscKey())
		sheet.body = []string{DimStyle.Render("No settings in this section")}
		return sheet.render(w, h)
	}
	focus := min(max(m.fieldIndex, 0), len(sec.fields)-1)
	focused := sec.fields[focus]

	longest := 0
	helps := make([]string, len(sec.fields))
	for i, fd := range sec.fields {
		longest = max(longest, ansi.StringWidth(fd.label))
		helps[i] = fd.help
	}
	col := m.bareLabelCol(longest)
	items := make([][]string, len(sec.fields))
	for i, fd := range sec.fields {
		changed := m.values[fd.key] != m.originalValues[fd.key]
		f := bareField{
			label: fd.label, ftype: fd.ftype, options: fd.options, value: m.values[fd.key],
			mark: bareFieldMark(i == focus, changed, changed && restartRequiredKeys[fd.key]),
		}
		if fd.ftype == fieldToggle {
			f.options = []string{"Yes", "No"}
		}
		if i == focus && fd.previewFn != nil {
			f.preview = fd.previewFn(m.values[fd.key])
		}
		items[i] = m.bareFieldRows(f, i == focus, col)
	}

	keys := []string{"Tab:section", m.bareEscKey()}
	if focused.ftype == fieldToggle || focused.ftype == fieldCycle || (focused.ftype == fieldNumber && len(focused.options) > 0) {
		keys = append(keys, "←→:change")
	}
	keys = append(keys, "↑↓:field", "Sh+←→:section")
	sheet := m.bareShell(keys...)

	// The password row of the Network page: a row of its own where the
	// terminal has one to give, the keys row where it has not.
	var tail []string
	if sec.name == "Network" {
		if line := m.barePasswordLine(focused); h >= 8 {
			tail = []string{line}
		} else if !m.closeConfirm {
			sheet.keys = line
		}
	}
	// The explanation: under the fields, which keep three rows (a neighbour
	// and the focused pair).
	helpRows := bareHelpRows(helps, w, max(h-2-len(tail)-3, 1))
	help := make([]string, helpRows)
	if focused.help != "" {
		help = bareRows(bareStyled(bareWrap(focused.help, w, helpRows), DimStyle), helpRows)
	}
	tail = append(tail, help...)

	msg, style := m.bareMessage()
	return m.barePage(sheet, items, focus, tail, msg, style)
}

// ---- the prompts ----

// bareRestart draws the restart prompt: the whole screen, six rows at the
// compact floor. It follows a save and a password removal that reset the
// network access, so it does not say "Saved".
func (m *SettingsModel) bareRestart() string {
	w, h := m.width, m.height
	body := bareWrap("Some changes apply only after a restart.", w, h)
	if 1+len(body)+1+2 <= h {
		body = append(body, "")
	}
	body = append(body,
		lipgloss.NewStyle().Foreground(ColorCyan).Render("Enter: restart now"),
		DimStyle.Render("Esc: close, restart later"))
	return bareSheet{left: YellowBoldStyle.Render("Restart needed."), body: body}.render(w, h)
}

// ---- the password sub-editor ----

// bareSecurity draws the password sub-editor: the whole screen. The inputs
// stay dotted. The message is under them and never covers one: it does not
// go away on the next key, as a save error does.
func (m *SettingsModel) bareSecurity() string {
	w, h := m.width, m.height
	const labelW = 9
	focusedRow := func(label string) string {
		return lipgloss.NewStyle().Foreground(ColorCyan).Render("> ") +
			lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(padRight(label, labelW)) +
			bareInput(m.textInput, w-2-labelW-1)
	}
	msg, style := m.secMessage, lipgloss.NewStyle().Foreground(m.secMessageColor)

	var sheet bareSheet
	if m.secMode == securityRemove {
		sheet.left = lipgloss.NewStyle().Foreground(ColorRed).Bold(true).Render("Remove password")
		sheet.keys = bareKeyRow(w, "Enter:confirm", "Esc:cancel")
		external := false
		if m.configStore != nil {
			m.configStore.Read(func(c *config.MoomboxConfig) {
				external = config.IsExternalAccess(c.Network.NetworkAccess)
			})
		}
		if external {
			// It changes who can reach the dashboard: whole, never cut.
			sheet.body = bareStyled(bareWrap("Network access will be reset to localhost.", w, h), YellowStyle)
		}
		sheet.body = append(sheet.body, focusedRow("Current"))
	} else {
		title := "Set password"
		type pwField struct{ label, value string }
		var fields []pwField
		if m.hasPassword() {
			title = "Change password"
			fields = append(fields, pwField{"Current", m.secCurrentPw})
		}
		fields = append(fields, pwField{"New", m.secNewPw}, pwField{"Confirm", m.secConfirmPw})
		sheet.left = TitleStyle.Render(title)
		sheet.keys = bareKeyRow(w, "Enter:save", "Esc:cancel")
		for i, f := range fields {
			if i == m.secFieldIndex {
				sheet.body = append(sheet.body, focusedRow(f.label))
				continue
			}
			dots := truncateString(renderPasswordDots(f.value), max(w-2-labelW, 1))
			sheet.body = append(sheet.body, "  "+DimStyle.Render(padRight(f.label, labelW))+dots)
		}
		if msg == "" && config.PasswordHasOuterSpace(m.secNewPw) {
			// A warning, not a refusal: it gives way to a message.
			msg, style = bareOuterSpaceWarning, YellowStyle
		}
	}
	sheet.errorUnder(msg, style, w, h)
	return sheet.render(w, h)
}
