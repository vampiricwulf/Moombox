package database

// OnJobUpdate registers a callback for job update events.
// Returns an unsubscribe function that removes the callback.
func (db *Database) OnJobUpdate(fn func(*Job)) func() {
	db.subMu.Lock()
	defer db.subMu.Unlock()
	id := len(db.onJobUpdate)
	db.onJobUpdate = append(db.onJobUpdate, fn)
	return func() {
		db.subMu.Lock()
		defer db.subMu.Unlock()
		if id < len(db.onJobUpdate) {
			db.onJobUpdate[id] = nil
		}
		// Trim trailing nils to reclaim memory without invalidating other indices
		db.onJobUpdate = trimTrailingNilFuncs(db.onJobUpdate)
	}
}

// OnJobsChange registers a callback for job add/delete events.
// Returns an unsubscribe function that removes the callback.
func (db *Database) OnJobsChange(fn func([]*Job)) func() {
	db.subMu.Lock()
	defer db.subMu.Unlock()
	id := len(db.onJobsChange)
	db.onJobsChange = append(db.onJobsChange, fn)
	return func() {
		db.subMu.Lock()
		defer db.subMu.Unlock()
		if id < len(db.onJobsChange) {
			db.onJobsChange[id] = nil
		}
		// Trim trailing nils to reclaim memory without invalidating other indices
		db.onJobsChange = trimTrailingNilJobsFuncs(db.onJobsChange)
	}
}

// trimTrailingNilFuncs removes trailing nil entries from the subscriber slice.
// This is safe because it doesn't reorder existing entries — earlier indices remain valid.
func trimTrailingNilFuncs(fns []func(*Job)) []func(*Job) {
	i := len(fns)
	for i > 0 && fns[i-1] == nil {
		i--
	}
	return fns[:i]
}

// trimTrailingNilJobsFuncs removes trailing nil entries from the jobs-change subscriber slice.
func trimTrailingNilJobsFuncs(fns []func([]*Job)) []func([]*Job) {
	i := len(fns)
	for i > 0 && fns[i-1] == nil {
		i--
	}
	return fns[:i]
}

// notifyJobUpdate snapshots subscribers and notifies them of a single job update.
func (db *Database) notifyJobUpdate(job *Job) {
	db.subMu.RLock()
	subs := make([]func(*Job), 0, len(db.onJobUpdate))
	for _, fn := range db.onJobUpdate {
		if fn != nil {
			subs = append(subs, fn)
		}
	}
	db.subMu.RUnlock()

	for _, fn := range subs {
		db.safeCallJobUpdate(fn, job)
	}
}

// notifyJobsChange must be called while db.mu is already held (Lock or RLock).
// It uses getAllJobsUnlocked to avoid deadlock.
func (db *Database) notifyJobsChange() {
	db.subMu.RLock()
	subs := make([]func([]*Job), 0, len(db.onJobsChange))
	for _, fn := range db.onJobsChange {
		if fn != nil {
			subs = append(subs, fn)
		}
	}
	db.subMu.RUnlock()

	if len(subs) == 0 {
		return
	}

	// Use unlocked version since caller already holds db.mu
	jobs, err := db.getAllJobsUnlocked()
	if err != nil {
		if db.logger != nil {
			db.logger.Error("notifyJobsChange: failed to get jobs", "err", err)
		}
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil && db.logger != nil {
				db.logger.Error("notifyJobsChange goroutine panic", "panic", r)
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
