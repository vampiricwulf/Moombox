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

// TestTrimDeleteModeFooterMatchesEsc: delete mode's footer said "Esc: Close",
// but Esc there cancels an armed delete and otherwise returns to create mode —
// it never closes the dialog.
//
// Mutant: restoring "Esc: Close" in either delete-mode footer.
func TestTrimDeleteModeFooterMatchesEsc(t *testing.T) {
	for _, trims := range [][]TrimInfo{nil, {{ID: "t1", EndTime: 10, Duration: 10}}} {
		m := NewTrimDialogModel()
		m.SetSize(100, 40)
		m.Open("A", "job A")
		m.SetTrims(trims)
		m.HandleKey("m")
		if view := stripANSI(m.View()); strings.Contains(view, "Esc: Close") || !strings.Contains(view, "Esc: Create mode") {
			t.Errorf("delete-mode footer (%d trims) misdescribes Esc:\n%s", len(trims), view)
		}
		m.HandleKey(keyEsc)
		if !m.IsVisible() || m.mode != TrimModeCreate {
			t.Errorf("Esc in delete mode: visible=%v mode=%v, want create mode", m.IsVisible(), m.mode)
		}
	}
}
