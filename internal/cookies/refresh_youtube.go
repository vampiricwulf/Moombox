package cookies

// refresh_youtube.go — the YouTube guide-endpoint exchange: request
// headers/body, the guide reply's auth verdict (and its fallback), and
// processing the reply's Set-Cookie headers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// setYouTubeHeaders applies the standard YouTube API headers for cookie-authenticated requests.
func setYouTubeHeaders(req *http.Request, cookieHeader, origin, authHeader string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", constants.UserAgents.Web)
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("X-Origin", origin)
}

// youtubeGuideRequestBody returns the standard Innertube WEB request body
// for /youtubei/v1/guide. Centralised here so a clientVersion bump only
// touches one site (audit reports/cookies.md #35) — that site is now
// constants.WebClient.ClientVersion, the single source of truth for every
// WEB-family client's version string.
func youtubeGuideRequestBody() string {
	return `{"context":{"client":{"clientName":"WEB","clientVersion":"` + constants.WebClient.ClientVersion + `","hl":"en"}}}`
}

// errGuideLoginMarkerUnreadable is what a 200 whose body carries no login
// marker we recognise resolves to. It is an INCONCLUSIVE outcome, in the same
// family as a non-200 and a failed provenance check, and every consumer already
// handles it: shouldFireRecovery returns false on any checkErr, and
// checkPlatformAuth maps it to verifyUnknown.
//
// It deliberately does NOT wrap ErrAuthCheckNotAttempted. That sentinel means
// the question could not be FORMED — no cookie header, no SAPISIDHASH, nothing
// left the process. Here a request went out and came back; we simply could not
// read the answer. autocookies_profile.go's `attempted` flag turns on exactly
// that distinction.
//
// WHERE THIS STRING ACTUALLY GOES, checked rather than assumed, because this
// comment has been wrong in both directions: an early draft claimed the Web UI
// and TUI when nothing read the field at all, and its replacement claimed no
// reader anywhere when Arc 8 Task 12a had given it two.
// AuthStatus.YouTubeError, the field doRefresh assigns it to, has exactly two
// readers today and both are PER-REQUEST:
//
//   - internal/web/routes/cookies.go's CookieStatusPayload /
//     TwitchAuthStatusPayload — the one copy of each wire shape, shared with
//     cmd/moombox/routes_wiring.go's status route — which project it as
//     `youtubeError` / `twitchError`;
//   - cmd/moombox/tui_wiring.go's OnRecheckCookies, which passes it through as
//     the reason on the R C result line.
//
// Nothing else reads it. authStatusToTUI's badge and the Web indicators still
// project from the booleans and the verdicts alone, and the second possible
// route stays closed: checkPlatformAuth consumes the error for an errors.Is
// test and discards the value, so the rollback messaging composes from the
// verification STATE and never interpolates this text.
//
// PER-REQUEST IS THE WHOLE CONCESSION, and it is what answered the objection
// that kept this field unread for two arcs. The fact the operator needs —
// "this check could not conclude" — is carried by RefreshUnknown, which is a
// bounded value; this string is server-authored prose whose lifecycle nothing
// establishes, so rendering it in an ALWAYS-ON panel would put unattributable
// text on screen indefinitely. A surface the operator asked for a moment ago
// has a lifecycle by construction: it answers one question once and is replaced
// by the next answer. authStatusChanged is where that rule is enforced — it
// excludes these two fields from the OnAuthChange gate as a contract, so no
// push-driven surface can start rendering them without someone widening the
// gate on purpose.
//
// The remaining sink is rs.logger.Debug in doRefresh, and what that means
// depends on the operator's log level — which makes the no-body-bytes rule
// below MORE load-bearing than "it goes to a log", not less, and more
// load-bearing again now that the same string also reaches two screens:
//
//   - At the default INFO (config.go's LogLevel) the line has NO sink at all.
//     Logger.log returns at its slog.Enabled gate before formatting, the ring
//     buffer, or any subscriber.
//   - At DEBUG the same line fans out to FIVE places: the rotating log file;
//     the in-memory ring buffer, which GET /api/logs serves to any authenticated
//     client; the Web UI's live log stream (cmd/moombox's log-forwarder
//     subscriber → wsHub.BroadcastLog → the frontend's "log" case); the per-job
//     log buffers the logger's line router writes to the DATABASE; and the TUI
//     log panel via its own subscriber.
//
// So that sink is conditional, persistent (file + DB), and remotely readable
// — and DEBUG is exactly the level an operator raises to when their cookies
// look broken, i.e. precisely when this error fires. TestUnreadableGuideError-
// CarriesNoBody earns its place on that basis: this error names no host, no
// header and no body bytes. The unreadable body is the subject of the report
// and must never become its content.
//
// The wording says "learned nothing", not "failed", and both UIs now agree with
// it. Follow-up 1 of the remediation plan — surface the inconclusive state in
// both UIs — landed as Arc 4+7's S12 (merge f2b4e30): verdictFromCheck maps this
// error to RefreshUnknown, AuthStatus carries that verdict beside each boolean,
// CookieStatusPayload projects it as `verification` for the Web indicators
// (cookieIndicatorState in web/public/modules/utils.js) and cookieBadgeFor reads
// it for the TUI status bar. So an install behind an intercepting intermediary
// renders as could-not-check, not as the "cookies found, not authenticated" this
// paragraph used to warn was still on screen.
//
// doRefresh does still set YouTubeAuthenticated: ytAuth, false on an
// inconclusive check, and that is deliberate rather than left over:
// `authenticated` keeps its "can we do authenticated work right now" meaning for
// every reader that predates the verdicts, and the verdict is what carries the
// distinction they cannot.
var errGuideLoginMarkerUnreadable = errors.New(
	"the guide reply carried no login marker we recognise, so this check learned nothing about the session")

