package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
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

// ---------------------------------------------------------------------------
// Task 4 slot-release flips (spec §10): a priority-1 backlog job that turns
// out to be a BROADCAST stops holding its channel's archive slot — at the
// Live status write, or on entering the upcoming wait (which can last days).
// ---------------------------------------------------------------------------

// TestBroadcastFlip_LiveReleasesSlot: a priority-1 job whose processor writes
// Live flips queue_priority to 0 in the SAME UpdateJobFields call, so it no
// longer counts in CountBacklogInFlight. Also pins the scan side: the flip
// guard reads Job.QueuePriority off the DB scan path, so GetJob must return
// what AddJob wrote (channel_id and queue_priority round-trip together).
func TestBroadcastFlip_LiveReleasesSlot(t *testing.T) {
	w, db := testWorkerSetup(t)
	chID := "UC_flip_live"
	addSchedJob(t, db, &chID, "flip_live", database.StatusUpcoming, 1)

	// Scan side: the processor's job comes from the DB scan path — the guard
	// is blind unless both v16 columns round-trip through the SELECT lists.
	job, err := db.GetJob("flip_live")
	if err != nil || job == nil {
		t.Fatalf("GetJob = %+v, %v", job, err)
	}
	if job.QueuePriority != 1 {
		t.Fatalf("scan side: QueuePriority = %d, want 1 — the flip guard reads the scanned value", job.QueuePriority)
	}
	if job.ChannelID == nil || *job.ChannelID != chID {
		t.Fatalf("scan side: ChannelID = %v, want %q", job.ChannelID, chID)
	}
	if n, err := db.CountBacklogInFlight(chID); err != nil || n != 1 {
		t.Fatalf("pre-flip CountBacklogInFlight = %d, %v; want 1", n, err)
	}

	res, err := w.streamProc.handleStreamStatus(context.Background(), job,
		&youtube.VideoInfo{StreamStatus: youtube.StreamLive})
	if err != nil {
		t.Fatalf("handleStreamStatus: %v", err)
	}
	if !res.ShouldDownload || res.IsVod {
		t.Fatalf("live result = %+v; want ShouldDownload && !IsVod", res)
	}

	if n, err := db.CountBacklogInFlight(chID); err != nil || n != 0 {
		t.Fatalf("post-Live CountBacklogInFlight = %d, %v; want 0 — the Live write must release the M slot", n, err)
	}
	got, err := db.GetJob("flip_live")
	if err != nil || got == nil {
		t.Fatalf("GetJob after flip = %+v, %v", got, err)
	}
	if got.Status != database.StatusLive {
		t.Fatalf("status = %s, want Live", got.Status)
	}
	if got.QueuePriority != 0 {
		t.Fatalf("queue_priority = %d, want 0 (one-way flip at the Live write)", got.QueuePriority)
	}
}

// TestBroadcastFlip_UpcomingReleasesSlotBeforeWait: a priority-1 job routed
// into the upcoming path flips BEFORE the wait. Asserted through the M query
// (the durable DB value) while waitForLive is still blocking on its probe
// timer, minutes away from the first probe — the job stays Upcoming through
// the whole wait, so only the queue_priority flip can drop the count.
func TestBroadcastFlip_UpcomingReleasesSlotBeforeWait(t *testing.T) {
	w, db := testWorkerSetup(t)
	chID := "UC_flip_up"
	addSchedJob(t, db, &chID, "flip_up", database.StatusUpcoming, 1)

	job, err := db.GetJob("flip_up")
	if err != nil || job == nil {
		t.Fatalf("GetJob = %+v, %v", job, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resCh := make(chan *StreamProcessResult, 1)
	go func() {
		res, _ := w.streamProc.handleStreamStatus(ctx, job,
			&youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming})
		resCh <- res
	}()

	// The first probe is ~5 minutes out; the flip must already be durable
	// while the wait blocks.
	waitForCond(t, 5*time.Second, "queue_priority 0 in the DB while the wait blocks", func() bool {
		n, err := db.CountBacklogInFlight(chID)
		return err == nil && n == 0
	})
	// The wait is still in progress — the flip preceded it, not followed it.
	select {
	case res := <-resCh:
		t.Fatalf("waitForLive returned before cancel: %+v", res)
	default:
	}
	got, err := db.GetJob("flip_up")
	if err != nil || got == nil {
		t.Fatalf("GetJob during wait = %+v, %v", got, err)
	}
	if got.QueuePriority != 0 {
		t.Fatalf("queue_priority = %d, want 0 before the wait", got.QueuePriority)
	}
	if got.Status != database.StatusUpcoming {
		t.Fatalf("status = %s, want still Upcoming through the wait (this path has no status write)", got.Status)
	}

	cancel()
	select {
	case res := <-resCh:
		if res == nil || res.Error != "cancelled" {
			t.Fatalf("result = %+v; want the cancelled wait result", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForLive did not return after cancel")
	}
}

// TestBroadcastFlips_WakeScheduler: each slot-release flip pokes the
// scheduler's coalesced wake signal — through the PRODUCTION wiring
// (NewDownloadWorker hands Scheduler.Wake to the stream processor) — so the
// freed slot admits the channel's next backlog VOD promptly instead of on the
// heartbeat. A priority-0 broadcast must NOT wake: no slot was held.
func TestBroadcastFlips_WakeScheduler(t *testing.T) {
	w, db := testWorkerSetup(t)
	chID := "UC_wake"

	drain := func() {
		select {
		case <-w.scheduler.wake:
		default:
		}
	}
	wakeFired := func() bool {
		select {
		case <-w.scheduler.wake:
			return true
		default:
			return false
		}
	}
	mustGetJob := func(id string) *database.Job {
		t.Helper()
		j, err := db.GetJob(id)
		if err != nil || j == nil {
			t.Fatalf("GetJob(%s) = %+v, %v", id, j, err)
		}
		return j
	}

	// Live flip wakes.
	addSchedJob(t, db, &chID, "wake_live", database.StatusUpcoming, 1)
	drain()
	if _, err := w.streamProc.handleStreamStatus(context.Background(), mustGetJob("wake_live"),
		&youtube.VideoInfo{StreamStatus: youtube.StreamLive}); err != nil {
		t.Fatal(err)
	}
	if !wakeFired() {
		t.Fatal("Live flip did not Wake the scheduler — the freed slot would wait for the heartbeat")
	}

	// Upcoming flip wakes. A pre-cancelled ctx returns the wait immediately —
	// the flip + wake happen before waitForLive is entered.
	addSchedJob(t, db, &chID, "wake_up", database.StatusUpcoming, 1)
	drain()
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, err := w.streamProc.handleStreamStatus(cctx, mustGetJob("wake_up"),
		&youtube.VideoInfo{StreamStatus: youtube.StreamUpcoming}); err != nil {
		t.Fatal(err)
	}
	if !wakeFired() {
		t.Fatal("upcoming flip did not Wake the scheduler")
	}

	// Priority-0 broadcast going live: nothing to release, no wake.
	addSchedJob(t, db, &chID, "wake_p0", database.StatusUpcoming, 0)
	drain()
	if _, err := w.streamProc.handleStreamStatus(context.Background(), mustGetJob("wake_p0"),
		&youtube.VideoInfo{StreamStatus: youtube.StreamLive}); err != nil {
		t.Fatal(err)
	}
	if wakeFired() {
		t.Fatal("priority-0 live transition woke the scheduler — no slot was freed")
	}
}
