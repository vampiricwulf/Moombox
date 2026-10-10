package worker

import (
	"context"
	"errors"
	"fmt"
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
// Mutant: return without handleCancellation when the failure is not
// recorded — the Job Cancelled the route left to this run is never sent.
// (The write's own guard is TestFailureOverTheRoutesCancelLeavesItToTheRoute's:
// here the flag settles it first.)
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
	if cancelled, flagged := w.CancelJob(job.ID); !cancelled || !flagged {
		t.Fatalf("CancelJob = %v, %v, want the job cancelled and its run flagged", cancelled, flagged)
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

// runningJob adds a Downloading job and dequeues it, so the queue holds a run
// for it as processJob would: a Cancel flags only a run in flight.
func runningJob(t *testing.T, w *DownloadWorker, db *database.Database, id string) *database.Job {
	t.Helper()
	job := &database.Job{
		ID: id, VideoID: id, URL: "u", Platform: "youtube", Title: "Cancel Me",
		ChannelName: "Chan", Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	w.queue.Enqueue(job.ID, database.StatusDownloading)
	if _, _, ok := w.queue.Dequeue(context.Background()); !ok {
		t.Fatal("Dequeue returned no job")
	}
	return job
}

// TestFailureBetweenCancelsTwoHalvesEndsTheRunCancelled: CancelJob flagged
// the run, then wrote Cancelled (the TUI's order, until CancelJob wrote
// first). A run already past its context check whose failure landed between
// the two found the row still Downloading, recorded Error and sent Job
// Failed; CancelJob then wrote Cancelled over it, the row kept the failure's
// text, and the Job Cancelled the TUI had left to the flagged run was never
// sent. The flag now settles it: the run ends as a cancelled one.
//
// Mutant: drop setJobError's settle — Error and Job Failed for the
// operator's Cancel, and no Job Cancelled.
func TestFailureBetweenCancelsTwoHalvesEndsTheRunCancelled(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec
	job := runningJob(t, w, db, "half_cancel")

	if !w.queue.Cancel(job.ID) { // the flag, first (the former order)
		t.Fatal("Cancel did not flag the run")
	}
	w.setJobError(job, errors.New("download: connection reset by peer"))
	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusCancelled}) // the write, second

	if row, _ := db.GetJob(job.ID); row.Status != database.StatusCancelled || row.Error != "" {
		t.Errorf("row = %s %q, want Cancelled with no failure on it", row.Status, row.Error)
	}
	if n := len(rec.ByEvent("error")); n != 0 {
		t.Errorf("sent %d Job Failed for an operator's Cancel", n)
	}
	if n := len(rec.ByEvent("cancelled")); n != 1 {
		t.Errorf("sent %d Job Cancelled, want the 1 the TUI left to the run", n)
	}
}

// TestFailureOverTheRoutesCancelLeavesItToTheRoute: CancelJob writes
// Cancelled, then flags the run (the Web's order, and now the TUI's), and the
// write alone cancels the download. A failure that reached setJobError in
// between found no flag, left the row alone and sent nothing; the flag then
// landed on the still-registered run, so the route left Job Cancelled to a
// run already past sending it, and nobody sent it. The failure settles the
// run: CancelJob answers false and the route sends it.
//
// Mutants: write the failure with UpdateJobFields — Error over the Cancel,
// and Job Failed; let Cancel flag a settled run — CancelJob answers true and
// nobody sends Job Cancelled.
func TestFailureOverTheRoutesCancelLeavesItToTheRoute(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec
	job := runningJob(t, w, db, "route_cancel")

	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusCancelled}) // CancelJob's write
	w.setJobError(job, errors.New("download: connection reset by peer"))
	if w.queue.Cancel(job.ID) { // and its flag
		t.Error("CancelJob flagged a run that had settled its outcome: the route leaves Job Cancelled to a run that will not send it")
	}

	if row, _ := db.GetJob(job.ID); row.Status != database.StatusCancelled || row.Error != "" {
		t.Errorf("row = %s %q, want the Cancel standing", row.Status, row.Error)
	}
	if n := len(rec.Calls()); n != 0 {
		t.Errorf("the run sent %d notifications, want none (the route sends Job Cancelled)", n)
	}
}

// TestRunEndedByTheRoutesWriteLeavesTheCancelToTheRoute is the same order
// reaching handleCancellation instead: CancelJob's Cancelled write stops the
// download, the run ends with no flag to read — an interruption, which
// sends nothing — and CancelJob then flagged it. Reading the flag settles the
// run, so CancelJob answers false and the route sends Job Cancelled.
//
// Mutant: WasCancelled without the settle — CancelJob answers true, and
// nobody sends it.
func TestRunEndedByTheRoutesWriteLeavesTheCancelToTheRoute(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec
	job := runningJob(t, w, db, "route_interrupt")

	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusCancelled}) // CancelJob's write
	w.handleCancellation(job)
	if w.queue.Cancel(job.ID) { // and its flag
		t.Error("CancelJob flagged a run that had already ended: nobody sends Job Cancelled")
	}
	if n := len(rec.Calls()); n != 0 {
		t.Errorf("the run sent %d notifications, want none", n)
	}
}

