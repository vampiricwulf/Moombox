package routes

import (
	"encoding/json"
	"errors"
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
			// Always emit both supported platforms so the frontend doesn't
			// need to handle a missing-key fallback (audit cookies.md #44).
			response["autoCookieReloginRequired"] = cookies.AutoCookieReloginRequired{
				"youtube": false,
				"twitch":  false,
			}
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

		// Detailed, for `renewed` alone. This is the manual refresh button —
		// the single most direct place an operator asks "did the browser
		// refresh actually do anything", and the one place that answered
		// unconditionally "successful" while the Last refresh line beside it
		// silently refused to advance. On Linux, where a launch can never be
		// confirmed to have acted, that pairing is the permanent steady state.
		result, err := autoCookieSvc.RefreshCookiesDetailed(req.Context())
		ok := result.YouTube == cookies.RefreshOK || result.Twitch == cookies.RefreshOK
		if err != nil {
			// Discriminate sentinel errors so the frontend can surface a
			// useful message (and so XHR callers with `if (!response.ok)`
			// branches treat this as a real error rather than the previous
			// HTTP 200 + {success:false,...} that was easy to mis-handle).
			switch {
			case errors.Is(err, cookies.ErrNoBrowserFound):
				jsonError(rw, "no supported browser installed", http.StatusFailedDependency)
			case errors.Is(err, cookies.ErrProfileNotFound),
				errors.Is(err, cookies.ErrProfileNotADirectory):
				jsonError(rw, "browser profile not found — run setup first", http.StatusNotFound)
			// Browser-free profile import failures. These carry the only
			// actionable detail the operator has (there is no browser UI in a
			// container), so pass the message through verbatim instead of
			// flattening it to "cookie refresh failed".
			case errors.Is(err, cookies.ErrCookieDBNotFound),
				errors.Is(err, cookies.ErrNoCookiesInProfile):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			case errors.Is(err, cookies.ErrCookieDBLocked):
				jsonError(rw, err.Error(), http.StatusConflict)
			case errors.Is(err, cookies.ErrCookieDBUnreadable):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			default:
				jsonError(rw, "cookie refresh failed", http.StatusInternalServerError)
			}
			return
		}

		// Re-check auth status after browser refresh
		refreshSvc.CheckNow(req.Context())
		status := refreshSvc.GetStatus()

		response := map[string]any{
			"success": ok,
			// renewed splits `success` into the two things it was conflating:
			// "the credentials work" (success) and "this pass produced them"
			// (renewed). Additive — success keeps its exact old meaning, so an
			// older frontend against a newer binary behaves as before.
			//
			// False here does NOT mean the browser failed. With no Job Object
			// to drain we cannot tell a browser that did nothing from one that
			// finished after we looked, so the copy must stop at "could not
			// confirm".
			"renewed": result.Renewed,
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
			switch {
			case errors.Is(err, cookies.ErrSetupInProgress),
				errors.Is(err, cookies.ErrRefreshInProgress):
				jsonError(rw, err.Error(), http.StatusConflict)
			case errors.Is(err, cookies.ErrNoBrowserFound):
				jsonError(rw, "no supported browser installed", http.StatusFailedDependency)
			default:
				jsonError(rw, "failed to start setup", http.StatusInternalServerError)
			}
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
			switch {
			case errors.Is(err, cookies.ErrNoSetupInProgress):
				jsonError(rw, err.Error(), http.StatusNotFound)
			case errors.Is(err, cookies.ErrSetupCancelled):
				jsonError(rw, err.Error(), http.StatusConflict)
			// readFirefoxCookies now reports an unreadable profile loudly
			// instead of returning an empty jar, so a broken profile reaches
			// the user as that rather than a bare 500. (An EMPTY profile is
			// not here: FinishSetup translates it to "no login detected",
			// which the setup dialog renders inline.)
			case errors.Is(err, cookies.ErrCookieDBNotFound),
				errors.Is(err, cookies.ErrCookieDBUnreadable):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			case errors.Is(err, cookies.ErrCookieDBLocked):
				jsonError(rw, err.Error(), http.StatusConflict)
			default:
				jsonError(rw, "failed to finish setup", http.StatusInternalServerError)
			}
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

	// POST /api/auto-cookies/validate-browser-path validates a user-specified
	// browser executable for the auto-cookies extraction. Used by the setup
	// UI's "Custom path…" dropdown option to give immediate feedback before
	// save. Rate-limited because it triggers a subprocess (--version).
	heavy.Post("/api/auto-cookies/validate-browser-path", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			Path string `json:"path"`
			Type string `json:"type"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := cookies.ValidateBrowserPath(req.Context(), body.Path, body.Type); err != nil {
			jsonResponse(rw, map[string]any{"valid": false, "error": err.Error()})
			return
		}
		jsonResponse(rw, map[string]any{"valid": true})
	})

	// GET /api/cookies/auto-status
	r.Get("/api/cookies/auto-status", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonResponse(rw, map[string]any{
				"configured":      false,
				"setupInProgress": false,
				"browser":         nil,
				"lastRefresh":     nil,
				"lastError":       nil,
				// Both supported platforms always present — see audit
				// cookies.md #44.
				"needsManualRelogin": cookies.AutoCookieReloginRequired{
					"youtube": false,
					"twitch":  false,
				},
			})
			return
		}

		jsonResponse(rw, autoCookieSvc.GetStatus())
	})
}
