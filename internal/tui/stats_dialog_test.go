package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/stats"
)

func sampleSnapshot() stats.Snapshot {
	return stats.Snapshot{
		Disk:              stats.Disk{Free: 250 << 30, Total: 1000 << 30, UsedPct: 75, WarnLevel: "warn"},
		TotalSize:         512 << 30,
		JobCount:          1234,
		SizeByPlatform:    map[string]int64{"youtube": 400 << 30, "twitch": 112 << 30},
		SizeByStatus:      map[string]int64{"finished": 500 << 30, "error": 12 << 30, "cancelled": 0},
		TotalFinished:     1200,
		TotalDuration:     3*3600 + 25*60 + 7,
		TotalChatMessages: 98765,
		ActiveDownloads:   2,
		ActiveMuxing:      1,
		CountByPlatform:   map[string]int{"youtube": 900, "twitch": 334},
		Uptime:            49*time.Hour + 30*time.Minute,
	}
}

// TestStatsDialogRendersEveryWebCard: the overlay shows the same thirteen
// figures the Web Stats tab shows, plus uptime, in the Web's units.
func TestStatsDialogRendersEveryWebCard(t *testing.T) {
	m := NewStatsDialogModel()
	m.SetSize(100, 40)
	m.Open()
	if !strings.Contains(m.View(), "Loading") {
		t.Fatalf("loading state not rendered:\n%s", m.View())
	}
	m.SetSnapshot(sampleSnapshot())
	v := stripANSI(m.View())
	for _, want := range []string{
		"Storage", "Activity",
		"750.0 GB", "1000.0 GB", "75.0%", // disk used / total / pct
		"Total Recorded", "512.0 GB",
		"Total Jobs", "1,234",
		"YouTube Storage", "400.0 GB", "Twitch Storage", "112.0 GB",
		"Finished", "500.0 GB", "Error", "12.0 GB",
		"Streams Archived", "1,200",
		"Total Recording Time", "3h 25m 7s",
		"Chat Messages", "98,765",
		"Active Downloads", "2", "Muxing", "1",
		"YouTube Jobs", "900", "Twitch Jobs", "334",
		"Uptime", "2d 1h 30m",
		"█", "░", // the disk bar
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	for line := range strings.SplitSeq(v, "\n") {
		if w := runewidth.StringWidth(line); w > 100 {
			t.Errorf("line wider than the dialog (%d): %q", w, line)
		}
	}
}

// TestStatsDialogWarnColoursAndErrors: warn/critical change the bar's colour
// (not its text); an error replaces the body; keys map to actions.
func TestStatsDialogWarnColoursAndErrors(t *testing.T) {
	m := NewStatsDialogModel()
	m.SetSize(80, 30)
	m.Open()
	s := sampleSnapshot()
	s.Disk.WarnLevel = "ok"
	m.SetSnapshot(s)
	okView := m.View()
	s.Disk.WarnLevel = "critical"
	m.SetSnapshot(s)
	if m.View() == okView {
		t.Error("critical and ok renders are identical — the bar colour must follow warnLevel")
	}
	m.SetError("stats unavailable: db closed")
	if !strings.Contains(m.View(), "db closed") {
		t.Errorf("error not rendered:\n%s", m.View())
	}
	if got := m.HandleKey("r"); got != "refresh" {
		t.Errorf("r → %q, want refresh", got)
	}
	if got := m.HandleKey("esc"); got != "close" || m.IsVisible() {
		t.Errorf("esc → %q visible=%v, want close/false", got, m.IsVisible())
	}
}

func TestStatsFormatting(t *testing.T) {
	for in, want := range map[int64]string{0: "0s", 59: "59s", 60: "1m 0s", 3661: "1h 1m 1s", 90061: "25h 1m 1s"} {
		if got := formatHMS(in); got != want {
			t.Errorf("formatHMS(%d) = %q, want %q", in, got, want)
		}
	}
	if got := formatUptime(49*time.Hour + 30*time.Minute); got != "2d 1h 30m" {
		t.Errorf("formatUptime = %q", got)
	}
	if got := formatUptime(5 * time.Minute); got != "5m" {
		t.Errorf("formatUptime(5m) = %q", got)
	}
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", in, got, want)
		}
	}
}
