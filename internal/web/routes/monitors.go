package routes

import (
	"net/http"
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
	debounce := newCallDebouncer(monitorCheckDebounce)

	r.Post("/api/monitors/check-now", func(w http.ResponseWriter, req *http.Request) {
		if deps.CheckNow == nil {
			jsonError(w, "monitors unavailable", http.StatusServiceUnavailable)
			return
		}
		if ok, wait := debounce.allow(time.Now()); !ok {
			writeDebounced(w, wait)
			return
		}
		deps.CheckNow()
		jsonResponse(w, map[string]any{"success": true})
	})
}
