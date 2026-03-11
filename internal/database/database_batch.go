package database

import "time"

// batchUpdateLoop coalesces rapid job updates into batched writes.
// Signal-driven: sleeps until work arrives, then coalesces for 100ms before flushing.
// Zero IO when idle.
func (db *Database) batchUpdateLoop() {
	defer func() {
		if r := recover(); r != nil {
			if db.logger != nil {
				db.logger.Error("batchUpdateLoop panic (closing batchDone)", "panic", r)
			}
			close(db.batchDone)
		}
	}()

	const coalesceDelay = 100 * time.Millisecond
	pending := make(map[string]*Job)
	var coalesceTimer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case job, ok := <-db.updateCh:
			if !ok {
				// Channel closed, flush remaining
				if coalesceTimer != nil {
					coalesceTimer.Stop()
				}
				db.flushUpdates(pending)
				close(db.batchDone)
				return
			}
			pending[job.ID] = job
			// Start coalesce timer on first item; subsequent items just accumulate
			if coalesceTimer == nil {
				coalesceTimer = time.NewTimer(coalesceDelay)
				timerC = coalesceTimer.C
			}

		case <-timerC:
			db.flushUpdates(pending)
			pending = make(map[string]*Job)
			coalesceTimer = nil
			timerC = nil
		}
	}
}

func (db *Database) flushUpdates(pending map[string]*Job) {
	if len(pending) == 0 {
		return
	}

	tx, err := db.db.BeginTx(db.getCtx(), nil)
	if err != nil {
		if db.logger != nil {
			db.logger.Error("database batch flush: failed to begin transaction", "err", err)
		}
		return
	}
	defer tx.Rollback() // no-op after successful commit, ensures cleanup on any error path

	// Track which jobs were successfully persisted
	persisted := make(map[string]*Job, len(pending))
	for id, job := range pending {
		if err := updateJobExec(db.getCtx(), tx, job); err != nil {
			if db.logger != nil {
				db.logger.Error("database batch flush: failed to update job", "jobID", id, "err", err)
			}
		} else {
			persisted[id] = job
		}
	}

	if err := tx.Commit(); err != nil {
		if db.logger != nil {
			db.logger.Error("database batch flush: commit failed", "err", err, "jobs", len(pending))
		}
		return
	}

	// Snapshot subscribers, then notify outside lock to avoid blocking
	db.subMu.RLock()
	subs := make([]func(*Job), 0, len(db.onJobUpdate))
	for _, fn := range db.onJobUpdate {
		if fn != nil {
			subs = append(subs, fn)
		}
	}
	db.subMu.RUnlock()

	for _, job := range persisted {
		for _, fn := range subs {
			db.safeCallJobUpdate(fn, job)
		}
	}
}
