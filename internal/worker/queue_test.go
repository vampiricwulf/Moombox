package worker

import (
	"context"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func TestNewJobQueue(t *testing.T) {
	tests := []struct {
		name              string
		maxDownloads      int
		expectedDownloads int
	}{
		{"positive value", 5, 5},
		{"one", 1, 1},
		{"zero defaults to 10", 0, 10},
		{"negative defaults to 10", -1, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := NewJobQueue(tt.maxDownloads)
			if q.maxDownloads != tt.expectedDownloads {
				t.Errorf("NewJobQueue(%d).maxDownloads = %d, want %d", tt.maxDownloads, q.maxDownloads, tt.expectedDownloads)
			}
			if q.maxLifecycle != 100 {
				t.Errorf("NewJobQueue: maxLifecycle = %d, want 100", q.maxLifecycle)
			}
			if q.processing == nil {
				t.Error("NewJobQueue: processing map should be initialized")
			}
			if q.holdingDlSlot == nil {
				t.Error("NewJobQueue: holdingDlSlot map should be initialized")
			}
			if q.notify == nil {
				t.Error("NewJobQueue: notify channel should be initialized")
			}
			if q.dlNotify == nil {
				t.Error("NewJobQueue: dlNotify channel should be initialized")
			}
		})
	}
}

