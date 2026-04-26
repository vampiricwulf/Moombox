package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
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
	// Single-instance guard. Acquire the named mutex BEFORE any side-
	// effects so a second moombox.exe in the same Windows session fails
	// fast with a clear message instead of fighting for the port. Held by
	// the launcher (not the child) because the launcher is the long-lived
	// supervisor; the child inherits the launcher's "I am moombox" claim
	// transitively. A crashed launcher releases the mutex via kernel
	// handle cleanup — stale locks are impossible. Audit cmd-moombox.md
	// TD-5 / W-single-instance.
	if err := acquireSingleInstanceLock(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		fmt.Fprintln(os.Stderr, "If you believe this is in error (the previous instance crashed), wait a few seconds and try again — Windows releases the mutex automatically.")
		os.Exit(1)
	}
	defer releaseSingleInstanceLock()

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

// deferDeleteOldLauncher schedules deletion of the .exe~ file via a one-shot
// Windows scheduled task. On Windows a running exe is locked, so we can't
// delete ourselves directly — we schedule a task to fire ~10 seconds after
// the launcher exits.
//
// Audit reports/cmd-moombox.md TD-4 — replaces the previous
// `cmd /C ping ... & del` shell-out. schtasks.exe is more robust:
//
//   - Survives the launcher process tree being killed; the task runs in
//     its own elevated-by-default Task Scheduler context.
//   - Doesn't depend on cmd.exe quoting (the old form had multiple `&`
//     and `>nul` redirects that subtly broke if the path contained
//     special characters).
//   - Falls back gracefully if schtasks is unavailable (older or
//     locked-down Windows installs).
func deferDeleteOldLauncher(exePath string) {
	oldPath := exePath + "~"
	if _, err := os.Stat(oldPath); err != nil {
		return // no .exe~ file
	}

	// Compute a fire time ~10s in the future so the launcher has time to
	// exit and release the file lock before the task tries to delete.
	fireAt := time.Now().Add(10 * time.Second).Format("15:04:05")
	taskName := "MoomboxCleanup_" + filepath.Base(oldPath)

	cmd := `del /f /q "` + oldPath + `" & schtasks /delete /tn "` + taskName + `" /f`
	cleanup := exec.Command("schtasks", "/create", "/f",
		"/tn", taskName,
		"/tr", `cmd /c `+cmd,
		"/sc", "once",
		"/st", fireAt,
	)
	cleanup.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := cleanup.Start(); err != nil {
		// Fallback to the legacy ping-based approach so older Windows
		// versions that block schtasks (very locked-down corporate
		// images) still get the delayed cleanup.
		fallback := exec.Command("cmd", "/C",
			"ping", "127.0.0.1", "-n", "11", ">nul", "2>nul", "&",
			"del", "/f", "/q", oldPath, ">nul", "2>nul")
		fallback.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
		fallback.Start()
		return
	}
}
