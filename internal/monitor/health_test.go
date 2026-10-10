package monitor

import (
	"errors"
	"testing"
)

// TestHealthTrackerStreakAndReset pins the failure-streak accounting:
// consecutive errors accumulate, a success resets, and the snapshot
// reflects the live state.
func TestHealthTrackerStreakAndReset(t *testing.T) {
	h := newHealthTracker()
	boom := errors.New("boom")

	h.recordError("ch1", boom)
	h.recordError("ch1", boom)
	if got := findHealth(h, "ch1").ConsecutiveErrors; got != 2 {
		t.Fatalf("consecutive: want 2, got %d", got)
	}
	if findHealth(h, "ch1").LastError != "boom" {
		t.Fatal("lastError not recorded")
	}

	h.recordSuccess("ch1")
	s := findHealth(h, "ch1")
	if s.ConsecutiveErrors != 0 || s.LastError != "" {
		t.Fatalf("success must reset streak+error, got %+v", s)
	}
}

// TestHealthTrackerFiresOnceAtThreshold verifies the unhealthy callback
// fires exactly once when the streak crosses the threshold, not on every
// subsequent failure, and re-arms after a recovery.
func TestHealthTrackerFiresOnceAtThreshold(t *testing.T) {
	h := newHealthTracker()
	var fires int
	h.onUnhealthy = func(id string, consecutive int, lastErr string) {
		fires++
		if consecutive != unhealthyThreshold {
			t.Errorf("callback consecutive: want %d, got %d", unhealthyThreshold, consecutive)
		}
	}
	boom := errors.New("boom")

	for range unhealthyThreshold - 1 {
		h.recordError("ch1", boom)
	}
	if fires != 0 {
		t.Fatalf("must not fire below threshold, fired %d", fires)
	}
	h.recordError("ch1", boom) // crosses
	if fires != 1 {
		t.Fatalf("must fire once at threshold, fired %d", fires)
	}
	h.recordError("ch1", boom) // still failing
	h.recordError("ch1", boom)
	if fires != 1 {
		t.Fatalf("must not re-fire within the same streak, fired %d", fires)
	}

	// Recovery then a fresh streak re-arms the callback.
	h.recordSuccess("ch1")
	for range unhealthyThreshold {
		h.recordError("ch1", boom)
	}
	if fires != 2 {
		t.Fatalf("must re-fire after recovery + new streak, fired %d", fires)
	}
}

// TestHealthTrackerPrune drops channels no longer in the active set.
func TestHealthTrackerPrune(t *testing.T) {
	h := newHealthTracker()
	h.recordError("keep", errors.New("x"))
	h.recordError("drop", errors.New("y"))

	h.prune(map[string]struct{}{"keep": {}})

	if len(h.snapshot()) != 1 {
		t.Fatalf("prune should leave 1 entry, got %d", len(h.snapshot()))
	}
	if findHealth(h, "keep").ChannelID != "keep" {
		t.Fatal("wrong channel survived prune")
	}
}

// TestRecordSuccessFiresOnHealthyOnlyAfterAStreakWasNotified is audit A3's
// tracker half: recordSuccess cleared `notified` with no callback, so the
// unhealthy alert had no pair.
//
// Mutants this kill:
//   - firing on every success: a healthy channel would publish a recovery on
//     every poll, forever.
//   - firing without clearing `notified`: the next success fires a second
//     recovery for the same streak.
func TestRecordSuccessFiresOnHealthyOnlyAfterAStreakWasNotified(t *testing.T) {
	h := newHealthTracker()
	var healthy []string
	h.onHealthy = func(id string) { healthy = append(healthy, id) }

	// A success with no streak behind it says nothing.
	h.recordSuccess("ch1")
	if len(healthy) != 0 {
		t.Fatalf("onHealthy fired %d times with no prior streak, want 0", len(healthy))
	}

	for i := 0; i < unhealthyThreshold; i++ {
		h.recordError("ch1", errors.New("boom"))
	}
	h.recordSuccess("ch1")
	if len(healthy) != 1 || healthy[0] != "ch1" {
		t.Fatalf("onHealthy = %v, want exactly one call for ch1", healthy)
	}

	h.recordSuccess("ch1")
	if len(healthy) != 1 {
		t.Fatalf("onHealthy fired %d times, want 1 — the streak was already closed", len(healthy))
	}
}

