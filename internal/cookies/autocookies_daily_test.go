package cookies

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDailyVisitDue is the due rule's table (owner ruling K3): a visit is due
// when no pass has renewed the cookies for 24 h plus the cycle's jitter, the
// loop has been up for the start hold, and no retry wait is running.
func TestDailyVisitDue(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	longUp := now.Add(-48 * time.Hour)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }

	cases := []struct {
		name string
		last *time.Time
		v    dailyVisit
		want bool
	}{
		{"renewed 23 h 59 min ago", ago(24*time.Hour - time.Minute), dailyVisit{started: longUp}, false},
		{"renewed exactly 24 h ago", ago(24 * time.Hour), dailyVisit{started: longUp}, true},
		{"overdue after a restart, inside the start hold", ago(72 * time.Hour),
			dailyVisit{started: now.Add(-4 * time.Minute)}, false},
		{"overdue after a restart, start hold just over", ago(72 * time.Hour),
			dailyVisit{started: now.Add(-5 * time.Minute)}, true},
		{"never renewed", nil, dailyVisit{started: longUp}, true},
		{"never renewed, inside the start hold", nil, dailyVisit{started: now}, false},
		{"last renewal stamped in the future", ago(-3 * time.Hour), dailyVisit{started: longUp}, true},
		{"24 h 5 min ago with 10 min of jitter", ago(24*time.Hour + 5*time.Minute),
			dailyVisit{started: longUp, jitter: 10 * time.Minute}, false},
		{"24 h 10 min ago with 10 min of jitter", ago(24*time.Hour + 10*time.Minute),
			dailyVisit{started: longUp, jitter: 10 * time.Minute}, true},
		{"overdue, retry wait still running", ago(30 * time.Hour),
			dailyVisit{started: longUp, retryAt: now.Add(time.Minute)}, false},
		{"overdue, retry wait over", ago(30 * time.Hour),
			dailyVisit{started: longUp, retryAt: now}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dailyVisitDue(now, tc.last, tc.v); got != tc.want {
				t.Errorf("dailyVisitDue = %v, want %v", got, tc.want)
			}
		})
	}
}

// dailyStepHarness drives dailyVisitStep with a clock the test moves and a
// pass the test scripts. No browser, no network, no timer.
type dailyStepHarness struct {
	s      *AutoCookieService
	log    *argRecordingLogger
	now    time.Time
	passes int
	// renew is what the scripted pass does: true stamps lastRefresh the way a
	// renewing pass that verified does.
	renew bool
}

func newDailyStepHarness(t *testing.T) *dailyStepHarness {
	t.Helper()
	log := &argRecordingLogger{}
	h := &dailyStepHarness{
		s:   NewAutoCookieService("", "", nil, log),
		log: log,
		now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	}
	// Up for a day already, so the start hold is out of the way.
	h.s.visit = dailyVisit{started: h.now.Add(-24 * time.Hour)}
	return h
}

func (h *dailyStepHarness) setLast(t time.Time) {
	h.s.mu.Lock()
	h.s.lastRefresh = &t
	h.s.mu.Unlock()
}

// step advances the clock by d and runs one check.
func (h *dailyStepHarness) step(d time.Duration) {
	h.now = h.now.Add(d)
	h.s.dailyVisitStep(context.Background(), func() time.Time { return h.now }, func(context.Context) bool {
		h.passes++
		if h.renew {
			h.setLast(h.now)
		}
		return true
	})
}

// TestDailyVisitRunsOncePerDayWhenPassesRenew: a visit runs when the last
// renewal is a day old, and a visit that renewed is not followed by another
// until a further day has passed.
func TestDailyVisitRunsOncePerDayWhenPassesRenew(t *testing.T) {
	h := newDailyStepHarness(t)
	h.renew = true
	h.setLast(h.now.Add(-23 * time.Hour))

	for range 59 {
		h.step(time.Minute)
	}
	if h.passes != 0 {
		t.Fatalf("%d passes ran before the last renewal was 24 h old, want 0", h.passes)
	}
	h.step(time.Minute)
	if h.passes != 1 {
		t.Fatalf("passes = %d at 24 h, want 1", h.passes)
	}
	// The next cycle's jitter is at most dailyVisitJitterMax.
	for range 24*60 - 1 {
		h.step(time.Minute)
	}
	if h.passes != 1 {
		t.Fatalf("passes = %d within a day of a visit that renewed, want 1", h.passes)
	}
	for range int(dailyVisitJitterMax/time.Minute) + 1 {
		h.step(time.Minute)
	}
	if h.passes != 2 {
		t.Errorf("passes = %d a day and the full jitter after the last visit, want 2", h.passes)
	}
}

