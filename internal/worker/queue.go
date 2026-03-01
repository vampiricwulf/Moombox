package worker

import (
	"context"
	"sync"

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
// process/wait/download pipeline simultaneously. Download concurrency (maxDownloads) gates
// how many jobs can be actively downloading segments at once. This matches the TS architecture
// where stream processing (probing, waiting for live) doesn't block download slots.
type JobQueue struct {
	mu              sync.Mutex
	maxDownloads    int
	maxLifecycle    int
	activeLifecycle int
	activeDownloads int
	pending         []pendingJob
	pendingSet      map[string]struct{} // O(1) duplicate detection for pending queue
	processing      map[string]context.CancelFunc
	holdingDlSlot   map[string]bool // tracks which jobs hold download slots
	cancelled       map[string]bool // tracks user-initiated cancellations (vs shutdown)
	notify          chan struct{}
	dlNotify        chan struct{} // signaling for download slot availability
}

// NewJobQueue creates a new job queue.
func NewJobQueue(maxDownloads int) *JobQueue {
	if maxDownloads <= 0 {
		maxDownloads = 2
	}
	return &JobQueue{
		maxDownloads:  maxDownloads,
		maxLifecycle:  100,
		pendingSet:    make(map[string]struct{}),
		processing:    make(map[string]context.CancelFunc),
		holdingDlSlot: make(map[string]bool),
		cancelled:     make(map[string]bool),
		notify:        make(chan struct{}, 1),
		dlNotify:      make(chan struct{}, 1),
	}
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
		return // Queue full — caller should log this
	}

	q.pending = append(q.pending, pendingJob{ID: jobID, Priority: calculatePriority(status)})
	q.pendingSet[jobID] = struct{}{}

	// Signal that there's work
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Dequeue returns the next job ID and a per-job cancellable context when a lifecycle slot
// is available. Selects the highest-priority pending job (FIFO among ties).
// Blocks until a job is available or the parent context is cancelled.
// The returned context is cancelled when Cancel(jobID) is called.
func (q *JobQueue) Dequeue(ctx context.Context) (string, context.Context, bool) {
	for {
		q.mu.Lock()
		if q.activeLifecycle < q.maxLifecycle && len(q.pending) > 0 {
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
			q.activeLifecycle++
			jobCtx, cancel := context.WithCancel(ctx)
			q.processing[jobID] = cancel
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

// AcquireDownloadSlot blocks until a download slot is available for the given job.
// Called after stream processing completes and before the actual download begins.
// Returns true if the slot was acquired, false if the context was cancelled.
func (q *JobQueue) AcquireDownloadSlot(ctx context.Context, jobID string) bool {
	for {
		q.mu.Lock()
		if q.activeDownloads < q.maxDownloads {
			q.activeDownloads++
			q.holdingDlSlot[jobID] = true
			q.mu.Unlock()
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

	if cancel, ok := q.processing[jobID]; ok {
		cancel()
		delete(q.processing, jobID)
		q.activeLifecycle--

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

	// Signal that a lifecycle slot is free
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Cancel cancels a specific job (user-initiated).
func (q *JobQueue) Cancel(jobID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.cancelled[jobID] = true

	if cancel, ok := q.processing[jobID]; ok {
		cancel()
	}
	// Also remove from pending
	for i, pj := range q.pending {
		if pj.ID == jobID {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			delete(q.pendingSet, jobID)
			break
		}
	}
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

// LifecycleCount returns the number of jobs in lifecycle processing
// (stream processing + downloading + muxing).
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

// ShouldProcess returns true if the job's status indicates it should be processed.
func ShouldProcess(job *database.Job) bool {
	switch job.Status {
	case database.StatusUpcoming, database.StatusLive, database.StatusDownloading:
		return true
	default:
		return false
	}
}
