package cookies

// autocookies_daily.go — the daily visit: a refresh pass that runs when no
// pass has renewed the cookies for 24 h, whether or not a job is active.
//
// Owner ruling KA-3 (2026-11-13). The 6-hourly tick skips while nothing is
// recording (shouldSkipPeriodicRefresh), so an install that sat idle went days
// without the managed browser visiting the sites, and the session cookies were
// never renewed in that time. The tick and its rule are unchanged. This is a
// second, slower schedule on the same goroutine, and it runs the same pass
// under the same guards (periodicTick).

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	// dailyVisitInterval is how long the cookies may go without a renewing
	// pass before one runs with no active job.
	dailyVisitInterval = 24 * time.Hour
	// dailyVisitRetryBase is the wait after a due visit that did not renew.
	// It doubles on each further one, up to dailyVisitRetryCap.
	dailyVisitRetryBase = 30 * time.Minute
	dailyVisitRetryCap  = 24 * time.Hour
	// dailyVisitJitterMax bounds the random delay added to each due time, so
	// the visits do not land at a machine-regular period.
	dailyVisitJitterMax = 10 * time.Minute
)

// dailyVisitCheckEvery is how often the loop asks whether a visit is due, and
// dailyVisitStartHold is how long after the loop starts no visit runs: clear
// of the launcher's 2-minute post-update window, in which a new build that
// exits is rolled back. Both are vars only so a test of the loop does not wait
// minutes; production never assigns them.
var (
	dailyVisitCheckEvery = time.Minute
	dailyVisitStartHold  = 5 * time.Minute
)

// dailyVisit is the daily visit's schedule. All of it lives in memory: the
// one input that survives a restart is the service's lastRefresh, which the
// cookie file's sidecar persists.
type dailyVisit struct {
	// started is when the loop started.
	started time.Time
	// jitter is this cycle's random delay, drawn again after each visit that
	// renewed.
	jitter time.Duration
	// retryAt and backoff are set by a due visit that did not renew, and
	// cleared as soon as anything has renewed the cookies.
	retryAt time.Time
	backoff time.Duration
	// offlineNoted keeps the "waiting, offline" line to one per offline spell.
	offlineNoted bool
	// noSource says a visit asked for now would launch nothing: there is no
	// managed profile yet, or the pass would be a browser-free import over a
	// cookies.txt that has something to lose (periodicPassStandsDown). The
	// status then names no next visit. The loop still asks on schedule,
	// because either can change at runtime. Worked out when the loop starts,
	// whenever lastRefresh has moved, and by each pass that is asked for.
	noSource bool
	// sourceAsOf is the lastRefresh noSource was last worked out against.
	sourceAsOf *time.Time
}

// sameTime reports whether two optional times are both absent or equal.
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// dailyVisitOverdue reports whether the last renewing pass is 24 h (plus this
// cycle's jitter) old. A pass that never happened counts, and so does one
// stamped in the future: a clock that was set back must not put the next visit
// off by however far it moved.
func dailyVisitOverdue(now time.Time, last *time.Time, jitter time.Duration) bool {
	if last == nil || last.After(now) {
		return true
	}
	return !now.Before(last.Add(dailyVisitInterval + jitter))
}

// dailyVisitDue is the one rule for "a daily visit runs now": the cookies are
// overdue, the loop has been up for dailyVisitStartHold, and no retry wait is
// still running.
func dailyVisitDue(now time.Time, last *time.Time, v dailyVisit) bool {
	if !dailyVisitOverdue(now, last, v.jitter) {
		return false
	}
	if now.Before(v.started.Add(dailyVisitStartHold)) {
		return false
	}
	return !now.Before(v.retryAt)
}

// nextDailyVisitBackoff is the wait after one more due visit that did not
// renew: 30 min, doubling, capped at 24 h.
func nextDailyVisitBackoff(prev time.Duration) time.Duration {
	if prev <= 0 {
		return dailyVisitRetryBase
	}
	return min(prev*2, dailyVisitRetryCap)
}

// runDailyVisitStep runs one check under its own recover, for the reason
// runPeriodicTick does: a panic must cost one check, not the loop.
func (s *AutoCookieService) runDailyVisitStep(ctx context.Context, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic in daily cookie visit check", "panic", fmt.Sprintf("%v", r))
		}
	}()
	s.dailyVisitStep(ctx, time.Now, func(ctx context.Context) bool {
		return s.periodicTick(ctx, interval, passDailyVisit)
	})
}

// dailyVisitStep is one minute's check. The due test comes first and reads
// only memory, so a minute in which nothing is due costs no stat, no browser
// resolution and no log line.
//
// pass runs the refresh pass and reports whether one ran at all (false when a
// guard stood it down). Whether it RENEWED is read from lastRefresh, which
// only a renewing pass that verified moves, so a visit that launched a browser
// and came back with the same cookies is retried like one that failed.
func (s *AutoCookieService) dailyVisitStep(ctx context.Context, now func() time.Time, pass func(context.Context) bool) {
	defer s.publishVisit()
	v := &s.visit
	t := now()
	last := s.lastRefreshTime()
	// A refresh that renewed the cookies (an import, a setup) may have
	// changed whether a visit has anything to launch. Asked only then, so a
	// minute in which nothing moved still costs no stat.
	if !sameTime(v.sourceAsOf, last) {
		v.sourceAsOf = last
		_, v.noSource = s.periodicPassStandsDown()
	}
	if !dailyVisitOverdue(t, last, v.jitter) {
		// Something renewed the cookies: a tick, a manual refresh, a setup or
		// a recovery. Whatever retry wait was running is over.
		v.retryAt, v.backoff, v.offlineNoted = time.Time{}, 0, false
		return
	}
	if !dailyVisitDue(t, last, *v) {
		return
	}
	if s.IsOnline != nil && !s.IsOnline() {
		// Nothing counted and nothing stamped: the visit is still due the
		// minute the connection is back.
		if !v.offlineNoted {
			v.offlineNoted = true
			s.logger.Debug("daily cookie visit is due and waiting — offline")
		}
		return
	}
	v.offlineNoted = false

	ran := pass(ctx)

	t = now()
	if !dailyVisitOverdue(t, s.lastRefreshTime(), v.jitter) {
		v.retryAt, v.backoff = time.Time{}, 0
		v.jitter = rand.N(dailyVisitJitterMax)
		return
	}
	v.backoff = nextDailyVisitBackoff(v.backoff)
	v.retryAt = t.Add(v.backoff)
	if ctx.Err() != nil {
		return
	}
	if ran {
		s.logger.Info("daily cookie visit did not renew the cookies — it will be tried again",
			"retry_in", v.backoff.String())
	} else {
		// Stood down by a guard that has already said why at Debug (no
		// browser profile yet, or an import with a cookies.txt to lose).
		s.logger.Debug("daily cookie visit did not run — it will be asked again",
			"retry_in", v.backoff.String())
	}
}

// lastRefreshTime returns a copy of lastRefresh, or nil when no pass has
// renewed the cookies.
func (s *AutoCookieService) lastRefreshTime() *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRefresh == nil {
		return nil
	}
	t := *s.lastRefresh
	return &t
}

// publishVisit copies the loop's schedule to where the status reads it.
func (s *AutoCookieService) publishVisit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.visitPub = s.visit
}
