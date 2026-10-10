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
// Mutant: drop NewDownloadWorker's OnJobDeleted subscription — the deleted
// row's count and hold outlive it (the re-created row is cleared by the
// insert, TestInsertedRowStartsWithoutAStreak).
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
	// Gone with the row, not only once the id is inserted again: a hold left
	// behind would widen every sweep's NextQueuedJobs until then.
	if n, held := backlogStreak(w, job.ID); n != 0 || held {
		t.Errorf("the deleted row: count %d held %v, want no streak", n, held)
	}
	ch := "UC_retry"
	addSchedJob(t, db, &ch, job.ID, database.StatusQueued, 1) // created again

	if n, held := backlogStreak(w, job.ID); n != 0 || held {
		t.Errorf("the re-created row: count %d held %v, want no streak", n, held)
	}
}

// TestPrunedBacklogRowLeavesNoStreak is the same through the other way a row
// goes: a channel removal's "delete its pending jobs" choice
// (DeletePendingChannelJobs -> DeleteJobsAndHistoryForChannel) deletes the
// channel's Queued rows in bulk and fires no OnJobDeleted, only one
// OnJobsChange. A row pruned mid-backoff
// left its count and hold behind, and the re-added channel's rescan created
// the same id again, held, with the old count. A held row of a channel the
// prune does not touch keeps its streak.
//
// Mutants: drop NewDownloadWorker's OnJobsChange subscription (the pruned
// streak never ends: the wait times out); end every streak in
// endStreaksGoneFrom whether or not its row is listed (the other channel's
// row loses its count and hold); end only the counts, or only the holds
// (the wait times out on the half left behind).
func TestPrunedBacklogRowLeavesNoStreak(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	requeue := func(id string) *database.Job {
		t.Helper()
		job, _ := db.GetJob(id)
		if ok, err := w.requeueBacklogAfterDiskFull(job, errOutOfDisk); !ok {
			t.Fatalf("requeue of %s did not happen: %v", id, err)
		}
		return job
	}
	backlogRetryJob(t, db, "streak_pruned", 1, true) // channel UC_retry
	pruned := requeue("streak_pruned")
	other := "UC_other"
	addSchedJob(t, db, &other, "streak_kept", database.StatusUpcoming, 1)
	addFeedItemRow(t, db, other, "streak_kept", "2026-07-10T00:00:00Z")
	kept := requeue("streak_kept")

	n, err := db.DeleteJobsAndHistoryForChannel("UC_retry",
		[]database.JobStatus{database.StatusQueued, database.StatusUpcoming, database.StatusCookies}, nil)
	if err != nil || n != 1 {
		t.Fatalf("prune: deleted %d, err %v; want the one Queued row", n, err)
	}
	// OnJobsChange is delivered off the writer's goroutine.
	waitForCond(t, 5*time.Second, "the pruned row's count and hold to end", func() bool {
		n, held := backlogStreak(w, pruned.ID)
		return n == 0 && !held
	})
	if n, held := backlogStreak(w, kept.ID); n != 1 || !held {
		t.Errorf("the other channel's row: count %d held %v, want its streak kept (1, held)", n, held)
	}

	ch := "UC_retry"
	addSchedJob(t, db, &ch, pruned.ID, database.StatusQueued, 1) // the re-added channel's rescan
	if n, held := backlogStreak(w, pruned.ID); n != 0 || held {
		t.Errorf("the re-created row after a channel prune: count %d held %v, want no streak", n, held)
	}
}

// TestInsertedRowStartsWithoutAStreak: the prune's list reaches the worker off
// the writer's goroutine, so the rescan's AddJob can come first. A row AddJob
// inserts has had no run to earn a streak, so one its id still holds is
// stale and goes at the insert, synchronously. Seeded here as a streak whose
// row went without any event, so nothing but the insert can end it.
//
// Mutant: drop NewDownloadWorker's OnJobAdded subscription — the new row is
// held, with the old count.
func TestInsertedRowStartsWithoutAStreak(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	const id = "streak_stale"
	w.noteBacklogRetry(id)
	w.scheduler.holdUntil(id, time.Now().Add(time.Hour))

	ch := "UC_retry"
	addSchedJob(t, db, &ch, id, database.StatusQueued, 1)
	if n, held := backlogStreak(w, id); n != 0 || held {
		t.Errorf("the inserted row: count %d held %v, want no streak", n, held)
	}
}
