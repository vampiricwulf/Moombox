package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForConnectivity_AlreadyOnline(t *testing.T) {
	start := time.Now()
	err := waitForConnectivity(t.Context(), func() bool { return true }, connectivityPollInterval)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("should return immediately when online")
	}
}

func TestWaitForConnectivity_WaitsAndReturns(t *testing.T) {
	// A millisecond poll instead of connectivityPollInterval: what this test
	// proves is that the ticker re-asks and returns once the probe flips, not
	// how long production's ticker happens to be (that is pinned by
	// TestDefaultDelaysMatchConstants).
	const poll = 50 * time.Millisecond
	var online atomic.Bool
	go func() {
		time.Sleep(20 * time.Millisecond) // flips before the first tick
		online.Store(true)
	}()

	start := time.Now()
	err := waitForConnectivity(t.Context(), func() bool { return online.Load() }, poll)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second { // safety net, ~20x one poll
		t.Fatalf("waitForConnectivity took %v after the probe flipped online, want one poll interval", elapsed)
	}
}

func TestWaitForConnectivity_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := waitForConnectivity(ctx, func() bool { return false }, connectivityPollInterval)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}
