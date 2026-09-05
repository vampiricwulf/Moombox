package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/stats"
)

func rtOffered(app *App) bool {
	for _, it := range app.buildMenuItems() {
		if it.Chord == "R T" {
			return true
		}
	}
	return false
}

// TestStatsChordOpensLoadsAndRefreshes: R T exists only with the callback;
// dispatch opens the overlay loading, the fetch fills it, r re-fetches, the
// 60 s tick re-fetches while open and is ignored once closed; a fetch error
// renders in the overlay.
func TestStatsChordOpensLoadsAndRefreshes(t *testing.T) {
	app := NewApp()
	app.OnGetStats = nil
	if rtOffered(app) {
		t.Fatal("R T offered without OnGetStats")
	}
	calls := 0
	app.OnGetStats = func() (stats.Snapshot, error) {
		calls++
		s := sampleSnapshot()
		s.JobCount = 1000 + calls
		return s, nil
	}
	if !rtOffered(app) {
		t.Fatal("R T missing with OnGetStats wired")
	}
	_, cmd := app.dispatchAction("R T", nil)
	if !app.statsDlg.IsVisible() || !strings.Contains(app.statsDlg.View(), "Loading") {
		t.Fatal("R T must open the overlay loading")
	}
	app.Update(runCmd(t, cmd)) // drains the batch and feeds the message back through app.Update
	if calls != 1 || !strings.Contains(stripANSI(app.statsDlg.View()), "1,001") {
		t.Fatalf("after open: calls=%d view=%s", calls, app.statsDlg.View())
	}
	_, cmd = app.handleKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
	app.Update(runCmd(t, cmd))
	if calls != 2 || !strings.Contains(stripANSI(app.statsDlg.View()), "1,002") {
		t.Fatalf("after r: calls=%d", calls)
	}
	_, cmd = app.Update(statsRefreshTickMsg{})
	app.Update(runCmd(t, cmd))
	if calls != 3 {
		t.Fatalf("the refresh tick must re-fetch while open, calls=%d", calls)
	}
	app.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.statsDlg.IsVisible() {
		t.Fatal("esc must close")
	}
	_, cmd = app.Update(statsRefreshTickMsg{})
	if cmd != nil {
		t.Fatal("a tick after close must not produce a command")
	}
	if calls != 3 {
		t.Fatal("a tick after close must not fetch")
	}
	app.OnGetStats = func() (stats.Snapshot, error) { return stats.Snapshot{}, errors.New("db closed") }
	_, cmd = app.dispatchAction("R T", nil)
	app.Update(runCmd(t, cmd))
	if !strings.Contains(app.statsDlg.View(), "db closed") {
		t.Fatal("fetch error must render in the overlay")
	}
}
