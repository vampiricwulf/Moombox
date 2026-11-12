package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// HistoryRoutesDeps holds dependencies for orphaned-history route handlers.
type HistoryRoutesDeps struct {
	DB     *database.Database
	Logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// HistoryRoutes registers orphaned processing-history cleanup endpoints. A
// history row with no matching job (its job was deleted, or the video was
// skipped and never jobbed) makes the monitor treat the video as already
// processed, so it won't be re-discovered. Removing the row unblocks it.
// Mirrors FileRoutes (orphaned files) so the two cleanups behave identically.
func HistoryRoutes(r chi.Router, deps *HistoryRoutesDeps) {
	// GET /api/history/orphaned — list history rows with no matching job
	r.Get("/api/history/orphaned", func(rw http.ResponseWriter, req *http.Request) {
		entries, err := deps.DB.ListOrphanedHistory()
		if err != nil {
			jsonError(rw, "failed to list orphaned history", http.StatusInternalServerError)
			return
		}
		if entries == nil {
			entries = []database.OrphanedHistoryEntry{}
		}
		jsonResponse(rw, entries)
	})

	// DELETE /api/history/orphaned — remove the given history video IDs. Only
	// rows that are still orphaned go: DeleteHistoryEntries leaves the row of
	// a video that has a job.
	r.Delete("/api/history/orphaned", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			VideoIDs []string `json:"videoIds"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		if len(body.VideoIDs) == 0 {
			jsonError(rw, "no video IDs specified", http.StatusBadRequest)
			return
		}

		n, err := deps.DB.DeleteHistoryEntries(body.VideoIDs)
		if err != nil {
			deps.Logger.Warn("failed to delete orphaned history", "err", err)
			jsonError(rw, "failed to delete orphaned history", http.StatusInternalServerError)
			return
		}
		deps.Logger.Info("removed orphaned history entries", "count", n)

		// The files endpoint's {deleted, errors} envelope, for the UI:
		// `deleted` echoes the requested IDs. `removed` is the number of rows
		// that actually went, which is lower when an ID had no history row or
		// its video has a job.
		jsonResponse(rw, map[string]any{
			"deleted": body.VideoIDs,
			"removed": n,
			"errors":  []map[string]string{},
		})
	})
}
