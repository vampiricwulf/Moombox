package tui

import (
	"strings"
	"testing"
)

// TestBackfillRetryShowsWhileNoScanIsInFlight (owner ruling R-E2): a failed
// backfill scan shows a dim "BF: retry" on the status bar — the dashboard's
// "Backfill failed — will retry" badge — while that channel's last state is
// "error" and no scan is in flight. A scan in flight takes the slot; the
// channel's next scan, completion or idle clears its failure.
//
// Mutant: dropping the failed set — the first check fails. Clearing it only
// on done — the "a new scan of the channel" check fails.
func TestBackfillRetryShowsWhileNoScanIsInFlight(t *testing.T) {
	view := func(m *StatusBarModel) string { m.SetWidth(200); return stripANSI(m.View()) }
	m := NewStatusBarModel()

	m.SetBackfillStatus("UC1", "One", "videos", 2, BackfillScanning)
	m.SetBackfillStatus("UC1", "One", "", 0, BackfillError)
	if out := view(m); !strings.Contains(out, "BF: retry") {
		t.Fatalf("a failed scan with nothing in flight shows no retry: %q", out)
	}

	m.SetBackfillStatus("UC2", "Two", "streams", 1, BackfillScanning)
	if out := view(m); strings.Contains(out, "BF: retry") || !strings.Contains(out, "Backfill Two: streams p1") {
		t.Errorf("a scan in flight must own the slot: %q", out)
	}
	m.SetBackfillStatus("UC2", "Two", "", 0, BackfillDone)
	if out := view(m); !strings.Contains(out, "BF: retry") {
		t.Errorf("another channel's success cleared UC1's failure: %q", out)
	}

	m.SetBackfillStatus("UC1", "One", "videos", 1, BackfillScanning)
	m.SetBackfillStatus("UC1", "One", "", 0, BackfillIdle)
	if out := view(m); strings.Contains(out, "BF:") || strings.Contains(out, "Backfill") {
		t.Errorf("a new scan of the channel clears its failure, as the dashboard's map does: %q", out)
	}

	m.SetBackfillStatus("UC1", "One", "", 0, BackfillError)
	m.SetBackfillStatus("UC1", "One", "", 0, BackfillDone)
	if out := view(m); strings.Contains(out, "BF:") {
		t.Errorf("a success did not clear the retry: %q", out)
	}
}

// TestBackfillRetryIsTheFirstThingDropped: "BF: retry" is informational, on
// the scan indicator's tiers (full and compact) and dim; an alert outlives it.
func TestBackfillRetryIsTheFirstThingDropped(t *testing.T) {
	m := NewStatusBarModel()
	m.SetBackfillStatus("UC1", "One", "", 0, BackfillError)
	counts := m.tallyJobs()
	for _, tc := range []struct {
		tier barTier
		want bool
	}{
		{tierFull, true}, {tierCompact, true}, {tierKeys, false}, {tierTight, false}, {tierEssential, false},
	} {
		got := strings.Contains(stripANSI(m.renderMetrics(tc.tier, counts)), "BF: retry")
		if got != tc.want {
			t.Errorf("tier %v: shows retry = %v, want %v", tc.tier, got, tc.want)
		}
	}
	if raw := m.renderMetrics(tierFull, counts); !strings.Contains(raw, statusBarDimStyle.Render("BF: retry")) {
		t.Errorf("retry is not rendered dim: %q", raw)
	}

	m.offline = true
	m.SetWidth(20)
	if out := stripANSI(m.View()); strings.Contains(out, "BF:") || !strings.Contains(out, "OFF") {
		t.Errorf("at width 20 the retry must give way to the alert: %q", out)
	}
}

// TestBackfillRetryIsInTheRenderKey: the memoised frame changes when a
// failure lands or clears with nothing else moving.
func TestBackfillRetryIsInTheRenderKey(t *testing.T) {
	m := NewStatusBarModel()
	m.SetWidth(200)
	before := m.View()
	m.SetBackfillStatus("UC1", "One", "", 0, BackfillError)
	if after := m.View(); after == before {
		t.Error("a failure did not change the cached frame")
	}
}
