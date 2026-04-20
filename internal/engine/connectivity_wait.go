package engine

import (
	"context"
	"time"
)

const connectivityPollInterval = 5 * time.Second

// waitForConnectivity blocks until isOnline returns true or ctx is cancelled.
// Returns nil when online, or ctx.Err() if cancelled.
func waitForConnectivity(ctx context.Context, isOnline func() bool) error {
	if isOnline() {
		return nil
	}
	ticker := time.NewTicker(connectivityPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if isOnline() {
				return nil
			}
		}
	}
}
