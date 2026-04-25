package database

// jobUpdateSub pairs a subscriber ID with its callback function.
type jobUpdateSub struct {
	id uint64
	fn func(*Job)
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
// A top-level recover guards the iteration itself (not just per-callback)
// so the caller of UpdateJobFields can never crash on a bad fan-out.
func (db *Database) notifyJobUpdate(job *Job) {
	defer func() {
		if r := recover(); r != nil && db.logger != nil {
			db.logger.Error("notifyJobUpdate iteration panic", "panic", r)
		}
	}()

	db.subMu.RLock()
	subs := make([]func(*Job), 0, len(db.onJobUpdate))
	for _, sub := range db.onJobUpdate {
		subs = append(subs, sub.fn)
	}
	db.subMu.RUnlock()

	for _, fn := range subs {
		db.safeCallJobUpdate(fn, job)
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
