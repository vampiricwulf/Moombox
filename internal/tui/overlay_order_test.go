package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// overlayCase is one overlay as a test sees it, written independently of
// the registry: opened through its own production entry point, its
// visibility and its own view read straight off the model.
type overlayCase struct {
	name    string
	open    func(*App)
	visible func(*App) bool
	view    func(*App) string
}

// overlayCases lists every overlay in the order handleKey, View and
// routeComponentMsg have always checked them (K23). It is written out
// here, apart from the registry, so that reordering the registry is a test
// failure and not a silent change of which overlay a key reaches.
func overlayCases(t *testing.T) []overlayCase {
	return []overlayCase{
		{"settings", func(a *App) { a.settings.Open(a.cfg) },
			func(a *App) bool { return a.settings.IsVisible() }, func(a *App) string { return a.settings.View() }},
		{"help", func(a *App) { a.help.Toggle() },
			func(a *App) bool { return a.help.IsVisible() }, func(a *App) string { return a.help.View() }},
		{"release notes", func(a *App) { a.releaseNotesPopup.open("v9.9.9", "# notes", a.width, a.height) },
			func(a *App) bool { return a.releaseNotesPopup.isOpen() }, func(a *App) string { return a.releaseNotesPopup.View() }},
		{"ffmpeg check", func(a *App) { a.ffmpegCheck.Open() },
			func(a *App) bool { return a.ffmpegCheck.IsVisible() }, func(a *App) string { return a.ffmpegCheck.View() }},
		{"setup wizard", func(a *App) { a.setupWiz.Open() },
			func(a *App) bool { return a.setupWiz.IsVisible() }, func(a *App) string { return a.setupWiz.View() }},
		{"action menu", func(a *App) { a.actionMenu.Open(a.buildMenuItems()) },
			func(a *App) bool { return a.actionMenu.IsVisible() }, func(a *App) string { return a.actionMenu.View() }},
		{"import", func(a *App) { a.importDlg.Open(t.TempDir()) },
			func(a *App) bool { return a.importDlg.IsVisible() }, func(a *App) string { return a.importDlg.View() }},
		{"cookie import", func(a *App) { a.cookieImportDlg.Open() },
			func(a *App) bool { return a.cookieImportDlg.IsVisible() }, func(a *App) string { return a.cookieImportDlg.View() }},
		{"chapter import", func(a *App) { a.chapterDlg.Open(a.taskList.GetJobByID("j1")) },
			func(a *App) bool { return a.chapterDlg.IsVisible() }, func(a *App) string { return a.chapterDlg.View() }},
		{"add video", func(a *App) { a.addVideo.Open() },
			func(a *App) bool { return a.addVideo.IsVisible() }, func(a *App) string { return a.addVideo.View() }},
		{"trim", func(a *App) { a.trimDlg.Open("j1", "a job") },
			func(a *App) bool { return a.trimDlg.IsVisible() }, func(a *App) string { return a.trimDlg.View() }},
		{"files", func(a *App) { a.filesDlg.Open() },
			func(a *App) bool { return a.filesDlg.IsVisible() }, func(a *App) string { return a.filesDlg.View() }},
		{"client tokens", func(a *App) { a.clientTokensDlg.Open() },
			func(a *App) bool { return a.clientTokensDlg.IsVisible() }, func(a *App) string { return a.clientTokensDlg.View() }},
		{"ytdlp plugin", func(a *App) { a.ytdlpDlg.Open() },
			func(a *App) bool { return a.ytdlpDlg.IsVisible() }, func(a *App) string { return a.ytdlpDlg.View() }},
		{"stats", func(a *App) { a.statsDlg.Open() },
			func(a *App) bool { return a.statsDlg.IsVisible() }, func(a *App) string { return a.statsDlg.View() }},
		{"job log", func(a *App) { a.jobLog.Open(a.taskList.GetJobByID("j1")) },
			func(a *App) bool { return a.jobLog.IsVisible() }, func(a *App) string { return a.jobLog.View() }},
	}
}

