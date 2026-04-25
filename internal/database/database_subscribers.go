package database

// JobChange is the event payload delivered to OnJobChange subscribers.
// Combines the post-update Job snapshot with the list of columns that
// were actually written. Subscribers that need fine-grained
// notifications (e.g. WebSocket broadcasters that want to skip
// identity-only updates, TUI views that want to re-render only the
// affected cells) can use Changes; those that only care about the
// full job can ignore it.
//
// This is the foundation for the DECISIONS #21 event-based subscriber
// migration. Today only UpdateJobFields emits JobChange events; future
// work will extend AddJob/DeleteJob/AddTrim/DeleteTrim to emit
// JobAdded/JobDeleted/TrimsChanged events through a similar API,
// letting subscribers apply diffs locally instead of re-fetching the
// full list every time.
type JobChange struct {
	Job     *Job
	Changes []string // schema column names from fieldToColumn that were written
}

// JobAdded is the event payload delivered to OnJobAdded subscribers when
// AddJob successfully inserts a new row. Carries the Job pointer the
// caller passed in (post-write — CreatedAt / UpdatedAt populated).
//
// Second event type in the DECISIONS #21 lifecycle-event set, paired with
// AddJob. Coexists with OnJobsChange during migration: AddJob fires both
// so consumers can move at their own pace. Once every consumer has
// migrated, AddJob will stop firing OnJobsChange and the full-list
// dispatch on insert goes away.
type JobAdded struct {
	Job *Job
}

// jobUpdateSub pairs a subscriber ID with its callback function.
type jobUpdateSub struct {
	id uint64
	fn func(*Job)
}

// jobChangeSub pairs a subscriber ID with its JobChange callback.
type jobChangeSub struct {
	id uint64
	fn func(*JobChange)
}

// jobAddedSub pairs a subscriber ID with its JobAdded callback.
type jobAddedSub struct {
	id uint64
	fn func(*JobAdded)
}

// jobsChangeSub pairs a subscriber ID with its callback function.
type jobsChangeSub struct {
	id uint64
	fn func([]*Job)
}

// shrinkJobUpdateSubs copies a jobUpdateSub slice into a fresh smaller-capacity
// slice when cap has grown to more than 4× len (typical after many
// subscribe/unsubscribe cycles). Keeps steady-state memory reasonable.
func shrinkJobUpdateSubs(s []jobUpdateSub) []jobUpdateSub {
	if cap(s) > 4*len(s) {
		shrunk := make([]jobUpdateSub, len(s))
		copy(shrunk, s)
		return shrunk
	}
	return s
}

// shrinkJobsChangeSubs is the jobsChangeSub counterpart to shrinkJobUpdateSubs.
func shrinkJobsChangeSubs(s []jobsChangeSub) []jobsChangeSub {
	if cap(s) > 4*len(s) {
		shrunk := make([]jobsChangeSub, len(s))
		copy(shrunk, s)
		return shrunk
	}
	return s
}

// shrinkJobChangeSubs is the jobChangeSub counterpart for the
// per-update event subscribers added for DECISIONS #21.
func shrinkJobChangeSubs(s []jobChangeSub) []jobChangeSub {
	if cap(s) > 4*len(s) {
		shrunk := make([]jobChangeSub, len(s))
		copy(shrunk, s)
		return shrunk
	}
	return s
}

// shrinkJobAddedSubs is the jobAddedSub counterpart for the lifecycle
// JobAdded subscribers added for DECISIONS #21.
func shrinkJobAddedSubs(s []jobAddedSub) []jobAddedSub {
	if cap(s) > 4*len(s) {
		shrunk := make([]jobAddedSub, len(s))
		copy(shrunk, s)
		return shrunk
	}
	return s
}

// OnJobUpdate registers a callback for job update events.
// Returns an unsubscribe function that removes the callback.
func (db *Database) OnJobUpdate(fn func(*Job)) func() {
	db.subMu.Lock()
	defer db.subMu.Unlock()
	id := db.nextSubID
	db.nextSubID++
	db.onJobUpdate = append(db.onJobUpdate, jobUpdateSub{id: id, fn: fn})
	return func() {
		db.subMu.Lock()
		defer db.subMu.Unlock()
		for i, sub := range db.onJobUpdate {
			if sub.id == id {
				db.onJobUpdate = append(db.onJobUpdate[:i], db.onJobUpdate[i+1:]...)
				db.onJobUpdate = shrinkJobUpdateSubs(db.onJobUpdate)
				break
			}
		}
	}
}

