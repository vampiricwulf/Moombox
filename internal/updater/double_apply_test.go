package updater

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Once an update is placed the process is restart-pending and .old holds the
// running binary — the only rollback artifact. A second apply in the same
// process (R U pressed again inside triggerRestart's grace window, or a TUI
// apply after a Web one) used to replace .old with the binary the first
// apply had just placed, so a rollback restored a release that never booted
// and the running one was gone from disk.
//
// Mutant: updateApplied not latching applied — the second apply proceeds.
func TestASecondApplyInOneProcessIsRefused(t *testing.T) {
	srv := swapTestServer(t)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)

	if err := u.ApplyUpdate(context.Background(), swapRelease(srv)); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	err := u.ApplyUpdate(context.Background(), swapRelease(srv))
	if err == nil || !strings.Contains(err.Error(), "restart pending") {
		t.Fatalf("second apply returned %v, want a restart-pending refusal", err)
	}
	if got, _ := os.ReadFile(exePath + ".old"); string(got) != "current binary" {
		t.Errorf(".old holds %q, want the running binary", got)
	}
	if got, _ := os.ReadFile(exePath); string(got) != "fresh moombox binary" {
		t.Errorf("exe holds %q, want the placed update", got)
	}
}

// Only a placed update latches: an apply that failed changed nothing on
// disk, so trying again must stay possible.
//
// Mutant: the latch set before the download — the retry is refused.
func TestAFailedApplyCanBeRetried(t *testing.T) {
	srv := swapTestServer(t)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)

	broken := swapRelease(srv)
	broken.SignatureURL = ""
	if err := u.ApplyUpdate(context.Background(), broken); err == nil {
		t.Fatal("an unsigned release was applied")
	}
	if err := u.ApplyUpdate(context.Background(), swapRelease(srv)); err != nil {
		t.Fatalf("retry after a failed apply: %v", err)
	}
	if got, _ := os.ReadFile(exePath); string(got) != "fresh moombox binary" {
		t.Errorf("exe holds %q after the retry", got)
	}
}

// A swap that failed both ways — the new binary not placed, the running one
// not renamed back — leaves the exe path empty and .old holding the running
// binary, the only copy, with .new kept and .update-broken written. It
// reports an error, so nothing latches and both UIs keep offering the update;
// a retry then overwrote .new, removed .old and failed its rename for want of
// an exe, leaving no binary anywhere. The state is laid out as that path
// (reachable only through the two-rename swap) leaves it.
//
// Mutant: ApplyUpdate without swapLeftBroken — .old and .new are gone.
func TestAnApplyOverABrokenSwapIsRefused(t *testing.T) {
	srv := swapTestServer(t)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)
	if err := os.Rename(exePath, exePath+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exePath+".new", []byte("staged binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exePath+brokenUpdateSuffix, []byte("manual recovery required"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := u.ApplyUpdate(context.Background(), swapRelease(srv)); err == nil || !strings.Contains(err.Error(), "recover by hand") {
		t.Fatalf("ApplyUpdate = %v, want a refusal", err)
	}
	if got, _ := os.ReadFile(exePath + ".old"); string(got) != "current binary" {
		t.Errorf(".old holds %q, want the running binary", got)
	}
	if got, _ := os.ReadFile(exePath + ".new"); string(got) != "staged binary" {
		t.Errorf(".new holds %q, want the staged binary left alone", got)
	}

	// Without the marker, an empty exe path is refused all the same.
	if err := os.Remove(exePath + brokenUpdateSuffix); err != nil {
		t.Fatal(err)
	}
	if err := u.ApplyUpdate(context.Background(), swapRelease(srv)); err == nil {
		t.Fatal("an apply with no running binary at the exe path went ahead")
	}
	if _, err := os.Stat(exePath + ".old"); err != nil {
		t.Errorf(".old is gone: %v", err)
	}
}

// The .update-broken marker refuses an apply on its own, with the running
// binary back at the exe path — an operator who put it back by hand but has
// not yet worked through the marker's instructions (security.md and
// operations.md give the marker its own row). The test above lays out the
// marker and the empty exe path together, so the missing-exe check alone
// satisfied it and deleting the marker check survived the whole package.
//
// Mutant: drop the os.Stat(u.exePath + brokenUpdateSuffix) refusal — the
// apply goes ahead and the exe holds the new binary.
func TestABrokenSwapMarkerAloneRefusesAnApply(t *testing.T) {
	srv := swapTestServer(t)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)
	if err := os.WriteFile(exePath+brokenUpdateSuffix, []byte("manual recovery required"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := u.ApplyUpdate(context.Background(), swapRelease(srv))
	if err == nil || !strings.Contains(err.Error(), brokenUpdateSuffix) {
		t.Fatalf("ApplyUpdate = %v, want a refusal naming the marker", err)
	}
	if got, _ := os.ReadFile(exePath); string(got) != "current binary" {
		t.Errorf("exe holds %q, want the running binary untouched", got)
	}
	if _, err := os.Stat(exePath + ".old"); !os.IsNotExist(err) {
		t.Errorf("the refused apply still moved the running binary aside (stat .old: %v)", err)
	}
}
