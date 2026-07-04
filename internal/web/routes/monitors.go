package routes

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
)

// MonitorRouteDeps carries the force-check trigger for the monitor routes.
type MonitorRouteDeps struct {
	// CheckNow kicks all three monitors immediately (feed/DECAPI/Twitch).
	// The monitors coalesce a mid-cycle kick via their pendingKick latch,
	// so this never stacks overlapping cycles.
	CheckNow func()
}

// monitorCheckDebounce bounds how often the force-check endpoint actually
// kicks the monitors, so a jumpy operator (or a double-click) can't spam
// third-party APIs. The monitors' own cadence is unaffected.
const monitorCheckDebounce = 30 * time.Second

// MonitorRoutes registers monitor utility endpoints.
//
// POST /api/monitors/check-now — force an immediate poll of all monitors
// (an operator who knows a stream is imminent shouldn't wait out the
// interval). Debounced server-side: a call inside the window returns 200
// with {"success":false,"debounced":true,"retryAfterMs":N} rather than
// re-kicking.
func MonitorRoutes(r chi.Router, deps *MonitorRouteDeps) {
	var lastCheckUnixNano atomic.Int64

	r.Post("/api/monitors/check-now", func(w http.ResponseWriter, req *http.Request) {
		if deps.CheckNow == nil {
			jsonError(w, "monitors unavailable", http.StatusServiceUnavailable)
			return
		}
		now := time.Now().UnixNano()
		prev := lastCheckUnixNano.Load()
		if prev != 0 && now-prev < int64(monitorCheckDebounce) {
			// CAS-free read is fine: worst case two racers both see stale
			// prev and both kick — harmless (pendingKick coalesces).
			wait := monitorCheckDebounce - time.Duration(now-prev)
			jsonResponse(w, map[string]any{
				"success":      false,
				"debounced":    true,
				"retryAfterMs": wait.Milliseconds(),
			})
			return
		}
		lastCheckUnixNano.Store(now)
		deps.CheckNow()
		jsonResponse(w, map[string]any{"success": true})
	})
}
