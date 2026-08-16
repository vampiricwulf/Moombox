package worker

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// newDBProgressTracker builds a tracker wired to a real temp database with a
// job row, so the activity write path (setActivity / refresh loop) can be
// exercised end to end.
func newDBProgressTracker(t *testing.T) (*ProgressTracker, *database.Database) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.AddJob(&database.Job{ID: "act-test", Status: database.StatusDownloading}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	return NewProgressTracker(db, "act-test", nopProgressLogger{}), db
}

func jobProgress(t *testing.T, db *database.Database) (progress, speed, eta string) {
	t.Helper()
	job, err := db.GetJob("act-test")
	if err != nil || job == nil {
		t.Fatalf("GetJob: %v (job=%v)", err, job)
	}
	return job.Progress, job.Speed, job.ETA
}

// TestSpeedUsesArrivalCounter pins the speed readout's data source: once any
// fetch has been reported, speed derives from the ARRIVAL counter
// (fetchedTotal), not the ordered-flush counter (bytesTotal) — the flush
// goes quiet for whole clumps during wide catch-up, which used to render a
// saturated connection as 0 B/s between clumps.
func TestSpeedUsesArrivalCounter(t *testing.T) {
	pt, db := newDBProgressTracker(t)
	defer pt.Close()

	pt.mu.Lock()
	pt.lastUpdate = time.Now().Add(-100 * time.Millisecond) // pass the 16ms throttle
	pt.lastBytesTime = time.Now().Add(-1 * time.Second)     // ~1s measurement window
	pt.fetchedTotal = 5 << 20                               // ~5 MB arrived off the network
	pt.bytesTotal = 0                                       // nothing flushed yet — old source read 0 B/s
	pt.videoReported = true
	pt.mu.Unlock()

	pt.maybeUpdate()

	_, speed, _ := jobProgress(t, db)
	if !strings.Contains(speed, "MB/s") {
		t.Errorf("speed = %q, want a ~5 MB/s reading sourced from fetched bytes (flush counter is 0)", speed)
	}
}

