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
	if _, err := os.Stat(store.Path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected .tmp to be gone after successful rename, stat err=%v", err)
	}
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
