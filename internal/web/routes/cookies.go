package routes

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// CookieRoutes registers cookie-related API routes. The optional rate
// limiter wraps the headless-browser endpoints (/auto-refresh and the
// auto-setup start/finish/cancel trio) so a buggy or hostile client
// can't trigger a flurry of browser process launches — an expensive
// operation per audit reports/web.md S-7.
func CookieRoutes(r chi.Router, refreshSvc *cookies.RefreshService, autoCookieSvc *cookies.AutoCookieService, getActivePlatforms func() map[string]bool, rl *web.RateLimiter) {
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

	// Sub-router for endpoints that spawn or steer a headless browser. The
	// underlying AutoCookieService already serialises with refreshCmd so
	// concurrent calls fast-fail, but rate-limiting the request flow stops
	// a caller from burning CPU on the fast-fail path.
	heavy := r.With(func(h http.Handler) http.Handler {
		if rl == nil {
			return h
		}
		return rl.Middleware(h)
	})

	// POST /api/cookies/auto-refresh — trigger headless browser cookie refresh
	heavy.Post("/api/cookies/auto-refresh", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		ok, err := autoCookieSvc.RefreshCookies(req.Context())
		if err != nil {
			// Return 500 so XHR callers with `if (!response.ok)` branches
			// treat this as a real error. Previously the handler replied
			// HTTP 200 with {success:false,...}, which the frontend could
			// easily mis-handle as a successful call.
			jsonError(rw, "cookie refresh failed", http.StatusInternalServerError)
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
	heavy.Post("/api/cookies/auto-setup/start", func(rw http.ResponseWriter, req *http.Request) {
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
			jsonError(rw, "failed to start setup", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/cookies/auto-setup/finish
	heavy.Post("/api/cookies/auto-setup/finish", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		ytAuth, twAuth, err := autoCookieSvc.FinishSetup(req.Context())
		if err != nil {
			jsonError(rw, "failed to finish setup", http.StatusInternalServerError)
			return
		}
		jsonResponse(rw, map[string]any{
			"success":             true,
			"authenticated":       ytAuth,
			"twitchAuthenticated": twAuth,
		})
	})

	// POST /api/cookies/auto-setup/cancel
	heavy.Post("/api/cookies/auto-setup/cancel", func(rw http.ResponseWriter, req *http.Request) {
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
