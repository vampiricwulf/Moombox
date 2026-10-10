package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupOldTrimTempDirsSweepsConcatDirs: ConcatCopy (the worker's part
// merge) spools its list into a moombox-concat-* directory, and the startup
// sweep only knew the trim and two-pass prefixes, so a hard abort mid-merge
// left that directory in %TEMP% for good.
//
// Mutant: drop "moombox-concat-" from the sweep's prefixes.
func TestCleanupOldTrimTempDirsSweepsConcatDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	if os.TempDir() != dir {
		t.Skipf("os.TempDir() = %q, not the test's %q", os.TempDir(), dir)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"moombox-concat-1", "moombox-trim-2", "moombox-2pass-3"} {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := CleanupOldTrimTempDirs()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Errorf("removed %d of 3 stale directories", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "moombox-concat-1")); err == nil {
		t.Error("the stale concat directory survived the sweep")
	}
}