// OnJobChange registers a callback for fine-grained job-update events.
// Each invocation receives a JobChange carrying both the post-write
// snapshot and the list of columns that were actually written.
// Returns an unsubscribe function that removes the callback.
//
// Coexists with OnJobUpdate (legacy, full-Job-only). Both fire on
// every UpdateJobFields call; subscribers can pick whichever shape
// fits their needs. A future migration will deprecate OnJobUpdate
// once all callers move to OnJobChange (DECISIONS #21).
func (db *Database) OnJobChange(fn func(*JobChange)) func() {
	db.subMu.Lock()
	defer db.subMu.Unlock()
	id := db.nextSubID
	db.nextSubID++
	db.onJobChange = append(db.onJobChange, jobChangeSub{id: id, fn: fn})
	return func() {
		db.subMu.Lock()
		defer db.subMu.Unlock()
		for i, sub := range db.onJobChange {
			if sub.id == id {
				db.onJobChange = append(db.onJobChange[:i], db.onJobChange[i+1:]...)
				db.onJobChange = shrinkJobChangeSubs(db.onJobChange)
				break
			}
		}
	}
}

// OnJobAdded registers a callback for job-insertion lifecycle events.
// Fires once per successful AddJob (does NOT fire when AddJob's
// INSERT OR IGNORE returns added=false because the row already
// existed). Returns an unsubscribe function that removes the
// callback.
//
// Coexists with OnJobsChange during the DECISIONS #21 migration —
// AddJob currently fires both so consumers can pick the granularity
// that fits. Subscribers that only need to know "a new job exists"
// should prefer OnJobAdded; those that maintain a sorted full-list
// view stay on OnJobsChange until further lifecycle events
// (JobDeleted, TrimsChanged) ship.
func (db *Database) OnJobAdded(fn func(*JobAdded)) func() {
	db.subMu.Lock()
	defer db.subMu.Unlock()
	id := db.nextSubID
	db.nextSubID++
	db.onJobAdded = append(db.onJobAdded, jobAddedSub{id: id, fn: fn})
	return func() {
		db.subMu.Lock()
		defer db.subMu.Unlock()
		for i, sub := range db.onJobAdded {
			if sub.id == id {
				db.onJobAdded = append(db.onJobAdded[:i], db.onJobAdded[i+1:]...)
				db.onJobAdded = shrinkJobAddedSubs(db.onJobAdded)
				break
			}
		}
	}
}

// OnJobsChange registers a callback for job add/delete events.
// Returns an unsubscribe function that removes the callback.
func (db *Database) OnJobsChange(fn func([]*Job)) func() {
	db.subMu.Lock()
	defer db.subMu.Unlock()
	id := db.nextSubID
	db.nextSubID++
	db.onJobsChange = append(db.onJobsChange, jobsChangeSub{id: id, fn: fn})
	return func() {
		db.subMu.Lock()
		defer db.subMu.Unlock()
		for i, sub := range db.onJobsChange {
			if sub.id == id {
				db.onJobsChange = append(db.onJobsChange[:i], db.onJobsChange[i+1:]...)
				db.onJobsChange = shrinkJobsChangeSubs(db.onJobsChange)
				break
			}
		}
	}
}

// notifyJobUpdate snapshots subscribers and notifies them of a single job
// update. The caller MUST NOT hold db.mu — subscribers may call back into
// the Database (db.GetJob, db.UpdateJobFields, …) and a held db.mu would
// deadlock against any RLock/Lock attempt those callbacks make. Audit
// reports/database.md C1.
//
// Fans out to BOTH the legacy OnJobUpdate(*Job) subscribers and the
// newer OnJobChange(*JobChange) subscribers. The caller passes the
// changed-columns slice so JobChange consumers can drive fine-grained
// rendering / broadcasting; OnJobUpdate consumers ignore it.
//
// A top-level recover guards the iteration itself (not just per-callback)
// so the caller of UpdateJobFields can never crash on a bad fan-out.
func (db *Database) notifyJobUpdate(job *Job, changes []string) {
	defer func() {
		if r := recover(); r != nil && db.logger != nil {
			db.logger.Error("notifyJobUpdate iteration panic", "panic", r)
		}
	}()

	db.subMu.RLock()
	updateSubs := make([]func(*Job), 0, len(db.onJobUpdate))
	for _, sub := range db.onJobUpdate {
		updateSubs = append(updateSubs, sub.fn)
	}
	changeSubs := make([]func(*JobChange), 0, len(db.onJobChange))
	for _, sub := range db.onJobChange {
		changeSubs = append(changeSubs, sub.fn)
	}
	db.subMu.RUnlock()

	for _, fn := range updateSubs {
		db.safeCallJobUpdate(fn, job)
	}
	if len(changeSubs) > 0 {
		event := &JobChange{Job: job, Changes: changes}
		for _, fn := range changeSubs {
			db.safeCallJobChange(fn, event)
		}
	}
}

