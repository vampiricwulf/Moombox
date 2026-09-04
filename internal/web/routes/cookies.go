package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
//   - mechanism — WHICH cookie source ran: "browser", "profile-import", or ""
//     when the pass declined before it chose one. Wording only: it exists so a
//     post-flight toast stops saying "Browser cookie refresh ..." after an
//     import that launched nothing, and nothing may branch a decision on it.
//     Additive like `ran` and `verdict` before it — an older frontend ignores it, and a newer
//     frontend against an older binary reads undefined and falls back to
//     cookies.acquisition, which is what it already used for the pre-flight
//     toast.
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
		"success":   result.AnyVerified(),
		"renewed":   result.Renewed,
		"verdict":   result.Overall().String(),
		"ran":       result.Ran,
		"mechanism": result.Mechanism,
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

// CookieStatusPayload renders AuthStatus's YouTube half onto the wire, and
// TwitchAuthStatusPayload the Twitch half.
//
// ONE copy of each shape, exported, because there were three hand-written
// copies of the first and two of the second across two packages — GET
// /api/status (cmd/moombox/routes_wiring.go), POST /api/cookies/recheck and
// POST /api/cookies/auto-refresh — and a field added to two of three is the
// junction defect this arc keeps finding: the badge silently keeps its old
// meaning on whichever endpoint was missed, which for `verification` means the
// dashboard reverts to "not authenticated" the moment the user reloads.
//
//   - found — was this install ever CONFIGURED for the platform? The loose
//     predicate, not "is the cookie set complete": a Twitch session whose
//     auth-token was pruned on expiry is a configured session with no
//     credential, which is a different thing to say than "no cookies".
//   - authenticated — UNCHANGED in meaning, on purpose. It is the only key a
//     pre-existing frontend reads, and it stays "can we do authenticated work
//     right now", false on an inconclusive check.
//   - verification — what the check CONCLUDED: "ok", "failed" or "unknown".
//     Only "failed" is a conclusive negative and only it may be worded as one.
//   - youtubeError / twitchError — WHY the check could not conclude. Empty
//     whenever it did. This is the half `verification` cannot carry: the UI
//     could say "could not check" and never say what stopped it, so an install
//     behind a captive portal, a rate limit and an intercepting proxy all
//     rendered identically and none of them named the thing to fix.
//
// SAFE TO RENDER, and that is a property of the producers rather than of this
// projection — checked at both of them before this was wired, not assumed. Every
// string that can reach these two fields is status-and-cause only: refresh.go's
// checkAndRefreshYouTube and checkTwitchAuth name a status code, a scheme+host,
// a header NAME, a transport error over a constant URL, or one of two static
// sentinels, and no branch of either interpolates a response body. The one
// error that is ABOUT a body — errGuideLoginMarkerUnreadable — is a fixed
// sentence naming no host, no header and no bytes, pinned by
// TestUnreadableGuideErrorCarriesNoBody. internal/twitch's ValidateToken, which
// did echo up to 1 MB of body until Arc 8 Task 9 clamped it, is NOT on this
// path at all. AuthStatus.TwitchError has exactly TWO producers, both in
// refresh.go and both inside that rule: its own checkTwitchAuth, and — since
// Arc 10 — RefreshService.NoteTwitchAuthLoss, whose reason renders through
// twitchAuthLossMessage, a switch whose every arm returns a string LITERAL, so
// the set of sentences it can produce is closed at compile time and no caller
// can widen it. Anything added to any producer must keep that rule, or this
// projection puts an intermediary's HTML on the dashboard.
//
// `verification` (and, for Twitch, `found`) are ADDITIVE, by the precedent
// `renewed` set and `ran`/`verdict` and the setup's two verification fields
// followed: an older frontend ignores the extra keys and behaves exactly as it
// did, and the new frontend branches POSITIVELY on the strings, so against an
// older binary that omits them it degrades to today's copy rather than to the
// hedged one. See cookieIndicatorState in web/public/modules/utils.js, whose
// arm order is what delivers the second half of that.
//
// The vocabulary is RefreshVerdict's, deliberately shared rather than
// re-invented — same reason as cookieSetupOutcome above.
func CookieStatusPayload(status cookies.AuthStatus) map[string]any {
	return map[string]any{
		"found":         status.HasYouTubeCookies,
		"authenticated": status.YouTubeAuthenticated,
		"verification":  status.YouTubeVerification.String(),
		"youtubeError":  status.YouTubeError,
	}
}

