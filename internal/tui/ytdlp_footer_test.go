package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

// TestYtdlpDialogFooterNamesEveryCloseKey: HandleKey has always closed the
// overlay on Q as well as Esc, but the footer named only Esc — the stats
// overlay beside it says "Esc/Q: Close". The hint now matches the handler, and
// the handler is pinned alongside it so the two cannot drift apart again.
func TestYtdlpDialogFooterNamesEveryCloseKey(t *testing.T) {
	app := NewApp()
	app.OnYtdlpPluginStatus = func() (ytdlpplugin.Info, error) {
		return ytdlpplugin.Info{Installed: true, PluginDir: "/plug", CurrentPort: 7740, InstalledPort: intPtr(7740)}, nil
	}
	_, cmd := app.dispatchAction("E Y", nil)
	app.Update(runCmd(t, cmd))
	if !app.ytdlpDlg.IsVisible() {
		t.Fatal("premise lost: E Y did not open the overlay")
	}
	if v := stripANSI(app.ytdlpDlg.View()); !strings.Contains(v, "Esc/Q: Close") {
		t.Errorf("the footer does not name both close keys:\n%s", v)
	}

	app.handleKey(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if app.ytdlpDlg.IsVisible() {
		t.Error("Q did not close the overlay although the footer says it does")
	}
}
