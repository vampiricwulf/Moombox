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

func TestJitter(t *testing.T) {
	// Zero or negative max returns 0
	if j := Jitter(0); j != 0 {
		t.Errorf("Jitter(0) = %v, want 0", j)
	}
	if j := Jitter(-1); j != 0 {
		t.Errorf("Jitter(-1) = %v, want 0", j)
	}

	// Positive max returns value in range [0, max)
	maxJ := 100 * time.Millisecond
	for i := 0; i < 100; i++ {
		j := Jitter(maxJ)
		if j < 0 || j >= maxJ {
			t.Errorf("Jitter(%v) = %v, out of range", maxJ, j)
		}
	}
}
