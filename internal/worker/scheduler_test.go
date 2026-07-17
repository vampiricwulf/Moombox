package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestSchedulerWakeIsSafeNoOp pins the Task 2 stub contract: creation sites
// call Scheduler().Wake() before the admission loop exists (Task 3 builds
// it), so Wake must be non-blocking with no reader — repeat calls coalesce
// into the buffered signal instead of blocking the monitor callback.
func TestSchedulerWakeIsSafeNoOp(t *testing.T) {
	w, _ := testWorkerSetup(t)

	s := w.Scheduler()
	if s == nil {
		t.Fatal("Scheduler() = nil — backlog creation calls Scheduler().Wake()")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 3 {
			s.Wake() // nothing drains the signal yet; must never block
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wake() blocked with no scheduler loop running")
	}
}

// TestCreationByDisposition_BacklogRestsOutsideQueue drives the worker-side
// half of spec §10's creator table with jobs shaped exactly as the host's
// OnVideoFound creation writes them. The backlog VOD (Queued, priority 1) is
// never handed to the JobQueue — its creation path only wakes the scheduler —
// while broadcast/new-VOD shapes are enqueued immediately. History rows exist
// for every disposition (HasProcessed means "a job was created").
func TestCreationByDisposition_BacklogRestsOutsideQueue(t *testing.T) {
	w, db := testWorkerSetup(t)

	chID := "UC_archive"
	shapes := []struct {
		id         string
		status     database.JobStatus
		priority   int
		enqueueNow bool
	}{
		{"disp_broadcast", database.StatusUpcoming, 0, true},
		{"disp_new_vod", database.StatusUpcoming, 0, true},
		{"disp_backlog_vod", database.StatusQueued, 1, false},
	}
	for _, s := range shapes {
		if _, err := db.AddJob(&database.Job{
			ID: s.id, VideoID: s.id, URL: "u",
			Status: s.status, ChannelID: &chID, QueuePriority: s.priority,
		}); err != nil {
			t.Fatal(err)
		}
		// AddToHistory fires for EVERY disposition, exactly as the host does.
		if err := db.AddToHistory(s.id); err != nil {
			t.Fatal(err)
		}
		if s.enqueueNow {
			w.EnqueueJob(s.id)
		} else {
			w.Scheduler().Wake()
		}
	}

	// The backlog job rests: not processing, no pending entry.
	if w.queue.IsProcessing("disp_backlog_vod") {
		t.Error("backlog VOD is processing — it must wait for the scheduler")
	}
	w.queue.mu.Lock()
	_, backlogPending := w.queue.pendingSet["disp_backlog_vod"]
	_, broadcastPending := w.queue.pendingSet["disp_broadcast"]
	_, newVODPending := w.queue.pendingSet["disp_new_vod"]
	w.queue.mu.Unlock()
	if backlogPending {
		t.Error("backlog VOD entered the pending queue — backlog creation must skip EnqueueJob")
	}
	if !broadcastPending || !newVODPending {
		t.Errorf("admit-now dispositions not enqueued (broadcast=%v newVOD=%v)", broadcastPending, newVODPending)
	}

	for _, s := range shapes {
		ok, err := db.HasProcessed(s.id)
		if err != nil || !ok {
			t.Errorf("HasProcessed(%s) = %v, %v — AddToHistory fires for every disposition", s.id, ok, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Task 3 scheduler tests: the admission sweep (spec §10, tests per §17).
// ---------------------------------------------------------------------------

// schedOp is one recorded scheduler action: the durable status write
// ("update") or the JobQueue handoff ("enqueue"). A single shared ordered log
// captures both so tests can pin the load-bearing write-THEN-enqueue order.
type schedOp struct {
	kind   string // "update" | "enqueue"
	jobID  string
	status database.JobStatus
}

type schedOpLog struct {
	mu  sync.Mutex
	ops []schedOp
}

func (l *schedOpLog) add(op schedOp) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, op)
}

func (l *schedOpLog) snapshot() []schedOp {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]schedOp, len(l.ops))
	copy(out, l.ops)
	return out
}

// admitted returns jobIDs of enqueue ops, in enqueue order.
func (l *schedOpLog) admitted() []string {
	var out []string
	for _, op := range l.snapshot() {
		if op.kind == "enqueue" {
			out = append(out, op.jobID)
		}
	}
	return out
}

func (l *schedOpLog) enqueueCount() int { return len(l.admitted()) }

// stubSchedQueue satisfies the scheduler's queue dependency with no real
// JobQueue machinery — it records the handoff into the shared op log.
type stubSchedQueue struct {
	log *schedOpLog
}

func (q *stubSchedQueue) Enqueue(jobID string, status database.JobStatus) {
	q.log.add(schedOp{kind: "enqueue", jobID: jobID, status: status})
}

// testSchedulerSetup builds a Scheduler against a real temp SQLite DB with a
// stub queue, an updateJob spy that records then delegates to the real
// UpdateJobFields (the M count must observe the durable write), and a
// resolveSlots stub returning m for every channel.
func testSchedulerSetup(t *testing.T, m int) (*Scheduler, *database.Database, *schedOpLog) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "sched.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	log := &schedOpLog{}
	s := &Scheduler{
		db:    db,
		queue: &stubSchedQueue{log: log},
		updateJob: func(jobID string, fields map[string]any) {
			st, _ := fields["status"].(database.JobStatus)
			log.add(schedOp{kind: "update", jobID: jobID, status: st})
			db.UpdateJobFields(jobID, fields)
		},
		resolveSlots: func(string) int { return m },
		wake:         make(chan struct{}, 1),
		log:          discardLogger{},
	}
	return s, db, log
}

