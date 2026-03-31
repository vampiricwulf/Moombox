package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// WatchRoutes registers watch tracking API routes.
func WatchRoutes(r chi.Router, db *database.Database) {

	// PUT /api/jobs/{id}/resume-position — lightweight periodic save
	r.Put("/api/jobs/{id}/resume-position", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		var body struct {
			Position float64 `json:"position"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		db.UpdateResumePosition(jobID, body.Position)
		rw.WriteHeader(http.StatusNoContent)
	})

	// POST /api/jobs/{id}/resume-position — sendBeacon fallback (beacon only sends POST)
	r.Post("/api/jobs/{id}/resume-position", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		var body struct {
			Position float64 `json:"position"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		db.UpdateResumePosition(jobID, body.Position)
		rw.WriteHeader(http.StatusNoContent)
	})

	// POST /api/jobs/{id}/watched — mark single job as watched
	r.Post("/api/jobs/{id}/watched", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}
		db.UpdateJobFields(jobID, map[string]any{
			"watched":         1,
			"resume_position": nil,
		})
		updated, err := db.GetJob(jobID)
		if err != nil || updated == nil {
			jsonError(rw, "failed to read back job", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, updated)
	})

	// DELETE /api/jobs/{id}/watched — mark single job as unwatched
	r.Delete("/api/jobs/{id}/watched", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}
		db.UpdateJobFields(jobID, map[string]any{
			"watched":         0,
			"resume_position": nil,
		})
		updated, err := db.GetJob(jobID)
		if err != nil || updated == nil {
			jsonError(rw, "failed to read back job", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, updated)
	})

	// POST /api/jobs/batch/watched — batch mark as watched
	r.Post("/api/jobs/batch/watched", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			JobIDs []string `json:"jobIds"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		if len(body.JobIDs) == 0 {
			jsonError(rw, "jobIds required", http.StatusBadRequest)
			return
		}
		db.BatchSetWatched(body.JobIDs, true)
		jsonResponse(rw, map[string]any{"success": true})
	})

	// DELETE /api/jobs/batch/watched — batch mark as unwatched
	r.Delete("/api/jobs/batch/watched", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			JobIDs []string `json:"jobIds"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		if len(body.JobIDs) == 0 {
			jsonError(rw, "jobIds required", http.StatusBadRequest)
			return
		}
		db.BatchSetWatched(body.JobIDs, false)
		jsonResponse(rw, map[string]any{"success": true})
	})
}
