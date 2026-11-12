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
