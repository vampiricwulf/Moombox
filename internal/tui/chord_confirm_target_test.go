package tui

import (
	"strings"
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

// TestSearchMovesTheDetailsPanelWithTheCursor: every query change re-filters
// and puts the cursor on the first match, but none of the search paths told
// the details panel, so the highlighted row and the panel beside it showed
// different jobs — and the next chord acts on the highlighted one. Paging and
// arrows already refresh it (TestTaskPanelPagingRefreshesTheDetailsPanel).
//
// Mutant: drop the updateSelectedJob calls on both search paths — the panel
// still shows Alpha after "beta" is typed. (A typed key reaches both
// UpdateSearchInput and HandleSearchKey, so either alone masks the other; a
// paste reaches only UpdateSearchInput.)
func TestSearchMovesTheDetailsPanelWithTheCursor(t *testing.T) {
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	a.taskList.SetJobs([]*database.Job{
		{ID: "a", Title: "Alpha", Status: database.StatusLive, Platform: "youtube"},
		{ID: "b", Title: "Beta", Status: database.StatusLive, Platform: "youtube"},
	})
	a.focusedPanel = PanelTasks
	a.updateSelectedJob()
	if a.details.job == nil || a.details.job.ID != "a" {
		t.Fatalf("premise lost: details show %v, want a", a.details.job)
	}

	a.handleKey(tea.KeyPressMsg{Code: '/', Text: "/"})
	if !a.taskList.IsSearching() {
		t.Fatal("premise lost: / did not open the search box")
	}
	for _, r := range "beta" {
		a.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	sel := a.taskList.SelectedJob()
	if sel == nil || sel.ID != "b" {
		t.Fatalf("premise lost: the filter did not select Beta (%v)", sel)
	}
	if a.details.job == nil || a.details.job.ID != "b" {
		t.Errorf("after typing, the cursor is on b but the details panel shows %v", a.details.job)
	}

	// Esc closes the box, a second Esc clears the applied query; the cursor
	// lands on the first row again and the panel must follow.
	a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if sel := a.taskList.SelectedJob(); sel != nil && (a.details.job == nil || a.details.job.ID != sel.ID) {
		t.Errorf("after clearing the search, the cursor is on %s but the details panel shows %v", sel.ID, a.details.job)
	}
}

// TestBatchChordsCountWhatTheyWillActOn: the batch confirm prompt counted the
// whole selection — "Press D to confirm delete 1 jobs" for one Downloading
// job, whose confirm then found nothing deletable — and A R / A I reported
// "Resumed 0 jobs" as a success. The Web's batch bar counts eligible targets
// and hides a verb with none. The prefix hint also hid batch chords whenever
// the CURSOR job was ineligible, though pressing them acted on the batch.
//
// Mutants: count SelectedCount() again (the mixed case says 2 jobs); drop the
// A R zero arm (a green "Resumed 0 jobs"); drop the batch term in
// chordFeedback (D is missing from the hint).
func TestBatchChordsCountWhatTheyWillActOn(t *testing.T) {
	press := func(app *App, r rune) {
		app.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	newApp := func() *App {
		app := NewApp()
		app.OnDeleteJob = func(string) {}
		app.OnResumeJob = func(string) {}
		app.HasStagingFiles = func(string) bool { return true }
		app.taskList.SetJobs([]*database.Job{
			{ID: "live", Title: "Live one", Status: database.StatusDownloading, Platform: "youtube", CreatedAt: "2026-10-04T10:00:00Z"},
			{ID: "done", Title: "Done one", Status: database.StatusFinished, Platform: "youtube", CreatedAt: "2026-10-04T09:00:00Z"},
		})
		return app
	}

	// Only the Downloading job selected: nothing to delete, so no prompt.
	app := newApp()
	app.taskList.ToggleSelection("live")
	press(app, 'a')
	press(app, 'd')
	if app.chord.action != "" || app.feedback.sev != severityWarning {
		t.Errorf("A D on a selection with nothing deletable armed %q / said %q", app.chord.action, app.feedback.msg)
	}

	// Mixed: one deletable of two selected → "1 job", singular.
	app = newApp()
	app.taskList.ToggleSelection("live")
	app.taskList.ToggleSelection("done")
	press(app, 'a')
	press(app, 'd')
	if want := "Press D to confirm delete 1 job (3s)"; app.feedback.msg != want {
		t.Errorf("mixed selection prompt = %q, want %q", app.feedback.msg, want)
	}

	// A R over a selection with nothing resumable: a warning, not "Resumed 0".
	app = newApp()
	app.taskList.ToggleSelection("live")
	press(app, 'a')
	press(app, 'r')
	if app.feedback.sev != severityWarning {
		t.Errorf("A R with nothing resumable said %q (severity %v), want a warning", app.feedback.msg, app.feedback.sev)
	}

	// The hint: cursor on the Downloading job, the Finished one selected.
	app = newApp()
	if sel := app.taskList.SelectedJob(); sel == nil || sel.ID != "live" {
		t.Fatalf("premise lost: cursor on %v, want live", sel)
	}
	app.taskList.ToggleSelection("done")
	press(app, 'a')
	if !strings.Contains(app.feedback.msg, "D Delete") {
		t.Errorf("hint %q hides D, though A D would delete the selected job", app.feedback.msg)
	}
}
