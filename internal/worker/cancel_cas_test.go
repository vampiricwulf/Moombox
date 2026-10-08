package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestSchedulerAdmissionLeavesACancelStanding is Q4's race: sweep read the
// row Queued, the operator cancelled it, and the admission's write then
// turned the Cancelled row back into Upcoming and enqueued it — the job the
// operator had just cancelled downloaded after all. The admission is a
// compare-and-set on Queued: the cancelled row is not admitted and takes no
// slot, so the next row the query returned gets the channel's turn. (A held
// job widens the query by one, which is what puts a next row in this sweep's
// hands; without one, the next sweep admits it.)
//
// Mutants: write the admission with UpdateJobFields (newScheduler's closure)
// — the Cancelled row is admitted; ignore updateJob's answer in sweep — the
// Cancelled row is enqueued; count the admission before the write — the slot
// is spent on the row that was not admitted, and the next is not.
func TestSchedulerAdmissionLeavesACancelStanding(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 1)
	ch := "UC_cas"
	addSchedJob(t, db, &ch, "cas_new", database.StatusQueued, 1)
	addFeedItemRow(t, db, ch, "cas_new", "2026-07-12T00:00:00Z")
	addSchedJob(t, db, &ch, "cas_old", database.StatusQueued, 1)
	addFeedItemRow(t, db, ch, "cas_old", "2026-07-11T00:00:00Z")
	addSchedJob(t, db, &ch, "cas_held", database.StatusQueued, 1)
	addFeedItemRow(t, db, ch, "cas_held", "2026-07-10T00:00:00Z")
	s.holdUntil("cas_held", time.Now().Add(time.Hour))

	write := s.updateJob
	s.updateJob = func(jobID string, fields map[string]any) bool {
		if jobID == "cas_new" {
			// The Cancel lands between NextQueuedJobs and this write.
			db.UpdateJobFields(jobID, map[string]any{"status": database.StatusCancelled})
		}
		return write(jobID, fields)
	}
	s.sweep()

	if row, _ := db.GetJob("cas_new"); row.Status != database.StatusCancelled {
		t.Errorf("cancelled row's status = %s after the sweep, want Cancelled", row.Status)
	}
	if got := log.admitted(); len(got) != 1 || got[0] != "cas_old" {
		t.Errorf("admitted %v, want only the next row in line", got)
	}
}

// TestSetJobErrorLeavesACancelStanding is the same race on the worker's error
// path: processJob read its context before setJobError's write, a Cancel
// landed between them, and the write turned Cancelled into Error, sent Job
// Failed, and never sent the Job Cancelled the cancel route had left to this
// run. The Cancel stands and the run ends as a cancelled one.
//
// Mutants: write the failure with UpdateJobFields — the row ends in Error;
// return without handleCancellation when the write does not apply — the
// Job Cancelled the route left to this run is never sent.
func TestSetJobErrorLeavesACancelStanding(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec
	job := &database.Job{
		ID: "err_cancel", VideoID: "ec", URL: "u", Platform: "youtube", Title: "Cancel Me",
		ChannelName: "Chan", Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	// A processing run, so the cancel route flags it and leaves the
	// notification to the run.
	w.queue.Enqueue(job.ID, database.StatusDownloading)
	if _, _, ok := w.queue.Dequeue(context.Background()); !ok {
		t.Fatal("Dequeue returned no job")
	}
	if !w.CancelJob(job.ID) {
		t.Fatal("CancelJob did not flag the run")
	}

	// The failure the run was already returning when the cancel landed.
	w.setJobError(job, errors.New("download: connection reset by peer"))

	row, _ := db.GetJob(job.ID)
	if row.Status != database.StatusCancelled || row.Error != "" {
		t.Errorf("row = %s %q, want the Cancel standing", row.Status, row.Error)
	}
	if n := len(rec.ByEvent("error")); n != 0 {
		t.Errorf("sent %d Job Failed for a cancelled job", n)
	}
	if n := len(rec.ByEvent("cancelled")); n != 1 {
		t.Errorf("sent %d Job Cancelled, want the 1 the cancel route left to the run", n)
	}
}

// TestBacklogRequeueLeavesACancelStanding: a backlog VOD's requeue after a
// transient failure is the same last-writer-wins write, and over a Cancel it
// was worse than Error — the scheduler admitted the job again. The Cancel
// stands, and the hold placed for the requeue that did not happen is gone.
//
// Mutants: write the requeue with UpdateJobFields — the row is back in
// Queued; drop the unhold — a hold outlives the requeue it was placed for.
func TestBacklogRequeueLeavesACancelStanding(t *testing.T) {
	w, db := testWorkerSetup(t)
	backlogRetryJob(t, db, "rq_cancel", 1, true)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		// The Cancel lands as the fetch fails.
		db.UpdateJobFields("rq_cancel", map[string]any{"status": database.StatusCancelled})
		return nil, dialRefused
	}
	w.processJob(context.Background(), "rq_cancel")

	if row, _ := db.GetJob("rq_cancel"); row.Status != database.StatusCancelled {
		t.Errorf("status = %s (%q), want the Cancel standing", row.Status, row.Error)
	}
	if w.scheduler.held("rq_cancel", time.Now()) {
		t.Error("the cancelled job is still held for a requeue that did not happen")
	}
}
