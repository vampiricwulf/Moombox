package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWriteFileAtomicWritesContentAndPerm is the ordinary path.
//
// Mutant this kills: dropping the os.Chmod leaves the file at CreateTemp's
// 0600 on POSIX, and the perm assertion fails.
func TestWriteFileAtomicWritesContentAndPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.js")

	if err := WriteFileAtomic(path, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("content: want %q, got %q", "payload", string(got))
	}
	if runtime.GOOS != "windows" {
		// Windows has no POSIX mode bits to assert; everywhere else the
		// chmod is what lifts the file off os.CreateTemp's 0600.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("perm: want 0644, got %v", info.Mode().Perm())
		}
	}
}

// TestWriteFileAtomicOverwritesAndLeavesNoTemp pins two properties at once:
// the replace is in-place (not an append or a second file), and no temp file
// survives a successful write.
//
// Mutant this kills: removing the ReplaceFile call and writing to path
// directly leaves the old content (or a partial file) and, with CreateTemp
// still in place, one leftover temp — either assertion fails.
func TestWriteFileAtomicOverwritesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.js")

	if err := os.WriteFile(path, []byte("stale-and-longer"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("fresh"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("content after overwrite: want %q, got %q", "fresh", string(got))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file %q survived a successful write", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("directory entries after one write: want 1, got %d", len(entries))
	}
}

// TestWriteFileAtomicUsesAUniqueTempName pins the reason this helper exists
// rather than `path + ".tmp"`: two writers aiming at one file must not be
// able to interleave into a single temp and rename the corrupt result into
// place. Rather than racing two writers, the property is checked directly --
// the directory is pre-seeded with the FIXED name a second writer would be
// using, and the helper must leave it untouched.
//
// Mutant this kills: reverting to `tmpPath := path + ".tmp"` clobbers the
// seeded file, and the assertion that it still holds its marker fails.
func TestWriteFileAtomicUsesAUniqueTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.js")
	fixed := path + ".tmp"

	if err := os.WriteFile(fixed, []byte("another-writer-is-mid-write"), 0o644); err != nil {
		t.Fatalf("seed fixed temp: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("fresh"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(fixed)
	if err != nil {
		t.Fatalf("the fixed .tmp name was consumed: %v", err)
	}
	if string(got) != "another-writer-is-mid-write" {
		t.Errorf("fixed .tmp content: want it untouched, got %q", string(got))
	}
}
