package worker

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// backlogStreak reports jobID's backlog retry count and whether the
// scheduler holds it.
func backlogStreak(w *DownloadWorker, jobID string) (int, bool) {
	w.backlogRetryMu.Lock()
	n := w.backlogRetries[jobID]
	w.backlogRetryMu.Unlock()
	return n, w.scheduler.held(jobID, time.Now())
}

// errOutOfDisk is the engine's write onto a full volume.
var errOutOfDisk = fmt.Errorf("download: %w", fmt.Errorf("%w: write chunk: %w", engine.ErrLocalWrite,
	&os.PathError{Op: "write", Path: "video.mp4", Err: diskFullErrnos[0]}))

// spendBacklogBudget runs id through the three disk-full requeues its budget
// allows, leaving it Queued and held with a count of three.
func spendBacklogBudget(t *testing.T, w *DownloadWorker, db *database.Database, id string) *database.Job {
	t.Helper()
	backlogRetryJob(t, db, id, 1, true)
	job, _ := db.GetJob(id)
	for i := 1; i <= backlogRetryLimit; i++ {
		db.UpdateJobFields(id, map[string]any{"status": database.StatusDownloading})
		if ok, err := w.requeueBacklogAfterDiskFull(job, errOutOfDisk); !ok {
			t.Fatalf("requeue %d did not happen: %v", i, err)
		}
	}
	if n, held := backlogStreak(w, id); n != backlogRetryLimit || !held {
		t.Fatalf("after the budget: count %d held %v, want %d and held", n, held, backlogRetryLimit)
	}
	return job
}

// retriedRunRequeues is the retried job's first run failing for disk space
// again: it must go back to Queued as retry 1, not give up.
func retriedRunRequeues(t *testing.T, w *DownloadWorker, db *database.Database, job *database.Job) {
	t.Helper()
	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusDownloading})
	if ok, err := w.requeueBacklogAfterDiskFull(job, errOutOfDisk); !ok {
		t.Errorf("the retried job's first disk-full failure was not requeued: %v", err)
	}
	if n, _ := backlogStreak(w, job.ID); n != 1 {
		t.Errorf("retry count = %d after the retried job's first requeue, want 1", n)
	}
}

// TestCancelWhileQueuedThenRetryGrantsAFreshBacklogBudget is the case the
// review found: a backlog VOD that ran out of disk three times waits in
// Queued for space, the operator cancels it, frees space and clicks Retry —
// and the retried job's first disk-full failure ended in Error, "gave up
// after 3 retries", since the count survived both. Nothing ends a run of a
// Queued job, so nothing reset it, and a fetch that succeeds no longer does.
// The Cancel ends the streak, count and hold both.
//
// Mutants: drop endBacklogStreak from CancelJob — the count and the hold of
// the cancelled job survive; endBacklogStreak without the unhold — the hold
// survives; without forgetBacklogRetries — the count does.
func TestCancelWhileQueuedThenRetryGrantsAFreshBacklogBudget(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	job := spendBacklogBudget(t, w, db, "streak_cancel")

	w.CancelJob(job.ID) // waiting in Queued: no run to end the streak
	if n, held := backlogStreak(w, job.ID); n != 0 || held {
		t.Errorf("after the Cancel: count %d held %v, want the streak over", n, held)
	}
	w.ReinitializeJob(job.ID)
	retriedRunRequeues(t, w, db, job)
}

// TestRetryAndResumeGrantAFreshBacklogBudget: Retry says it grants a fresh
// budget, and Resume restarts a job the operator chose to restart, but the
// count is in memory and neither cleared it. A row can reach Error with the
// streak still counted — processJob's panic recovery writes Error without
// setJobError — and the restarted job then gave up on its first failure.
//
// Mutants: drop endBacklogStreak from reinitializeNow — the Retry subtest
// gives up; drop it from ResumeJob — the Resume subtest does.
func TestRetryAndResumeGrantAFreshBacklogBudget(t *testing.T) {
	for _, verb := range []string{"retry", "resume"} {
		t.Run(verb, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			job := spendBacklogBudget(t, w, db, "streak_"+verb)
			db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusError}) // a panicked run
			if verb == "retry" {
				w.ReinitializeJob(job.ID)
			} else {
				w.ResumeJob(job.ID)
			}
			retriedRunRequeues(t, w, db, job)
		})
	}
}

// TestDeletedBacklogRowLeavesNoStreak: the job id is the video id, so a
// backlog rescan can create a deleted row again — and it inherited the old
// row's count and hold, waiting out a backoff it never earned. A deleted
// row's streak goes with it.
//
// Mutant: drop NewDownloadWorker's OnJobDeleted subscription — the new row
// is held, with the old count.
func TestDeletedBacklogRowLeavesNoStreak(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	backlogRetryJob(t, db, "streak_deleted", 1, true)
	job, _ := db.GetJob("streak_deleted")
	if ok, err := w.requeueBacklogAfterDiskFull(job, errOutOfDisk); !ok {
		t.Fatalf("requeue did not happen: %v", err)
	}
	if err := db.DeleteJob(job.ID); err != nil {
		t.Fatal(err)
	}
	ch := "UC_retry"
	addSchedJob(t, db, &ch, job.ID, database.StatusQueued, 1) // created again

	if n, held := backlogStreak(w, job.ID); n != 0 || held {
		t.Errorf("the re-created row: count %d held %v, want no streak", n, held)
	}
}
