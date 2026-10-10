package worker

import (
	"os"
	"path/filepath"
	"testing"
)

// A seg_N dir a Tier 4 merge tombstoned holds media already inside the merged
// part; it lingers only when its RemoveAll failed or never ran. Every mux
// path skips it (stagedSegDirs), but the visibility probe counted it, so a
// finished job kept offering Mux and muxOnRestart kept re-finalizing it.
//
// Mutant: HasSegmentFiles walking every seg_ dir again — the tombstoned-only
// staging reads as recoverable.
func TestHasSegmentFilesSkipsTombstonedParts(t *testing.T) {
	base := t.TempDir()
	seg := filepath.Join(base, "j", "seg_1")
	if err := os.MkdirAll(seg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seg, "video_stream"), []byte("merged already"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasSegmentFiles(base, "j") {
		t.Fatal("a live seg_1 with media must count")
	}
	if err := os.WriteFile(filepath.Join(seg, mergeTombstoneFile), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HasSegmentFiles(base, "j") {
		t.Error("a tombstoned seg_1 counted as recoverable media")
	}
}
