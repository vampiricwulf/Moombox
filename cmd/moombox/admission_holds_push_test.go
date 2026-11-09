package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/logger"
)

type holdsHubSpy struct {
	mu     sync.Mutex
	frames []any
}

func (h *holdsHubSpy) Broadcast(msgType string, payload any) {
	if msgType != "admission_holds" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.frames = append(h.frames, payload)
}

func (h *holdsHubSpy) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.frames)
}

// TestAdmissionHoldsArePushedToBothUIs: each change the scheduler signals
// sends the new snapshot to the dashboard as an admission_holds frame and to
// the TUI's hook, and the push stops with its context.
//
// Mutants: the WebSocket broadcast dropped; the TUI hook not called.
func TestAdmissionHoldsArePushedToBothUIs(t *testing.T) {
	log, err := logger.New(filepath.Join(t.TempDir(), "holds.log"), "error", 4096, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	s := &runState{log: log}
	var tuiGot []jobstate.AdmissionHolds
	var mu sync.Mutex
	fn := func(h jobstate.AdmissionHolds) { mu.Lock(); tuiGot = append(tuiGot, h); mu.Unlock() }
	s.admissionHoldsTUI.Store(&fn)
	changed := make(chan struct{}, 1)
	hub := &holdsHubSpy{}
	snap := jobstate.AdmissionHolds{DiskHeld: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runAdmissionHoldsPush(ctx, changed, func() jobstate.AdmissionHolds { return snap }, hub)
	}()
	changed <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for hub.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if hub.count() != 1 {
		t.Fatalf("admission_holds frames = %d, want 1", hub.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tuiGot) != 1 || !tuiGot[0].DiskHeld {
		t.Errorf("TUI got %+v, want the snapshot once", tuiGot)
	}
}
