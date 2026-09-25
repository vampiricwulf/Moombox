package utils

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type resumeTestState struct {
	MessageCount int      `json:"messageCount"`
	Continuation string   `json:"continuation"`
	RecentIDs    []string `json:"recentIds"`
}

func TestResumeStoreSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := ResumeStore[resumeTestState]{Path: filepath.Join(dir, "state.json")}

	want := resumeTestState{
		MessageCount: 42,
		Continuation: "tok-abc",
		RecentIDs:    []string{"a", "b", "c"},
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MessageCount != want.MessageCount || got.Continuation != want.Continuation {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
	}
	if len(got.RecentIDs) != 3 || got.RecentIDs[2] != "c" {
		t.Errorf("RecentIDs lost: %+v", got.RecentIDs)
	}
}

func TestResumeStoreLoadMissingReturnsErrNoResume(t *testing.T) {
	dir := t.TempDir()
	store := ResumeStore[resumeTestState]{Path: filepath.Join(dir, "never-written.json")}

	_, err := store.Load()
	if !errors.Is(err, ErrNoResume) {
		t.Errorf("expected ErrNoResume, got %v", err)
	}
}

func TestResumeStoreLoadCorruptedJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	store := ResumeStore[resumeTestState]{Path: path}
	_, err := store.Load()
	if err == nil {
		t.Fatal("expected error on corrupted JSON, got nil")
	}
	if errors.Is(err, ErrNoResume) {
		t.Error("corrupted JSON should not map to ErrNoResume")
	}
}

