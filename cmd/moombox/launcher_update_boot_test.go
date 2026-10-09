package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeReleases writes each path with its body, the stand-in for a release.
func writeReleases(t *testing.T, files map[string]string) {
	t.Helper()
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// The second update of one Windows launcher lifetime, walked through the
// record the launcher loop keeps. The first restart names ~ (its rename of
// .old succeeded, and ~ has been the launcher's image since); the second
// names .old (its rename over that image failed); that boot's milestone sweeps
// .old and leaves ~, two versions back; then the boot crashes inside the
// window. Judged by the names on disk at the exit, ~ stood in for the swept
// .old and the crash was rolled back to it — N+2 to N, with N+1 gone. Judged by
// the artifact the second restart recorded, the boot is past rollback and its
// crash is supervised. The restart artifacts are handleUpdateRestart's Windows
// answers, so this runs on every platform; TestASecondUpdatesSweptOldIsPastRollback
// walks the same steps through the real Windows handleUpdateRestart.
//
// Mutants: judge handing judgeChildExit the at-exit lookup instead of the
// record (`func() string { if _, err := os.Stat(b.artifact); err == nil {
// return b.artifact }; return strings.TrimSuffix(b.artifact, ".old") + "~"
// }()`, rollbackArtifactPath's old chain) — the crash is routed to a rollback;
// restarted arming without recording (`b.armed = true; _ = artifact`) — the
// record never names the update's artifact; judge leaving the one-shot armed
// (drop `b.armed = false`) — the boot is still armed after its exit.
func TestASecondUpdateIsJudgedByTheArtifactItsRestartRecorded(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	var boot postUpdateBoot

	// Update 1 (N → N+1): the restart renamed .old to ~.
	writeReleases(t, map[string]string{exePath: "N+1", exePath + "~": "N"})
	boot.restarted(exePath+"~", false, 0)
	if !boot.armed || boot.artifact != exePath+"~" {
		t.Fatalf("after the first update's restart the record = %+v, want armed with its ~", boot)
	}

	// The N+1 boot proves itself, then exits 42 to apply update 2.
	action, first := boot.judge(false, exitCodeRestart, 10*time.Minute, false, 0)
	if action != childRestart || !first {
		t.Fatalf("the first boot's update restart = (%v, first %v), want a restart of the first boot", action, first)
	}

	// Update 2 (N+1 → N+2): the rename over ~ failed, so .old is the artifact.
	writeReleases(t, map[string]string{exePath: "N+2", exePath + ".old": "N+1"})
	boot.restarted(exePath+".old", first, 10*time.Minute)
	if !boot.armed || boot.artifact != exePath+".old" {
		t.Fatalf("after the second update's restart the record = %+v, want armed with its .old", boot)
	}

	// The N+2 boot reaches its milestone, whose CleanupOldBinary sweeps .old
	// and cannot delete ~. Then it crashes.
	if err := os.Remove(exePath + ".old"); err != nil {
		t.Fatal(err)
	}
	action, first = boot.judge(false, 2, 30*time.Second, false, 0)
	if action != childCrash || first {
		t.Errorf("a quick crash after the second update's .old was swept = (%v, first %v), want a supervised crash — "+
			"~ is two versions back, not this update's artifact", action, first)
	}
	if boot.armed {
		t.Error("judge must consume the one-shot: the respawned child is no first boot")
	}
}

// A first post-update boot that dies quickly is rolled back to the artifact
// its restart recorded, by both of the loop's paths: an exit judged
// childPostUpdateFailure, and a binary that would not even start (code -1,
// recoverFailedBoot on a still-armed boot). Either way the boot is disarmed,
// so a quick death of the restored binary takes the fail-fast path instead of
// rolling back again.
//
// Mutants: restarted arming without recording (`b.armed = true; _ =
// artifact`) — judgeChildExit stats "", every first boot is past rollback, and
// the crash is supervised instead of rolled back; recoverFailedBoot leaving
// the boot armed (drop its `b.armed = false`) — a failed Start of the restored
// binary would roll back again.
func TestAFirstUpdatesQuickDeathRollsBackToItsArtifact(t *testing.T) {
	for _, tc := range []struct {
		name  string
		judge bool // the boot ran and exited; otherwise it would not start
		code  int
	}{
		{"quick crash", true, 2},
		{"would not start", false, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath := filepath.Join(t.TempDir(), "moombox")
			writeReleases(t, map[string]string{exePath: "N+1", exePath + ".old": "N"})
			var boot postUpdateBoot
			boot.restarted(exePath+".old", false, 0)

			if tc.judge {
				action, first := boot.judge(false, tc.code, 10*time.Second, false, 0)
				if action != childPostUpdateFailure || !first {
					t.Fatalf("a quick crash of the first boot = (%v, first %v), want a post-update failure", action, first)
				}
			}
			var restored bool
			captureStderr(t, func() { restored = boot.recoverFailedBoot(exePath, tc.code) })
			if !restored {
				t.Fatal("recoverFailedBoot did not restore the recorded artifact")
			}
			if !bytesEqualFile(t, exePath, "N") {
				t.Error("the plain name must hold the release the update replaced")
			}
			if boot.armed {
				t.Error("the restored binary is no first boot: recoverFailedBoot must disarm the record")
			}
		})
	}
}

