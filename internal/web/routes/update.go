package routes

import (
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
	Updater    *updater.Updater
	Version    string
	Cfg        *config.MoomboxConfig
	ConfigPath string
	OnRestart  func()
	OnFound    func(*updater.ReleaseInfo) // broadcast update to WebSocket + TUI
}

// UpdateRoutes registers the update check/apply/dismiss API endpoints.
func UpdateRoutes(r chi.Router, deps *UpdateRouteDeps) {
	// GET /api/update/status — current update status
	r.Get("/api/update/status", func(w http.ResponseWriter, r *http.Request) {
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
		jsonResponse(w, resp)
	})

	// POST /api/update/check — manually trigger an update check
	r.Post("/api/update/check", func(w http.ResponseWriter, r *http.Request) {
		if deps.Updater == nil {
			jsonError(w, "updater not available", http.StatusServiceUnavailable)
			return
		}

		release, err := deps.Updater.CheckForUpdate(r.Context())
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
			if deps.OnFound != nil {
				deps.OnFound(release)
			}
			resp["available"] = true
			resp["version"] = release.Version
			resp["tagName"] = release.TagName
			resp["releaseNotes"] = release.ReleaseNotes
			resp["publishedAt"] = release.PublishedAt
		}

		jsonResponse(w, resp)
	})

	// POST /api/update/apply — download update and restart
	r.Post("/api/update/apply", func(w http.ResponseWriter, r *http.Request) {
		if deps.Updater == nil {
			jsonError(w, "updater not available", http.StatusServiceUnavailable)
			return
		}

		release := SharedUpdateInfo.Load()
		if release == nil {
			jsonError(w, "no update available", http.StatusBadRequest)
			return
		}

		if err := deps.Updater.ApplyUpdate(r.Context(), release); err != nil {
			jsonError(w, "update failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		jsonResponse(w, map[string]any{"success": true})

		// Flush the response before triggering restart to ensure the
		// client receives the success response.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Trigger restart after response is flushed
		go func() {
			defer func() {
				recover() // restart panics are non-recoverable; prevent process crash
			}()
			if deps.OnRestart != nil {
				deps.OnRestart()
			}
		}()
	})

	// POST /api/update/verify — verify current binary's signature
	r.Post("/api/update/verify", func(w http.ResponseWriter, r *http.Request) {
		if deps.Updater == nil {
			jsonError(w, "updater not available", http.StatusServiceUnavailable)
			return
		}
		if err := deps.Updater.VerifyCurrentSignature(r.Context()); err != nil {
			jsonResponse(w, map[string]any{"error": err.Error()})
			return
		}
		jsonResponse(w, map[string]any{"verified": true})
	})

	// POST /api/update/dismiss — disable auto-check and clear update info
	r.Post("/api/update/dismiss", func(w http.ResponseWriter, r *http.Request) {
		deps.Cfg.Updates.AutoCheckUpdates = false
		SharedUpdateInfo.Store(nil)

		if err := config.Save(deps.Cfg, deps.ConfigPath); err != nil {
			jsonError(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
			return
		}

		jsonResponse(w, map[string]any{"success": true})
	})
}
