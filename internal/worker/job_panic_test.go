package worker

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
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
