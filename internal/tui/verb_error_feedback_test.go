package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestVerbRefusalReachesTheFeedbackLine: the worker refuses a Resume or a
// Reinitialize when a different verb already waits on the job (owner rulings
// R-A3/R-A4). The TUI used to call them blind and report "Resuming: …"
// regardless; it now shows the worker's text — the one REST puts in its 409 —
// in red. A batch says how many it could not act on, and the first reason.
//
// Mutants: the callback's error ignored (the line says "Resuming"); the
// batch counting a refused job as done; the line not stated red.
func TestVerbRefusalReachesTheFeedbackLine(t *testing.T) {
	press := func(app *App, r rune) {
		app.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	refusal := errors.New("reinitialize already pending")
	newApp := func() *App {
		app := NewApp()
		app.HasStagingFiles = func(string) bool { return true }
		app.HasSegmentFiles = func(string) bool { return true }
		app.OnResumeJob = func(id string) error {
			if id == "a" {
				return refusal
			}
			return nil
		}
		app.OnReinitializeJob = func(id string) error {
			if id == "a" {
				return errors.New("resume already pending")
			}
			return nil
		}
		app.OnMuxJob = func(string) error { return RefusalError{Err: errors.New("mux in progress")} }
		app.taskList.SetJobs([]*database.Job{
			{ID: "a", Title: "Alpha", Status: database.StatusError, Platform: "youtube", CreatedAt: "2026-10-04T10:00:00Z"},
			{ID: "b", Title: "Beta", Status: database.StatusCancelled, Platform: "youtube", CreatedAt: "2026-10-04T09:00:00Z"},
		})
		return app
	}

	app := newApp()
	press(app, 'a')
	press(app, 'r')
	if app.feedback.msg != "reinitialize already pending" || app.feedback.sev != severityError {
		t.Errorf("A R refused: line %q (sev %v), want exactly the worker's text (REST's 409) in red", app.feedback.msg, app.feedback.sev)
	}

	app = newApp()
	press(app, 'a')
	press(app, 'i')
	press(app, 'i')
	if app.feedback.msg != "resume already pending" || app.feedback.sev != severityError {
		t.Errorf("A I I refused: line %q (sev %v), want exactly the worker's text (REST's 409) in red", app.feedback.msg, app.feedback.sev)
	}

	app = newApp()
	press(app, 'a')
	press(app, 'm')
	press(app, 'm')
	if app.feedback.msg != "mux in progress" || app.feedback.sev != severityError {
		t.Errorf("A M refused: line %q (sev %v), want exactly the worker's text (REST's 409) in red", app.feedback.msg, app.feedback.sev)
	}

	// A fault while the mux ran, not a refusal, keeps its prefix.
	app = newApp()
	app.OnMuxJob = func(string) error { return errors.New("ffmpeg exited 1") }
	press(app, 'a')
	press(app, 'm')
	press(app, 'm')
	if app.feedback.msg != "Mux failed: ffmpeg exited 1" || app.feedback.sev != severityError {
		t.Errorf("A M failed: line %q (sev %v), want \"Mux failed: \" before the fault, in red", app.feedback.msg, app.feedback.sev)
	}

	// A batch: one refused, one resumed.
	app = newApp()
	app.taskList.ToggleSelection("a")
	app.taskList.ToggleSelection("b")
	press(app, 'a')
	press(app, 'r')
	msg := app.feedback.msg
	if !strings.Contains(msg, "Resumed 1 job") || !strings.Contains(msg, "1 refused") ||
		!strings.Contains(msg, "reinitialize already pending") || app.feedback.sev != severityError {
		t.Errorf("batch A R with one refusal: line %q (sev %v), want the count resumed, the count refused and the first reason, in red",
			msg, app.feedback.sev)
	}
}
