package sidecar

import (
	"sync/atomic"
	"time"
)

// Health is the BotGuard sidecar's liveness as both user interfaces report it.
//
// It is a PACKAGE-LEVEL snapshot rather than a field on Sidecar, mirroring
// routes.SharedDiskStatus and routes.SharedUpdateInfo: one value the producer
// writes and each UI's wiring reads, so neither the web routes nor
// cmd/moombox's TUI wiring needs a handle on the Sidecar itself — and
// internal/tui keeps importing neither this package nor internal/bgutils
// (it receives a plain bool through the same app.Send shape ConnectivityMsg
// uses).
type Health struct {
	// Healthy is false from the moment the child dies until a supervisor
	// restart succeeds.
	Healthy bool
	// Reason is why it last went down ("stdout EOF", "readPump panic", a
	// failed initial start). Empty while healthy.
	Reason string
	// Restarts counts SUCCESSFUL supervisor restarts this process.
	Restarts uint64
	// Since is when Healthy last changed.
	Since time.Time
}

// sharedHealth holds the last published snapshot. nil means "never published",
// which is exactly what a process with `[bgutils] use_sidecar = false` looks
// like — and is deliberately NOT the same as "unhealthy", so a correct
// sidecar-disabled config never draws an alert.
var sharedHealth atomic.Pointer[Health]

// PublishHealth records the current sidecar health for both UIs.
func PublishHealth(h Health) {
	sharedHealth.Store(&h)
}

// CurrentHealth returns the last published snapshot. ok is false until the
// first publish.
func CurrentHealth() (Health, bool) {
	if h := sharedHealth.Load(); h != nil {
		return *h, true
	}
	return Health{}, false
}