// The guide reply's login marker, in the two shapes it has been observed in.
//
// Measured anonymously against the live endpoint on 2026-08-27 (an anonymous
// POST needs no credentials, so this is cheap to re-verify):
//
//	"serviceTrackingParams":[...{"key":"logged_in","value":"0"}...]
//	"mainAppWebResponseContext":{"loggedOut":true,...}
//
// Two things in that are worth writing down. The negative marker really is
// `logged_in` = `"0"` — a STRING "0", not a JSON false — which is why the
// sibling reader in internal/youtube deliberately refuses to map 1/0 and this
// one must: the two layers read different serialisations of the same flag.
// And an anonymous reply carries NO `loggedIn` key at all; it carries
// `loggedOut:true` instead. So a bare `bool` field could not tell "the flag
// said false" from "the flag was absent", which is precisely the distinction
// this fix turns on — hence the pointers on the struct below.
const (
	guideLoginParamKey = "logged_in"
	guideLoginParamIn  = "1"
	guideLoginParamOut = "0"
)

// guideLoginMarkersIn / guideLoginMarkersOut are the literal needles for the
// string fallback, which runs ONLY when the body is not valid JSON at all.
//
// Positive needles only ever grew: the two that were here before are kept
// verbatim, and the third is the shape actually measured on the wire (the
// params are objects, so `"logged_in":"1"` never matched a real reply — a
// latent miss that was harmless while every real reply parsed as JSON, and is
// still harmless now because a miss is inconclusive). Never removing an
// accepted positive form is the rule that keeps this change from costing an
// authenticated session its verdict.
//
// Negative needles are new and are what make a conclusion possible here at
// all, so each is either measured on the wire or the unambiguous negation of a
// positive already accepted above.
//
// Deliberately NOT tolerant of whitespace or alternate quoting, unlike the
// sibling reader in internal/youtube: see youtubeGuideAuthVerdict.
var (
	guideLoginMarkersIn = []string{
		`"key":"logged_in","value":"1"`,
		`"logged_in":"1"`,
		`"loggedIn":true`,
	}
	guideLoginMarkersOut = []string{
		`"key":"logged_in","value":"0"`,
		`"logged_in":"0"`,
		`"loggedIn":false`,
		`"loggedOut":true`,
	}
)

