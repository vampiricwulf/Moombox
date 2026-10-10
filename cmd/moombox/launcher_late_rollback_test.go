package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/updater"
)

// The first update of a Windows launcher lifetime, rolled back by the record
// the launcher keeps, with the boots on either side played by the helpers
// run() calls. The update's artifact is the launcher's ~ image, which outlives
// the milestone sweep, so a release that crashes anywhere inside
// postUpdateFailureWindow is rolled back — including after its own boot has
// passed the milestone, removed the .update-pending breadcrumb and stamped its
// version. The restored boot then had no record of the release: it was not
// marked skipped, the daily check offered it again, and the version change
// went out as "Update Applied: updated from v2.0.0 to v1.0.0 … restarted
// successfully" beside "Previous Update Failed". Both crash times must end as
// a crash before the milestone always has: the release skipped and the
// rollback announced as a rollback. The artifacts are handleUpdateRestart's
// Windows answers, so this runs on every platform.
//
// Mutants: recoverFailedBoot not restoring the breadcrumb (drop its
// restoreUpdateBreadcrumb call) — the late crash's release is not skipped and
// the rollback is announced as an update; restarted arming without the tag
// (`b.armed, b.artifact = true, artifact`) — the same. The launcher loop's
// read of the tag is pendingUpdateTag, which this calls as the loop does;
// TestTheLauncherArmsTheBootWithTheBreadcrumbsRelease pins the loop's call.
func TestALateRollbackSkipsTheReleaseAndIsNoUpdate(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pastMilestone bool
	}{
		{"crash before the milestone", false},
		{"crash 30 s in, past the milestone", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exePath := filepath.Join(t.TempDir(), "moombox.exe")
			pendingPath := exePath + updater.PendingVersionSuffix
			// ApplyUpdate placed v2.0.0 and wrote the breadcrumb; the restart
			// renamed .old to ~.
			writeReleases(t, map[string]string{exePath: "v2.0.0", exePath + "~": "v1.0.0", pendingPath: "v2.0.0"})
			var boot postUpdateBoot
			boot.restarted(exePath+"~", pendingUpdateTag(exePath), false, 0)

			store := config.NewStore(config.Defaults(), filepath.Join(t.TempDir(), "config.toml"))
			lastRun := "1.0.0"
			if tc.pastMilestone {
				// The v2.0.0 boot past its milestone: the update landed, as
				// far as that boot can tell.
				rolledBackFrom := rolledBackRelease(exePath, "2.0.0")
				if !announcesVersionChange(lastRun, "2.0.0", rolledBackFrom) {
					t.Fatal("the update's own boot must announce the update")
				}
				lastRun = "2.0.0"
				resolveUpdateBreadcrumb(store, exePath, "2.0.0", rolledBackFrom, true, &nopLogger{})
				if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
					t.Fatalf("the update's boot left its breadcrumb (stat: %v)", err)
				}
			}

			action, _ := boot.judge(false, 2, 30*time.Second, false, 0)
			if action != childPostUpdateFailure {
				t.Fatalf("a crash 30 s into the first boot of a Windows update = %v, want a rollback", action)
			}
			var restored bool
			captureStderr(t, func() { restored = boot.recoverFailedBoot(exePath, 2) })
			if !restored || !bytesEqualFile(t, exePath, "v1.0.0") {
				t.Fatal("the launcher did not roll back to v1.0.0")
			}

			// The restored v1.0.0 boot.
			rolledBackFrom := rolledBackRelease(exePath, "1.0.0")
			if rolledBackFrom != "v2.0.0" {
				t.Errorf("the restored boot reads rollback %q, want v2.0.0", rolledBackFrom)
			}
			if announcesVersionChange(lastRun, "1.0.0", rolledBackFrom) {
				t.Errorf("the restored boot announces v%s → v1.0.0 as an update applied", lastRun)
			}
			resolveUpdateBreadcrumb(store, exePath, "1.0.0", rolledBackFrom, true, &nopLogger{})
			var skipped string
			store.Read(func(c *config.MoomboxConfig) { skipped = c.Updates.SkippedVersion })
			if skipped != "v2.0.0" {
				t.Errorf("skipped version = %q, want v2.0.0 — the daily check offers the release that just failed", skipped)
			}
		})
	}
}

// An update whose ApplyUpdate wrote no breadcrumb (its write failed) leaves
// the launcher no release to put back, and the rollback must not invent one.
//
// Mutant: restoreUpdateBreadcrumb without its empty-tag guard — an empty
// breadcrumb is written.
func TestARollbackWithNoRecordedReleaseWritesNoBreadcrumb(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	writeReleases(t, map[string]string{exePath: "v2.0.0", exePath + "~": "v1.0.0"})
	var boot postUpdateBoot
	boot.restarted(exePath+"~", pendingUpdateTag(exePath), false, 0)
	boot.judge(false, 2, 30*time.Second, false, 0)
	captureStderr(t, func() { boot.recoverFailedBoot(exePath, 2) })
	if _, err := os.Stat(exePath + updater.PendingVersionSuffix); !os.IsNotExist(err) {
		t.Errorf("a breadcrumb was written for no recorded release (stat: %v)", err)
	}
}