// TwitchAuthStatusPayload is CookieStatusPayload's Twitch counterpart. See
// there for the field contract — including the no-response-bodies rule that
// makes `twitchError` safe to render — ; `found` is new on this side because
// AuthStatus had no HasTwitchCookies to project until this arc.
func TwitchAuthStatusPayload(status cookies.AuthStatus) map[string]any {
	return map[string]any{
		"found":         status.HasTwitchCookies,
		"authenticated": status.TwitchAuthenticated,
		"verification":  status.TwitchVerification.String(),
		"twitchError":   status.TwitchError,
	}
}

// The `cause` values the two browser-read sentinels reach the frontend as.
//
// A SHORT STABLE TOKEN, never the sentinel's message. The message is prose that
// will be reworded; a frontend branch keyed on prose breaks silently the first
// time it is, and the wording is the half a human reads anyway — it still rides
// along as `error`. These are the machine half.
//
// One per sentinel, and the two must stay distinct for the same reason the
// sentinels are two: a blocked ladder is a state the operator changes, an
// unanswered read is the browser side having failed. Collapsing them on the
// wire would undo that distinction one layer after it was made.
const (
	causeBrowserLadderBlocked  = "browser-ladder-blocked"
	causeBrowserReadUnanswered = "browser-read-unanswered"
)

// jsonErrorCause is jsonError plus a machine-readable `cause`.
//
// Deliberately NOT a field on AutoCookieStatus, and not a widened jsonError.
// The cause describes THIS request's failure, not the service's state, so it
// belongs to the response that carries it; and every other jsonError call site
// in the tree answers a state that has no sentinel to name, so giving them all
// an empty key would teach the frontend a field that is absent exactly when it
// would be interesting.
func jsonErrorCause(w http.ResponseWriter, msg, cause string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "cause": cause})
}

// writeBrowserReadError answers the two browser-read sentinels and reports
// whether it did. Nothing is written when it returns false.
//
// ONE function for BOTH the setup-finish and the refresh handler, called ahead
// of each one's own switch. The residual that prompted these sentinels was
// reported against the setup wire, but cdpGetCookiesAsNetscape is reached from
// both endpoints — so teaching one of two consumers about an error introduced
// in the same change would be the junction defect rather than an inherited one,
// and two hand-written copies of the mapping would be free to drift on the
// status code.
//
// The message passes through VERBATIM, like the profile-import failures below
// it: cdpCookieReadOutcome composes the only description of what actually
// stopped the read, and it is the sentence the operator has to act on.
//
//   - blocked ladder → 409. Something on this machine is holding or
//     intercepting the debugging port; that is a CONDITION the operator changes
//     and then retries, the same shape as the locked cookie DB.
//   - unanswered read → 502. Moombox asked and the browser side produced
//     nothing at all. The failure is upstream of this server, which is what
//     separates it from a 500 (Moombox itself broke).
//
// NEITHER is cookies.IsNoBrowserProfile, so the dashboard's shift+click
// fallback to the in-process refresh is unaffected — correctly: both come from
// a pass that RAN, and a plain recheck cannot fix either.
func writeBrowserReadError(rw http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, cookies.ErrBrowserLadderBlocked):
		jsonErrorCause(rw, err.Error(), causeBrowserLadderBlocked, http.StatusConflict)
	case errors.Is(err, cookies.ErrBrowserReadUnanswered):
		jsonErrorCause(rw, err.Error(), causeBrowserReadUnanswered, http.StatusBadGateway)
	default:
		return false
	}
	return true
}