// youtubeGuideAuthVerdict reads a 200 guide reply and says whether it is an
// observation of a signed-in session, a signed-out one, or neither.
//
// A conclusive "not authenticated" now requires an EXPLICIT negative marker.
// This used to be the fall-off-the-end answer: three separate exits — JSON
// parse failure with no positive needle, no `logged_in` = "1" among the
// tracking params, and `loggedIn` not true — all ended in `(false, nil)`,
// which is the one thing shouldFireRecovery acts on.
//
// The body that breaks that shape is a 200 carrying no marker at all. A
// transparent, NON-redirecting intermediary — captive portal, corporate proxy
// (http.ProxyFromEnvironment is on the shared transport) — answering our POST
// with HTML passes the provenance guard (same host, same scheme, credential
// header intact) and passes the status check, then produces a conclusive
// verdict of "your cookies are dead" about cookies that are perfectly fine.
// The same shape covers the fleet-wide case: one serialisation change upstream
// would tell every install at once, at the only tier that notifies today.
// A false failure is worse than a missed one, so anything unrecognisable is
// now errGuideLoginMarkerUnreadable.
//
// This is the rule livenessVerdict (internal/youtube/watch_page.go) already
// applies to the watch page: an explicit marker or nothing. The RULE is
// mirrored, not the code — internal/cookies must not import internal/youtube
// (internal/youtube/auth.go already imports this package, so the dependency
// only runs the other way), and the two read different serialisations anyway:
// booleans in a ytcfg blob there, "1"/"0" strings in a JSON object here.
//
// Whitespace and quoting tolerance, decided deliberately rather than copied:
//
//   - The JSON path already has FULL tolerance, and better tolerance than the
//     sibling's hand-rolled reader — encoding/json is a real parser, so
//     arbitrary whitespace, key order, unknown sibling fields and escaped
//     strings all cost nothing. Every real reply reaches this path.
//   - The string fallback stays literal-substring, with no whitespace or
//     quote-form tolerance. It runs only when the body is NOT valid JSON, i.e.
//     when the serialisation is already broken, and under the new rule every
//     needle it misses lands on inconclusive rather than on a verdict. Adding
//     a tolerant scanner would mean duplicating ~120 lines of the sibling's
//     generic marker reader to buy accuracy on a path whose failure mode is
//     already the safe one — and a redundant helper is its own defect.
//
// Positive wins over negative, and is checked first, so no reply that read as
// authenticated before reads as anything else now.
func youtubeGuideAuthVerdict(respBody []byte) (bool, error) {
	var data struct {
		ResponseContext struct {
			ServiceTrackingParams []struct {
				Params []struct {
					Key string `json:"key"`
					// RawMessage, not string, and the reason is a real failure
					// mode rather than tidiness. encoding/json fails the WHOLE
					// body if any single field mistypes, so one unrelated param
					// gaining a non-string value — `cver`, `e`, `visitor_data`,
					// keys this reader has no interest in — would collapse the
					// JSON path to the literal-needle fallback for every reply.
					// That fallback cannot see the measured wire shape's
					// positive as reliably as a parser can, so tier-1 would
					// degrade to permanently inconclusive: safe, but blind, and
					// triggered by a field we never asked about. Deferring the
					// decode confines that failure to the one param it actually
					// happened on.
					//
					// Key stays a plain string, and the residual is real: a
					// non-string `key` on ANY param collapses the whole body
					// exactly the same way, through the field this does not
					// confine. (An earlier version of this comment claimed such
					// a key would be unsurvivable in any typing. That is wrong —
					// a lazy Key would survive it fine, for the same reason a
					// lazy Value survives a mistyped Value.)
					//
					// It stays strict on COST, not on impossibility. Key is
					// compared on every param in the reply, so deferring it
					// means a decode per param on a ~15KB body on every refresh
					// cycle, where Value is decoded at most a handful of times —
					// only for params whose key already matched. Paying that to
					// guard a param NAME, the half of a key/value list least
					// likely to change type, is the wrong trade.
					//
					// If it ever happens the failure direction is the safe one:
					// the body falls to the literal-needle fallback and tier-1
					// degrades toward inconclusive, never toward a false alarm.
					Value json.RawMessage `json:"value"`
				} `json:"params"`
			} `json:"serviceTrackingParams"`
			MainAppWebResponseContext struct {
				// Pointers on purpose: absent and present-false are different
				// answers here, and only one of them is a verdict. See the
				// marker commentary above — a real anonymous reply omits
				// loggedIn entirely and sends loggedOut instead.
				LoggedIn  *bool `json:"loggedIn"`
				LoggedOut *bool `json:"loggedOut"`
			} `json:"mainAppWebResponseContext"`
		} `json:"responseContext"`
	}

	if err := json.Unmarshal(respBody, &data); err != nil {
		return youtubeGuideAuthVerdictFallback(respBody)
	}

	// An explicit negative anywhere is remembered but never returned early:
	// the scan keeps looking for a positive, because losing a signed-in read
	// is the one regression this must not introduce.
	negative := false

	// Primary: serviceTrackingParams carries logged_in across all Innertube
	// responses (several services each stamp their own copy).
	for _, service := range data.ResponseContext.ServiceTrackingParams {
		for _, param := range service.Params {
			if param.Key != guideLoginParamKey {
				continue
			}
			// Decoded here, for this param only — see the Value field comment.
			var value string
			if err := json.Unmarshal(param.Value, &value); err != nil {
				// The marker itself is no longer a string. A marker we found
				// and could not read is not a verdict; keep going.
				continue
			}
			switch value {
			case guideLoginParamIn:
				return true, nil
			case guideLoginParamOut:
				negative = true
			}
			// Any other value is a marker we found and could not read. Not a
			// verdict — fall through and let the rest of the reply speak.
		}
	}

	// Secondary: mainAppWebResponseContext's own flag, in whichever direction
	// it is emitted.
	if in := data.ResponseContext.MainAppWebResponseContext.LoggedIn; in != nil {
		if *in {
			return true, nil
		}
		negative = true
	}
	if out := data.ResponseContext.MainAppWebResponseContext.LoggedOut; out != nil && *out {
		negative = true
	}
	// `loggedOut:false` is deliberately not read as a positive. It has never
	// been observed, and inferring "signed in" from it would be a guess; the
	// cost of not guessing is an inconclusive result, which is safe.

	if negative {
		return false, nil
	}
	return false, errGuideLoginMarkerUnreadable
}

