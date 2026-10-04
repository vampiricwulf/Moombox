package utils

import (
	"context"
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
