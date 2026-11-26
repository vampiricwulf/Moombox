package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// pressApply sends one key through handleKey, runs the command it returns
// and delivers that command's message, as the program would.
func pressApply(a *App, msg tea.KeyPressMsg) {
	if _, cmd := a.handleKey(msg); cmd != nil {
		if res := cmd(); res != nil {
			a.Update(res)
		}
	}
}

func jobVerbApp() *App {
	app := NewApp()
	app.HasStagingFiles = func(string) bool { return true }
	app.taskList.SetJobs([]*database.Job{
		{ID: "a", Title: "Alpha", Status: database.StatusError, Platform: "youtube", CreatedAt: "2026-10-04T10:00:00Z"},
		{ID: "b", Title: "Beta", Status: database.StatusCancelled, Platform: "youtube", CreatedAt: "2026-10-04T09:00:00Z"},
	})
	return app
}

// The worker's Reinitialize deletes a staging directory on its caller's
// goroutine. The dispatch must not call it: the call belongs to the command.
// Mutant: calling the verb inline again.
func TestResumeAndReinitializeRunInACommand(t *testing.T) {
	for _, chord := range []string{"A R", "A I"} {
		app := jobVerbApp()
		var called []string
		verb := func(id string) error { called = append(called, id); return nil }
		app.OnResumeJob, app.OnReinitializeJob = verb, verb
		job := app.taskList.GetJobByID("a")

		_, cmd := app.dispatchAction(chord, job)
		if len(called) != 0 {
			t.Fatalf("%s ran the worker's verb on the update goroutine", chord)
		}
		if cmd == nil || !app.jobVerb.busy {
			t.Fatalf("%s: no command, or not busy (%v)", chord, app.jobVerb.busy)
		}
		if !strings.Contains(app.feedback.msg, "Alpha") {
			t.Errorf("%s: dispatch line = %q, want it to say what started", chord, app.feedback.msg)
		}
		app.Update(cmd())
		if len(called) != 1 || called[0] != "a" || app.jobVerb.busy {
			t.Errorf("%s: called %v, busy %v after the answer", chord, called, app.jobVerb.busy)
		}

		// A batch: the eligible jobs, the selection cleared at dispatch.
		called = nil
		app.taskList.ToggleSelection("a")
		app.taskList.ToggleSelection("b")
		_, cmd = app.dispatchAction(chord, nil)
		if len(called) != 0 || cmd == nil {
			t.Fatalf("%s batch: called %v before the command ran", chord, called)
		}
		if app.taskList.SelectedCount() != 0 {
			t.Errorf("%s batch: the selection must clear at dispatch", chord)
		}
		if !strings.Contains(app.feedback.msg, "2 jobs...") {
			t.Errorf("%s batch: dispatch line = %q", chord, app.feedback.msg)
		}
		app.Update(cmd())
		if len(called) != 2 || !strings.Contains(app.feedback.msg, "2 jobs") || strings.Contains(app.feedback.msg, "...") {
			t.Errorf("%s batch: called %v, line %q", chord, called, app.feedback.msg)
		}
	}
}

// One run at a time: a second Resume or Reinitialize while one is running is
// refused with a line saying so, and starts nothing.
func TestASecondJobVerbIsRefusedWhileOneRuns(t *testing.T) {
	app := jobVerbApp()
	calls := 0
	app.OnResumeJob = func(string) error { calls++; return nil }
	app.OnReinitializeJob = func(string) error { calls++; return nil }

	_, first := app.dispatchAction("A I", app.taskList.GetJobByID("a"))
	app.taskList.ToggleSelection("b")
	_, second := app.dispatchAction("A R", app.taskList.GetJobByID("b"))
	if second != nil {
		t.Fatal("a second verb started while the first was running")
	}
	if !strings.Contains(app.feedback.msg, "Reinitialize is still running") || app.feedback.sev != severityWarning {
		t.Errorf("line = %q (sev %v), want the busy refusal", app.feedback.msg, app.feedback.sev)
	}
	_, batch := app.dispatchAction("A R", nil)
	if batch != nil || app.taskList.SelectedCount() != 1 {
		t.Errorf("a refused batch must start nothing and keep its selection (selected %d)", app.taskList.SelectedCount())
	}
	app.Update(first())
	if calls != 1 {
		t.Errorf("verb calls = %d, want only the first run's", calls)
	}
	if _, again := app.dispatchAction("A R", app.taskList.GetJobByID("b")); again == nil {
		t.Error("a verb must start again once the run has answered")
	}
}

