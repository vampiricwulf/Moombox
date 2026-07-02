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

// TestRefreshCookiesRefusesWhileSlotHeld pins the double-refresh gate the
// refreshChromium sentinel-restore relies on: while the refresh slot is held
// (claim sentinel or real cmd), a second RefreshCookies must no-op WITHOUT
// clearing the holder's claim — the first refresh's tail (merge → atomic
// write → verify → meta save) depends on the slot staying closed until its
// own outer defer releases it.
func TestRefreshCookiesRefusesWhileSlotHeld(t *testing.T) {
	s := NewAutoCookieService("", "", nil, nopAutoCookieLogger{})
	sentinel := &exec.Cmd{}
	s.refreshCmd = sentinel // simulate an in-flight refresh (claim or tail)

	ok, err := s.RefreshCookies(context.Background())
	if ok || err != nil {
		t.Fatalf("RefreshCookies while slot held = (%v, %v), want (false, nil) no-op", ok, err)
	}
	if s.refreshCmd != sentinel {
		t.Error("second RefreshCookies must not clear the first refresh's slot claim")
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