func TestEnqueueDequeue_FIFO(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job2", database.StatusUpcoming)
	q.Enqueue("job3", database.StatusUpcoming)

	if q.PendingCount() != 3 {
		t.Errorf("PendingCount() = %d, want 3", q.PendingCount())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	id1, _, ok1 := q.Dequeue(ctx)
	id2, _, ok2 := q.Dequeue(ctx)
	id3, _, ok3 := q.Dequeue(ctx)

	if !ok1 || !ok2 || !ok3 {
		t.Fatal("Dequeue returned false unexpectedly")
	}
	if id1 != "job1" {
		t.Errorf("first dequeue = %q, want %q", id1, "job1")
	}
	if id2 != "job2" {
		t.Errorf("second dequeue = %q, want %q", id2, "job2")
	}
	if id3 != "job3" {
		t.Errorf("third dequeue = %q, want %q", id3, "job3")
	}
}

func TestEnqueue_DuplicateRejection(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job1", database.StatusUpcoming)

	if q.PendingCount() != 1 {
		t.Errorf("PendingCount() after duplicate enqueues = %d, want 1", q.PendingCount())
	}
}

func TestEnqueue_RejectsProcessingJob(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, _, ok := q.Dequeue(ctx)
	if !ok {
		t.Fatal("Dequeue returned false unexpectedly")
	}

	// job1 is now processing; re-enqueue should be rejected
	q.Enqueue("job1", database.StatusUpcoming)
	if q.PendingCount() != 0 {
		t.Errorf("PendingCount() = %d, want 0 (job1 is processing)", q.PendingCount())
	}
}

func TestDequeue_ContextCancellation(t *testing.T) {
	q := NewJobQueue(10) // no jobs enqueued

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, ok := q.Dequeue(ctx)
	if ok {
		t.Error("Dequeue with no jobs should return false on context cancellation")
	}
}

func TestDequeue_PriorityOrdering(t *testing.T) {
	q := NewJobQueue(10)

	// Enqueue jobs with different priorities: Error(-1), Upcoming(0), Live(1)
	q.Enqueue("error_job", database.StatusError)
	q.Enqueue("upcoming_job", database.StatusUpcoming)
	q.Enqueue("live_job", database.StatusLive)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Live should come first (priority 1)
	id1, _, ok1 := q.Dequeue(ctx)
	if !ok1 || id1 != "live_job" {
		t.Errorf("first dequeue = %q, ok=%v, want live_job/true", id1, ok1)
	}

	// Upcoming should come second (priority 0)
	id2, _, ok2 := q.Dequeue(ctx)
	if !ok2 || id2 != "upcoming_job" {
		t.Errorf("second dequeue = %q, ok=%v, want upcoming_job/true", id2, ok2)
	}

	// Error should come last (priority -1)
	id3, _, ok3 := q.Dequeue(ctx)
	if !ok3 || id3 != "error_job" {
		t.Errorf("third dequeue = %q, ok=%v, want error_job/true", id3, ok3)
	}
}

func TestDequeue_PriorityFIFOWithinTies(t *testing.T) {
	q := NewJobQueue(10)

	// Enqueue two live jobs — should be FIFO among same priority
	q.Enqueue("live1", database.StatusLive)
	q.Enqueue("live2", database.StatusLive)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	id1, _, _ := q.Dequeue(ctx)
	id2, _, _ := q.Dequeue(ctx)

	if id1 != "live1" {
		t.Errorf("first dequeue = %q, want live1 (FIFO among ties)", id1)
	}
	if id2 != "live2" {
		t.Errorf("second dequeue = %q, want live2 (FIFO among ties)", id2)
	}
}

func TestDequeue_LifecycleGating(t *testing.T) {
	q := NewJobQueue(10)
	q.maxLifecycle = 1 // Override for testing
	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job2", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Dequeue first job (fills the single lifecycle slot)
	id1, _, ok := q.Dequeue(ctx)
	if !ok || id1 != "job1" {
		t.Fatalf("first dequeue = %q, ok=%v, want job1/true", id1, ok)
	}

	if q.LifecycleCount() != 1 {
		t.Errorf("LifecycleCount() = %d, want 1", q.LifecycleCount())
	}

	// Second dequeue should block because maxLifecycle=1
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()

	_, _, ok = q.Dequeue(shortCtx)
	if ok {
		t.Error("Dequeue should have blocked/returned false when lifecycle slots full")
	}
}

func TestAcquireDownloadSlot_BlocksWhenFull(t *testing.T) {
	q := NewJobQueue(1) // Only 1 download slot
	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job2", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Dequeue both (lifecycle allows it)
	q.Dequeue(ctx)
	q.Dequeue(ctx)

	// Acquire download slot for job1
	if !q.AcquireDownloadSlot(ctx, "job1") {
		t.Fatal("AcquireDownloadSlot should succeed for first job")
	}

	if q.ActiveCount() != 1 {
		t.Errorf("ActiveCount() = %d, want 1", q.ActiveCount())
	}

	// Acquire download slot for job2 should block because maxDownloads=1
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()

	if q.AcquireDownloadSlot(shortCtx, "job2") {
		t.Error("AcquireDownloadSlot should have blocked when all download slots are full")
	}
}

func TestAcquireDownloadSlot_UnblocksOnRelease(t *testing.T) {
	q := NewJobQueue(1)
	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job2", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	q.Dequeue(ctx)
	q.Dequeue(ctx)

	q.AcquireDownloadSlot(ctx, "job1")

	// Release job1's download slot in a goroutine
	go func() {
		time.Sleep(20 * time.Millisecond)
		q.ReleaseDownloadSlot("job1")
	}()

	// job2 should now be able to acquire
	if !q.AcquireDownloadSlot(ctx, "job2") {
		t.Error("AcquireDownloadSlot should succeed after release")
	}
}

func TestReleaseDownloadSlot_FreesSlot(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	q.Dequeue(ctx)
	q.AcquireDownloadSlot(ctx, "job1")

	if q.ActiveCount() != 1 {
		t.Errorf("ActiveCount() = %d, want 1", q.ActiveCount())
	}

	q.ReleaseDownloadSlot("job1")

	if q.ActiveCount() != 0 {
		t.Errorf("ActiveCount() after ReleaseDownloadSlot = %d, want 0", q.ActiveCount())
	}
}

func TestReleaseDownloadSlot_DoubleRelease(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	q.Dequeue(ctx)
	q.AcquireDownloadSlot(ctx, "job1")
	q.ReleaseDownloadSlot("job1")
	q.ReleaseDownloadSlot("job1") // should be a no-op

	if q.ActiveCount() != 0 {
		t.Errorf("ActiveCount() after double ReleaseDownloadSlot = %d, want 0", q.ActiveCount())
	}
}

func TestReleaseDownloadSlot_NonexistentJob(t *testing.T) {
	q := NewJobQueue(10)
	// Should not panic
	q.ReleaseDownloadSlot("nonexistent")
}

func TestComplete_FreesLifecycleSlot(t *testing.T) {
	q := NewJobQueue(10)
	q.maxLifecycle = 1
	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job2", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	q.Dequeue(ctx)
	if q.IsProcessing("job1") != true {
		t.Error("job1 should be processing after dequeue")
	}

	q.Complete("job1")

	if q.IsProcessing("job1") {
		t.Error("job1 should not be processing after Complete")
	}
	if q.LifecycleCount() != 0 {
		t.Errorf("LifecycleCount() after Complete = %d, want 0", q.LifecycleCount())
	}

	// job2 should now be dequeueable
	id2, _, ok := q.Dequeue(ctx)
	if !ok || id2 != "job2" {
		t.Errorf("dequeue after Complete = %q, ok=%v, want job2/true", id2, ok)
	}
}

func TestComplete_AlsoReleasesDownloadSlot(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	q.Dequeue(ctx)
	q.AcquireDownloadSlot(ctx, "job1")

	if q.ActiveCount() != 1 {
		t.Errorf("ActiveCount() = %d, want 1", q.ActiveCount())
	}

	// Complete without prior ReleaseDownloadSlot — should release both
	q.Complete("job1")

	if q.ActiveCount() != 0 {
		t.Errorf("ActiveCount() after Complete = %d, want 0", q.ActiveCount())
	}
	if q.LifecycleCount() != 0 {
		t.Errorf("LifecycleCount() after Complete = %d, want 0", q.LifecycleCount())
	}
}

func TestComplete_AfterReleaseDownloadSlot(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	q.Dequeue(ctx)
	q.AcquireDownloadSlot(ctx, "job1")
	q.ReleaseDownloadSlot("job1")
	q.Complete("job1")

	// Should not double-decrement
	if q.ActiveCount() != 0 {
		t.Errorf("ActiveCount() = %d, want 0", q.ActiveCount())
	}
	if q.LifecycleCount() != 0 {
		t.Errorf("LifecycleCount() = %d, want 0", q.LifecycleCount())
	}
	if q.IsProcessing("job1") {
		t.Error("job1 should not be processing after Complete")
	}
}

func TestCancel_RemovesFromPending(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job2", database.StatusUpcoming)
	q.Enqueue("job3", database.StatusUpcoming)

	q.Cancel("job2")

	if q.PendingCount() != 2 {
		t.Errorf("PendingCount() after Cancel = %d, want 2", q.PendingCount())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	id1, _, _ := q.Dequeue(ctx)
	id2, _, _ := q.Dequeue(ctx)

	if id1 != "job1" {
		t.Errorf("first dequeue = %q, want job1", id1)
	}
	if id2 != "job3" {
		t.Errorf("second dequeue = %q, want job3 (job2 was cancelled)", id2)
	}
}

func TestCancel_ProcessingJob(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, jobCtx, _ := q.Dequeue(ctx)

	q.Cancel("job1")

	// The job context should be cancelled
	select {
	case <-jobCtx.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Error("Cancel should cancel the job's context")
	}
}

func TestCancel_NonexistentJob(t *testing.T) {
	q := NewJobQueue(10)
	// Should not panic
	q.Cancel("nonexistent")
}

func TestIsProcessing(t *testing.T) {
	q := NewJobQueue(10)
	q.Enqueue("job1", database.StatusUpcoming)

	if q.IsProcessing("job1") {
		t.Error("job1 should not be processing before dequeue")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	q.Dequeue(ctx)

	if !q.IsProcessing("job1") {
		t.Error("job1 should be processing after dequeue")
	}

	q.Complete("job1")

	if q.IsProcessing("job1") {
		t.Error("job1 should not be processing after Complete")
	}
}

func TestSetMaxDownloads(t *testing.T) {
	q := NewJobQueue(1)
	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job2", database.StatusUpcoming)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	q.Dequeue(ctx)
	q.Dequeue(ctx)

	// Acquire slot for job1
	q.AcquireDownloadSlot(ctx, "job1")

	// Increase max downloads
	q.SetMaxDownloads(2)

	// Now job2 should be able to acquire a download slot
	if !q.AcquireDownloadSlot(ctx, "job2") {
		t.Error("AcquireDownloadSlot should succeed after SetMaxDownloads(2)")
	}
}

func TestSetMaxParallel_Alias(t *testing.T) {
	q := NewJobQueue(1)
	q.SetMaxParallel(5)

	q.mu.Lock()
	if q.maxDownloads != 5 {
		t.Errorf("SetMaxParallel(5) should update maxDownloads to 5, got %d", q.maxDownloads)
	}
	q.mu.Unlock()
}

func TestShouldProcess(t *testing.T) {
	tests := []struct {
		name     string
		status   database.JobStatus
		expected bool
	}{
		{"upcoming", database.StatusUpcoming, true},
		{"live", database.StatusLive, true},
		{"downloading", database.StatusDownloading, true},
		{"muxing", database.StatusMuxing, false},
		{"finished", database.StatusFinished, false},
		{"error", database.StatusError, false},
		{"cancelled", database.StatusCancelled, false},
		{"cookies", database.StatusCookies, false},
		{"queued", database.StatusQueued, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &database.Job{Status: tt.status}
			result := ShouldProcess(job)
			if result != tt.expected {
				t.Errorf("ShouldProcess(status=%q) = %v, want %v", tt.status, result, tt.expected)
			}
		})
	}
}

func TestActiveAndPendingCounts(t *testing.T) {
	q := NewJobQueue(10)

	if q.ActiveCount() != 0 {
		t.Errorf("initial ActiveCount() = %d, want 0", q.ActiveCount())
	}
	if q.PendingCount() != 0 {
		t.Errorf("initial PendingCount() = %d, want 0", q.PendingCount())
	}
	if q.LifecycleCount() != 0 {
		t.Errorf("initial LifecycleCount() = %d, want 0", q.LifecycleCount())
	}

	q.Enqueue("job1", database.StatusUpcoming)
	q.Enqueue("job2", database.StatusUpcoming)

	if q.PendingCount() != 2 {
		t.Errorf("PendingCount() after 2 enqueues = %d, want 2", q.PendingCount())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	q.Dequeue(ctx)

	if q.LifecycleCount() != 1 {
		t.Errorf("LifecycleCount() after 1 dequeue = %d, want 1", q.LifecycleCount())
	}
	if q.ActiveCount() != 0 {
		t.Errorf("ActiveCount() after dequeue (no download slot) = %d, want 0", q.ActiveCount())
	}
	if q.PendingCount() != 1 {
		t.Errorf("PendingCount() after 1 dequeue = %d, want 1", q.PendingCount())
	}

	q.AcquireDownloadSlot(ctx, "job1")

	if q.ActiveCount() != 1 {
		t.Errorf("ActiveCount() after AcquireDownloadSlot = %d, want 1", q.ActiveCount())
	}
}

func TestCalculatePriority(t *testing.T) {
	tests := []struct {
		status   database.JobStatus
		expected int
	}{
		{database.StatusLive, 1},
		{database.StatusUpcoming, 0},
		{database.StatusDownloading, 0},
		{database.StatusError, -1},
		{database.StatusFinished, 0},
		{database.StatusCancelled, 0},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			got := calculatePriority(tt.status)
			if got != tt.expected {
				t.Errorf("calculatePriority(%q) = %d, want %d", tt.status, got, tt.expected)
			}
		})
	}
}

// TestAcquireDownloadSlotCascadingWakeup guards the cascading-wakeup forward
// in AcquireDownloadSlot. dlNotify has capacity 1, so two releases in quick
// succession can collapse into one signal; without the acquirer re-signaling
// while capacity remains, one of two parked waiters could sleep next to a
// free slot until the NEXT release. The invariant: when both slots free up,
// both waiters acquire promptly.
func TestAcquireDownloadSlotCascadingWakeup(t *testing.T) {
	q := NewJobQueue(2)
	ctx := context.Background()

	if !q.AcquireDownloadSlot(ctx, "a") || !q.AcquireDownloadSlot(ctx, "b") {
		t.Fatal("initial acquires failed")
	}

	acquired := make(chan string, 2)
	for _, id := range []string{"w1", "w2"} {
		go func(id string) {
			if q.AcquireDownloadSlot(ctx, id) {
				acquired <- id
			}
		}(id)
	}
	// Let both waiters park on dlNotify before releasing.
	time.Sleep(50 * time.Millisecond)

	q.ReleaseDownloadSlot("a")
	q.ReleaseDownloadSlot("b")

	for i := range 2 {
		select {
		case <-acquired:
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d never acquired a free slot (lost wakeup)", i+1)
		}
	}
}

// TestQueuedIsRestingState pins the Queued resting-state guarantees:
// ShouldProcess is an enqueuer predicate — both callers (worker.go
// enqueueExistingJobs and the pollForJobs heartbeat) feed its true straight
// into queue.Enqueue, which silently drops beyond 100 pending items — so
// Queued must stay false there. It is also not a terminal state.
func TestQueuedIsRestingState(t *testing.T) {
	j := &database.Job{ID: "q1", Status: database.StatusQueued}
	if ShouldProcess(j) {
		t.Fatal("ShouldProcess(Queued) must be false — both callers Enqueue on true (worker.go startup + heartbeat)")
	}
	if j.IsTerminal() {
		t.Fatal("Queued is waiting, not finished")
	}
}

// TestEnqueueExistingJobsIgnoresQueued runs the startup scan against a DB
// holding a Queued row and asserts the row is neither enqueued (it waits for
// the archive-slots scheduler, not the worker's self-scan) nor mutated (only
// interrupted Muxing jobs are rewritten at startup). An Upcoming control row
// proves the scan actually enqueues eligible jobs, so the Queued assertions
// can't pass vacuously.
func TestEnqueueExistingJobsIgnoresQueued(t *testing.T) {
	w, db := testWorkerSetup(t)

	if _, err := db.AddJob(&database.Job{
		ID: "q_rest", VideoID: "q_rest", URL: "u", Status: database.StatusQueued,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&database.Job{
		ID: "u_ctrl", VideoID: "u_ctrl", URL: "u", Status: database.StatusUpcoming,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetJob("q_rest")
	if err != nil {
		t.Fatal(err)
	}

	w.enqueueExistingJobs()

	if got := w.queue.PendingCount(); got != 1 {
		t.Fatalf("PendingCount = %d, want 1 (only the Upcoming control job)", got)
	}
	w.queue.mu.Lock()
	_, queuedPending := w.queue.pendingSet["q_rest"]
	_, upcomingPending := w.queue.pendingSet["u_ctrl"]
	w.queue.mu.Unlock()
	if queuedPending {
		t.Error("Queued job was enqueued — it must wait for the scheduler")
	}
	if !upcomingPending {
		t.Error("Upcoming control job was not enqueued — the scan didn't run as expected")
	}

	after, err := db.GetJob("q_rest")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != database.StatusQueued {
		t.Errorf("Queued job status mutated: got %s, want Queued", after.Status)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("Queued job row mutated: updated_at changed %q -> %q", before.UpdatedAt, after.UpdatedAt)
	}
}