// TestCancelInAFailuresTailIsTheCallersToReport: setJobError parks the job in
// COOKIES? and then waits on the automatic cookie refresh, up to two minutes,
// with the run still registered — and both UIs offer Cancel on a COOKIES?
// row. That Cancel flagged the run, its caller left Job Cancelled to it, and
// the run, past reading the flag, never sent it. Settled by its failure, the
// run is not flagged: CancelJob answers false and the caller sends it.
//
// Mutants: drop setJobError's settle — the parked run is flagged; let
// Cancel flag a settled run — the same.
func TestCancelInAFailuresTailIsTheCallersToReport(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec
	job := runningJob(t, w, db, "tail_cancel")

	cancelled, flagged := false, true
	w.OnCookieRefreshNeeded = func(string) CookieRefreshOutcome {
		cancelled, flagged = w.CancelJob(job.ID) // the operator cancels the COOKIES? row
		return CookieRefreshNotRestored
	}
	w.setJobError(job, fmt.Errorf("%w: sign in to confirm", ErrCookiesRequired))

	if !cancelled {
		t.Error("CancelJob did not cancel the COOKIES? row")
	}
	if flagged {
		t.Error("CancelJob flagged a run in its failure's tail: its caller leaves Job Cancelled to a run that will not send it")
	}
	if n := len(rec.ByEvent("cancelled")); n != 0 {
		t.Errorf("the run sent %d Job Cancelled, want none (its caller sends it)", n)
	}
}

// TestBacklogRequeueBetweenCancelsTwoHalvesEndsTheRunCancelled is the TUI's
// former order against the backlog requeue: the flag landed after
// processJob's context check and before CancelJob's Cancelled write, the
// requeue found the row still in flight and wrote Queued, and the flag was
// never read — no Job Cancelled. The flag settles it: no requeue, no hold,
// and the run ends as a cancelled one through setJobError. Once requeued, the
// run is settled, and a Cancel of its Queued row is the caller's to report.
//
// Mutant: drop requeueBacklog's settle — the job is requeued over the flag,
// nobody sends Job Cancelled, and a later Cancel is flagged.
func TestBacklogRequeueBetweenCancelsTwoHalvesEndsTheRunCancelled(t *testing.T) {
	w, db := testWorkerSetup(t)
	rec := notificationtest.New()
	w.notifier = rec
	backlogRetryJob(t, db, "rq_half", 1, true)
	job, _ := db.GetJob("rq_half")
	w.queue.Enqueue(job.ID, database.StatusUpcoming)
	if _, _, ok := w.queue.Dequeue(context.Background()); !ok {
		t.Fatal("Dequeue returned no job")
	}

	w.queue.Cancel(job.ID) // the flag, first (the former order)
	requeued, err := w.requeueBacklogAfterTransientFailure(job, dialRefused)
	if !requeued {
		w.setJobError(job, err) // processJob's path
	}
	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusCancelled}) // the write, second

	if requeued {
		t.Error("requeued a backlog job the operator had cancelled")
	}
	if w.scheduler.held(job.ID, time.Now()) {
		t.Error("the cancelled job is held for a requeue that did not happen")
	}
	if n := len(rec.ByEvent("cancelled")); n != 1 {
		t.Errorf("sent %d Job Cancelled, want the 1 the TUI left to the run", n)
	}

	// A run that did requeue has settled.
	backlogRetryJob(t, db, "rq_settled", 1, true)
	job2, _ := db.GetJob("rq_settled")
	w.queue.Enqueue(job2.ID, database.StatusUpcoming)
	if _, _, ok := w.queue.Dequeue(context.Background()); !ok {
		t.Fatal("Dequeue returned no job")
	}
	if ok, _ := w.requeueBacklogAfterTransientFailure(job2, dialRefused); !ok {
		t.Fatal("the transient failure was not requeued")
	}
	if _, flagged := w.CancelJob(job2.ID); flagged {
		t.Error("CancelJob flagged a requeued run: its caller leaves Job Cancelled to a run that will not send it")
	}
}

// parkedRun adds a job (a backlog VOD with its feed_items partner when
// backlog) and dequeues it, so setJobError can park it as a run would.
func parkedRun(t *testing.T, w *DownloadWorker, db *database.Database, id string, backlog bool) *database.Job {
	t.Helper()
	ch := "UC_park"
	job := &database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Title: "T",
		ChannelName: "Chan", Status: database.StatusDownloading, ChannelID: &ch}
	if backlog {
		job.QueuePriority = 1
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	if backlog {
		addFeedItemRow(t, db, ch, id, "2026-07-10T00:00:00Z")
	}
	w.queue.Enqueue(id, database.StatusDownloading)
	if _, _, ok := w.queue.Dequeue(context.Background()); !ok {
		t.Fatal("Dequeue returned no job")
	}
	return job
}