// youtubeGuideAuthVerdictFallback is youtubeGuideAuthVerdict for a body that
// is not valid JSON. Same rule, literal needles.
//
// The slice is capped so the resulting Go string never carries the full
// multi-MB payload — the marker lives in the first few hundred bytes of the
// responseContext block, and scanning past 16KB only inflates memory and
// widens the surface for session material to reach a log line (#24).
func youtubeGuideAuthVerdictFallback(respBody []byte) (bool, error) {
	respStr := string(respBody[:min(len(respBody), authBodyFallbackLimit)])
	for _, marker := range guideLoginMarkersIn {
		if strings.Contains(respStr, marker) {
			return true, nil
		}
	}
	for _, marker := range guideLoginMarkersOut {
		if strings.Contains(respStr, marker) {
			return false, nil
		}
	}
	return false, errGuideLoginMarkerUnreadable
}

// authResponseIsOurs returns nil only when resp can be read as an answer about
// THIS install's credential, and otherwise an error saying which way it failed
// to qualify. `sent` is the request we dispatched; credentialHeader is the
// header carrying the credential ("Cookie" for YouTube, "Authorization" for
// Twitch).
//
// This is internal/youtube's livenessResponseIsOurs rule, ported rather than
// shared: that function is unexported, and this package must not import
// internal/youtube — internal/youtube/auth.go already imports this one, so the
// dependency only runs the other way. Any change to either should be made to
// both.
//
// Why the tier-1 checks need it at all. cookiesHTTPClient installs no
// CheckRedirect, so Go follows redirects; on the first hop to a different
// HOSTNAME it drops the manually-set credential header, and the decision is
// STICKY (client.go:620 declares stripSensitiveHeaders once before the redirect
// loop and only ever sets it inside at :688; nothing clears it on a later hop).
// So origin → wall → origin lands back on the host we asked for and delivers a
// body fetched with no credentials. Neither guide check nor the Twitch validate
// check looked at where the answer came from: any 200 whose body lacked both
// `"logged_in":"1"` and `"loggedIn":true` fell through to a CONCLUSIVE "not
// authenticated", and any 401 was Twitch's documented dead-token verdict. Both
// are what shouldFireRecovery acts on, and after Task 7 a fire notifies the
// operator on BOTH install shapes. Task 1 closed the non-200 half of this; a
// followed redirect never presents as non-200, so it could not catch this one.
//
// The trigger is an intercepting intermediary — captive portal, transparent or
// corporate proxy (http.ProxyFromEnvironment is on the shared transport at
// internal/httpx/client.go:40, and this package consults no connectivity gate).
//
// A provenance failure is INCONCLUSIVE — an error, matching what Task 1 and
// Task 1b established for a non-200 — never a verdict. It deliberately does NOT
// wrap ErrAuthCheckNotAttempted: a request did leave the process, so this is
// the "the site could not answer for us" unknown, not the "we could not form
// the question" one, and checkPlatformAuth tells those apart.
//
// The check is non-vacuous only because both callers refuse the empty-credential
// case BEFORE fetching (checkYouTubeAuth/checkAndRefreshYouTube on an empty
// cookie header, checkTwitchAuth on an empty token). Past that point the header
// was definitely set, so finding none on the answering request can only mean it
// was taken away.
//
// COUPLING, and it fails silently if broken: the header rule only means
// anything because cookiesHTTPClient has no http.CookieJar. With one installed
// the stdlib would re-add a Cookie header on the final hop from the jar's own
// scope rules, the check would pass on a request that never carried OUR
// session, and nothing here would fail. httpx.Client documents that property;
// TestCookiesHTTPClientCarriesNoCookieJar pins it for THIS client, as
// internal/utils' TestUtilsHTTPClientCarriesNoCookieJar does for the one the
// tier-2 probes use.
//
// Positive confirmation throughout, and deliberately STRICTER than the stdlib
// rule it defends against, so every disagreement resolves toward inconclusive:
//
//   - Host is compared as host:port, raw. Go compares URL.Hostname() —
//     port-stripped and permitting subdomains (isDomainOrSubdomain,
//     client.go:1028) — so a port change or a subdomain hop fails here while Go
//     would still forward the credential.
//   - Scheme is compared at all. Go's strip decision looks only at Host, so an
//     https→http downgrade on the same host keeps the credential; we refuse it
//     rather than read a verdict off an exchange made in clear.
//
// Errors name a host and a header NAME — never a header value, never response
// bytes. Since Arc 8 Task 12a they reach two PER-REQUEST screens as
// AuthStatus.YouTubeError / TwitchError — the REST cookie-status payload and the
// TUI's R C result line — besides the Debug log; for the full accounting, and
// why that makes the rule more load-bearing rather than less, see
// errGuideLoginMarkerUnreadable's doc comment. They do NOT reach
// AutoCookieService.setError: checkPlatformAuth discards the value.
func authResponseIsOurs(resp *http.Response, sent *http.Request, credentialHeader string) error {
	if sent == nil || sent.URL == nil {
		return fmt.Errorf("could not determine what was asked")
	}
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return fmt.Errorf("could not determine what answered %s", sent.URL.Host)
	}
	final := resp.Request.URL
	if !strings.EqualFold(final.Scheme, sent.URL.Scheme) || !strings.EqualFold(final.Host, sent.URL.Host) {
		return fmt.Errorf("%s was answered by %s://%s; not an observation of this session",
			sent.URL.Host, final.Scheme, final.Host)
	}
	if resp.Request.Header.Get(credentialHeader) == "" {
		return fmt.Errorf("%s was answered by a request that no longer carried the %s header; not an observation of this session",
			sent.URL.Host, credentialHeader)
	}
	return nil
}

