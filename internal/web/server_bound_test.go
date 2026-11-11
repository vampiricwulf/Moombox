package web

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// freeLoopbackPort returns a port nothing was listening on a moment ago.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// Bound closes once the listener is bound, with ActualPort already set, and
// Start keeps serving until its context ends.
//
// Mutant: drop markBound from Start (the wait below times out).
func TestStartClosesBoundOnceTheListenerIsUp(t *testing.T) {
	cfg := config.Defaults()
	cfg.Network.Port = freeLoopbackPort(t)
	s := NewServer(config.NewStore(cfg, ""), testWSLogger{})

	select {
	case <-s.Bound():
		t.Fatal("Bound is closed before Start ran")
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	select {
	case <-s.Bound():
	case err := <-done:
		cancel()
		t.Fatalf("Start returned before binding: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Bound never closed")
	}
	if s.ActualPort() <= 0 {
		t.Errorf("ActualPort is %d once Bound is closed; want the bound port", s.ActualPort())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start after shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Start did not return after its context ended")
	}
}

// A Start that fails before binding returns its error and never closes
// Bound, so a caller selecting on both reads the failure.
//
// Mutant: close Bound at the top of Start.
func TestStartFailureLeavesBoundOpen(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "bad.crt")
	key := filepath.Join(dir, "bad.key")
	for _, p := range []string{cert, key} {
		if err := os.WriteFile(p, []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.Network.Port = freeLoopbackPort(t)
	cfg.Network.HTTPSEnabled = true
	cfg.Network.TLSCertPath = cert
	cfg.Network.TLSKeyPath = key
	s := NewServer(config.NewStore(cfg, ""), testWSLogger{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err == nil {
		t.Fatal("Start with an unreadable certificate returned nil")
	}
	select {
	case <-s.Bound():
		t.Error("Bound is closed after a Start that never bound")
	default:
	}
	if s.ActualPort() != 0 {
		t.Errorf("ActualPort is %d after a failed Start; want 0", s.ActualPort())
	}
}
