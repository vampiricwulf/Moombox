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
	// starting is true while the main loop is mid-launch (cmd.Start in flight,
	// child not yet stored). The forwarder uses it to distinguish "genuinely no
	// child" from "child being started" without a wall-clock guess, so a slow
	// cmd.Start (e.g. AV scanning a fresh update binary) can't race the spin.
	var starting atomic.Bool
	// terminating records that the launcher itself initiated the child's
	// death (SIGTERM forwarding). Crash supervision must never respawn a
	// stop the user/system asked for — and exit-code heuristics can't tell
	// (Windows p.Kill yields exit 1, the same as many crashes).
	var terminating atomic.Bool
	forward := func(p *os.Process) {
		terminating.Store(true)
		if err := p.Signal(syscall.SIGTERM); err != nil {
			_ = p.Kill()
		}
	}
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
				forward(p)
				continue
			}
			// No child registered. If the main loop is mid-launch, wait for the
			// child to register (or the launch to fail) rather than exiting and
			// orphaning a just-started process. child.Store happens before
			// starting clears, so once starting is false the child is visible.
			for starting.Load() && child.Load() == nil {
				time.Sleep(time.Millisecond)
			}
			if p := child.Load(); p != nil {
				forward(p)
				continue
			}
			// Genuinely no child (before the first Start, the respawn window,
			// or a failed Start). signal.Notify removed the default terminate
			// disposition, so without this exit the SIGTERM would be swallowed
			// and the launcher would respawn as if nothing happened. Process
			// death releases the instance lock.
			os.Exit(143) // 128 + SIGTERM
		}
	}()

	// firstAfterUpdate marks the next spawn as the first boot of a freshly
	// applied update (set when an exit-42 restart found a .old binary —
	// config restarts never create one). If that first boot fails quickly,
	// the launcher PRESERVES the rollback artifact instead of deleting it
	// on the way out, and leaves recovery instructions. One-shot: consumed
	// by the next child exit regardless of outcome.
	firstAfterUpdate := false
	// Crash supervision state: consecutive abnormal exits (reset whenever a
	// child survives launcherHealthyWindow) and the exit code that triggered
	// the pending respawn (passed to the child so it can notify the operator
	// that a recovery happened).
	consecutiveCrashes := 0
	crashRespawnCode := 0
	var spawnedAt time.Time

	// Windows: a kill-on-close Job Object ties every child (and ITS
	// children, e.g. ffmpeg) to the launcher's lifetime, so hard-killing
	// the launcher PID — which releases the single-instance lock — can no
	// longer leave an orphaned recorder running unlocked. One handle for
	// the launcher's whole life. Linux uses Pdeathsig instead (set per
	// child in configureLauncherChild). Both are best-effort: failure logs
	// and supervision continues.
	childJob := newLauncherJob()

	for {
		cmd := exec.Command(exePath, os.Args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		env := append(os.Environ(), "_MOOMBOX_CHILD=1")
		// wasRespawn: this spawn exists because the previous child crashed.
		// A quick death of a respawned child must route into the crash
		// COUNTER (backoff + cutoff), not the fail-fast branch — otherwise
		// the counter can never pass 1 and the cutoff is dead code.
		wasRespawn := crashRespawnCode != 0
		if wasRespawn {
			// Tell the respawned child WHY it's being started so it can
			// send a crash_recovered notification once services are up.
			env = append(env, fmt.Sprintf("_MOOMBOX_CRASH_RESPAWN=%d", crashRespawnCode))
			crashRespawnCode = 0
		}
		cmd.Env = env
		configureLauncherChild(cmd)

		// Reset the stop-intent latch per spawn: a SIGTERM that raced a
		// previous child's restart (child exited 42 anyway because its
		// restart was already committed) must not permanently disable
		// crash supervision and rollback preservation for every future
		// child. A genuine stop re-sets it via the forwarder.
		terminating.Store(false)

		starting.Store(true)
		spawnedAt = time.Now()
		if startErr := cmd.Start(); startErr != nil {
			starting.Store(false)
			fmt.Fprintf(os.Stderr, "Failed to run moombox: %v\n", startErr)
			if firstAfterUpdate {
				// The freshly-updated binary would not even start — restore
				// the previous version and respawn it; fall back to keeping
				// it on disk with manual instructions if that isn't possible.
				if attemptAutoRollback(exePath, -1) {
					firstAfterUpdate = false
					continue
				}
				preserveUpdateRollback(exePath, -1)
				os.Exit(1)
			}
			deferDeleteOldLauncher(exePath)
			os.Exit(1)
		}
		assignLauncherJob(childJob, cmd.Process)
		child.Store(cmd.Process)
		starting.Store(false)
		err := cmd.Wait()
		child.Store(nil)

		ranFor := time.Since(spawnedAt)
		wasFirstAfterUpdate := firstAfterUpdate
		firstAfterUpdate = false
		// A healthy run ends the crash streak. (Quick deaths of RESPAWNED
		// children deliberately don't reset — they're the streak.)
		if ranFor >= launcherHealthyWindow {
			consecutiveCrashes = 0
		}

		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				fmt.Fprintf(os.Stderr, "Failed to run moombox: %v\n", err)
				deferDeleteOldLauncher(exePath)
				os.Exit(1)
			}
		}

		switch {
		case code == exitCodeRestart:
			// Update applied: rename .old → ~ on Windows so the .old name
			// is free for the next update (returns whether a .old existed,
			// i.e. binary update vs config restart). Linux just reports.
			// A config restart INSIDE a still-unproven update's window
			// carries the one-shot forward — otherwise it would silently
			// drop rollback preservation for a binary that never proved
			// itself.
			firstAfterUpdate = handleUpdateRestart(exePath) ||
				(wasFirstAfterUpdate && ranFor < postUpdateFailureWindow)
			continue

		case code == 0:
			// Deliberate quit — clean up as always.
			deferDeleteOldLauncher(exePath)
			os.Exit(0)

		case terminating.Load():
			// The launcher forwarded a stop signal — the user/system asked
			// for this exit. Never respawn it AND never treat it as a
			// failed update (checked before the preserve branch: stopping
			// the service right after updating must not leave a scary
			// "update failed" marker), whatever the code.
			deferDeleteOldLauncher(exePath)
			os.Exit(code)

		case wasFirstAfterUpdate && code != 0 && ranFor < postUpdateFailureWindow:
			// First boot of a fresh update failed almost immediately —
			// retrying a binary that just proved broken buys nothing, so
			// this takes priority over crash-respawn. Roll back to the
			// preserved previous binary and respawn it as a KNOWN-GOOD
			// fresh launch: not a crash respawn (no _MOOMBOX_CRASH_RESPAWN
			// — the restored version didn't crash), and with a clean crash
			// budget (any pre-update streak belonged to different
			// circumstances). firstAfterUpdate is already false, so a quick
			// death of the RESTORED binary hits the normal fail-fast path
			// — no rollback ping-pong is possible. When the artifact is
			// gone (the boot reached the milestone sweep before dying) or
			// the restore fails, fall back to preserving what's left with
			// manual instructions.
			if attemptAutoRollback(exePath, code) {
				crashRespawnCode = 0
				consecutiveCrashes = 0
				continue
			}
			preserveUpdateRollback(exePath, code)
			os.Exit(code)

		case code == 130 || code == 143:
			// Unix user-interrupt conventions (128+SIGINT / 128+SIGTERM):
			// user intent, propagate as before.
			deferDeleteOldLauncher(exePath)
			os.Exit(code)

		case ranFor < launcherHealthyWindow && !wasRespawn && consecutiveCrashes == 0:
			// Supervision arms only after a child proves it can run: a
			// deterministic startup failure (bad config exit 1, bad flags
			// exit 2, refused DB migration) on a FRESH launch must fail
			// fast and visibly — exactly today's behavior — not crash-loop
			// against the same wall. Quick deaths of respawned children
			// fall through to the counter below instead: they're what the
			// backoff + cutoff exist for, and routing them here would cap
			// supervision at a single retry forever. The first post-update
			// window above already handled the fresh-update flavor.
			deferDeleteOldLauncher(exePath)
			os.Exit(code)

		default:
			// A previously-healthy child died abnormally (panic exit 2,
			// OOM/AV kill, signal death) — or a respawned child crashed
			// again mid-streak. For a 24/7 unattended archiver a
			// dead-until-noticed daemon is the worst outcome — respawn with
			// backoff, bounded by the crash-loop cutoff.
			consecutiveCrashes++
			if consecutiveCrashes > maxConsecutiveCrashes {
				fmt.Fprintf(os.Stderr,
					"moombox crashed %d times in a row (last exit code %d) — giving up; check the logs\n",
					consecutiveCrashes, code)
				deferDeleteOldLauncher(exePath)
				os.Exit(code)
			}
			backoff := crashBackoff(consecutiveCrashes)
			fmt.Fprintf(os.Stderr,
				"moombox exited abnormally (code %d) — restarting in %s (attempt %d/%d)\n",
				code, backoff, consecutiveCrashes, maxConsecutiveCrashes)
			// A SIGTERM during this sleep finds no child and exits the
			// launcher via the forwarder's no-child branch — the sleep
			// doesn't trap the stop.
			time.Sleep(backoff)
			crashRespawnCode = code
			continue
		}
	}
}

