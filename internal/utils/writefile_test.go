package utils

import (
	"errors"
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

// TestWriteFileAtomicSyncFailureLeavesNoTempAndTargetUntouched drives a real
// failure branch via the syncFile seam: the target pre-exists, the fsync
// fails, and the helper must (1) return an error wrapping the injected one,
// (2) leave the pre-existing target's content untouched, and (3) leave no
// temp file behind.
//
// Mutant this kills: dropping the shared deferred os.Remove(tmpPath) (or an
// unconditional `done = true`) leaves the temp file on disk despite the
// error — assertion (3) fails. Dropping the syncFile call entirely means the
// injected error is never consulted, the write "succeeds", and both (1) (no
// error) and the target-untouched check (2) fail instead.
func TestWriteFileAtomicSyncFailureLeavesNoTempAndTargetUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.js")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	injected := errors.New("injected sync failure")
	orig := syncFile
	syncFile = func(f *os.File) error { return injected }
	t.Cleanup(func() { syncFile = orig })

	err := WriteFileAtomic(path, []byte("new-content"), 0o644)
	if err == nil {
		t.Fatal("WriteFileAtomic: want error from injected sync failure, got nil")
	}
	if !errors.Is(err, injected) {
		t.Errorf("error chain: want it to wrap %v, got %v", injected, err)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back target: %v", readErr)
	}
	if string(got) != "original" {
		t.Errorf("target content after sync failure: want untouched %q, got %q", "original", string(got))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file %q survived a sync failure", e.Name())
		}
	}
}

// TestWriteFileAtomicSyncsBeforeReplacingTarget pins fsync-before-rename on a
// FRESH target (one that does not exist until ReplaceFile creates it): the
// syncFile seam records whether the target already existed at the moment it
// ran. If the sync genuinely happens before the rename, it never does.
//
// Mutant this kills: moving the syncFile call to after the ReplaceFile call
// means the target already exists by the time the seam runs, flipping
// targetExisted to true; dropping the syncFile call entirely means the seam
// is never invoked, leaving called false.
func TestWriteFileAtomicSyncsBeforeReplacingTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.js")

	orig := syncFile
	called := false
	targetExisted := false
	syncFile = func(f *os.File) error {
		called = true
		if _, statErr := os.Stat(path); statErr == nil {
			targetExisted = true
		}
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = orig })

	if err := WriteFileAtomic(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if !called {
		t.Fatal("syncFile seam was never invoked")
	}
	if targetExisted {
		t.Error("target already existed when syncFile ran; Sync must happen before ReplaceFile")
	}
}

// TestWriteFileAtomicRenameFailureLeavesNoTempAndTargetIntact drives the
// late failure branch: path is a NON-EMPTY directory, so ReplaceFile's
// underlying rename refuses it on both Windows and POSIX (a file can never
// replace a directory, empty or not). The helper must return an error, leave
// the directory and its contents untouched, and leave no temp file behind.
//
// Mutant this kills: dropping the shared deferred os.Remove(tmpPath) on the
// ReplaceFile-failure branch (or an unconditional `done = true` set before
// the check) leaves the temp file on disk despite the error.
func TestWriteFileAtomicRenameFailureLeavesNoTempAndTargetIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "player.js")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed target directory: %v", err)
	}
	marker := filepath.Join(path, "keep.txt")
	if err := os.WriteFile(marker, []byte("do-not-touch"), 0o644); err != nil {
		t.Fatalf("seed marker file: %v", err)
	}

	err := WriteFileAtomic(path, []byte("content"), 0o644)
	if err == nil {
		t.Fatal("WriteFileAtomic: want error replacing a non-empty directory, got nil")
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stat target directory: %v", statErr)
	}
	if !info.IsDir() {
		t.Error("target: want it still a directory")
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "do-not-touch" {
		t.Errorf("marker file: want untouched, read err=%v content=%q", readErr, string(got))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file %q survived a rename failure", e.Name())
		}
	}
}