// overlayTestApp is a sized App with one Finished job, enough for every
// overlay's production opener.
func overlayTestApp(t *testing.T) *App {
	t.Helper()
	a := NewApp()
	a.SetConfigStore(config.NewStore(config.Defaults(), ""))
	a.width, a.height = 120, 40
	a.recalcLayout()
	a.taskList.SetJobs([]*database.Job{
		{ID: "j1", Title: "a job", Status: database.StatusFinished, Platform: "youtube", VideoID: "aaaaaaaaaaa"},
	})
	return a
}

// With overlay i and every lower-priority overlay open at once, the screen
// shows overlay i and Esc reaches only overlay i. Overlays are exclusive in
// practice; this pins the order the code applies if they were not.
//
// Mutant: swapping two adjacent overlays in the order fails the View check
// for the higher one.
func TestOverlayPriorityViewAndKey(t *testing.T) {
	cases := overlayCases(t)
	for i, c := range cases {
		a := overlayTestApp(t)
		for _, lower := range cases[i:] {
			lower.open(a)
		}
		if !c.visible(a) {
			t.Fatalf("%s: did not open", c.name)
		}
		if got, want := a.View().Content, a.viewWithMode(c.view(a)).Content; got != want {
			t.Errorf("%s: the screen does not show it while lower overlays are also open", c.name)
		}

		// Esc goes to overlay i only: every lower overlay stays open.
		_, _ = a.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
		for _, lower := range cases[i+1:] {
			if !lower.visible(a) {
				t.Errorf("%s on top: Esc reached %s underneath", c.name, lower.name)
			}
		}
	}
}

// The registry is the one place the order lives: it must be exactly the
// order the five hand-written chains had, every entry needs the three
// required handlers, and every entry needs a case above — so a new overlay
// cannot skip the census below or the Ctrl+C census.
func TestOverlayRegistryOrder(t *testing.T) {
	cases := overlayCases(t)
	if len(overlayRegistry) != len(cases) {
		t.Fatalf("registry has %d overlays, overlayCases %d: add the new overlay's case", len(overlayRegistry), len(cases))
	}
	for i, o := range overlayRegistry {
		if o.name != cases[i].name {
			t.Errorf("registry[%d] = %q, want %q", i, o.name, cases[i].name)
		}
		if o.visible == nil || o.view == nil || o.key == nil {
			t.Errorf("overlay %q: visible, view and key are all required", o.name)
		}
	}
}

// Each registry entry reads the same overlay its case opens: the entry is
// the active one exactly when its own overlay is the only one open.
func TestOverlayRegistryEntriesMatchTheirOverlays(t *testing.T) {
	for i, c := range overlayCases(t) {
		a := overlayTestApp(t)
		if a.hasActiveOverlay() {
			t.Fatalf("an overlay is open before any was opened")
		}
		c.open(a)
		top := a.activeOverlay()
		if top == nil || top != &overlayRegistry[i] {
			t.Errorf("%s: activeOverlay = %v", c.name, top)
			continue
		}
		if top.view(a) != c.view(a) {
			t.Errorf("%s: the registry renders some other overlay", c.name)
		}
	}
}

// The mouse follows the key order. It used to check the action menu ahead
// of settings — unreachable today, since overlays are exclusive.
//
// Mutant: giving the action menu's entry priority in handleMouse again —
// the wheel moves the menu's cursor.
func TestOverlayMouseFollowsKeyOrder(t *testing.T) {
	a := overlayTestApp(t)
	a.settings.Open(a.cfg)
	a.actionMenu.Open(a.buildMenuItems())
	before := a.actionMenu.mainList.Index()
	_, _ = a.handleMouse(tea.MouseWheelMsg{X: 10, Y: 10, Button: tea.MouseWheelDown})
	if a.actionMenu.mainList.Index() != before {
		t.Fatalf("a wheel notch reached the action menu under settings")
	}
}