func addSchedJob(t *testing.T, db *database.Database, chID *string, videoID string, status database.JobStatus, prio int) {
	t.Helper()
	if _, err := db.AddJob(&database.Job{
		ID: videoID, VideoID: videoID, URL: "u",
		Status: status, ChannelID: chID, QueuePriority: prio,
	}); err != nil {
		t.Fatal(err)
	}
}

func addFeedItemRow(t *testing.T, db *database.Database, chID, videoID, published string) {
	t.Helper()
	if _, err := db.UpsertFeedItem(database.FeedItem{
		ChannelID: chID, VideoID: videoID, Title: videoID,
		Published: published, DatePrecision: "exact", Source: "rss",
		FirstSeen: "2026-07-16T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
}

func waitForCond(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// assertAdmissionInvariants checks that every enqueue was preceded by its
// job's durable status write (write-THEN-enqueue, the load-bearing order),
// that every durable write set StatusUpcoming, and that no job was admitted
// twice.
func assertAdmissionInvariants(t *testing.T, log *schedOpLog) {
	t.Helper()
	updated := map[string]bool{}
	enqueued := map[string]bool{}
	for i, op := range log.snapshot() {
		switch op.kind {
		case "update":
			if op.status != database.StatusUpcoming {
				t.Fatalf("op %d: admission wrote status %q, want %q", i, op.status, database.StatusUpcoming)
			}
			updated[op.jobID] = true
		case "enqueue":
			if !updated[op.jobID] {
				t.Fatalf("op %d: %s enqueued BEFORE its durable status write — M would count 0 forever", i, op.jobID)
			}
			if enqueued[op.jobID] {
				t.Fatalf("op %d: %s admitted twice", i, op.jobID)
			}
			enqueued[op.jobID] = true
		}
	}
}

// TestScheduler_Converges300 is spec §17's convergence case: 300 Queued rows
// on one channel with M=3 drain through the scheduler without the pending
// count ever exceeding M and without anything reaching the queue that was not
// admitted. First sweep admits exactly 3; completing one + Wake admits
// exactly 1 more; completing everything round by round converges all 300.
func TestScheduler_Converges300(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 3)
	chID := "UC_converge"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 300 {
		id := fmt.Sprintf("v%03d", i)
		addSchedJob(t, db, &chID, id, database.StatusQueued, 1)
		addFeedItemRow(t, db, chID, id, base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// First sweep: exactly M=3 admitted.
	s.Wake()
	waitForCond(t, 5*time.Second, "first sweep to admit 3", func() bool { return log.enqueueCount() >= 3 })
	time.Sleep(25 * time.Millisecond) // settle: catch over-admission
	if n := log.enqueueCount(); n != 3 {
		t.Fatalf("first sweep admitted %d jobs, want exactly 3", n)
	}
	assertAdmissionInvariants(t, log)
	// The durable write happened first, so the M count observes all 3.
	if n, err := db.CountBacklogInFlight(chID); err != nil || n != 3 {
		t.Fatalf("CountBacklogInFlight = %d, %v; want 3 (admission must be durable before enqueue)", n, err)
	}
	// Newest-first: the three most recently published rows admit first.
	first := log.admitted()
	want := map[string]bool{"v299": true, "v298": true, "v297": true}
	for _, id := range first {
		if !want[id] {
			t.Fatalf("first sweep admitted %v; want the 3 newest-published (v297..v299)", first)
		}
	}

	// Completing one + Wake admits exactly 1 more.
	finished := map[string]bool{}
	db.UpdateJobFields(first[0], map[string]any{"status": database.StatusFinished})
	finished[first[0]] = true
	s.Wake()
	waitForCond(t, 5*time.Second, "one freed slot to admit 1 more", func() bool { return log.enqueueCount() >= 4 })
	time.Sleep(25 * time.Millisecond)
	if n := log.enqueueCount(); n != 4 {
		t.Fatalf("after one completion, %d total admitted, want exactly 4", n)
	}
	if n, err := db.CountBacklogInFlight(chID); err != nil || n > 3 {
		t.Fatalf("in-flight = %d, %v; pending must never exceed M=3", n, err)
	}

	// Converge: finish every admitted-but-unfinished job, wake, repeat.
	for log.enqueueCount() < 300 {
		adm := log.admitted()
		for _, id := range adm {
			if !finished[id] {
				db.UpdateJobFields(id, map[string]any{"status": database.StatusFinished})
				finished[id] = true
			}
		}
		prev := len(adm)
		s.Wake()
		next := min(prev+3, 300)
		waitForCond(t, 5*time.Second, fmt.Sprintf("admissions to reach %d", next), func() bool {
			return log.enqueueCount() >= next
		})
		if n, err := db.CountBacklogInFlight(chID); err != nil || n > 3 {
			t.Fatalf("in-flight = %d, %v; pending must never exceed M=3", n, err)
		}
	}

	time.Sleep(25 * time.Millisecond)
	if n := log.enqueueCount(); n != 300 {
		t.Fatalf("converged to %d admissions, want exactly 300 (nothing beyond admitted jobs)", n)
	}
	assertAdmissionInvariants(t, log)
	if chans, err := db.QueuedChannels(); err != nil || len(chans) != 0 {
		t.Fatalf("QueuedChannels after convergence = %v, %v; want empty", chans, err)
	}
}

// TestScheduler_MCountIsAllowList pins the allow-list semantics of the M
// count: a priority-1 job resting in COOKIES? does NOT hold an archive slot
// (a channel whose cookies lapse must not silently lose its throughput), a
// priority-1 job in Muxing DOES, and a priority-0 job never counts at all.
func TestScheduler_MCountIsAllowList(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 1) // M=1
	chID := "UC_allow"

	// A priority-1 COOKIES? job and a priority-0 Downloading job: neither
	// holds the channel's single slot.
	addSchedJob(t, db, &chID, "j_cookies", database.StatusCookies, 1)
	addSchedJob(t, db, &chID, "j_live", database.StatusDownloading, 0)
	addSchedJob(t, db, &chID, "v_q1", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_q1", "2026-07-01T00:00:00Z")

	if n, err := db.CountBacklogInFlight(chID); err != nil || n != 0 {
		t.Fatalf("CountBacklogInFlight = %d, %v; want 0 — COOKIES? and priority-0 must not hold slots", n, err)
	}
	s.sweep()
	if got := log.admitted(); len(got) != 1 || got[0] != "v_q1" {
		t.Fatalf("sweep admitted %v; want [v_q1] — a COOKIES? job must not block admission", got)
	}

	// The admitted job (Upcoming, priority 1) now holds the slot; Muxing
	// holds it too. A second backlog row must wait.
	addSchedJob(t, db, &chID, "v_q2", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_q2", "2026-07-02T00:00:00Z")
	db.UpdateJobFields("v_q1", map[string]any{"status": database.StatusMuxing})
	if n, err := db.CountBacklogInFlight(chID); err != nil || n != 1 {
		t.Fatalf("CountBacklogInFlight = %d, %v; want 1 — Muxing holds a slot", n, err)
	}
	s.sweep()
	if n := log.enqueueCount(); n != 1 {
		t.Fatalf("sweep admitted %d total, want still 1 — a Muxing priority-1 job holds the slot", n)
	}

	// Cookies lapse on the in-flight job: the slot frees, v_q2 admits.
	db.UpdateJobFields("v_q1", map[string]any{"status": database.StatusCookies})
	s.sweep()
	if got := log.admitted(); len(got) != 2 || got[1] != "v_q2" {
		t.Fatalf("after COOKIES? transition sweep admitted %v; want v_q2 admitted — COOKIES? must release the slot", got)
	}
	assertAdmissionInvariants(t, log)
}

// TestScheduler_AdmissionOrderPublishedDesc: three Queued rows whose
// feed_items dates are inserted in shuffled order admit newest-first.
func TestScheduler_AdmissionOrderPublishedDesc(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 3)
	chID := "UC_order"

	// Shuffled insertion order; published dates disagree with it.
	addSchedJob(t, db, &chID, "v_old", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_old", "2026-01-01T00:00:00Z")
	addSchedJob(t, db, &chID, "v_new", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_new", "2026-03-01T00:00:00Z")
	addSchedJob(t, db, &chID, "v_mid", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_mid", "2026-02-01T00:00:00Z")

	s.sweep()

	got := log.admitted()
	wantOrder := []string{"v_new", "v_mid", "v_old"}
	if len(got) != len(wantOrder) {
		t.Fatalf("admitted %v, want %v", got, wantOrder)
	}
	for i := range wantOrder {
		if got[i] != wantOrder[i] {
			t.Fatalf("admission order %v, want %v (published DESC)", got, wantOrder)
		}
	}
	assertAdmissionInvariants(t, log)
}

// TestScheduler_IgnoresNullChannel: a Queued row with NULL channel_id
// (defensive — no production creator writes that shape) is never admitted
// and does not wedge the sweep for real channels.
func TestScheduler_IgnoresNullChannel(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 3)
	chID := "UC_real"

	addSchedJob(t, db, nil, "v_null", database.StatusQueued, 1) // NULL channel_id
	addSchedJob(t, db, &chID, "v_ok", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_ok", "2026-07-01T00:00:00Z")

	chans, err := db.QueuedChannels()
	if err != nil || len(chans) != 1 || chans[0] != chID {
		t.Fatalf("QueuedChannels = %v, %v; want [%s] — NULL channel_id rows are excluded", chans, err, chID)
	}

	s.sweep()

	if got := log.admitted(); len(got) != 1 || got[0] != "v_ok" {
		t.Fatalf("sweep admitted %v; want [v_ok] only", got)
	}
	// The NULL-channel row rests untouched in Queued.
	j, err := db.GetJob("v_null")
	if err != nil || j == nil || j.Status != database.StatusQueued {
		t.Fatalf("NULL-channel job = %+v, %v; want untouched in Queued", j, err)
	}
	assertAdmissionInvariants(t, log)
}
