package tui

import (
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// A job the filter hides leaves the selection. It used to stay selected, so
// a batch chord counted it in its confirm and deleted it while only the
// filter's rows were on screen.
//
// Mutant: the delete in rebuildVirtualList's filter gate removed — the hidden
// Finished job is still selected and A D D deletes it.
func TestAFilterDropsTheSelectionItHides(t *testing.T) {
	app := NewApp()
	app.taskList.SetSize(100, 30)
	var deleted []string
	app.OnDeleteJob = func(id string) { deleted = append(deleted, id) }
	fin := &database.Job{ID: "fin", Title: "Old finished", Status: database.StatusFinished, Platform: "youtube",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	live := &database.Job{ID: "live", Title: "Live now", Status: database.StatusLive, Platform: "youtube"}
	app.taskList.SetJobs([]*database.Job{fin, live})
	app.taskList.ToggleSelection("fin")

	app.taskList.applyQuery("status:active")
	if n := app.taskList.SelectedCount(); n != 0 {
		t.Errorf("%d selected after the filter hid the only selected job", n)
	}

	app.focusedPanel = PanelTasks
	key := func(s string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: []rune(s)[0], Text: s} }
	app.handleKey(key("a"))
	app.handleKey(key("d"))
	if _, cmd := app.handleKey(key("d")); cmd != nil {
		cmd()
	}
	for _, id := range deleted {
		if id == "fin" {
			t.Error("A D D deleted a job the filter hides")
		}
	}

	// Clearing the filter does not bring the dropped selection back.
	app.taskList.applyQuery("")
	if n := app.taskList.SelectedCount(); n != 0 {
		t.Errorf("%d selected after the filter was cleared", n)
	}
}

// A collapsed archive hides its rows as surely as the filter does, so they
// leave the selection too. They used to stay selected: a row ticked while the
// archive was open was counted in the next batch's confirm ("delete 2 jobs"
// with one ✓ on screen) and deleted by it, and so was a ticked Finished row
// that aged into the collapsed archive at the minute's resweep.
//
// Mutants: the delete in rebuildVirtualList's archived arm removed — the
// collapsed archive keeps "arch" selected and A D D deletes it, and the aged
// row stays selected; its `!m.archiveExpanded` guard dropped — a row ticked in
// the open archive is lost at the next rebuild.
func TestACollapsedArchiveDropsTheSelectionItHides(t *testing.T) {
	app := NewApp()
	app.taskList.SetSize(100, 30)
	var deleted []string
	app.OnDeleteJob = func(id string) { deleted = append(deleted, id) }
	old := time.Now().Add(-60 * 24 * time.Hour).UTC().Format(time.RFC3339)
	arch := &database.Job{ID: "arch", Title: "Old finished", Status: database.StatusFinished, Platform: "youtube", UpdatedAt: old}
	errJob := &database.Job{ID: "err", Title: "Broken", Status: database.StatusError, Platform: "youtube", UpdatedAt: old}
	m := app.taskList
	m.SetJobs([]*database.Job{arch, errJob})

	m.ToggleArchive() // open
	m.ToggleSelection("arch")
	// A rebuild while the archive is open keeps the row it shows.
	m.UpdateJob(errJob)
	if got := m.SelectedIDs(); len(got) != 1 || got[0] != "arch" {
		t.Fatalf("selection with the archive open = %v, want [arch]", got)
	}

	m.ToggleArchive() // collapse — "arch" is no longer on screen
	if slices.Contains(visibleJobIDs(m), "arch") {
		t.Fatal("precondition: the collapsed archive must hide its row")
	}
	m.ToggleSelection("err")
	if got := m.SelectedIDs(); len(got) != 1 || got[0] != "err" {
		t.Errorf("selection after collapsing the archive = %v, want [err]: the hidden row is a batch target", got)
	}

	app.focusedPanel = PanelTasks
	key := func(s string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: []rune(s)[0], Text: s} }
	app.handleKey(key("a"))
	app.handleKey(key("d"))
	if _, cmd := app.handleKey(key("d")); cmd != nil {
		cmd()
	}
	if !slices.Equal(deleted, []string{"err"}) {
		t.Errorf("A D D deleted %v, want only the row on screen [err]", deleted)
	}

	// Opening the archive again does not bring the dropped selection back.
	m.ToggleArchive()
	if slices.Contains(m.SelectedIDs(), "arch") {
		t.Error("reopening the archive brought back a selection it had dropped")
	}

	// A ticked Finished row that ages into the collapsed archive at the
	// minute's resweep leaves the selection the same way.
	a := NewTaskListModel()
	a.SetSize(100, 30)
	a.SetHideFinishedAgeDays(1)
	fin := &database.Job{ID: "fin", Title: "Fresh finished", Status: database.StatusFinished, Platform: "youtube",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	a.SetJobs([]*database.Job{fin})
	a.ToggleSelection("fin")
	fin.UpdatedAt = time.Now().Add(-3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if !a.ResweepArchive() {
		t.Fatal("precondition: the aged row must re-bucket into the archive")
	}
	if n := a.SelectedCount(); n != 0 {
		t.Errorf("%d selected after the ticked row aged into the collapsed archive", n)
	}
}
