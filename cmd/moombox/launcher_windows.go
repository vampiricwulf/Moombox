package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// createNoWindow prevents a console window from appearing when spawning
// detached processes on Windows (passed to SysProcAttr.CreationFlags).
const createNoWindow = 0x08000000

// cleanupOrphans removes any stale `~` files left over from a prior
// session. Runs once at launcher startup, before the supervised child
// is spawned. The .exe~ may exist if a prior launcher exited before
// the deferred ping/del fired (system shutdown during the 11s window,
// antivirus blocked the cmd, etc.). Now-unlocked, removable.
func cleanupOrphans(exePath string) {
	os.Remove(exePath + "~")
}

// handleUpdateRestart runs after the child exits with exitCodeRestart.
// On Windows the launcher cannot delete its own running binary, so we
// rename the just-superseded .old to ~ to free the .old name for the
// next update. The ~ file is then deferred-cleaned on launcher exit.
func handleUpdateRestart(exePath string) {
	oldPath := exePath + ".old"
	if _, statErr := os.Stat(oldPath); statErr == nil {
		os.Rename(oldPath, exePath+"~")
	}
}

// setSysProcAttr applies Windows-only CreationFlags so the spawned
// process doesn't open a visible console window. Used for any
// fire-and-forget background spawn.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

// deferDeleteOldLauncher schedules deletion of the .exe~ file via a
// detached cmd /c invocation that uses ping as a sleep mechanism,
// then runs del. The ~11s wait covers normal launcher exit-and-
// handle-release time. The launcher's startup cleanupOrphans is the
// safety net if this somehow doesn't fire.
//
// History: tried timeout.exe earlier — it errors out unconditionally
// when stdin is redirected (per Microsoft docs), which it always is
// for a Go-spawned cmd subprocess. The `& del` then ran immediately,
// before the launcher had released the file lock. ping has no stdin
// dependency: 12 pings × 1s default interval = 11s of wall-clock
// delay. This was the original approach pre-schtasks era.
//
// Earlier history: also tried schtasks (task name containing ~ was
// rejected, /st time wraparound, /tr quoting fragility, no exit-code
// check). ping is the simplest reliable option.
func deferDeleteOldLauncher(exePath string) {
	oldPath := exePath + "~"
	if _, err := os.Stat(oldPath); err != nil {
		return // no .exe~ file to clean up
	}
	delayedDel := fmt.Sprintf(
		`ping 127.0.0.1 -n 12 >nul & del /f /q "%s" >nul 2>nul`, oldPath)
	cleanup := exec.Command("cmd", "/C", delayedDel)
	setSysProcAttr(cleanup)
	cleanup.Start() // fire-and-forget; we exit shortly anyway
}
