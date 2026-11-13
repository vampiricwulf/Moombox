package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The wording, which the dashboard's activeDownloadsRestartWarning
// (web/public/modules/utils.js) shares word for word.
func TestActiveRestartWording(t *testing.T) {
	if activeRestartClause(0, "restart") != "" || activeRestartWarning(0, "restart") != "" {
		t.Error("nothing active must say nothing")
	}
	if got := activeRestartClause(1, "restart"); got != "1 download is active — the restart interrupts them" {
		t.Errorf("clause(1) = %q", got)
	}
	want := "3 downloads are active — the update restart interrupts them, and live segments during the ~30s gap may be lost (Twitch especially)."
	if got := activeRestartWarning(3, "update restart"); got != want {
		t.Errorf("warning(3) = %q, want %q", got, want)
	}
}

// restartApp is an App with two jobs a restart interrupts and one it does
// not, and a restart callback that counts.
func restartApp(restarts *int) *App {
	app := NewApp()
	app.width, app.height = 120, 40
	app.OnRestart = func() { *restarts++ }
	app.statusMap["d"] = database.StatusDownloading
	app.statusMap["l"] = database.StatusLive
	app.statusMap["q"] = database.StatusQueued
	return app
}

// R P's confirm line names the downloads the restart interrupts, as the
// update restart's does. Mutant: the generic "Press P to confirm restart".
func TestRestartChordConfirmWarnsAboutActiveDownloads(t *testing.T) {
	restarts := 0
	app := restartApp(&restarts)
	press := func(r rune) { app.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)}) }
	press('r')
	press('p')
	want := "Press P to confirm restart: 2 downloads are active — the restart interrupts them (3s)"
	if app.feedback.msg != want {
		t.Errorf("confirm line = %q, want %q", app.feedback.msg, want)
	}
	if restarts != 0 {
		t.Fatal("R P restarted without its confirm")
	}

	// Nothing active: the line it always had.
	idle := NewApp()
	idle.OnRestart = func() {}
	idle.handleKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
	idle.handleKey(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if idle.feedback.msg != "Press P to confirm restart (3s)" {
		t.Errorf("idle confirm line = %q", idle.feedback.msg)
	}
}

// The update restart's line is the same clause, unchanged.
func TestUpdateConfirmUsesTheSharedClause(t *testing.T) {
	restarts := 0
	app := restartApp(&restarts)
	app.updateAvailable = &UpdateStatusMsg{TagName: "v9.9.9"}
	app.OnApplyUpdate = func(string) (string, bool) { return "", false }
	if cmd := app.applyUpdateAction(); cmd != nil {
		t.Fatal("the update ran without its second press")
	}
	want := "2 downloads are active — the update restart interrupts them; press R U again within 5s to confirm"
	if app.feedback.msg != want {
		t.Errorf("update line = %q, want %q", app.feedback.msg, want)
	}
}

// The action menu's confirm for Restart Program carries the same clause, in
// the normal form and whole in the bare one at the compact floor.
func TestMenuRestartConfirmWarnsAboutActiveDownloads(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {30, 6}} {
		restarts := 0
		app := restartApp(&restarts)
		app.width, app.height = size[0], size[1]
		app.actionMenu.SetSize(size[0], size[1])
		app.actionMenu.Open(app.buildMenuItems())
		for i, it := range app.actionMenu.mainList.Items() {
			if mi, ok := it.(menuActionItem); ok && mi.action != nil && mi.action.Chord == "R P" {
				app.actionMenu.mainList.Select(i)
			}
		}
		app.actionMenu.HandleKey(keyEnter)
		if app.actionMenu.mode != menuConfirm {
			t.Fatalf("%v: menu mode = %v, want the confirm", size, app.actionMenu.mode)
		}
		view := stripANSI(app.actionMenu.View())
		flat := strings.Join(strings.Fields(view), " ")
		if !strings.Contains(flat, "2 downloads are active — the restart interrupts them") {
			t.Errorf("%v: the confirm does not carry the warning:\n%s", size, view)
		}
		if rows := strings.Count(view, "\n") + 1; size[1] == 6 && rows != 6 {
			t.Errorf("%v: bare confirm is %d rows", size, rows)
		}
	}
}

// The Settings restart prompt gives the warning from the App's own count,
// read when drawn; with nothing active it is the prompt it always was.
func TestSettingsRestartPromptWarnsAboutActiveDownloads(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {60, 20}, {40, 10}, {30, 6}} {
		restarts := 0
		app := restartApp(&restarts)
		app.settings.SetSize(size[0], size[1])
		app.settings.visible = true
		app.settings.showRestartOverlay = true

		view := stripANSI(app.settings.View())
		flat := strings.Join(strings.Fields(strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ").Replace(view)), " ")
		if !strings.Contains(flat, "2 downloads are active — the restart interrupts them") {
			t.Errorf("%v: the restart prompt does not warn:\n%s", size, view)
		}
		if !strings.Contains(strings.ToLower(flat), "enter: restart now") {
			t.Errorf("%v: the warning pushed the keys off the prompt:\n%s", size, view)
		}
		if rows := strings.Count(view, "\n") + 1; rows > size[1] {
			t.Errorf("%v: the prompt is %d rows tall", size, rows)
		}

		// The count is read when drawn, not copied.
		delete(app.statusMap, "d")
		delete(app.statusMap, "l")
		if idle := stripANSI(app.settings.View()); strings.Contains(idle, "active") {
			t.Errorf("%v: the prompt still warns with nothing active:\n%s", size, idle)
		}
	}
}
