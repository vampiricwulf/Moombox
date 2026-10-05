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
