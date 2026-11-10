package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/stats"
)

func TestBareStatsFrames(t *testing.T) {
	wantFrame(t, "40x10", bareStats(40, 10).View(), 40, 10, []string{
		"Stats 1/10         R refresh · Esc close",
		"Disk ███████████░░░░ 72.2%  500.0GB free",
		"1.3TB used of 1.8TB",
		"Active 2             Muxing 1",
		"Recorded 1.2TB       Jobs 1,234",
		"YouTube 900.0GB      Twitch 300.0GB",
		"Finished 1.1TB       Error 2.0GB",
		"Cancelled 1.0GB      Archived 1,100",
		"Rec time 3412h 5m 7s Chat 12,345,678",
		"YT jobs 900          TW jobs 334",
	})
	m := bareStats(30, 6)
	wantFrame(t, "30x6", m.View(), 30, 6, []string{
		"Stats 1/17       R refresh·Esc",
		"Disk 72.2%  500.0GB free",
		"1.3TB used of 1.8TB",
		"Active 2",
		"Muxing 1",
		"Recorded 1.2TB",
	})
	// The whole list, in the order a small screen wants it: the disk, what
	// is running, then the totals, uptime last.
	m.bareVP.SetHeight(100)
	got := plainRows(m.bareVP.View())[:17]
	want := []string{
		"Disk 72.2%  500.0GB free", "1.3TB used of 1.8TB", "Active 2", "Muxing 1", "Recorded 1.2TB", "Jobs 1,234",
		"YouTube 900.0GB", "Twitch 300.0GB", "Finished 1.1TB", "Error 2.0GB", "Cancelled 1.0GB", "Archived 1,100",
		"Rec time 3412h 5m 7s", "Chat 12,345,678", "YT jobs 900", "TW jobs 334", "Uptime 26h 3m 0s",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the list at 30 columns:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// No disk reading: the normal form's notice in place of the disk rows.
	m = NewStatsDialogModel()
	m.SetSize(30, 6)
	m.Open()
	m.SetSnapshot(stats.Snapshot{})
	if rows := plainRows(m.View()); rows[1] != "No disk reading available" || rows[2] != "Active 0" {
		t.Errorf("no disk reading:\n%s", strings.Join(rows, "\n"))
	}
	// Loading before the first snapshot.
	m = NewStatsDialogModel()
	m.SetSize(30, 6)
	m.Open()
	if rows := plainRows(m.View()); !strings.HasSuffix(rows[1], "Loading...") || rows[0] != "Stats            R refresh·Esc" {
		t.Errorf("loading:\n%s", strings.Join(rows, "\n"))
	}
}

// A row never outgrows the terminal, whatever the figures: the widest disk
// reading keeps its percentage and free space, and a pair that does not fit
// side by side takes two rows.
func TestBareStatsRowsFitAnyFigures(t *testing.T) {
	big := bareStatsSnapshot()
	big.Disk = stats.Disk{Total: 1 << 50, Free: 1099404095488, UsedPct: 100, WarnLevel: "critical"}
	big.TotalDuration = 12345*3600 + 59*60 + 59
	big.TotalChatMessages = 9_876_543_210
	compactSizes(func(w, h int) {
		m := NewStatsDialogModel()
		m.SetSize(w, h)
		m.Open()
		m.SetSnapshot(big)
		rows := m.bareRows(w)
		for _, row := range rows {
			if ansi.StringWidth(row) > w {
				t.Errorf("%dx%d: a row is %d cells: %q", w, h, ansi.StringWidth(row), ansi.Strip(row))
			}
		}
		all := ansi.Strip(strings.Join(rows, "\n"))
		for _, want := range []string{"100.0%", "1023.9GB free", "Rec time 12345h 59m 59s", "Chat 9,876,543,210", "Uptime "} {
			if !strings.Contains(all, want) {
				t.Errorf("%dx%d: %q is missing or cut:\n%s", w, h, want, all)
			}
		}
	})
}

// An error is shown whole: wrapped, never cut, and scrollable where it is
// longer than the screen.
func TestBareStatsErrorIsShownWhole(t *testing.T) {
	m := bareStats(30, 6)
	m.SetError(longError)
	var seen []string
	for range 40 {
		seen = append(seen, plainRows(m.View())[1:]...)
		m.bareScroll(5)
	}
	words := map[string]bool{}
	for _, f := range strings.Fields(strings.Join(seen, " ")) {
		words[f] = true
	}
	for _, f := range strings.Fields("the database is locked by another process and the retry budget is spent") {
		if !words[f] {
			t.Errorf("the word %q of the error never came on screen", f)
		}
	}
	if !strings.Contains(strings.Join(seen, ""), "moombox.db") {
		t.Error("the path in the error never came on screen whole")
	}
}

// E T at the compact floor, through the App: the arrows and the wheel
// scroll the list, R refreshes and Esc closes. In the normal layout the
// arrows and the wheel still do nothing.
func TestBareStatsThroughTheApp(t *testing.T) {
	r := newCompactRig(t, 30, 6)
	a := r.app
	fetched := 0
	a.OnGetStats = func() (stats.Snapshot, error) { fetched++; return bareStatsSnapshot(), nil }
	r.press("e")
	_, cmd := a.handleKey(keyMsg("t"))
	if !a.statsDlg.IsVisible() || a.screenHeld() {
		t.Fatalf("E T did not open the statistics bare at 30x6: feedback %q", a.feedback.msg)
	}
	applyStatsCmd(a, cmd)
	wantExact(t, "statistics at 30x6", a.View().Content, 30, 6)
	if got := plainFrame(a); !strings.HasPrefix(got, "Stats 1/17") {
		t.Fatalf("the statistics did not load:\n%s", got)
	}
	r.press("down")
	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelDown})
	if got := a.statsDlg.bareVP.YOffset(); got != 1+overlayWheelRows {
		t.Errorf("Down and a wheel notch moved the list to row %d, want %d", got, 1+overlayWheelRows)
	}
	if !strings.HasPrefix(plainFrame(a), "Stats 5/17") {
		t.Errorf("the header does not follow the scroll:\n%s", plainFrame(a))
	}
	_, cmd = a.handleKey(keyMsg("r"))
	applyStatsCmd(a, cmd)
	if fetched != 2 {
		t.Errorf("R fetched %d times in all, want 2", fetched)
	}
	if got := a.statsDlg.bareVP.YOffset(); got != 1+overlayWheelRows {
		t.Errorf("a refresh moved the reader to row %d", got)
	}

	// Grown to the normal layout: nothing scrolls.
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	before := a.View().Content
	r.press("down")
	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelDown})
	if a.View().Content != before {
		t.Error("the arrows or the wheel changed the normal form")
	}
	r.press("esc")
	if a.hasActiveOverlay() {
		t.Error("Esc did not close the statistics")
	}
}

// A refresh after a failed one clears the error from the bare list at once:
// the earlier snapshot is shown while the fetch runs, as in the normal form.
func TestBareStatsRefreshClearsTheError(t *testing.T) {
	m := bareStats(40, 10)
	before := ansi.Strip(m.View())
	m.SetError("stats: the database is locked")
	if got := ansi.Strip(m.View()); !strings.Contains(got, "database is locked") {
		t.Fatalf("premise lost: the error is not shown:\n%s", got)
	}
	m.Open() // R
	got := ansi.Strip(m.View())
	wantExact(t, "a refresh after an error", m.View(), 40, 10)
	if strings.Contains(got, "database is locked") {
		t.Errorf("the cleared error is still on screen:\n%s", got)
	}
	if rows, was := plainRows(m.View()), strings.Split(before, "\n"); len(rows) < 3 || strings.TrimRight(was[2], " ") != rows[2] {
		t.Errorf("the earlier snapshot is not shown while the fetch runs:\n%s\nwas:\n%s", got, before)
	}
}
