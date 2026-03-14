package routes

import (
	"encoding/json"
	"math"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TrimRoutes registers trim-related API routes.
func TrimRoutes(r chi.Router, db *database.Database, trimSvc *worker.TrimService) {
	// POST /api/jobs/:id/trims
	r.Post("/api/jobs/{id}/trims", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, err := db.GetJob(jobID)
		if err != nil || job == nil {
			jsonError(rw, "job not found", http.StatusNotFound)
			return
		}

		var body struct {
			StartTime *float64 `json:"startTime"`
			EndTime   *float64 `json:"endTime"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		// Validate trim parameters (matching TypeScript createTrimSchema)
		if body.StartTime == nil || body.EndTime == nil {
			jsonError(rw, "Validation failed: startTime and endTime are required", http.StatusBadRequest)
			return
		}
		if math.IsNaN(*body.StartTime) || math.IsInf(*body.StartTime, 0) || math.IsNaN(*body.EndTime) || math.IsInf(*body.EndTime, 0) {
			jsonError(rw, "Validation failed: startTime and endTime must be finite numbers", http.StatusBadRequest)
			return
		}
		if *body.StartTime < 0 {
			jsonError(rw, "Validation failed: Start time cannot be negative", http.StatusBadRequest)
			return
		}
		if *body.EndTime <= *body.StartTime {
			jsonError(rw, "Validation failed: End time must be after start time", http.StatusBadRequest)
			return
		}
		if *body.EndTime-*body.StartTime < 1 {
			jsonError(rw, "Validation failed: Trim must be at least 1 second", http.StatusBadRequest)
			return
		}

		record, err := trimSvc.CreateTrim(req.Context(), job, *body.StartTime, *body.EndTime)
		if err != nil {
			// Match TS: don't expose internal error details
			jsonError(rw, "Failed to create trim", http.StatusBadRequest)
			return
		}

		jsonResponse(rw, map[string]any{"trim": record})
	})

	// DELETE /api/jobs/:id/trims/:trimId
	r.Delete("/api/jobs/{id}/trims/{trimId}", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		trimID := chi.URLParam(req, "trimId")

		if err := trimSvc.DeleteTrim(jobID, trimID); err != nil {
			jsonError(rw, "failed to delete trim", http.StatusBadRequest)
			return
		}

		jsonResponse(rw, map[string]any{"success": true})
	})
}
