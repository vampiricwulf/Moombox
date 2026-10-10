package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
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
	// updateJob is the admission's durable write, reporting whether it
	// applied (production: db.UpdateJobFieldsIf on Queued). Injected so
	// tests can spy on admission ordering.
	updateJob func(jobID string, fields map[string]any) bool
	// resolveSlots maps a channel_id to its archive_slots M. Injected by the
	// host (cmd/moombox) against the live config store — see
	// DownloadWorker.SetArchiveSlotsResolver.
	resolveSlots func(channelID string) int
	// wake coalesces signals: capacity 1, non-blocking send. A wake that
	// arrives while one is already pending is absorbed — the run loop
	// drains at most one signal per admission sweep.
	wake chan struct{}
	log  logger

	// conn is the connectivity monitor (nil: always online). An admission
	// made during an outage is a backlog VOD sent to fail its first fetch, so
	// sweep admits nothing while it reports offline, and Run sweeps again the
	// moment it reports online.
	conn Connectivity

	// readDisk reads the volume the jobs write to against the disk_critical
	// threshold (DownloadWorker.readOutputDisk; nil: never full). An
	// admission while it reads critical is a backlog VOD sent to download
	// onto a disk that is filling, so sweep admits nothing from then until it
	// reads clear of the threshold, and reads again on every sweep after —
	// the heartbeat's included, which is what notices the space coming back.
	readDisk func() (diskReading, error)
	// diskHeld is the gate's state as the last reading left it: what a
	// reading inside the recovery margin keeps, and what logs the close and
	// the reopen once each. Seeded before Start by RestoreDiskHold, and
	// touched after that only by sweep, which only Run's goroutine calls.
	diskHeld bool
	// recordDiskHold is told diskHeld each time it changes, and when
	// RestoreDiskHold seeds it (RecordDiskHold; nil: memory only). Set
	// before Start, like the seed.
	recordDiskHold func(held bool)

	// holds are the backlog jobs a transient pre-download failure returned
	// to Queued (DownloadWorker.requeueBacklogAfterTransientFailure), each
	// with the time before which sweep must not admit it again. In memory:
	// a restart forgets the backoff, never the job. Lazily allocated, so a
	// Scheduler literal (the tests build several) needs no constructor.
	holdMu sync.Mutex
	holds  map[string]time.Time
}

// newScheduler creates the worker-owned scheduler. resolveSlots stays nil
// until the host injects it (before Start).
func newScheduler(db *database.Database, queue jobEnqueuer, log logger) *Scheduler {
	return &Scheduler{
		db:    db,
		queue: queue,
		// A compare-and-set on Queued: sweep read the row Queued, and an
		// operator's Cancel can land before this write — which, written
		// unconditionally, turned the Cancelled row back into a download.
		updateJob: func(jobID string, fields map[string]any) bool {
			return db.UpdateJobFieldsIf(jobID, database.StatusQueued, fields)
		},
		wake: make(chan struct{}, 1),
		log:  log,
	}
}

// holdUntil keeps sweep from admitting jobID before until.
func (s *Scheduler) holdUntil(jobID string, until time.Time) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	if s.holds == nil {
		s.holds = map[string]time.Time{}
	}
	s.holds[jobID] = until
}

// unhold drops jobID's hold: the requeue it was placed for did not happen.
func (s *Scheduler) unhold(jobID string) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	delete(s.holds, jobID)
}

// held reports whether jobID is still held at now, forgetting a hold that
// has run out.
func (s *Scheduler) held(jobID string, now time.Time) bool {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	until, ok := s.holds[jobID]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(s.holds, jobID)
		return false
	}
	return true
}

// heldCount is how many holds are outstanding, across every channel.
func (s *Scheduler) heldCount() int {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	return len(s.holds)
}

