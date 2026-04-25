package cookies

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// TestStartSetupReturnsSetupInProgressSentinel verifies the producer at
// autocookies.go:254-257 returns ErrSetupInProgress when a previous
// StartSetup is still active. Audit cross-cutting C3 sentinel migration.
func TestStartSetupReturnsSetupInProgressSentinel(t *testing.T) {
	s := NewAutoCookieService("", "", nil, nopAutoCookieLogger{})
	s.setupProcess = &os.Process{Pid: -1} // simulate active setup

	err := s.StartSetup("youtube")
	if !errors.Is(err, ErrSetupInProgress) {
		t.Fatalf("StartSetup with active setup: want errors.Is(err, ErrSetupInProgress), got %v", err)
	}
}

// TestStartSetupReturnsRefreshInProgressSentinel verifies the producer at
// autocookies.go:258-261 wraps ErrRefreshInProgress when a refresh holds
// the slot. The wrap intentionally keeps a "try again shortly" prefix so
// the user-facing message is still useful.
func TestStartSetupReturnsRefreshInProgressSentinel(t *testing.T) {
	s := NewAutoCookieService("", "", nil, nopAutoCookieLogger{})
	s.refreshCmd = &exec.Cmd{} // simulate active refresh

	err := s.StartSetup("youtube")
	if !errors.Is(err, ErrRefreshInProgress) {
		t.Fatalf("StartSetup with active refresh: want errors.Is(err, ErrRefreshInProgress), got %v", err)
	}
	if errors.Is(err, ErrSetupInProgress) {
		t.Error("StartSetup with active refresh: should NOT match ErrSetupInProgress")
	}
}

// TestFinishSetupReturnsNoSetupInProgressSentinel covers the "called
// without StartSetup" case at autocookies.go:306-309.
func TestFinishSetupReturnsNoSetupInProgressSentinel(t *testing.T) {
	s := NewAutoCookieService("", "", nil, nopAutoCookieLogger{})

	_, _, err := s.FinishSetup(context.Background())
	if !errors.Is(err, ErrNoSetupInProgress) {
		t.Fatalf("FinishSetup without active setup: want errors.Is(err, ErrNoSetupInProgress), got %v", err)
	}
}

// TestFinishSetupReturnsCancelledSentinel covers the "called after
// CancelSetup flipped the cancelled flag" case at autocookies.go:310-313.
func TestFinishSetupReturnsCancelledSentinel(t *testing.T) {
	s := NewAutoCookieService("", "", nil, nopAutoCookieLogger{})
	s.setupProcess = &os.Process{Pid: -1}
	s.setupBrowser = &DetectedBrowser{Type: "Chrome", Path: "fake"}
	s.cancelled = true

	_, _, err := s.FinishSetup(context.Background())
	if !errors.Is(err, ErrSetupCancelled) {
		t.Fatalf("FinishSetup after cancel: want errors.Is(err, ErrSetupCancelled), got %v", err)
	}
	if errors.Is(err, ErrNoSetupInProgress) {
		t.Error("FinishSetup after cancel: should NOT match ErrNoSetupInProgress (state IS populated)")
	}
}

// TestSentinelsAreDistinct sanity-checks that each sentinel only matches
// itself — guards against accidental aliasing if a future refactor
// collapses two sentinels into one.
func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{
		ErrNoBrowserFound,
		ErrSetupInProgress,
		ErrNoSetupInProgress,
		ErrSetupCancelled,
		ErrRefreshInProgress,
		ErrProfileNotFound,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel aliasing: %v should not match %v", a, b)
			}
		}
	}
}
