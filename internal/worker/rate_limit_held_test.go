package worker

import (
	"testing"
	"time"
)

// TestRateLimitHeldReadsTheBacklogHold pins the read the feed walk asks
// (monitor.FeedMonitor.RateLimitHeld, wave FC): true while a rate-limit
// hold stands, false before one is placed and once it has run out — and the
// read clears nothing, so the sweep that finds the hold run out still saves
// and logs it (rateLimitGateClosed).
//
// Mutants: answering rateHeld alone fails the run-out row (true past the
// hour); answering the instant alone fails the restored-then-cleared row;
// clearing the hold on the read fails the sweep's run-out report.
func TestRateLimitHeldReadsTheBacklogHold(t *testing.T) {
	var saved []time.Time
	s := &Scheduler{log: discardLogger{}}
	s.RecordRateLimitHold(func(until time.Time) { saved = append(saved, until) })
	t0 := time.Now()
	if s.RateLimitHeld(t0) {
		t.Fatal("held before any hold was placed")
	}
	s.holdForRateLimit(t0, time.Hour)
	if !s.RateLimitHeld(t0.Add(59 * time.Minute)) {
		t.Error("not held inside the hour")
	}
	if s.RateLimitHeld(t0.Add(time.Hour)) {
		t.Error("held once the hour has run out")
	}
	if s.rateLimitGateClosed(t0.Add(time.Hour)) || len(saved) != 2 || !saved[1].IsZero() {
		t.Errorf("the sweep after the read saved %v, want the run-out (zero) report after the hold's", saved)
	}
	s.rateMu.Lock()
	s.rateLimitedUntil = t0.Add(2 * time.Hour) // a stale instant, the hold cleared
	s.rateMu.Unlock()
	if s.RateLimitHeld(t0.Add(time.Hour + time.Minute)) {
		t.Error("held by an instant whose hold the sweep cleared")
	}
}
