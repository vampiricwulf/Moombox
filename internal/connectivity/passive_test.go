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
