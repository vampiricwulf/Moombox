package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// FileRoutesDeps holds dependencies for file route handlers.
type FileRoutesDeps struct {
	DB     *database.Database
	Cfg    *config.MoomboxConfig
	Logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// FileRoutes registers orphaned file management endpoints.
func FileRoutes(r chi.Router, deps *FileRoutesDeps) {
	// GET /api/files/orphaned — scan and return orphaned files
	r.Get("/api/files/orphaned", func(rw http.ResponseWriter, req *http.Request) {
		entries, err := worker.ScanOrphanedFiles(deps.DB, deps.Cfg)
		if err != nil {
			jsonError(rw, "failed to scan orphaned files", http.StatusInternalServerError)
			return
		}
		if entries == nil {
			entries = []worker.OrphanedEntry{}
		}

		jsonResponse(rw, entries)
	})

	// DELETE /api/files/orphaned — delete specific orphaned paths
	r.Delete("/api/files/orphaned", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		if len(body.Paths) == 0 {
			jsonError(rw, "no paths specified", http.StatusBadRequest)
			return
		}

		var deleted []string
		var errors []map[string]string

		for _, path := range body.Paths {
			if err := worker.DeleteOrphanedFile(path, deps.Cfg); err != nil {
				errors = append(errors, map[string]string{
					"path":  path,
					"error": "failed to delete file",
				})
				deps.Logger.Warn("failed to delete orphaned file", "path", path, "err", err)
			} else {
				deleted = append(deleted, path)
				deps.Logger.Info("deleted orphaned file", "path", path)
			}
		}

		if deleted == nil {
			deleted = []string{}
		}
		if errors == nil {
			errors = []map[string]string{}
		}

		jsonResponse(rw, map[string]any{
			"deleted": deleted,
			"errors":  errors,
		})
	})
}