// youtubeGuideExchange makes the guide POST that both YouTube auth entry points
// are built on, and hands the verdict AND the response back to its caller. It
// never acts on the response itself, and that is the whole reason it has this
// shape.
//
// # Why it returns the response instead of using it
//
// checkYouTubeAuth is exported as CheckYouTubeAuth and wired into
// AutoCookieService.VerifyYouTubeAuth (cmd/moombox/services.go), where
// checkPlatformAuth runs it on the ROLLBACK path of a profile import: its answer
// is what decides whether the PREVIOUS cookies are restored. A shared exchange
// that merged Set-Cookie headers itself would therefore write the jar from the
// very response being used to judge the import — a bad import would rewrite the
// credentials it was about to be rolled back for, and the rollback would then
// restore them over a file that had already moved.
//
// So the write decision stays with the caller that owns it. This function reads
// the jar and never writes it; only checkAndRefreshYouTube calls
// processYouTubeSetCookies, and only on its own authenticated path. The
// invariant is structural rather than a matter of discipline: there is no jar
// write reachable from here to delete.
//
// The returned response has already had its body read to a verdict and CLOSED,
// so only its headers are still meaningful — which is all
// processYouTubeSetCookies reads. It is non-nil exactly when a request was made
// and a readable answer came back. Every error path returns nil, and so does the
// never-configured gate, which is what makes "a reply we could not read is not a
// reply anyone may write the jar from" a fact about the return values rather
// than a rule someone has to remember.
//
// # The gates
//
// The three entry gates encode one rule — the rule this subsystem kept getting
// wrong. Only the FIRST of them may answer (false, nil).
//
//   - Nothing configured at all — no session to have an opinion about, so a
//     silent "not authenticated" is the truth and shouldFireRecovery's
//     cookiesPresent gate (fed by the same predicate) keeps it silent.
//   - Configured but no request could be built — a check that did NOT happen.
//     (false, nil) would report it as dead credentials, so it errors instead.
//
// Everything in between reaches the network. In particular a jar with SAPISID
// and a cleared LOGIN_INFO — YouTube's own rotation-invalidation state — is
// CONFIGURED with BROKEN credentials, and its verdict has to come from YouTube
// rather than from a missing name in a map.
//
// The order after the request is load-bearing and is asserted in that order:
// provenance, then status, then body.
//
// It takes no URL: the two entry points were reading the same endpoint from two
// differently-named vars (see youtubeGuideURL), so the URL was never a thing
// they varied on.
func (rs *RefreshService) youtubeGuideExchange(ctx context.Context) (bool, *http.Response, error) {
	if !rs.jar.HasAnyYouTubeAuthCookie() {
		return false, nil, nil // Nothing configured at all.
	}

	cookieHeader := rs.jar.GetCookieHeader()
	if cookieHeader == "" {
		return false, nil, fmt.Errorf("youtube auth check: no cookie header could be built: %w", ErrAuthCheckNotAttempted)
	}

	origin := "https://www.youtube.com"
	authHeader := rs.jar.GenerateAuthorizationHeader(origin)
	if authHeader == "" {
		return false, nil, fmt.Errorf("youtube auth check: no SAPISIDHASH could be generated: %w", ErrAuthCheckNotAttempted)
	}

	ctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()

	body := youtubeGuideRequestBody()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, youtubeGuideURL, strings.NewReader(body))
	if err != nil {
		return false, nil, err
	}

	setYouTubeHeaders(req, cookieHeader, origin, authHeader)

	resp, err := cookiesHTTPClient.Do(req)
	if err != nil {
		return false, nil, fmt.Errorf("youtube auth check: %w", err)
	}
	// Closed here, not by the caller: by the time this returns the body has
	// been read to a verdict and nothing downstream needs it. The headers
	// survive the close.
	defer resp.Body.Close()

	// Before the status, for the same reason internal/youtube checks it first:
	// a redirected answer is not this session's answer whatever status it
	// carries, and naming the route is more accurate than naming the code.
	if err := authResponseIsOurs(resp, req, "Cookie"); err != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil, fmt.Errorf("youtube auth check: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		// NOT (false, nil). That means "conclusively not authenticated" to
		// shouldFireRecovery, so a 429/503/edge block would be reported as
		// dead credentials. We learned nothing about the session here.
		return false, nil, fmt.Errorf("youtube auth check: unexpected status %d", resp.StatusCode)
	}

	// YouTube always returns 200 even with invalid cookies, so the verdict has
	// to come out of the body. youtubeGuideAuthVerdict owns that rule for both
	// entry points; in particular a 200 carrying no marker it recognises is an
	// inconclusive error here, NOT (false, nil).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return false, nil, fmt.Errorf("read YouTube auth response: %w", err)
	}
	authenticated, err := youtubeGuideAuthVerdict(respBody)
	if err != nil {
		return false, nil, fmt.Errorf("youtube auth check: %w", err)
	}
	return authenticated, resp, nil
}

