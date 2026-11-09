package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/updater"
)

// TestDowngradeRefusalNamesTheLauncherArtifacts is C L17's drift guard.
//
// The schema-downgrade refusal in internal/database names the two files the
// launcher's automatic rollback leaves behind. internal/updater owns their
// names (FailedBinarySuffix, UpdateFailedMarkerSuffix), but internal/database
// imports nothing internal by design, so the suffixes are mirrored there, and
// this test — in a package that can see both sides — asserts they agree.
//
// The .failed half is a constant comparison against
// updater.FailedBinarySuffix, which attemptAutoRollback uses. The marker half
// is BEHAVIOURAL rather than a second comparison: the test makes the launcher
// write a real marker into a temp directory and checks that the database's
// suffix names the file that appeared, and that it is
// updater.UpdateFailedMarkerSuffix.
//
// MUTANT: change either constant in internal/database/migrations.go — say
// ".failed" -> ".broken", or ".update-failed" -> ".update-broken" (the name of
// a DIFFERENT marker the launcher also knows about, which is exactly the
// confusion this guards). The first fails the comparison; the second fails
// the stat. Neither is visible to TestMigrateRefusesNewerSchema, which reads
// the suffixes through the same accessor the message formats from and so
// stays green while both sides drift together — this test is the ONLY thing
// standing between that drift and a refusal message naming files that are
// not there.
func TestDowngradeRefusalNamesTheLauncherArtifacts(t *testing.T) {
	failed, marker := database.RollbackArtifactSuffixes()

	if failed != updater.FailedBinarySuffix {
		t.Errorf("internal/database names the kept binary %q, but attemptAutoRollback writes %q — "+
			"the refusal message sends the operator to a file that is not there",
			failed, updater.FailedBinarySuffix)
	}
	if marker != updater.UpdateFailedMarkerSuffix {
		t.Errorf("internal/database names the rollback marker %q, updater %q", marker, updater.UpdateFailedMarkerSuffix)
	}

	// writeAutoRollbackMarker only writes beside exePath; the binary itself
	// need not exist. It also prints the marker to stderr, as it does in
	// launcher_rollback_test.go.
	exePath := filepath.Join(t.TempDir(), "moombox.exe")
	writeAutoRollbackMarker(exePath, 1, true)
	if _, err := os.Stat(exePath + marker); err != nil {
		t.Errorf("internal/database names the rollback marker %q, but writeAutoRollbackMarker wrote "+
			"no such file (%v) — the refusal message points at a path that does not exist",
			exePath+marker, err)
	}
}
