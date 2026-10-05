package tui

import (
	"strings"
	"testing"
)

// TestTrimDeleteResultGoesToTheDialogThatAsked: the delete path had no job
// check (create is guarded by trimInProgress; delete was not), so deleting a
// trim on job A, leaving, and opening job B's trims showed A's error in B's
// dialog — or removed a row by A's trim ID from B's list. The answer now
// reaches the dialog only if it is still the one that asked; otherwise it
// goes to the feedback line.
//
// Mutant: dropping the job check — B's dialog shows A's error.
func TestTrimDeleteResultGoesToTheDialogThatAsked(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.trimDlg.Open("A", "job A")
	a.trimDlg.SetLoading(true)
	a.trimDlg.Close()
	a.trimDlg.Open("B", "job B")

	a.Update(deleteTrimResultMsg{JobID: "A", TrimID: "t1", Err: "disk on fire"})
	if a.trimDlg.errorMsg != "" {
		t.Errorf("job B's dialog shows job A's error: %q", a.trimDlg.errorMsg)
	}
	if !strings.Contains(a.feedback.msg, "disk on fire") {
		t.Errorf("the failure was not reported on the feedback line: %q", a.feedback.msg)
	}

	a.Update(deleteTrimResultMsg{JobID: "B", TrimID: "t9", Err: "permission denied"})
	if a.trimDlg.errorMsg != "permission denied" {
		t.Errorf("B's own answer did not reach B's dialog: %q", a.trimDlg.errorMsg)
	}
}
