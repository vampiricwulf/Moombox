package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// trimRunningApp is an App whose trim dialog is on its progress step for a
// trim the dialog started, with OnStopTrim recording what it was asked.
func trimRunningApp(t *testing.T, trimID string, stopErr error) (*App, *[]string) {
	t.Helper()
	a := NewApp()
	a.width, a.height = 100, 40
	a.trimDlg.SetSize(100, 40)
	a.trimDlg.Open("A", "job A")
	a.trimDlg.StartProgress()
	a.trimInProgress = true
	a.trimRunningID = trimID
	var asked []string
	a.OnStopTrim = func(jobID, id string) error {
		asked = append(asked, jobID+"/"+id)
		return stopErr
	}
	return a, &asked
}

// pressTrim delivers key to the trim dialog and runs what it returns.
func pressTrim(a *App, key string) {
	_, cmd := a.keyTrim(tea.KeyPressMsg{}, key)
	if cmd != nil {
		a.Update(cmd())
	}
}

// TestTrimDialogSStopsItsTrim: the trim dialog's progress step had one key,
// Esc, which only hid it; the encode ran on to its end. S now stops the
// trim the dialog started, by the id OnCreateTrim handed over, once: a
// second S while the stop is out sends nothing. The trim's own result then
// comes back stopped, which closes the dialog and says "Trim stopped", not
// failed.
//
// Mutants: drop the progress step's S (nothing is asked); drop its
// !m.stopping check (a second S asks again); drop createTrimResultMsg's
// Stopped arm (the dialog stays, the stop read as a failure).
func TestTrimDialogSStopsItsTrim(t *testing.T) {
	a, asked := trimRunningApp(t, "trim_A_1", nil)
	pressTrim(a, "s")
	pressTrim(a, "S")
	if len(*asked) != 1 || (*asked)[0] != "A/trim_A_1" {
		t.Fatalf("S asked to stop %v, want A/trim_A_1 once", *asked)
	}
	if view := stripANSI(a.trimDlg.View()); !strings.Contains(view, "Stopping...") {
		t.Errorf("the dialog does not say the stop is out:\n%s", view)
	}

	a.Update(createTrimResultMsg{Stopped: true})
	if a.trimDlg.IsVisible() {
		t.Error("the stopped trim's dialog is still open")
	}
	if a.trimInProgress {
		t.Error("trimInProgress still set after the stopped trim's result")
	}
	if a.feedback.msg != "Trim stopped" {
		t.Errorf("feedback = %q, want \"Trim stopped\"", a.feedback.msg)
	}
}

// TestTrimDialogRefusedStopRunsOn: a stop refused (the clip is being saved,
// or the trim already ended) leaves the trim running with its reason shown,
// and S works again; with no id yet (the trim still being checked) nothing
// is asked and the dialog says so.
//
// Mutants: drop stopTrimResultMsg's StopRefused (the dialog stays on
// "Stopping..." with S dead); drop stopTrimCmd's empty-id check (an empty
// id is sent).
func TestTrimDialogRefusedStopRunsOn(t *testing.T) {
	a, asked := trimRunningApp(t, "trim_A_1", errors.New("the trim has finished encoding and is being saved"))
	pressTrim(a, "s")
	if !a.trimDlg.IsVisible() || a.trimDlg.Stopping() {
		t.Fatalf("after a refused stop: visible=%v stopping=%v, want the progress step with S live", a.trimDlg.IsVisible(), a.trimDlg.Stopping())
	}
	if a.trimDlg.errorMsg != "the trim has finished encoding and is being saved" {
		t.Errorf("dialog error = %q, want the refusal", a.trimDlg.errorMsg)
	}
	pressTrim(a, "s")
	if len(*asked) != 2 {
		t.Errorf("S after a refusal asked %d times in all, want 2", len(*asked))
	}

	a, asked = trimRunningApp(t, "", nil)
	pressTrim(a, "s")
	if len(*asked) != 0 {
		t.Errorf("a stop with no trim id yet asked %v", *asked)
	}
	if !strings.Contains(a.trimDlg.errorMsg, "still starting") || a.trimDlg.Stopping() {
		t.Errorf("no id yet: error %q stopping=%v", a.trimDlg.errorMsg, a.trimDlg.Stopping())
	}
}

