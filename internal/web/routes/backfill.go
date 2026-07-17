package routes

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
)

// BackfillRouteDeps carries the forced re-scan trigger for the feed-history
// backfill routes.
type BackfillRouteDeps struct {
	// Rescan runs a forced backfill sweep (§11 manual re-run): every
	// configured channel is treated as never-backfilled and rescans. The
	// worker's in-flight rules still skip an already-running, non-stale
	// scan, so this never cancels work in progress.
	Rescan func()
}

// backfillRescanDebounce bounds how often the rescan endpoint actually
// forces a sweep, so a jumpy operator (or a double-click) can't spam
// third-party APIs. The cycle-driven sweep's cadence is unaffected.
const backfillRescanDebounce = 30 * time.Second

// BackfillRoutes registers feed-history backfill endpoints.
//
// POST /api/backfill/rescan — force a feed-history re-scan of every
// configured channel (an operator who fixed cookies or suspects a gap
// shouldn't wait for a config change to re-trigger). Debounced server-side:
// a call inside the window returns 200 with
// {"success":false,"debounced":true,"retryAfterMs":N} rather than
// re-kicking.
func BackfillRoutes(r chi.Router, deps *BackfillRouteDeps) {
	var lastRescanUnixNano atomic.Int64

	r.Post("/api/backfill/rescan", func(w http.ResponseWriter, req *http.Request) {
		if deps.Rescan == nil {
			jsonError(w, "backfill unavailable", http.StatusServiceUnavailable)
			return
		}
		now := time.Now().UnixNano()
		prev := lastRescanUnixNano.Load()
		if prev != 0 && now-prev < int64(backfillRescanDebounce) {
			// CAS-free read is fine: worst case two racers both see stale
			// prev and both kick — harmless (the sweep is idempotent and
			// in-flight scans dedupe).
			wait := backfillRescanDebounce - time.Duration(now-prev)
			jsonResponse(w, map[string]any{
				"success":      false,
				"debounced":    true,
				"retryAfterMs": wait.Milliseconds(),
			})
			return
		}
		lastRescanUnixNano.Store(now)
		deps.Rescan()
		jsonResponse(w, map[string]any{"success": true})
	})
}
