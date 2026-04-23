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
