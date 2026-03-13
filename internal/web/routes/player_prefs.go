package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// PlayerPrefsRoutes registers player preference API routes.
func PlayerPrefsRoutes(r chi.Router, db *database.Database) {
	// GET /api/player-prefs/{videoID}
	r.Get("/api/player-prefs/{videoID}", func(rw http.ResponseWriter, req *http.Request) {
		videoID := chi.URLParam(req, "videoID")
		offset, ok := db.GetPlayerPref(videoID)
		if !ok {
			jsonError(rw, "no prefs for video", http.StatusNotFound)
			return
		}
		jsonResponse(rw, map[string]any{"chatOffset": offset})
	})

	// PUT /api/player-prefs/{videoID}
	r.Put("/api/player-prefs/{videoID}", func(rw http.ResponseWriter, req *http.Request) {
		videoID := chi.URLParam(req, "videoID")
		var body struct {
			ChatOffset float64 `json:"chatOffset"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := db.SetPlayerPref(videoID, body.ChatOffset); err != nil {
			jsonError(rw, "failed to save preference", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, map[string]any{"success": true})
	})

	// DELETE /api/player-prefs/{videoID}
	r.Delete("/api/player-prefs/{videoID}", func(rw http.ResponseWriter, req *http.Request) {
		videoID := chi.URLParam(req, "videoID")
		if err := db.DeletePlayerPref(videoID); err != nil {
			jsonError(rw, "failed to delete preference", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, map[string]any{"success": true})
	})
}