// maxCookieImportBytes caps POST /api/cookies/import.
//
// A Netscape export of a signed-in YouTube + Twitch profile is a few kilobytes;
// 512 KiB is three orders of magnitude of headroom and still inside the
// chain-wide 1 MiB MaxBodySize (internal/web/server.go), so THIS is the limit
// that fires and its message is the one the operator reads. MaxBytesReader
// wrappers NEST rather than override — an inner SMALLER limit errors first,
// which is the direction that makes this cap real; the import endpoint's 500 MB
// reader is exempted from MaxBodySize for the opposite reason.
const maxCookieImportBytes = 512 << 10

// readCookieImportBody pulls the Netscape text out of either accepted request
// shape, answers the client itself on every refusal, and reports whether it
// did. Nothing is written when it returns false.
//
// TWO shapes because the panel has two controls, and they are the two a browser
// can send without ceremony: the textarea POSTs its text/plain contents, the
// file picker POSTs a multipart form with the file in a `cookies` part. Every
// other content type is refused rather than guessed at — a JSON body from a
// well-meaning client would otherwise be read as cookie text and rejected with
// a sentence about Netscape format, which describes the wrong mistake.
//
// The empty-body check is here rather than in prepareCookieImport because an
// empty request is a REQUEST-shape problem: "that cookie file has no cookie
// rows" is a claim about a file, and the operator sent none.
func readCookieImportBody(rw http.ResponseWriter, req *http.Request) (string, bool) {
	req.Body = http.MaxBytesReader(rw, req.Body, maxCookieImportBytes)

	var raw []byte
	var err error
	switch contentType := req.Header.Get("Content-Type"); {
	case strings.HasPrefix(contentType, "multipart/form-data"):
		// Parsed explicitly so an oversize upload surfaces as the cap error
		// below rather than as "no cookies part" — FormFile would swallow the
		// MaxBytesError into a generic parse failure and answer the wrong
		// sentence.
		if perr := req.ParseMultipartForm(maxCookieImportBytes); perr != nil {
			err = perr
			break
		}
		file, _, ferr := req.FormFile("cookies")
		if ferr != nil {
			jsonError(rw, "the upload has no `cookies` file part", http.StatusBadRequest)
			return "", false
		}
		defer file.Close()
		raw, err = io.ReadAll(file)
	case contentType == "",
		strings.HasPrefix(contentType, "text/plain"),
		strings.HasPrefix(contentType, "application/octet-stream"):
		raw, err = io.ReadAll(req.Body)
	default:
		jsonError(rw, "send the cookie file as text/plain, or as a multipart upload in a `cookies` part",
			http.StatusUnsupportedMediaType)
		return "", false
	}

	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			jsonError(rw, fmt.Sprintf("that cookie file is larger than the %d KiB this endpoint accepts",
				maxCookieImportBytes/1024), http.StatusRequestEntityTooLarge)
			return "", false
		}
		jsonError(rw, "could not read the request body", http.StatusBadRequest)
		return "", false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		jsonError(rw, "no cookie data was supplied", http.StatusBadRequest)
		return "", false
	}
	return string(raw), true
}

