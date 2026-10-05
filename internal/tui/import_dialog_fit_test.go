package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The picker's key line is 68 columns and the box 54 wide at the 60-column
// floor: it wrapped and left "Esc: Cancel" alone on a second row. And an
// empty folder read bubbles' own "Bummer. No Files Found." — a failure, when
// the folder just holds no .zip.
//
// Mutant: importPickerHint always answering the full wording, or the picker
// keeping bubbles' empty-folder text.
func TestImportPickerFitsTheFloorAndSaysWhatIsMissing(t *testing.T) {
	m := NewImportDialogModel()
	m.SetSize(60, 24)
	cmd := m.Open(t.TempDir())
	if cmd != nil {
		m.UpdateComponents(cmd())
	}
	v := ansi.Strip(m.View())
	hintRows := 0
	for line := range strings.SplitSeq(v, "\n") {
		if w := ansi.StringWidth(line); w > 60 {
			t.Errorf("a line is %d columns: %q", w, line)
		}
		if strings.Contains(line, "Esc: Cancel") {
			hintRows++
			if !strings.Contains(line, "Open/Select") {
				t.Errorf("the key line wrapped: %q", line)
			}
		}
	}
	if hintRows != 1 {
		t.Errorf("want the key line on one row, found %d:\n%s", hintRows, v)
	}
	if !strings.Contains(v, "No .zip archives here") || strings.Contains(v, "Bummer") {
		t.Errorf("the empty folder is not described:\n%s", v)
	}
}
