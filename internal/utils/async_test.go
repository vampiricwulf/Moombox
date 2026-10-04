package utils

import (
	"context"
	"testing"
	"time"
)

func TestSleep(t *testing.T) {
	start := time.Now()
	err := Sleep(context.Background(), 10*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Errorf("Sleep returned error: %v", err)
	}
	if elapsed < 10*time.Millisecond {
		t.Errorf("Sleep returned too early: %v", elapsed)
	}
}

func TestSleepCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Sleep(ctx, time.Hour)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
