package main

import (
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/updater"
)

// interruptedUpdate lays out what an update whose restart never ran leaves
// beside the binary: the release it installed at the plain name, the one it
// replaced at .old, and the breadcrumb naming the installed release.
func interruptedUpdate(t *testing.T) string {
	t.Helper()
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	writeReleases(t, map[string]string{
		exePath:                                "N+1",
		exePath + updater.OldSuffix:            "N",
		exePath + updater.PendingVersionSuffix: "v2.0.1",
	})
	return exePath
}

// A fresh start that finds the update applied and never booted arms its first
// boot with .old itself, and a quick death of that boot is rolled back
// automatically (owner ruling R3-10): the previous release is back at the
// plain name, the failed one kept at .failed, the breadcrumb names the failed
// release so the restored boot marks it skipped, and the marker says what
// happened. The restored binary is no update's first boot: the next fresh
// start does not arm again.
//
// Mutants: drop the armInterruptedUpdate call's effect (`return false` first)
// — the quick death takes the fail-fast arm and nothing is restored; arm with
// the ~ name (`artifact := exePath + updater.LauncherImageSuffix`) — the boot
// is judged past rollback and nothing is restored.
func TestAnInterruptedUpdatesQuickDeathIsRolledBack(t *testing.T) {
	exePath := interruptedUpdate(t)
	var boot postUpdateBoot
	if !boot.armInterruptedUpdate(exePath, "2.0.1") {
		t.Fatal("a fresh start with .old and a breadcrumb naming this release must arm the boot")
	}

	action, first := boot.judge(false, 2, 5*time.Second, false, 0)
	if action != childPostUpdateFailure || !first {
		t.Fatalf("a quick crash of the startup-armed boot = (%v, first %v), want a post-update failure", action, first)
	}
	var restored bool
	captureStderr(t, func() { restored = boot.recoverFailedBoot(exePath, 2) })
	if !restored {
		t.Fatal("recoverFailedBoot did not roll back to .old")
	}
	if !bytesEqualFile(t, exePath, "N") {
		t.Error("the plain name must hold the release the update replaced")
	}
	if !bytesEqualFile(t, exePath+updater.FailedBinarySuffix, "N+1") {
		t.Error("the failed release must be kept at .failed")
	}
	if !bytesEqualFile(t, exePath+updater.PendingVersionSuffix, "v2.0.1") {
		t.Error("the breadcrumb must still name the failed release, for the restored boot to skip it")
	}
	if _, err := os.Stat(exePath + updater.UpdateFailedMarkerSuffix); err != nil {
		t.Errorf("no rollback marker: %v", err)
	}
	if boot.armed {
		t.Error("the restored binary is no first boot: the record must be disarmed")
	}
	// The restored release starts again later, from a fresh launcher.
	var next postUpdateBoot
	if next.armInterruptedUpdate(exePath, "2.0.0") {
		t.Error("a fresh start of the rolled-back release must not arm: the breadcrumb names the release it replaced")
	}
}

// A startup-armed boot that proves itself is no longer protected: its
// milestone sweeps .old and resolves the breadcrumb, so a later crash, even a
// quick one, is a supervised crash that restores nothing, and the next fresh
// start finds nothing to arm.
//
// Mutants: arming with a file the milestone does not sweep (`artifact :=
// exePath`) — the crash after the sweep is routed to a rollback with nothing
// to restore; postUpdatePastRollback reporting false — the same.
func TestAnInterruptedUpdateThatBootsIsNotRolledBack(t *testing.T) {
	exePath := interruptedUpdate(t)
	var boot postUpdateBoot
	if !boot.armInterruptedUpdate(exePath, "2.0.1") {
		t.Fatal("the boot must be armed")
	}
	// The boot reaches its milestone: CleanupOldBinary, then the breadcrumb.
	for _, p := range []string{exePath + updater.OldSuffix, exePath + updater.PendingVersionSuffix} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	action, first := boot.judge(false, 2, 30*time.Second, false, 0)
	if action != childCrash || first {
		t.Errorf("a crash after the milestone = (%v, first %v), want a supervised crash", action, first)
	}
	if !bytesEqualFile(t, exePath, "N+1") {
		t.Error("a boot past its milestone must keep the release it runs")
	}
	var next postUpdateBoot
	if next.armInterruptedUpdate(exePath, "2.0.1") {
		t.Error("with .old and the breadcrumb swept there is nothing to arm")
	}
}

