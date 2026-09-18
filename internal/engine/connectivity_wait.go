package engine

import (
	"context"
	"sync"
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

// onlineProbe is one in-flight IsOnline call. ok is written by the probe
// goroutine strictly before done closes, so every waiter that receives from
// done sees the finished value — and because each flight owns its own struct,
// a later flight can never overwrite an earlier one's answer.
type onlineProbe struct {
	done chan struct{}
	ok   bool
}

// isOnlineInFlight is the probe currently running, if any, under isOnlineMu.
// A hung IsOnline used to leak one goroutine per 5 s poll for the whole
// outage, and every live downloader polls independently (sweep-2 ENGINE-15).
var (
	isOnlineMu       sync.Mutex
	isOnlineInFlight *onlineProbe
)

// callIsOnline invokes the caller-supplied probe with a hard timeout. If the
// probe doesn't return within isOnlineProbeTimeout, we treat it as offline
// (returns false) so the polling loop keeps trying rather than wedging on a
// hung callback. ctx is not honored inside the goroutine — the probe runs to
// completion in the background — but while one is running no second probe is
// started, so a hung callback costs ONE goroutine for the whole outage rather
// than one per poll per downloader.
//
// The probe's own design is untouched: the monitor decides what "online"
// means and how often it is asked; this is only the engine's wrapper around
// calling it.
func callIsOnline(isOnline func() bool) bool {
	if isOnline == nil {
		return true
	}

	isOnlineMu.Lock()
	p := isOnlineInFlight
	if p == nil {
		p = &onlineProbe{done: make(chan struct{})}
		isOnlineInFlight = p
		go func() {
			defer func() {
				// A panic inside the caller's probe must not kill the engine;
				// p.ok is already false, so the flight simply reads offline.
				if r := recover(); r != nil {
					p.ok = false
				}
				isOnlineMu.Lock()
				isOnlineInFlight = nil
				isOnlineMu.Unlock()
				close(p.done)
			}()
			p.ok = isOnline()
		}()
	}
	isOnlineMu.Unlock()

	select {
	case <-p.done:
		return p.ok
	case <-time.After(isOnlineProbeTimeout):
		return false
	}
}

// waitForConnectivity blocks until isOnline returns true or ctx is cancelled.
// Returns nil when online, or ctx.Err() if cancelled. The isOnline callback
// is run with a per-call timeout (see callIsOnline) so a slow or hung probe
// can't block cancellation. poll is how often isOnline is re-asked
// (connectivityPollInterval in production; tests pass milliseconds).
func waitForConnectivity(ctx context.Context, isOnline func() bool, poll time.Duration) error {
	if callIsOnline(isOnline) {
		return nil
	}
	ticker := time.NewTicker(poll)
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
