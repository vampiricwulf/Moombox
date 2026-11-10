package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The bare form of the orphaned items browser, drawn below the normal layout
// (overlay_bare.go). A fixed frame, so no message can push the keys or the
// selected row off the screen:
//
//	row 0        the title and the two counts
//	rows 1..     the list: a cursor mark, a short kind tag, the path, the size
//	row h-2      one message: the armed confirm, else a delete error, else a
//	             load error, else the last result, else the selected entry's
//	             set-aside recordings where the row has no room for them
//	row h-1      the keys
//
// An armed delete is shortened (owner ruling R-C11): it names the kind, the
// set-aside recordings a staging entry holds and, for a whole section, the
// count; the marked, yellow row it is about stays on screen. It is derived
// from the confirm fields on every draw, never from the worded feedbackMsg.
// An error is never cut to one row: it wraps upward over the list, which
// keeps at least the row the cursor is on.

// bareFilesAsidesCol is the narrowest terminal whose rows carry the
// "N asides" count; below it the count is on the message row.
const bareFilesAsidesCol = 36

// bareFilesNameCol is the narrowest terminal whose single-item confirm names
// the entry.
const bareFilesNameCol = 40

// bare reports whether the dialog draws its bare form: its own size is below
// the normal layout. Derived on every call, never stored.
func (m *FilesDialogModel) bare() bool {
	return layoutModeFor(m.width, m.height) == layoutCompact
}

// bareAsides words a count of set-aside recordings.
func bareAsides(n int) string {
	if n == 1 {
		return "1 aside"
	}
	return fmt.Sprintf("%d asides", n)
}

// bareFileTag is the four-cell kind tag of a file row.
func bareFileTag(kind string) string {
	switch kind {
	case "staging":
		return "stg "
	case "output":
		return "out "
	}
	return fmt.Sprintf("%-4s", truncateWidth(kind, 4, ""))
}

// bareRenderItem draws one row of the bare list.
func (m *FilesDialogModel) bareRenderItem(w io.Writer, l list.Model, index int, item list.Item) {
	width := l.Width()
	selected := index == l.Index()
	mark := " "
	if selected {
		mark = ">"
	}
	// row lays out the mark, the tag, the name cut to what is left and the
	// right-hand figure against the right edge.
	row := func(tag, name, right string, armed bool, tagStyle lipgloss.Style) {
		name = truncateString(name, max(width-1-len(tag)-1-ansi.StringWidth(right), 1))
		gap := strings.Repeat(" ", max(width-1-len(tag)-ansi.StringWidth(name)-ansi.StringWidth(right), 1))
		switch {
		case armed:
			fmt.Fprint(w, YellowStyle.Render(mark+tag+name+gap+right))
		case selected:
			fmt.Fprint(w, lipgloss.NewStyle().Foreground(ColorCyan).Render(mark+tag+name+gap+right))
		default:
			fmt.Fprint(w, mark+tagStyle.Render(tag)+name+gap+right)
		}
	}
	switch it := item.(type) {
	case sectionHeaderItem:
		fmt.Fprint(w, DimStyle.Render(" ── history ──"))
	case historyItem:
		added := it.entry.AddedAt
		if t, err := time.Parse(time.RFC3339, added); err == nil {
			added = relativeTime(t)
		}
		row("hist ", it.entry.VideoID, added, m.deleteConfirmID == it.entry.VideoID, lipgloss.NewStyle().Foreground(ColorGray))
	case fileItem:
		f := it.entry
		tagStyle := lipgloss.NewStyle().Foreground(ColorGray)
		switch f.Type {
		case "staging":
			tagStyle = lipgloss.NewStyle().Foreground(ColorMuxing)
		case "output":
			tagStyle = lipgloss.NewStyle().Foreground(ColorCyan)
		}
		right := formatFileSize(f.Size)
		if n := len(f.Asides); n > 0 && width >= bareFilesAsidesCol {
			right += " " + bareAsides(n)
		}
		row(bareFileTag(f.Type)+" ", f.RelPath, right, m.deleteConfirmID == f.Path, tagStyle)
	}
}

