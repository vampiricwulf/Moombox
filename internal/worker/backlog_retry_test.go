package worker

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// fakeConn is a Connectivity whose state the test flips; a flip calls every
// registered callback the way the monitor's transition does.
type fakeConn struct {
	online atomic.Bool
	mu     sync.Mutex
	cbs    []func(online bool)
}

func (c *fakeConn) IsOnline() bool { return c.online.Load() }

func (c *fakeConn) OnStateChange(fn func(online bool)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cbs = append(c.cbs, fn)
	return func() {}
}

func (c *fakeConn) set(online bool) {
	c.online.Store(online)
	c.mu.Lock()
	cbs := append([]func(bool){}, c.cbs...)
	c.mu.Unlock()
	for _, fn := range cbs {
		fn(online)
	}
}

// TestSchedulerHoldsAdmissionWhileOffline: an admission during an outage is a
// backlog VOD sent to fail its first fetch, so a sweep admits nothing while
// the connectivity monitor reports offline.
//
// Mutant: drop the offline gate from sweep — both jobs are admitted offline.
func TestSchedulerHoldsAdmissionWhileOffline(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 2)
	conn := &fakeConn{}
	s.conn = conn
	ch := "UC_outage"
	for _, id := range []string{"out_a", "out_b"} {
		addSchedJob(t, db, &ch, id, database.StatusQueued, 1)
		addFeedItemRow(t, db, ch, id, "2026-07-10T00:00:00Z")
	}

	s.sweep()
	if n := log.enqueueCount(); n != 0 {
		t.Fatalf("admitted %d backlog jobs while offline, want 0", n)
	}
	conn.online.Store(true)
	s.sweep()
	if n := log.enqueueCount(); n != 2 {
		t.Errorf("admitted %d once online, want 2", n)
	}
}

// TestSchedulerSweepsWhenConnectivityReturns: the backlog an outage held must
// not idle until the heartbeat once the network is back.
//
// Mutant: drop Run's OnStateChange subscription — nothing is admitted until
// the 60 s heartbeat.
func TestSchedulerSweepsWhenConnectivityReturns(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 1)
	conn := &fakeConn{}
	s.conn = conn
	ch := "UC_back"
	addSchedJob(t, db, &ch, "back_a", database.StatusQueued, 1)
	addFeedItemRow(t, db, ch, "back_a", "2026-07-10T00:00:00Z")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitForCond(t, 2*time.Second, "Run's subscription", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return len(conn.cbs) == 1
	})
	time.Sleep(50 * time.Millisecond) // the startup sweep, offline
	if n := log.enqueueCount(); n != 0 {
		t.Fatalf("admitted %d while offline", n)
	}
	conn.set(true)
	waitForCond(t, 2*time.Second, "an admission once connectivity returned", func() bool {
		return log.enqueueCount() == 1
	})
}

// TestSchedulerSkipsHeldBacklogJob: a job a transient failure sent back to
// Queued waits out its backoff without costing the rest of its channel the
// turn, and is admitted once the hold runs out.
//
// Mutants: drop the held() skip — the held job is admitted at once; drop the
// heldCount() widening of NextQueuedJobs — with one slot the query returns
// only the held job and nothing is admitted.
func TestSchedulerSkipsHeldBacklogJob(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 1)
	ch := "UC_hold"
	addSchedJob(t, db, &ch, "hold_new", database.StatusQueued, 1)
	addFeedItemRow(t, db, ch, "hold_new", "2026-07-12T00:00:00Z")
	addSchedJob(t, db, &ch, "hold_old", database.StatusQueued, 1)
	addFeedItemRow(t, db, ch, "hold_old", "2026-07-11T00:00:00Z")

	s.holdUntil("hold_new", time.Now().Add(time.Hour))
	s.sweep()
	if got := log.admitted(); len(got) != 1 || got[0] != "hold_old" {
		t.Fatalf("admitted %v, want the next job past the held one", got)
	}

	db.UpdateJobFields("hold_old", map[string]any{"status": database.StatusFinished})
	s.holdUntil("hold_new", time.Now().Add(-time.Second))
	s.sweep()
	if got := log.admitted(); len(got) != 2 || got[1] != "hold_new" {
		t.Errorf("admitted %v, want the held job once its hold ran out", got)
	}
}