// TestSetWaitActivityWritesAndTickerRefreshes pins the two behaviors the
// refresh loop exists for: an orchestrator-level wait reaches the progress
// line immediately, and its elapsed counter keeps advancing WITHOUT any
// further SetWaitActivity/emit calls — the exact window (blocking waits,
// 5-minute verify sleeps) where the line used to freeze.
func TestSetWaitActivityWritesAndTickerRefreshes(t *testing.T) {
	pt, db := newDBProgressTracker(t)
	defer pt.Close()

	pt.SetWaitActivity(engine.ActivityVerifyingEnd)

	first, speed, eta := jobProgress(t, db)
	if !strings.HasPrefix(first, "Verifying stream ended...") {
		t.Fatalf("progress after SetWaitActivity = %q, want verifying message", first)
	}
	if speed != "" || eta != "" {
		t.Errorf("speed/eta = %q/%q, want blanked", speed, eta)
	}

	// No further calls — the refresh loop alone must advance the elapsed.
	deadline := time.Now().Add(4 * time.Second)
	for {
		cur, _, _ := jobProgress(t, db)
		if strings.HasPrefix(cur, "Verifying stream ended...") && cur != first {
			return // elapsed advanced
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress never refreshed past %q without new emissions", first)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestChatTickKeepsActivityMessage: while a wait is showing, a chat tick's
// maybeUpdate must render the wait too — not stamp the frozen counter and
// stale speed over it (the two writers used to alternate once a second).
func TestChatTickKeepsActivityMessage(t *testing.T) {
	pt, db := newDBProgressTracker(t)
	defer pt.Close()

	pt.SetWaitActivity(engine.ActivityVerifyingEnd)
	time.Sleep(20 * time.Millisecond) // clear maybeUpdate's 16ms gate
	pt.SetChatCount(7)

	progress, speed, _ := jobProgress(t, db)
	if !strings.HasPrefix(progress, "Verifying stream ended...") {
		t.Errorf("progress after chat tick = %q, want the activity message", progress)
	}
	if speed != "" {
		t.Errorf("speed after chat tick = %q, want blanked", speed)
	}
}

// TestSegmentClearsWaitActivity: the next delivered segment ends an
// orchestrator wait with no explicit teardown — the design's auto-revert
// contract, extended to the orch slot.
func TestSegmentClearsWaitActivity(t *testing.T) {
	pt, _ := newDBProgressTracker(t)
	defer pt.Close()

	pt.SetWaitActivity(engine.ActivityReconnecting)

	dl := engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: "x"})
	pt.AttachVideoDownloader(dl)
	dl.OnProgress(engine.DownloadProgress{Seq: 42, Bytes: 1})

	pt.mu.Lock()
	orch := pt.orchActivity
	pt.mu.Unlock()
	if orch != engine.ActivityNone {
		t.Errorf("orchActivity after a segment = %v, want ActivityNone", orch)
	}
}

// TestFinalizeStopsActivityWrites: after Finalize (Close shares the same
// teardown), neither SetWaitActivity nor the refresh loop may write — a
// straggler would overwrite the finalize-phase progress (Muxing status, mux
// percent lines).
func TestFinalizeStopsActivityWrites(t *testing.T) {
	pt, db := newDBProgressTracker(t)

	pt.Finalize()
	pt.SetWaitActivity(engine.ActivityVerifyingEnd)

	// Give a (wrongly-started) refresh loop time to tick once.
	time.Sleep(1200 * time.Millisecond)

	progress, _, _ := jobProgress(t, db)
	if progress != "" {
		t.Errorf("progress after Finalize+SetWaitActivity = %q, want unchanged (empty)", progress)
	}
}

// TestVerifyingEndElapsedUsesLastSegment pins the unified elapsed clock:
// "Verifying stream ended..." counts from the last delivered segment (the
// number that says whether the stream is over), not from when the current
// wait began; the other activities keep their wait-start clock, and
// VerifyingEnd falls back to it when no segment was ever delivered.
func TestVerifyingEndElapsedUsesLastSegment(t *testing.T) {
	base := time.Now()
	pt := newTestProgressTracker()
	pt.lastSegmentAt = base.Add(-10 * time.Minute)
	waitStart := base.Add(-30 * time.Second)

	if got := pt.activityElapsedLocked(engine.ActivityVerifyingEnd, waitStart, base); got != 10*time.Minute {
		t.Errorf("VerifyingEnd elapsed = %v, want 10m (since last segment)", got)
	}
	if got := pt.activityElapsedLocked(engine.ActivityReconnecting, waitStart, base); got != 30*time.Second {
		t.Errorf("Reconnecting elapsed = %v, want 30s (since wait start)", got)
	}

	pt.lastSegmentAt = time.Time{} // no segment ever delivered
	if got := pt.activityElapsedLocked(engine.ActivityVerifyingEnd, waitStart, base); got != 30*time.Second {
		t.Errorf("VerifyingEnd elapsed without lastSegmentAt = %v, want 30s fallback", got)
	}
}

// TestDominantActivityIncludesOrchSlot: the orchestrator slot competes with
// the stream slots under the same longest-running-wait rule.
func TestDominantActivityIncludesOrchSlot(t *testing.T) {
	base := time.Now()
	pt := newTestProgressTracker()
	pt.lastSegmentAt = base.Add(-10 * time.Second)
	pt.videoActivity = engine.ActivityVerifyingEnd
	pt.videoActivityStart = base.Add(-5 * time.Second)
	pt.orchActivity = engine.ActivityReconnecting
	pt.orchActivityStart = base.Add(-20 * time.Second) // earlier = wins

	act, start := pt.dominantActivity(base)
	if act != engine.ActivityReconnecting || !start.Equal(base.Add(-20*time.Second)) {
		t.Errorf("got (%v, %v), want (Reconnecting, -20s)", act, start)
	}
}