// snapshotJobsChange returns the full job list when at least one
// OnJobsChange subscriber is registered, otherwise nil. The caller MUST
// hold db.mu (Lock or RLock) — the snapshot uses getAllJobsUnlocked so
// it must run under the existing critical section to capture state at
// time-of-write. The returned slice is then handed to dispatchJobsChange
// AFTER the caller releases db.mu, closing the C2 deadlock window where
// a subscriber called back into Database.
//
// Returns nil when no subscribers are registered so callers can skip the
// dispatch step (also saves the SELECT cost when no one's listening).
// Audit reports/database.md C2.
func (db *Database) snapshotJobsChange() []*Job {
	db.subMu.RLock()
	n := len(db.onJobsChange)
	db.subMu.RUnlock()
	if n == 0 {
		return nil
	}
	jobs, err := db.getAllJobsUnlocked()
	if err != nil {
		if db.logger != nil {
			db.logger.Error("snapshotJobsChange: failed to read jobs", "err", err)
		}
		return nil
	}
	return jobs
}

// dispatchJobsChange fans out the OnJobsChange callbacks. Caller MUST
// NOT hold db.mu — callbacks may acquire other locks or call back into
// the Database. A nil jobs slice (no subscribers) is a no-op.
//
// Per-callback invocations run sequentially in a fresh goroutine so the
// caller's write path returns immediately. A top-level recover guards
// the goroutine itself; safeCallJobsChange recovers per-callback panics.
func (db *Database) dispatchJobsChange(jobs []*Job) {
	if jobs == nil {
		return
	}

	db.subMu.RLock()
	subs := make([]func([]*Job), 0, len(db.onJobsChange))
	for _, sub := range db.onJobsChange {
		subs = append(subs, sub.fn)
	}
	db.subMu.RUnlock()

	if len(subs) == 0 {
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil && db.logger != nil {
				db.logger.Error("dispatchJobsChange goroutine panic", "panic", r)
			}
		}()
		for _, fn := range subs {
			db.safeCallJobsChange(fn, jobs)
		}
	}()
}

// safeCallJobUpdate calls a subscriber callback, recovering from panics so
// one misbehaving subscriber can't prevent others from being notified.
func (db *Database) safeCallJobUpdate(fn func(*Job), job *Job) {
	defer func() {
		if r := recover(); r != nil && db.logger != nil {
			db.logger.Error("database subscriber panic on job update", "jobID", job.ID, "panic", r)
		}
	}()
	fn(job)
}

// safeCallJobsChange calls a jobs-change subscriber callback with panic recovery.
func (db *Database) safeCallJobsChange(fn func([]*Job), jobs []*Job) {
	defer func() {
		if r := recover(); r != nil && db.logger != nil {
			db.logger.Error("database subscriber panic on jobs change", "panic", r)
		}
	}()
	fn(jobs)
}

// safeCallJobChange calls an OnJobChange subscriber with panic recovery.
// One misbehaving consumer can't take down the rest of the fan-out.
func (db *Database) safeCallJobChange(fn func(*JobChange), event *JobChange) {
	defer func() {
		if r := recover(); r != nil && db.logger != nil {
			db.logger.Error("database subscriber panic on job change", "jobID", event.Job.ID, "panic", r)
		}
	}()
	fn(event)
}

// safeCallJobAdded calls an OnJobAdded subscriber with panic recovery.
// One misbehaving consumer can't take down the rest of the fan-out.
func (db *Database) safeCallJobAdded(fn func(*JobAdded), event *JobAdded) {
	defer func() {
		if r := recover(); r != nil && db.logger != nil {
			db.logger.Error("database subscriber panic on job added", "jobID", event.Job.ID, "panic", r)
		}
	}()
	fn(event)
}

// notifyJobAdded fans out the OnJobAdded callbacks. The caller MUST NOT
// hold db.mu — subscribers may call back into the Database (e.g. GetJob,
// UpdateJobFields, AddJobLog) and a held db.mu would deadlock against
// any Lock/RLock attempt.
//
// Per-callback invocations run sequentially in the caller's goroutine —
// AddJob is comparatively rare (channel discovery / manual add) so the
// goroutine spawn used by dispatchJobsChange isn't worth the bookkeeping
// here. A top-level recover guards the iteration itself; safeCallJobAdded
// recovers per-callback panics so one misbehaving consumer can't break
// the rest of the fan-out.
func (db *Database) notifyJobAdded(job *Job) {
	defer func() {
		if r := recover(); r != nil && db.logger != nil {
			db.logger.Error("notifyJobAdded iteration panic", "panic", r)
		}
	}()

	db.subMu.RLock()
	if len(db.onJobAdded) == 0 {
		db.subMu.RUnlock()
		return
	}
	subs := make([]func(*JobAdded), 0, len(db.onJobAdded))
	for _, sub := range db.onJobAdded {
		subs = append(subs, sub.fn)
	}
	db.subMu.RUnlock()

	event := &JobAdded{Job: job}
	for _, fn := range subs {
		db.safeCallJobAdded(fn, event)
	}
}
