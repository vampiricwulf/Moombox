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
		// Staleness handshake: the supervisor process keeps running the
		// binary it started from across every in-place update, so its
		// version diverges from the child's until a full stop/start. Tell
		// the child which version this launcher is; the child logs a light
		// INFO suggestion when they differ (helpers.go
		// launcherStalenessNote). Absent for pre-handshake launchers, which
		// the child treats as "an earlier version".
		env = append(env, "_MOOMBOX_LAUNCHER_VERSION="+version)
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

		switch classifyChildExit(code, ranFor, wasFirstAfterUpdate, terminating.Load(), wasRespawn, consecutiveCrashes) {
		case childRestart:
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

		case childPostUpdateFailure:
			// Roll back to the preserved previous binary and respawn it as a
			// KNOWN-GOOD fresh launch: not a crash respawn (no
			// _MOOMBOX_CRASH_RESPAWN — the restored version didn't crash),
			// and with a clean crash budget (any pre-update streak belonged
			// to different circumstances). firstAfterUpdate is already false,
			// so a quick death of the RESTORED binary hits the normal
			// fail-fast path — no rollback ping-pong is possible. When the
			// artifact is gone (the boot reached the milestone sweep before
			// dying) or the restore fails, fall back to preserving what's
			// left with manual instructions.
			//
			// A deterministic startup error (exitCodeStartupError) skips the
			// rollback entirely: the environment, not the binary, is what
			// failed. The artifact is still PRESERVED with instructions —
			// this branch does not call deferDeleteOldLauncher — so a manual
			// rollback stays one rename away, and because the next boot runs
			// the SAME version the .update-pending breadcrumb names,
			// shouldSkipPendingVersion is false and the release is not
			// marked skipped.
			if classifyPostUpdateExit(code) == postUpdateRollback && attemptAutoRollback(exePath, code) {
				crashRespawnCode = 0
				consecutiveCrashes = 0
				continue
			}
			preserveUpdateRollback(exePath, code)
			os.Exit(code)

		case childPropagate:
			deferDeleteOldLauncher(exePath)
			os.Exit(code)

		default: // childCrash
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

// childExitAction is what the launcher does with one child exit.
type childExitAction int

const (
	childRestart           childExitAction = iota // respawn: an update or config restart (exitCodeRestart)
	childPropagate                                // stop supervising and exit with the child's code
	childPostUpdateFailure                        // a fresh update's first boot failed: roll back or preserve
	childCrash                                    // count the crash and respawn with backoff
)

// classifyChildExit decides what one child exit means. Pure, so the order of
// the rules below — which is the whole policy — is testable.
func classifyChildExit(code int, ranFor time.Duration, wasFirstAfterUpdate, terminating, wasRespawn bool, consecutiveCrashes int) childExitAction {
	switch {
	case code == exitCodeRestart:
		return childRestart

	case code == 0:
		// Deliberate quit — clean up as always.
		return childPropagate

	case terminating:
		// The launcher forwarded a stop signal — the user/system asked for
		// this exit. Never respawn it AND never treat it as a failed update
		// (checked before the post-update rule: stopping the service right
		// after updating must not leave a scary "update failed" marker),
		// whatever the code.
		return childPropagate

	case wasFirstAfterUpdate && code != 0 &&
		(ranFor < postUpdateFailureWindow || code == exitCodeStartupError):
		// First boot of a fresh update failed almost immediately — retrying
		// a binary that just proved broken buys nothing, so this takes
		// priority over crash-respawn. A startup error is routed here
		// whatever its timing: the child waits for a keypress before exiting
		// 3, so how long it "ran" is how long the operator took to press
		// Enter, which must not decide whether the rollback artifact is kept
		// (CORE-23).
		return childPostUpdateFailure

	case code == 130 || code == 143:
		// Unix user-interrupt conventions (128+SIGINT / 128+SIGTERM): user
		// intent, propagate as before.
		return childPropagate

	case code == exitCodeStartupError:
		// A deterministic startup error: the environment is wrong, and a
		// respawn hits the same wall. Propagated whatever the timing, for
		// the keypress reason above — the healthy-window rule below used to
		// decide it, so an operator who took over a minute to press Enter
		// was put through five identical prompts and a "crashed 5 times"
		// verdict.
		return childPropagate

	case ranFor < launcherHealthyWindow && !wasRespawn && consecutiveCrashes == 0:
		// Supervision arms only after a child proves it can run: a
		// deterministic startup failure (bad config exit 1, bad flags exit
		// 2, refused DB migration) on a FRESH launch must fail fast and
		// visibly — exactly today's behavior — not crash-loop against the
		// same wall. Quick deaths of respawned children fall through to the
		// counter instead: they're what the backoff + cutoff exist for, and
		// routing them here would cap supervision at a single retry forever.
		return childPropagate

	default:
		// A previously-healthy child died abnormally (panic exit 2, OOM/AV
		// kill, signal death) — or a respawned child crashed again
		// mid-streak. For a 24/7 unattended archiver a dead-until-noticed
		// daemon is the worst outcome — respawn with backoff, bounded by the
		// crash-loop cutoff.
		return childCrash
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

// postUpdateVerdict is what the launcher does with a non-zero exit from the
// first boot of a freshly-applied update.
type postUpdateVerdict int

const (
	// postUpdateRollback restores the previous binary and respawns it.
	postUpdateRollback postUpdateVerdict = iota
	// postUpdatePreserve keeps the rollback artifact on disk with written
	// instructions and propagates the exit code without respawning.
	postUpdatePreserve
)

// classifyPostUpdateExit decides how a failed first-post-update boot is
// treated. Only exitCodeStartupError is spared the rollback: it names a
// deterministic environment failure the new binary is not responsible for,
// and rolling back would hide the real cause behind a version downgrade
// while marking a perfectly good release skipped (CORE-23).
func classifyPostUpdateExit(code int) postUpdateVerdict {
	if code == exitCodeStartupError {
		return postUpdatePreserve
	}
	return postUpdateRollback
}

// failedBinarySuffix names the binary a failed first-post-update boot leaves
// behind. The rollback used to DELETE it on the grounds that it is
// bit-identical to the published GitHub asset — but when the failure is a
// refused DB downgrade, the older binary the rollback restores then prints
// "restore the newer binary … if still present" about a file the rollback
// had just removed, and the install is down until a manual re-download
// (CORE-1). Keeping it costs ~90 MB until the next healthy boot sweeps it
// (internal/updater.CleanupOldBinary).
const failedBinarySuffix = ".failed"

// attemptAutoRollback restores the previous binary after a failed first
// post-update boot: the broken binary at exePath is KEPT, renamed to
// <exe>.failed so the DB downgrade guard's "restore the newer binary"
// advice names a file that exists, the preserved rollback artifact is
// renamed back to the plain name, and a marker documents what happened
// — the next child boot announces it, and (via the .update-pending
// breadcrumb ApplyUpdate writes) marks the failed version skipped so
// automatic checks stop offering a release that just proved broken.
//
// Returns false without touching anything when no rollback artifact
// exists (the boot survived long enough to reach the milestone sweep
// before dying) and on the move-aside failure path; the caller then
// falls back to preserveUpdateRollback's manual instructions. A restore
// failure AFTER the move aside succeeded is the one unrecoverable shape
// (the plain name is empty) — the preserve fallback's instructions still
// point at the intact artifact, so recovery stays one manual rename.
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
	// Move the broken binary aside first, then rename over the freed name. NOT
	// because os.Rename cannot replace an existing file on Windows — it is
	// MoveFileEx with MOVEFILE_REPLACE_EXISTING and ordinarily would — but
	// because replacing a destination needs delete access to it, which any
	// process holding that file without FILE_SHARE_DELETE denies. Taking the
	// hit on moving it aside instead makes it RETRYABLE (a rename needs the same
	// DELETE access to the source that a remove does): the child has exited (its
	// image is unmapped), so the holder is an AV scanner still on the
	// freshly-downloaded file, and brief retries ride it out. A move that
	// never succeeds returns false below, which is the failure path
	// preserveUpdateRollback's manual instructions cover. A pre-existing
	// .failed from an earlier failed update is replaced, not an obstacle:
	// MOVEFILE_REPLACE_EXISTING on Windows, replace semantics on POSIX.
	failedPath := exePath + failedBinarySuffix
	var mvErr error
	for range 3 {
		if mvErr = os.Rename(exePath, failedPath); mvErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	// The rename needs DELETE access to the destination as well as the
	// source, so a stale .failed from an EARLIER failed update — the
	// DB-downgrade strand leaves one until a healthy boot sweeps it — held
	// open by a scanner without FILE_SHARE_DELETE fails all three attempts
	// where the os.Remove(exePath) this replaced used to succeed. Keeping the
	// broken binary is worth three retries, never the rollback itself: fall
	// back to removing it once, which is exactly the old behaviour, so this
	// can only ever succeed where today's code already does.
	kept := mvErr == nil
	if mvErr != nil {
		if rmErr := os.Remove(exePath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "auto-rollback: could not move failed binary aside: %v\n", mvErr)
			return false
		}
		fmt.Fprintf(os.Stderr,
			"auto-rollback: could not keep the failed binary aside (%v); removed it instead\n", mvErr)
	}
	if err := os.Rename(backup, exePath); err != nil {
		fmt.Fprintf(os.Stderr, "auto-rollback: could not restore previous binary: %v\n", err)
		return false
	}
	writeAutoRollbackMarker(exePath, exitCode, kept)
	return true
}

// writeAutoRollbackMarker records a completed automatic rollback in the
// same .update-failed marker file the manual-recovery path uses — existing
// children (2.7.0+) already announce this marker's content on boot, and
// pending-breadcrumb-aware children additionally mark the failed version
// skipped when they see it.
//
// kept says whether the broken binary really is at <exe>.failed. It normally
// is (that is CORE-1's whole point), but the rollback falls back to removing
// it when the move aside cannot be done, and a marker that names a file which
// is not there sends an operator hunting for it.
func writeAutoRollbackMarker(exePath string, exitCode int, kept bool) {
	fate := fmt.Sprintf(
		"The failed release was KEPT at:\n  %s\n"+
			"(If the restored binary refuses to start — a database the newer version\n"+
			"already migrated is one such case — rename that file back over %s.)\n",
		exePath+failedBinarySuffix, exePath)
	if !kept {
		fate = fmt.Sprintf(
			"The failed release could not be kept aside and was REMOVED.\n"+
				"(If the restored binary refuses to start — a database the newer version\n"+
				"already migrated is one such case — re-download that release from GitHub\n"+
				"and put it back at %s.)\n",
			exePath)
	}
	msg := fmt.Sprintf(
		"Moombox: the first launch after a self-update failed (exit code %d) at %s.\n"+
			"Moombox AUTOMATICALLY ROLLED BACK to the previous binary and restarted it.\n"+
			"%s"+
			"Automatic update checks will skip the failed version where supported; use a\n"+
			"manual \"Check for updates\" to retry it deliberately.\n"+
			"Delete this marker file once acknowledged.\n",
		exitCode, time.Now().Format(time.RFC3339), fate)
	markerPath := exePath + ".update-failed"
	if err := os.WriteFile(markerPath, []byte(msg), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write %s: %v\n", markerPath, err)
	}
	fmt.Fprint(os.Stderr, "\n"+msg)
}

// preserveUpdateRollback runs when the first boot of a freshly-applied
// update fails AND automatic rollback was not possible (artifact already
// swept, the restore itself failed, or — exit 3 — the rollback was never
// attempted): it deliberately SKIPS the ~-file cleanup (Windows; on Linux the
// .old survives because the child never reached its post-milestone sweep),
// writes a recovery-instruction marker next to the binary, and prints the
// same instructions to stderr. Recovery is one file rename instead of a
// GitHub re-download.
//
// The one exception is exitCodeStartupError, which gets the honest notice and
// NO marker — see the branch below.
func preserveUpdateRollback(exePath string, exitCode int) {
	backup := rollbackArtifactPath(exePath)
	// A deterministic startup failure is not a failed update, and the marker
	// is what the NEXT boot announces as "Previous Update Failed" — with
	// instructions whose first step is to replace the binary. An operator who
	// follows them on a port conflict lands on the older release and gets the
	// good one marked skipped. Same preservation (this path still skips the
	// artifact cleanup, so a manual rollback stays one rename away), honest
	// notice, no marker (CORE-23).
	if classifyPostUpdateExit(exitCode) == postUpdatePreserve {
		fmt.Fprintf(os.Stderr,
			"\nMoombox could not start after a self-update (exit code %d): a startup/environment\n"+
				"failure — a config it cannot load, a port it cannot bind, a database it refuses —\n"+
				"not a broken release. Fix the cause and start Moombox again.\n"+
				"The previous version's binary is still at:\n  %s\n"+
				"Rolling back is not the fix and would make automatic checks skip this release.\n",
			exitCode, backup)
		return
	}
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
