//go:build !windows

package main

import "os/exec"

// cleanupOrphans is a no-op on Linux. Linux can delete a running
// binary directly (file deletion removes the directory entry while
// keeping the inode alive for any process holding it open), so the
// child's CleanupOldBinary at startup handles everything.
func cleanupOrphans(exePath string) {}

// handleUpdateRestart is a no-op on Linux. After the child exits with
// exitCodeRestart, we just spawn a new child from the new binary path.
// The old .old is removed by the next child's CleanupOldBinary on
// startup (works on Linux because os.Remove of a locked file succeeds).
func handleUpdateRestart(exePath string) {}

// setSysProcAttr is a no-op on Linux. There's no equivalent of
// CreationFlags=createNoWindow because Linux processes don't open
// console windows the same way Windows ones do.
func setSysProcAttr(cmd *exec.Cmd) {}

// deferDeleteOldLauncher is a no-op on Linux. No deferred cleanup
// needed because Linux has no orphan files to clean.
func deferDeleteOldLauncher(exePath string) {}
