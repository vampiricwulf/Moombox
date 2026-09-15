package tui

import (
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

// runCmd executes cmd — flattening tea.Batch, whose message is the list of
// commands it batched rather than anything a model reacts to — and returns
// the one message the test is waiting on.
//
// Spinner ticks are skipped rather than returned: every async overlay batches
// its first tick alongside the command that does the work, and the tick is
// noise here. It takes *testing.T so an empty command tree fails at the call
// site instead of feeding App.Update a nil message that silently does nothing.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msg := firstRealMsg(cmd)
	if msg == nil {
		t.Fatal("command produced no message")
	}
	return msg
}

func firstRealMsg(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		for _, sub := range msg {
			if got := firstRealMsg(sub); got != nil {
				return got
			}
		}
		return nil
	case spinner.TickMsg:
		return nil
	default:
		return msg
	}
}

// TestYtdlpDialogShowsStatusAndInstalls: the overlay loads through the status
// callback, renders installed/dir/port mismatch, and I runs the install
// callback then reloads.
func TestYtdlpDialogShowsStatusAndInstalls(t *testing.T) {
	app := NewApp()
	statusCalls, installCalls := 0, 0
	app.OnYtdlpPluginStatus = func() (ytdlpplugin.Info, error) {
		statusCalls++
		return ytdlpplugin.Info{
			Installed:     statusCalls > 1,
			PluginDir:     "/plug",
			CurrentPort:   7740,
			InstalledPort: intPtr(7739),
			PortMismatch:  statusCalls == 1,
		}, nil
	}
	app.OnInstallYtdlpPlugin = func() error { installCalls++; return nil }
	if !ryOffered(t, app) {
		t.Fatal("R Y missing with the callbacks wired")
	}

	_, cmd := app.dispatchAction("R Y", nil)
	if !app.ytdlpDlg.IsVisible() || !strings.Contains(app.ytdlpDlg.View(), "Loading") {
		t.Fatal("R Y must open the overlay in its loading state")
	}
	app.Update(runCmd(t, cmd)) // execute the batch and feed its status msg back

	v := app.ytdlpDlg.View()
	for _, want := range []string{"not installed", "/plug", "7740", "7739", "mismatch"} {
		if !strings.Contains(strings.ToLower(v), strings.ToLower(want)) {
			t.Errorf("status view lacks %q:\n%s", want, v)
		}
	}

	_, cmd = app.handleKey(tea.KeyPressMsg{Code: 'i', Text: "i"})
	if !strings.Contains(app.ytdlpDlg.View(), "Installing") {
		t.Errorf("I did not move the overlay to its installing state:\n%s", app.ytdlpDlg.View())
	}
	// Two hops, not one: the install result arm answers with the reload
	// command, and the reload is the half that refreshes what is on screen.
	_, cmd = app.Update(runCmd(t, cmd))
	app.Update(runCmd(t, cmd))
	if installCalls != 1 || statusCalls != 2 {
		t.Fatalf("install=%d status=%d, want 1 and 2 (install then reload)", installCalls, statusCalls)
	}
	v = app.ytdlpDlg.View()
	if strings.Contains(v, "not installed") {
		t.Errorf("view not refreshed after install:\n%s", v)
	}
	if !strings.Contains(v, "yes") {
		t.Errorf("the reloaded status does not report the plugin as installed:\n%s", v)
	}

	app.OnYtdlpPluginStatus = nil
	if ryOffered(t, app) {
		t.Fatal("R Y offered without the status callback")
	}
}

// TestYtdlpDialogReportsErrors: a failing status load and a failing install
// both land in the overlay rather than vanishing, and I with no install
// callback says so instead of doing nothing.
func TestYtdlpDialogReportsErrors(t *testing.T) {
	app := NewApp()
	app.OnYtdlpPluginStatus = func() (ytdlpplugin.Info, error) {
		return ytdlpplugin.Info{}, errors.New("cannot determine yt-dlp plugin directory")
	}
	_, cmd := app.dispatchAction("R Y", nil)
	app.Update(runCmd(t, cmd))
	if !strings.Contains(app.ytdlpDlg.View(), "cannot determine yt-dlp plugin directory") {
		t.Errorf("the status error is not rendered:\n%s", app.ytdlpDlg.View())
	}

	// I with no install callback: the chord exists (the status callback is
	// what gates it), so the key must explain itself rather than no-op.
	app.handleKey(tea.KeyPressMsg{Code: 'i', Text: "i"})
	if !strings.Contains(strings.ToLower(app.ytdlpDlg.View()), "unavailable") {
		t.Errorf("I with no install callback said nothing:\n%s", app.ytdlpDlg.View())
	}

	app.OnInstallYtdlpPlugin = func() error { return errors.New("permission denied") }
	_, cmd = app.handleKey(tea.KeyPressMsg{Code: 'i', Text: "i"})
	app.Update(runCmd(t, cmd))
	if !strings.Contains(app.ytdlpDlg.View(), "Install failed: permission denied") {
		t.Errorf("the install error is not rendered:\n%s", app.ytdlpDlg.View())
	}

	if !app.ytdlpDlg.IsVisible() {
		t.Fatal("the overlay closed on its own")
	}
	app.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.ytdlpDlg.IsVisible() {
		t.Error("Esc did not close the overlay")
	}
}

