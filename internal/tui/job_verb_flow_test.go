package tui

import (
	"errors"
	"strings"
	"testing"

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