// An answer for a run that is not the one in flight is dropped: it neither
// writes the feedback line nor ends the run that is.
func TestALateJobVerbAnswerIsDropped(t *testing.T) {
	app := jobVerbApp()
	app.OnResumeJob = func(string) error { return nil }
	_, cmd := app.dispatchAction("A R", app.taskList.GetJobByID("a"))
	line := app.feedback.msg

	stale := jobVerbResultMsg{Gen: app.jobVerb.gen - 1, Chord: "A I", Title: "Gone"}
	stale.Refused.add(errors.New("stale refusal"))
	app.Update(stale)
	if app.feedback.msg != line || !app.jobVerb.busy {
		t.Fatalf("a stale answer was applied: line %q, busy %v", app.feedback.msg, app.jobVerb.busy)
	}
	res := cmd()
	app.Update(res)
	app.Update(res) // the same answer twice: the second finds no run
	if app.jobVerb.busy || app.feedback.msg != "Resuming: Alpha" {
		t.Errorf("after the answer: busy %v, line %q", app.jobVerb.busy, app.feedback.msg)
	}
}

// A panic in the verb still ends the run, with the fault on the line.
func TestAPanickingJobVerbEndsItsRun(t *testing.T) {
	app := jobVerbApp()
	app.OnReinitializeJob = func(string) error { panic("boom") }
	_, cmd := app.dispatchAction("A I", app.taskList.GetJobByID("a"))
	app.Update(cmd())
	if app.jobVerb.busy {
		t.Fatal("a panic left the flow busy for the session")
	}
	if !strings.Contains(app.feedback.msg, "boom") || app.feedback.sev != severityError {
		t.Errorf("line = %q (sev %v)", app.feedback.msg, app.feedback.sev)
	}
}

func pressRune(a *App, r rune) (tea.Model, tea.Cmd) {
	return a.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
}

// A single job's answer can arrive seconds after its dispatch. It must not
// take the feedback line from a confirm armed in the meantime: the chord
// would stay armed with its prompt gone. Mutant: finishJobVerb setting
// "Reinitializing: <title>" again on success.
func TestAJobVerbAnswerLeavesAnArmedConfirmPromptAlone(t *testing.T) {
	app := jobVerbApp()
	app.OnReinitializeJob = func(string) error { return nil }
	app.OnDeleteJob = func(string) {}
	_, cmd := app.dispatchAction("A I", app.taskList.GetJobByID("a"))

	pressRune(app, 'a')
	pressRune(app, 'd')
	prompt := app.feedback.msg
	if !strings.Contains(prompt, "Press D to confirm") {
		t.Fatalf("no delete prompt armed: %q", prompt)
	}
	app.Update(cmd())
	if app.feedback.msg != prompt {
		t.Errorf("the answer replaced the armed prompt with %q", app.feedback.msg)
	}
	if app.chord.action == "" {
		t.Error("the confirm must stay armed")
	}

	// A batch's count is held back the same way; a refusal is still shown.
	app = jobVerbApp()
	app.OnResumeJob = func(string) error { return nil }
	app.OnDeleteJob = func(string) {}
	app.taskList.ToggleSelection("a")
	app.taskList.ToggleSelection("b")
	_, cmd = app.dispatchAction("A R", nil)
	pressRune(app, 'a')
	hint := app.feedback.msg
	app.Update(cmd())
	if app.feedback.msg != hint {
		t.Errorf("a batch's count replaced the chord hint with %q", app.feedback.msg)
	}

	app = jobVerbApp()
	app.OnResumeJob = func(string) error { return errors.New("refused: reset pending") }
	_, cmd = app.dispatchAction("A R", app.taskList.GetJobByID("a"))
	pressRune(app, 'a')
	app.Update(cmd())
	if !strings.Contains(app.feedback.msg, "reset pending") || app.feedback.sev != severityError {
		t.Errorf("a refusal must be shown whatever holds the line: %q", app.feedback.msg)
	}
}