// launcherHealthyWindow is how long a child must run before crash
// supervision arms for its exit (and before the consecutive-crash counter
// resets). Deterministic startup failures die well inside it and keep
// today's fail-fast behavior.
const launcherHealthyWindow = 60 * time.Second

// maxConsecutiveCrashes bounds the respawn loop: after this many abnormal
// exits without a healthy 60s run in between, the launcher gives up and
// propagates the last exit code.
const maxConsecutiveCrashes = 5

// crashBackoff returns the pre-respawn delay for the nth consecutive crash:
// 1s, 2s, 4s, 8s, 16s (capped at 60s should the cutoff ever be raised).
func crashBackoff(n int) time.Duration {
	d := time.Second << (n - 1)
	if d > time.Minute {
		return time.Minute
	}
	return d
}

// postUpdateFailureWindow bounds how soon after spawning the first
// post-update child an abnormal exit is treated as "the update is broken"
// (preserve the rollback binary) rather than an ordinary crash later in
// life. Generous enough for slow AV-scanned first boots; a child that ran
// past it has proven the binary starts.
const postUpdateFailureWindow = 2 * time.Minute

// attemptAutoRollback restores the previous binary after a failed first
// post-update boot: the broken binary at exePath is removed (it is
// bit-identical to the published GitHub asset, so nothing diagnostic is
// lost), the preserved rollback artifact is renamed back to the plain
// name, and a marker documents what happened — the next child boot
// announces it, and (via the .update-pending breadcrumb ApplyUpdate
// writes) marks the failed version skipped so automatic checks stop
// offering a release that just proved broken.
//
// Returns false without touching anything when no rollback artifact
// exists (the boot survived long enough to reach the milestone sweep
// before dying) and on the remove failure path; the caller then falls
// back to preserveUpdateRollback's manual instructions. A rename failure
// AFTER the remove succeeded is the one unrecoverable shape (the plain
// name is empty) — the preserve fallback's instructions still point at
// the intact artifact, so recovery stays one manual rename.
//
// Windows note: the artifact (the ~ file) is this launcher's own mapped
// image — renaming a mapped image is legal (it is how the update swap
// renamed it to ~ in the first place), and afterwards the launcher runs
// from the file at the plain name, exactly like a fresh start.
func attemptAutoRollback(exePath string, exitCode int) bool {
	backup := rollbackArtifactPath(exePath)
	if _, err := os.Stat(backup); err != nil {
		return false
	}
	// os.Rename cannot replace an existing file on Windows, so the broken
	// binary must be removed first. The child has exited (its image is
	// unmapped); brief retries ride out an AV scanner still holding the
	// freshly-downloaded file.
	var rmErr error
	for range 3 {
		if rmErr = os.Remove(exePath); rmErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if rmErr != nil {
		fmt.Fprintf(os.Stderr, "auto-rollback: could not remove failed binary: %v\n", rmErr)
		return false
	}
	if err := os.Rename(backup, exePath); err != nil {
		fmt.Fprintf(os.Stderr, "auto-rollback: could not restore previous binary: %v\n", err)
		return false
	}
	writeAutoRollbackMarker(exePath, exitCode)
	return true
}

// writeAutoRollbackMarker records a completed automatic rollback in the
// same .update-failed marker file the manual-recovery path uses — existing
// children (2.7.0+) already announce this marker's content on boot, and
// pending-breadcrumb-aware children additionally mark the failed version
// skipped when they see it.
func writeAutoRollbackMarker(exePath string, exitCode int) {
	msg := fmt.Sprintf(
		"Moombox: the first launch after a self-update failed (exit code %d) at %s.\n"+
			"Moombox AUTOMATICALLY ROLLED BACK to the previous binary and restarted it.\n"+
			"The failed release was removed from disk (it is re-downloadable from GitHub).\n"+
			"Automatic update checks will skip the failed version where supported; use a\n"+
			"manual \"Check for updates\" to retry it deliberately.\n"+
			"Delete this marker file once acknowledged.\n",
		exitCode, time.Now().Format(time.RFC3339))
	markerPath := exePath + ".update-failed"
	if err := os.WriteFile(markerPath, []byte(msg), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write %s: %v\n", markerPath, err)
	}
	fmt.Fprint(os.Stderr, "\n"+msg)
}

// preserveUpdateRollback runs when the first boot of a freshly-applied
// update fails AND automatic rollback was not possible (artifact already
// swept, or the restore itself failed): it deliberately SKIPS the ~-file
// cleanup (Windows; on Linux the .old survives because the child never
// reached its post-milestone sweep), writes a recovery-instruction marker
// next to the binary, and prints the same instructions to stderr.
// Recovery is one file rename instead of a GitHub re-download.
func preserveUpdateRollback(exePath string, exitCode int) {
	backup := rollbackArtifactPath(exePath)
	msg := fmt.Sprintf(
		"Moombox: the first launch after a self-update failed (exit code %d) at %s.\n"+
			"The previous version's binary should still be present at:\n  %s\n"+
			"To roll back: replace %s with that file and start Moombox again.\n"+
			"(If the backup is missing — the boot got far enough to sweep it —\n"+
			"re-download the previous release from GitHub instead.)\n"+
			"Delete this marker file once resolved.\n",
		exitCode, time.Now().Format(time.RFC3339), backup, exePath)
	markerPath := exePath + ".update-failed"
	if err := os.WriteFile(markerPath, []byte(msg), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write %s: %v\n", markerPath, err)
	}
	fmt.Fprint(os.Stderr, "\n"+msg)
}
