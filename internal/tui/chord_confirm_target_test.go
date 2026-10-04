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