// TestYtdlpDialogRefreshReloads: R re-runs the status callback and returns to
// the loading state while it does.
func TestYtdlpDialogRefreshReloads(t *testing.T) {
	app := NewApp()
	calls := 0
	app.OnYtdlpPluginStatus = func() (ytdlpplugin.Info, error) {
		calls++
		return ytdlpplugin.Info{Installed: true, PluginDir: "/plug", CurrentPort: 7740, InstalledPort: intPtr(7740)}, nil
	}
	_, cmd := app.dispatchAction("R Y", nil)
	app.Update(runCmd(t, cmd))
	if calls != 1 {
		t.Fatalf("open ran the status callback %d times, want 1", calls)
	}
	_, cmd = app.handleKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
	if !strings.Contains(app.ytdlpDlg.View(), "Loading") {
		t.Errorf("R did not return the overlay to its loading state:\n%s", app.ytdlpDlg.View())
	}
	app.Update(runCmd(t, cmd))
	if calls != 2 {
		t.Fatalf("R ran the status callback %d times in total, want 2", calls)
	}
}

// TestYtdlpPluginChordExistsOnlyWhenWired: nil status callback ⇒ no R Y in
// dispatch, menu or help; wired ⇒ all three, and dispatch opens the overlay.
func TestYtdlpPluginChordExistsOnlyWhenWired(t *testing.T) {
	app := NewApp()
	app.OnYtdlpPluginStatus = nil
	if ryOffered(t, app) {
		t.Fatal("R Y offered with no callback")
	}
	app.dispatchAction("R Y", nil)
	if app.ytdlpDlg.IsVisible() {
		t.Fatal("dispatching R Y with no callback opened an overlay that can never load")
	}
	app.OnYtdlpPluginStatus = func() (ytdlpplugin.Info, error) {
		return ytdlpplugin.Info{}, nil
	}
	if !ryOffered(t, app) {
		t.Fatal("R Y missing with the callback wired")
	}
	app.dispatchAction("R Y", nil)
	if !app.ytdlpDlg.IsVisible() {
		t.Fatal("dispatching R Y did not open the overlay")
	}
}

// ryOffered reports whether R Y is reachable by the two derived routes an
// operator has — the action menu and the help overlay, which is built from
// the menu. Same shape as riOffered (cookie_import_dialog_test.go).
func ryOffered(t *testing.T, app *App) bool {
	t.Helper()
	items := app.buildMenuItems()
	if len(items) == 0 {
		t.Fatal("buildMenuItems returned nothing — nothing below can be concluded")
	}
	menu := false
	for _, it := range items {
		if strings.TrimSpace(it.Chord) == "R Y" {
			menu = true
		}
	}
	h := NewHelpModel()
	h.SetMenuItems(items)
	help := false
	for _, sec := range h.orderedSections() {
		for _, k := range sec.keys {
			if strings.TrimSpace(k.key) == "R Y" {
				help = true
			}
		}
	}
	if menu != help {
		t.Fatalf("R Y is in the menu (%v) but not the help overlay (%v) — the two reads disagree", menu, help)
	}
	return menu
}

// TestYtdlpDialogFlagsAnUnrecognisedFile: the overlay renders the same verdict
// the dashboard's card does. Without it an unparseable plugin file shows
// "Installed: yes" with no port row and nothing to explain either.
//
// Mutant: dropping the Unparseable row from statusRows fails this.
func TestYtdlpDialogFlagsAnUnrecognisedFile(t *testing.T) {
	app := NewApp()
	app.OnYtdlpPluginStatus = func() (ytdlpplugin.Info, error) {
		return ytdlpplugin.Info{
			Installed:   true,
			Unparseable: true,
			PluginDir:   "/plug",
			CurrentPort: 774,
			// The shape Status really produces: ExtractedPath is filled
			// whenever the plugin dir is known, and Installed can only become
			// true inside that block — so this row is present in 100% of real
			// occurrences, and a fixture without it hides a label collision.
			ExtractedPath: "/plug/moombox",
		}, nil
	}
	_, cmd := app.dispatchAction("R Y", nil)
	app.Update(runCmd(t, cmd))

	v := app.ytdlpDlg.View()
	if !strings.Contains(v, "not recognized") {
		t.Errorf("the overlay does not report the unrecognised file:\n%s", v)
	}
	if !strings.Contains(v, "I reinstalls") {
		t.Errorf("the overlay does not say which key fixes it:\n%s", v)
	}
	// One label per row. The path row is on screen alongside this one in
	// every real occurrence, so a shared label would read as a render bug.
	//
	// Mutant: labelling either row with the other's name makes one count 2
	// and the other 0, and fails this.
	if n := strings.Count(v, "Plugin state:"); n != 1 {
		t.Errorf("the unrecognised-file row's label appears %d times, want exactly 1:\n%s", n, v)
	}
	if n := strings.Count(v, "Plugin path:"); n != 1 {
		t.Errorf("the plugin-path row's label appears %d times, want exactly 1:\n%s", n, v)
	}
}