// TestCreateTrimCmdKeepsTheStartedTrimsID: the id S stops comes from
// OnCreateTrim's onStarted, while the encode runs; and a submit clears the
// last trim's id, so an S before the new trim has one cannot stop the old.
//
// Mutants: drop createTrimCmd's onStarted write (no id is kept); drop the
// submit's reset (the old id survives into the next trim).
func TestCreateTrimCmdKeepsTheStartedTrimsID(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	var seen string
	a.OnCreateTrim = func(_ string, _, _ float64, onStarted func(string), _ func(float64)) (string, string, bool) {
		onStarted("trim_A_7")
		a.trimProgressMu.Lock()
		seen = a.trimRunningID
		a.trimProgressMu.Unlock()
		return "", "", true
	}
	if msg, ok := a.createTrimCmd("A", 0, 10)().(createTrimResultMsg); !ok || !msg.Stopped {
		t.Errorf("a stopped OnCreateTrim's result = %#v, want Stopped", msg)
	}
	if seen != "trim_A_7" {
		t.Errorf("trimRunningID while the trim ran = %q, want trim_A_7", seen)
	}

	a.trimDlg.SetSize(100, 40)
	a.trimDlg.Open("A", "job A")
	a.trimDlg.createStep = trimStepConfirm
	a.OnCreateTrim = func(string, float64, float64, func(string), func(float64)) (string, string, bool) {
		return "", "", false
	}
	_, _ = a.keyTrim(tea.KeyPressMsg{}, keyEnter)
	if a.trimRunningID != "" {
		t.Errorf("a new submit kept the last trim's id %q", a.trimRunningID)
	}
}

// TestTrimStoppedWithAReasonSaysIt: a trim whose job was deleted comes back
// stopped with why; the feedback line says so, not "Trim failed".
//
// Mutant: drop the Err from createTrimCmd's stopped result — the reason is
// lost.
func TestTrimStoppedWithAReasonSaysIt(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.OnCreateTrim = func(string, float64, float64, func(string), func(float64)) (string, string, bool) {
		return "", "the job was deleted", true
	}
	a.trimInProgress = true
	a.Update(a.createTrimCmd("A", 0, 10)())
	if a.feedback.msg != "Trim stopped: the job was deleted" {
		t.Errorf("feedback = %q, want \"Trim stopped: the job was deleted\"", a.feedback.msg)
	}
}

// TestATBringsBackTheBackgroundTrim: Esc sends the dialog's trim to the
// background, and A T was then refused ("A trim is already in progress"), so
// nothing reached S and the TUI had no stop for its own trim. A T now shows
// the progress step again, whatever job the cursor is on, and S stops that
// trim. The chord goes through the key path, whose job gate refused it on a
// row that is no finished job with a file (or on an empty list), and the
// status bar's A hints offer it there too.
//
// Mutants: A T refused while a trim runs (OnStopTrim is never asked); drop
// buildMenuItems' trimInBackground arm (the key path refuses A T on the
// Downloading row and with no jobs, and the hint leaves it out).
func TestATBringsBackTheBackgroundTrim(t *testing.T) {
	for _, tc := range []struct {
		name string
		jobs []*database.Job
	}{
		{"downloading row", []*database.Job{{ID: "D", Title: "D", Status: database.StatusDownloading}}},
		{"no jobs", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, asked := trimRunningApp(t, "trim_A_1", nil)
			a.taskList.SetJobs(tc.jobs)
			pressTrim(a, keyEsc)
			if a.trimDlg.IsVisible() {
				t.Fatal("Esc did not send the trim to the background")
			}

			a.handleChord("a")
			if hint := stripANSI(a.feedback.msg); !strings.Contains(hint, "T Trim") {
				t.Errorf("the A hint leaves out T Trim: %q", hint)
			}
			a.handleChord("t")
			if !a.trimDlg.IsVisible() || a.trimDlg.JobID() != "A" {
				t.Fatalf("A T did not bring back the running trim's dialog (visible %v, job %q, feedback %q)",
					a.trimDlg.IsVisible(), a.trimDlg.JobID(), a.feedback.msg)
			}
			pressTrim(a, "S")
			if len(*asked) != 1 || (*asked)[0] != "A/trim_A_1" {
				t.Errorf("S after A T asked to stop %v, want A/trim_A_1", *asked)
			}
		})
	}
}

// TestALateStopRefusalLeavesTheTrimsResult: S pressed as the trim finished.
// Its result arrives first and says "Trim created"; the stop then answers
// that the trim has already ended. With no trim running that refusal is
// dropped, rather than shown in red over the trim that succeeded.
//
// Mutants: drop the !a.trimInProgress check (the refusal covers the result);
// drop FailCreate's stopping reset (a failed trim's dialog says
// "Stopping..." on).
func TestALateStopRefusalLeavesTheTrimsResult(t *testing.T) {
	a, _ := trimRunningApp(t, "trim_A_1", errors.New("the trim has already ended"))
	_, stop := a.keyTrim(tea.KeyPressMsg{}, "S")
	a.Update(createTrimResultMsg{Filename: "clip.mp4"})
	a.Update(stop())
	if a.feedback.msg != "Trim created: clip.mp4" {
		t.Errorf("feedback = %q, want the trim's own result", a.feedback.msg)
	}

	b, _ := trimRunningApp(t, "trim_A_1", nil)
	pressTrim(b, "S")
	b.Update(createTrimResultMsg{Err: "boom"})
	if b.trimDlg.Stopping() {
		t.Error("the failed trim's dialog still holds a stop")
	}
}
