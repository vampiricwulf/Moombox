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

// probeRaceTimeout bounds the whole multi-target race. A live anycast target
// answers in tens of ms; a dead/blackholed network fails within this window.
const probeRaceTimeout = 3 * time.Second

// reachabilityProbe races TCP dials to targets and returns true as soon as ANY
// handshake completes. A completed TCP handshake proves actual routability to a
// live host — unlike Windows' InternetGetConnectedState, which only reports
// adapter/route state and can report "connected" during a real outage. Pure
// Go, no CGo; identical on every platform.
func reachabilityProbe(ctx context.Context, targets []string) bool {
	if len(targets) == 0 {
		targets = defaultProbeTargets
	}
	ctx, cancel := context.WithTimeout(ctx, probeRaceTimeout)
	defer cancel() // cancels still-in-flight dials once we have a winner

	resultCh := make(chan bool, len(targets)) // buffered: late senders never block
	var d net.Dialer
	for _, t := range targets {
		go func(addr string) {
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				resultCh <- false
				return
			}
			_ = conn.Close()
			resultCh <- true
		}(t)
	}
	for range targets {
		if <-resultCh {
			return true
		}
	}
	return false
}
