package worker

import (
	"context"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// pendingJob represents a job waiting in the queue with its priority.
type pendingJob struct {
	ID       string
	Priority int
}

// calculatePriority returns the queue priority for a job status.
// Live=1 (highest), Upcoming/Downloading=0, Error=-1 (lowest).
// Matches TS: Live streams are processed before upcoming/retried jobs.
func calculatePriority(status database.JobStatus) int {
	switch status {
	case database.StatusLive:
		return 1
	case database.StatusUpcoming, database.StatusDownloading:
		return 0
	case database.StatusError:
		return -1
	default:
		return 0
	}
}

// JobQueue manages the download job queue with separate lifecycle and download concurrency.
// Lifecycle concurrency (maxLifecycle=100) gates how many jobs can be in the
// DOWNLOAD half of the pipeline simultaneously — it is claimed at the
// ShouldDownload decision (owner decision O-F), not at Dequeue, so the wait
// phase (Upcoming, a manually-added offline Twitch channel) runs slot-free.
// Download concurrency (maxDownloads) gates how many jobs can be actively
// downloading segments at once. This matches the TS architecture
// where stream processing (probing, waiting for live) doesn't block download slots.
type JobQueue struct {
	mu               sync.Mutex
	maxDownloads     int
	maxLifecycle     int
	activeLifecycle  int
	activeDownloads  int
	pending          []pendingJob
	pendingSet       map[string]struct{} // O(1) duplicate detection for pending queue
	processing       map[string]context.CancelFunc
	done             map[string]chan struct{} // closed when the job's processing goroutine returns
	holdingDlSlot    map[string]bool          // tracks which jobs hold download slots
	holdingLifecycle map[string]bool          // tracks which jobs hold lifecycle slots
	droppedLogged    map[string]struct{}      // jobs whose backlog drop has been logged
	cancelled        map[string]bool          // tracks user-initiated cancellations (vs shutdown)
	notify           chan struct{}
	dlNotify         chan struct{} // signaling for download slot availability
	lifeNotify       chan struct{} // signaling for lifecycle slot availability
	logger           logger        // optional logger for warnings
	// lifecycleWarnAfter is how long AcquireLifecycleSlot may block before it
	// says so. A field rather than a const so a test can reach the branch
	// without waiting out the real threshold; production installs
	// lifecycleWaitWarnAfter and nothing else writes it.
	lifecycleWarnAfter time.Duration
}

// lifecycleWaitWarnAfter is how long a job may sit behind the lifecycle cap
// before the queue logs it. At the cap the symptom is a capture that simply
// does not start, and until this existed there was no line, no status and no
// counter anyone reads to explain it (sweep-2 Task 9 review, Important 1).
// Long enough that ordinary slot churn is silent; short enough that a wedged
// pool is named while the stream is still live.
const lifecycleWaitWarnAfter = 30 * time.Second

// NewJobQueue creates a new job queue.
func NewJobQueue(maxDownloads int) *JobQueue {
	if maxDownloads <= 0 {
		maxDownloads = 10
	}
	return &JobQueue{
		maxDownloads:       maxDownloads,
		maxLifecycle:       100,
		pendingSet:         make(map[string]struct{}),
		processing:         make(map[string]context.CancelFunc),
		done:               make(map[string]chan struct{}),
		holdingDlSlot:      make(map[string]bool),
		holdingLifecycle:   make(map[string]bool),
		droppedLogged:      make(map[string]struct{}),
		cancelled:          make(map[string]bool),
		notify:             make(chan struct{}, 1),
		dlNotify:           make(chan struct{}, 1),
		lifeNotify:         make(chan struct{}, 1),
		lifecycleWarnAfter: lifecycleWaitWarnAfter,
	}
}

// SetLogger sets an optional logger for queue warnings.
func (q *JobQueue) SetLogger(l logger) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.logger = l
}

