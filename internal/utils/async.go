package utils

import (
	"context"
	"math/rand"
	"time"
)

// Sleep sleeps for the given duration, respecting context cancellation.
// Returns the context error if cancelled.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Jitter returns a random duration between 0 and maxJitter.
func Jitter(maxJitter time.Duration) time.Duration {
	if maxJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(maxJitter)))
}

// SleepWithJitter sleeps for duration + random jitter, respecting context.
func SleepWithJitter(ctx context.Context, d time.Duration, maxJitter time.Duration) error {
	return Sleep(ctx, d+Jitter(maxJitter))
}
