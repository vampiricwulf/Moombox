package utils

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// useTempDir points os.TempDir at a fresh directory for the test.
func useTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir) // Unix
	t.Setenv("TMP", dir)    // Windows
	t.Setenv("TEMP", dir)
	if os.TempDir() != dir {
		t.Skipf("os.TempDir() = %q, not the test's %q", os.TempDir(), dir)
	}
	return dir
}

// TestRemoveStaleTempEntries: old entries with a listed prefix go, files and
// directories alike; fresh ones and other names stay.
func TestRemoveStaleTempEntries(t *testing.T) {
	dir := useTempDir(t)
	old := time.Now().Add(-48 * time.Hour)
	mk := func(name string, isDir bool, mtime time.Time) {
		t.Helper()
		p := filepath.Join(dir, name)
		var err error
		if isDir {
			err = os.Mkdir(p, 0o700)
			if err == nil {
				err = os.WriteFile(filepath.Join(p, "concat.txt"), []byte("x"), 0o600)
			}
		} else {
			err = os.WriteFile(p, []byte("x"), 0o600)
		}
		if err == nil {
			err = os.Chtimes(p, mtime, mtime)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	mk("moombox-concat-1", true, old)
	mk("moombox-import-2.zip", false, old)
	mk("moombox-concat-fresh", true, time.Now())
	mk("someone-else-3", true, old)

	removed, err := RemoveStaleTempEntries(24*time.Hour, "moombox-concat-", "moombox-import-")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}
	for name, want := range map[string]bool{
		"moombox-concat-1":     false,
		"moombox-import-2.zip": false,
		"moombox-concat-fresh": true,
		"someone-else-3":       true,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		if exists := err == nil; exists != want {
			t.Errorf("%s exists = %v, want %v", name, exists, want)
		}
	}
}