// Only an update applied and never booted arms a fresh start: the breadcrumb
// must name exactly this binary's release, and .old must be there. Anything
// else is an ordinary fresh launch whose quick failure fails fast.
//
// Mutants: drop the version comparison — a restored binary, or a binary
// swapped by hand beside a stale breadcrumb, arms; drop the .old stat — a
// boot is armed with nothing to roll back to.
func TestAFreshStartArmsOnlyAnUpdateNeverBooted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"no breadcrumb", map[string]string{updater.OldSuffix: "N"}},
		{"breadcrumb naming another release", map[string]string{updater.OldSuffix: "N", updater.PendingVersionSuffix: "v2.0.2"}},
		{"empty breadcrumb", map[string]string{updater.OldSuffix: "N", updater.PendingVersionSuffix: ""}},
		{"no .old", map[string]string{updater.PendingVersionSuffix: "v2.0.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath := filepath.Join(t.TempDir(), "moombox.exe")
			files := map[string]string{exePath: "N+1"}
			for suffix, body := range tc.files {
				files[exePath+suffix] = body
			}
			writeReleases(t, files)
			var boot postUpdateBoot
			if boot.armInterruptedUpdate(exePath, "2.0.1") || boot.armed {
				t.Fatalf("armed = true, want an ordinary fresh launch")
			}
			if action, _ := boot.judge(false, 2, 5*time.Second, false, 0); action != childPropagate {
				t.Errorf("a quick death of an unarmed fresh launch = %v, want fail-fast propagation", action)
			}
		})
	}
}

// A stop during the startup-armed boot is not a failed update, whatever the
// code the child exits with — a restart it had pending included — and an
// exit 3 (a port in use, a config it cannot load) is the environment, not the
// release. Both leave .old and the plain name as they are and write no
// rollback marker, and the next fresh start arms again.
//
// Mutants: armInterruptedUpdate renaming .old to ~ as handleUpdateRestart does
// on Windows — the ~ is deleted on the way out and .old is gone; the stop
// checked after the post-update rule in classifyChildExit — the stop is
// rolled back.
func TestAStopDuringAnInterruptedUpdatesBootKeepsOld(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		terminating bool
	}{
		{"stop over a pending restart", exitCodeRestart, true},
		{"stop the child exited 1 for", 1, true},
		{"startup error", exitCodeStartupError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath := interruptedUpdate(t)
			var boot postUpdateBoot
			if !boot.armInterruptedUpdate(exePath, "2.0.1") {
				t.Fatal("the boot must be armed")
			}
			action, _ := boot.judge(false, tc.code, 5*time.Second, tc.terminating, 0)
			switch action {
			case childPropagate:
				deferDeleteOldLauncher(exePath)
			case childPostUpdateFailure:
				var restored bool
				captureStderr(t, func() { restored = boot.recoverFailedBoot(exePath, tc.code) })
				if restored {
					t.Fatal("a startup error must not be rolled back")
				}
			default:
				t.Fatalf("action = %v, want the launcher to exit", action)
			}
			if !bytesEqualFile(t, exePath+updater.OldSuffix, "N") {
				t.Error(".old must stay where it is")
			}
			if !bytesEqualFile(t, exePath, "N+1") {
				t.Error("the installed release must stay at the plain name")
			}
			if _, err := os.Stat(exePath + updater.UpdateFailedMarkerSuffix); err == nil {
				t.Error("no rollback marker may be written")
			}
			var next postUpdateBoot
			if !next.armInterruptedUpdate(exePath, "2.0.1") {
				t.Error("the next fresh start must arm the same boot again")
			}
		})
	}
}

// launchAndSupervise arms the record from disk once, before its first child:
// the helper above is what the tests drive, and this pins that the loop calls
// it with the running binary's path and version.
//
// Mutants: the call deleted, moved into the loop (every respawn, the restored
// binary's included, would be re-armed from disk), or passed another version.
func TestTheLauncherArmsAnInterruptedUpdateBeforeItsFirstChild(t *testing.T) {
	const want = "boot.armInterruptedUpdate(exePath, version)"
	_, body := funcBody(t, "launcher.go", "launchAndSupervise")
	callAt, loopAt, calls := -1, -1, 0
	for i, st := range body.List {
		if _, ok := st.(*ast.ForStmt); ok && loopAt < 0 {
			loopAt = i
		}
		ast.Inspect(st, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "armInterruptedUpdate" {
				calls++
				callAt = i
				if got := types.ExprString(call); got != want {
					t.Errorf("launchAndSupervise calls %s, want %s", got, want)
				}
			}
			return true
		})
	}
	if calls != 1 || loopAt < 0 || callAt >= loopAt {
		t.Fatalf("launchAndSupervise calls armInterruptedUpdate %d times (last at statement %d, the loop at %d), want once, before the loop", calls, callAt, loopAt)
	}
}
