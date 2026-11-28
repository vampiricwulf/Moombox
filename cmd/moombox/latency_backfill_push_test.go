package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

type latencyHubSpy struct {
	mu     sync.Mutex
	frames []any
}

func (h *latencyHubSpy) Broadcast(msgType string, payload any) {
	if msgType != "latency_backfill" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.frames = append(h.frames, payload)
}

func (h *latencyHubSpy) snapshot() []any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]any(nil), h.frames...)
}

// TestLatencyBackfillStatusIsPushedToBothUIs: each change the walk signals
// sends the status to the dashboard as a latency_backfill frame (the
// structured status, which the dashboard words itself) and the worded line
// to the TUI's hook (owner ruling RV-20); the push stops with its context.
//
// Mutants: the WebSocket broadcast dropped; the TUI hook not called; the
// frame carrying the line instead of the status.
func TestLatencyBackfillStatusIsPushedToBothUIs(t *testing.T) {
	log, err := logger.New(filepath.Join(t.TempDir(), "latency.log"), "error", 4096, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	s := &runState{log: log}
	lines := make(chan string, 4)
	fn := func(line string) { lines <- line }
	s.latencyBackfillTUI.Store(&fn)
	changed := make(chan struct{}, 1)
	hub := &latencyHubSpy{}
	st := worker.LatencyBackfillStatus{State: worker.LatencyBackfillRunning, Done: 212, Total: 540}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runLatencyBackfillPush(ctx, changed, func() worker.LatencyBackfillStatus { return st }, hub)
	}()
	changed <- struct{}{}
	select {
	case line := <-lines:
		if line != "212 of 540 recordings done" {
			t.Errorf("TUI got %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the TUI got no line")
	}
	cancel()
	<-done
	frames := hub.snapshot()
	if len(frames) != 1 || frames[0] != st {
		t.Errorf("latency_backfill frames = %v, want the status once", frames)
	}
}
