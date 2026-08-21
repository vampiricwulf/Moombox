package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// --- I7 tombstone: skipped by all three stagedSegDirs consumers --------
//
// After a Tier 4 merge commits and starts cleaning up a superseded part's
// staging dir, it writes mergeTombstoneFile into that dir BEFORE attempting
// RemoveAll. A crash mid-removal, or a locked-dir failure, then leaves the
// dir behind with real (already-merged, now-duplicate) raw media inside —
// stagedSegDirs must never surface a tombstoned dir to ANY of its three
// consumers, or that content gets resurrected: re-persisted (duplication)
// by muxUnrecordedSegments/hasUnmuxedPartsForJob, or captured as a live
// resume target by discoverResumeSegment.

func stageRawMedia(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "video_stream"), []byte("raw video data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func writeTombstone(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, mergeTombstoneFile), []byte("2026-08-20T00:00:00Z"), 0o644); err != nil {
		t.Fatalf("write tombstone: %v", err)
	}
}

// TestStagedSegDirsSkipsTombstoned is the direct unit test for the shared
// choke point: a tombstoned seg_N dir never appears in stagedSegDirs'
// output, while an ordinary sibling dir does.
func TestStagedSegDirsSkipsTombstoned(t *testing.T) {
	staging := t.TempDir()
	stageRawMedia(t, filepath.Join(staging, "seg_1"))
	stageRawMedia(t, filepath.Join(staging, "seg_2"))
	writeTombstone(t, filepath.Join(staging, "seg_2"))

	got := stagedSegDirs(staging)

	if len(got) != 1 {
		t.Fatalf("stagedSegDirs = %d entries, want 1 (seg_2 tombstoned)", len(got))
	}
	if got[0].idx != 1 {
		t.Errorf("surviving entry idx = %d, want 1", got[0].idx)
	}
}

// TestHasUnmuxedPartsForJobSkipsTombstoned: a tombstoned dir carrying real
// unmuxed-looking raw media (no segment row recorded for its index) must
// NOT make hasUnmuxedPartsForJob report true -- that would preserve (or
// worse, cause a caller to re-persist) content the merge already
// superseded.
func TestHasUnmuxedPartsForJobSkipsTombstoned(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "j_tombstoneUnmuxed"
	if _, err := db.AddJob(&database.Job{ID: jobID, VideoID: jobID, URL: "u"}); err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(dir, "staging")
	stageRawMedia(t, filepath.Join(staging, "seg_1")) // NOT recorded in DB, would normally count as unmuxed
	writeTombstone(t, filepath.Join(staging, "seg_1"))

	if hasUnmuxedPartsForJob(db, jobID, staging) {
		t.Error("hasUnmuxedPartsForJob = true for a tombstoned dir, want false -- its content is already superseded, not unmuxed")
	}

	t.Run("control: the same dir without a tombstone DOES report unmuxed", func(t *testing.T) {
		staging2 := filepath.Join(dir, "staging2")
		stageRawMedia(t, filepath.Join(staging2, "seg_1"))
		if !hasUnmuxedPartsForJob(db, jobID, staging2) {
			t.Error("hasUnmuxedPartsForJob = false without a tombstone, want true -- proves the tombstone (not something else) is what suppressed it above")
		}
	})
}

// TestDiscoverResumeSegmentSkipsTombstoned: a tombstoned seg_N dir must
// never be handed back as a resume target -- appending fresh download data
// into a dir whose content the merge already consumed would corrupt or
// duplicate that content.
func TestDiscoverResumeSegmentSkipsTombstoned(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "j_tombstoneResume"
	if _, err := db.AddJob(&database.Job{ID: jobID, VideoID: jobID, URL: "u"}); err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(dir, "staging")
	stageRawMedia(t, staging)                         // root, part 0
	stageRawMedia(t, filepath.Join(staging, "seg_1")) // highest staged part, tombstoned/superseded
	writeTombstone(t, filepath.Join(staging, "seg_1"))
	if err := db.AddSegment(&database.Segment{JobID: jobID, SegmentIndex: 0, Quality: "1080p60", Filename: "x.mp4"}); err != nil {
		t.Fatal(err)
	}

	o := &DownloadOrchestrator{db: db, logger: &discardLogger{}}
	jobCtx := &JobContext{Job: &database.Job{ID: jobID}, StagingDir: staging}

	idx, resumeDir := o.discoverResumeSegment(jobCtx)

	// seg_1 must be invisible entirely -- with it hidden, the only
	// recorded part is 0, so the next fresh index is 1, in a BRAND NEW
	// seg_1 dir the caller will MkdirAll -- NOT the tombstoned one (which
	// happens to share the same path here, but discoverResumeSegment must
	// treat it as absent, not as an existing dir to append into).
	if idx != 1 || resumeDir != filepath.Join(staging, "seg_1") {
		t.Errorf("discoverResumeSegment = (%d, %q), want (1, fresh seg_1) -- the tombstoned dir must not be reused as a live append target", idx, resumeDir)
	}
}

// TestMuxUnrecordedSegmentsSkipsTombstoned: a tombstoned dir must never be
// muxed and persisted as a "recovered" segment row -- that would
// re-introduce content already folded into the merged output.
func TestMuxUnrecordedSegmentsSkipsTombstoned(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "j_tombstoneMux"
	if _, err := db.AddJob(&database.Job{ID: jobID, VideoID: jobID, URL: "u"}); err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(dir, "staging")
	stageRawMedia(t, filepath.Join(staging, "seg_1")) // NOT recorded, would normally be "recovered" and muxed
	writeTombstone(t, filepath.Join(staging, "seg_1"))

	o := &DownloadOrchestrator{db: db, logger: &discardLogger{}}
	jobCtx := &JobContext{Job: &database.Job{ID: jobID}, StagingDir: staging}

	// Must return without ever reaching ffmpeg/ffprobe/muxSegment -- every
	// staged dir is tombstoned, so stagedSegDirs reports none, and the
	// function's own len(segDirs)==0 early return fires.
	o.muxUnrecordedSegments(context.Background(), jobCtx)

	segs, err := db.GetSegments(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 0 {
		t.Errorf("segments after muxUnrecordedSegments = %d, want 0 -- the tombstoned dir must not be resurrected as a recovered segment", len(segs))
	}
	// The tombstoned dir and its raw media must survive untouched --
	// muxUnrecordedSegments never even looked at it.
	if _, err := os.Stat(filepath.Join(staging, "seg_1", "video_stream")); err != nil {
		t.Errorf("tombstoned dir's raw media should survive untouched: %v", err)
	}
}
