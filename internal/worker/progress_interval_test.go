package worker

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// fakeClock is a hand-advanced clock for the burst below. ProgressTracker
// reads the wall clock only through pt.now, and only under pt.mu, so a test
// that installs this must hold pt.mu to swap it and to advance it.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// The synthetic burst: 160 events one fake millisecond apart. Chosen so both
// intervals under test divide it exactly — a 16 ms gate admits 10 of them and
// an 8 ms gate admits 20 — which makes the expected counts arithmetic rather
// than a tolerance band.
const (
	burstStep  = time.Millisecond
	burstSteps = 160
)

// reportsInOneBurst drives burstSteps chat-count updates through a tracker
// built with the given interval, advancing a fake clock by burstStep before
// each one, and returns how many DB writes came out the other end.
//
// The count is taken at the database's OnJobUpdate fan-out rather than by
// watching the job row, because maybeUpdate writes the same columns every
// time and a row cannot say how often it was written. UpdateJobFields
// notifies inline on the calling goroutine (internal/database/database.go
// releases db.mu at :406 and calls notifyJobUpdate at :427), so the counter
// needs no synchronisation and the total is exact rather than
// eventually-consistent.
//
// No activity is ever set, so ensureActivityTickerLocked never fires and this
// tracker owns no goroutine: the fake clock has exactly one reader.
func reportsInOneBurst(t *testing.T, interval time.Duration) int {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "burst.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.AddJob(&database.Job{ID: "burst", Status: database.StatusDownloading}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	reports := 0
	t.Cleanup(db.OnJobUpdate(func(*database.Job) { reports++ }))

	pt := NewProgressTracker(db, "burst", nopProgressLogger{}, interval)
	t.Cleanup(pt.Close)

	clk := &fakeClock{t: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	pt.mu.Lock()
	pt.now = clk.now
	// The constructor stamped these from the real clock; re-stamp them onto
	// the fake one so the first gate comparison starts from t0.
	pt.lastUpdate, pt.lastPersist, pt.startTime = clk.t, clk.t, clk.t
	pt.mu.Unlock()

	for i := 1; i <= burstSteps; i++ {
		pt.mu.Lock()
		clk.advance(burstStep)
		pt.mu.Unlock()
		pt.SetChatCount(i)
	}
	return reports
}

// TestAnEightMillisecondTrackerReportsTwiceAsOften is the behavioural claim
// behind downloader.progress_interval_ms: the value actually governs the
// report rate, and halving it doubles the reports over the same arrival
// pattern. The exact counts are pinned as well as the ratio, because a gate
// that ignored its interval entirely and admitted EVERY event would satisfy
// "at least twice as many" while being the opposite of throttled.
//
// MUTANT: leaving maybeUpdate's gate on the progressUpdateInterval constant —
// the fast tracker reports 10 times, not 20. MUTANT: dropping the gate
// altogether — both report 160. MUTANT: storing updateInterval but stamping
// pt.lastUpdate before the comparison instead of after — both report 0 (the
// comparison then sees a zero gap on every event).
func TestAnEightMillisecondTrackerReportsTwiceAsOften(t *testing.T) {
	slow := reportsInOneBurst(t, 16*time.Millisecond)
	fast := reportsInOneBurst(t, 8*time.Millisecond)

	if slow != 10 {
		t.Errorf("16 ms tracker reported %d times across %v of burst, want 10", slow, burstSteps*burstStep)
	}
	if fast != 20 {
		t.Errorf("8 ms tracker reported %d times across %v of burst, want 20", fast, burstSteps*burstStep)
	}
	if fast < 2*slow {
		t.Errorf("halving the interval did not at least double the reports: 16 ms gave %d, 8 ms gave %d", slow, fast)
	}
}

// TestTrackerDefaultMatchesTheConfigDefault pins the two places 16 ms is
// written down against each other. config.Defaults() is what an operator who
// never touches the key gets; progressUpdateInterval is what a tracker built
// with a non-positive interval falls back to (a JobContext literal in a test,
// or the early-init window before a config store is wired). If those two ever
// disagree, the fallback quietly becomes a second, undocumented default.
//
// MUTANT: changing either 16 without the other — the first assertion fails.
// MUTANT: writing the fallback as `if updateInterval == 0` — the negative
// case below keeps a negative interval, which makes maybeUpdate's gate always
// true and the tracker ungated.
func TestTrackerDefaultMatchesTheConfigDefault(t *testing.T) {
	want := time.Duration(config.Defaults().Downloader.ProgressIntervalMS) * time.Millisecond
	if progressUpdateInterval != want {
		t.Errorf("progressUpdateInterval = %v, but config.Defaults() says %v — the fallback and the documented default must agree",
			progressUpdateInterval, want)
	}
	for _, in := range []time.Duration{0, -5 * time.Millisecond} {
		pt := NewProgressTracker(nil, "fallback", nopProgressLogger{}, in)
		if pt.updateInterval != progressUpdateInterval {
			t.Errorf("NewProgressTracker(..., %v).updateInterval = %v, want the %v fallback",
				in, pt.updateInterval, progressUpdateInterval)
		}
	}
}

// TestTheNextJobsTrackerTakesANewProgressInterval is the hot-reload claim in
// the key's doc comment: no restart. buildJobContext snapshots the config
// under readConfig at job start, so a value saved while Moombox runs reaches
// the next job's tracker — and a tracker already running keeps what it
// started with, which is why the assertion is on a tracker built AFTER the
// save rather than on one built before it.
//
// The store is built with an empty save path: Store.Update then validates
// (and rolls back on failure) without writing a file, which is the half this
// test cares about.
//
// MUTANT: reading the value from w.cfg directly instead of through
// readConfig — testWorkerSetup builds a bare MoomboxConfig, so the FIRST
// assertion fails: 0s where the 16 ms default was expected. MUTANT:
// converting with time.Duration(ms) instead of time.Duration(ms) *
// time.Millisecond — the first assertion fails with 16ns (t.Fatalf stops
// there).
func TestTheNextJobsTrackerTakesANewProgressInterval(t *testing.T) {
	w, db := testWorkerSetup(t)

	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = t.TempDir()
	store := config.NewStore(cfg, "") // no save path: validate in memory only
	w.SetConfigStore(store)

	job := &database.Job{ID: "pi-job", VideoID: "pi", Status: database.StatusDownloading}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	if got := w.buildJobContext(job).Config.ProgressInterval; got != 16*time.Millisecond {
		t.Fatalf("ProgressInterval before the save = %v, want the 16 ms default", got)
	}

	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Downloader.ProgressIntervalMS = 8
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}

	jobCtx := w.buildJobContext(job)
	if jobCtx.Config.ProgressInterval != 8*time.Millisecond {
		t.Fatalf("ProgressInterval after the save = %v, want 8ms", jobCtx.Config.ProgressInterval)
	}

	pt := NewProgressTracker(db, job.ID, nopProgressLogger{}, jobCtx.Config.ProgressInterval)
	t.Cleanup(pt.Close)
	if pt.updateInterval != 8*time.Millisecond {
		t.Errorf("the tracker built after the save gates at %v, want 8ms", pt.updateInterval)
	}
}

// TestCalculateETAReadsTheTrackerClock pins the seam completion: calculateETA
// must read elapsed time through pt.now, the same field every other gate in
// the file reads, not time.Now directly — otherwise a test that drives pt.now
// off a fake clock (as reportsInOneBurst does above) gets a real-clock ETA
// with no way to control it.
//
// MUTANT: reverting calculateETA to time.Since(pt.startTime) — elapsed comes
// out near zero (the fake clock never advanced the real one), so the early
// "too early for meaningful estimate" guard fires and the result is "" where
// this test wants "15m 0s".
func TestCalculateETAReadsTheTrackerClock(t *testing.T) {
	pt := NewProgressTracker(nil, "eta", nopProgressLogger{}, 0)
	t.Cleanup(pt.Close)

	clk := &fakeClock{t: pt.startTime.Add(100 * time.Second)}
	pt.mu.Lock()
	pt.now = clk.now
	pt.videoReported, pt.videoSeq, pt.videoTotal = true, 100, 1000
	pt.mu.Unlock()

	if got := pt.calculateETA(); got != "15m 0s" {
		t.Errorf("calculateETA() = %q, want %q", got, "15m 0s")
	}
}
