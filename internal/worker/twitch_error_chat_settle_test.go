package worker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestNoErrorWriteLeavesATwitchChatInFlight (owner ruling R-H1): every write
// that leaves a Twitch row in Error settles the chat the run left in flight
// by the one rule a Cancel and a YouTube Error follow (stoppedChatStatus:
// pending → "", downloading → "incomplete"), and leaves a recorded verdict as
// it is. The writers are all of them — setJobError (a download or
// stream-processing failure, a VOD's, a clip's), the two panic writes
// (recordRunPanic, and Start's recover through writePanicError with the ID
// alone), a failed Mux, and the automatic mux that could not start.
//
// The verdict rows are the unconfirmed-end arm's: ExecuteTwitch records the
// capture's own outcome there (recordChatOutcome) before the failure is
// written, and the settle does not touch it.
//
// Mutant: settleErrorChat's platform gate (YouTube only, as R-E1 first named
// it) — every pending and downloading row below keeps its value.
func TestNoErrorWriteLeavesATwitchChatInFlight(t *testing.T) {
	stagingOf := func(w *DownloadWorker, id string) string {
		var base string
		w.readConfig(func(c *config.MoomboxConfig) { base = c.Paths.EffectiveStagingDir() })
		return filepath.Join(base, id)
	}
	writers := []struct {
		name string
		// status is the row's status as the writer finds it.
		status database.JobStatus
		park   database.ParkReason
		write  func(t *testing.T, w *DownloadWorker, job *database.Job)
	}{
		{"setJobError", database.StatusDownloading, database.ParkReasonNone,
			func(t *testing.T, w *DownloadWorker, job *database.Job) {
				w.setJobError(job, errors.New("download: reset"))
			}},
		{"setJobError, end unconfirmed", database.StatusDownloading, database.ParkReasonNone,
			func(t *testing.T, w *DownloadWorker, job *database.Job) {
				w.setJobError(job, markEndUnconfirmed(errors.New("download: reset")))
			}},
		{"recordRunPanic", database.StatusDownloading, database.ParkReasonNone,
			func(t *testing.T, w *DownloadWorker, job *database.Job) {
				w.recordRunPanic(job.ID, job, "boom")
			}},
		{"Start's recover", database.StatusDownloading, database.ParkReasonNone,
			func(t *testing.T, w *DownloadWorker, job *database.Job) {
				w.writePanicError(job.ID, "boom")
			}},
		{"a failed Mux", database.StatusError, database.ParkReasonNone,
			func(t *testing.T, w *DownloadWorker, job *database.Job) {
				staging := stagingOf(w, job.ID)
				if err := os.MkdirAll(staging, 0o755); err != nil {
					t.Fatal(err)
				}
				// A recording FFmpeg cannot read: the Mux starts and fails.
				if err := os.WriteFile(filepath.Join(staging, "video.ts"), []byte("not a recording"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := w.MuxJob(job.ID); err != nil {
					t.Fatalf("MuxJob = %v", err)
				}
				w.wg.Wait()
			}},
		{"an automatic mux that could not start", database.StatusError, database.ParkReasonTwitchEndUnconfirmed,
			func(t *testing.T, w *DownloadWorker, job *database.Job) {
				// Nothing staged: muxJob refuses with a fault, and autoMuxNow
				// writes its own Error.
				w.autoMuxNow(job.ID)
				w.wg.Wait()
			}},
	}
	chats := []struct {
		from, want database.ChatStatus
	}{
		{database.ChatPending, database.ChatNone},
		{database.ChatDownloading, database.ChatIncomplete},
		{database.ChatFinished, database.ChatFinished},
		{database.ChatIncomplete, database.ChatIncomplete},
		{database.ChatUnavailable, database.ChatUnavailable},
	}
	for _, wr := range writers {
		for _, kind := range []struct{ name, id, video string }{
			{"live", "tw_100", "100"},
			{"vod", "tw_v200", "v200"},
		} {
			for _, c := range chats {
				t.Run(wr.name+"/"+kind.name+"/"+string(c.from), func(t *testing.T) {
					w, db := testWorkerSetup(t)
					job := &database.Job{ID: kind.id, VideoID: kind.video, URL: "https://www.twitch.tv/x",
						Platform: "twitch", Status: wr.status, ChatStatus: c.from,
						OutputDirectory: t.TempDir()}
					if _, err := db.AddJob(job); err != nil {
						t.Fatal(err)
					}
					if wr.park != database.ParkReasonNone { // AddJob does not carry it
						db.UpdateJobFields(job.ID, map[string]any{"park_reason": wr.park})
					}
					wr.write(t, w, job)
					row, _ := db.GetJob(job.ID)
					if row.Status != database.StatusError {
						t.Fatalf("status = %s (error %q), want Error", row.Status, row.Error)
					}
					if row.ChatStatus != c.want {
						t.Errorf("chat_status = %q, want %q (error %q)", row.ChatStatus, c.want, row.Error)
					}
					if row.ChatStatus.InFlight() {
						t.Errorf("an Error row reads an in-flight chat %q", row.ChatStatus)
					}
				})
			}
		}
	}
}
