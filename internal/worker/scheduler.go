package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// jobEnqueuer is the narrow JobQueue surface the scheduler needs — tests
// inject a stub; production passes the worker's *JobQueue.
type jobEnqueuer interface {
	Enqueue(jobID string, status database.JobStatus)
}

// Scheduler admits backlog (Queued) jobs M-at-a-time per channel — the
// archive-slots pacing of spec §10. It owns the only path out of Queued:
// ShouldProcess(Queued) is false by design, so neither startup recovery nor
// the worker's heartbeat poller ever touches a Queued row.
type Scheduler struct {
	db    *database.Database
	queue jobEnqueuer
	// updateJob is the durable-write primitive (production:
	// db.UpdateJobFields). Injected so tests can spy on admission ordering.
	updateJob func(jobID string, fields map[string]any)
	// resolveSlots maps a channel_id to its archive_slots M. Injected by the
	// host (cmd/moombox) against the live config store — see
	// DownloadWorker.SetArchiveSlotsResolver.
	resolveSlots func(channelID string) int
	// wake coalesces signals: capacity 1, non-blocking send. A wake that
	// arrives while one is already pending is absorbed — the run loop
	// drains at most one signal per admission sweep.
	wake chan struct{}
	log  logger
}

// newScheduler creates the worker-owned scheduler. resolveSlots stays nil
// until the host injects it (before Start).
func newScheduler(db *database.Database, queue jobEnqueuer, log logger) *Scheduler {
	return &Scheduler{
		db:    db,
		queue: queue,
		updateJob: func(jobID string, fields map[string]any) {
			db.UpdateJobFields(jobID, fields)
		},
		wake: make(chan struct{}, 1),
		log:  log,
	}
}

// Wake signals the scheduler that backlog state changed (a Queued job was
// created, or a slot may have freed). Non-blocking and safe from any
// goroutine — repeat calls coalesce into the single buffered signal.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run is the scheduler's single goroutine: it wakes on Wake() signals (backlog
// creation, job completion) or the safety heartbeat, then performs one
// admission sweep. Single-threaded by construction, so count-then-admit needs
// no lock — two concurrent sweeps reading M-1 would both admit.
//
// Wrapped in the pollForJobs restart-on-panic pattern (worker.go): this
// goroutine owns the only path out of Queued, so a permanent death would
// strand the backlog silently with no error anywhere.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("scheduler panic, restarting", "panic", fmt.Sprint(r))
				}
			}()

			for {
				select {
				case <-ctx.Done():
					return
				case <-s.wake:
				case <-time.After(heartbeatInterval):
					// Safety net: catches slots freed by paths that don't
					// Wake (e.g. user-driven MuxJob finishing, CancelJob on
					// a never-dequeued row).
				}
				s.sweep()
			}
		}()

		// Check if context is done before restarting.
		// ctx-aware sleep so shutdown during the pause returns promptly.
		if err := utils.Sleep(ctx, time.Second); err != nil {
			return
		}
	}
}

// sweep performs one admission pass: for every channel with Queued rows,
// admit up to (archive_slots − in-flight) backlog jobs, newest published
// first.
func (s *Scheduler) sweep() {
	if s.resolveSlots == nil {
		// Host wiring bug. Admitting nothing here would strand the backlog
		// silently — the exact failure mode spec §10 warns about — so shout.
		s.log.Error("scheduler: no archive-slots resolver wired; backlog admission stalled")
		return
	}

	channels, err := s.db.QueuedChannels()
	if err != nil {
		s.log.Error("scheduler: QueuedChannels failed", "err", err)
		return
	}
	for _, ch := range channels {
		inFlight, err := s.db.CountBacklogInFlight(ch)
		if err != nil {
			s.log.Error("scheduler: CountBacklogInFlight failed", "channel", ch, "err", err)
			continue
		}
		admit := s.resolveSlots(ch) - inFlight
		if admit <= 0 {
			continue
		}
		ids, err := s.db.NextQueuedJobs(ch, admit)
		if err != nil {
			s.log.Error("scheduler: NextQueuedJobs failed", "channel", ch, "err", err)
			continue
		}
		for _, id := range ids {
			// 1. durable FIRST — this is what the M count observes; Enqueue
			//    touches no DB row, so without this write M counts 0 forever
			//    and every tick over-admits. Upcoming is what creators write
			//    for "created, awaiting processing" and ShouldProcess accepts
			//    it, so a crash between the two steps is self-healing:
			//    enqueueExistingJobs re-enqueues the row on restart.
			s.updateJob(id, map[string]any{"status": database.StatusUpcoming})
			// 2. hand to JobQueue
			s.queue.Enqueue(id, database.StatusUpcoming)
			s.log.Info("scheduler: admitted backlog job", "jobID", id, "channel", ch)
		}
	}
}
