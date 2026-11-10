package tui

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The bare form of the trim dialog, drawn below the normal layout
// (overlay_bare.go): a title row, the body, the keys.
//
//	times     the recording's length, the two fields, then the format hint
//	          or the error
//	confirm   the range and the resulting length, the estimated size, and
//	          "Re-encodes into a new file." whole; a failed create's error
//	          under them
//	progress  the percentage in the title, the bar, the elapsed time; the
//	          keys row says Esc closes and the encode keeps running
//	delete    the trims in a window that always holds the cursor, and one
//	          message under it: the error, else "Deleting...", else the
//	          armed "Enter again deletes this trim", whole
//
// An error never covers a field or the selected trim. One a row too long for
// the rows under the fields takes the title row; longer still it takes the
// keys row and the title before any of it is cut.

// bareTrimArmed is the armed delete's line. It is 29 cells: whole at the
// compact floor.
const bareTrimArmed = "Enter again deletes this trim"

// bare reports whether the dialog draws its bare form: its own size is below
// the normal layout. Derived on every call, never stored.
func (m *TrimDialogModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// bareSetSize is SetSize for a change into, out of or within the bare form:
// the width of the text input the two forms share. renderTimeInputPair
// draws it on a copy w-12 cells wide; the input itself has no width in the
// normal form.
func (m *TrimDialogModel) bareSetSize() {
	if m.bare() {
		bareInputWidth(&m.textInput, m.width-12)
	} else {
		bareInputWidth(&m.textInput, 0)
	}
}

// bareTrimRow is one trim of the delete list in room cells: the range, then
// the length and the size where they fit. The range is never what is cut.
func bareTrimRow(tr TrimInfo, room int) string {
	row := formatTimeSeconds(tr.StartTime) + "-" + formatTimeSeconds(tr.EndTime)
	more := []string{formatDuration(time.Duration(tr.Duration * float64(time.Second)))}
	if tr.FileSize > 0 {
		more = append(more, formatFileSize(tr.FileSize))
	}
	for _, part := range more {
		if ansi.StringWidth(row)+1+ansi.StringWidth(part) > room {
			break
		}
		row += " " + part
	}
	return truncateString(row, room)
}

// bareView draws the bare form: exactly width x height cells.
func (m *TrimDialogModel) bareView() string {
	if m.mode == TrimModeDelete {
		return m.bareDeleteView()
	}
	w, h := m.width, m.height
	var sheet bareSheet
	keys := func(full, short string) {
		if ansi.StringWidth(full) > w {
			full = short
		}
		sheet.keys = DimStyle.Render(full)
	}
	switch m.createStep {
	case trimStepTimes:
		sheet.left = TitleStyle.Render("Trim")
		if m.jobTitle != "" {
			sheet.left = TitleStyle.Render("Trim: ") + truncateString(m.jobTitle, max(w-6, 1))
		}
		keys("Tab Enter:Next M:Delete mode Esc:Cancel", "Tab Enter M:Del mode Esc")
		if m.lengthSeconds > 0 {
			sheet.body = append(sheet.body, DimStyle.Render("Length "+formatTimeSeconds(m.lengthSeconds)))
		}
		sheet.body = append(sheet.body, renderTimeInputPair(m.startTimeInput, m.endTimeInput, m.activeField, m.textInput, w, ColorCyan)...)
		if m.errorMsg != "" {
			sheet.errorUnder(m.errorMsg, ErrorStyle, w, h)
		} else {
			sheet.noteUnder("H:MM:SS, MM:SS or seconds", DimStyle, w, h)
		}

	case trimStepConfirm:
		sheet.left = TitleStyle.Render("Create trim?")
		keys("Enter: Create  M: Delete mode  Esc: Back", "Enter:Create M:Del mode Esc")
		length := m.parsedEnd - m.parsedStart
		sheet.body = append(sheet.body, lipgloss.NewStyle().Foreground(ColorGreen).Render(truncateString(fmt.Sprintf("%s - %s  (%s)",
			formatTimeSeconds(m.parsedStart), formatTimeSeconds(m.parsedEnd),
			formatDuration(time.Duration(length*float64(time.Second)))), w)))
		var facts []string
		if m.fileSize > 0 && m.lengthSeconds > 0 {
			est := int64(float64(m.fileSize) / m.lengthSeconds * length)
			facts = append(facts, "About "+formatFileSize(est))
		}
		facts = append(facts, YellowStyle.Render("Re-encodes into a new file."))
		// An error too long to sit under the facts (errorUnder lets the title
		// give way for one row) takes their rows: only the range is kept, so
		// the error is one block with room to wrap.
		if m.errorMsg == "" || len(bareWrap(m.errorMsg, w, len(m.errorMsg)+1)) <= h-2-len(facts) {
			sheet.body = append(sheet.body, facts...)
		}
		sheet.errorUnder(m.errorMsg, ErrorStyle, w, h)

	case trimStepProgress:
		sheet.left = m.spinner.View() + " " + TitleStyle.Render("Creating trim")
		sheet.right = lipgloss.NewStyle().Foreground(ColorCyan).Render(fmt.Sprintf("%.1f%%", m.progressPercent))
		sheet.keys = DimStyle.Render("Esc: keep running, close")
		// Set on every draw, as the normal form does: nothing outlives it.
		m.progressBar.SetWidth(w)
		progress.WithColors(ColorDownloading, ColorMuxing)(&m.progressBar)
		sheet.body = []string{
			m.progressBar.ViewAs(m.progressPercent / 100),
			DimStyle.Render(fmt.Sprintf("Elapsed %s", m.progressElapsed.Truncate(time.Second))),
		}
		sheet.errorUnder(m.errorMsg, ErrorStyle, w, h)
	}
	return sheet.render(w, h)
}

// bareDeleteView draws delete mode's bare form.
func (m *TrimDialogModel) bareDeleteView() string {
	w, h := m.width, m.height
	magenta := lipgloss.NewStyle().Foreground(ColorCookies)
	sheet := bareSheet{left: magenta.Bold(true).Render("Delete trim")}
	n := len(m.trims)
	if n == 0 {
		sheet.body = []string{DimStyle.Render("No trims to delete")}
		sheet.keys = DimStyle.Render("M/Esc: Create mode")
		return sheet.render(w, h)
	}
	sel := min(max(m.selectedTrimIdx, 0), n-1)
	sheet.right = DimStyle.Render(fmt.Sprintf("%d/%d", sel+1, n))
	sheet.keys = DimStyle.Render("↑↓ Enter:Delete M/Esc:New")

	// The one message under the list. An error wraps, and leaves the list
	// at least the cursor's row; one too long for the rows under that row
	// takes the keys row and then the header before any of it is cut.
	body := max(h-2, 1)
	if m.errorMsg != "" && len(bareWrap(m.errorMsg, w, len(m.errorMsg)+1)) > body-1 {
		tr := m.trims[sel]
		sheet.body = []string{magenta.Render("> " + bareTrimRow(tr, max(w-2, 1)))}
		sheet.keep, sheet.msg, sheet.msgStyle = 1, m.errorMsg, ErrorStyle
		return sheet.render(w, h)
	}
	var msg []string
	switch {
	case m.errorMsg != "":
		msg = bareStyled(bareWrap(m.errorMsg, w, max(body-1, 1)), ErrorStyle)
	case m.loading:
		msg = []string{m.spinner.View() + " Deleting..."}
	case m.deleteConfirmID != "":
		msg = []string{YellowStyle.Render(truncateString(bareTrimArmed, w))}
	}

	// The window is derived from the cursor on every draw, so the selected
	// (and the armed) trim is always inside it.
	rows := max(body-len(msg), 1)
	top := min(max(sel-rows/2, 0), max(n-rows, 0))
	list := make([]string, 0, rows)
	for i := top; i < n && i < top+rows; i++ {
		tr := m.trims[i]
		mark, style := "  ", lipgloss.NewStyle()
		if i == sel {
			mark, style = "> ", magenta
		}
		if m.deleteConfirmID == tr.ID {
			style = YellowStyle
		}
		list = append(list, style.Render(mark+bareTrimRow(tr, max(w-2, 1))))
	}
	sheet.body = append(bareRows(list, rows), msg...)
	return sheet.render(w, h)
}
