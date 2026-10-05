package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The key line is budgeted one row (filesBoxDims), but its full wording is 66
// columns and the box is 54 wide at the 60-column floor: it wrapped and left
// "| Esc: Close" alone on a second row. The shorter wording fits there, and
// with nothing listed only the keys that still do anything are named.
//
// Mutant: always rendering the full wording, or naming D/A on an empty list.
func TestFilesDialogFooterFitsAndFitsTheState(t *testing.T) {
	for _, w := range []int{60, 80} {
		m := NewFilesDialogModel()
		m.SetSize(w, 24)
		m.Open()
		m.SetFiles([]OrphanedFileEntry{{Path: "/a/one.ts", RelPath: "one.ts"}})
		m.SetHistory(nil)
		v := ansi.Strip(m.View())
		var footer []string
		for line := range strings.SplitSeq(v, "\n") {
			if strings.Contains(line, "Esc: Close") || strings.Contains(line, "Refresh") {
				footer = append(footer, line)
			}
			if lw := ansi.StringWidth(line); lw > w {
				t.Errorf("width %d: a line is %d columns: %q", w, lw, line)
			}
		}
		if len(footer) != 1 || !strings.Contains(footer[0], "D: Delete") || !strings.Contains(footer[0], "Esc: Close") {
			t.Errorf("width %d: the key line is not one row naming D and Esc: %q", w, footer)
		}
	}

	m := NewFilesDialogModel()
	m.SetSize(80, 24)
	m.Open()
	m.SetFiles(nil)
	m.SetHistory(nil)
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "R: Refresh | Esc: Close") || strings.Contains(v, "D: Delete") {
		t.Errorf("an empty list still offers deletes:\n%s", v)
	}
}
