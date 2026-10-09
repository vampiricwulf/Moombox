package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The first boot of a fresh update whose rollback artifact is gone has nothing
// to roll back to: on Linux the boot reached its first-successful-boot
// milestone, whose sweep removed .old. An exit inside the post-update window
// used to end the launcher on preserve-with-instructions — with nothing to
// preserve — where any other boot's crash is respawned. While the artifact is
// there (before that sweep; on Windows, when it is the launcher's ~ image,
// all window long) the boot is still one a rollback can undo.
//
// Mutant: postUpdatePastRollback reporting false whatever the disk holds —
// the boot past its sweep is still routed to a rollback that cannot happen.
func TestPostUpdatePastRollbackFollowsTheArtifact(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox")
	if err := os.WriteFile(exePath, []byte("new release"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup := exePath + ".old"
	if err := os.WriteFile(backup, []byte("previous release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if postUpdatePastRollback(backup) {
		t.Error("with .old on disk the boot can still be rolled back")
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	if !postUpdatePastRollback(backup) {
		t.Error("with no rollback artifact left the boot is past rollback")
	}
}

// The launcher loop feeds postUpdatePastRollback into classifyChildExit twice:
// a first boot past its rollback artifact is no longer a first boot a
// rollback can undo, and its quick death is supervised rather than failed
// fast. Both helpers were tested; the wiring between them was not.
//
// Mutants: drop `|| pastRollback` from the supervised argument — the boot past
// its artifact fails fast and ends the launcher; drop `&& !pastRollback` —
// it is routed to a rollback that cannot happen.
func TestJudgeChildExitWiresThePastRollbackBoot(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox")
	if err := os.WriteFile(exePath, []byte("new release"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup := exePath + ".old"
	if err := os.WriteFile(backup, []byte("previous release"), 0o755); err != nil {
		t.Fatal(err)
	}

	action, first := judgeChildExit(backup, true, false, 1, 10*time.Second, false, 0)
	if action != childPostUpdateFailure || !first {
		t.Errorf("a quick crash with .old on disk = (%v, first %v), want a rollback of the first boot", action, first)
	}

	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	action, first = judgeChildExit(backup, true, false, 1, 10*time.Second, false, 0)
	if action != childCrash || first {
		t.Errorf("a quick crash past the rollback artifact = (%v, first %v), want a supervised crash", action, first)
	}

	// Not a first boot at all: the fresh-launch fail-fast rule stands.
	if action, _ := judgeChildExit(backup, false, false, 1, 10*time.Second, false, 0); action != childPropagate {
		t.Errorf("a fresh launch's quick crash = %v, want it propagated", action)
	}
}

// The second update of one Windows launcher lifetime leaves its previous
// binary at .old, because the ~ name is still the launcher's own mapped image
// (handleUpdateRestart reports .old as the artifact). Once that boot reaches
// its milestone, CleanupOldBinary sweeps .old and only ~ — two versions back —
// is left. Judged by the names on disk, the boot looked like one a rollback
// could undo, and the rollback restored ~ over the release in between: N+2
// rolled back to N, N+1 gone, and N facing a database N+1 or N+2 had already
// migrated. Judged by the artifact recorded at the restart, the boot is past
// rollback and its crash is supervised. Cross-platform: the judgement takes a
// path, whatever names the platform gives it.
//
// Mutants: postUpdatePastRollback falling back to the ~ sibling when the
// artifact is gone (`if err != nil { _, err = os.Stat(strings.TrimSuffix(
// artifact, ".old") + "~") }`, rollbackArtifactPath's old chain) — the crash
// is routed to a rollback; attemptAutoRollback restoring that sibling instead
// of the artifact it is handed — the exe holds "N".
func TestASweptOldIsNotStoodInForByTheLauncherImage(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	for _, f := range []struct{ path, body string }{
		{exePath, "N+2"},
		{exePath + "~", "N"}, // the launcher's image since the first update
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	artifact := exePath + ".old" // N+1, recorded at the restart, since swept

	action, first := judgeChildExit(artifact, true, false, 1, 30*time.Second, false, 0)
	if action != childCrash || first {
		t.Errorf("a quick crash after the second update's .old was swept = (%v, first %v), want a supervised crash — "+
			"~ is two versions back, not this update's artifact", action, first)
	}
	if attemptAutoRollback(exePath, artifact, 1) {
		t.Error("attemptAutoRollback restored something although this update's artifact is gone")
	}
	if !bytesEqualFile(t, exePath, "N+2") {
		t.Error("the release in place must be left as it is")
	}
	if !bytesEqualFile(t, exePath+"~", "N") {
		t.Error("the launcher's image must be left as it is")
	}
}
