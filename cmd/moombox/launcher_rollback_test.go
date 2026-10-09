package main

import (
	"io"
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
	backup := exePath + ".old"
	if err := os.WriteFile(backup, []byte("known-good previous binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !attemptAutoRollback(exePath, backup, 2) {
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

	if attemptAutoRollback(exePath, exePath+".old", 2) {
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
	if err := os.WriteFile(exePath+".old", []byte("PREVIOUS"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !attemptAutoRollback(exePath, exePath+".old", 1) {
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
		{exePath + ".old", "PREVIOUS"},
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !attemptAutoRollback(exePath, exePath+".old", 1) {
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

// The move aside is worth three retries; the ROLLBACK is not negotiable. A
// destination the rename cannot replace — a stale <exe>.failed from an
// earlier failed update, held open by a scanner without FILE_SHARE_DELETE —
// used to block a restore that the plain os.Remove(exePath) this code
// replaced would have completed. After the three failed renames the fallback
// removes the broken binary and carries on (B-6).
//
// A non-empty DIRECTORY at <exe>.failed is the portable stand-in for that
// undeletable destination: os.Rename refuses it on both platforms, and
// removing exePath (an ordinary file) still succeeds.
//
// Mutant: drop the os.Remove fallback — attemptAutoRollback returns false and
// the install stays on the broken binary.
func TestAutoRollbackFallsBackToRemovingTheFailedBinary(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox-test.exe")
	if err := os.WriteFile(exePath, []byte("BROKEN"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exePath+".old", []byte("PREVIOUS"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An undeletable destination: a directory that is not empty.
	blocked := exePath + failedBinarySuffix
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "occupant"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !attemptAutoRollback(exePath, exePath+".old", 1) {
		t.Fatal("a destination the move aside cannot take must not block the rollback — the " +
			"os.Remove this replaced would have restored the previous binary")
	}
	if !bytesEqualFile(t, exePath, "PREVIOUS") {
		t.Error("the previous binary must be back at the plain name")
	}
	marker, err := os.ReadFile(exePath + ".update-failed")
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.Contains(string(marker), "KEPT at") {
		t.Errorf("the marker names a kept binary that is not there, got:\n%s", marker)
	}
	if !strings.Contains(string(marker), "REMOVED") {
		t.Errorf("the marker must say the failed release was removed, got:\n%s", marker)
	}
}

// The .update-failed marker is what the NEXT boot announces as "Previous
// Update Failed", and its instructions begin "To roll back: replace <exe>".
// On a deterministic startup error that advice is wrong twice over: the
// binary is fine, and an operator who follows it lands on the older release
// and has the good one marked skipped. The preserve path still skips the
// artifact cleanup — a manual rollback stays one rename away — but it says so
// honestly and writes no marker (CORE-23 / B-5).
//
// Mutant: drop the classifyPostUpdateExit branch from preserveUpdateRollback
// — exit 3 writes the "update failed / roll back" marker again.
func TestStartupErrorPreservesWithoutAFailedUpdateMarker(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox-test.exe")
	if err := os.WriteFile(exePath, []byte("current binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup := exePath + ".old"
	if err := os.WriteFile(backup, []byte("PREVIOUS"), 0o755); err != nil {
		t.Fatal(err)
	}

	stderr := captureStderr(t, func() { preserveUpdateRollback(exePath, backup, exitCodeStartupError) })

	if _, err := os.Stat(exePath + ".update-failed"); !os.IsNotExist(err) {
		t.Errorf("a startup error must write no .update-failed marker (stat err: %v) — the next boot "+
			"would announce a failed update and advise a rollback that is not the fix", err)
	}
	for _, want := range []string{"exit code 3", "startup/environment", backup, "not the fix"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the startup-error notice does not mention %q, got:\n%s", want, stderr)
		}
	}
	if !bytesEqualFile(t, backup, "PREVIOUS") {
		t.Error("the rollback artifact must be preserved untouched")
	}

	// Every other code keeps the marker, byte for byte as before.
	marker := exePath + ".update-failed"
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	preserveUpdateRollback(exePath, backup, 1)
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("exit 1 must still write the marker: %v", err)
	}
	if !strings.Contains(string(got), "To roll back") {
		t.Errorf("the exit-1 marker lost its rollback instructions, got:\n%s", got)
	}
}

// captureStderr runs body with os.Stderr redirected to a pipe and returns
// what it wrote. A pipe, not a file, so nothing is left in the temp dir; the
// reader runs concurrently so a message larger than the pipe buffer cannot
// deadlock.
func captureStderr(t *testing.T, body func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	body()
	os.Stderr = saved
	w.Close()
	s := <-out
	r.Close()
	return s
}

func bytesEqualFile(t *testing.T, path, want string) bool {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read %s: %v", path, err)
		return false
	}
	return string(got) == want
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