// Enqueue adds a job ID to the queue with priority based on its status.
func (q *JobQueue) Enqueue(jobID string, status database.JobStatus) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Don't add duplicates
	if _, ok := q.pendingSet[jobID]; ok {
		return
	}
	if _, ok := q.processing[jobID]; ok {
		return
	}

	// Backlog limit to prevent unbounded growth (matches TS queue.size >= 100)
	if len(q.pending) >= 100 {
		// Once per job, not once per offer: the 60 s heartbeat re-enqueues
		// every ShouldProcess job forever, so the old unconditional Warn was
		// one line per minute per dropped job (sweep-2 ENGINE-10).
		if _, logged := q.droppedLogged[jobID]; !logged {
			q.droppedLogged[jobID] = struct{}{}
			if q.logger != nil {
				q.logger.Warn("job queue full, dropping job", "jobID", jobID, "limit", 100)
			}
		}
		return
	}

	// The job got in: forget the earlier drop so a LATER drop episode is
	// logged again, and so the map holds at most one entry per job currently
	// being dropped.
	delete(q.droppedLogged, jobID)
	q.pending = append(q.pending, pendingJob{ID: jobID, Priority: calculatePriority(status)})
	q.pendingSet[jobID] = struct{}{}

	// Signal that there's work
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// isPending reports whether jobID is waiting in the backlog. Test-facing
// read of state Enqueue owns; kept here so the mutex stays private.
func (q *JobQueue) isPending(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.pendingSet[jobID]
	return ok
}

// Dequeue returns the next job ID and a per-job cancellable context.
// Selects the highest-priority pending job (FIFO among ties).
// Blocks until a job is available or the parent context is cancelled.
// The returned context is cancelled when Cancel(jobID) is called.
func (q *JobQueue) Dequeue(ctx context.Context) (string, context.Context, bool) {
	for {
		q.mu.Lock()
		// No lifecycle gate here (owner decision O-F): the slot is claimed at
		// the ShouldDownload decision instead. Gating the DEQUEUE meant every
		// Upcoming job and every manually-added offline Twitch channel held
		// one of the 100 slots for its whole wait — hours to days — and at
		// 100 waiters a newly live stream was never started at all.
		if len(q.pending) > 0 {
			// Find highest priority job (FIFO among ties — first match wins)
			bestIdx := 0
			for i := 1; i < len(q.pending); i++ {
				if q.pending[i].Priority > q.pending[bestIdx].Priority {
					bestIdx = i
				}
			}
			jobID := q.pending[bestIdx].ID
			q.pending = append(q.pending[:bestIdx], q.pending[bestIdx+1:]...)
			delete(q.pendingSet, jobID)
			jobCtx, cancel := context.WithCancel(ctx)
			q.processing[jobID] = cancel
			q.done[jobID] = make(chan struct{})
			q.mu.Unlock()
			return jobID, jobCtx, true
		}
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return "", nil, false
		case <-q.notify:
			continue
		}
	}
}

// AcquireLifecycleSlot blocks until one of the maxLifecycle slots is free,
// then claims it for jobID. Called once stream processing has decided the job
// will actually download (owner decision O-F), so the cap now bounds
// CONCURRENT DOWNLOADS rather than concurrent waits — a limit no realistic
// install approaches, which is the point: the wait phase is unbounded except
// by per-job goroutine cost. Returns false if ctx is cancelled first — which
// is what makes Stop prompt: a parked job must not hold shutdown open.
//
// A wait that outlasts lifecycleWarnAfter logs ONE line naming the job and how
// many slots are held. Once per wait, not once per wakeup: at the cap every
// release wakes every waiter, and a line each would bury the first one.
func (q *JobQueue) AcquireLifecycleSlot(ctx context.Context, jobID string) bool {
	var warnTimer *time.Timer
	var warnC <-chan time.Time
	defer func() {
		if warnTimer != nil {
			warnTimer.Stop()
		}
	}()
	for {
		q.mu.Lock()
		if q.activeLifecycle < q.maxLifecycle {
			q.activeLifecycle++
			q.holdingLifecycle[jobID] = true
			stillFree := q.activeLifecycle < q.maxLifecycle
			q.mu.Unlock()
			// Cascade the wakeup for the same reason AcquireDownloadSlot does:
			// lifeNotify has capacity 1, so two releases in quick succession
			// collapse into one signal and a second waiter would sleep beside
			// a free slot until the next release.
			if stillFree {
				select {
				case q.lifeNotify <- struct{}{}:
				default:
				}
			}
			return true
		}
		held, limit, warnAfter, lg := q.activeLifecycle, q.maxLifecycle, q.lifecycleWarnAfter, q.logger
		q.mu.Unlock()

		if warnTimer == nil && warnAfter > 0 {
			warnTimer = time.NewTimer(warnAfter)
			warnC = warnTimer.C
		}

		select {
		case <-ctx.Done():
			return false
		case <-q.lifeNotify:
		case <-warnC:
			warnC = nil // one line per wait
			if lg != nil {
				lg.Warn("waiting for a lifecycle slot; the download cannot start until one frees",
					"jobID", jobID, "held", held, "limit", limit, "waited", warnAfter.String())
			}
		}
	}
}

