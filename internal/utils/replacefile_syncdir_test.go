package utils

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A replace makes the new name durable, not just the bytes: ReplaceFile
// fsyncs the destination's directory after the rename succeeds, and only
// then. Without it a power loss shortly after a replace could bring back the
// old file, or leave the database naming an archive whose rename was lost.
//
// Mutant: the syncDirectory call removed — nothing is synced.
func TestReplaceFileSyncsTheDestinationDirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "file.tmp")
	dst := filepath.Join(dir, "file")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var synced []string
	prev := syncDirectory
	t.Cleanup(func() { syncDirectory = prev })
	syncDirectory = func(d string) { synced = append(synced, d) }

	if err := ReplaceFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Errorf("synced %v, want [%s]", synced, dir)
	}

	synced = nil
	if err := ReplaceFile(filepath.Join(dir, "absent"), dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReplaceFile of a missing source = %v", err)
	}
	if len(synced) != 0 {
		t.Errorf("a failed replace synced %v", synced)
	}
}

// syncDir is best-effort: a directory that is gone is not an error to report.
func TestSyncDirToleratesAMissingDirectory(t *testing.T) {
	syncDir(t.TempDir())
	syncDir(filepath.Join(t.TempDir(), "gone"))
}
