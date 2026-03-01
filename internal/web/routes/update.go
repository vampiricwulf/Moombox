package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/updater"
)

// SharedUpdateInfo is the package-level atomic pointer storing the latest
// available update info. Set by main.go, read by status route and update routes.
var SharedUpdateInfo atomic.Pointer[updater.ReleaseInfo]

// UpdateRouteDeps holds dependencies for the update routes.
type UpdateRouteDeps struct {
	Updater          *updater.Updater
	Version          string
	Cfg              *config.MoomboxConfig
	ConfigPath       string
	RestartRequested *atomic.Bool
	UpdateRestart    *atomic.Bool // signals main() to exec new binary
	CancelFunc       context.CancelFunc
	QuitTUI          *func()
}

// UpdateRoutes registers the update check/apply/dismiss API endpoints.
func UpdateRoutes(r chi.Router, deps *UpdateRouteDeps) {
	// GET /api/v1/update/status — current update status
	r.Get("/api/v1/update/status", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"currentVersion": deps.Version,
			"available":      false,
		}
		if ui := SharedUpdateInfo.Load(); ui != nil {
			resp["available"] = true
			resp["version"] = ui.Version
			resp["tagName"] = ui.TagName
			resp["releaseNotes"] = ui.ReleaseNotes
			resp["publishedAt"] = ui.PublishedAt
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// POST /api/v1/update/check — manually trigger an update check
	r.Post("/api/v1/update/check", func(w http.ResponseWriter, r *http.Request) {
		if deps.Updater == nil {
			jsonError(w, "updater not available", http.StatusServiceUnavailable)
			return
		}

		release, err := deps.Updater.CheckForUpdate()
		if err != nil {
			jsonError(w, "check failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		resp := map[string]any{
			"currentVersion": deps.Version,
			"available":      false,
		}
		if release != nil {
			SharedUpdateInfo.Store(release)
			resp["available"] = true
			resp["version"] = release.Version
			resp["tagName"] = release.TagName
			resp["releaseNotes"] = release.ReleaseNotes
			resp["publishedAt"] = release.PublishedAt
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// POST /api/v1/update/apply — download update and restart
	r.Post("/api/v1/update/apply", func(w http.ResponseWriter, r *http.Request) {
		if deps.Updater == nil {
			jsonError(w, "updater not available", http.StatusServiceUnavailable)
			return
		}

		release := SharedUpdateInfo.Load()
		if release == nil {
			jsonError(w, "no update available", http.StatusBadRequest)
			return
		}

		if err := deps.Updater.ApplyUpdate(release); err != nil {
			jsonError(w, "update failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true})

		// Flush the response before triggering restart to ensure the
		// client receives the success response.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Trigger restart after response is flushed
		go func() {
			deps.RestartRequested.Store(true)
			deps.UpdateRestart.Store(true)
			deps.CancelFunc()
			if deps.QuitTUI != nil && *deps.QuitTUI != nil {
				(*deps.QuitTUI)()
			}
		}()
	})

	// POST /api/v1/update/dismiss — disable auto-check and clear update info
	r.Post("/api/v1/update/dismiss", func(w http.ResponseWriter, r *http.Request) {
		deps.Cfg.Updates.AutoCheckUpdates = false
		SharedUpdateInfo.Store(nil)

		if err := config.Save(deps.Cfg, deps.ConfigPath); err != nil {
			jsonError(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	})
}
