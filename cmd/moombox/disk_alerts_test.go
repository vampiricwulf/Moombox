package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

func diskReading(level string, usedPct float64) *routes.DiskStatus {
	return &routes.DiskStatus{Free: 40 << 30, Total: 1000 << 30, UsedPct: usedPct, WarnLevel: level}
}

// TestDiskAllClearClosesAWarningThatWasSent is audit A2. Every warning was an
// open incident with no end, and the 30-minute repeat made the missing
// resolved line more visible, not less.
//
// Mutants this kill:
//   - sending the all-clear unconditionally: a healthy install announces a
//     recovery on its first reading, and again after every ok reading.
//   - not clearing lastLevel: the second warning episode never closes.
func TestDiskAllClearClosesAWarningThatWasSent(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	now := time.Now()

	// An ok reading with no warning behind it says nothing.
	d.onReading(diskReading("ok", 40), "./output", now)
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("an ok reading recorded %d calls, want 0", got)
	}

	d.onReading(diskReading("warn", 91), "./output", now)
	if got := len(rec.ByEvent("disk_warning")); got != 1 {
		t.Fatalf("recorded %d disk_warning calls, want 1", got)
	}
	rec.Reset()

	d.onReading(diskReading("ok", 40), "./output", now.Add(time.Minute))
	calls := rec.ByEvent("disk_ok")
	if len(calls) != 1 {
		t.Fatalf("recorded %d disk_ok calls, want 1", len(calls))
	}
	if calls[0].Type != notifications.TypeSuccess {
		t.Errorf("type = %v, want TypeSuccess", calls[0].Type)
	}

	rec.Reset()
	d.onReading(diskReading("ok", 40), "./output", now.Add(2*time.Minute))
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("a second ok reading recorded %d calls, want 0", got)
	}
}

// TestDiskWarningCooldownIsUnchanged guards the behaviour this task LIFTS but
// must not alter: a level change sends immediately, the same level waits 30
// minutes.
func TestDiskWarningCooldownIsUnchanged(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	now := time.Now()

	d.onReading(diskReading("warn", 91), "./output", now)
	d.onReading(diskReading("warn", 92), "./output", now.Add(5*time.Minute))
	if got := len(rec.Calls()); got != 1 {
		t.Fatalf("recorded %d calls inside the cooldown, want 1", got)
	}
	d.onReading(diskReading("critical", 97), "./output", now.Add(6*time.Minute))
	if got := len(rec.ByEvent("disk_critical")); got != 1 {
		t.Fatalf("a level change recorded %d disk_critical calls, want 1 — it must not wait out the cooldown", got)
	}
	d.onReading(diskReading("critical", 98), "./output", now.Add(40*time.Minute))
	if got := len(rec.ByEvent("disk_critical")); got != 2 {
		t.Fatalf("recorded %d disk_critical calls after the cooldown expired, want 2", got)
	}
}

// TestDiskMonitoringRecoveredClosesAReadFailure.
//
// Mutants this kill:
//   - announcing a recovery after the FIRST failed read, which never alerted
//     (the alert is on the second, ~12 minutes in).
//   - not resetting the counter: the next failure streak never alerts.
func TestDiskMonitoringRecoveredClosesAReadFailure(t *testing.T) {
	rec := notificationtest.New()
	d := newDiskAlerts(rec, &nopLogger{})
	now := time.Now()

	d.onReadFailure("./output")
	d.onReading(diskReading("ok", 40), "./output", now)
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("one failed read then a recovery recorded %d calls, want 0 — nothing had been reported", got)
	}

	d.onReadFailure("./output")
	d.onReadFailure("./output")
	if got := len(rec.Calls()); got != 1 {
		t.Fatalf("recorded %d calls for a two-read failure streak, want 1", got)
	}
	rec.Reset()

	d.onReading(diskReading("ok", 40), "./output", now.Add(time.Minute))
	calls := rec.ByEvent("disk_ok")
	if len(calls) != 1 {
		t.Fatalf("recorded %d disk_ok calls after monitoring recovered, want 1", len(calls))
	}
	if calls[0].Title != "Disk Monitoring Recovered" {
		t.Errorf("title = %q", calls[0].Title)
	}
}
