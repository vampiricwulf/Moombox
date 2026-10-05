package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The first boot of a fresh update whose rollback artifact is gone has nothing
// to roll back to: on Linux the boot reached its first-successful-boot
// milestone, whose sweep removed .old. An exit inside the post-update window
// used to end the launcher on preserve-with-instructions — with nothing to
// preserve — where any other boot's crash is respawned. While the artifact is
// there (before that sweep; on Windows, the launcher's ~ image all window
// long) the boot is still one a rollback can undo.
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
	if postUpdatePastRollback(exePath) {
		t.Error("with .old on disk the boot can still be rolled back")
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	// Windows falls back to the launcher's ~ image; there is none here.
	if !postUpdatePastRollback(exePath) {
		t.Error("with no rollback artifact left the boot is past rollback")
	}
}