// cookieImportOutcome renders one operator-supplied cookie import onto the
// wire.
//
// The KEY SET IS cookieSetupOutcome's, PLUS the per-platform import outcome,
// and the shared half is the point rather than a coincidence: the two answer
// the same question about the same three states, the dashboard already has copy
// for it (cookieSetupAcceptedToast / cookieSetupRejectedMessage in
// web/public/modules/utils.js), and a fourth phrasing of "saved, but we could
// not check them" is how the copy drifts apart. See cookieSetupOutcome for what
// each shared key means and why `authenticated` and `*Verification` can
// disagree. Pinned by TestCookieImportOutcomeSpeaksTheSetupOutcomeVocabulary.
//
//   - youtubeImport / twitchImport — what the import DID to that platform's
//     rows: "imported", "rolled-back", "rejected" or "unchanged". The setup has
//     no counterpart because a wizard finish has no paste to give back — it
//     extracts from a browser the user just signed into, and there is no second
//     generation of rows to choose between.
//
//     FOUR values on the wire, not five. ImportOutcome's zero value renders
//     "unknown", and no response carries it: every exit that leaves the
//     outcomes unset returns an error, and the handler answers all of those
//     with jsonError before reaching this projection. The zero value is a guard
//     inside the package (see ImportOutcome), not a state the dashboard has to
//     have copy for — and if a future exit ever returns one on a 200, that is
//     the bug, not a fifth case to render.
//
//     This is the key the toast could not be written without. After a rollback
//     `authenticated` is TRUE and the verification is "ok" — correctly, because
//     the previous credentials are in force and they work — so every existing
//     key says success while the operator's paste was thrown out. Additive, by
//     the precedent `renewed` and `ran`/`verdict` set: an older frontend ignores
//     it and behaves as it did, and the new frontend branches POSITIVELY on
//     "rolled-back", so against an older binary it reads undefined and falls
//     back to today's copy.
//
// Deliberately NOT carrying a cookieStatus block. That comes from
// RefreshService.GetStatus, whose pass has not run yet at the moment this is
// rendered — the re-check is detached and fires after the response — so it
// would be the PRE-import snapshot: a status block contradicting the verdict
// beside it, which is precisely the junction defect the two payload projections
// above exist to prevent.
func cookieImportOutcome(result cookies.ImportResult) map[string]any {
	return map[string]any{
		"success":             true,
		"authenticated":       result.YouTubeAccepted,
		"twitchAuthenticated": result.TwitchAccepted,
		"youtubeVerification": result.YouTube.String(),
		"twitchVerification":  result.Twitch.String(),
		"youtubeImport":       result.YouTubeOutcome.String(),
		"twitchImport":        result.TwitchOutcome.String(),
	}
}

// cookieImportOnlyKeys are the keys cookieImportOutcome carries that
// cookieSetupOutcome has no counterpart for. Written down rather than inferred
// so that adding a third one is a deliberate edit here and not a silent
// widening of the two payloads' shared vocabulary. See cookieImportOutcome for
// why these two exist and the setup's equivalents do not.
var cookieImportOnlyKeys = map[string]bool{"youtubeImport": true, "twitchImport": true}

