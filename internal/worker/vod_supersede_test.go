package worker

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// splitJobFixture stages the job V8 is about: a live capture that
// quality-split — part 0 muxed and recorded, its raw capture still in the
// staging root; part 1 captured into seg_1 and never muxed — and was then
// interrupted. Returns the job context and part 0's output path.
func splitJobFixture(t *testing.T, w *DownloadWorker, db *database.Database, ffmpegPath, jobID string) (*JobContext, string) {
	t.Helper()
	staging, _ := muxFixtureJob(t, w, db, jobID)
	job, _ := db.GetJob(jobID)
	jobCtx := w.buildJobContext(job)

	part0 := filepath.Join(jobCtx.OutputDir, jobCtx.Filename+" - part1.mp4")
	writeMuxFixture(t, ffmpegPath, part0, 4)
	if err := db.AddSegment(&database.Segment{JobID: jobID, SegmentIndex: 0, UnixStart: 1_700_000_000, UnixEnd: 1_700_000_004,
		Quality: "720p", Filename: filepath.Base(part0), FilePath: part0, DurationSeconds: 4}); err != nil {
		t.Fatal(err)
	}
	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "video_stream"), 4)
	if err := os.WriteFile(filepath.Join(staging, "video_stream.resume.json"), []byte(`{"lastSeq":4}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staging, "seg_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "seg_1", "video_stream"), 3)
	return jobCtx, part0
}

// restartSiblingDurations returns the probed durations of the .restart-
// siblings in dir, shortest first.
func restartSiblingDurations(t *testing.T, o *DownloadOrchestrator, dir string) []int {
	t.Helper()
	var durs []int
	for _, n := range mp4sIn(t, dir) {
		if _, ok := engine.RestartSiblingStem(n); !ok {
			continue
		}
		p := o.runFFprobe(context.Background(), filepath.Join(dir, n))
		if p == nil {
			t.Fatalf("sibling %s does not probe", n)
		}
		durs = append(durs, int(p.DurationSec+0.5))
	}
	sort.Ints(durs)
	return durs
}

// TestSplitJobVodDownloadSupersedesParts is V8 end to end: a split live job
// comes back as a finished VOD, and ExecuteWithChat downloads the complete
// 12 s recording. That recording must be the archive; the two partial live
// parts (4 s recorded, 3 s never muxed) must survive beside it as siblings
// rather than be the archive or be deleted; and the staging is reclaimed.
//
// Mutants, each failing this test: drop the claimStagingRootForVod call from
// ExecuteWithChat (the download lands beside part 0's capture, no root is
// claimed, the parts finalize); drop the markVodRootComplete call (the parts
// finalize, the download is only shielded); drop the supersedePartsWithVod
// call from muxAndFinalize (the parts finalize).
func TestSplitJobVodDownloadSupersedesParts(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	jobCtx, part0 := splitJobFixture(t, w, db, ffmpegPath, "j-splitvod")

	full := filepath.Join(t.TempDir(), "full.mp4")
	writeMuxFixture(t, ffmpegPath, full, 12)
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	srv := serveWholeFile(t, body)

	o := NewDownloadOrchestrator(db, nil, ffmpegPath, discardLogger{}, nil, stubCipherSolver{}, nil, nil, nil)
	if err := o.ExecuteWithChat(context.Background(), jobCtx, wholeFileVodInfo(srv, len(body)), true, nil); err != nil {
		t.Fatalf("ExecuteWithChat: %v", err)
	}
	w.cleanupStagingAfterMux("j-splitvod", jobCtx.StagingDir)

	fresh, _ := db.GetJob("j-splitvod")
	if fresh.Status != database.StatusFinished {
		t.Fatalf("status = %s (%q), want Finished", fresh.Status, fresh.Error)
	}
	if p := o.runFFprobe(context.Background(), fresh.OutputFile); p == nil || p.DurationSec < 11 {
		t.Fatalf("archive %s probes %+v, want the complete 12 s download", fresh.OutputFile, p)
	}
	if segs, _ := db.GetSegments("j-splitvod"); len(segs) != 0 {
		t.Errorf("segment rows = %+v, want none: the job is the single archive now", segs)
	}
	if got := restartSiblingDurations(t, o, jobCtx.OutputDir); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Errorf("superseded parts beside the archive probe %v s, want [3 4]", got)
	}
	if fileExists(part0) {
		t.Errorf("part 0 is still under its part name %s", filepath.Base(part0))
	}
	if _, err := os.Stat(jobCtx.StagingDir); !os.IsNotExist(err) {
		t.Errorf("staging was kept (%v); everything in it is in the archive or beside it", err)
	}
}

// TestRestartMuxSupersedesPartsWithCompleteVod covers the other way into the
// finalize: the from-the-start download completed (root claimed and marked
// complete, part 0's capture in seg_0) and the process restarted in Muxing,
// so the restart mux (muxOnRestart → muxFromStaging) finalizes from staging
// alone. The never-muxed part 1 is muxed by the recovery and then superseded
// like part 0.
//
// Mutant: drop the supersedePartsWithVod call from muxAndFinalize — the
// archive is the parts.
func TestRestartMuxSupersedesPartsWithCompleteVod(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	jobCtx, _ := splitJobFixture(t, w, db, ffmpegPath, "j-splitmux")
	root := jobCtx.StagingDir
	o := w.orchestrator
	if err := o.claimStagingRootForVod(jobCtx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	writeMuxFixture(t, ffmpegPath, filepath.Join(root, "video.mp4"), 12)
	o.markVodRootComplete(jobCtx)

	w.enqueueExistingJobs()
	w.Stop()

	fresh, _ := db.GetJob("j-splitmux")
	if fresh.Status != database.StatusFinished {
		t.Fatalf("status = %s (%q), want Finished", fresh.Status, fresh.Error)
	}
	if p := o.runFFprobe(context.Background(), fresh.OutputFile); p == nil || p.DurationSec < 11 {
		t.Fatalf("archive %s probes %+v, want the complete 12 s download", fresh.OutputFile, p)
	}
	if got := restartSiblingDurations(t, o, jobCtx.OutputDir); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Errorf("superseded parts beside the archive probe %v s, want [3 4]", got)
	}
}

// TestClaimStagingRootMovesPartZeroIntoSegZero pins the claim's layout: part
// 0's live capture and its sidecar leave the root for seg_0 (so the root is
// no longer read as part 0), the root is marked, and a job that never split
// is left alone.
//
// Mutant: drop the move loop — part 0's capture stays in the root.
func TestClaimStagingRootMovesPartZeroIntoSegZero(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-claim")
	job, _ := db.GetJob("j-claim")
	jobCtx := w.buildJobContext(job)
	for _, n := range []string{"video_stream", "audio_stream", "video_stream.resume.json"} {
		if err := os.WriteFile(filepath.Join(staging, n), []byte("\x00\x00\x00\x18ftypdash"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Not split: nothing moves, no marker.
	if err := w.orchestrator.claimStagingRootForVod(jobCtx); err != nil {
		t.Fatal(err)
	}
	if vodRootState(staging) != "" || !fileExists(filepath.Join(staging, "video_stream")) {
		t.Fatalf("a job that never split had its root claimed")
	}

	if err := db.AddSegment(&database.Segment{JobID: "j-claim", SegmentIndex: 0, Filename: "x - part1.mp4"}); err != nil {
		t.Fatal(err)
	}
	if err := w.orchestrator.claimStagingRootForVod(jobCtx); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"video_stream", "audio_stream", "video_stream.resume.json"} {
		if fileExists(filepath.Join(staging, n)) {
			t.Errorf("%s is still in the staging root", n)
		}
		if !fileExists(filepath.Join(staging, "seg_0", n)) {
			t.Errorf("%s did not move to seg_0", n)
		}
	}
	if got := vodRootState(staging); got != vodRootDownloading {
		t.Errorf("marker = %q, want %q", got, vodRootDownloading)
	}
}

// TestSupersedePartsIsReentrant pins the two edges of the supersede: a part
// an earlier finalize renamed to the plain archive name moves out of the
// archive's way, and a part whose file already moved (a crash between the
// moves and the row delete) is skipped rather than failing the finalize.
//
// Mutants: drop the missing-file `continue` — the supersede errors on the
// already-moved part and the rows stay; skip the tombstone write — part 2's
// dir reads as an unmuxed part once its row is gone.
func TestSupersedePartsIsReentrant(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-reentry")
	job, _ := db.GetJob("j-reentry")
	jobCtx := w.buildJobContext(job)
	plain := filepath.Join(outputDir, jobCtx.Filename+".mp4")
	if err := os.WriteFile(plain, []byte("merged part"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{plain, filepath.Join(outputDir, "gone - part2.mp4")} {
		if err := db.AddSegment(&database.Segment{JobID: "j-reentry", SegmentIndex: i, UnixStart: int64(1_700_000_000 + i),
			Filename: filepath.Base(p), FilePath: p}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(staging, "seg_1"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := w.orchestrator.supersedePartsWithVod(jobCtx); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if fileExists(plain) {
		t.Errorf("the part at the plain name is still where the archive is about to be written")
	}
	if segs, _ := db.GetSegments("j-reentry"); len(segs) != 0 {
		t.Errorf("rows = %+v, want none", segs)
	}
	if !isMergeTombstoned(filepath.Join(staging, "seg_1")) {
		t.Errorf("recorded part 2's staging was not tombstoned")
	}
	var kept int
	for _, n := range mp4sIn(t, outputDir) {
		if _, ok := engine.RestartSiblingStem(n); ok {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("siblings = %d, want the one part that still had a file", kept)
	}
}

// TestSupersededRootIsNeverReadAsPartZero is W20-19: the supersede tombstones
// seg_0 with the other recorded parts, and a part dir that never got a row —
// an empty seg_2 a split created before its first segment, or one whose media
// FFmpeg cannot read — survives the finalize untombstoned. The complete VOD
// in the root must still not read as an unmuxed part 0: not to the cleanup,
// which reclaims the staging when nothing else in it needs keeping, and not
// to a Mux of the unreadable part, which must not write the whole VOD out as
// one more full-length sibling.
//
// Mutants: hasUnmuxedSegmentParts back on stagedSegDirs' first index — the
// empty case's staging is kept for an "unmuxed part 0"; muxUnrecordedSegments
// back on it — the Mux muxes the root as part 0 and the supersede moves that
// 12 s copy beside the archive.
func TestSupersededRootIsNeverReadAsPartZero(t *testing.T) {
	for _, tc := range []struct {
		name       string
		seg2       []byte // nil: an empty seg_2; else seg_2/video_stream's bytes
		wantKept   bool
		muxOffered bool
	}{
		{"empty part dir left by an interrupted split", nil, false, false},
		{"part dir whose media FFmpeg cannot read", []byte("\x00\x00\x00\x18ftypdash not really media"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ffmpegPath, _ := requireFFmpegTools(t)
			w, db := testWorkerSetup(t)
			jobCtx, _ := splitJobFixture(t, w, db, ffmpegPath, "j-tomb")
			root := jobCtx.StagingDir
			if err := os.MkdirAll(filepath.Join(root, "seg_2"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.seg2 != nil {
				if err := os.WriteFile(filepath.Join(root, "seg_2", "video_stream"), tc.seg2, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			full := filepath.Join(t.TempDir(), "full.mp4")
			writeMuxFixture(t, ffmpegPath, full, 12)
			body, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			srv := serveWholeFile(t, body)

			o := NewDownloadOrchestrator(db, nil, ffmpegPath, discardLogger{}, nil, stubCipherSolver{}, nil, nil, nil)
			if err := o.ExecuteWithChat(context.Background(), jobCtx, wholeFileVodInfo(srv, len(body)), true, nil); err != nil {
				t.Fatalf("ExecuteWithChat: %v", err)
			}
			w.cleanupStagingAfterMux("j-tomb", root)

			_, statErr := os.Stat(root)
			if kept := statErr == nil; kept != tc.wantKept {
				t.Errorf("staging kept = %v, want %v (root media %+v)", kept, tc.wantKept, discoverStagingMedia(root))
			}
			stagingBase := filepath.Dir(root)
			if got := HasUnmuxedParts(db, stagingBase, "j-tomb"); got != tc.muxOffered {
				t.Fatalf("HasUnmuxedParts = %v, want %v", got, tc.muxOffered)
			}
			if tc.muxOffered {
				if err := w.MuxJob("j-tomb"); err != nil {
					t.Fatalf("MuxJob: %v", err)
				}
				w.wg.Wait()
			}

			if got := restartSiblingDurations(t, o, jobCtx.OutputDir); len(got) != 2 || got[0] != 3 || got[1] != 4 {
				t.Errorf("siblings beside the archive probe %v s, want only the superseded parts [3 4] — no copy of the VOD", got)
			}
			fresh, _ := db.GetJob("j-tomb")
			if p := o.runFFprobe(context.Background(), fresh.OutputFile); p == nil || p.DurationSec < 11 {
				t.Errorf("archive %s probes %+v, want the complete 12 s download", fresh.OutputFile, p)
			}
		})
	}
}

// TestCleanupKeepsRootRecordingTheFinalizeDidNotUse pins the cleanup shield:
// a job that finalized as parts keeps its staging when the root holds a
// from-the-start recording — a claimed root, or the whole-file pair no part is
// ever made from — and a single-file job keeps it when the root holds a second
// recording of the other shape; otherwise the staging goes.
//
// Mutants: drop the unusedRootRecording branch from cleanupStagingAfterMux —
// the staging (and the download in it) is deleted; drop the single-file
// two-shapes arm — the last two cases' complete downloads are deleted; look
// only for video_stream / video.ts beside the whole file (W20-20) — the
// audio-only job's complete audio.m4a is deleted with staging.
func TestCleanupKeepsRootRecordingTheFinalizeDidNotUse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		split    bool
		root     []string
		claimed  bool
		wantKept bool
	}{
		{"whole-file download beside the parts", true, []string{"video.mp4"}, false, true},
		{"claimed root holding a post-live capture", true, []string{"video_stream"}, true, true},
		{"part 0's own capture", true, []string{"video_stream"}, false, false},
		{"single file, one recording", false, []string{"video.mp4", "audio.m4a"}, false, false},
		{"single file, a capture beside the download", false, []string{"video_stream", "video.mp4"}, false, true},
		{"single file, an audio capture beside the audio download", false, []string{"audio_stream", "audio.m4a"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			staging, _ := muxFixtureJob(t, w, db, "j-shield")
			db.UpdateJobFields("j-shield", map[string]any{"status": database.StatusFinished})
			if tc.split {
				if err := db.AddSegment(&database.Segment{JobID: "j-shield", SegmentIndex: 0, Filename: "x - part1.mp4"}); err != nil {
					t.Fatal(err)
				}
			}
			for _, n := range tc.root {
				if err := os.WriteFile(filepath.Join(staging, n), []byte("\x00\x00\x00\x18ftypdash"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.claimed {
				if err := os.MkdirAll(filepath.Join(staging, "seg_0"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := writeVodRootState(staging, vodRootDownloading); err != nil {
					t.Fatal(err)
				}
			}
			w.cleanupStagingAfterMux("j-shield", staging)
			_, err := os.Stat(staging)
			if kept := err == nil; kept != tc.wantKept {
				t.Errorf("staging kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}