// The run's line is the only sign of the busy state, so it outlives the 3 s
// an ordinary line gets, and comes back after a line that replaced it has
// expired. Once the run answers it gets the ordinary hold. Mutant: the tick
// without keepJobVerbShown (the line is gone on the first tick after 3 s).
func TestTheRunningLineStaysUpWhileTheRunDoes(t *testing.T) {
	app := jobVerbApp()
	app.OnReinitializeJob = func(string) error { return nil }
	app.taskList.ToggleSelection("a")
	app.taskList.ToggleSelection("b")
	_, cmd := app.dispatchAction("A I", nil)
	running := app.feedback.msg

	app.feedback.until = app.feedback.until.Add(-time.Hour) // far past any hold
	app.Update(tickMsg{})
	if app.feedback.msg != running {
		t.Fatalf("the running line expired mid-run: %q", app.feedback.msg)
	}
	app.setFeedback("something else")
	app.feedback.until = time.Now().Add(-time.Second)
	app.Update(tickMsg{})
	if app.feedback.msg != running {
		t.Errorf("the running line did not return after another line expired: %q", app.feedback.msg)
	}

	app.Update(cmd())
	if app.feedback.until.IsZero() {
		t.Fatal("the result line must expire")
	}
	app.feedback.until = time.Now().Add(-time.Second)
	app.Update(tickMsg{})
	if app.feedback.msg != "" {
		t.Errorf("a line was left up after the run: %q", app.feedback.msg)
	}
}

// A I is refused while a run is in flight before its confirm is armed, not
// after the operator has answered it. Mutant: the busy check left to
// dispatchAction alone.
func TestReinitializeChordIsRefusedBeforeItsConfirmWhileARunIsInFlight(t *testing.T) {
	app := jobVerbApp()
	app.OnReinitializeJob = func(string) error { return nil }
	summaries := 0
	app.ReinitializeSummary = func([]string) string { summaries++; return "" }
	app.dispatchAction("A R", nil) // nothing selected: starts nothing
	app.OnResumeJob = func(string) error { return nil }
	app.dispatchAction("A R", app.taskList.GetJobByID("b"))

	summaries = 0
	pressRune(app, 'a')
	pressRune(app, 'i')
	if app.chord.prefix != "" || app.chord.action != "" {
		t.Error("the chord must be disarmed by the refusal")
	}
	if !strings.Contains(app.feedback.msg, "Resume is still running") {
		t.Errorf("line = %q, want the busy refusal", app.feedback.msg)
	}
	if summaries != 0 {
		t.Error("the confirm's preflight ran for a chord that was refused")
	}
}

// Quitting or restarting mid-run can stop a reset half way, so both prompts
// say a run is in flight. Mutant: jobVerbRunningNote returning "".
func TestQuitAndRestartPromptsNameARunInFlight(t *testing.T) {
	app := jobVerbApp()
	app.OnReinitializeJob = func(string) error { return nil }
	app.OnRestart = func() {}
	pressRune(app, 'q')
	if app.feedback.msg != "Quit: Q Confirm (3s)" {
		t.Errorf("idle quit prompt = %q", app.feedback.msg)
	}
	app.disarmChord()

	app.dispatchAction("A I", app.taskList.GetJobByID("a"))
	pressRune(app, 'q')
	if app.feedback.msg != "Quit: Q Confirm (3s) — Reinitialize is still running" || app.feedback.sev != severityWarning {
		t.Errorf("quit prompt = %q (sev %v)", app.feedback.msg, app.feedback.sev)
	}
	app.disarmChord()
	pressRune(app, 'r')
	pressRune(app, 'p')
	if app.feedback.msg != "Press P to confirm restart: Reinitialize is still running (3s)" {
		t.Errorf("restart prompt = %q", app.feedback.msg)
	}
}

