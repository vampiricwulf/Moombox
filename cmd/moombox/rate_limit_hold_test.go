package main

import (
	"path/filepath"
	"testing"
	"time"
)

// rateLimitGateSpy keeps what restoreRateLimitHold restores into it and the
// recorder it is handed: the backlog scheduler's half of the restore. Like
// worker.Scheduler's, its restore refuses a hold that has run out, and
// reports one it takes.
type rateLimitGateSpy struct {
	restored []time.Time
	record   func(until time.Time)
}

func (g *rateLimitGateSpy) RestoreRateLimitHold(until time.Time) bool {
	g.restored = append(g.restored, until)
	if !time.Now().Before(until) {
		return false
	}
	g.record(until)
	return true
}

func (g *rateLimitGateSpy) RecordRateLimitHold(record func(until time.Time)) { g.record = record }

// TestRateLimitHoldSurvivesARestart: the scheduler's YouTube rate-limit hold
// is saved in open-alerts.json beside the disk gate's, and the next process
// starts held until the same instant; the run-out it records clears it. A
// hold that ran out while no process ran restores nothing, and is cleared
// from the file so later starts do not read it. Kept in memory only, a
// restart inside the hour reopened backlog admission into the limit.
//
// Mutants: leave RateLimitHeldUntil out of snapshot — the next start sees
// no hold; install no recorder — nothing is saved; skip the clear of a hold
// that ran out — every later start reads it again.
func TestRateLimitHoldSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), openAlertsFileName)
	until := time.Now().Add(40 * time.Minute).UTC()

	first := &rateLimitGateSpy{}
	restoreRateLimitHold(restart(t, path), first)
	if len(first.restored) != 0 || first.record == nil {
		t.Fatalf("first start: restored %v, recorder set %v — want nothing restored and a recorder", first.restored, first.record != nil)
	}
	first.record(until) // a rate-limit verdict

	second := &rateLimitGateSpy{}
	restoreRateLimitHold(restart(t, path), second)
	if len(second.restored) != 1 || !second.restored[0].Equal(until) {
		t.Fatalf("restored %v after a restart inside the hold, want [%v]", second.restored, until)
	}
	second.record(time.Time{}) // the sweep that finds it run out

	third := &rateLimitGateSpy{}
	restoreRateLimitHold(restart(t, path), third)
	if len(third.restored) != 0 {
		t.Errorf("restored %v after the hold ran out, want nothing", third.restored)
	}

	// A hold that ran out while the process was down.
	third.record(time.Now().Add(-time.Minute))
	stale := &rateLimitGateSpy{}
	restoreRateLimitHold(restart(t, path), stale)
	if len(stale.restored) != 1 {
		t.Fatalf("restored %v, want the expired hold offered once", stale.restored)
	}
	if got := restart(t, path).snapshot().RateLimitHeldUntil; !got.IsZero() {
		t.Errorf("the file still holds %v after a restart found it run out, want it cleared", got)
	}
}