// diskGateClosed reports whether backlog admission must wait for disk space.
// The gate closes when the volume the jobs write to reaches the disk_critical
// threshold and reopens only once usage is config.DiskRecoveryMargin points
// below it — the reading an open disk_critical alert steps down on. A reading
// in between leaves the gate as it was: reopened on the first reading under
// the threshold, a volume sitting on the line admitted a backlog VOD every
// time it dipped under it, each one more download onto the disk the gate had
// just closed for. The close and the reopen are each logged once, not once
// per sweep.
//
// A reading that fails leaves the gate as the last good one left it — the
// disk alerts freeze on their last good reading the same way, and say so
// themselves — so a volume that went offline while full does not reopen
// admission, and one that was fine is not held on no evidence.
func (s *Scheduler) diskGateClosed() bool {
	if s.readDisk == nil {
		return false
	}
	r, err := s.readDisk()
	if err != nil {
		s.log.Debug("scheduler: disk space reading failed; the backlog disk gate stays as it was",
			"dir", r.dir, "held", s.diskHeld, "err", err)
		return s.diskHeld
	}
	closed := s.diskHeld
	switch {
	case r.critical:
		closed = true
	case r.clear:
		closed = false
	}
	if closed != s.diskHeld {
		s.diskHeld = closed
		s.reportDiskHold()
		freeGB := fmt.Sprintf("%.1f", float64(r.free)/(1<<30))
		usedPct := fmt.Sprintf("%.1f", r.usedPct)
		if closed {
			s.log.Warn("scheduler: disk at the critical threshold; backlog VODs wait in Queued until usage falls to resumeAtPct (live and manually added jobs are not held)",
				"dir", r.dir, "usedPct", usedPct, "freeGB", freeGB,
				"resumeAtPct", fmt.Sprintf("%.1f", r.resumeAtPct))
		} else {
			s.log.Info("scheduler: disk clear of the critical threshold again; backlog admission resumes",
				"dir", r.dir, "usedPct", usedPct, "freeGB", freeGB)
		}
	}
	return closed
}

// RestoreDiskHold starts the backlog disk gate closed, for a host whose
// previous process left it closed, or left a disk_critical alert open
// (cmd/moombox). The gate keeps its close in memory, and a new process's gate
// started open: it read a volume still inside the recovery margin — 94%
// against a threshold of 95 — as neither critical nor clear, kept the open
// state it started with, and its first sweep admitted backlog onto the disk
// the last run had stopped admitting to. The first reading clear of the
// threshold reopens it, as it reopens any close. The seeded hold is reported
// like any other (RecordDiskHold). Call it before the worker starts: Run's
// goroutine owns the gate from then on.
func (s *Scheduler) RestoreDiskHold() {
	s.diskHeld = true
	s.reportDiskHold()
	s.log.Warn("scheduler: the last run stopped with the backlog disk gate closed or a disk_critical alert open; backlog VODs wait in Queued until usage falls marginPoints below the critical threshold (live and manually added jobs are not held)",
		"marginPoints", config.DiskRecoveryMargin)
}

// RecordDiskHold has the gate report whether it is holding backlog admission
// for disk space: on every close and every reopen, and when RestoreDiskHold
// seeds a hold. cmd/moombox persists it beside the open alerts, for the next
// process's RestoreDiskHold. The gate reads the disk on every sweep while the
// alerts read it about every six minutes, so the alert's saved level alone
// missed a close the alerts never read — and the next process started open
// inside the margin. Call it before the worker starts, as RestoreDiskHold:
// record is called from Run's goroutine from then on.
func (s *Scheduler) RecordDiskHold(record func(held bool)) {
	s.recordDiskHold = record
}