// The update restart (R U, and U in the release notes) asks for its second
// press while a run is in flight, as it does for active downloads, and names
// the run. Mutant: applyUpdateAction reading the download count alone.
func TestUpdateRestartAsksAgainWhileARunIsInFlight(t *testing.T) {
	app := jobVerbApp()
	app.OnReinitializeJob = func(string) error { return nil }
	app.updateAvailable = &UpdateStatusMsg{TagName: "v9.9.9"}
	applied := 0
	app.OnApplyUpdate = func(string) (string, bool) { applied++; return "", false }

	app.dispatchAction("A I", app.taskList.GetJobByID("a"))
	if cmd := app.applyUpdateAction(); cmd != nil {
		t.Fatal("the update ran mid-reset without its second press")
	}
	want := "Reinitialize is still running; press R U again within 5s to confirm"
	if app.feedback.msg != want || app.feedback.sev != severityWarning {
		t.Errorf("update line = %q (sev %v), want %q", app.feedback.msg, app.feedback.sev, want)
	}
	if cmd := app.applyUpdateAction(); cmd == nil {
		t.Fatal("the second press did not apply the update")
	}

	// Nothing running, nothing active: no second press, as before.
	idle := jobVerbApp()
	idle.updateAvailable = &UpdateStatusMsg{TagName: "v9.9.9"}
	idle.OnApplyUpdate = func(string) (string, bool) { return "", false }
	if cmd := idle.applyUpdateAction(); cmd == nil {
		t.Error("an idle update asked for a second press")
	}
}

