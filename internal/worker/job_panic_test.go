package worker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// TestJobPanicLeavesAnOutcomeStanding: Start's recover records a panicking
// run as Error, and wrote it over whatever the row held — an operator's
// Cancel that landed while the run was in flight came back as "internal
// panic", and so did a job that had already finished. The panic still
// records Error on a job with no outcome yet, and leaves one alone.
//
// Mutants: write the panic's Error with UpdateJobFields — the Cancelled and
// Finished rows turn Error; drop the write — the job with no outcome is left
// Upcoming with nothing running it.
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
