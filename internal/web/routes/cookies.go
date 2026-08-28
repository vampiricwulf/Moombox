package routes

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// cookieRefreshOutcome renders one refresh pass onto the wire.
//
// Three of the fields are independent facts the manual-refresh toast needs and
// none of them can be derived from another: verdict, ran and renewed. The
// fourth, success, is the legacy alias for verdict == "ok" and is kept because
// it is the only one a pre-existing caller reads.
//
// Everything but success is therefore ADDITIVE: success keeps its exact
// historical meaning ("at least one platform is conclusively authenticated"),
// so an older frontend against a newer binary behaves as before. `renewed` set
// that precedent and `ran`/`verdict` follow it.
//
//   - success — can we do authenticated work at all?
//   - renewed — did THIS pass produce the credentials it verified? A working
//     cookies.txt outlives a browser refresh that did nothing, because the
//     independent 30-minute RefreshService keeps the session alive. False here
//     means "could not confirm", never "the browser failed".
//   - verdict — what the pass CONCLUDED: "ok", "failed", or "unknown". Only
//     "failed" is a conclusive negative and only it may be worded as a
//     verification failure.
//   - ran — did the pass do any work at all? This is what splits the two very
//     different events inside "unknown". The single refresh slot is held by
//     the periodic tick and by interactive setup, so clicking "Refresh now"
//     during either returns a pass that never looked at anything — and that
//     was being reported as "auth verification failed", in the same payload
//     whose cookieStatus said the session was authenticated.
//
// The three-way wording of a non-success — declined / ran-but-inconclusive /
// conclusively failed — follows cookieRefreshReportFor in
// cmd/moombox/services.go, which draws the same line for the worker's log.
//
// Deliberately NOT derived from the cookieStatus block the handler adds
// alongside: that comes from RefreshService's own check, a different mechanism
// that can satisfy the same observable, and reading a verdict off it would put
// the assertion downstream of the junction rather than at the pass itself.
func cookieRefreshOutcome(result cookies.RefreshResult) map[string]any {
	return map[string]any{
		"success": result.AnyVerified(),
		"renewed": result.Renewed,
		"verdict": result.Overall().String(),
		"ran":     result.Ran,
	}
}

