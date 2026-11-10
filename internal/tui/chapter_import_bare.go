package tui

import (
	"fmt"
	"math"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// The bare form of the chapter import, drawn below the normal layout
// (overlay_bare.go). It is the first overlay whose states need different
// sizes (bareNeed): editing needs 30x10, the confirm and the saving step
// 30x6. Below its state's size the overlay is kept under the notice.
//
//	editing  the job's title; the paste box (two rows, more in a taller
//	         terminal); the offset field; where the offset came from, whole
//	         (owner decision CK-2: a wrong offset shifts every chapter); the
//	         verdict (how many chapters, and how many a save replaces, or
//	         why the paste yields none); a save's refusal; then as many
//	         preview rows as are left
//	confirm  the question whole, with both counts and the offset, in place
//	         of everything else
//	saving   that the archive is being rewritten, and that closing does not
//	         stop it
//
// 30x10 is the editing step's worst case at 30 columns: the title, two paste
// rows, the offset, a two-row source note, the verdict, a two-row refusal
// and the keys. A parse error or a refusal longer than the rows under the
// note takes the keys row, then the title; it never covers the paste box,
// the offset field or the note.

// The size the editing step needs. The confirm and the saving step fit the
// compact floor.
const (
	bareChapterEditW = compactMinWidth
	bareChapterEditH = 10
)

// The paste box's placeholder: the normal form's, and one that fits a bare
// box 30 cells wide.
const (
	chapterPastePlaceholder     = "Paste a timestamp comment: 0:00 Intro, 12:34 First song, …"
	bareChapterPastePlaceholder = "Paste a timestamp comment"
	// The offset input's placeholder: the normal form's, and the one that
	// fits beside "Offset: " below bareOffsetShortBelow columns.
	chapterOffsetPlaceholder     = "0 (seconds or ±H:MM:SS)"
	bareChapterOffsetPlaceholder = "0 (s or ±H:MM:SS)"
	bareOffsetShortBelow         = 34
)

// bare reports whether the dialog draws its bare form: its own size is below
// the normal layout. Derived on every call, never stored.
func (m *ChapterImportDialogModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// bareNeed is the smallest terminal the overlay's current step is drawn bare
// in (overlayBare.now).
func (m *ChapterImportDialogModel) bareNeed() (w, h int) {
	if m.step == chapterImportStepEdit {
		return bareChapterEditW, bareChapterEditH
	}
	return compactMinWidth, compactMinHeight
}

// barePasteRows is the paste box's height in a bare terminal h rows tall:
// two rows, enough to see that a paste landed, and more from 13 rows up.
func barePasteRows(h int) int {
	return min(max((h-4)/3, 2), 8)
}

// bareSetSize is SetSize for the bare form: the paste box as wide as the
// terminal (its width counts its two-cell prompt), the offset input under
// its eight-cell prompt with a cell kept for the cursor, and a placeholder
// that fits.
func (m *ChapterImportDialogModel) bareSetSize() {
	m.text.Placeholder = bareChapterPastePlaceholder
	m.text.SetWidth(m.width)
	m.text.SetHeight(barePasteRows(m.height))
	m.offset.Placeholder = chapterOffsetPlaceholder
	if m.width < bareOffsetShortBelow {
		m.offset.Placeholder = bareChapterOffsetPlaceholder
	}
	bareInputWidth(&m.offset, m.width-9)
}

// bareLeave undoes, after the normal SetSize, what the bare form set on the
// components the two forms share.
func (m *ChapterImportDialogModel) bareLeave() {
	m.text.Placeholder = chapterPastePlaceholder
	m.offset.Placeholder = chapterOffsetPlaceholder
	bareInputWidth(&m.offset, m.offset.Width())
}

// bareTitle is the header of every step.
func (m *ChapterImportDialogModel) bareTitle() string {
	const head = "Chapters: "
	return TitleStyle.Render(head) + truncateString(m.title, max(m.width-len(head), 1))
}

// bareOffsetClock is the previewed offset as the field writes it, "" for
// none.
func (m *ChapterImportDialogModel) bareOffsetClock() string {
	if m.view == nil || m.view.OffsetSeconds == 0 {
		return ""
	}
	return utils.FormatChapterOffset(int64(math.Round(m.view.OffsetSeconds * 1000)))
}

// bareEdit lays the editing step out and reports how many rows it has for
// the preview's chapters (0 when the preview is not drawn).
func (m *ChapterImportDialogModel) bareEdit() (sheet bareSheet, previewRows int) {
	w, h := m.width, m.height
	sheet.left = m.bareTitle()
	keys := "Tab:Field  Ctrl+S:Save  Esc:Cancel"
	if len(keys) > w {
		keys = "Tab ^S:Save Esc"
	}
	sheet.keys = DimStyle.Render(keys)

	// The rows nothing may cover: the paste box, the offset, its source.
	paste := strings.Split(m.text.View(), "\n")
	sheet.body = append(bareRows(paste, m.text.Height()), m.offset.View())
	if note := offsetSourceNote(m.view); note != "" {
		sheet.body = append(sheet.body, bareStyled(bareWrap(note, w, len(note)+1), DimStyle)...)
	}

	// The verdict: one row, or an error that wraps.
	errText := ""
	switch {
	case m.previewErr != "":
		errText = m.previewErr
	case m.view == nil:
		sheet.body = append(sheet.body, DimStyle.Render("Reading the job's chapters…"))
	case m.text.Value() == "":
		sheet.body = append(sheet.body, DimStyle.Render(fmt.Sprintf("Current: %d chapters", len(m.view.Current))))
	case m.view.Error != "":
		errText = m.view.Error
	default:
		n, cur := len(m.view.Chapters), len(m.view.Current)
		verdict := fmt.Sprintf("%d chapters, replaces current %d", n, cur)
		if len(verdict) > w {
			verdict = fmt.Sprintf("%d new, replaces %d", n, cur)
		}
		if top := min(m.previewTop, max(n-1, 0)); top > 0 {
			if more := fmt.Sprintf("  ↑%d", top); len(verdict)+len(more)-2 <= w {
				verdict += more
			}
		}
		sheet.body = append(sheet.body, truncateString(verdict, w))
		if m.saveErr != "" {
			errText = "Not saved: " + m.saveErr
		}
	}

	room := h - 2 - len(sheet.body)
	if errText != "" {
		rows := bareWrap(errText, w, len(errText)+1)
		if len(rows) > room {
			sheet.keep, sheet.msg, sheet.msgStyle = len(sheet.body), errText, ErrorStyle
			return sheet, 0
		}
		sheet.body = append(sheet.body, bareStyled(rows, ErrorStyle)...)
		room -= len(rows)
	}
	if !m.previewDrawn() {
		return sheet, 0
	}
	if room < 2 && len(m.view.Chapters) > room {
		// One row for several chapters would be the "and n more" row with
		// no chapter above it.
		return sheet, 0
	}
	return sheet, max(room, 0)
}

// barePreviewMaxTop is previewMaxTop for the bare form.
func (m *ChapterImportDialogModel) barePreviewMaxTop() int {
	if !m.previewDrawn() || m.step != chapterImportStepEdit {
		return 0
	}
	_, rows := m.bareEdit()
	if rows == 0 {
		return 0
	}
	return max(len(m.view.Chapters)-rows, 0)
}

// bareView draws the bare form: exactly width x height cells.
func (m *ChapterImportDialogModel) bareView() string {
	w, h := m.width, m.height
	switch m.step {
	case chapterImportStepConfirm:
		n, cur := 0, 0
		if m.view != nil {
			n, cur = len(m.view.Chapters), len(m.view.Current)
		}
		sheet := bareSheet{left: m.bareTitle(), keys: DimStyle.Render("y: Save  any other key: Back")}
		question := fmt.Sprintf("Save %d chapters, replacing the current %d?", n, cur)
		sheet.body = bareStyled(bareWrap(question, w, max(h-2, 1)), YellowStyle)
		if clock := m.bareOffsetClock(); clock != "" {
			sheet.body = append(sheet.body, "Offset "+clock)
		}
		return sheet.render(w, h)

	case chapterImportStepSaving:
		sheet := bareSheet{left: m.bareTitle(), keys: DimStyle.Render("Esc: Close")}
		sheet.body = bareWrap("Saving: rewriting the archive with the chapters.", w, max(h-2, 1))
		sheet.noteUnder("The save keeps running if you close.", DimStyle, w, h)
		return sheet.render(w, h)
	}

	sheet, rows := m.bareEdit()
	if rows > 0 {
		top := min(m.previewTop, max(len(m.view.Chapters)-rows, 0))
		list := strings.TrimSuffix(m.renderRows(m.view.Chapters[top:], rows, w), "\n")
		sheet.body = append(sheet.body, strings.Split(list, "\n")...)
	}
	return sheet.render(w, h)
}
