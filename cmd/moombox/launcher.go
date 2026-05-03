package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
)

// launchAndSupervise is the launcher/supervisor loop. It spawns moombox
// as a child process in the same console and waits. If the child exits
// with exitCodeRestart (config change or update applied), it respawns —
// picking up any new binary on disk. For any other exit code it
// propagates and exits.
//
// This keeps one stable parent holding the console connection so the
// child's BubbleTea properly restores terminal state, and avoids
// process chain buildup since the launcher always swaps to a fresh
// child rather than nesting.
//
// Platform-specific cleanup logic (handling the .exe~ orphan on
// Windows, no-op on Linux) lives in launcher_windows.go and
// launcher_unix.go.
func launchAndSupervise() {
	if err := acquireSingleInstanceLock(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		fmt.Fprintln(os.Stderr, "If you believe this is in error (the previous instance crashed), wait a few seconds and try again.")
		os.Exit(1)
	}
	defer releaseSingleInstanceLock()

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to determine executable path: %v\n", err)
		os.Exit(1)
	}

	// Clean up old launcher binary from a previous session (now unlocked).
	// Windows-only; no-op on Linux.
	cleanupOrphans(exePath)

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
					// Update applied: rename .old → ~ on Windows so the
					// .old name is free for the next update. No-op on Linux.
					handleUpdateRestart(exePath)
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
