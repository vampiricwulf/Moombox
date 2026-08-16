package tui

import (
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// sgrPattern matches the SGR escape sequences lipgloss emits. Assertions
// below run on stripped text: a styled chord renders as
// "\x1b[..mA\x1b[m Action", so "A Action" is not a literal substring of the
// raw output even though that is exactly what the terminal shows.
var sgrPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return sgrPattern.ReplaceAllString(s, "") }

// busyStatusBar is a worst-case bar: every optional indicator populated,
// including a long backfill channel name. Narrow-width behavior is only
// interesting when there is more content than room.
func busyStatusBar() *StatusBarModel {
	m := NewStatusBarModel()
	m.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	m.SetActivePlatforms(true, true)
	m.SetDiskStatus(120*1024*1024*1024, 45, "ok")
	m.SetBackfillStatus("UC123", "A Very Long Channel Name", "videos", 3, "scanning")
	m.SelectedCount = 12
	m.SetJobs([]*database.Job{
		{Status: database.StatusDownloading},
		{Status: database.StatusLive},
	})
	return m
}

// TestStatusBarNeverExceedsWidth pins the overflow guard. The pre-tier bar
// dropped the whole left half on overflow but left the right half
// unbounded, so a busy bar in a narrow window rendered WIDER than the
// terminal — which wraps, pushing a second line into the frame and
// corrupting every row below it. Clipping is the acceptable failure;
// wrapping is not.
func TestStatusBarNeverExceedsWidth(t *testing.T) {
	m := busyStatusBar()
	for w := 1; w <= 200; w++ {
		m.SetWidth(w)
		out := m.View()
		if got := lipgloss.Width(out); got > w {
			t.Fatalf("width %d: rendered %d columns (would wrap): %q", w, got, out)
		}
		if strings.Contains(out, "\n") {
			t.Fatalf("width %d: status bar must be a single line, got %q", w, out)
		}
	}
}

// TestStatusBarKeepsKeybindsWhenNarrow is the regression test for the
// reported bug: the old rule blanked the entire chord-hint half the moment
// both sides didn't fit, so ordinary narrow terminals showed no keybinds at
// all. Some chord must survive at every width that can seat one.
func TestStatusBarKeepsKeybindsWhenNarrow(t *testing.T) {
	m := busyStatusBar()
	for w := 12; w <= 200; w++ {
		m.SetWidth(w)
		out := m.View()
		if !strings.Contains(stripANSI(out), "?") {
			t.Errorf("width %d: no chord hint survived: %q", w, out)
		}
	}
}

// TestStatusBarShowsFullLabelsWhenWide: a wide terminal must still get the
// verbose rendering — the ladder is for scarcity, not a blanket downgrade.
func TestStatusBarShowsFullLabelsWhenWide(t *testing.T) {
	m := busyStatusBar()
	m.SetWidth(200)
	out := m.View()
	for _, want := range []string{"A Action", "Tab Focus", "` Settings", "? Help", "Disk 45%", "Active: 2"} {
		if !strings.Contains(stripANSI(out), want) {
			t.Errorf("wide bar missing %q: %q", want, out)
		}
	}
}

// TestStatusBarTiersNarrowMonotonically: each rung of both ladders must be
// no wider than the rung above it, or fitTiers' descent could step onto a
// WIDER rendering and loop or overflow.
func TestStatusBarTiersNarrowMonotonically(t *testing.T) {
	m := busyStatusBar()
	m.SetWidth(200)

	for _, tc := range []struct {
		name  string
		tiers []string
	}{
		{"controls", m.controlTiers()},
		{"metrics", m.metricTiers()},
	} {
		if len(tc.tiers) != int(tierNone)+1 {
			t.Fatalf("%s: %d tiers, want %d (one per barTier)", tc.name, len(tc.tiers), tierNone+1)
		}
		for i := 1; i < len(tc.tiers); i++ {
			prev, cur := lipgloss.Width(tc.tiers[i-1]), lipgloss.Width(tc.tiers[i])
			if cur > prev {
				t.Errorf("%s tier %d (%d cols) is wider than tier %d (%d cols)", tc.name, i, cur, i-1, prev)
			}
		}
		if got := lipgloss.Width(tc.tiers[tierNone]); got != 0 {
			t.Errorf("%s: tierNone must be empty, got %d cols", tc.name, got)
		}
	}
}

