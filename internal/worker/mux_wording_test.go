package worker

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// "Muxing Starting" said "Download complete, muxing" for the operator's Mux of
// a failed or cancelled job too, whose staging may stop well short of the
// end. MuxJob reads the row before its own Muxing write: a row that was
// already Muxing is a boot re-mux of a download that completed; anything else
// muxes what was captured, and the embed says so.
//
// Mutants: sendMuxingStarting ignoring CapturedMux — the Error row's embed
// claims a complete download; MuxJob reading the row after its Muxing write,
// or not setting CapturedMux — likewise; capturedMux defaulting to false — the
// same.
func TestMuxingStartingSaysWhatTheMuxHas(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	for _, tc := range []struct {
		name   string
		status database.JobStatus
		want   string
	}{
		{"boot re-mux of a completed download", database.StatusMuxing, "Download complete, muxing: "},
		{"operator's Mux of a failed job", database.StatusError, "Muxing the captured media: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			rec := notificationtest.New()
			w.orchestrator.notifier = rec
			jobID := "j-mux-wording"
			staging, _ := muxFixtureJob(t, w, db, jobID)
			writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
			db.UpdateJobFields(jobID, map[string]any{"status": tc.status, "title": "A Stream"})

			if err := w.MuxJob(jobID); err != nil {
				t.Fatalf("MuxJob: %v", err)
			}
			w.Stop()

			got := rec.ByEvent("muxing")
			if len(got) != 1 {
				t.Fatalf("%d muxing sends, want 1: %+v", len(got), rec.Calls())
			}
			if want := tc.want + "A Stream"; got[0].Description != want {
				t.Errorf("description = %q, want %q", got[0].Description, want)
			}
		})
	}
}