// backlogRetryJob adds an admitted backlog VOD with its feed_items partner.
func backlogRetryJob(t *testing.T, db *database.Database, id string, priority int, partner bool) {
	t.Helper()
	ch := "UC_retry"
	addSchedJob(t, db, &ch, id, database.StatusUpcoming, priority)
	if partner {
		addFeedItemRow(t, db, ch, id, "2026-07-10T00:00:00Z")
	}
}

// dialRefused is what an outage does to GetVideoInfo.
var dialRefused = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}

// TestBacklogTransientFailureRequeuesThenErrors is V5's worker half through
// processJob: a backlog VOD whose pre-download fetch fails transiently goes
// back to Queued, held for a backoff, and only once its retries are spent
// does it end in Error, saying so.
//
// Mutants: drop the requeue from processJob's stream-processing failure —
// the first failure is an Error; drop the retry limit — the job never reaches
// Error; drop the holdUntil — the requeued job is admitted again at once.
func TestBacklogTransientFailureRequeuesThenErrors(t *testing.T) {
	w, db := testWorkerSetup(t)
	backlogRetryJob(t, db, "retry_vod", 1, true)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		return nil, dialRefused
	}

	for attempt := 1; attempt <= backlogRetryLimit; attempt++ {
		w.processJob(context.Background(), "retry_vod")
		row, _ := db.GetJob("retry_vod")
		if row.Status != database.StatusQueued {
			t.Fatalf("attempt %d: status = %s (%q), want Queued", attempt, row.Status, row.Error)
		}
		if !w.scheduler.held("retry_vod", time.Now()) {
			t.Fatalf("attempt %d: the requeued job is not held from re-admission", attempt)
		}
		// The scheduler's re-admission, once the hold is over.
		db.UpdateJobFields("retry_vod", map[string]any{"status": database.StatusUpcoming})
	}
	w.processJob(context.Background(), "retry_vod")
	row, _ := db.GetJob("retry_vod")
	if row.Status != database.StatusError || !strings.Contains(row.Error, "gave up after") {
		t.Errorf("after the budget: status = %s (%q), want Error naming the spent retries", row.Status, row.Error)
	}
}

// TestBacklogRetryOnlyForTransientBacklogFailures: a definitive refusal, a
// job that is not backlog, and a backlog job with no feed_items partner (the
// scheduler admits Queued rows through that join, so Queued would strand it)
// all end in Error at once.
//
// Mutants: drop the classifyProbeErr check — the 404 is requeued; drop the
// CookieResumeStatus check — the broadcast is requeued and the partnerless
// job is stranded in Queued.
func TestBacklogRetryOnlyForTransientBacklogFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		priority int
		partner  bool
		err      error
	}{
		{"a deleted video", 1, true, errors.New("full fetch failed: web API error: HTTP 404")},
		{"not backlog", 0, true, dialRefused},
		{"no feed_items partner", 1, false, dialRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			backlogRetryJob(t, db, "once", tc.priority, tc.partner)
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				return nil, tc.err
			}
			w.processJob(context.Background(), "once")
			if row, _ := db.GetJob("once"); row.Status != database.StatusError {
				t.Errorf("status = %s, want Error", row.Status)
			}
		})
	}
}

// TestBacklogStaleReextractFailureRequeues: the re-extraction a stale VOD
// makes once its slots are held (V3) fails the same way a first fetch can,
// and a backlog job takes the same retry.
//
// Mutant: drop the requeue after refreshStaleVodInfo — the job ends in Error.
func TestBacklogStaleReextractFailureRequeues(t *testing.T) {
	w, db := testWorkerSetup(t)
	backlogRetryJob(t, db, "stale_vod", 1, true)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		return &StreamProcessResult{ShouldDownload: true, IsVod: true, VideoInfo: vodInfoAt("http://127.0.0.1:1", 1, 1)}, nil
	}
	w.refreshVodInfoFn = func(context.Context, *database.Job) (*youtube.VideoInfo, error) {
		return nil, dialRefused
	}
	w.processJob(context.Background(), "stale_vod")
	if row, _ := db.GetJob("stale_vod"); row.Status != database.StatusQueued {
		t.Errorf("status = %s (%q), want Queued for a retry", row.Status, row.Error)
	}
	if n := w.queue.ActiveCount(); n != 0 {
		t.Errorf("download slots still held after the requeue: %d", n)
	}
}
