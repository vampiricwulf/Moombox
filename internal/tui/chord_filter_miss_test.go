package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestChordWithIneligibleJobResetsAndSaysWhy: a registered chord whose
// selected job fails the item's JobFilter used to be swallowed in silence with
// the prefix left armed, so the operator's next press was read as the second
// key of a chord they thought had ended. It now behaves like the invalid-chord
// path — the chord resets to idle and the key is consumed — and the line
// explains itself with the item's DisabledReason, the words the action menu
// shows beside a greyed entry, in the advisory colour.
//
// Mutants: leave a.chord armed (the second A below is then read as "A A" and
// opens Add Video instead of re-arming); set the line with plain setFeedback
// (it renders green).
func TestChordWithIneligibleJobResetsAndSaysWhy(t *testing.T) {
	press := func(app *App, r rune) {
		app.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	app := NewApp()
	app.OnSetWatched = func([]string, bool) error { return nil }
	app.OnDeleteJob = func(string) {}
	app.taskList.SetJobs([]*database.Job{{ID: "d", Title: "live one", Status: database.StatusDownloading}})
	if app.taskList.SelectedJob() == nil {
		t.Fatal("premise lost: no selected job")
	}

	// A W needs a Finished job and the selection is Downloading.
	press(app, 'a')
	if app.chord.prefix != "a" {
		t.Fatalf("premise lost: A did not arm (%q)", app.feedback.msg)
	}
	press(app, 'w')
	if app.chord.prefix != "" || app.chord.action != "" {
		t.Errorf("the chord stayed armed after a filter miss: %+v", app.chord)
	}
	if !strings.Contains(app.feedback.msg, "Toggle Watched") || !strings.Contains(app.feedback.msg, "no finished jobs") {
		t.Errorf("the line does not carry the item's disabled reason: %q", app.feedback.msg)
	}
	if got := feedbackColor(app.feedback.msg, app.feedback.sev); got != ColorYellow {
		t.Errorf("the filter-miss line rendered %v, want yellow", got)
	}

	// The next key is read fresh: A arms again rather than completing a chord.
	press(app, 'a')
	if app.chord.prefix != "a" {
		t.Fatalf("A after the miss did not re-arm (%q) — the key was swallowed as a second key", app.feedback.msg)
	}
	if app.addVideo.IsVisible() {
		t.Fatal("A after the miss opened Add Video — it was read as the second key of A A")
	}

	// A confirm-gated chord never arms its confirm step for a job it cannot
	// act on: A D on a Downloading job.
	press(app, 'd')
	if app.chord.action != "" {
		t.Errorf("A D armed its confirm step for a job it cannot delete: %+v", app.chord)
	}
	if app.chord.prefix != "" {
		t.Errorf("the prefix stayed armed after the confirm-gated miss: %+v", app.chord)
	}
	if !strings.Contains(app.feedback.msg, "Delete Job") || !strings.Contains(app.feedback.msg, "no deletable jobs") {
		t.Errorf("the confirm-gated miss does not carry the item's disabled reason: %q", app.feedback.msg)
	}

	// With no job at all the same reset and the same wording apply.
	app.taskList.SetJobs(nil)
	app.clearFeedback()
	press(app, 'a')
	press(app, 'w')
	if app.chord.prefix != "" || app.chord.action != "" {
		t.Errorf("the chord stayed armed with no job selected: %+v", app.chord)
	}
	if !strings.Contains(app.feedback.msg, "no finished jobs") {
		t.Errorf("the no-job line does not carry the disabled reason: %q", app.feedback.msg)
	}
}
