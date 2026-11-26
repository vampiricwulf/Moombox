package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/twitch"
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
// Mutants: the errFinalizeCutShortByShutdown return dropped from
// finalizeMultiSegmentJob (the first case reads Finished); the merge's report
// to the finalize dropped from mergeSameFormatParts (the test below reads
// Finished in both stopping cases).
func TestASplitFinalizeCutShortByShutdownStaysMuxing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopping   bool
		unmuxed    bool // part 2 has no row, and FFmpeg cannot read its staging
		aside      bool // a set-aside recording FFmpeg cannot read
		wantStatus database.JobStatus
	}{
		{"a part mux fails while stopping", true, true, false, database.StatusMuxing},
		{"a set-aside recording's copy fails while stopping", true, false, true, database.StatusMuxing},
		{"nothing fails while stopping", true, false, false, database.StatusFinished},
		{"a part mux fails on a running worker", false, true, false, database.StatusFinished},
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
			if tc.aside {
				aside := filepath.Join(staging, "video.ts"+engine.StagedRestartSuffix+"1700000000")
				if err := os.WriteFile(aside, []byte("not a recording"), 0o644); err != nil {
					t.Fatal(err)
				}
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

// The merge leg of the same ruling (RV-1: a killed merge counts). A YouTube
// split finalize probes its recorded parts and concat-copies the ones that
// match. Both are child processes a tree-wide stop signal kills, and the
// merge returns its input untouched either way, so the finalize finished
// with the parts unmerged and reclaimed staging. With the worker stopping
// the row stays Muxing with both parts and both rows, and the next start
// runs the merge again. On a running worker the same failure still finishes:
// the merge is an improvement, never a gate.
//
// Mutants: the pm.cutShort report dropped from mergeSameFormatParts (both
// stopping cases read Finished); cutShort not set at the probe's error
// return (the probe case reads Finished).
func TestAMergeCutShortByShutdownStaysMuxing(t *testing.T) {
	same := &streamParams{VCodec: "h264", Width: 64, Height: 64, FrameRate: "5/1"}
	for _, tc := range []struct {
		name       string
		stopping   bool
		probe      func(context.Context, string, string) (*streamParams, error)
		wantStatus database.JobStatus
	}{
		// The parts are not recordings, so the real concat fails on them.
		{"the concat fails while stopping", true,
			func(context.Context, string, string) (*streamParams, error) { return same, nil }, database.StatusMuxing},
		{"a probe fails while stopping", true,
			func(context.Context, string, string) (*streamParams, error) {
				return nil, errors.New("signal: terminated")
			}, database.StatusMuxing},
		{"the concat fails on a running worker", false,
			func(context.Context, string, string) (*streamParams, error) { return same, nil }, database.StatusFinished},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := probeStreamParamsFn
			t.Cleanup(func() { probeStreamParamsFn = orig })
			probeStreamParamsFn = tc.probe

			w, db := testWorkerSetup(t)
			staging, outputDir := muxFixtureJobOn(t, w, db, "yt_split", "youtube")
			var parts []string
			for i := range 2 {
				part := filepath.Join(outputDir, fmt.Sprintf("Show - part%d.mp4", i+1))
				if err := os.WriteFile(part, []byte("part"), 0o644); err != nil {
					t.Fatal(err)
				}
				parts = append(parts, part)
				if err := db.AddSegment(&database.Segment{JobID: "yt_split", SegmentIndex: i, Filename: filepath.Base(part), FilePath: part,
					UnixStart: 1_700_000_000 + int64(i)*600, DurationSeconds: 300}); err != nil {
					t.Fatal(err)
				}
			}
			// Part 2's staged media, already muxed: what the Mux action
			// finds in staging to act on.
			media := filepath.Join(staging, "seg_1", "video.ts")
			if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(media, []byte("not a recording"), 0o644); err != nil {
				t.Fatal(err)
			}
			// A staged thumbnail, so the finalize that finishes fetches none
			// from YouTube.
			if err := os.WriteFile(filepath.Join(staging, "thumbnail.jpg"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.stopping {
				w.stopBegun.Store(true)
			}

			if err := w.restartMux("yt_split"); err != nil {
				t.Fatalf("MuxJob: %v", err)
			}
			w.wg.Wait()

			row, _ := db.GetJob("yt_split")
			if row == nil || row.Status != tc.wantStatus || row.Error != "" {
				t.Fatalf("row = %v (error %q), want %s with no error", statusOf(row), errorOf(row), tc.wantStatus)
			}
			if segs, _ := db.GetSegments("yt_split"); len(segs) != 2 {
				t.Errorf("segment rows = %d, want both parts", len(segs))
			}
			for _, part := range parts {
				if b, err := os.ReadFile(part); err != nil || string(b) != "part" {
					t.Errorf("%s = %q, %v; want the part as recorded", filepath.Base(part), b, err)
				}
			}
			if tc.wantStatus == database.StatusMuxing {
				if _, err := os.Stat(media); err != nil {
					t.Errorf("staging was reclaimed under a row left Muxing: %v", err)
				}
			}
		})
	}
}

// The queue path of the same event (owner ruling RV-1: the sentinel never
// writes Error). A split job's download ends and processJob's own finalize
// meets a part FFmpeg cannot mux while the worker is stopping, with the
// run's context still live, as it is for the ten seconds Stop waits. The
// sentinel comes back to processJob as the download's error, and processJob
// leaves the row as it is beside its cancellation: Muxing, staging kept,
// nothing announced. Read as a failed download it was written Error, a row
// the next start does not re-mux.
//
// Mutant: errFinalizeCutShortByShutdown dropped from processJob's
// cancellation guard (the row reads Error and a Job Failed is sent). The
// run's context is live at that guard: nothing here cancels it.
func TestAQueueRunsSplitFinalizeCutShortByShutdownStaysMuxing(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	fastEngine(t)
	w, db := testWorkerSetup(t)
	w.orchestrator.SetFfmpegPath(ffmpegPath)
	rec := notificationtest.New()
	w.notifier = rec
	w.orchestrator.notifier = rec
	const id = "tw_v900000001"
	outputDir := t.TempDir()
	if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "https://www.twitch.tv/videos/900000001",
		Platform: "twitch", Status: database.StatusUpcoming, IsVod: true, OutputDirectory: outputDir}); err != nil {
		t.Fatal(err)
	}

	// A split job: part 1 recorded, part 2 staged with no row and unreadable.
	part := filepath.Join(outputDir, "Show - part1.mp4")
	if err := os.WriteFile(part, []byte("part"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSegment(&database.Segment{JobID: id, SegmentIndex: 0, Filename: filepath.Base(part), FilePath: part,
		UnixStart: 1_700_000_000, DurationSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	unmuxed := filepath.Join(stagingBase, id, "seg_1", "video.ts")
	if err := os.MkdirAll(filepath.Dir(unmuxed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unmuxed, []byte("not a recording"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The download itself ends cleanly: a complete two-segment VOD.
	ts := oneSecondTS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			_, _ = io.WriteString(rw, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n"+
				"#EXTINF:1.000,\nseg0.ts\n#EXTINF:1.000,\nseg1.ts\n#EXT-X-ENDLIST\n")
			return
		}
		_, _ = rw.Write(ts)
	}))
	t.Cleanup(srv.Close)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		return &StreamProcessResult{ShouldDownload: true, IsVod: true,
			TwitchVariant: &twitch.TwitchHLSVariant{URL: srv.URL + "/vod.m3u8", Name: "720p30", Width: 1280, Height: 720, FPS: 30}}, nil
	}
	w.stopBegun.Store(true)

	w.queue.Enqueue(id, database.StatusUpcoming)
	_, ctx, ok := w.queue.Dequeue(context.Background())
	if !ok {
		t.Fatal("Dequeue returned no job")
	}
	w.processJob(ctx, id)

	row, _ := db.GetJob(id)
	if row == nil || row.Status != database.StatusMuxing || row.Error != "" {
		t.Fatalf("row = %v (error %q), want Muxing with no error for the next start's re-mux", statusOf(row), errorOf(row))
	}
	if _, err := os.Stat(unmuxed); err != nil {
		t.Errorf("the unmuxed part was removed from staging: %v", err)
	}
	for _, event := range []string{"error", "cancelled", "finished"} {
		if n := len(rec.ByEvent(event)); n != 0 {
			t.Errorf("%d %q notification(s) sent for a finalize the shutdown cut short, want none", n, event)
		}
	}
}