// TestDailyVisitRetriesAPassThatDidNotRenew: a due visit that leaves
// lastRefresh where it was is tried again after 30 min, then 1 h, 2 h and so
// on, never more than 24 h apart, and never once a minute.
func TestDailyVisitRetriesAPassThatDidNotRenew(t *testing.T) {
	h := newDailyStepHarness(t)
	h.setLast(h.now.Add(-25 * time.Hour))

	h.step(time.Minute)
	if h.passes != 1 {
		t.Fatalf("passes = %d on the first due check, want 1", h.passes)
	}
	want := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour,
		8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	for i, gap := range want {
		before := h.passes
		h.step(gap - time.Minute)
		if h.passes != before {
			t.Fatalf("retry %d ran %v after the last attempt, a minute before its wait of %v was over",
				i+1, gap-time.Minute, gap)
		}
		h.step(time.Minute)
		if h.passes != before+1 {
			t.Fatalf("retry %d did not run %v after the last attempt", i+1, gap)
		}
	}

	// Something else renews the cookies (a tick, a manual refresh): the retry
	// wait ends, and the next overdue day starts again at 30 min.
	h.setLast(h.now)
	h.step(time.Minute)
	h.step(24*time.Hour + dailyVisitJitterMax)
	before := h.passes
	h.step(29 * time.Minute)
	if h.passes != before {
		t.Fatal("a retry ran 29 min after a failed visit — the back-off did not start again at 30 min")
	}
	h.step(time.Minute)
	if h.passes != before+1 {
		t.Error("no retry 30 min after a failed visit — the old back-off outlived a renewal")
	}
}

// TestDailyVisitWaitsWhileOffline: offline, a due visit launches nothing and
// counts nothing, so it runs on the first check after the connection is back
// with no back-off to wait out. A nil IsOnline means online.
func TestDailyVisitWaitsWhileOffline(t *testing.T) {
	h := newDailyStepHarness(t)
	h.setLast(h.now.Add(-25 * time.Hour))
	online := false
	h.s.IsOnline = func() bool { return online }

	for range 90 {
		h.step(time.Minute)
	}
	if h.passes != 0 {
		t.Fatalf("%d passes ran while offline, want 0", h.passes)
	}
	if h.s.visit.backoff != 0 || !h.s.visit.retryAt.IsZero() {
		t.Errorf("an offline wait was counted as a failed visit: backoff=%v retryAt=%v",
			h.s.visit.backoff, h.s.visit.retryAt)
	}
	if got := h.s.lastRefreshTime(); !got.Equal(h.now.Add(-25*time.Hour - 90*time.Minute)) {
		t.Errorf("lastRefresh moved to %v while offline", got)
	}
	if n := strings.Count(h.log.all(), "daily cookie visit is due and waiting — offline"); n != 1 {
		t.Errorf("the offline wait was logged %d times over 90 checks, want once per offline spell", n)
	}

	online = true
	h.step(time.Minute)
	if h.passes != 1 {
		t.Errorf("passes = %d on the first check back online, want 1", h.passes)
	}
}

// TestDailyVisitNotDueCostsNothing: a minute in which no visit is due runs no
// pass, does not ask IsOnline and logs nothing.
func TestDailyVisitNotDueCostsNothing(t *testing.T) {
	h := newDailyStepHarness(t)
	h.setLast(h.now)
	asked := 0
	h.s.IsOnline = func() bool { asked++; return true }
	for range 120 {
		h.step(time.Minute)
	}
	if h.passes != 0 || asked != 0 || h.log.all() != "" {
		t.Errorf("not due: passes=%d IsOnline calls=%d log=%q, want none of each", h.passes, asked, h.log.all())
	}
}

