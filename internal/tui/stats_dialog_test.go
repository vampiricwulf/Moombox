package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/stats"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

func sampleSnapshot() stats.Snapshot {
	return stats.Snapshot{
		Disk:              stats.Disk{Free: 250 << 30, Total: 1000 << 30, UsedPct: 75, WarnLevel: "warn"},
		TotalSize:         512 << 30,
		JobCount:          1234,
		SizeByPlatform:    map[string]int64{"youtube": 400 << 30, "twitch": 112 << 30},
		SizeByStatus:      map[string]int64{"finished": 497 << 30, "error": 12 << 30, "cancelled": 3 << 30},
		TotalFinished:     1200,
		TotalDuration:     3*3600 + 25*60 + 7,
		TotalChatMessages: 98765,
		ActiveDownloads:   2,
		ActiveMuxing:      1,
		CountByPlatform:   map[string]int{"youtube": 900, "twitch": 334},
		Uptime:            49*time.Hour + 30*time.Minute,
	}
}

// TestStatsDialogRendersEveryWebCard: the overlay shows the same fourteen
// figures the Web Stats tab shows, plus uptime, in the Web's own units —
// utils.FormatFileSize is the Web's formatBytes ("512.0GB", no space) and
// formatHMS is its formatDurationSeconds ("49h 30m 0s").
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
		"750.0GB used of 1000.0GB", "250.0GB free (75.0% used)", // both of the Web's disk labels
		"Total Recorded", "512.0GB",
		"Total Jobs", "1,234",
		"YouTube Storage", "400.0GB", "Twitch Storage", "112.0GB",
		"Finished", "497.0GB", "Error", "12.0GB", "Cancelled", "3.0GB",
		"Streams Archived", "1,200",
		"Total Recording Time", "3h 25m 7s",
		"Chat Messages", "98,765",
		"Active Downloads", "2", "Muxing", "1",
		"YouTube Jobs", "900", "Twitch Jobs", "334",
		"Uptime", "49h 30m 0s",
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
	// Uptime is a time.Duration in seconds through the same formatter, so the
	// overlay prints what the Web's formatDurationSeconds prints.
	if got := formatHMS(int64((49*time.Hour + 30*time.Minute).Seconds())); got != "49h 30m 0s" {
		t.Errorf("uptime = %q, want %q", got, "49h 30m 0s")
	}
	// Sizes are utils.FormatFileSize, which carries the Web's TB tier — the
	// overlay is the surface that reaches it (a full archive disk).
	for in, want := range map[int64]string{0: "0B", 512: "512B", 2 << 40: "2.0TB", 1536 << 30: "1.5TB"} {
		if got := utils.FormatFileSize(in); got != want {
			t.Errorf("utils.FormatFileSize(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestStatsDialogFitsAShortTerminal: the overlay is the package's tallest, and
// Bubble Tea v2 drops overflow from the TOP — the border and title go first —
// so a 24-row terminal has to get the whole box. Under statsDialogFullHeight
// the two blank spacers are dropped (the footer keeps its own); at an ordinary
// height they are there.
func TestStatsDialogFitsAShortTerminal(t *testing.T) {
	rowOf := func(lines []string, want string) int {
		t.Helper()
		for i, l := range lines {
			if strings.Contains(l, want) {
				return i
			}
		}
		t.Fatalf("no line contains %q:\n%s", want, strings.Join(lines, "\n"))
		return -1
	}

	short := NewStatsDialogModel()
	short.SetSize(80, 24)
	short.Open()
	short.SetSnapshot(sampleSnapshot())
	sv := short.View()
	if n := strings.Count(sv, "\n") + 1; n > 24 {
		t.Errorf("the overlay must fit a 24-row terminal, rendered %d lines:\n%s", n, stripANSI(sv))
	}
	sl := strings.Split(stripANSI(sv), "\n")
	if rowOf(sl, "Statistics")+1 != rowOf(sl, "Storage") {
		t.Errorf("at 24 rows the blank line under the title must go:\n%s", stripANSI(sv))
	}
	if rowOf(sl, "Cancelled")+1 != rowOf(sl, "Activity") {
		t.Errorf("at 24 rows the blank line between the sections must go:\n%s", stripANSI(sv))
	}

	tall := NewStatsDialogModel()
	tall.SetSize(100, 40)
	tall.Open()
	tall.SetSnapshot(sampleSnapshot())
	tl := strings.Split(stripANSI(tall.View()), "\n")
	if rowOf(tl, "Statistics")+2 != rowOf(tl, "Storage") {
		t.Error("at 40 rows the title keeps its blank line")
	}
	if rowOf(tl, "Cancelled")+2 != rowOf(tl, "Activity") {
		t.Error("at 40 rows the sections keep their blank line")
	}
}

// TestStatsDialogFitsTheFloorTerminal: below the compact box's 23 rows — down
// to the TUI's 60x20 floor — the box drew 23 lines anyway, and the renderer
// dropped its top border, title and "Storage" header. There the hint joins the
// title line and the blank above it and Uptime go, so every figure the Web
// Stats tab shows still fits.
//
// Mutant: keeping the footer on its own line when tight — 20 rows overflow.
func TestStatsDialogFitsTheFloorTerminal(t *testing.T) {
	for _, size := range [][2]int{{60, 20}, {80, 21}, {80, 22}, {80, 23}} {
		m := NewStatsDialogModel()
		m.SetSize(size[0], size[1])
		m.Open()
		m.SetSnapshot(sampleSnapshot())
		v := stripANSI(m.View())
		if n := strings.Count(v, "\n") + 1; n > size[1] {
			t.Errorf("%dx%d: rendered %d lines:\n%s", size[0], size[1], n, v)
		}
		for _, want := range []string{"Statistics", "Storage", "Activity", "Twitch Jobs", "R: Refresh", "Cancelled 3.0GB"} {
			if !strings.Contains(v, want) {
				t.Errorf("%dx%d: %q is not on screen:\n%s", size[0], size[1], want, v)
			}
		}
	}
}

// Total Recorded counts cancelled jobs' files, and the by-status breakdown
// used to show only Finished and Error, so the two did not add up whenever a
// cancelled job kept its file. Cancelled is shown in every layout — on its own
// row where there is room, sharing one line with the other two in the tight
// one — and that shared line fits the 60-column floor at its widest.
//
// Mutant: dropping Cancelled from either layout, or the tight layout keeping
// three rows (the floor then overflows).
func TestStatsDialogShowsCancelledStorage(t *testing.T) {
	snap := sampleSnapshot()
	big := int64(9999) << 30 / 10 // 999.9GB
	snap.SizeByStatus = map[string]int64{"finished": big, "error": big, "cancelled": big}
	for _, size := range [][2]int{{60, 20}, {80, 24}, {100, 40}} {
		m := NewStatsDialogModel()
		m.SetSize(size[0], size[1])
		m.Open()
		m.SetSnapshot(snap)
		v := stripANSI(m.View())
		if !strings.Contains(v, "Cancelled") || strings.Count(v, "999.9GB") != 3 {
			t.Errorf("%dx%d: the three by-status sizes are not all shown:\n%s", size[0], size[1], v)
		}
		if n := strings.Count(v, "\n") + 1; n > size[1] {
			t.Errorf("%dx%d: rendered %d lines", size[0], size[1], n)
		}
		for line := range strings.SplitSeq(v, "\n") {
			if w := runewidth.StringWidth(line); w > size[0] {
				t.Errorf("%dx%d: line wider than the terminal (%d): %q", size[0], size[1], w, line)
			}
		}
	}
}
