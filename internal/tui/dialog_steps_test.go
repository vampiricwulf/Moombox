package tui

import "testing"

// The dialog steps are named types (K28), so a step field takes only its
// own dialog's constants: these assignments compile, and assigning another
// dialog's constant (or an int variable) to any of them would not.
var (
	_ importStep        = ImportDialogModel{}.step
	_ trimCreateStep    = TrimDialogModel{}.createStep
	_ chapterImportStep = ChapterImportDialogModel{}.step
	_ cookieImportStep  = CookieImportDialogModel{}.step
	_ AddVideoStep      = AddVideoModel{}.step
)

// The constants keep the values the bare ints had, so nothing that reads a
// step — the dialogs' own switches, the panic recovery that puts an import
// back on its metadata step — changes meaning.
func TestDialogStepValues(t *testing.T) {
	if importStepPick != 0 || importStepMeta != 1 || importStepImporting != 2 {
		t.Error("importStep values moved")
	}
	if trimStepTimes != 0 || trimStepConfirm != 1 || trimStepProgress != 2 {
		t.Error("trimCreateStep values moved")
	}
}

// A create that fails while the dialog is still open returns it to the
// confirmation through the dialog's own FailCreate — App no longer writes
// createStep — with the spinner stopped and the error shown, and Enter
// there submits again.
//
// Mutant: FailCreate leaving createStep on trimStepProgress — Enter does
// not resubmit.
func TestCreateTrimFailureReturnsToTheConfirmation(t *testing.T) {
	a := overlayTestApp(t)
	a.trimDlg.Open("j1", "a job")
	a.trimDlg.StartProgress()
	a.trimDlg.SetLoading(true)
	a.trimInProgress = true

	a.Update(createTrimResultMsg{Err: "ffmpeg exited 1"})
	if a.trimInProgress {
		t.Error("trimInProgress still set after the result")
	}
	if !a.trimDlg.IsVisible() || a.trimDlg.createStep != trimStepConfirm {
		t.Fatalf("visible=%v step=%v, want the confirmation", a.trimDlg.IsVisible(), a.trimDlg.createStep)
	}
	if a.trimDlg.loading || a.trimDlg.errorMsg != "ffmpeg exited 1" {
		t.Errorf("loading=%v error=%q", a.trimDlg.loading, a.trimDlg.errorMsg)
	}
	if got := a.trimDlg.HandleKey(keyEnter); got != "submit" {
		t.Errorf("Enter on the confirmation after a failure = %q, want submit", got)
	}
}
