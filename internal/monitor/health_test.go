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

	for i := 0; i < unhealthyThreshold-1; i++ {
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
	for i := 0; i < unhealthyThreshold; i++ {
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

func findHealth(h *healthTracker, id string) ChannelHealth {
	for _, ch := range h.snapshot() {
		if ch.ChannelID == id {
			return ch
		}
	}
	return ChannelHealth{}
}
