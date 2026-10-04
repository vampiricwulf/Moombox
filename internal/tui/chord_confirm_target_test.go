package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestConfirmChordActsOnTheJobItNamed: the confirm step re-resolved its target
// from the cursor at the third key, and the cursor can move inside the 3 s
// window — a mouse click or wheel tick, or a deletion from the dashboard
// handing the cursor to a neighbour. "Press D to confirm delete "A"" then
// deleted B. The chord now carries the job it named.
//
// Mutant: resolve the confirm step from SelectedJob() again — B is deleted.
func TestConfirmChordActsOnTheJobItNamed(t *testing.T) {
	press := func(app *App, r rune) {
		app.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	app := NewApp()
	var deleted []string
	app.OnDeleteJob = func(id string) { deleted = append(deleted, id) }
	app.taskList.SetJobs([]*database.Job{
		{ID: "a", Title: "Alpha", Status: database.StatusFinished, CreatedAt: "2026-10-04T10:00:00Z"},
		{ID: "b", Title: "Beta", Status: database.StatusFinished, CreatedAt: "2026-10-04T09:00:00Z"},
	})
	first := app.taskList.SelectedJob()
	if first == nil {
		t.Fatal("premise lost: no selected job")
	}

	press(app, 'a')
	press(app, 'd')
	if app.chord.action != "d" {
		t.Fatalf("premise lost: A D did not arm (%q)", app.feedback.msg)
	}
	app.taskList.MoveDown() // what a wheel tick does
	if moved := app.taskList.SelectedJob(); moved == nil || moved.ID == first.ID {
		t.Fatalf("premise lost: the cursor did not move off %q", first.ID)
	}
	// The delete runs off the update loop, in the command the key returns.
	_, cmd := app.handleKey(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if cmd == nil {
		t.Fatalf("the confirm produced no command (%q)", app.feedback.msg)
	}
	cmd()
	if len(deleted) != 1 || deleted[0] != first.ID {
		t.Errorf("the confirm deleted %v; the prompt named %q", deleted, first.ID)
	}
}

// TestSelectionDropsJobsThatAreGone: nothing pruned the batch selection, so a
// selected job deleted from the dashboard (or missing from a fresh snapshot)
// kept "1 selected" in the status bar and armed batch confirms for a ghost.
//
// Mutant: drop the prune from rebuildJobIndex — both counts stay at 2/1.
func TestSelectionDropsJobsThatAreGone(t *testing.T) {
	m := NewTaskListModel()
	m.SetJobs([]*database.Job{
		{ID: "a", Title: "Alpha", Status: database.StatusFinished},
		{ID: "b", Title: "Beta", Status: database.StatusFinished},
		{ID: "c", Title: "Gamma", Status: database.StatusFinished},
	})
	m.ToggleSelection("a")
	m.ToggleSelection("b")

	m.RemoveJob("a")
	if got := m.SelectedIDs(); len(got) != 1 || got[0] != "b" {
		t.Errorf("after deleting a selected job: selection %v, want [b]", got)
	}
	m.SetJobs([]*database.Job{{ID: "c", Title: "Gamma", Status: database.StatusFinished}})
	if n := m.SelectedCount(); n != 0 {
		t.Errorf("after a snapshot without b: %d selected, want 0", n)
	}
}
