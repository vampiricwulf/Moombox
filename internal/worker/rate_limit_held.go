package worker

import "time"

// RateLimitHeld reports whether the backlog's YouTube rate-limit hold
// (holdForRateLimit, restored across a restart by RestoreRateLimitHold)
// stands at now. It only reads: the sweep that finds the hold run out is
// what clears and saves it (rateLimitGateClosed). The feed walk reads it
// (monitor.FeedMonitor.RateLimitHeld, wired by cmd/moombox) to skip the
// re-check of a row whose last probe was refused by the rate limit while the
// hold stands, rather than climb that row's inconclusive ladder (owner
// decision, wave FC). Safe from any goroutine.
func (s *Scheduler) RateLimitHeld(now time.Time) bool {
	_, held := s.RateLimitHeldUntil(now)
	return held
}

// RateLimitHeldUntil is RateLimitHeld with when the hold runs out (zero when
// none stands): the latency-mode backfill pauses on it and names the end on
// its status line (owner ruling RV-20). Safe from any goroutine.
func (s *Scheduler) RateLimitHeldUntil(now time.Time) (time.Time, bool) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rateHeld && now.Before(s.rateLimitedUntil) {
		return s.rateLimitedUntil, true
	}
	return time.Time{}, false
}
