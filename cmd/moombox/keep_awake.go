package main

import (
	"sync"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/keepawake"
)

// keepAwake holds the host awake while a capture or a mux is running and
// downloader.keep_awake is on, and lets it sleep again as soon as none is
// (owner ruling R2-X5). Windows only: elsewhere the holder does nothing.
//
// Whether to hold is derived from the jobs' current statuses
// (jobstate.KeepsHostAwake), never counted up and down: the set of working job
// IDs follows every status write, insert and delete, is rebuilt from the
// database after a bulk change, and is checked against the database again
// while a hold is in place (reconcile), so a missed event cannot leave the PC
// awake until Moombox exits.
type keepAwake struct {
	mu      sync.Mutex
	enabled bool
	working map[string]struct{}
	held    bool
	stopped bool
	// hold asks the operating system (keepawake.Holder.Hold); allJobs reads
	// every row for a rebuild.
	hold    func(on bool) bool
	allJobs func() ([]*database.Job, error)
	logger  interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// jobChanged follows one job's status: a status write or an insert.
func (k *keepAwake) jobChanged(job *database.Job) {
	if job == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	_, was := k.working[job.ID]
	now := jobstate.KeepsHostAwake(job.Status)
	if was == now {
		return
	}
	if now {
		if k.working == nil {
			k.working = make(map[string]struct{})
		}
		k.working[job.ID] = struct{}{}
	} else {
		delete(k.working, job.ID)
	}
	k.applyLocked()
}

// jobDeleted forgets a deleted row.
func (k *keepAwake) jobDeleted(jobID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, was := k.working[jobID]; !was {
		return
	}
	delete(k.working, jobID)
	k.applyLocked()
}

// rebuild replaces the working set with what jobs says: the bulk writers'
// full list (OnJobsChange).
func (k *keepAwake) rebuild(jobs []*database.Job) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rebuildLocked(jobs)
}

func (k *keepAwake) rebuildLocked(jobs []*database.Job) {
	working := make(map[string]struct{})
	for _, job := range jobs {
		if job != nil && jobstate.KeepsHostAwake(job.Status) {
			working[job.ID] = struct{}{}
		}
	}
	k.working = working
	k.applyLocked()
}

// seed rebuilds the working set from the database: at boot, when rows a
// previous run left Downloading or Muxing are about to be resumed.
func (k *keepAwake) seed() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.readLocked()
}

// reconcile checks a hold in place against the database. It reads nothing
// while nothing is held: a hold that is missing is corrected by the next
// status write, and costs nothing meanwhile.
func (k *keepAwake) reconcile() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.held {
		return
	}
	k.readLocked()
}

// readLocked reads under mu, so a status write that lands meanwhile is applied
// after the rebuild rather than lost under it (the database notifies outside
// its own lock).
func (k *keepAwake) readLocked() {
	if k.allJobs == nil {
		return
	}
	jobs, err := k.allJobs()
	if err != nil {
		k.logger.Warn("keep-awake: could not read the jobs", "error", err)
		return
	}
	k.rebuildLocked(jobs)
}

// setEnabled applies downloader.keep_awake: at boot and on every save.
func (k *keepAwake) setEnabled(on bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.enabled = on
	k.applyLocked()
}

// stop lets the host sleep again for good: the shutdown's.
func (k *keepAwake) stop() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopped = true
	k.applyLocked()
}

// applyLocked makes the hold match what is wanted.
func (k *keepAwake) applyLocked() {
	want := k.enabled && !k.stopped && len(k.working) > 0
	if want == k.held {
		return
	}
	if !k.hold(want) {
		if want && keepawake.Supported() {
			k.logger.Warn("keep-awake: Windows refused the request; the PC may sleep during this capture")
		}
		k.held = false
		return
	}
	k.held = want
	if want {
		k.logger.Debug("keep-awake: a capture or mux is running, asking Windows not to sleep", "jobs", len(k.working))
	} else {
		k.logger.Debug("keep-awake: nothing is capturing or muxing, Windows may sleep again")
	}
}

// startKeepAwake builds the controller, subscribes it to the job events and
// seeds it. The subscriptions live as long as the database does.
func (s *runState) startKeepAwake(enabled bool) {
	holder := keepawake.New()
	k := &keepAwake{
		enabled: enabled,
		hold:    holder.Hold,
		allJobs: s.db.GetAllJobs,
		logger:  s.log,
	}
	s.keepAwake = k
	s.db.OnJobChange(func(ev *database.JobChange) {
		if database.IsProgressOnlyChange(ev.Changes) {
			return // a progress tick moves no status
		}
		k.jobChanged(ev.Job)
	})
	s.db.OnJobAdded(func(ev *database.JobAdded) { k.jobChanged(ev.Job) })
	s.db.OnJobDeleted(func(ev *database.JobDeleted) { k.jobDeleted(ev.JobID) })
	s.db.OnJobsChange(k.rebuild)
	k.seed()
}

// applyKeepAwake re-applies downloader.keep_awake: the config PUT's diff and
// the TUI save.
func (s *runState) applyKeepAwake(on bool) {
	if s.keepAwake != nil {
		s.keepAwake.setEnabled(on)
	}
}
