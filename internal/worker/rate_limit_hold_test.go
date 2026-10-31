package worker

import (
	"testing"
	"testing/synctest"
	"time"
)

// TestRateLimitHoldOutlivesARestart is the rate-limit hold's restart, on the
// bubble's clock: the hold reports when it runs out each time that changes —
// placed, pushed on, found run out — and the next process, seeded with the
// last report, holds the backlog until that same instant and no longer. A
// hold that ran out before the restart restores nothing, and one a clock set
// back left hours ahead is held for the hour a hold placed now could last.
// In memory alone, the restart reopened admission into the limit the moment
// it came up.
//
// Mutants: drop the report from holdForRateLimit — nothing is saved; from
// rateLimitGateClosed — the run-out is never saved, so the next start reads
// a hold; RestoreRateLimitHold leaves rateHeld false — the restarted
// scheduler admits; restore one that has run out — the late start holds; drop
// the hour's bound — the far one holds past it.
func TestRateLimitHoldOutlivesARestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var saved time.Time
		record := func(until time.Time) { saved = until }

		first := &Scheduler{log: discardLogger{}}
		first.RecordRateLimitHold(record)
		t0 := time.Now()
		first.holdForRateLimit(t0, rateLimitBackoff)
		if !saved.Equal(t0.Add(rateLimitBackoff)) {
			t.Fatalf("saved %v after the hold, want %v", saved, t0.Add(rateLimitBackoff))
		}
		first.holdForRateLimit(t0.Add(10*time.Minute), rateLimitBackoff)
		if want := t0.Add(10*time.Minute + rateLimitBackoff); !saved.Equal(want) {
			t.Fatalf("saved %v once the hold was pushed on, want %v", saved, want)
		}

		// The process stops 20 minutes in, and the next one starts from what
		// was saved.
		time.Sleep(20 * time.Minute)
		s, db, log := testSchedulerSetup(t, 2)
		queueBacklog(t, db, "UC_restore", "restore_a", "restore_b")
		s.RecordRateLimitHold(record)
		if !s.RestoreRateLimitHold(saved) {
			t.Fatal("the restart did not restore the standing hold")
		}
		s.sweep()
		if n := log.enqueueCount(); n != 0 {
			t.Fatalf("admitted %d after a restart inside the hold, want 0", n)
		}
		time.Sleep(50*time.Minute - time.Second)
		s.sweep()
		if n := log.enqueueCount(); n != 0 {
			t.Fatalf("admitted %d a second before the hold ran out, want 0", n)
		}
		time.Sleep(time.Second)
		s.sweep()
		if n := log.enqueueCount(); n != 2 {
			t.Errorf("admitted %d once the hold ran out, want 2", n)
		}
		if !saved.IsZero() {
			t.Errorf("saved %v once the hold ran out, want none", saved)
		}

		// A hold that ran out while no process ran restores nothing.
		late := &Scheduler{log: discardLogger{}}
		if late.RestoreRateLimitHold(t0.Add(10*time.Minute+rateLimitBackoff)) || late.rateLimitGateClosed(time.Now()) {
			t.Error("a hold that had run out was restored")
		}
		// And one hours ahead, a clock set back, holds for the hour only.
		far := &Scheduler{log: discardLogger{}}
		now := time.Now()
		if !far.RestoreRateLimitHold(now.Add(5 * time.Hour)) {
			t.Fatal("the far hold was not restored")
		}
		if !far.rateLimitGateClosed(now.Add(rateLimitBackoff-time.Second)) || far.rateLimitGateClosed(now.Add(rateLimitBackoff)) {
			t.Error("a hold restored five hours out does not run out at the hour")
		}
	})
}