// reportDiskHold tells the recorder RecordDiskHold set what diskHeld is now.
func (s *Scheduler) reportDiskHold() {
	if s.recordDiskHold != nil {
		s.recordDiskHold(s.diskHeld)
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
	// Connectivity returning is backlog state changing: sweep held every
	// admission while offline, and the heartbeat would otherwise leave the
	// backlog idle for up to a minute after the network came back.
	if s.conn != nil {
		unregister := s.conn.OnStateChange(func(online bool) {
			if online {
				s.Wake()
			}
		})
		defer unregister()
	}
	// first gates the startup sweep below to the FIRST pass of this loop. The
	// panic-restart re-enters the same func literal, so an ungated sweep-at-
	// start would turn a deterministically panicking sweep() into a ~1 s loop
	// (panic -> recover -> 1 s restart sleep -> sweep -> panic) instead of the
	// ~61 s one it is without it — ~60× the restart lines and ~60× the DB load
	// on a path that is already a bug. Cleared BEFORE the sweep runs, so the
	// panicking case cannot re-enter it.
	first := true
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("scheduler panic, restarting", "panic", fmt.Sprint(r))
				}
			}()

			// Startup admission sweep (MON-9). Nothing Wakes a process whose
			// only backlog predates it: the wake sites are all event-driven —
			// backlog CREATION, job COMPLETION, a cookie repair returning
			// parked backlog to Queued — and a restart has none of them, so
			// leftover Queued rows waited for the 60 s heartbeat. One pass per
			// Run, which in the healthy case is one extra sweep per process.
			if first {
				first = false
				s.sweep()
			}

			for {
				select {
				case <-ctx.Done():
					return
				case <-s.wake:
				case <-time.After(heartbeatInterval):
					// Safety net: catches slots freed by paths that don't
					// Wake (e.g. user-driven MuxJob finishing, CancelJob on
					// a never-dequeued row). Also the disk gate's recheck:
					// nothing signals space being freed, so a backlog held
					// on a full disk resumes within one heartbeat of it.
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

	// No admissions during an outage. Every one would fail its first fetch
	// and free the slot for the next, so a channel's whole backlog drained
	// into Error in the minutes the network was down. Run's connectivity
	// subscription sweeps again when it returns.
	if s.conn != nil && !s.conn.IsOnline() {
		s.log.Debug("scheduler: offline; backlog admission waits for connectivity")
		return
	}

	channels, err := s.db.QueuedChannels()
	if err != nil {
		s.log.Error("scheduler: QueuedChannels failed", "err", err)
		return
	}
	// No admissions onto a full disk either. Read when there is a backlog
	// to admit, and on every sweep while the gate is closed: an idle install
	// pays no disk query per heartbeat, and a closed gate follows the volume
	// through a stretch with nothing Queued. Read only with a backlog, a
	// close outlived its incident — the operator cancelled the backlog and
	// freed space, the alert closed, and the backlog a later scan queued at
	// 94% waited on the close from before, inside the margin, with no
	// critical reading since.
	if (len(channels) > 0 || s.diskHeld) && s.diskGateClosed() {
		return
	}
	now := time.Now()
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
		// Past the held jobs, which wait out their backoff without costing
		// the rest of the channel its turn: ask for enough rows that every
		// hold could be among them.
		ids, err := s.db.NextQueuedJobs(ch, admit+s.heldCount())
		if err != nil {
			s.log.Error("scheduler: NextQueuedJobs failed", "channel", ch, "err", err)
			continue
		}
		admitted := 0
		for _, id := range ids {
			if admitted == admit {
				break
			}
			if s.held(id, now) {
				continue
			}
			// 1. durable FIRST — this is what the M count observes; Enqueue
			//    touches no DB row, so without this write M counts 0 forever
			//    and every tick over-admits. Upcoming is what creators write
			//    for "created, awaiting processing" and ShouldProcess accepts
			//    it, so a crash between the two steps is self-healing:
			//    enqueueExistingJobs re-enqueues the row on restart.
			//    Only while the row is still Queued: one that left it since
			//    NextQueuedJobs read it (a Cancel) is not admitted and takes
			//    no slot. The next row in line gets it — in this sweep when
			//    the query returned one past it, else in the next sweep.
			//    Not a re-query or a Wake here: a write that fails for any
			//    other reason (a database that cannot write) would spin.
			if !s.updateJob(id, map[string]any{"status": database.StatusUpcoming}) {
				s.log.Info("scheduler: backlog job left Queued before its admission; not admitted", "jobID", id, "channel", ch)
				continue
			}
			admitted++
			// 2. hand to JobQueue
			s.queue.Enqueue(id, database.StatusUpcoming)
			s.log.Info("scheduler: admitted backlog job", "jobID", id, "channel", ch)
		}
	}
}
