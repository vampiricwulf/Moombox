package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
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

	// Forward SIGTERM to the child instead of dying with the default
	// disposition. The single-instance lock lives in THIS process: if a
	// plain `kill <launcher-pid>` (the PID a user sees for the foreground
	// process) killed only the launcher, the child would keep running —
	// and writing to the database — while the lock is released, letting a
	// second instance start against the same DB. Windows has no SIGTERM
	// delivery for console apps, so the fallback there is Kill; outright
	// TerminateProcess on the launcher remains uninterceptable.
	var child atomic.Pointer[os.Process]
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "panic in launcher signal forwarder: %v\n", r)
			}
		}()
		for range sigCh {
			if p := child.Load(); p != nil {
				if err := p.Signal(syscall.SIGTERM); err != nil {
					_ = p.Kill()
				}
				continue
			}
			// No child registered yet. Usually the genuine "no child" state
			// (before the first Start, or in the respawn window), but it can
			// also be the microsecond gap between cmd.Start() and
			// child.Store() — exiting there would orphan the just-started
			// child. Spin briefly to catch that registration first.
			signaled := false
			for i := 0; i < 100; i++ {
				if p := child.Load(); p != nil {
					if err := p.Signal(syscall.SIGTERM); err != nil {
						_ = p.Kill()
					}
					signaled = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if signaled {
				continue
			}
			// Genuinely no child after the grace window. signal.Notify removed
			// the default terminate disposition, so without this exit the
			// SIGTERM would be swallowed entirely and the launcher would
			// respawn as if nothing happened. Process death releases the lock.
			os.Exit(143) // 128 + SIGTERM
		}
	}()

	for {
		cmd := exec.Command(exePath, os.Args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "_MOOMBOX_CHILD=1")

		if startErr := cmd.Start(); startErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to run moombox: %v\n", startErr)
			deferDeleteOldLauncher(exePath)
			os.Exit(1)
		}
		child.Store(cmd.Process)
		err := cmd.Wait()
		child.Store(nil)

		if err != nil {
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