// checkYouTubeAuth asks YouTube whether the jar's session is still signed in,
// and does nothing else.
//
// This is the VERIFY path. It is exported as CheckYouTubeAuth and its answer
// decides whether an autocookies profile import is committed or ROLLED BACK, so
// it must not touch the jar — see youtubeGuideExchange's doc comment for why
// that invariant is expressed by discarding the response here rather than by a
// guard somewhere inside.
func (rs *RefreshService) checkYouTubeAuth(ctx context.Context) (bool, error) {
	authenticated, _, err := rs.youtubeGuideExchange(ctx)
	return authenticated, err
}

// checkAndRefreshYouTube makes a single guide API request to both check
// YouTube auth status and refresh session cookies from Set-Cookie headers.
// This avoids the redundancy of separate check + refresh requests.
//
// It is the only caller in this file that writes the jar from a guide reply.
func (rs *RefreshService) checkAndRefreshYouTube(ctx context.Context) (bool, error) {
	// Whose session the request is about to carry. The reply's Set-Cookie
	// rotations belong to it and to nothing else; see updateCookieFile.
	sentAs := rs.jar.youTubeSessionKey()
	authenticated, resp, err := rs.youtubeGuideExchange(ctx)
	if err != nil || !authenticated {
		// Anything short of an authenticated, readable reply stops here without
		// the jar being touched. Two of those cases are worth naming at the
		// write decision itself:
		//
		//   - Provenance. youtubeGuideExchange runs authResponseIsOurs before
		//     the status AND before the body, which matters more on THIS path
		//     than on the verify one: this is where Set-Cookie headers are
		//     merged back into the jar, and a redirected exchange must not be
		//     allowed to write to it at all.
		//   - An unreadable body. A reply we could not read is not a reply we
		//     may write the jar from, for the same reason a redirected one is
		//     not: we do not know whose session it describes. Both cases return
		//     a nil response, so there would be nothing to merge here even if
		//     this branch were deleted.
		return authenticated, err
	}
	rs.processYouTubeSetCookies(resp, sentAs)
	return true, nil
}

