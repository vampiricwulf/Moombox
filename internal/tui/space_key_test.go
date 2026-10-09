package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// realSpace is the keypress a terminal delivers for Space: ultraviolet's
// decoder maps byte 0x20 to KeyPressEvent{Code: KeySpace, Text: " "}.
func realSpace() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "} }

// The keys that bind Space matched " ", but a Space keypress stringifies to
// "space", so no terminal ever reached them: Space ticked no task for a batch
// (help.go and the spec promise it does), and every "Space to toggle" in the
// notification editor did nothing. The editor's own tests drove its key
// handler with " " directly and never saw it.
//
// Mutants: keySpace back to " " — the pin fails; app_keys.go's arm back to
// `case " ":` — the real Space ticks nothing; settings_notifications.go's arm
// back to `case " ":` — TestNotifEditTogglesDeliveryMode and
// TestNotifEditEnabledToggleRoundTrips, which press keySpace, fail.
func TestSpaceReachesTheKeysThatBindIt(t *testing.T) {
	if got := realSpace().String(); got != keySpace {
		t.Fatalf("a Space keypress stringifies to %q, keySpace is %q", got, keySpace)
	}

	app := NewApp()
	app.taskList.SetSize(100, 30)
	app.taskList.SetJobs([]*database.Job{
		{ID: "err", Title: "Broken", Status: database.StatusError, Platform: "youtube"},
	})
	app.focusedPanel = PanelTasks
	app.Update(realSpace())
	if got := app.taskList.SelectedIDs(); len(got) != 1 || got[0] != "err" {
		t.Fatalf("selection after Space = %v, want the cursor row [err]", got)
	}
	app.Update(realSpace())
	if n := app.taskList.SelectedCount(); n != 0 {
		t.Errorf("%d selected after a second Space, want the tick toggled off", n)
	}

	// Only on the Tasks panel.
	app.focusedPanel = PanelLogs
	app.Update(realSpace())
	if n := app.taskList.SelectedCount(); n != 0 {
		t.Errorf("Space on the Logs panel ticked %d task(s)", n)
	}
}
