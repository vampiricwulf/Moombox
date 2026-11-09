package updater

import "os"

// The on-disk names a self-update and the launcher place beside the
// executable, each the executable's path plus the suffix. They are part of
// the update path between versions — an old launcher reads what a new child
// wrote and the reverse — so their bytes never change. internal/database
// mirrors FailedBinarySuffix and UpdateFailedMarkerSuffix (it imports nothing
// internal), pinned by cmd/moombox TestDowngradeRefusalNamesTheLauncherArtifacts.
// PendingVersionSuffix is the breadcrumb's, beside its own doc in updater.go.
const (
	// NewSuffix is the download ApplyUpdate stages and then places.
	NewSuffix = ".new"
	// NewSigSuffix is the staged download's signature (NewSuffix + ".sig").
	NewSigSuffix = NewSuffix + ".sig"
	// OldSuffix is the running binary ApplyUpdate moves aside, kept until a
	// boot proves the new one healthy.
	OldSuffix = ".old"
	// FailedBinarySuffix is the binary a failed first post-update boot
	// leaves when the launcher rolls back (cmd/moombox attemptAutoRollback).
	// The rollback used to DELETE it on the grounds that it is bit-identical
	// to the published GitHub asset — but when the failure is a refused DB
	// downgrade, the older binary the rollback restores then prints "restore
	// the newer binary … if still present" about a file the rollback had just
	// removed, and the install is down until a manual re-download (CORE-1).
	// Keeping it costs ~90 MB until the next healthy boot sweeps it
	// (CleanupOldBinary).
	FailedBinarySuffix = ".failed"
	// LauncherImageSuffix is the Windows launcher's superseded image:
	// handleUpdateRestart renames OldSuffix to it while executing from it.
	LauncherImageSuffix = "~"
	// UpdateFailedMarkerSuffix is the marker the launcher writes when it rolls
	// a failed update back, or cannot and asks for manual recovery.
	UpdateFailedMarkerSuffix = ".update-failed"
	// UpdateBrokenMarkerSuffix is the marker a swap writes when both placing
	// the new binary and renaming the running one back failed.
	UpdateBrokenMarkerSuffix = ".update-broken"
)

// FailureMarkers returns the failure markers present beside exePath, broken
// first and then failed. The order is part of the contract: the boot's
// announcement joins the markers it saw into the dedupe stamp it persists
// (Updates.LastFailureMarkerSeen, "a;b"), and a stamp written broken-first
// by an older boot must compare equal to the next one's.
//
// A presence check only — the deciders that read it (the launcher keeping
// LauncherImageSuffix, rolledBackRelease) combine it with what they know;
// it is not a classifier of the install's state, part of which lives only in
// the launcher's memory.
func FailureMarkers(exePath string) []string {
	var present []string
	for _, suffix := range []string{UpdateBrokenMarkerSuffix, UpdateFailedMarkerSuffix} {
		if _, err := os.Stat(exePath + suffix); err == nil {
			present = append(present, exePath+suffix)
		}
	}
	return present
}
