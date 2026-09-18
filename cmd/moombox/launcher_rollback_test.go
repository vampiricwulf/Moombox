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

// TestRollbackArtifactPathPrefersTheOldFile pins the fix for the second
// in-place update of one launcher lifetime: when handleUpdateRestart could not
// rename .old out of the way (the ~ name is held by the launcher's own mapped
// image), .old is the version that was running a moment ago and ~ is the one
// BEFORE it. Restoring ~ there rolls back two versions AND the restored child's
// CleanupOldBinary then deletes the real previous binary.
//
// Cross-platform on purpose: on Linux .old is the only artifact there has ever
// been, so "the artifact is .old when .old exists" is true on every platform
// and this runs on both CI runners.
//
// Mutant: reverting rollbackArtifactPath to `return exePath + "~"` fails this
// on Windows.
func TestRollbackArtifactPathPrefersTheOldFile(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox.exe")
	if err := os.WriteFile(exePath, []byte("current binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exePath+".old", []byte("previous binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got, want := rollbackArtifactPath(exePath), exePath+".old"; got != want {
		t.Errorf("rollbackArtifactPath = %q, want %q — a surviving .old is the freshest previous binary", got, want)
	}
}

// A failed first-post-update boot must KEEP the broken binary as
// <exe>.failed instead of deleting it: the DB downgrade guard the restored
// (older) binary then hits tells the operator to restore the newer binary,
// and before this the rollback had just deleted the only copy (CORE-1).
//
// Mutant: restoring `os.Remove(exePath)` in attemptAutoRollback — the
// .failed file is absent and the marker no longer names it.
func TestAutoRollbackKeepsTheFailedBinary(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox-test.exe")
	if err := os.WriteFile(exePath, []byte("BROKEN"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollbackArtifactPath(exePath), []byte("PREVIOUS"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !attemptAutoRollback(exePath, 1) {
		t.Fatal("attemptAutoRollback must succeed when the artifact exists")
	}

	restored, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("the previous binary must be back at the plain name: %v", err)
	}
	if string(restored) != "PREVIOUS" {
		t.Errorf("plain name holds %q, want the restored previous binary", restored)
	}
	failed, err := os.ReadFile(exePath + failedBinarySuffix)
	if err != nil {
		t.Fatalf("the broken binary must survive as %s: %v", failedBinarySuffix, err)
	}
	if string(failed) != "BROKEN" {
		t.Errorf("%s holds %q, want the broken binary", failedBinarySuffix, failed)
	}
	marker, err := os.ReadFile(exePath + ".update-failed")
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if !strings.Contains(string(marker), exePath+failedBinarySuffix) {
		t.Errorf("the marker must name the kept binary by path, got:\n%s", marker)
	}
}

// A second failed update replaces the first .failed artifact rather than
// failing the rollback — os.Rename replaces an existing destination on both
// platforms.
//
// Mutant: guarding the rename with an os.Stat "already exists" bail-out —
// the artifact still holds BROKEN-1.
func TestAutoRollbackReplacesAnOlderFailedArtifact(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox-test.exe")
	for _, f := range []struct{ path, body string }{
		{exePath, "BROKEN-2"},
		{exePath + failedBinarySuffix, "BROKEN-1"},
		{rollbackArtifactPath(exePath), "PREVIOUS"},
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !attemptAutoRollback(exePath, 1) {
		t.Fatal("attemptAutoRollback must succeed")
	}
	failed, err := os.ReadFile(exePath + failedBinarySuffix)
	if err != nil {
		t.Fatalf("read %s: %v", failedBinarySuffix, err)
	}
	if string(failed) != "BROKEN-2" {
		t.Errorf("%s holds %q, want the newest broken binary", failedBinarySuffix, failed)
	}
}

// A deterministic startup error is the operator's environment (bad config,
// bad flags, a refused migration), not proof the new binary is broken — so
// it must never auto-roll-back and never mark the release skipped. Before
// this, whether such an exit counted as "the update failed" depended on
// whether the operator pressed Enter at waitForKeypress inside the 2-minute
// window (CORE-23). classifyPostUpdateExit is that decision, extracted so it
// can be asserted without spawning a launcher.
//
// Mutant: returning postUpdateRollback for exitCodeStartupError — the
// "startup error" row reports rollback.
func TestStartupErrorIsNeverAFailedUpdate(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want postUpdateVerdict
	}{
		{"startup error", exitCodeStartupError, postUpdatePreserve},
		{"panic-shaped crash", 2, postUpdateRollback},
		{"generic failure", 1, postUpdateRollback},
	} {
		if got := classifyPostUpdateExit(tc.code); got != tc.want {
			t.Errorf("%s: classifyPostUpdateExit(%d) = %v, want %v", tc.name, tc.code, got, tc.want)
		}
	}
}
