package tui

import (
	"strings"
	"testing"
)

// TestDialogCommandPanicsAnswerTheirDialog: a recovered panic in an async
// command used to come back as a generic panicRecoveryMsg that named no
// command, so the dialog waiting on it never heard back — the import overlay
// spun on its importing step with no key to leave it, and a panicking trim
// left trimInProgress set ("already in progress") for the rest of the session.
// Each such command now answers with its own error result.
//
// Mutant: building either command with plain safeCmd — the dialog stays stuck.
func TestDialogCommandPanicsAnswerTheirDialog(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40

	a.OnImportFile = func(string, string, string) (string, error) { panic("zip reader exploded") }
	a.importDlg.SetSize(100, 40)
	a.importDlg.visible, a.importDlg.step = true, 2
	a.Update(a.importFileCmd("/x.zip")())
	if a.importDlg.step == 2 || !strings.Contains(a.importDlg.errorMsg, "zip reader exploded") {
		t.Errorf("import dialog after a panic: step %d, error %q — want it back on its form with the error", a.importDlg.step, a.importDlg.errorMsg)
	}

	a.OnCreateTrim = func(string, float64, float64, func(float64)) (string, string) { panic("encoder exploded") }
	a.trimInProgress = true
	a.Update(a.createTrimCmd("j", 0, 10)())
	if a.trimInProgress {
		t.Error("a panicking trim left trimInProgress set")
	}
}