// TestDailyVisitPassIgnoresTheIdleSkipAndKeepsTheImportGuard drives the real
// pass (periodicTick) on a browserless host with a profile to import from.
//
//   - With no active job the 6-hourly tick skips, as it always has.
//   - The daily visit runs anyway: that is the whole of ruling K3. Mutant:
//     ask shouldSkipPeriodicRefresh for passDailyVisit too — no verification
//     runs and the second assertion fails.
//   - Once cookies.txt exists, a browser-free daily visit stands down on the
//     import guard exactly as a tick does: nothing to lose is still the rule.
//
// It is also the leak test for the visit's log lines: no cookie value from
// the profile may appear in anything logged.
func TestDailyVisitPassIgnoresTheIdleSkipAndKeepsTheImportGuard(t *testing.T) {
	rows := youtubeAuthRows()
	profileDir := writeWALCookieProfile(t, rows)
	log := &argRecordingLogger{}
	s := NewAutoCookieService(profileDir, filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
	t.Cleanup(func() { joinBackground(t, s) })
	s.detectBrowser = func() *DetectedBrowser { return nil }
	var verified atomic.Int64
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified.Add(1); return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	s.HasActiveJobs = func() bool { return false }

	if ran := s.periodicTick(t.Context(), 6*time.Hour, passTick); ran || verified.Load() != 0 {
		t.Fatalf("the 6-hourly tick ran with no active job (ran=%v, verifications=%d) — its rule must not change",
			ran, verified.Load())
	}
	if ran := s.periodicTick(t.Context(), 6*time.Hour, passDailyVisit); !ran || verified.Load() == 0 {
		t.Fatalf("the daily visit did not run with no active job (ran=%v, verifications=%d)", ran, verified.Load())
	}
	if s.lastRefreshTime() == nil {
		t.Fatal("a daily visit that imported and verified did not stamp lastRefresh, so the schedule " +
			"would treat it as a visit that renewed nothing")
	}

	before := verified.Load()
	if ran := s.periodicTick(t.Context(), 6*time.Hour, passDailyVisit); ran || verified.Load() != before {
		t.Errorf("a browser-free daily visit ran over an existing cookies.txt (ran=%v)", ran)
	}
	if !strings.Contains(log.all(), guardSkipped) {
		t.Error("the second daily visit did not stand down on the import guard")
	}

	logged := log.all()
	for _, r := range rows {
		if strings.Contains(logged, r.value) {
			t.Errorf("the value of cookie %s was logged", r.name)
		}
	}
}

// TestPeriodicLoopAsksForTheDailyVisit: StartPeriodicRefresh's loop runs the
// daily check on its own ticker, and not before the start hold is over.
//
// The observable is the "no browser profile directory yet" line. With a 1 h
// interval the tick never fires in this test, so only a daily visit that was
// due (nothing has ever renewed) can reach the guard that logs it.
//
// Mutants: drop the `daily.C` case from the loop — the first half fails;
// ignore the start hold in dailyVisitDue — the second half fails.
func TestPeriodicLoopAsksForTheDailyVisit(t *testing.T) {
	const noProfile = "periodic auto-cookie refresh skipped — no browser profile directory yet"
	start := func(t *testing.T, hold time.Duration) *recordingCookieLogger {
		t.Helper()
		oldEvery, oldHold := dailyVisitCheckEvery, dailyVisitStartHold
		t.Cleanup(func() { dailyVisitCheckEvery, dailyVisitStartHold = oldEvery, oldHold })
		dailyVisitCheckEvery, dailyVisitStartHold = 5*time.Millisecond, hold
		log := &recordingCookieLogger{}
		s := NewAutoCookieService(filepath.Join(t.TempDir(), "no-profile-yet"),
			filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
		startJoinedPeriodicRefresh(t, s, time.Hour)
		return log
	}

	t.Run("due once the start hold is over", func(t *testing.T) {
		log := start(t, 0)
		if !waitFor(func() bool { return log.count(noProfile) > 0 }, 10*time.Second) {
			t.Fatal("the loop never asked for a daily visit that was due")
		}
	})
	t.Run("held just after start", func(t *testing.T) {
		log := start(t, time.Hour)
		time.Sleep(60 * time.Millisecond)
		if n := log.count(noProfile); n != 0 {
			t.Errorf("%d daily visits were asked for inside the start hold, want 0", n)
		}
	})
}