// releaseLifecycleSlotLocked frees jobID's lifecycle slot if it holds one.
// Caller holds q.mu.
func (q *JobQueue) releaseLifecycleSlotLocked(jobID string) {
	if !q.holdingLifecycle[jobID] {
		return
	}
	delete(q.holdingLifecycle, jobID)
	q.activeLifecycle--
	select {
	case q.lifeNotify <- struct{}{}:
	default:
	}
}

// AcquireDownloadSlot blocks until a download slot is available for the given job.
// Called after stream processing completes and before the actual download begins.
// Returns true if the slot was acquired, false if the context was cancelled.
func (q *JobQueue) AcquireDownloadSlot(ctx context.Context, jobID string) bool {
	for {
		q.mu.Lock()
		if q.activeDownloads < q.maxDownloads {
			q.activeDownloads++
			q.holdingDlSlot[jobID] = true
			stillFree := q.activeDownloads < q.maxDownloads
			q.mu.Unlock()
			// Cascade the wakeup: dlNotify has capacity 1, so two releases in
			// quick succession collapse into one signal — without this forward,
			// one waiter would take one slot while a second waiter slept next
			// to a free slot until the NEXT release (potentially hours on live
			// streams). Each successful acquirer re-signals while capacity
			// remains so every free slot finds its waiter.
			if stillFree {
				select {
				case q.dlNotify <- struct{}{}:
				default:
				}
			}
			return true
		}
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return false
		case <-q.dlNotify:
			continue
		}
	}
}

// ReleaseDownloadSlot frees the download slot for a job without cancelling its context.
// Called after download completes but before muxing, so the next download can start
// while muxing runs (mux is CPU-bound, not a download slot).
func (q *JobQueue) ReleaseDownloadSlot(jobID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.holdingDlSlot[jobID] {
		delete(q.holdingDlSlot, jobID)
		q.activeDownloads--
		// Signal that a download slot is free
		select {
		case q.dlNotify <- struct{}{}:
		default:
		}
	}
}

