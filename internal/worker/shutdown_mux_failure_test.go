package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// A mux whose FFmpeg dies once Moombox's stop has begun is the shutdown, not
// a failed recording (owner ruling R3-8). systemd's default KillMode signals
// every process in the unit, FFmpeg included, while Stop still waits ten
// seconds before it cancels the mux root, so the mux's own context reads as
// live and the Mux action wrote Error: a row the next start's re-mux skips.
// The row now stays Muxing, staging intact, from the moment the stop begins:
// at signal receipt (Start's context ends) or at Stop's first line.
//
// Mutant: the shuttingDown check dropped from muxJob's failure branch (the
// row reads Error with the mux's failure).
func TestAMuxThatFailsWhileShuttingDownStaysMuxing(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(w *DownloadWorker)
	}{
		{"the stop signal ended Start's context", func(w *DownloadWorker) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			w.runCtx.Store(&ctx)
		}},
		{"Stop was entered", func(w *DownloadWorker) { w.stopBegun.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			staging, _ := muxFixtureJob(t, w, db, "j-stop")
			// A recording FFmpeg cannot read: the mux fails whatever stopped it.
			media := filepath.Join(staging, "video.mp4")
			if err := os.WriteFile(media, []byte("not a recording"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.stop(w)

			if err := w.restartMux("j-stop"); err != nil {
				t.Fatalf("MuxJob: %v", err)
			}
			w.wg.Wait()

			row, _ := db.GetJob("j-stop")
			if row == nil || row.Status != database.StatusMuxing || row.Error != "" {
				t.Fatalf("row = %v (error %q), want Muxing with no error for the next start's re-mux",
					statusOf(row), errorOf(row))
			}
			if _, err := os.Stat(media); err != nil {
				t.Errorf("staging media was removed: %v", err)
			}
		})
	}
}

// A part mux that fails once the stop has begun is logged as the shutdown,
// not as an Error: no status is written either way, and the part stays in
// staging for the finalize.
//
// Mutant: partMuxFailed's stopping check dropped (an ERROR line).
func TestAPartMuxFailingDuringShutdownIsNotAnError(t *testing.T) {
	log := &levelLogger{}
	stopping := false
	o := &DownloadOrchestrator{logger: log, shuttingDown: func() bool { return stopping }}

	o.partMuxFailed("failed to mux quality segment", "jobID", "j")
	if log.count("ERROR", "failed to mux quality segment") != 1 {
		t.Fatalf("a part mux failing on a running worker logged %v, want one ERROR line", log.lines)
	}
	stopping = true
	o.partMuxFailed("failed to mux quality segment", "jobID", "j")
	if n := log.count("ERROR", "failed to mux quality segment"); n != 1 {
		t.Errorf("a part mux failing during shutdown logged an ERROR line (%v)", log.lines)
	}
}
