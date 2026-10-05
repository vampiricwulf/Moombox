package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/updater"
)

// The first boot of a fresh update resolves the .update-pending breadcrumb
// at the first-successful-boot milestone, just after it sweeps .old. An exit
// after that used to be treated as a failed update for the rest of the
// two-minute window: on Linux, with nothing left to roll back to, the
// launcher ended on preserve-with-instructions where any other boot's crash
// is respawned. The breadcrumb's disappearance now proves the boot — but
// only when it was there at spawn, since ApplyUpdate writes it best-effort.
//
// Mutants: dropping pendingAtSpawn (a boot that never had a breadcrumb reads
// as proved), and ignoring the file (a boot that died before the milestone
// reads as proved).
func TestUpdateBootProvedByTheResolvedBreadcrumb(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox")
	pending := exePath + updater.PendingVersionSuffix

	if err := os.WriteFile(pending, []byte("v9.9.9"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !updatePendingPresent(exePath) {
		t.Fatal("updatePendingPresent: false with the breadcrumb on disk")
	}
	if updateBootProved(exePath, true) {
		t.Error("a boot that left the breadcrumb unresolved (died before the milestone) must stay unproven")
	}

	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	if !updateBootProved(exePath, true) {
		t.Error("a boot that resolved the breadcrumb it started with must be proved")
	}
	if updateBootProved(exePath, false) {
		t.Error("a boot that started without a breadcrumb must stay unproven: its absence proves nothing")
	}
}