// processYouTubeSetCookies parses Set-Cookie headers from a YouTube API response
// and merges updated cookies into the cookie file.
//
// sentAs is the jar's youTubeSessionKey when the request was built: the
// rotations are written only into a file that still holds that session.
func (rs *RefreshService) processYouTubeSetCookies(resp *http.Response, sentAs string) {
	setCookies := resp.Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		rs.logger.Debug("youtube session refresh: no Set-Cookie headers")
		return
	}

	// originYouTube: this function is only ever fed a youtube.com guide
	// response, and admitSetCookie needs to be told so — a Set-Cookie with no
	// Domain= is host-scoped to the response that carried it, and the header
	// alone does not say which response that was.
	updates := make(map[cookieUpdateKey]cookieUpdate)
	refused := 0
	for _, sc := range setCookies {
		key, cu, ok := admitSetCookie(sc, originYouTube)
		if !ok {
			refused++
			continue
		}
		updates[key] = cu
	}
	if refused > 0 {
		// A COUNT, never the header. A Set-Cookie is the credential itself, and
		// a refused one is by definition the shape we did not vouch for, so
		// neither its value nor its raw text may be written to a log.
		rs.logger.Debug("youtube session refresh: Set-Cookie headers not admitted", "count", refused)
	}

	if len(updates) == 0 {
		rs.logger.Debug("youtube session refresh: no relevant cookies to update")
		return
	}

	rs.logger.Debug("youtube session refresh: updating cookies", "count", len(updates))

	// A failure here is not cosmetic: the rotated values are discarded and
	// the on-disk session ages out, so downloads eventually start failing
	// for a reason that has nothing to do with the download. Name the
	// deployment mistake that actually causes it — updateCookieFile ends in
	// writeFileAtomic (temp file + rename), and a rename cannot replace a
	// single-file bind mount.
	//
	// The same originYouTube is declared a second time here, to the write path,
	// which needs it for its own three decisions — see updateCookieFile.
	if err := rs.updateCookieFile(updates, originYouTube, sentAs); err != nil {
		if errors.Is(err, errCookieSessionReplaced) {
			rs.logger.Debug("youtube session refresh: cookies.txt now holds another session; its rotations were not applied")
			return
		}
		rs.logger.Warn("youtube session refresh: failed to update cookie file — rotated session cookies were discarded and the file will go stale",
			"err", err,
			"hint", "if this is Docker, do not bind-mount cookies.txt as an individual file; put it inside the mounted /data directory so the atomic rename can replace it")
		return
	}

	if err := rs.jar.Reload(); err != nil {
		rs.logger.Warn("youtube session refresh: failed to reload jar", "err", err)
	}
}