// bareConfirm is the armed confirm in its short form, "" when nothing is
// armed.
func (m *FilesDialogModel) bareConfirm() string {
	if m.deleteAllArmed {
		if m.deleteAllSection == "history" {
			return fmt.Sprintf("A again: remove %d history", len(m.history))
		}
		return fmt.Sprintf("A again: delete all %d files", len(m.files))
	}
	if m.deleteConfirmID == "" {
		return ""
	}
	if f := m.SelectedFile(); f != nil && f.Path == m.deleteConfirmID {
		const head = "D again: delete "
		switch {
		case len(f.Asides) > 0:
			// The recordings it holds are named at every size.
			return head + "+ " + bareAsides(len(f.Asides))
		case m.width >= bareFilesNameCol:
			return head + bareHeadCut(f.RelPath, m.width-len(head))
		}
		return head + "this file"
	}
	if h := m.SelectedHistory(); h != nil && h.VideoID == m.deleteConfirmID {
		if long := "D again: remove history " + h.VideoID; m.width >= bareFilesNameCol && ansi.StringWidth(long) <= m.width {
			return long
		}
		return "D again: remove this history"
	}
	return ""
}

// bareMessage is the message under the list as rows of at most w cells, at
// most maxRows of them: one row for everything but an error, which wraps.
func (m *FilesDialogModel) bareMessage(w, maxRows int, listed bool) []string {
	loadErr := ""
	if listed {
		// With nothing listed the load error is the body itself.
		loadErr = m.loadErrorText()
	}
	switch {
	case m.loading:
	case m.bareConfirm() != "":
		return []string{YellowStyle.Render(truncateString(m.bareConfirm(), w))}
	case m.actionErr != "":
		return bareStyled(bareWrap(m.actionErr, w, maxRows), ErrorStyle)
	case loadErr != "":
		return bareStyled(bareWrap(loadErr, w, maxRows), ErrorStyle)
	case m.feedbackMsg != "" && !strings.HasPrefix(m.feedbackMsg, "Press "):
		return []string{YellowStyle.Render(truncateString(m.feedbackMsg, w))}
	case w < bareFilesAsidesCol:
		if f := m.SelectedFile(); f != nil && len(f.Asides) > 0 {
			return []string{DimStyle.Render("holds " + bareAsides(len(f.Asides)))}
		}
	}
	return []string{""}
}

// bareView draws the bare form: exactly width x height cells.
func (m *FilesDialogModel) bareView() string {
	w := m.width
	total := len(m.list.Items())
	body := max(m.height-2, 1) // every row but the title and the keys

	title := fmt.Sprintf("Orphans: %d files, %d history", len(m.files), len(m.history))
	if len(title) > w {
		title = fmt.Sprintf("Orphans: %d files, %d hist", len(m.files), len(m.history))
	}
	keys := "D delete  A all  R refresh  Esc"
	switch {
	case total == 0 && !m.loading:
		keys = "R refresh  Esc"
	case len(keys) > w:
		keys = "D del  A all  R refresh  Esc"
	}
	sheet := bareSheet{left: TitleStyle.Render(title), keys: DimStyle.Render(keys)}

	listed := !m.loading && total > 0
	msg := m.bareMessage(w, max(body-1, 1), listed)
	top := max(body-len(msg), 1)
	var rows []string
	switch {
	case m.loading:
		rows = []string{m.spinner.View() + " Scanning..."}
	case !listed:
		if loadErr := m.loadErrorText(); loadErr != "" {
			rows = bareStyled(bareWrap(loadErr, w, top), ErrorStyle)
		} else {
			rows = []string{DimStyle.Render("No orphaned files or history.")}
		}
	default:
		// The list gives up rows to a wrapped error and takes them back
		// after it; it keeps the cursor's row on its page either way.
		if m.list.Height() != top {
			m.list.SetHeight(top)
		}
		rows = strings.Split(m.list.View(), "\n")
	}
	sheet.body = append(bareRows(rows, top), msg...)
	return sheet.render(w, m.height)
}
