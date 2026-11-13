package connectivity

import (
	"context"
	"net"
	"time"
)

// defaultProbeTargets are anycast HTTPS endpoints used to verify real internet
// reachability when no override is configured. Port 443 (not DNS/53) because
// some networks block outbound DNS but allow HTTPS. Multiple targets so a
// single host's outage can't cause a false-offline.
//
// The user-facing default lives in config.DefaultProbeTargets and is wired in
// via Monitor.SetProbeTargets; this slice is the in-package safety fallback for
// when SetProbeTargets is never called or is passed an empty list.
var defaultProbeTargets = []string{"1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"}

// probeFallbackTargets are the platform hosts Moombox itself needs. They join
// the probe's race only when no configured target has connected after
// probeFallbackDelay, so a network that blocks the configured targets (a rule
// against public DoH resolvers, a routing fault to those anycast addresses)
// but reaches YouTube or Twitch is not read as offline: a monitor that is
// wrongly offline polls no channel, admits no backlog and sends no webhook.
// A new Monitor copies this list; a test that must not touch the real network
// empties it, or calls Monitor.SetProbeFallbackTargets(nil).
var probeFallbackTargets = []string{"www.youtube.com:443", "gql.twitch.tv:443"}

// probeFallbackDelay is how long the configured targets have the race to
// themselves. A live anycast target answers in tens of ms, so a healthy
// network never dials the fallback hosts (and never resolves their names).
var probeFallbackDelay = time.Second

// probeRaceTimeout bounds the whole multi-target race, fallback hosts
// included: they are added to the one race, never raced after it. A live
// anycast target answers in tens of ms; a dead/blackholed network fails
// within this window.
const probeRaceTimeout = 3 * time.Second

// probeDial performs one TCP dial; a variable so tests can inject failure
// modes (including a panic) without a real network.
var probeDial = func(ctx context.Context, d *net.Dialer, addr string) (net.Conn, error) {
	return d.DialContext(ctx, "tcp", addr)
}

// reachabilityProbe races TCP dials to targets and returns true as soon as ANY
// handshake completes. A completed TCP handshake proves actual routability to a
// live host — unlike Windows' InternetGetConnectedState, which only reports
// adapter/route state and can report "connected" during a real outage. Pure
// Go, no CGo; identical on every platform.
//
// When no target has connected after probeFallbackDelay — or all of them have
// failed sooner — the fallback hosts are dialled in the same race, under the
// same deadline. viaFallback reports that a fallback host was the one that
// connected.
func reachabilityProbe(ctx context.Context, targets, fallback []string) (online, viaFallback bool) {
	if len(targets) == 0 {
		targets = defaultProbeTargets
	}
	ctx, cancel := context.WithTimeout(ctx, probeRaceTimeout)
	defer cancel() // cancels still-in-flight dials once we have a winner

	type result struct{ ok, fallback bool }
	resultCh := make(chan result, len(targets)+len(fallback)) // buffered: late senders never block
	var d net.Dialer
	// Read once, here: the probe returns on the first winner and leaves the
	// losing dials running, so a dial goroutine reading the variable itself
	// could do so after a test had already swapped it for the next case.
	dial := probeDial
	start := func(addrs []string, isFallback bool) {
		for _, t := range addrs {
			go func(addr string) {
				// Inline recovery (project rule). A panic before the send would
				// leave the receive loop waiting for a result that never comes;
				// report "unreachable" instead. A recovered dial panic is not
				// logged (no logger reaches this free function); the Monitor's
				// recover covers its poll loop, not these goroutines.
				defer func() {
					if r := recover(); r != nil {
						resultCh <- result{false, isFallback}
					}
				}()
				conn, err := dial(ctx, &d, addr)
				if err != nil {
					resultCh <- result{false, isFallback}
					return
				}
				_ = conn.Close()
				resultCh <- result{true, isFallback}
			}(t)
		}
	}
	start(targets, false)
	pending := len(targets)

	var fallbackC <-chan time.Time
	if len(fallback) > 0 {
		timer := time.NewTimer(probeFallbackDelay)
		defer timer.Stop()
		fallbackC = timer.C
	}
	startFallback := func() {
		fallbackC = nil
		start(fallback, true)
		pending += len(fallback)
	}
	for pending > 0 {
		select {
		case r := <-resultCh:
			if r.ok {
				return true, r.fallback
			}
			pending--
			if pending == 0 && fallbackC != nil {
				startFallback() // every configured target failed early
			}
		case <-fallbackC:
			startFallback()
		}
	}
	return false, false
}