// A config restart inside a still-unproven update's window carries the
// one-shot forward with the artifact it was armed with — whichever file that
// is. In the field it is a first Windows update's ~ (the boot reached its
// milestone, so no .old is left for the restart to find), and dropping or
// replacing the record there loses the rollback for a binary that never
// proved itself. A .old artifact and a ~ decoy two versions back make a
// record that drifts to the ~ name visible.
//
// Mutants: the carry-forward resetting the artifact (`b.artifact = ""`) — the
// crash after the config restart is supervised instead of rolled back;
// replacing it with its ~ sibling (`b.artifact =
// strings.TrimSuffix(b.artifact, ".old") + "~"`) — the rollback restores N-1;
// recoverFailedBoot restoring exePath+"~" instead of the record — the same.
func TestAConfigRestartKeepsTheArmedArtifact(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	writeReleases(t, map[string]string{exePath: "N+1", exePath + ".old": "N", exePath + "~": "N-1"})
	var boot postUpdateBoot
	boot.restarted(exePath+".old", false, 0)

	// The first boot exits 42 for a settings change 20s in; the restart finds
	// no new update.
	action, first := boot.judge(false, exitCodeRestart, 20*time.Second, false, 0)
	if action != childRestart || !first {
		t.Fatalf("a config restart of the first boot = (%v, first %v), want a restart of the first boot", action, first)
	}
	boot.restarted("", first, 20*time.Second)
	if !boot.armed || boot.artifact != exePath+".old" {
		t.Fatalf("after a config restart inside the window the record = %+v, want still armed with %s", boot, exePath+".old")
	}

	// The same release, respawned, dies at once.
	action, first = boot.judge(false, 2, 5*time.Second, false, 0)
	if action != childPostUpdateFailure || !first {
		t.Fatalf("a quick crash after the config restart = (%v, first %v), want a post-update failure", action, first)
	}
	var restored bool
	captureStderr(t, func() { restored = boot.recoverFailedBoot(exePath, 2) })
	if !restored {
		t.Fatal("recoverFailedBoot did not restore the recorded artifact")
	}
	if !bytesEqualFile(t, exePath, "N") {
		t.Error("the rollback must restore the artifact the update recorded, not the ~ two versions back")
	}
	if !bytesEqualFile(t, exePath+"~", "N-1") {
		t.Error("the ~ file must be left as it is")
	}
}

// restarted with no new update arms the next boot only to carry a first boot
// forward, and only inside postUpdateFailureWindow: a config restart after the
// window, or after a boot that was no first boot (a fresh launch, a boot past
// its rollback artifact), leaves the next child an ordinary one. A restart
// that names an artifact arms the boot with it whatever came before, replacing
// an earlier update's.
//
// Mutants: drop `ranFor < postUpdateFailureWindow` — a boot that proved itself
// is armed again; drop `wasFirst &&` — every early config restart arms a
// boot; record a named artifact only over an empty record (`if b.artifact ==
// "" { b.artifact = artifact }`) — the second update keeps the first's ~.
func TestRestartedArmsOnlyAnUpdateOrAnUnprovenFirstBoot(t *testing.T) {
	const old, tilde = "/x/moombox.exe.old", "/x/moombox.exe~"
	for _, tc := range []struct {
		name      string
		before    postUpdateBoot
		artifact  string
		wasFirst  bool
		ranFor    time.Duration
		wantArmed bool
		wantName  string
	}{
		{"update", postUpdateBoot{}, old, false, time.Hour, true, old},
		{"later update replaces the record", postUpdateBoot{artifact: tilde}, old, true, 10 * time.Minute, true, old},
		{"update right after an update", postUpdateBoot{artifact: tilde}, old, true, 5 * time.Second, true, old},
		{"config restart inside the window", postUpdateBoot{artifact: tilde}, "", true, time.Minute, true, tilde},
		{"config restart past the window", postUpdateBoot{artifact: tilde}, "", true, postUpdateFailureWindow, false, tilde},
		{"config restart of no first boot", postUpdateBoot{artifact: tilde}, "", false, time.Minute, false, tilde},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boot := tc.before // judge has already disarmed it
			boot.restarted(tc.artifact, tc.wasFirst, tc.ranFor)
			if boot.armed != tc.wantArmed {
				t.Errorf("armed = %v, want %v", boot.armed, tc.wantArmed)
			}
			if tc.wantArmed && boot.artifact != tc.wantName {
				t.Errorf("artifact = %q, want %q", boot.artifact, tc.wantName)
			}
		})
	}
}