// TestCookieRefreshResumeLeavesACancelStanding: setJobError parks the job in
// COOKIES? and then waits on the automatic cookie refresh, up to two minutes,
// while both UIs offer Cancel on the parked row. The operator cancels; the
// refresh succeeds; and the resume, written unconditionally, turned the
// Cancelled row back into Upcoming (Queued for a backlog VOD) and handed it on
// once the run exited — the job the operator had cancelled downloaded after
// all. The resume is a compare-and-set on COOKIES?, and nothing is handed on.
//
// Mutants: write the resume with UpdateJobFields — the row is resumed and
// enqueued; hand off whatever the row now reads — the Cancelled job is
// enqueued.
func TestCookieRefreshResumeLeavesACancelStanding(t *testing.T) {
	for _, backlog := range []bool{false, true} {
		t.Run(fmt.Sprintf("backlog=%v", backlog), func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			id := fmt.Sprintf("refresh_cancel_%v", backlog)
			job := parkedRun(t, w, db, id, backlog)
			w.OnCookieRefreshNeeded = func(string) CookieRefreshOutcome {
				if row, _ := db.GetJob(id); row.Status != database.StatusCookies {
					t.Errorf("status during the refresh = %s, want COOKIES?", row.Status)
				}
				w.CancelJob(id) // the operator cancels the parked row
				return CookieRefreshRestored
			}
			w.setJobError(job, fmt.Errorf("%w: sign in to confirm", ErrCookiesRequired))
			w.queue.Complete(id) // the run exits
			w.wg.Wait()          // and any hand-off it left runs

			if row, _ := db.GetJob(id); row.Status != database.StatusCancelled {
				t.Errorf("status = %s after the operator cancelled the parked job, want Cancelled", row.Status)
			}
			if backlog {
				w.scheduler.resolveSlots = func(string) int { return 1 }
				w.scheduler.sweep()
			}
			if w.queue.isPending(id) {
				t.Error("the cancelled job was enqueued for download")
			}
		})
	}
}

// TestCookieRefreshResumeHandsOnARowTheSweepResumed: the refresh's own
// re-check sets off the credential sweep, which resumes every parked row of
// the platform — this one among them — before the refresh returns, so the
// resume's compare-and-set on COOKIES? finds it Upcoming. That is the resume
// this run would have made, and it is still handed on when the run exits
// rather than left to the heartbeat.
//
// Mutant: return whenever the resume does not apply — the job waits for the
// heartbeat.
func TestCookieRefreshResumeHandsOnARowTheSweepResumed(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	job := parkedRun(t, w, db, "refresh_swept", false)
	w.OnCookieRefreshNeeded = func(string) CookieRefreshOutcome {
		db.UpdateJobFieldsIf(job.ID, database.StatusCookies, map[string]any{
			"status": database.StatusUpcoming, "error": "", "park_reason": database.ParkReasonNone,
		}) // the sweep's resume
		return CookieRefreshRestored
	}
	w.setJobError(job, fmt.Errorf("%w: sign in to confirm", ErrCookiesRequired))
	w.queue.Complete(job.ID)
	w.wg.Wait()

	if !w.queue.isPending(job.ID) {
		t.Error("the resumed job was not handed on when its parked run exited")
	}
}

// TestCancelJobLeavesAnEndedJobAlone: a Cancel is decided on a status read
// earlier, and CancelJob wrote Cancelled whatever the row held by then — a
// job that finished or failed in between became a Cancelled one. It now
// leaves an outcome as it is and does nothing else either: the run still
// registered for the row (a finished run's tail) is neither flagged nor
// stopped, and the answer says nothing was cancelled.
//
// Mutants: write with UpdateJobFields — the outcome becomes Cancelled and
// CancelJob answers cancelled; flag before the write, CancelJob's former
// order — the run is stopped and flagged for a cancel that did not happen.
func TestCancelJobLeavesAnEndedJobAlone(t *testing.T) {
	for _, st := range []database.JobStatus{database.StatusFinished, database.StatusError, database.StatusCancelled} {
		t.Run(string(st), func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			id := "ended_" + string(st)
			if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube",
				Status: database.StatusDownloading, Error: "kept"}); err != nil {
				t.Fatal(err)
			}
			w.queue.Enqueue(id, database.StatusDownloading)
			_, runCtx, ok := w.queue.Dequeue(context.Background())
			if !ok {
				t.Fatal("Dequeue returned no job")
			}
			db.UpdateJobFields(id, map[string]any{"status": st}) // the run's outcome lands

			if cancelled, flagged := w.CancelJob(id); cancelled || flagged {
				t.Errorf("CancelJob = %v, %v on a %s job, want nothing cancelled", cancelled, flagged, st)
			}
			if row, _ := db.GetJob(id); row.Status != st || row.Error != "kept" {
				t.Errorf("row = %s %q, want the %s left as it was", row.Status, row.Error, st)
			}
			if runCtx.Err() != nil {
				t.Error("the run was stopped by a cancel that did not apply")
			}
			if w.queue.WasCancelled(id) {
				t.Error("the run was flagged by a cancel that did not apply")
			}
		})
	}
}
