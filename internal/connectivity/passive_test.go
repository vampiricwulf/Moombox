package connectivity

import (
	"testing"
	"time"
)

func TestPassiveTracker_NoTriggerOnSingleCaller(t *testing.T) {
	pt := NewPassiveTracker()
	for range 10 {
		pt.ReportFailure("engine/fetch")
	}
	if pt.ShouldTriggerOffline() {
		t.Fatal("should not trigger with only 1 caller tag")
	}
}

func TestPassiveTracker_TriggersOnMultipleCallers(t *testing.T) {
	pt := NewPassiveTracker()
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("monitor/feed")
	pt.ReportFailure("monitor/feed")

	if !pt.ShouldTriggerOffline() {
		t.Fatal("should trigger: 5 failures from 2 callers")
	}
}

func TestPassiveTracker_SuccessClearsTrigger(t *testing.T) {
	pt := NewPassiveTracker()
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("monitor/feed")
	pt.ReportFailure("monitor/feed")

	pt.ReportSuccess("engine/fetch")

	if pt.ShouldTriggerOffline() {
		t.Fatal("should not trigger after success")
	}
	if pt.IsTriggered() {
		t.Fatal("triggered flag should clear on success")
	}
}

// TestPassiveTracker_SuccessPerTag verifies that ReportSuccess only drops
// failures for the given tag rather than wiping everything. A flaky subsystem
// producing alternating success/failure should not mask an outage in another
// subsystem. Regression for audit reports/small-packages.md question #3.
func TestPassiveTracker_SuccessPerTag(t *testing.T) {
	pt := NewPassiveTracker()
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("engine/fetch")
	pt.ReportFailure("monitor/feed")
	pt.ReportFailure("monitor/feed")
	if !pt.ShouldTriggerOffline() {
		t.Fatal("should trigger: 5 failures across 2 tags")
	}

	// engine/fetch recovers but monitor/feed is still failing — the trigger
	// should clear because the remaining failures come from only one tag,
	// but monitor/feed's failures must NOT be erased.
	pt.ReportSuccess("engine/fetch")
	if pt.IsTriggered() {
		t.Error("triggered flag should clear after per-tag success drops us below minTags")
	}
	pt.mu.Lock()
	remaining := len(pt.failures)
	pt.mu.Unlock()
	if remaining != 2 {
		t.Errorf("expected 2 monitor/feed failures to remain, got %d", remaining)
	}

	// Two more monitor/feed failures alone still shouldn't trigger — one tag.
	pt.ReportFailure("monitor/feed")
	pt.ReportFailure("monitor/feed")
	pt.ReportFailure("monitor/feed")
	if pt.ShouldTriggerOffline() {
		t.Error("should not trigger with failures from only one tag")
	}
}

func TestPassiveTracker_FailureSliceCapped(t *testing.T) {
	pt := NewPassiveTracker()
	// Push far more than the cap. Without bounding, the slice would grow
	// unbounded until window expiry.
	for range maxFailureEntries * 3 {
		pt.ReportFailure("engine/fetch")
	}
	pt.mu.Lock()
	got := len(pt.failures)
	pt.mu.Unlock()
	if got > maxFailureEntries {
		t.Fatalf("failure slice exceeded cap: got %d, want <= %d", got, maxFailureEntries)
	}
}

func TestPassiveTracker_WindowExpiry(t *testing.T) {
	pt := &PassiveTracker{
		window:   100 * time.Millisecond,
		minFails: 5,
		minTags:  2,
	}
	pt.ReportFailure("a")
	pt.ReportFailure("a")
	pt.ReportFailure("a")
	pt.ReportFailure("b")
	pt.ReportFailure("b")

	time.Sleep(150 * time.Millisecond)

	if pt.ShouldTriggerOffline() {
		t.Fatal("should not trigger after window expiry")
	}
}

// TestPassiveTracker_LatchClearsOnIdlePrune locks in the F19 fix:
// after a brief failure burst trips the latch, an idle Moombox (no
// further failures, no ReportSuccess) must see IsTriggered()=false
// once the failure window has aged out. Pre-fix, pruneOld trimmed
// the slice but never cleared `triggered`, so IsTriggered() reported
// true forever until the next HTTP success fired ReportSuccess.
func TestPassiveTracker_LatchClearsOnIdlePrune(t *testing.T) {
	pt := &PassiveTracker{
		window:   100 * time.Millisecond,
		minFails: 3,
		minTags:  2,
	}
	pt.ReportFailure("a")
	pt.ReportFailure("a")
	pt.ReportFailure("b")

	if !pt.ShouldTriggerOffline() {
		t.Fatal("expected initial failure burst to trigger offline")
	}
	if !pt.IsTriggered() {
		t.Fatal("expected latch to be set after ShouldTriggerOffline returned true")
	}

	// Sleep past the failure window — failures age out, no new
	// failures arrive, no ReportSuccess fires.
	time.Sleep(150 * time.Millisecond)

	// IsTriggered alone doesn't prune (it just reads the flag).
	// ShouldTriggerOffline does prune AND now also clears the latch
	// when failures fall below threshold.
	if pt.ShouldTriggerOffline() {
		t.Error("expected no trigger after window expiry (failures all aged out)")
	}
	if pt.IsTriggered() {
		t.Error("expected latch cleared by pruneOld once failures fell below threshold; got still-triggered")
	}
}
