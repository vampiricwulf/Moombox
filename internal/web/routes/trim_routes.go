package routes

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TrimRoutes registers trim-related API routes.
// rl bounds trim creation (an FFmpeg process per call); nil leaves it
// unbounded.
func TrimRoutes(r chi.Router, db *database.Database, trimSvc *worker.TrimService, rl *web.RateLimiter) {
	// POST /api/jobs/:id/trims
	r.With(limitedBy(rl)).Post("/api/jobs/{id}/trims", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		job, ok := loadJob(rw, db, jobID)
		if !ok {
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

		record, err := trimSvc.CreateTrim(req.Context(), job, *body.StartTime, *body.EndTime, nil)
		if err != nil {
			// A refusal the user can act on (the job's state, the range, a
			// duplicate, a trim already running) is shown as written: the
			// dashboard toasts this message, and "Failed to create trim" told
			// someone whose end time ran past the video nothing. Anything
			// else is an internal failure — its detail (paths, ffmpeg output)
			// stays out of the response, as before, and it is a 500 now
			// rather than a 400 that blamed the request.
			var refused *worker.TrimRefusedError
			switch {
			case errors.As(err, &refused) && refused.Conflict:
				jsonError(rw, refused.Reason, http.StatusConflict)
			case errors.As(err, &refused):
				jsonError(rw, refused.Reason, http.StatusBadRequest)
			default:
				jsonError(rw, "Failed to create trim", http.StatusInternalServerError)
			}
			return
		}

		jsonResponse(rw, map[string]any{"trim": record})
	})

	// DELETE /api/jobs/:id/trims/:trimId
	r.Delete("/api/jobs/{id}/trims/{trimId}", func(rw http.ResponseWriter, req *http.Request) {
		jobID := chi.URLParam(req, "id")
		trimID := chi.URLParam(req, "trimId")

		if err := trimSvc.DeleteTrim(jobID, trimID); err != nil {
			if errors.Is(err, worker.ErrTrimNotFound) {
				jsonError(rw, "trim not found", http.StatusNotFound)
				return
			}
			jsonError(rw, "failed to delete trim", http.StatusInternalServerError)
			return
		}

		jsonResponse(rw, map[string]any{"success": true})
	})
}
