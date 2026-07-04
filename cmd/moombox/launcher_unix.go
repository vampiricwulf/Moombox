//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// cleanupOrphans is a no-op on Linux. Linux can delete a running
// binary directly (file deletion removes the directory entry while
// keeping the inode alive for any process holding it open), so the
// child's CleanupOldBinary handles everything.
func cleanupOrphans(exePath string) {}

// handleUpdateRestart runs after the child exits with exitCodeRestart.
// Linux needs no rename dance (the child's post-milestone
// CleanupOldBinary removes .old directly), but the launcher still
// reports whether this restart followed a binary update (.old exists) —
// config-change restarts never create .old, so this is the launcher's
// only signal that the NEXT child is the first boot of a fresh update.
func handleUpdateRestart(exePath string) bool {
	_, statErr := os.Stat(exePath + ".old")
	return statErr == nil
}

// rollbackArtifactPath is where the previous version's binary survives
// after an update on this platform. On Linux the .old file keeps its
// name; a boot-crashing update never reaches the post-milestone
// CleanupOldBinary sweep, so it is still present exactly when the
// recovery instructions need it.
func rollbackArtifactPath(exePath string) string {
	return exePath + ".old"
}

// setSysProcAttr is a no-op on Linux. There's no equivalent of
// CreationFlags=createNoWindow because Linux processes don't open
// console windows the same way Windows ones do.
func setSysProcAttr(cmd *exec.Cmd) {}

// deferDeleteOldLauncher is a no-op on Linux. No deferred cleanup
// needed because Linux has no orphan files to clean.
func deferDeleteOldLauncher(exePath string) {}
