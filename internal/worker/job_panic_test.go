package worker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestJobPanicLeavesAnOutcomeStanding: a panicking run is recorded as Error
// (recordRunPanic; Start's recover, until the record moved ahead of
// processJob's Complete), and the record wrote it over whatever the row held
// — an operator's Cancel that landed while the run was in flight came back as
// "internal panic", and so did a job that had already finished. The panic
// still records Error on a job with no outcome yet, and leaves one alone.
//
// Mutants: write recordRunPanic's Error with UpdateJobFields — the Cancelled
// and Finished rows turn Error; drop the write — the job with no outcome is
// left Upcoming with nothing running it.
func TestJobPanicLeavesAnOutcomeStanding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		landed database.JobStatus // what the row reaches before the panic; "" for nothing
		want   database.JobStatus
	}{
		{"operator's Cancel", database.StatusCancelled, database.StatusCancelled},
		{"finished", database.StatusFinished, database.StatusFinished},
		{"no outcome", "", database.StatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			if _, err := db.AddJob(&database.Job{ID: "panics", VideoID: "panics", URL: "u", Platform: "youtube",
				Status: database.StatusUpcoming}); err != nil {
				t.Fatal(err)
			}
			reached := make(chan struct{})
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				if tc.landed != "" {
					db.UpdateJobFields("panics", map[string]any{"status": tc.landed})
				}
				close(reached)
				panic("boom")
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			go func() {
				defer close(started)
				w.Start(ctx) // enqueues the Upcoming row and runs it
			}()
			<-reached
			cancel()
			<-started
			w.wg.Wait() // the run's goroutine, recover included, has returned

			row, _ := db.GetJob("panics")
			if row.Status != tc.want {
				t.Errorf("status = %s (%q) after the run panicked, want %s", row.Status, row.Error, tc.want)
			}
		})
	}
}

// TestAPanicAfterAFlaggedCancelEndsTheRunCancelled: the cancel route leaves
// Job Cancelled to a run its Cancel flagged (CancelJob answers flagged), and a
// run that then panicked sent nothing — processJob's deferred Complete, which
// runs before Start's recover, dropped the flag, so the recover found a run
// with nothing to say it was cancelled and the Job Cancelled was never sent.
// The panic now settles the run the way setJobError settles a failure
// (JobQueue.settle) ahead of Complete: flagged first, it ends as a cancelled
// run and sends the one Job Cancelled — unless the run's own Finished landed
// over the Cancel, which stands as Start's recover lets it stand.
//
// Mutants: drop the settle from the panic's record — no Job Cancelled;
// register the recover ahead of processJob's Complete defer, so it runs after
// it — the flag is gone and no Job Cancelled; end a flagged run cancelled
// whatever its row holds — the Finished archive turns Cancelled and Job
// Cancelled is sent for it.
func TestAPanicAfterAFlaggedCancelEndsTheRunCancelled(t *testing.T) {
	for _, tc := range []struct {
		name          string
		landed        database.JobStatus // what the run writes after the Cancel, before it panics
		want          database.JobStatus
		wantCancelled int
	}{
		{"cancelled, then panics", "", database.StatusCancelled, 1},
		{"finishes over the cancel, then panics", database.StatusFinished, database.StatusFinished, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			rec := notificationtest.New()
			w.notifier = rec
			const id = "panics-cancelled"
			if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube",
				Status: database.StatusUpcoming}); err != nil {
				t.Fatal(err)
			}
			reached, proceed := make(chan struct{}), make(chan struct{})
			w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
				close(reached)
				<-proceed
				if tc.landed != "" {
					db.UpdateJobFields(id, map[string]any{"status": tc.landed})
				}
				panic("boom")
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			go func() {
				defer close(started)
				w.Start(ctx) // enqueues the Upcoming row and runs it
			}()
			<-reached
			done := w.queue.Done(id)
			if cancelled, flagged := w.CancelJob(id); !cancelled || !flagged {
				t.Fatalf("CancelJob = (%v, %v), want the running job cancelled and its run flagged", cancelled, flagged)
			}
			close(proceed)
			<-done
			cancel()
			<-started
			w.wg.Wait() // the run's goroutine, recover included, has returned

			if row, _ := db.GetJob(id); statusOf(row) != tc.want {
				t.Errorf("status = %s (%q) after the run panicked, want %s", statusOf(row), errorOf(row), tc.want)
			}
			if n := len(rec.ByEvent("cancelled")); n != tc.wantCancelled {
				t.Errorf("sent %d Job Cancelled, want %d", n, tc.wantCancelled)
			}
		})
	}
}