// TestStatusBarDescentSchedule pins the agreed degradation order, which is
// a deliberate product decision rather than an emergent property of the
// fitting loop: the right half steps down ALONE to tierKeys, the left then
// follows to tierKeys, and from there the two alternate — right first —
// down to tierNone. Every step must lower exactly one side by exactly one
// rung, or the "first fit is the richest fit" property that lets fitTiers
// be a plain scan no longer holds.
func TestStatusBarDescentSchedule(t *testing.T) {
	want := []struct{ left, right barTier }{
		{tierFull, tierFull},
		{tierFull, tierCompact},
		{tierFull, tierKeys},
		{tierCompact, tierKeys},
		{tierKeys, tierKeys},
		{tierKeys, tierTight},
		{tierTight, tierTight},
		{tierTight, tierEssential},
		{tierEssential, tierEssential},
		{tierEssential, tierNone},
		{tierNone, tierNone},
	}
	if len(statusBarDescent) != len(want) {
		t.Fatalf("descent has %d steps, want %d", len(statusBarDescent), len(want))
	}
	for i, w := range want {
		if statusBarDescent[i] != w {
			t.Errorf("step %d = (%v,%v), want (%v,%v)",
				i, statusBarDescent[i].left, statusBarDescent[i].right, w.left, w.right)
		}
	}

	// Exactly one side moves down exactly one rung per step, never up.
	for i := 1; i < len(statusBarDescent); i++ {
		prev, cur := statusBarDescent[i-1], statusBarDescent[i]
		dl, dr := cur.left-prev.left, cur.right-prev.right
		if dl < 0 || dr < 0 {
			t.Errorf("step %d moves a side UP: (%v,%v) -> (%v,%v)", i, prev.left, prev.right, cur.left, cur.right)
		}
		if dl+dr != 1 {
			t.Errorf("step %d changes %d rungs, want exactly 1", i, dl+dr)
		}
	}

	// The right must reach tierKeys before the left leaves tierFull — the
	// "status verbosity is the cheapest thing to lose" rule.
	for _, step := range statusBarDescent {
		if step.left > tierFull && step.right < tierKeys {
			t.Errorf("left degraded to %v while right was still at %v (richer than tierKeys)", step.left, step.right)
		}
	}
}

// TestStatusBarAlertsOutliveCounters pins the priority rule: when space is
// scarce the informational indicators (backfill scan, selection count,
// active tally) go first and the alerts (OFFLINE, re-login) stay. An
// operator squeezed for columns needs to know something is WRONG far more
// than they need a healthy tally.
func TestStatusBarAlertsOutliveCounters(t *testing.T) {
	m := busyStatusBar()
	m.offline = true
	m.SetCookieStatus(CookieStatusRelogin, CookieStatusOK)

	// A width that cannot seat the full bar but is far from degenerate.
	m.SetWidth(46)
	out := m.View()

	if !strings.Contains(stripANSI(out), "OFF") {
		t.Errorf("offline alert dropped at width 46: %q", out)
	}
	if !strings.Contains(stripANSI(out), "YT") {
		t.Errorf("re-login alert dropped at width 46: %q", out)
	}
	if sa := stripANSI(out); strings.Contains(sa, "Backfill") || strings.Contains(sa, "BF:") {
		t.Errorf("informational backfill survived past the alerts at width 46: %q", out)
	}
}

// TestStatusBarHealthyAuthYieldsToAlerts: a green "YT"/"TW" is reassurance,
// so it is dropped at tierEssential — but a re-login prompt at the same tier
// is not.
func TestStatusBarHealthyAuthYieldsToAlerts(t *testing.T) {
	healthy := NewStatusBarModel()
	healthy.SetActivePlatforms(true, true)
	healthy.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	if got := healthy.renderCookieStatus(tierEssential); got != "" {
		t.Errorf("healthy auth at tierEssential = %q, want dropped", got)
	}

	alerting := NewStatusBarModel()
	alerting.SetActivePlatforms(true, true)
	alerting.SetCookieStatus(CookieStatusRelogin, CookieStatusOK)
	if got := alerting.renderCookieStatus(tierEssential); !strings.Contains(stripANSI(got), "YT") {
		t.Errorf("re-login at tierEssential = %q, want it to survive", got)
	}
}

// TestStatusBarChordHintDegrades: the newcomer hint takes the same ladder
// rather than vanishing at the first squeeze.
func TestStatusBarChordHintDegrades(t *testing.T) {
	m := busyStatusBar()
	m.ShowChordHint = true
	for w := 12; w <= 120; w++ {
		m.SetWidth(w)
		if out := m.View(); !strings.Contains(stripANSI(out), "?") {
			t.Errorf("width %d: chord hint fully vanished: %q", w, out)
		}
	}
}

// TestStatusBarZeroWidth: an unsized bar renders nothing rather than
// panicking on a negative pad.
func TestStatusBarZeroWidth(t *testing.T) {
	m := busyStatusBar()
	for _, w := range []int{0, -1, -80} {
		m.SetWidth(w)
		if out := m.View(); out != "" {
			t.Errorf("width %d: expected empty bar, got %q", w, out)
		}
	}
}
