package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// The cancel route writes Cancelled before the run has stopped, and both UIs
// offer Mux on a Cancelled row with staged media at once — while a cancelled
// Twitch capture still waits out its part muxes and its chat's shutdown
// grace. A Mux clicked in that window wrote Muxing and Finished and reclaimed
// staging, and then the run's own end (handleCancellation) wrote Cancelled
// over the Finished archive, leaving nothing staged for another Mux.

// A Mux of a job whose last run is still registered is refused as the
// conflict ErrStagingBusy is, with nothing written; once the run has exited
// the same Mux is accepted.
//
// Mutant: muxJob's IsProcessing refusal dropped — the first Mux is accepted
// and writes Muxing over the Cancelled row while the run is still unwinding.
func TestMuxJobRefusesWhileTheJobsRunIsRegistered(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	job := &database.Job{ID: "tw_unwinding", VideoID: "tw_unwinding", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusLive, OutputDirectory: t.TempDir()}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	stageMediaFor(t, stagingBase, job.ID)
	w.queue.Enqueue(job.ID, database.StatusLive)
	id, _, ok := w.queue.Dequeue(context.Background())
	if !ok || id != job.ID {
		t.Fatalf("Dequeue = %q %v", id, ok)
	}
	if cancelled, _ := w.CancelJob(job.ID); !cancelled {
		t.Fatal("CancelJob did not cancel")
	}

	log := watchStatuses(t, db, job.ID)
	err := w.MuxJob(job.ID)
	if !errors.Is(err, ErrStagingBusy) {
		t.Errorf("MuxJob while the cancelled run is still registered = %v, want ErrStagingBusy", err)
	}
	if seq := log.statuses(); len(seq) != 0 {
		t.Errorf("the refused Mux wrote %v, want nothing", seq)
	}
	if row, _ := db.GetJob(job.ID); row == nil || row.Status != database.StatusCancelled {
		t.Errorf("row = %+v, want the Cancel standing", row)
	}

	w.queue.Complete(job.ID) // the run has exited
	if err := w.MuxJob(job.ID); err != nil {
		t.Errorf("MuxJob once the run has exited = %v, want it accepted", err)
	}
	w.wg.Wait()
	if seq := log.statuses(); len(seq) == 0 || seq[0] != database.StatusMuxing {
		t.Errorf("statuses after the accepted Mux = %v, want it to start with Muxing", seq)
	}
}

// A flagged run's end settles the row its Cancel wrote, and only while the
// row still reads Cancelled: an outcome another actor wrote in the meantime —
// a Finished archive — stands. The chat settlement still lands on a row that
// does read Cancelled.
//
// Mutant: handleCancellation's write unconditional again (UpdateJobFields) —
// the Finished row reads Cancelled.
func TestACancelledRunsEndLeavesAnotherActorsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		then       map[string]any // written between the Cancel and the run's end
		wantStatus database.JobStatus
		wantChat   string
	}{
		{"a Finished archive stands", map[string]any{"status": database.StatusFinished, "chat_status": "finished"},
			database.StatusFinished, "finished"},
		{"a Cancelled row is settled", map[string]any{"chat_status": "downloading"},
			database.StatusCancelled, chatStatusIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			job := &database.Job{ID: "tw_end_outcome", VideoID: "tw_end_outcome", URL: "https://twitch.tv/streamer",
				Platform: "twitch", Status: database.StatusLive}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			w.queue.Enqueue(job.ID, database.StatusLive)
			if _, _, ok := w.queue.Dequeue(context.Background()); !ok {
				t.Fatal("Dequeue returned no job")
			}
			if cancelled, flagged := w.CancelJob(job.ID); !cancelled || !flagged {
				t.Fatalf("CancelJob = %v, %v, want the job cancelled and its run flagged", cancelled, flagged)
			}
			db.UpdateJobFields(job.ID, tc.then)

			w.handleCancellation(job)

			row, _ := db.GetJob(job.ID)
			if row == nil || row.Status != tc.wantStatus || row.ChatStatus != tc.wantChat {
				t.Errorf("row = %+v, want %s with chat_status %q", row, tc.wantStatus, tc.wantChat)
			}
		})
	}
}
