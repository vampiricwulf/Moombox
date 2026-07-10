package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAttemptAutoRollbackRestoresPreviousBinary pins the happy path: with a
// rollback artifact present, the broken binary is replaced by the previous
// one, the artifact is consumed, and the marker documents the rollback.
func TestAttemptAutoRollbackRestoresPreviousBinary(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox.exe")
	if err := os.WriteFile(exePath, []byte("broken new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup := rollbackArtifactPath(exePath)
	if err := os.WriteFile(backup, []byte("known-good previous binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !attemptAutoRollback(exePath, 2) {
		t.Fatal("attemptAutoRollback: want true with artifact present")
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("read restored exe: %v", err)
	}
	if string(got) != "known-good previous binary" {
		t.Errorf("restored exe contents: want previous binary, got %q", string(got))
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Errorf("rollback artifact should be consumed by the restore, stat err: %v", err)
	}
	marker, err := os.ReadFile(exePath + ".update-failed")
	if err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if !strings.Contains(string(marker), "AUTOMATICALLY ROLLED BACK") {
		t.Errorf("marker should document the automatic rollback, got:\n%s", marker)
	}
	if !strings.Contains(string(marker), "exit code 2") {
		t.Errorf("marker should carry the failing exit code, got:\n%s", marker)
	}
}

// TestAttemptAutoRollbackNoArtifact pins the fallback contract: with no
// rollback artifact (the boot survived to the milestone sweep before dying),
// the function must decline WITHOUT touching the binary or writing a marker —
// the caller then runs preserveUpdateRollback's manual-instruction path.
func TestAttemptAutoRollbackNoArtifact(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox.exe")
	if err := os.WriteFile(exePath, []byte("broken new binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if attemptAutoRollback(exePath, 2) {
		t.Fatal("attemptAutoRollback: want false with no artifact")
	}
	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("read exe: %v", err)
	}
	if string(got) != "broken new binary" {
		t.Errorf("exe must be untouched on decline, got %q", string(got))
	}
	if _, err := os.Stat(exePath + ".update-failed"); !os.IsNotExist(err) {
		t.Errorf("no marker may be written on decline, stat err: %v", err)
	}
}