func TestResumeStoreClearIdempotent(t *testing.T) {
	dir := t.TempDir()
	store := ResumeStore[resumeTestState]{Path: filepath.Join(dir, "state.json")}

	// Clear before Save must not error
	if err := store.Clear(); err != nil {
		t.Errorf("Clear on missing file should be nil, got %v", err)
	}

	// Save then Clear
	if err := store.Save(resumeTestState{MessageCount: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(store.Path); err != nil {
		t.Fatalf("file should exist after Save: %v", err)
	}
	if err := store.Clear(); err != nil {
		t.Errorf("Clear should succeed, got %v", err)
	}
	if _, err := os.Stat(store.Path); !os.IsNotExist(err) {
		t.Errorf("file should be gone after Clear, stat err=%v", err)
	}

	// Clear again — still idempotent
	if err := store.Clear(); err != nil {
		t.Errorf("Clear on missing file should be nil, got %v", err)
	}
}

func TestResumeStoreEmptyPathIsNoOp(t *testing.T) {
	store := ResumeStore[resumeTestState]{Path: ""}

	if err := store.Save(resumeTestState{MessageCount: 1}); err != nil {
		t.Errorf("Save with empty path should be nil, got %v", err)
	}
	_, err := store.Load()
	if !errors.Is(err, ErrNoResume) {
		t.Errorf("Load with empty path should return ErrNoResume, got %v", err)
	}
	if err := store.Clear(); err != nil {
		t.Errorf("Clear with empty path should be nil, got %v", err)
	}
}

func TestResumeStoreAtomicWriteNoTmpLeft(t *testing.T) {
	dir := t.TempDir()
	store := ResumeStore[resumeTestState]{Path: filepath.Join(dir, "state.json")}

	if err := store.Save(resumeTestState{MessageCount: 7}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// No temp file of ANY name may survive a successful Save. The old
	// fixed-name stat is retired because the name is now unpredictable; the
	// glob is its honest replacement on this SUCCESS path.
	//
	// Mutant this kills: WriteFileAtomic replacing its ReplaceFile rename with
	// a copy — the temp then outlives a successful Save and the glob finds it.
	// It does NOT carry the deferred-cleanup mutant: deleting WriteFileAtomic's
	// `defer … os.Remove(tmpPath)` leaves this test green, because a successful
	// ReplaceFile consumes the temp by renaming it onto the target. That mutant
	// belongs to TestResumeStoreSaveRoutesThroughTheSharedWriter, the
	// failure-path test, which is where it was verified to fail.
	assertNoTempSurvives(t, dir)
}

// TestResumeStoreJSONBackwardCompatible confirms the on-disk shape is identical
// to what the pre-refactor callers wrote, so existing .resume.json sidecars
// from a v2.5.2 deployment still load cleanly. The test marshals a state the
// way a pre-refactor caller would have (json.Marshal with the same struct)
// and verifies ResumeStore can read it.
func TestResumeStoreJSONBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")

	legacy := resumeTestState{MessageCount: 9, Continuation: "legacy-cont", RecentIDs: []string{"x"}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("legacy marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("legacy write: %v", err)
	}

	store := ResumeStore[resumeTestState]{Path: path}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load legacy sidecar: %v", err)
	}
	if got.MessageCount != 9 || got.Continuation != "legacy-cont" || got.RecentIDs[0] != "x" {
		t.Errorf("legacy round-trip mismatch: %+v", got)
	}
}

// TestResumeStoreSaveUsesAUniqueTempName pins the adopt: Save now goes through
// WriteFileAtomic, so the fixed `Path + ".tmp"` a concurrent writer might be
// holding is no longer consumed. Checked directly by seeding that fixed name
// rather than by racing two goroutines.
//
// Mutant this kills: restoring the local `tmp := s.Path + ".tmp"` writer — it
// truncates the seeded file and renames it onto the target, so the ReadFile
// below fails outright.
func TestResumeStoreSaveUsesAUniqueTempName(t *testing.T) {
	dir := t.TempDir()
	store := ResumeStore[resumeTestState]{Path: filepath.Join(dir, "state.json")}
	fixed := store.Path + ".tmp"

	if err := os.WriteFile(fixed, []byte("another-writer-is-mid-write"), 0o644); err != nil {
		t.Fatalf("seed fixed temp: %v", err)
	}
	if err := store.Save(resumeTestState{MessageCount: 7}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := os.ReadFile(fixed)
	if err != nil {
		t.Fatalf("the fixed .tmp name was consumed: %v", err)
	}
	if string(got) != "another-writer-is-mid-write" {
		t.Errorf("fixed .tmp content: want it untouched, got %q", string(got))
	}
}

// TestResumeStoreSaveRoutesThroughTheSharedWriter drives WriteFileAtomic's
// syncFile seam from Save's side: with the fsync failing, Save must fail, a
// pre-existing sidecar must be left exactly as it was, and no temp may
// survive. Losing the sidecar's OLD contents on a failed write is the real
// cost here — a corrupt or missing sidecar sends a REPLAY chat run back to the
// top of the archive (see Save's doc comment).
//
// Mutants this kills: reverting to the local writer — its own f.Sync() never
// consults the seam, so Save returns nil and the seeded sidecar is replaced;
// dropping WriteFileAtomic's deferred os.Remove — the leftover *.tmp
// assertion fails. This failure path, NOT the success-path glob in
// TestResumeStoreAtomicWriteNoTmpLeft, is what carries that second mutant
// (verified at plan review: with the deferred cleanup deleted this test
// reports `temp files survived: [...\state.json.732729699.tmp]` while the
// no-tmp-left test stays green).
func TestResumeStoreSaveRoutesThroughTheSharedWriter(t *testing.T) {
	dir := t.TempDir()
	store := ResumeStore[resumeTestState]{Path: filepath.Join(dir, "state.json")}
	if err := os.WriteFile(store.Path, []byte(`{"messageCount":1}`), 0o644); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}

	injected := errors.New("injected sync failure")
	orig := syncFile
	syncFile = func(f *os.File) error { return injected }
	t.Cleanup(func() { syncFile = orig })

	err := store.Save(resumeTestState{MessageCount: 99})
	if err == nil {
		t.Fatal("Save: want the injected fsync failure, got nil")
	}
	if !errors.Is(err, injected) {
		t.Errorf("error chain: want it to wrap %v, got %v", injected, err)
	}

	got, readErr := os.ReadFile(store.Path)
	if readErr != nil {
		t.Fatalf("read back sidecar: %v", readErr)
	}
	if string(got) != `{"messageCount":1}` {
		t.Errorf("sidecar after a failed Save: want untouched, got %q", string(got))
	}
	assertNoTempSurvives(t, dir)
}

// TestResumeStoreEmptyPathNeverReachesTheSharedWriter guards the no-op that
// the adopt could quietly have dropped. With Path == "" the shared writer must
// not run at all: filepath.Dir("") is ".", so it would create a stray temp in
// the process working directory before failing on the rename. Green before and
// after the adopt — it exists so the guard cannot be deleted later.
//
// Mutant this kills: removing `if s.Path == "" { return nil }` — the seam is
// invoked (called becomes true) and Save returns a rename error instead of nil,
// so both assertions fire.
func TestResumeStoreEmptyPathNeverReachesTheSharedWriter(t *testing.T) {
	orig := syncFile
	called := false
	syncFile = func(f *os.File) error {
		called = true
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = orig })

	store := ResumeStore[resumeTestState]{Path: ""}
	if err := store.Save(resumeTestState{MessageCount: 1}); err != nil {
		t.Fatalf("Save with an empty Path: want nil, got %v", err)
	}
	if called {
		t.Error("the shared writer ran for an empty Path; the no-op guard must return before it")
	}
}

// resumeGoldenPreAdopt is the EXACT sidecar the pre-adopt Save wrote for the
// fixture in TestResumeStoreSaveEncodingIsUnchanged: compact json.Marshal, no
// indent, no trailing newline. Captured from the writer at main @ ce304b09
// before this task changed it.
const resumeGoldenPreAdopt = `{"messageCount":42,"continuation":"tok-abc","recentIds":["a","b","c"]}`

// TestResumeStoreSaveEncodingIsUnchanged is a REGRESSION pin, not a red-first
// test: green before AND after the adopt. Sidecars written by earlier builds
// are read back by Load on the next run (TestResumeStoreJSONBackwardCompatible
// covers the other direction), so the encoder must not move.
//
// Mutants this kills: swapping json.Marshal for json.MarshalIndent; appending
// a trailing newline. Verify its teeth by execution — make one of those edits,
// watch this test fail, revert.
func TestResumeStoreSaveEncodingIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	store := ResumeStore[resumeTestState]{Path: filepath.Join(dir, "state.json")}

	if err := store.Save(resumeTestState{
		MessageCount: 42,
		Continuation: "tok-abc",
		RecentIDs:    []string{"a", "b", "c"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(raw) != resumeGoldenPreAdopt {
		t.Errorf("encoding drifted.\n got: %q\nwant: %q", string(raw), resumeGoldenPreAdopt)
	}
}
