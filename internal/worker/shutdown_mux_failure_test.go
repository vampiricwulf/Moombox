package worker

import (
	"context"
	"fmt"
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

// The split shape of the same event (owner ruling RV-1). A split finalize's
// own FFmpeg runs swallow their failure: the recovery of a part with no row,
// the copy of a set-aside recording, the same-format merge. The finalize then
// finished on its context alone, which a stop leaves live for ten seconds, so
// a part mux the shutdown killed ended in a Finished row without the part,
// which the next start does not re-mux. With the worker stopping the row now
// stays Muxing, staging intact. A finalize whose work all succeeded inside
// the stop's wait still finishes, and so does one whose part mux failed on a
// running worker (the part is kept in staging for a Mux, as before).
//
// Mutant: the errFinalizeCutShortByShutdown return dropped from
// finalizeMultiSegmentJob (the first case reads Finished).
func TestASplitFinalizeCutShortByShutdownStaysMuxing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopping   bool
		unmuxed    bool // part 2 has no row, and FFmpeg cannot read its staging
		wantStatus database.JobStatus
	}{
		{"a part mux fails while stopping", true, true, database.StatusMuxing},
		{"nothing fails while stopping", true, false, database.StatusFinished},
		{"a part mux fails on a running worker", false, true, database.StatusFinished},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			// Twitch: its parts are never merged, so no probe runs.
			staging, outputDir := muxFixtureJobOn(t, w, db, "tw_split", "twitch")
			rows := 2
			if tc.unmuxed {
				rows = 1
			}
			for i := range rows {
				part := filepath.Join(outputDir, fmt.Sprintf("Show - part%d.mp4", i+1))
				if err := os.WriteFile(part, []byte("part"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := db.AddSegment(&database.Segment{JobID: "tw_split", SegmentIndex: i, Filename: filepath.Base(part), FilePath: part,
					UnixStart: 1_700_000_000 + int64(i)*600, DurationSeconds: 300}); err != nil {
					t.Fatal(err)
				}
			}
			media := filepath.Join(staging, "seg_1", "video.ts")
			if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(media, []byte("not a recording"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.stopping {
				w.stopBegun.Store(true)
			}

			if err := w.restartMux("tw_split"); err != nil {
				t.Fatalf("MuxJob: %v", err)
			}
			w.wg.Wait()

			row, _ := db.GetJob("tw_split")
			if row == nil || row.Status != tc.wantStatus || row.Error != "" {
				t.Fatalf("row = %v (error %q), want %s with no error", statusOf(row), errorOf(row), tc.wantStatus)
			}
			if segs, _ := db.GetSegments("tw_split"); len(segs) != rows {
				t.Errorf("segment rows = %d, want the %d recorded", len(segs), rows)
			}
			if tc.unmuxed {
				if _, err := os.Stat(media); err != nil {
					t.Errorf("the unmuxed part was removed from staging: %v", err)
				}
			}
		})
	}
}
