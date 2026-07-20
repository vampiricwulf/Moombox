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

	// The latch was never set (ShouldTriggerOffline not called yet), so the
	// success reports no clear edge even though it drops the set below threshold.
	if cleared := pt.ReportSuccessAndCleared("engine/fetch"); cleared {
		t.Fatal("reported a latch-clear edge, but the latch was never set")
	}

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
	// but monitor/feed's failures must NOT be erased. The latch was set above
	// (ShouldTriggerOffline), so this success reports the triggered→cleared edge.
	if !pt.ReportSuccessAndCleared("engine/fetch") {
		t.Error("expected the success to report clearing the latch (survivors dropped below minTags)")
	}
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

// TestPassiveTracker_LatchClearsViaPruneOnMinTagsDrop locks in the second
// offline-recovery fix: pruneOld must clear the latch when the surviving
// failures drop below minTags, not only below minFails. Pre-fix, pruneOld
// checked minFails alone, so a set that aged down to a single tag (still
// >= minFails) kept IsTriggered()=true even though ShouldTriggerOffline would
// no longer trigger.
func TestPassiveTracker_LatchClearsViaPruneOnMinTagsDrop(t *testing.T) {
	pt := &PassiveTracker{window: 120 * time.Millisecond, minFails: 3, minTags: 2}

	// Tag "a" fails first; later, tag "b" fails enough to keep len >= minFails
	// after "a" ages out — but leaving only a single distinct tag.
	pt.ReportFailure("a")
	time.Sleep(80 * time.Millisecond)
	pt.ReportFailure("b")
	pt.ReportFailure("b")
	pt.ReportFailure("b")
	if !pt.ShouldTriggerOffline() {
		t.Fatal("expected trigger: 4 failures across 2 tags within window")
	}

	// Let "a" age past the 120ms window while the "b" burst stays inside it.
	time.Sleep(80 * time.Millisecond)

	// A pruning read now drops "a", leaving 3 "b" failures: still >= minFails
	// but only one tag (< minTags), so the latch must clear.
	if pt.IsTriggeredPruned() {
		t.Error("latch should clear once survivors drop below minTags, even with len >= minFails")
	}
	if pt.IsTriggered() {
		t.Error("latch should remain cleared after pruning")
	}

	// Sanity: the "b" failures survived — we did not wipe everything.
	pt.mu.Lock()
	remaining := len(pt.failures)
	pt.mu.Unlock()
	if remaining != 3 {
		t.Errorf("expected 3 surviving 'b' failures, got %d", remaining)
	}
}
