package main

import (
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
// then runs del. The ~4s wait covers normal launcher exit-and-
// handle-release time (os.Exit releases handles within milliseconds;
// 4s is generous headroom). The launcher's startup cleanupOrphans
// is the safety net if this somehow doesn't fire.
//
// Args are passed variadically — NOT joined into a single string —
// because Go's syscall.EscapeArg targets CRT-style parsing (compatible
// with CommandLineToArgvW), which disagrees with cmd.exe on quotes.
// Joining `... & del /f /q "%s" ...` into one arg makes Go wrap the
// whole string in quotes and escape the inner literal " as \", which
// cmd then mis-parses (cmd uses "" for embedded quotes, not \"). del
// receives a mangled path and >nul 2>nul swallows the failure. With
// variadic args, tokens like `>nul`, `&`, `2>nul` go through unquoted
// as bare cmd operators, and oldPath only gets quoted by Go if it
// actually contains spaces — both cases cmd parses correctly.
//
// History: tried timeout.exe earlier — it errors out unconditionally
// when stdin is redirected (per Microsoft docs), which it always is
// for a Go-spawned cmd subprocess. The `& del` then ran immediately,
// before the launcher had released the file lock. ping has no stdin
// dependency: 5 pings × 1s default interval = 4s of wall-clock delay.
//
// Earlier history: also tried schtasks (task name containing ~ was
// rejected, /st time wraparound, /tr quoting fragility, no exit-code
// check). ping is the simplest reliable option.
func deferDeleteOldLauncher(exePath string) {
	oldPath := exePath + "~"
	if _, err := os.Stat(oldPath); err != nil {
		return // no .exe~ file to clean up
	}
	cleanup := exec.Command("cmd", "/C",
		"ping", "127.0.0.1", "-n", "5", ">nul", "&",
		"del", "/f", "/q", oldPath, ">nul", "2>nul")
	setSysProcAttr(cleanup)
	cleanup.Start() // fire-and-forget; we exit shortly anyway
}
