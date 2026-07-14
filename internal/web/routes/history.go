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

	// DELETE /api/history/orphaned — remove the given history video IDs
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

		// Report which of the requested IDs are now gone. The delete is
		// all-or-nothing per batch, so on success every requested ID is deleted;
		// mirror the files endpoint's {deleted, errors} envelope for the UI.
		jsonResponse(rw, map[string]any{
			"deleted": body.VideoIDs,
			"errors":  []map[string]string{},
		})
	})
}
