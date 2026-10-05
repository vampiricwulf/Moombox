package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// mergeFixture is a two-part run ready to merge: the part files in out and a
// seg_0/seg_1 staging pair beside them.
func mergeFixture(t *testing.T) (pm *partMerger, stagingDir string, segments []database.Segment) {
	t.Helper()
	outDir, stagingDir := t.TempDir(), t.TempDir()
	for i, name := range []string{"base - part1.mp4", "base - part2.mp4"} {
		if err := os.WriteFile(filepath.Join(outDir, name), []byte("orig"), 0o644); err != nil {
			t.Fatal(err)
		}
		seg := filepath.Join(stagingDir, "seg_"+itoa(i))
		if err := os.MkdirAll(seg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seg, "video_stream"), []byte("raw"), 0o644); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, database.Segment{SegmentIndex: i, Filename: name, FilePath: filepath.Join(outDir, name), DurationSeconds: 100})
	}
	pm = newTestPartMerger(t)
	pm.probe = matchingParams
	pm.concat = writeConcatStub([]byte("merged"))
	pm.updateFile = func(int, string, string, string) error { return nil }
	return pm, stagingDir, segments
}

// The commit deletes the superseded parts' rows; their seg_N dirs used to be
// tombstoned only after it, in the cleanup. A crash in between left row-less,
// untombstoned media that the next finalize re-muxed as a new part and merged
// AGAIN after the file that already held it. The tombstone now lands before
// the commit — and is taken back if the commit fails.
//
// Mutant: dropping writeSupersededTombstones, or its take-back on failure.
func TestMergeTombstonesBeforeItCommits(t *testing.T) {
	pm, stagingDir, segments := mergeFixture(t)
	marker := filepath.Join(stagingDir, "seg_1", mergeTombstoneFile)
	seenAtCommit := false
	pm.replace = func(string, []database.Segment) error {
		_, err := os.Stat(marker)
		seenAtCommit = err == nil
		return nil
	}
	pm.merge(context.Background(), "job", stagingDir, segments)
	if !seenAtCommit {
		t.Error("the superseded part's seg_1 was not tombstoned when the commit ran")
	}

	pm, stagingDir, segments = mergeFixture(t)
	marker = filepath.Join(stagingDir, "seg_1", mergeTombstoneFile)
	pm.replace = func(string, []database.Segment) error { return errors.New("db locked") }
	pm.merge(context.Background(), "job", stagingDir, segments)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a failed commit left the still-recorded part's dir tombstoned (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, "seg_1", "video_stream")); err != nil {
		t.Errorf("a failed commit touched the superseded part's staging: %v", err)
	}
}

// A recording the engine set aside in a superseded part's dir is not merged
// content: the merge's cleanup RemoveAll'd the dir before the finalize's
// aside recovery ran, and a tombstoned dir was invisible to the aside scan
// and every shield built on it. The dir is kept, and the scan sees it.
//
// Mutant: the cleanup removing a dir that holds an aside, or
// stagedAsideRecordings skipping tombstoned dirs.
func TestMergeKeepsASupersededDirHoldingAnAside(t *testing.T) {
	pm, stagingDir, segments := mergeFixture(t)
	aside := filepath.Join(stagingDir, "seg_1", "video_stream"+engine.StagedRestartSuffix+"1700000000")
	if err := os.WriteFile(aside, []byte("set aside"), 0o644); err != nil {
		t.Fatal(err)
	}
	pm.replace = func(string, []database.Segment) error { return nil }
	pm.merge(context.Background(), "job", stagingDir, segments)

	if _, err := os.Stat(aside); err != nil {
		t.Fatalf("the merge cleanup deleted a set-aside recording: %v", err)
	}
	found := false
	for _, a := range stagedAsideRecordings(stagingDir) {
		found = found || a == aside
	}
	if !found {
		t.Error("an aside in a tombstoned dir is invisible to stagedAsideRecordings")
	}
}
