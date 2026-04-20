package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForConnectivity_AlreadyOnline(t *testing.T) {
	start := time.Now()
	err := waitForConnectivity(context.Background(), func() bool { return true })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("should return immediately when online")
	}
}

func TestWaitForConnectivity_WaitsAndReturns(t *testing.T) {
	var online atomic.Bool
	go func() {
		time.Sleep(200 * time.Millisecond)
		online.Store(true)
	}()

	err := waitForConnectivity(context.Background(), func() bool { return online.Load() })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForConnectivity_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := waitForConnectivity(ctx, func() bool { return false })
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}
