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
// IDs follows every status write, insert and delete, is re-read from the
// database after a bulk change, and is checked against the database again on
// the stats tick while the setting is on (reconcile), so an event that was
// missed or arrived out of order can neither leave the PC awake until Moombox
// exits nor let it sleep through the rest of a capture.
type keepAwake struct {
	mu      sync.Mutex
	enabled bool
	working map[string]struct{}
	held    bool
	stopped bool
	// refused: the operating system turned down the hold that is wanted now,
	// and the warning for it has been logged. The request is made again on
	// every change and every reconcile; the warning is not repeated until a
	// hold has succeeded or none is wanted.
	refused bool
	// supported is keepawake.Supported(): where holding does nothing, seed and
	// reconcile read nothing.
	supported bool
	// hold asks the operating system (keepawake.Holder.Hold); close ends the
	// holder at shutdown (keepawake.Holder.Close; nil in a test that has
	// none); allJobs reads every row for a rebuild.
	hold    func(on bool) bool
	close   func()
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
// previous run left Downloading or Muxing are about to be resumed, and after a
// bulk change. The list a bulk writer hands OnJobsChange is not used: it was
// read at that writer's commit and is delivered later on its own goroutine, so
// a job that went Downloading in between would be dropped by it and the hold
// released for the rest of the capture. Where holding does nothing (Linux)
// it reads nothing. With the setting off it still reads, because turning the
// setting on takes the hold from the working set as it stands.
func (k *keepAwake) seed() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.supported {
		return
	}
	k.readLocked()
}

// jobsChanged is the bulk writers' event. Its list is ignored: see seed.
func (k *keepAwake) jobsChanged([]*database.Job) { k.seed() }

// reconcile checks the hold against the database, in both directions: a hold
// whose job ended unreported is dropped, and one that is missing is taken. The
// database notifies outside its lock, so two writes to one job can be
// delivered out of order, and a progress tick moves no status: without this a
// lost hold would stay lost until the capture ended. It reads nothing while
// the setting is off, after stop, or where holding does nothing.
func (k *keepAwake) reconcile() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.held && (!k.enabled || k.stopped || !k.supported) {
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

// stop lets the host sleep again for good and ends the holder's thread: the
// shutdown's. Nothing is asked of the holder after it.
func (k *keepAwake) stop() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopped = true
	k.applyLocked()
	if k.close != nil {
		k.close()
	}
}

// applyLocked makes the hold match what is wanted.
func (k *keepAwake) applyLocked() {
	want := k.enabled && !k.stopped && len(k.working) > 0
	if !want {
		k.refused = false
	}
	if want == k.held {
		return
	}
	if !k.hold(want) {
		// Asked again at the next change or reconcile; warned about once.
		if want && k.supported && !k.refused {
			k.refused = true
			k.logger.Warn("keep-awake: Windows refused the request; the PC may sleep during this capture")
		}
		k.held = false
		return
	}
	k.held = want
	k.refused = false
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
		enabled:   enabled,
		supported: keepawake.Supported(),
		hold:      holder.Hold,
		close:     holder.Close,
		allJobs:   s.db.GetAllJobs,
		logger:    s.log,
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
	s.db.OnJobsChange(k.jobsChanged)
	k.seed()
}

// applyKeepAwake re-applies downloader.keep_awake: the config PUT's diff and
// the TUI save.
func (s *runState) applyKeepAwake(on bool) {
	if s.keepAwake != nil {
		s.keepAwake.setEnabled(on)
	}
}
