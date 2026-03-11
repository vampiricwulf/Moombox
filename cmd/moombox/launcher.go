package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// launchAndSupervise is the launcher/supervisor loop. It spawns moombox as a
// child process in the same console and waits. If the child exits with
// exitCodeRestart (config change or update applied), it respawns — picking up
// any new binary on disk. For any other exit code it propagates and exits.
//
// This keeps one stable parent holding the console connection so the child's
// BubbleTea properly restores terminal state, and avoids process chain buildup
// since the launcher always swaps to a fresh child rather than nesting.
//
// After an update restart, the launcher renames .old -> .exe~ so the .old name
// stays free for future updates. Windows locks running executables (the launcher
// itself is running from the old binary), so the .old can't be deleted — but it
// CAN be renamed. The .exe~ file is cleaned up on exit via a deferred delete
// process, or on the next fresh launcher start.
func launchAndSupervise() {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to determine executable path: %v\n", err)
		os.Exit(1)
	}

	// Clean up old launcher binary from a previous session (now unlocked).
	os.Remove(exePath + "~")

	// Ignore interrupts in the launcher — the child handles Ctrl+C.
	signal.Ignore(os.Interrupt)

	for {
		cmd := exec.Command(exePath, os.Args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "_MOOMBOX_CHILD=1")

		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				if exitErr.ExitCode() == exitCodeRestart {
					// If an update was applied, the old binary is now
					// .old but is locked by us (the launcher). Rename
					// to .exe~ to free the .old name for future updates.
					oldPath := exePath + ".old"
					if _, statErr := os.Stat(oldPath); statErr == nil {
						os.Rename(oldPath, exePath+"~")
					}
					continue
				}
				deferDeleteOldLauncher(exePath)
				os.Exit(exitErr.ExitCode())
			}
			fmt.Fprintf(os.Stderr, "Failed to run moombox: %v\n", err)
			deferDeleteOldLauncher(exePath)
			os.Exit(1)
		}
		// Normal exit (code 0)
		deferDeleteOldLauncher(exePath)
		os.Exit(0)
	}
}

// deferDeleteOldLauncher spawns a detached process to delete the .exe~ file
// after the launcher exits. On Windows, a running exe is locked — we can't
// delete ourselves, so we schedule a brief delay then delete.
func deferDeleteOldLauncher(exePath string) {
	oldPath := exePath + "~"
	if _, err := os.Stat(oldPath); err != nil {
		return // no .exe~ file
	}
	// ping localhost is used as a portable delay (timeout doesn't work in
	// non-interactive contexts). -n 3 ~ 2 seconds, enough for the launcher
	// to fully exit and release the file lock.
	cleanup := exec.Command("cmd", "/C",
		"ping", "127.0.0.1", "-n", "3", ">nul", "2>nul", "&",
		"del", "/f", "/q", oldPath, ">nul", "2>nul")
	cleanup.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	cleanup.Start() // fire and forget
}
