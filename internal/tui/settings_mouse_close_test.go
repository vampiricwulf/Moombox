package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// TestMouseSaveAndReturnRunsTheCloseHook: Esc and Enter close the settings
// overlay through handleKey's "close" action, which re-applies the archive
// threshold to the task list. A click on [ Save & Return ] closed it inside
// HandleMouse and returned, so the Tasks panel kept the old threshold until
// the 60 s sweep.
//
// Mutant: drop the afterSettingsClose call in handleMouse.
func TestMouseSaveAndReturnRunsTheCloseHook(t *testing.T) {
	cfg := config.Defaults()
	a := NewApp()
	a.SetConfig(cfg)
	a.SetConfigStore(config.NewStore(cfg, ""))
	a.settings.OnSave = func(*config.MoomboxConfig) error { return nil }
	a.settings.SetSize(120, 40)
	a.settings.Open(cfg)
	a.settings.values["hide_finished_age_days"] = "5"
	a.settings.recheckDirty()

	view := stripANSI(a.settings.View())
	x, y := -1, -1
	for row, line := range strings.Split(view, "\n") {
		if i := strings.Index(line, "[ Save & Return ]"); i >= 0 {
			x, y = len([]rune(line[:i]))+2, row
			break
		}
	}
	if y < 0 {
		t.Fatalf("no Save & Return button in the view:\n%s", view)
	}

	a.handleMouse(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})

	if a.settings.IsVisible() {
		t.Fatalf("the click at (%d,%d) did not close the overlay", x, y)
	}
	if got := a.taskList.hideFinishedAgeDays; got != 5 {
		t.Errorf("task list threshold = %v after the mouse save, want 5", got)
	}
}
