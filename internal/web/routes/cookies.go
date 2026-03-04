package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// CookieRoutes registers cookie-related API routes.
func CookieRoutes(r chi.Router, refreshSvc *cookies.RefreshService, autoCookieSvc *cookies.AutoCookieService, getActivePlatforms func() map[string]bool) {
	// POST /api/cookies/recheck
	r.Post("/api/cookies/recheck", func(rw http.ResponseWriter, req *http.Request) {
		refreshSvc.CheckNow(req.Context())
		status := refreshSvc.GetStatus()
		response := map[string]any{
			"success": status.YouTubeAuthenticated || status.TwitchAuthenticated,
			"cookieStatus": map[string]any{
				"found":         status.HasYouTubeCookies,
				"authenticated": status.YouTubeAuthenticated,
			},
			"twitchAuthStatus": map[string]any{
				"authenticated": status.TwitchAuthenticated,
			},
		}
		if autoCookieSvc != nil {
			autoStatus := autoCookieSvc.GetStatus()
			response["autoCookieReloginRequired"] = autoStatus.NeedsManualRelogin
		} else {
			response["autoCookieReloginRequired"] = cookies.AutoCookieReloginRequired{}
		}
		if getActivePlatforms != nil {
			response["activePlatforms"] = getActivePlatforms()
		}
		jsonResponse(rw, response)
	})

	// POST /api/cookies/auto-refresh — trigger headless browser cookie refresh
	r.Post("/api/cookies/auto-refresh", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		ok, err := autoCookieSvc.RefreshCookies(req.Context())
		if err != nil {
			jsonResponse(rw, map[string]any{"success": false, "error": err.Error()})
			return
		}

		// Re-check auth status after browser refresh
		refreshSvc.CheckNow(req.Context())
		status := refreshSvc.GetStatus()

		response := map[string]any{
			"success": ok,
			"cookieStatus": map[string]any{
				"found":         status.HasYouTubeCookies,
				"authenticated": status.YouTubeAuthenticated,
			},
			"twitchAuthStatus": map[string]any{
				"authenticated": status.TwitchAuthenticated,
			},
		}
		autoStatus := autoCookieSvc.GetStatus()
		response["autoCookieReloginRequired"] = autoStatus.NeedsManualRelogin
		if getActivePlatforms != nil {
			response["activePlatforms"] = getActivePlatforms()
		}
		jsonResponse(rw, response)
	})

	// POST /api/cookies/auto-setup/start
	r.Post("/api/cookies/auto-setup/start", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			Platform string `json:"platform"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			body.Platform = "youtube"
		}
		if body.Platform == "" {
			body.Platform = "youtube"
		}

		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		if err := autoCookieSvc.StartSetup(body.Platform); err != nil {
			jsonResponse(rw, map[string]any{"success": false, "error": err.Error()})
			return
		}
		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/cookies/auto-setup/finish
	r.Post("/api/cookies/auto-setup/finish", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		ytAuth, twAuth, err := autoCookieSvc.FinishSetup(req.Context())
		if err != nil {
			jsonResponse(rw, map[string]any{
				"success":              false,
				"authenticated":        false,
				"twitchAuthenticated":  false,
				"error":               err.Error(),
			})
			return
		}
		jsonResponse(rw, map[string]any{
			"success":             true,
			"authenticated":       ytAuth,
			"twitchAuthenticated": twAuth,
		})
	})

	// POST /api/cookies/auto-setup/cancel
	r.Post("/api/cookies/auto-setup/cancel", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		autoCookieSvc.CancelSetup()
		jsonResponse(rw, map[string]any{"success": true})
	})

	// GET /api/cookies/auto-status
	r.Get("/api/cookies/auto-status", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonResponse(rw, map[string]any{
				"configured":         false,
				"setupInProgress":    false,
				"browser":            nil,
				"lastRefresh":        nil,
				"lastError":          nil,
				"needsManualRelogin": cookies.AutoCookieReloginRequired{},
			})
			return
		}

		jsonResponse(rw, autoCookieSvc.GetStatus())
	})
}
