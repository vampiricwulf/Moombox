package engine

import (
	"context"
	"time"
)

const (
	connectivityPollInterval = 5 * time.Second
	// isOnlineProbeTimeout caps how long we wait for a single isOnline()
	// callback to return. Per audit reports/engine.md #18, the engine has
	// no way to enforce that callers' isOnline implementation is fast or
	// non-blocking; without a timeout, a misbehaving callback could block
	// cancellation indefinitely. Two seconds is plenty for any reasonable
	// in-process check (ICMP ping, cached state, etc.) without making
	// recovery from a real outage perceptibly slower.
	isOnlineProbeTimeout = 2 * time.Second
)

// callIsOnline invokes the caller-supplied probe with a hard timeout. If the
// probe doesn't return within isOnlineProbeTimeout, we treat it as offline
// (returns false) so the polling loop keeps trying rather than wedging on a
// hung callback. ctx is not honored inside the goroutine — the probe runs to
// completion in the background, but we stop waiting on it.
func callIsOnline(isOnline func() bool) bool {
	if isOnline == nil {
		return true
	}
	resultCh := make(chan bool, 1)
	go func() {
		defer func() {
			// A panic inside the caller's probe must not kill the engine.
			if r := recover(); r != nil {
				resultCh <- false
				return
			}
		}()
		resultCh <- isOnline()
	}()
	select {
	case ok := <-resultCh:
		return ok
	case <-time.After(isOnlineProbeTimeout):
		return false
	}
}

// waitForConnectivity blocks until isOnline returns true or ctx is cancelled.
// Returns nil when online, or ctx.Err() if cancelled. The isOnline callback
// is run with a per-call timeout (see callIsOnline) so a slow or hung probe
// can't block cancellation.
func waitForConnectivity(ctx context.Context, isOnline func() bool) error {
	if callIsOnline(isOnline) {
		return nil
	}
	ticker := time.NewTicker(connectivityPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if callIsOnline(isOnline) {
				return nil
			}
		}
	}
}
