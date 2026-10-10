package routes

import (
	"os"
	"testing"
	"time"
)

// TestCleanupOldImportTempRemovesAStaleSpool: a hard abort mid-import left
// the upload's spool file (up to 500 MB) in the temp directory, and nothing
// swept it.
//
// Mutant: sweep a prefix the route does not use.
func TestCleanupOldImportTempRemovesAStaleSpool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	if os.TempDir() != dir {
		t.Skipf("os.TempDir() = %q, not the test's %q", os.TempDir(), dir)
	}
	f, err := os.CreateTemp("", importTempPrefix+"*.zip") // the route's own pattern
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(f.Name(), old, old); err != nil {
		t.Fatal(err)
	}
	if removed, err := CleanupOldImportTemp(t.TempDir()); err != nil || removed != 1 {
		t.Fatalf("CleanupOldImportTemp = %d, %v; want 1, nil", removed, err)
	}
	if _, err := os.Stat(f.Name()); err == nil {
		t.Error("the stale spool file survived")
	}
}
