package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// chordDisarmApp builds an App with two Finished jobs, the Tasks panel
// focused, and a delete callback that records what it was asked to delete.
func chordDisarmApp(t *testing.T) (*App, *[]string) {
	t.Helper()
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	var deleted []string
	a.OnDeleteJob = func(id string) { deleted = append(deleted, id) }
	a.taskList.SetJobs([]*database.Job{
		{ID: "a", Title: "Alpha", Status: database.StatusFinished, CreatedAt: "2026-10-04T10:00:00Z"},
		{ID: "b", Title: "Beta", Status: database.StatusFinished, CreatedAt: "2026-10-04T09:00:00Z"},
	})
	a.focusedPanel = PanelTasks
	a.updateSelectedJob()
	return a, &deleted
}

// pressRun sends one key through handleKey and runs whatever command it
// returns (the delete runs off the update loop, in that command).
func pressRun(a *App, msg tea.KeyPressMsg) {
	if _, cmd := a.handleKey(msg); cmd != nil {
		cmd()
	}
}

func armDelete(t *testing.T, a *App) {
	t.Helper()
	pressRun(a, keyMsg("a"))
	pressRun(a, keyMsg("d"))
	if a.chord.action != "d" {
		t.Fatalf("premise lost: A D did not arm (%q)", a.feedback.msg)
	}
}

// TestEscClearingASearchDisarmsTheChord: with a search applied, Esc clears
// the search and returned before the chord reset, so a confirm armed by A D
// stayed armed and a lone D within 3 s deleted the job.
//
// Mutant: drop the disarm on the clear-search Esc branch — "a" is deleted.
func TestEscClearingASearchDisarmsTheChord(t *testing.T) {
	a, deleted := chordDisarmApp(t)
	a.taskList.applyQuery("alpha")
	if a.taskList.Query() != "alpha" {
		t.Fatal("premise lost: the search was not applied")
	}
	armDelete(t, a)
	pressRun(a, keyMsg("esc"))
	if a.taskList.Query() != "" {
		t.Fatalf("premise lost: Esc did not clear the search (%q)", a.taskList.Query())
	}
	if a.chord.prefix != "" {
		t.Errorf("chord still armed after Esc: %+v", a.chord)
	}
	pressRun(a, keyMsg("d"))
	if len(*deleted) != 0 {
		t.Errorf("a lone D after Esc deleted %v", *deleted)
	}
}

// TestSlashDisarmsTheChord: "/" opened the search box and left the chord
// armed, so A D, /, Enter, D deleted the job.
//
// Mutant: drop the disarm before the Tasks "/" intercept — "a" is deleted.
func TestSlashDisarmsTheChord(t *testing.T) {
	a, deleted := chordDisarmApp(t)
	armDelete(t, a)
	pressRun(a, keyMsg("/"))
	if !a.taskList.IsSearching() {
		t.Fatal("premise lost: / did not open the search box")
	}
	pressRun(a, keyMsg("enter"))
	if a.taskList.IsSearching() {
		t.Fatal("premise lost: Enter did not close the search box")
	}
	if a.chord.prefix != "" {
		t.Errorf("chord still armed after / and Enter: %+v", a.chord)
	}
	pressRun(a, keyMsg("d"))
	if len(*deleted) != 0 {
		t.Errorf("D after / Enter deleted %v", *deleted)
	}
}

// TestSlashOnTheLogPanelDisarmsTheChord: the same through the log panel's
// "/" intercept — a chord armed on Tasks survives a Tab to Logs.
func TestSlashOnTheLogPanelDisarmsTheChord(t *testing.T) {
	a, deleted := chordDisarmApp(t)
	armDelete(t, a)
	a.focusedPanel = PanelLogs
	pressRun(a, keyMsg("/"))
	if !a.logs.IsSearching() {
		t.Fatal("premise lost: / did not open the log search")
	}
	if a.chord.prefix != "" {
		t.Errorf("chord still armed after / on the log panel: %+v", a.chord)
	}
	pressRun(a, keyMsg("esc"))
	a.focusedPanel = PanelTasks
	pressRun(a, keyMsg("d"))
	if len(*deleted) != 0 {
		t.Errorf("D after the log search deleted %v", *deleted)
	}
}

// TestEscWithASelectionDisarmsTheChord pins the branch that already
// disarmed: Esc clearing a batch selection.
func TestEscWithASelectionDisarmsTheChord(t *testing.T) {
	a, deleted := chordDisarmApp(t)
	a.taskList.ToggleSelection("b")
	pressRun(a, keyMsg("a"))
	pressRun(a, keyMsg("d"))
	if a.chord.prefix == "" {
		t.Fatalf("premise lost: A D did not arm (%q)", a.feedback.msg)
	}
	pressRun(a, keyMsg("esc"))
	if a.taskList.SelectedCount() != 0 {
		t.Fatal("premise lost: Esc did not clear the selection")
	}
	if a.chord.prefix != "" {
		t.Errorf("chord still armed after Esc: %+v", a.chord)
	}
	pressRun(a, keyMsg("d"))
	if len(*deleted) != 0 {
		t.Errorf("D after Esc deleted %v", *deleted)
	}
}

// TestEscClearingALogSearchDisarmsTheChord: on the Logs panel with a log
// search applied (box closed, query live), Esc is consumed by the log
// viewer's search handler, which returned before the Esc branch that
// disarms — so A D, Esc, D still deleted the cursor job.
//
// Mutant: drop the disarm in the Logs intercept — "a" is deleted.
func TestEscClearingALogSearchDisarmsTheChord(t *testing.T) {
	a, deleted := chordDisarmApp(t)
	a.focusedPanel = PanelLogs
	a.logs.searchQuery = "alpha"
	armDelete(t, a)
	pressRun(a, keyMsg("esc"))
	if a.logs.searchQuery != "" {
		t.Fatalf("premise lost: Esc did not clear the log search (%q)", a.logs.searchQuery)
	}
	if a.chord.prefix != "" {
		t.Errorf("chord still armed after Esc: %+v", a.chord)
	}
	pressRun(a, keyMsg("d"))
	if len(*deleted) != 0 {
		t.Errorf("a lone D after Esc deleted %v", *deleted)
	}
}