// cookieSetupOutcome renders one interactive setup onto the wire.
//
// Two facts per platform, and the whole point is that they can disagree:
//
//   - authenticated / twitchAuthenticated — did the setup ACCEPT the sign-in?
//     This is what lights the "cookies configured" badge and what goes into
//     active_platforms. Its meaning is UNCHANGED: a sign-in the user just
//     completed is accepted when the site could not answer, exactly as before.
//   - youtubeVerification / twitchVerification — what the auth check
//     CONCLUDED: "ok", "failed" or "unknown". Only "failed" is a conclusive
//     negative and only it may be worded as a verification failure.
//
// The pair (accepted, "unknown") is the state this exists for and the one the
// dialog could not say: the cookies are saved and in use, and Moombox could
// not reach the site to confirm them. FinishSetup has computed it since the
// tri-state landed; it survived only as a server log line, so a user whose
// network blipped during the check was told their login failed.
//
// The two verification fields are ADDITIVE. `authenticated` keeps its exact
// historical meaning, so an older frontend against a newer binary behaves as
// it did; and the new frontend branches POSITIVELY on the strings ("=== ok",
// "=== unknown"), so against an older binary that omits them it degrades to
// the unqualified copy rather than to the hedged one. Same precedent
// `ran`/`verdict` set for the refresh outcome.
//
// The vocabulary is RefreshVerdict's, deliberately shared rather than
// re-invented: the manual-refresh surfaces already say "failed" / "could not
// establish" / "nothing was learned", and a fourth phrasing for the same three
// states is how the copy drifts apart.
func cookieSetupOutcome(result cookies.SetupResult) map[string]any {
	return map[string]any{
		"success":             true,
		"authenticated":       result.YouTubeAccepted,
		"twitchAuthenticated": result.TwitchAccepted,
		"youtubeVerification": result.YouTube.String(),
		"twitchVerification":  result.Twitch.String(),
	}
}

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

		// Detailed, not the bool. This is the manual refresh button — the
		// single most direct place an operator asks "did the browser refresh
		// actually do anything" — and the bool cannot answer it: it answered
		// unconditionally "successful" while the Last refresh line beside it
		// silently refused to advance (on Linux, where a launch can never be
		// confirmed to have acted, permanently), and it reported a verification
		// failure for a pass that never ran one. See cookieRefreshOutcome.
		result, err := autoCookieSvc.RefreshCookiesDetailed(req.Context())
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
			// S9's abort: Moombox could not read the existing cookies.txt and
			// deliberately did not write to it. This carries the only
			// actionable detail the operator has — same reasoning as the two
			// cases above — and it is the one message that must NOT be
			// flattened to "cookie refresh failed", which reads as "replace
			// your cookies" and would send the operator to overwrite the
			// exact file this abort just refused to destroy.
			case errors.Is(err, cookies.ErrCookieFileUnreadable):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			default:
				jsonError(rw, "cookie refresh failed", http.StatusInternalServerError)
			}
			return
		}

		// Re-check auth status after browser refresh
		refreshSvc.CheckNow(req.Context())
		status := refreshSvc.GetStatus()

		response := cookieRefreshOutcome(result)
		response["cookieStatus"] = map[string]any{
			"found":         status.HasYouTubeCookies,
			"authenticated": status.YouTubeAuthenticated,
		}
		response["twitchAuthStatus"] = map[string]any{
			"authenticated": status.TwitchAuthenticated,
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
			// Shutdown, not a fault: the service latched stopped and will not
			// launch another browser. 503 rather than the 409 the two
			// in-progress cases get, because this one never clears.
			case errors.Is(err, cookies.ErrServiceStopped):
				jsonError(rw, err.Error(), http.StatusServiceUnavailable)
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

		result, err := autoCookieSvc.FinishSetupDetailed(req.Context())
		if err != nil {
			switch {
			case errors.Is(err, cookies.ErrNoSetupInProgress):
				jsonError(rw, err.Error(), http.StatusNotFound)
			case errors.Is(err, cookies.ErrSetupCancelled):
				jsonError(rw, err.Error(), http.StatusConflict)
			// readFirefoxCookies now reports an unreadable profile loudly
			// instead of returning an empty jar, so a broken profile reaches
			// the user as that rather than a bare 500. (An EMPTY profile is
			// not here, for EITHER browser family: FinishSetup translates it
			// to "no login detected" and returns no error at all, which the
			// setup dialog renders inline. Chromium reached the default 500
			// below until cdpGetCookiesAsNetscape learned to tell an empty
			// profile from a failed read.)
			case errors.Is(err, cookies.ErrCookieDBNotFound),
				errors.Is(err, cookies.ErrCookieDBUnreadable):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			case errors.Is(err, cookies.ErrCookieDBLocked):
				jsonError(rw, err.Error(), http.StatusConflict)
			// S9's abort: Moombox could not read the existing cookies.txt
			// before merging in the cookies this setup call just extracted,
			// and deliberately did not write anything. Passed through
			// verbatim for the same reason as the two cases above — and it
			// must not fall to "failed to finish setup", which gives no hint
			// that the fix is a permissions/mount problem rather than
			// running setup again.
			case errors.Is(err, cookies.ErrCookieFileUnreadable):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			default:
				jsonError(rw, "failed to finish setup", http.StatusInternalServerError)
			}
			return
		}
		jsonResponse(rw, cookieSetupOutcome(result))
	})

	// POST /api/cookies/auto-setup/cancel
	heavy.Post("/api/cookies/auto-setup/cancel", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		// Same shape as the finish handler's ErrNoSetupInProgress arm above,
		// deliberately: both endpoints are answering "was there a setup here
		// to act on?" and there is no reason for the two to disagree about
		// what a missing one looks like on the wire. Before this, cancel
		// answered {"success": true} unconditionally — a second cancel, or a
		// cancel with no setup ever started, reported a cancel that never
		// happened.
		if err := autoCookieSvc.CancelSetup(); err != nil {
			switch {
			case errors.Is(err, cookies.ErrNoSetupInProgress):
				jsonError(rw, err.Error(), http.StatusNotFound)
			default:
				jsonError(rw, "failed to cancel setup", http.StatusInternalServerError)
			}
			return
		}
		jsonResponse(rw, map[string]any{"success": true})
	})

	// POST /api/cookies/auto-setup/abandon
	//
	// The unload beacon's endpoint, and deliberately NOT /cancel. A user
	// pressing Cancel consents to the setup browser closing; a dashboard tab
	// unloading does not — and since the Firefox setup gained a Job Object,
	// every cancel closes that window. The flow tells the user to leave this
	// tab and go sign in, so closing it had become a remote kill of the window
	// they were typing into. See AbandonSetup for what this does instead and
	// why it is a no-op on Windows and load-bearing everywhere else.
	//
	// Answers a missing setup exactly as /cancel and /finish do; the beacon
	// cannot read any of it, but a 404 must not read as a server fault to
	// anything that can.
	heavy.Post("/api/cookies/auto-setup/abandon", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		released, err := autoCookieSvc.AbandonSetup()
		if err != nil {
			switch {
			case errors.Is(err, cookies.ErrNoSetupInProgress):
				jsonError(rw, err.Error(), http.StatusNotFound)
			default:
				jsonError(rw, "failed to release setup", http.StatusInternalServerError)
			}
			return
		}
		jsonResponse(rw, map[string]any{"success": true, "released": released})
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