// Complete marks a job as finished, freeing its lifecycle slot and cleaning up.
// Also releases the download slot if still held.
func (q *JobQueue) Complete(jobID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Outside the processing branch on purpose: a job that claimed a slot and
	// then had its row deleted (or whose setJobError/handleCancellation
	// Complete already removed the processing entry) must still give the slot
	// back. The holdingLifecycle guard makes the repeat call a no-op, so the
	// two Completes every error path fires cannot over-release.
	q.releaseLifecycleSlotLocked(jobID)

	// Outside the processing branch for the same reason: a job with a
	// droppedLogged entry was never admitted to pending, so it was never
	// dequeued and has no processing row — inside the branch the delete could
	// not run at all (Task 9 review, Minor 4). Enqueue's success tail is the
	// usual cleaner; this is the belt to its braces.
	delete(q.droppedLogged, jobID)

	if cancel, ok := q.processing[jobID]; ok {
		cancel()
		delete(q.processing, jobID)

		// Drop any unconsumed user-cancel flag so it can't leak or
		// misclassify the job's next run (WasCancelled normally consumes it,
		// but error paths can finish a run without ever reading it).
		delete(q.cancelled, jobID)

		// Signal that the processing goroutine has returned.
		ch := q.done[jobID]
		delete(q.done, jobID)
		if ch != nil {
			close(ch)
		}

		// Also release download slot if still held
		if q.holdingDlSlot[jobID] {
			delete(q.holdingDlSlot, jobID)
			q.activeDownloads--
			select {
			case q.dlNotify <- struct{}{}:
			default:
			}
		}
	}

	// Wake a parked Dequeue so it re-checks the backlog (the lifecycle slot is
	// released above, and Dequeue no longer waits on it).
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Cancel cancels a specific job (user-initiated). Returns true when it
// flagged an actively-processing run — that run's handleCancellation will
// emit the "cancelled" notification, so notifying callers (the cancel
// route) skip their own emission; previously one user cancel produced two
// embeds for an in-flight job.
func (q *JobQueue) Cancel(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Only flag jobs that are actually processing: the flag exists so
	// handleCancellation (inside a running processJob) can distinguish
	// user-cancel from shutdown. For a pending-only job there is no run to
	// classify — the entry would leak forever and, worse, misclassify a
	// future run of the same job (a later shutdown interruption would read
	// the stale flag and flip a resumable job to Cancelled).
	flagged := false
	if cancel, ok := q.processing[jobID]; ok {
		q.cancelled[jobID] = true
		cancel()
		flagged = true
	}
	// Also remove from pending
	for i, pj := range q.pending {
		if pj.ID == jobID {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			delete(q.pendingSet, jobID)
			break
		}
	}
	return flagged
}

// WasCancelled returns true if the job was explicitly cancelled by the user
// (as opposed to being stopped by shutdown). Clears the flag after reading.
func (q *JobQueue) WasCancelled(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cancelled[jobID] {
		delete(q.cancelled, jobID)
		return true
	}
	return false
}

// SetMaxDownloads updates the max parallel downloads.
func (q *JobQueue) SetMaxDownloads(n int) {
	q.mu.Lock()
	q.maxDownloads = n
	q.mu.Unlock()

	select {
	case q.dlNotify <- struct{}{}:
	default:
	}
}

// SetMaxParallel is an alias for SetMaxDownloads for compatibility.
func (q *JobQueue) SetMaxParallel(n int) {
	q.SetMaxDownloads(n)
}

// ActiveCount returns the number of active download slots (what the dashboard displays).
func (q *JobQueue) ActiveCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.activeDownloads
}

// LifecycleCount returns the number of jobs holding a lifecycle slot —
// downloading + muxing. Stream processing and the wait for a stream to go live
// are deliberately NOT counted (owner decision O-F): they run slot-free.
func (q *JobQueue) LifecycleCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.activeLifecycle
}

// PendingCount returns the number of jobs waiting in the queue.
func (q *JobQueue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// IsProcessing returns true if the given job is currently being processed.
func (q *JobQueue) IsProcessing(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.processing[jobID]
	return ok
}

// Done returns a channel that is closed when the job's processing goroutine
// returns. If the jobID isn't currently in flight (already finished or never
// started), a pre-closed channel is returned so callers can select on
// Done(id) without checking IsProcessing first.
func (q *JobQueue) Done(jobID string) <-chan struct{} {
	q.mu.Lock()
	ch, ok := q.done[jobID]
	q.mu.Unlock()
	if !ok {
		// Already done (or never started). Return a pre-closed channel.
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return ch
}

// ShouldProcess returns true if the job's status indicates it should be processed.
func ShouldProcess(job *database.Job) bool {
	switch job.Status {
	case database.StatusUpcoming, database.StatusLive, database.StatusDownloading:
		return true
	default:
		return false
	}
}