// The Settings restart prompt names a run in flight, at every size it draws
// in, and keeps its keys. Mutant: SettingsModel.restartRunningNote returning
// "".
func TestSettingsRestartPromptNamesARunInFlight(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {60, 20}, {40, 10}, {30, 6}} {
		for _, downloads := range []bool{false, true} {
			app := jobVerbApp()
			app.OnReinitializeJob = func(string) error { return nil }
			if downloads {
				app.statusMap["d"] = database.StatusDownloading
				app.statusMap["l"] = database.StatusLive
			}
			app.settings.SetSize(size[0], size[1])
			app.settings.visible = true
			app.settings.showRestartOverlay = true
			flatten := func() (string, string) {
				view := stripANSI(app.settings.View())
				return view, strings.Join(strings.Fields(strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ").Replace(view)), " ")
			}
			if _, flat := flatten(); strings.Contains(flat, "still running") {
				t.Errorf("%v: the prompt names a run with none in flight:\n%s", size, flat)
			}

			app.dispatchAction("A I", app.taskList.GetJobByID("a"))
			view, flat := flatten()
			if !strings.Contains(flat, "Reinitialize is still running") {
				t.Errorf("%v downloads=%v: the restart prompt does not name the run:\n%s", size, downloads, view)
			}
			if downloads && !strings.Contains(flat, "2 downloads are active — the restart interrupts them") {
				t.Errorf("%v: the run's note pushed out the downloads warning:\n%s", size, view)
			}
			if !strings.Contains(strings.ToLower(flat), "enter: restart now") {
				t.Errorf("%v downloads=%v: the note pushed the keys off the prompt:\n%s", size, downloads, view)
			}
			if rows := strings.Count(view, "\n") + 1; rows > size[1] {
				t.Errorf("%v downloads=%v: the prompt is %d rows tall", size, downloads, rows)
			}
		}
	}
}

// dispatchApply dispatches chord, runs the command it returns and delivers
// that command's message, as the program would.
func dispatchApply(a *App, chord string, job *database.Job) {
	if _, cmd := a.dispatchAction(chord, job); cmd != nil {
		if res := cmd(); res != nil {
			a.Update(res)
		}
	}
}

// cancelApp is a list of cancellable rows, as a backlog leaves them.
func cancelApp(calls *[]string) *App {
	app := NewApp()
	app.OnCancelJob = func(id string) (bool, error) { *calls = append(*calls, id); return true, nil }
	app.OnResumeJob = func(string) error { return nil }
	app.OnReinitializeJob = func(string) error { return nil }
	app.HasStagingFiles = func(string) bool { return true }
	app.taskList.SetJobs([]*database.Job{
		{ID: "q1", Title: "Queued one", Status: database.StatusQueued, Platform: "youtube", CreatedAt: "2026-11-25T10:00:00Z"},
		{ID: "q2", Title: "Queued two", Status: database.StatusQueued, Platform: "youtube", CreatedAt: "2026-11-25T09:00:00Z"},
		{ID: "e", Title: "Errored", Status: database.StatusError, Platform: "youtube", CreatedAt: "2026-11-25T08:00:00Z"},
	})
	return app
}

// A batch Cancel is one committed row write per job, and a selection can be
// the whole list: the dispatch must not call OnCancelJob, the command does
// (owner ruling R3-15). Mutant: the batch arm calling OnCancelJob inline.
func TestABatchCancelRunsInACommand(t *testing.T) {
	var calls []string
	app := cancelApp(&calls)
	app.taskList.ToggleSelection("q1")
	app.taskList.ToggleSelection("q2")
	app.taskList.ToggleSelection("e") // not cancellable: left out
	_, cmd := app.dispatchAction("A X", nil)
	if len(calls) != 0 {
		t.Fatalf("the batch cancelled %v on the update goroutine", calls)
	}
	if cmd == nil || !app.jobVerb.busy {
		t.Fatalf("no command, or not busy (%v)", app.jobVerb.busy)
	}
	if app.feedback.msg != "Cancelling 2 jobs..." {
		t.Errorf("dispatch line = %q", app.feedback.msg)
	}
	if app.taskList.SelectedCount() != 0 {
		t.Error("the selection must clear at dispatch")
	}
	app.Update(cmd())
	if len(calls) != 2 || app.jobVerb.busy || app.feedback.msg != "Cancelled 2 jobs" {
		t.Errorf("after the answer: calls %v, busy %v, line %q", calls, app.jobVerb.busy, app.feedback.msg)
	}

	// Nothing cancellable: answered at once, no run begun.
	app.taskList.ToggleSelection("e")
	if _, cmd := app.dispatchAction("A X", nil); cmd != nil || app.jobVerb.busy {
		t.Error("a selection with nothing cancellable started a run")
	}
	if app.feedback.msg != "No cancellable jobs in selection" {
		t.Errorf("line = %q", app.feedback.msg)
	}

	// A single job's Cancel is one write and stays synchronous.
	calls = nil
	if _, cmd := app.dispatchAction("A X", app.taskList.GetJobByID("q1")); cmd != nil || len(calls) != 1 {
		t.Errorf("single Cancel: cmd %v, calls %v; want it done at dispatch", cmd != nil, calls)
	}
}

// While a batch Cancel runs, a new Resume, Reinitialize or Cancel (single or
// batch) is refused with a line saying Cancel is still running, and starts
// nothing; the refused batch keeps its selection. While a Resume runs, a
// batch Cancel is refused (one busy state) but a single job's Cancel still
// runs. Mutants: cancelBusy refusing a single Cancel beside a Resume, or
// letting one through beside a batch Cancel; the batch arm skipping the
// busy check.
func TestABatchCancelSharesTheOneBusyState(t *testing.T) {
	var calls []string
	app := cancelApp(&calls)
	app.taskList.ToggleSelection("q1")
	app.taskList.ToggleSelection("q2")
	_, first := app.dispatchAction("A X", nil)

	refused := func(what string, chord string, job *database.Job) {
		t.Helper()
		app.clearFeedback()
		if _, cmd := app.dispatchAction(chord, job); cmd != nil {
			t.Errorf("%s started while a batch Cancel ran", what)
		}
		if !strings.HasPrefix(app.feedback.msg, "Cancel is still running") || app.feedback.sev != severityWarning {
			t.Errorf("%s: line = %q (sev %v), want the busy refusal", what, app.feedback.msg, app.feedback.sev)
		}
	}
	refused("Resume", "A R", app.taskList.GetJobByID("e"))
	refused("Reinitialize", "A I", app.taskList.GetJobByID("e"))
	refused("single Cancel", "A X", app.taskList.GetJobByID("q1"))
	app.taskList.ToggleSelection("q1")
	refused("batch Cancel", "A X", nil)
	if app.taskList.SelectedCount() != 1 {
		t.Error("a refused batch must keep its selection")
	}
	if len(calls) != 0 {
		t.Fatalf("calls %v before the run's command ran", calls)
	}
	app.Update(first())
	if len(calls) != 2 || app.jobVerb.busy {
		t.Fatalf("after the answer: calls %v, busy %v", calls, app.jobVerb.busy)
	}

	// Beside a Resume: a batch Cancel is refused, a single one runs.
	_, resume := app.dispatchAction("A R", app.taskList.GetJobByID("e"))
	if resume == nil {
		t.Fatal("premise lost: Resume did not start")
	}
	app.clearFeedback()
	if _, cmd := app.dispatchAction("A X", nil); cmd != nil || !strings.HasPrefix(app.feedback.msg, "Resume is still running") {
		t.Errorf("a batch Cancel beside a Resume: cmd %v, line %q", cmd != nil, app.feedback.msg)
	}
	calls = nil
	app.dispatchAction("A X", app.taskList.GetJobByID("q2"))
	if len(calls) != 1 {
		t.Errorf("a single Cancel beside a Resume was refused: line %q", app.feedback.msg)
	}
}

// The A X chord is refused before its confirm is armed while it would be
// refused at dispatch, so the operator does not answer a prompt only to be
// told no. Mutant: no Cancel check in the chord handler.
func TestCancelChordIsRefusedBeforeItsConfirmWhileABatchCancelRuns(t *testing.T) {
	var calls []string
	app := cancelApp(&calls)
	app.taskList.ToggleSelection("q1")
	app.dispatchAction("A X", nil)

	pressRune(app, 'a')
	pressRune(app, 'x')
	if app.chord.prefix != "" || app.chord.action != "" {
		t.Error("the chord must be disarmed by the refusal")
	}
	if !strings.HasPrefix(app.feedback.msg, "Cancel is still running") {
		t.Errorf("line = %q, want the busy refusal", app.feedback.msg)
	}
}

// A batch Cancel's answer that lands on an armed confirm leaves it alone;
// a refusal is still shown. Mutant: finishJobVerb writing the cancel count
// whatever holds the line.
func TestABatchCancelAnswerLeavesAnArmedConfirmPromptAlone(t *testing.T) {
	var calls []string
	app := cancelApp(&calls)
	app.OnCancelJob = func(id string) (bool, error) { return id != "q2", nil } // q2 ended meanwhile
	app.OnDeleteJob = func(string) {}
	app.taskList.ToggleSelection("q1")
	app.taskList.ToggleSelection("q2")
	_, cmd := app.dispatchAction("A X", nil)
	app.taskList.GoToStart()
	for i := 0; i < 5; i++ {
		if j := app.taskList.SelectedJob(); j != nil && j.ID == "e" {
			break
		}
		app.taskList.MoveDown()
	}
	if j := app.taskList.SelectedJob(); j == nil || j.ID != "e" {
		t.Fatalf("premise lost: the cursor is not on the Error row (%v)", j)
	}
	pressRune(app, 'a')
	pressRune(app, 'd')
	prompt := app.feedback.msg
	if !strings.Contains(prompt, "Press D to confirm") {
		t.Fatalf("no delete prompt armed: %q", prompt)
	}
	app.Update(cmd())
	if app.feedback.msg != prompt || app.chord.action == "" {
		t.Errorf("the answer replaced the armed prompt with %q", app.feedback.msg)
	}

	app = cancelApp(&calls)
	app.OnCancelJob = func(string) (bool, error) { return false, errors.New("Not cancelled — the job is already finishing") }
	app.taskList.ToggleSelection("q1")
	_, cmd = app.dispatchAction("A X", nil)
	pressRune(app, 'a')
	app.Update(cmd())
	if !strings.Contains(app.feedback.msg, "already finishing") || app.feedback.sev != severityError {
		t.Errorf("a refusal must be shown whatever holds the line: %q", app.feedback.msg)
	}
}
