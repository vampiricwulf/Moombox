//go:build !windows

package main

import (
	"os"
)

// cleanupOrphans is a no-op on Linux. Linux can delete a running
// binary directly (file deletion removes the directory entry while
// keeping the inode alive for any process holding it open), so the
// child's CleanupOldBinary handles everything.
func cleanupOrphans(exePath string) {}

// handleUpdateRestart runs after the child exits with exitCodeRestart.
// Linux needs no rename dance (the child's post-milestone
// CleanupOldBinary removes .old directly), but the launcher still
// learns whether this restart followed a binary update (.old exists) —
// config-change restarts never create .old, so this is the launcher's
// only signal that the NEXT child is the first boot of a fresh update.
//
// It returns that update's rollback artifact, "" for a config restart. On
// Linux the .old file keeps its name; a boot-crashing update never reaches
// the post-milestone CleanupOldBinary sweep, so it is still present exactly
// when the rollback and the recovery instructions need it.
func handleUpdateRestart(exePath string) string {
	oldPath := exePath + ".old"
	if _, statErr := os.Stat(oldPath); statErr != nil {
		return ""
	}
	return oldPath
}

// deferDeleteOldLauncher is a no-op on Linux. No deferred cleanup
// needed because Linux has no orphan files to clean.
func deferDeleteOldLauncher(exePath string) {}

// keepAsideByLink keeps the failed binary at failedPath as a second hard link,
// replacing any stale one, so attemptAutoRollback's restore is a single rename
// over exePath and the plain name is never empty — a kill between the old
// move-aside and the restore left no binary there and nothing that could
// start to repair it. False (a filesystem without hard links, a failedPath
// that cannot be cleared) sends the rollback down the move-aside path.
func keepAsideByLink(exePath, failedPath string) bool {
	os.Remove(failedPath)
	return os.Link(exePath, failedPath) == nil
}