// TestRecordSuccessBelowTheThresholdIsSilent: a short failure run never
// alerted, so it has nothing to close.
func TestRecordSuccessBelowTheThresholdIsSilent(t *testing.T) {
	h := newHealthTracker()
	fired := 0
	h.onHealthy = func(string) { fired++ }

	for i := 0; i < unhealthyThreshold-1; i++ {
		h.recordError("ch1", errors.New("blip"))
	}
	h.recordSuccess("ch1")
	if fired != 0 {
		t.Errorf("onHealthy fired %d times for a streak that never alerted, want 0", fired)
	}
}

func findHealth(h *healthTracker, id string) ChannelHealth {
	for _, ch := range h.snapshot() {
		if ch.ChannelID == id {
			return ch
		}
	}
	return ChannelHealth{}
}

// TestEveryMonitorRestoresIntoItsOwnTracker: each monitor's RestoreUnhealthy
// seeds the tracker its checks record into, so the healthy callback it was
// given closes the restored outage on the first success.
//
// Mutant: an empty RestoreUnhealthy body on any one monitor — its close never
// fires.
func TestEveryMonitorRestoresIntoItsOwnTracker(t *testing.T) {
	log := silentLogger{}
	feed := NewFeedMonitor(nil, nil, log)
	decapi := NewDecapiMonitor(nil, nil, log)
	tw := NewTwitchMonitor(nil, nil, nil, log)
	for name, m := range map[string]struct {
		restore    func([]string)
		setHealthy func(func(string))
		health     *healthTracker
	}{
		"feed":   {feed.RestoreUnhealthy, feed.SetOnChannelHealthy, feed.health},
		"decapi": {decapi.RestoreUnhealthy, decapi.SetOnChannelHealthy, decapi.health},
		"twitch": {tw.RestoreUnhealthy, tw.SetOnChannelHealthy, tw.health},
	} {
		var closed []string
		m.setHealthy(func(id string) { closed = append(closed, id) })
		m.restore([]string{"ch"})
		m.health.recordSuccess("ch")
		if len(closed) != 1 || closed[0] != "ch" {
			t.Errorf("%s: closes = %v, want the restored channel's", name, closed)
		}
	}
}

// TestRestoredUnhealthyChannelClosesOnItsFirstSuccess: a channel whose alert a
// previous process raised and never closed behaves as if the streak had
// survived the restart. Its first success fires onHealthy once; a failure
// before that joins the streak silently, however long it runs; a channel not
// restored is untouched; nothing restored shows in the snapshot before it is
// checked; and prune drops a restored channel no longer configured.
//
// Mutants: drop takeRestored from recordSuccess (the close never fires); drop
// it from recordError (the failing restored channel alerts a second time at
// the threshold); never delete the restored entry (the second success closes
// again); drop the restored half of prune (the de-configured channel closes).
func TestRestoredUnhealthyChannelClosesOnItsFirstSuccess(t *testing.T) {
	h := newHealthTracker()
	var healthy, unhealthy []string
	h.onHealthy = func(id string) { healthy = append(healthy, id) }
	h.onUnhealthy = func(id string, _ int, _ string) { unhealthy = append(unhealthy, id) }
	h.restoreUnhealthy([]string{"up", "down", "gone"})

	if len(h.snapshot()) != 0 {
		t.Errorf("snapshot = %v before any check, want nothing — a restored channel has not been checked", h.snapshot())
	}

	h.recordSuccess("up")
	h.recordSuccess("up")
	h.recordSuccess("never-alerted")
	if len(healthy) != 1 || healthy[0] != "up" {
		t.Fatalf("onHealthy = %v, want one close for the restored channel", healthy)
	}

	for i := 0; i < 2*unhealthyThreshold; i++ {
		h.recordError("down", errors.New("still down"))
	}
	if len(unhealthy) != 0 {
		t.Errorf("onUnhealthy = %v — the restored outage was already announced", unhealthy)
	}
	h.recordSuccess("down")
	if len(healthy) != 2 || healthy[1] != "down" {
		t.Fatalf("onHealthy = %v, want the restored outage closed on its first success", healthy)
	}

	h.prune(map[string]struct{}{"up": {}, "down": {}})
	h.recordSuccess("gone")
	if len(healthy) != 2 {
		t.Errorf("onHealthy = %v — a channel pruned from the config still closed", healthy)
	}
}
