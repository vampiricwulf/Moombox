package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// menuItem returns the built menu entry for a chord, or nil.
func menuItem(app *App, chord string) *ActionMenuItem {
	items := app.buildMenuItems()
	for i := range items {
		if items[i].Chord == chord {
			return &items[i]
		}
	}
	return nil
}

// Owner ruling R3: A D carries the Web's DELETE_STATUSES
// (web/public/modules/utils.js) — Finished, Error, Cancelled, COOKIES? and
// nothing else. Mutant: a nil JobFilter (what it had), which offers Delete
// for a running download.
func TestDeleteChordCarriesTheWebsStatusSet(t *testing.T) {
	app := NewApp()
	item := menuItem(app, "A D")
	if item == nil || item.JobFilter == nil {
		t.Fatalf("A D missing or unfiltered: %+v", item)
	}
	for _, s := range []database.JobStatus{
		database.StatusFinished, database.StatusError,
		database.StatusCancelled, database.StatusCookies,
	} {
		if !item.JobFilter(&database.Job{Status: s}) {
			t.Errorf("A D must offer Delete for %s — the Web does", s)
		}
	}
	for _, s := range []database.JobStatus{
		database.StatusDownloading, database.StatusLive,
		database.StatusUpcoming, database.StatusQueued, database.StatusMuxing,
	} {
		if item.JobFilter(&database.Job{Status: s}) {
			t.Errorf("A D offers Delete for %s; the Web hides it (DELETE_STATUSES)", s)
		}
	}
}

// The two surfaces the filter drives: the action menu's disabled reason and
// the job selector it opens.
func TestDeleteMenuEntryIsDisabledWithOnlyActiveJobs(t *testing.T) {
	app := NewApp()
	app.OnDeleteJob = func(string) {}
	app.taskList.SetJobs([]*database.Job{{ID: "d", Title: "Downloading one", Status: database.StatusDownloading}})
	app.actionMenu.SetJobs(app.taskList.Jobs())
	app.actionMenu.SetSize(100, 40)
	app.actionMenu.Open(app.buildMenuItems())

	if got := stripANSI(app.actionMenu.View()); !strings.Contains(got, "no deletable jobs") {
		t.Errorf("the Delete entry must render its disabled reason with only an active job:\n%s", got)
	}
	if got := app.actionMenu.filterJobs(menuItem(app, "A D").JobFilter); len(got) != 0 {
		t.Errorf("A D's job selector offered %d active job(s)", len(got))
	}
}

// The batch arm never sees JobFilter (dispatchAction is handed a nil job), so
// it applies the same rule itself — as A C and A W already do. Mutant:
// filtering the menu but deleting the whole selection anyway.
func TestDeleteBatchSkipsActiveJobs(t *testing.T) {
	app := NewApp()
	var deleted []string
	app.OnDeleteJob = func(id string) { deleted = append(deleted, id) }

	done := &database.Job{ID: "f1", Title: "done", Status: database.StatusFinished}
	down := &database.Job{ID: "d1", Title: "downloading", Status: database.StatusDownloading}
	app.taskList.SetJobs([]*database.Job{done, down})
	for _, id := range []string{"f1", "d1"} {
		app.taskList.ToggleSelection(id)
	}
	if _, cmd := app.dispatchAction("A D", nil); cmd != nil {
		cmd()
	}
	if len(deleted) != 1 || deleted[0] != "f1" {
		t.Fatalf("batch deleted %v, want only the Finished job", deleted)
	}
	if app.taskList.SelectedCount() != 0 {
		t.Error("a fired batch must clear the selection")
	}

	// A selection with nothing deletable says so instead of firing.
	deleted = nil
	app.taskList.ToggleSelection("d1")
	if _, cmd := app.dispatchAction("A D", nil); cmd != nil {
		cmd()
	}
	if len(deleted) != 0 {
		t.Errorf("an all-active selection deleted %v", deleted)
	}
	if !strings.Contains(app.feedback.msg, "No deletable jobs in selection") {
		t.Errorf("feedback = %q, want it to say the selection held nothing deletable", app.feedback.msg)
	}
}