// TestAPanicOverTheRoutesCancelLeavesItToTheRoute is the other order:
// CancelJob writes Cancelled and then flags the run, and a panic recorded in
// between finds no flag. The record settles the run, so the flag that lands
// next is refused — CancelJob answers false and the route sends Job
// Cancelled — where a run left unsettled was flagged, its caller left the
// notification to it, and the run, past its record, never sent it.
//
// Mutant: read the flag without settling the run in recordRunPanic — Cancel
// flags it, and nobody sends Job Cancelled.
func TestAPanicOverTheRoutesCancelLeavesItToTheRoute(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec
	job := runningJob(t, w, db, "panic_route_cancel")

	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusCancelled}) // CancelJob's write
	w.recordRunPanic(job.ID, job, "boom")
	if w.queue.Cancel(job.ID) { // and its flag
		t.Error("CancelJob flagged a run whose panic had settled it: the route leaves Job Cancelled to a run that will not send it")
	}

	if row, _ := db.GetJob(job.ID); statusOf(row) != database.StatusCancelled || row.Error != "" {
		t.Errorf("row = %s %q, want the Cancel standing", statusOf(row), errorOf(row))
	}
	if n := len(rec.Calls()); n != 0 {
		t.Errorf("the run sent %d notifications, want none (the route sends Job Cancelled)", n)
	}
}

// panicOnEvent panics on the send of one notification event, after running
// before: a stand-in for any panic at that point of the mux.
type panicOnEvent struct {
	event  string
	before func()
}

func (p panicOnEvent) Send(_, _ string, _ notifications.NotificationType, _ []notifications.Field, opts notifications.SendOptions) {
	if opts.Event != p.event {
		return
	}
	if p.before != nil {
		p.before()
	}
	panic("boom on " + p.event)
}

// TestMuxJobPanicLeavesAnOutcomeStanding is Start's guard for the off-queue
// mux — the Mux action, the boot re-mux and the automatic mux of an ended
// Twitch broadcast. Its recover wrote Error over whatever the row held: a
// mux that wrote Finished and then panicked in its tail turned the archive
// into an Error, and so did an operator's Cancel that landed on the Muxing
// row. A panic still records Error on a mux with no outcome yet.
//
// Mutants: write the panic's Error with UpdateJobFields — the Finished and
// Cancelled rows turn Error; drop the write — the row with no outcome is left
// Muxing with nothing running it.
func TestMuxJobPanicLeavesAnOutcomeStanding(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	for _, tc := range []struct {
		name   string
		event  string // the send the mux panics on
		cancel bool   // the operator's Cancel lands just before the panic
		want   database.JobStatus
	}{
		{"finished", "finished", false, database.StatusFinished},
		{"operator's Cancel", "muxing", true, database.StatusCancelled},
		{"no outcome", "muxing", false, database.StatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			const id = "mux-panics"
			p := panicOnEvent{event: tc.event}
			if tc.cancel {
				p.before = func() { db.UpdateJobFields(id, map[string]any{"status": database.StatusCancelled}) }
			}
			w.orchestrator.notifier = p
			staging, _ := muxFixtureJob(t, w, db, id)
			writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 2)

			if err := w.MuxJob(id); err != nil {
				t.Fatalf("MuxJob: %v", err)
			}
			w.wg.Wait() // the mux's goroutine, recover included, has returned

			row, _ := db.GetJob(id)
			if statusOf(row) != tc.want {
				t.Errorf("status = %s (%q) after the mux panicked on %q, want %s", statusOf(row), errorOf(row), tc.event, tc.want)
			}
		})
	}
}
