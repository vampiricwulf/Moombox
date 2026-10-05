package tui

import (
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
