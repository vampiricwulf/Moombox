package tui

import (
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The bare form of the archive import, drawn below the normal layout
// (overlay_bare.go): a title row, the body, the keys.
//
//	pick       "Import .zip" and the end of the folder's path; the picker's
//	           rows without the size column, each cut to the width
//	meta       the file's name in the title, cut in its middle so that its
//	           start and its ".zip" both show; the two fields on the first
//	           two body rows, always; under them the server's refusal
//	importing  the spinner and the file, and that Esc is off
//
// A refusal wraps under the fields. One too long for the rows there takes
// the keys row and then the title before any of it is cut; it never covers
// a field.

// bareImportLabelW is the width of a meta field's label, "> Title:   ".
const bareImportLabelW = 11

// bareImportEmpty is the empty-folder text, as newImportPicker words it.
const bareImportEmpty = "No .zip archives here (← goes up a folder)"

// bare reports whether the dialog draws its bare form: its own size is below
// the normal layout. Derived on every call, never stored.
func (m *ImportDialogModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// pickerRows is the picker's height at the dialog's size: every row but the
// title and the keys in the bare form, the box's in the normal one.
func (m *ImportDialogModel) pickerRows() int {
	if m.bare() {
		return max(m.height-2, 1)
	}
	return max(1, m.height-8)
}

// bareSetSize is SetSize for a change into, out of or within the bare form.
// It sets the two pieces of component state the forms share: the text
// input's width (none in the normal form) and the picker's height.
func (m *ImportDialogModel) bareSetSize() {
	if m.bare() {
		bareInputWidth(&m.textInput, m.width-bareImportLabelW-1)
	} else {
		bareInputWidth(&m.textInput, 0)
	}
	rows := m.pickerRows()
	if m.picker.Height() == rows {
		return
	}
	m.picker.SetHeight(rows)
	// The picker's window follows a new height only on a size message
	// (with AutoHeight off that is all the message does): it then runs
	// from the same first row to the new height. It never moves to keep
	// the cursor inside; pickerShowCursor does that, for a picker that is
	// on screen (drawing one reads its files, and a closed dialog's picker
	// is replaced by the next Open).
	m.picker, _ = m.picker.Update(tea.WindowSizeMsg{})
	if m.visible {
		m.pickerShowCursor()
	}
}

// pickerCursorShown reports whether the picker's cursor row is in its
// window, and whether the folder is empty (or not read yet), where there is
// no cursor to show.
func (m *ImportDialogModel) pickerCursorShown() (shown, empty bool) {
	view := m.picker.View()
	if h := m.picker.Height(); view == m.picker.Styles.EmptyDirectory.Height(h).MaxHeight(h).String() {
		return false, true
	}
	for _, line := range strings.Split(ansi.Strip(view), "\n") {
		if strings.HasPrefix(line, m.picker.Cursor) {
			return true, false
		}
	}
	return false, false
}

// pickerShowCursor brings the picker's cursor row back into its window after
// the window shrank above it. The picker's cursor and window are private:
// Up moves the cursor up to the window's last row, and as many Downs move it
// back with the window following. A cursor that many Ups do not bring back
// goes to the top of the folder.
func (m *ImportDialogModel) pickerShowCursor() {
	const limit = 1000
	ups := 0
	for ; ups < limit; ups++ {
		shown, empty := m.pickerCursorShown()
		if empty {
			return
		}
		if shown {
			break
		}
		m.picker, _ = m.picker.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	if ups == limit {
		m.picker, _ = m.picker.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
		return
	}
	for range ups {
		m.picker, _ = m.picker.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
}

// bareField is one meta field's row: the label, and the input when it is
// the focused one.
func (m *ImportDialogModel) bareField(label, value string, focused bool) string {
	if focused {
		return bareFocus("> "+label) + m.textInput.View()
	}
	return "  " + DimStyle.Render(label) + renderInactiveInput(value, m.width-bareImportLabelW, ColorGray)
}

// bareView draws the bare form: exactly width x height cells.
func (m *ImportDialogModel) bareView() string {
	w, h := m.width, m.height
	var sheet bareSheet
	switch m.step {
	case importStepPick:
		const title = "Import .zip"
		sheet.left = TitleStyle.Render(title)
		if room := w - len(title) - 2; room >= 6 {
			sheet.left += "  " + DimStyle.Render(bareHeadCut(m.picker.CurrentDirectory, room))
		}
		keys := "↑↓  ←:Up  →/Enter:Open  Esc:Cancel"
		if ansi.StringWidth(keys) > w {
			keys = "←:Up →/Enter:Open Esc"
		}
		sheet.keys = DimStyle.Render(keys)
		if _, empty := m.pickerCursorShown(); empty {
			sheet.body = bareStyled(bareWrap(bareImportEmpty, w, max(h-2, 1)), DimStyle)
			break
		}
		// A copy without the size column: the name is what tells two
		// archives apart, and a row is cut at its end.
		picker := m.picker
		picker.ShowSize = false
		for _, row := range strings.Split(picker.View(), "\n") {
			sheet.body = append(sheet.body, truncateWidth(row, w, ""))
		}

	case importStepMeta:
		const head = "Import: "
		sheet.left = TitleStyle.Render(head) + bareMiddleCut(filepath.Base(m.filePath), max(w-len(head), 1))
		keys := "Tab: Field  Enter: Import  Esc: Back"
		if len(keys) > w {
			keys = "Tab Enter:Import Esc:Back"
		}
		sheet.keys = DimStyle.Render(keys)
		sheet.body = []string{
			m.bareField("Title:   ", m.title, m.metaFocus == 0),
			m.bareField("Channel: ", m.channel, m.metaFocus == 1),
		}
		if m.errorMsg != "" {
			sheet.errorUnder(m.errorMsg, ErrorStyle, w, h)
		} else {
			sheet.noteUnder("Blank keeps the archive's own.", DimStyle, w, h)
		}

	case importStepImporting:
		sheet.left = TitleStyle.Render("Importing")
		sheet.keys = DimStyle.Render("Working. Esc is off.")
		spin := m.spinner.View() + " "
		sheet.body = []string{spin + bareMiddleCut(filepath.Base(m.filePath), max(w-ansi.StringWidth(spin), 1))}
	}
	return sheet.render(w, h)
}
