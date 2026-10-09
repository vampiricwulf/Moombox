package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// FileRoutesDeps holds dependencies for file route handlers.
type FileRoutesDeps struct {
	DB     *database.Database
	Store  *config.Store
	Logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// FileRoutes registers orphaned file management endpoints.
//
// The orphan scan/delete helpers receive a Store.Snapshot() rather than
// the live config pointer — they keep reading path fields while the scan
// walks the filesystem, which would race config.Store.Update mutating
// the shared struct in place.
func FileRoutes(r chi.Router, deps *FileRoutesDeps) {
	// GET /api/files/orphaned — scan and return orphaned files
	r.Get("/api/files/orphaned", func(rw http.ResponseWriter, req *http.Request) {
		entries, err := worker.ScanOrphanedFiles(deps.DB, deps.Store.Snapshot())
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
	//
	// Each path is decided on its own, as the terminal's Delete All decides
	// them: what can go goes, and each refusal is named in errors. A path the
	// delete refuses because it is no longer an orphan — a row names it now,
	// a finalize is writing it, its job needs its staging — means the list the
	// request came from is stale, which is a conflict with the server's state
	// rather than a failure: the answer is 409, the same {deleted, errors}
	// body plus an error telling the operator to refresh the list. Any other
	// refusal keeps the 200 and its fixed "failed to delete file".
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

		deleted := []string{}
		failures := []map[string]string{}
		var stale []string // the NotOrphanError messages, in request order

		cfg := deps.Store.Snapshot()
		for _, path := range body.Paths {
			err := worker.DeleteOrphanedFile(path, deps.DB, cfg)
			var notOrphan *worker.NotOrphanError
			switch {
			case err == nil:
				deleted = append(deleted, path)
				deps.Logger.Info("deleted orphaned file", "path", path)
			case errors.As(err, &notOrphan):
				failures = append(failures, map[string]string{"path": path, "error": notOrphan.Error()})
				stale = append(stale, notOrphan.Error())
				deps.Logger.Info("refused to delete a file that is no longer an orphan",
					"path", path, "owner", notOrphan.Owner)
			default:
				failures = append(failures, map[string]string{
					"path":  path,
					"error": "failed to delete file",
				})
				deps.Logger.Warn("failed to delete orphaned file", "path", path, "err", err)
			}
		}

		result := map[string]any{
			"deleted": deleted,
			"errors":  failures,
		}
		if len(stale) > 0 {
			msg := stale[0]
			if len(stale) > 1 {
				msg = fmt.Sprintf("%d of these are no longer orphans. Refresh the list.", len(stale))
			}
			result["error"] = msg
			// Content-Type before the explicit WriteHeader — headers set after
			// it are dropped.
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(rw).Encode(result)
			return
		}
		jsonResponse(rw, result)
	})
}