// CookieRoutes registers cookie-related API routes. The optional rate
// limiter wraps the headless-browser endpoints (/auto-refresh and the
// auto-setup start/finish/cancel trio) so a buggy or hostile client
// can't trigger a flurry of browser process launches — an expensive
// operation per audit reports/web.md S-7.
func CookieRoutes(r chi.Router, refreshSvc *cookies.RefreshService, autoCookieSvc *cookies.AutoCookieService, getActivePlatforms func() map[string]bool, rl *web.RateLimiter) {
	// POST /api/cookies/recheck
	r.Post("/api/cookies/recheck", func(rw http.ResponseWriter, req *http.Request) {
		// CheckNow's bool — "did a pass actually run" — is deliberately ignored,
		// and this handler is the TUI's R C by another door, so it ignores it for
		// the same reason (see OnRecheckCookies in cmd/moombox/tui_wiring.go).
		// The payload below is a STATUS SNAPSHOT, not a claim that this request
		// produced it: every field is read from GetStatus and is true of the
		// credentials whichever pass last computed it. A collision with the
		// 30-minute ticker costs at most one snapshot of freshness.
		//
		// Surfacing "a refresh was already running" would need a new key and a
		// new sentence, and neither exists: the toast is rendered from
		// cookies.RecheckReport's per-platform verdicts (pinned across both UIs
		// by cookies_recheck_toast_test.go), and cookies.RefreshDeclinedCauses is
		// the BROWSER refresher's exhaustive set, pinned in three consumers —
		// this guard is the in-process refresh's own single-flight and must not
		// be reported through it.
		//
		// The guard is also why this route can stay on the plain router: with it
		// in place, hammering the endpoint cannot start a second pass.
		refreshSvc.CheckNow(req.Context())
		status := refreshSvc.GetStatus()
		response := map[string]any{
			"success":          status.YouTubeAuthenticated || status.TwitchAuthenticated,
			"cookieStatus":     CookieStatusPayload(status),
			"twitchAuthStatus": TwitchAuthStatusPayload(status),
		}
		if autoCookieSvc != nil {
			// ReloginStatus, not GetStatus: this response reads nothing but
			// the relogin map, and GetStatus's browser/registry detection
			// scan would otherwise run on every recheck — the TUI's R C by
			// another door (see the comment above).
			response["autoCookieReloginRequired"] = autoCookieSvc.ReloginStatus()
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
			// Ahead of the switch, and shared with the finish handler below —
			// see writeBrowserReadError for why one function answers both.
			if writeBrowserReadError(rw, err) {
				return
			}
			// Discriminate sentinel errors so the frontend can surface a
			// useful message (and so XHR callers with `if (!response.ok)`
			// branches treat this as a real error rather than the previous
			// HTTP 200 + {success:false,...} that was easy to mis-handle).
			switch {
			// Verbatim, NOT the static "no supported browser installed" this
			// used to substitute. Two states reach this sentinel on a refresh —
			// no browser is installed, or one is installed and
			// cookies.auto_enabled has switched headless runs off — and that
			// sentence is a claim only the first can support. Telling the
			// second operator to install a browser they already have is the
			// unearned cause this plan keeps finding: a sentence that outlived
			// the condition it was written for. RefreshCookiesDetailed writes
			// both messages and each says which cookie source was missing.
			//
			// The auto-setup/start handler below keeps the static string, and
			// correctly: StartSetup is never gated, so there the sentinel means
			// exactly one thing.
			case errors.Is(err, cookies.ErrNoBrowserFound):
				jsonError(rw, err.Error(), http.StatusFailedDependency)
			case errors.Is(err, cookies.ErrProfileNotFound):
				jsonError(rw, "browser profile not found — run setup first", http.StatusNotFound)
			// Browser-free profile import failures. These carry the only
			// actionable detail the operator has (there is no browser UI in a
			// container), so pass the message through verbatim instead of
			// flattening it to "cookie refresh failed".
			//
			// ErrProfileNotADirectory belongs HERE and not with the case above,
			// which is where it used to sit. It is the classic "bind-mounted
			// cookies.txt onto browser_profile_dir" mistake: the path exists,
			// the pass RAN, and the remedy is to fix the mount — so "browser
			// profile not found — run setup first" was wrong on both counts.
			// It is also what keeps the two manual surfaces in step: the
			// dashboard's shift+click falls back to the in-process refresh on
			// this arm's 404, and the TUI's R F does not fall back for this
			// error (see cookies.IsNoBrowserProfile). One gesture, one rule.
			// ErrProfileDirUnreadable belongs here for the same reason as
			// ErrProfileNotADirectory, and is the likelier of the two in a
			// container: a bind-mounted profile the process cannot stat because
			// the host owns it and compose's `user:` says otherwise. It used to
			// wrap ErrProfileNotFound, which routed it to the 404 above and so
			// into the dashboard's fallback — losing the permissions sentence
			// on the deployment that had no other way to find out.
			// ErrProfileDirNotOptedIn joins them for the same reason: the
			// profile is there, the pass reached a decision about it, and the
			// message names the one setting that changes the answer. Flattening
			// it would leave a Windows desktop operator with "cookie refresh
			// failed" and no way to learn that cookies.acquisition exists.
			case errors.Is(err, cookies.ErrProfileDirUnreadable),
				errors.Is(err, cookies.ErrProfileNotADirectory),
				errors.Is(err, cookies.ErrProfileDirNotOptedIn),
				errors.Is(err, cookies.ErrCookieDBNotFound),
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

		// Re-check auth status after browser refresh.
		//
		// This is the dashboard twin of the TUI's R F, which logs when CheckNow
		// reports that a refresh was already in flight (the pass already running
		// read the cookie file BEFORE this browser pass rewrote it, so the status
		// below can be pre-refresh and stays that way until the next tick). The
		// routes package has no operational logger — SetPanicLogger installs an
		// Error-only sink for recovered panics and is not one — so this side
		// records the same fact in a comment rather than borrowing that. The
		// refresh's OWN outcome comes from `result` via cookieRefreshOutcome and
		// is unaffected; only cookieStatus/twitchAuthStatus can lag.
		refreshSvc.CheckNow(req.Context())
		status := refreshSvc.GetStatus()

		response := cookieRefreshOutcome(result)
		response["cookieStatus"] = CookieStatusPayload(status)
		response["twitchAuthStatus"] = TwitchAuthStatusPayload(status)
		// ReloginStatus, not GetStatus: this response reads nothing but the
		// relogin map, and GetStatus's browser/registry detection scan would
		// otherwise run on every manual refresh click for a field this never
		// uses.
		response["autoCookieReloginRequired"] = autoCookieSvc.ReloginStatus()
		if getActivePlatforms != nil {
			response["activePlatforms"] = getActivePlatforms()
		}
		jsonResponse(rw, response)
	})

	// POST /api/cookies/import — the browser-free re-authentication path.
	//
	// The transport half of a mechanism that was otherwise complete: replacing
	// the bytes of cookies.txt already reaches jar.Reload, the guide check,
	// OnAuthRecovered and the parked-job resume. What was missing is a way to
	// deliver those bytes without shell or filesystem access to the volume,
	// which is every container deployment and every remote dashboard.
	//
	// ANY AUTHENTICATED CLIENT, not loopback-gated, by owner ruling. The setup
	// wizard's loopback gate protects an UNCLAIMED instance from being claimed
	// by a stranger; this endpoint exists only behind an authenticated session
	// on an already-claimed one, and a loopback-gated ingest would be useless
	// in precisely the deployment it is built for — the operator reaches the
	// dashboard over the LAN or a tunnel, never from inside the container. The
	// capability it grants an attacker who already holds a session and a CSRF
	// token is to install cookies they already possess, which is strictly
	// smaller than the session they already have.
	//
	// There is NO GET, and there must never be one, however natural it feels
	// beside an upload control. That single asymmetry — accepts credential
	// bytes, never serves them — is what keeps this from being an exfiltration
	// path. Pinned by TestCookieImportRouteHasNoGET.
	heavy.Post("/api/cookies/import", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			jsonError(rw, "auto-cookie service not configured", http.StatusServiceUnavailable)
			return
		}

		netscape, ok := readCookieImportBody(rw, req)
		if !ok {
			return
		}

		result, err := autoCookieSvc.ImportCookies(req.Context(), netscape)

		// The same deferred, detached, flushed re-check the setup-wizard finish
		// runs, and for the same reasons — see that handler for the full
		// derivation. In short: refresh's status block is the only place the
		// credential fingerprint is compared and the Twitch auth mark cleared;
		// a request-scoped context would let a closed tab cancel the comparison
		// its own import caused; and the Flush is what stops the client waiting
		// out a re-check it has already been answered for.
		//
		// Deferred rather than placed after the response so it covers the
		// jar-reload error exit too — the one error path that runs over a file
		// already replaced, and the one where a re-check is worth most, because
		// refresh's own jar.Reload repairs the stale in-memory jar.
		defer func() {
			if refreshSvc == nil || !result.Wrote {
				return
			}
			if f, ok := rw.(http.Flusher); ok {
				f.Flush()
			}
			recheckCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			refreshSvc.CheckNow(recheckCtx)
		}()

		if err != nil {
			switch {
			// The three refusals, verbatim. Each names the operator's next move
			// and none of them quotes the submitted text; flattening them to a
			// 500 would answer a bad export with a server fault.
			case errors.Is(err, cookies.ErrImportNotNetscape),
				errors.Is(err, cookies.ErrImportNoRows),
				errors.Is(err, cookies.ErrImportNoCredential):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			// S9's abort: Moombox could not read the existing cookies.txt and
			// deliberately did not write. Verbatim, for the reason the sentinel's
			// doc gives — the fix is the permission or the mount, and this is the
			// one message that must never read as "replace your cookies", over a
			// file the endpoint just refused to touch.
			case errors.Is(err, cookies.ErrCookieFileUnreadable):
				jsonError(rw, err.Error(), http.StatusUnprocessableEntity)
			// A CONDITION the operator changes and then retries — the same shape
			// as the locked cookie DB and the blocked ladder, and 409 for the
			// same reason. The message names the single-file bind mount, which
			// is what actually produces it in a container.
			case errors.Is(err, cookies.ErrCookieFileUnwritable):
				jsonError(rw, err.Error(), http.StatusConflict)
			// The paste was REJECTED and the rollback did not finish. Verbatim,
			// and AHEAD of the `result.Wrote` arm below, which is the arm both
			// of these used to fall into: `Wrote` is true here (cookies.txt was
			// replaced), so that arm answered "the cookies were imported and
			// written" about a paste Moombox had just judged dead and tried to
			// undo. False in both directions at once — the paste was not
			// accepted, and the state left behind is not one a re-check
			// repairs.
			//
			// The sentinel's own message is a summary; the wrapped sentence
			// says which of the two states this is (the rejected paste is still
			// on disk, or the file is right and this process is not), and that
			// distinction is the whole of what the operator can act on. See
			// cookies.ErrImportRollbackIncomplete.
			//
			// 500, not 409: unlike the unwritable-file arm above this is not a
			// condition the operator changes and retries into success — the
			// import already ran, was rejected, and left two things disagreeing
			// about which credentials are in force.
			case errors.Is(err, cookies.ErrImportRollbackIncomplete):
				jsonError(rw, err.Error(), http.StatusInternalServerError)
			// The write LANDED and this process could not load it. Saying "the
			// import failed" would be false about the file on disk, and would
			// send an operator to repeat an import that already worked.
			case result.Wrote:
				jsonError(rw, "the cookies were imported and written, but this process could not load "+
					"them — the auth re-check that follows re-reads the file", http.StatusInternalServerError)
			default:
				jsonError(rw, "cookie import failed", http.StatusInternalServerError)
			}
			return
		}

		response := cookieImportOutcome(result)
		// ReloginStatus, not GetStatus: this reads nothing but the relogin map,
		// which ImportCookies has just updated, and GetStatus's browser/registry
		// detection scan would otherwise run for a field this never uses.
		response["autoCookieReloginRequired"] = autoCookieSvc.ReloginStatus()
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

		// The dashboard twin of the TUI wizard's re-check: a completed wizard
		// has just rewritten cookies.txt, and refresh's status block is the only
		// place the credential fingerprint is compared, the Twitch auth mark
		// cleared and OnCredentialsChanged fired. Bare, with no log line, for
		// the reason given at the auto-refresh handler above — this package has
		// no operational logger.
		//
		// DETACHED from req.Context(), which is the one thing this call does
		// differently from its two neighbours, and deliberately. A client that
		// closes the page mid-pass cancels both auth checks, and
		// shouldObserveCredentials bails on a check error — so the Twitch
		// fan-out never runs, no live chat session is told, and the identity
		// baseline is not advanced either, which leaves the whole thing waiting
		// on the 30-minute ticker. That is precisely what R5 rules out, on the
		// most deliberate credential change there is. The neighbours are exempt
		// for reasons that do not apply here: /api/cookies/check writes nothing,
		// and /api/cookies/refresh has the same exposure but predates this.
		//
		// 45 s is a backstop above the pass's own worst case, not a budget it
		// is expected to spend: the shared HTTP client caps every request at
		// 30 s and each auth check wraps its own 15 s context on top, so a live
		// pass finishes well inside this. It exists so a wedged round-trip
		// cannot hold a detached context open indefinitely — and, since the
		// handler does not return until the deferred pass does, so it cannot
		// hold the connection open indefinitely either.
		//
		// Deferred, so the gate is evaluated independently of the error switch
		// below: the jar reload after a successful write returns an error over a
		// file that has already been replaced, and that is the one error exit
		// this re-check must not skip.
		//
		// The Flush is what makes "the client is not waiting on this" true. The
		// defer alone does not: jsonResponse and jsonError both write into
		// net/http's bufio writer and neither flushes, the handler does not
		// return until this defer completes, and Server.WriteTimeout is 0 — so
		// without it a browser on the setup dialog's 60 s AbortController waits
		// for FinishSetupDetailed PLUS up to 45 s of re-check and can abort a
		// setup that in fact succeeded. Flushing here commits the response
		// first; the gzip wrapper implements Flusher, and /api/update/apply
		// already relies on the same thing before it restarts the process.
		//
		// Inside the defer rather than after jsonResponse so it covers the
		// jar-reload error exit too, which answers through jsonError and is
		// precisely the error path that reaches the re-check.
		defer func() {
			if refreshSvc == nil || !result.Wrote {
				return
			}
			if f, ok := rw.(http.Flusher); ok {
				f.Flush()
			}
			recheckCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			refreshSvc.CheckNow(recheckCtx)
		}()

		if err != nil {
			// Ahead of the switch. Both browser-read failures used to fall to
			// its default arm, whose "failed to finish setup" this file's own
			// comment called out as giving no hint — and one of them, the
			// blocked ladder, is specifically the case where the dialog's other
			// answer ("no login detected") would be wrong, because the read
			// never got far enough to have a verdict.
			if writeBrowserReadError(rw, err) {
				return
			}
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
		// A successfully validated custom path is exactly the moment an
		// operator has proven a browser exists somewhere the routine
		// filesystem+registry scan may not have looked (or may have looked
		// before it was installed) — invalidate so the next status poll's
		// detection reflects it instead of riding out the remainder of the
		// 60s TTL.
		cookies.InvalidateBrowserDetection()
		jsonResponse(rw, map[string]any{"valid": true})
	})

	// GET /api/cookies/auto-status
	r.Get("/api/cookies/auto-status", func(rw http.ResponseWriter, req *http.Request) {
		if autoCookieSvc == nil {
			// Every key here has to exist on cookies.AutoCookieStatus too, or
			// this branch teaches the frontend a field the real one never
			// sends. `configured` used to sit at the top and was deleted with
			// the struct field — see the type's doc comment.
			jsonResponse(rw, map[string]any{
				"setupInProgress": false,
				"browser":         nil,
				// AvailableBrowsers has no `omitempty` on AutoCookieStatus
				// and the real GetStatus() never sends a nil slice (DetectBrowsers
				// returns non-nil so callers can range without a nil check) — []
				// here, not nil, so this branch cannot teach the frontend's
				// unconditional iteration a null it would never see from the
				// real service. ConfiguredBrowserPath/Type are NOT added here:
				// both carry `omitempty` and are empty on a fresh install, so a
				// zero-value AutoCookieStatus omits them too — adding them would
				// be the drift in the other direction, a key this branch sends
				// that the real zero-value response does not.
				"availableBrowsers": []cookies.DetectedBrowser{},
				"lastRefresh":       nil,
				"lastError":         nil,
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
