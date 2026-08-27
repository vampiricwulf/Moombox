package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

var (
	// jsURLRegex matches the JSON field holding the player script URL.
	// YouTube has historically used several names for the same value
	// ("jsUrl", "PLAYER_JS_URL", and more recently "scriptSrc"); match any
	// of them so a rename doesn't quietly disable cipher compilation.
	jsURLRegex = regexp.MustCompile(`"(?:jsUrl|PLAYER_JS_URL|scriptSrc)":"([^"]+)"`)
	// visitorDataRegex matches both the camel-case form seen on watch pages
	// and the snake-case form the homepage has historically used. Kept in
	// sync with service.go's homepage extraction so a single rename only
	// has to be edited in one place.
	visitorDataRegex      = regexp.MustCompile(`"(?:visitorData|visitor_data)":"([^"]+)"`)
	sessionIndexRegex     = regexp.MustCompile(`"SESSION_INDEX":"?(\d+)"?`)
	delegatedSessionRegex = regexp.MustCompile(`"DELEGATED_SESSION_ID":"([^"]+)"`)
	dataSyncIDRegex       = regexp.MustCompile(`"datasyncId":"([^"]+)"`)
	// gvsBindVideoIDRegex detects the html5_generate_content_po_token
	// experiment inside WEB_PLAYER_CONTEXT_CONFIGS' serializedExperimentFlags.
	// Those flags are a query string embedded in a JSON string, so '=' arrives
	// backslash-u-escaped on real pages (verified 2026-08-15); accept the
	// escaped and literal forms.
	gvsBindVideoIDRegex = regexp.MustCompile(`html5_generate_content_po_token(?:=|\\u003d)true`)
	// Matches encryptedHostFlags in flat JSON objects. May fail if nested objects
	// precede the field (YouTube's embed page config is typically flat here).
	encryptedHostFlagsRegex = regexp.MustCompile(`"WEB_PLAYER_CONTEXT_CONFIG_ID_EMBEDDED_PLAYER":\{[^}]*"encryptedHostFlags":"([^"]+)"`)
	// Multiple patterns for extracting player response — YouTube occasionally
	// changes the variable name or assignment format.
	playerResponsePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?s)var ytInitialPlayerResponse\s*=\s*({.+?});`),
		regexp.MustCompile(`(?s)window\["ytInitialPlayerResponse"\]\s*=\s*({.+?});`),
		regexp.MustCompile(`(?s)ytInitialPlayerResponse\s*=\s*({.+?});`),
	}
	// ytInitialDataRegex extracts the ytInitialData JSON blob used for chat
	// continuation token extraction. Mirrors the same regex used by
	// internal/chat/api.go for its standalone watch-page fetch path.
	ytInitialDataRegex = regexp.MustCompile(`(?s)var ytInitialData = ({.+?});</script>`)
	// ytAtNOpenRe locates the opening of a window.ytAtN(...) call. Only the
	// call prefix is matched here; the object literal's extent is found by
	// scanning balanced braces (scanBalancedObject) rather than by a
	// non-greedy regex. moonarchive's INITIAL_ATTESTATION_PATTERN uses the
	// regex form, but a `})` sequence anywhere inside the opaque challenge
	// payload truncates that match into an unbalanced fragment, which then
	// fails to parse and is indistinguishable from "page had no challenge".
	ytAtNOpenRe = regexp.MustCompile(`window\.ytAtN\(\s*\{`)
)

// allowedInterpreterDomains gates which hosts may serve a BotGuard interpreter
// referenced by a page-sourced challenge. The sidecar EXECUTES the fetched
// body, and watch-page HTML embeds attacker-authored video metadata, so a
// challenge is only forwarded when its interpreter lives on a Google-owned
// host. The sidecar re-checks this independently (bgutil-sidecar/src/server.js
// assertGoogleHost) — this copy stops a hostile challenge from ever leaving
// the Go process. Suffix matches are dot-anchored so "evilgoogle.com" and
// "google.com.evil.tld" cannot pass.
//
// Regional Google properties (google.de, google.co.uk, …) are matched
// separately by regionalGoogleRe rather than enumerated. A host that is
// genuinely Google's but missing here does not break downloads — the
// challenge is dropped and the sidecar's /att/get flow runs — but it does
// silently disable session coherence, so rejections are logged with the host
// (atnBadInterpHost) precisely so an unlisted host shows up as a name to add
// instead of an unexplained regression.
// EXACT hosts only — never suffix matches, never patterns. An adversarial
// review (2026-08-15) defeated both weaker forms:
//
//   - Suffix matching `.googleapis.com` / `.google.com` re-admitted the very
//     user-content class it was meant to exclude: storage.googleapis.com and
//     firebasestorage.googleapis.com serve anyone's uploaded bucket objects,
//     and sites/script/drive.google.com host third-party content. Registering
//     a bucket and pointing a video description at it reached
//     new Function() end-to-end.
//   - A "regional Google" pattern (^google\.[a-z]{2,3}(\.[a-z]{2})?$) matched
//     SHAPE, not ownership: google.com.se is a live third-party site, as are
//     google.co.nl and google.org.ru. Anyone can register one.
//
// So the rule is now membership in this list and nothing else. Regional
// Google domains are unsupported: the interpreter is served from a global
// host (observed: www.google.com), and a genuinely-Google host missing here
// fails closed — the challenge is dropped, the sidecar's /att/get flow runs,
// and the rejected host is named in the reason string so it can be added
// deliberately rather than guessed at by a pattern.
// staticScriptPathRe is the shape a genuine interpreter path takes: an
// unreserved-character path ending in .js (observed
// /js/th/qtyJVB4UpQW6ehm0Eb6anVy7Y_bU8GitWVbp9gjCikM.js). Percent-encoding is
// excluded deliberately — see the call site — as are query and fragment
// markers, which cannot appear in a path this alphabet allows.
var staticScriptPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]+\.[Jj][Ss]$`)

var allowedInterpreterHosts = []string{
	"www.google.com",
	"google.com",
	"www.gstatic.com",
	"ssl.gstatic.com",
	"gstatic.com",
	"s.ytimg.com",
	"www.youtube.com",
	"youtube.com",
}

// Attestation-challenge extraction outcomes. Every failure mode is distinct so
// a silently-disabled feature is never mistaken for "YouTube sent no
// challenge" — the whole point of the challenge plumbing is diagnosing 403s,
// which a single catch-all reason would defeat.
const (
	atnOK             = "ok"
	atnNoCall         = "no ytAtN call on page"
	atnUnbalanced     = "ytAtN argument never closes"
	atnJSConvert      = "JS-to-JSON conversion failed"
	atnOuterParse     = "outer object is not JSON"
	atnNoRKey         = "no R string key"
	atnRParse         = "R payload is not JSON"
	atnNoChallenge    = "no bgChallenge in R payload"
	atnChallengeShape = "bgChallenge is not an object"
	atnNoInterpURL    = "bgChallenge has no interpreterUrl (inline interpreterJavascript is refused from page-sourced challenges)"
	atnBadInterpHost  = "bgChallenge interpreter host not allowed"
	atnBadInterpPath  = "bgChallenge interpreter URL is not a static script"
)

// WatchPageResult contains data extracted from a YouTube watch page.
//
// The raw HTML is intentionally NOT a field. Watch-page responses for popular
// live streams routinely exceed 5 MB, and quality-monitor polling fetches the
// page every interval — keeping HTML on the result struct produced a ~5 MB/min
// leak (every poll's HTML pinned in the heap, observed in pprof). All
// downstream needs (ytcfg fields, player response, chat continuation) are
// extracted at parse time so the body string can be GC'd as soon as
// FetchWatchPage returns.
type WatchPageResult struct {
	Ytcfg *YtcfgData
	// SessionAuth is YouTube's own verdict on whether this fetch was a
	// signed-in session. The zero value is SessionAuthUnknown, which is what
	// callers that synthesize a WatchPageResult after a failed fetch get for
	// free — and what watchPageSessionAuth returns for a 200 that is not a
	// recognisable watch-page shell. Neither "we never saw a page" nor "we
	// saw something we can't read" may be reported as "logged out": that
	// verdict is now printed to the user as "your cookies are dead".
	SessionAuth      SessionAuthState
	PlayerResponse   map[string]any
	ChatContinuation string
	ChatIsReplay     bool
	// ChatErr captures any failure encountered while extracting the chat
	// continuation token. Non-nil + empty ChatContinuation means "no chat
	// available" with diagnostic context for the caller's debug log.
	ChatErr error
	// AttestationChallenge is the compact JSON of the BotGuard bgChallenge
	// YouTube embedded in this page load via window.ytAtN(...) — the
	// session's own attestation challenge, used to mint session-coherent
	// GVS PO tokens (moonarchive 96344fe parity). Empty when the page did
	// not carry one or it failed to parse; consumers must treat empty as
	// "fall back to the sidecar's /att/get flow".
	AttestationChallenge string
	// AttestationReason names WHY AttestationChallenge is empty (one of the
	// atn* constants). A genuine absence and a silently-broken extractor both
	// yield "", and this feature exists to diagnose 403s — so the two must be
	// distinguishable in a log line.
	AttestationReason string
}

// isConsentRedirect reports whether a watch-page fetch's FINAL URL (after
// redirects) landed on the EU consent interstitial (consent.youtube.com /
// consent.google.com). The interstitial answers 200 with none of the
// watch-page payloads, so extraction would silently yield an empty ytcfg —
// no visitorData, PlayerURL, STS, or PO token — with nothing in the log.
// Mirrors the chat API's detection (audit chat.md C14).
func isConsentRedirect(resp *http.Response) bool {
	return resp != nil && resp.Request != nil && resp.Request.URL != nil &&
		strings.HasPrefix(resp.Request.URL.Host, "consent.")
}

// FetchWatchPage fetches and parses a YouTube watch page.
func FetchWatchPage(ctx context.Context, videoID string, cookieHeader string) (*WatchPageResult, error) {
	url := fmt.Sprintf("%s?v=%s", constants.YouTubeURLs.Watch, videoID)

	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
	}
	if cookieHeader != "" {
		headers["Cookie"] = cookieHeader
	}

	// FetchWithTimeout (not FetchBody) so the FINAL post-redirect URL is
	// inspectable — the consent interstitial answers 200, so status-code
	// checks alone can never see it.
	resp, cancel, err := utils.FetchWithTimeout(ctx, url, 30*time.Second, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch watch page: %w", err)
	}
	defer cancel()
	defer resp.Body.Close()

	if isConsentRedirect(resp) {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("watch page redirected to consent wall (%s); supply CONSENT cookies via a cookie file", resp.Request.URL.Host)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("failed to fetch watch page: HTTP %d from %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, utils.MaxFetchBodySize))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch watch page: %w", err)
	}

	html := string(body)
	sessionAuth := watchPageSessionAuth(html)

	ytcfg, playerResponse := extractYtcfgAndPlayerResponse(html)
	chatContinuation, chatIsReplay, chatErr := extractChatContinuation(html)
	attestationChallenge, attestationReason := extractAttestationChallenge(html)

	return &WatchPageResult{
		Ytcfg:                ytcfg,
		SessionAuth:          sessionAuth,
		PlayerResponse:       playerResponse,
		ChatContinuation:     chatContinuation,
		ChatIsReplay:         chatIsReplay,
		ChatErr:              chatErr,
		AttestationChallenge: attestationChallenge,
		AttestationReason:    attestationReason,
	}, nil
}

// Login-verdict markers, shared by the string and []byte detectors so the
// two can never drift. Two ytcfg spellings have been observed for the same
// flag; either counts.
const (
	sessionAuthKey       = `"LOGGED_IN":`
	sessionAuthCamelKey  = `"isLoggedIn":`
	sessionAuthTrue      = "true"
	sessionAuthFalse     = "false"
	sessionAuthYtcfgMark = "ytcfg.set"
)

// sessionAuthMaxSpaceSkip bounds how far past the colon the value reader will
// walk through whitespace before giving up.
//
// A bound rather than an unbounded loop because these functions run over a
// ~1MB page on a monitor cadence and must stay O(1) past the key. Eight is
// far more than any serialiser puts between a colon and its value (one space
// is the realistic maximum) while still being a run no plausible emitter
// produces by accident — past it we are no longer looking at a marker we
// recognise, and Unknown is the honest answer.
const sessionAuthMaxSpaceSkip = 8

// sessionAuthBody is the two shapes a fetched page arrives in. The value
// reader is generic over both so the string and []byte detectors read a value
// through ONE piece of logic: this used to be duplicated, and the whole
// hazard the file guards against is those two drifting apart.
type sessionAuthBody interface{ ~string | ~[]byte }

// sessionAuthWordAt reports whether word sits at b[i:] as a complete token —
// i.e. not merely as the prefix of a longer identifier, so `truthy` does not
// read as `true`. Byte-by-byte rather than a slice conversion so no []byte
// ever becomes a string here; see TestSessionAuthFromBytesDoesNotAllocate.
func sessionAuthWordAt[T sessionAuthBody](b T, i int, word string) bool {
	if i < 0 || i+len(word) > len(b) {
		return false
	}
	for k := 0; k < len(word); k++ {
		if b[i+k] != word[k] {
			return false
		}
	}
	j := i + len(word)
	return j >= len(b) || !isSessionAuthWordByte(b[j])
}

// isSessionAuthWordByte reports whether c can continue a JS/JSON identifier
// or number, which is what makes `true` in `truthy` not a value of `true`.
func isSessionAuthWordByte(c byte) bool {
	return c == '_' || c == '.' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

// isSessionAuthSpace is the whitespace a serialiser may put between the colon
// and the value.
func isSessionAuthSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// sessionAuthValue reads the login-marker value that begins at rest[0] — rest
// being whatever follows the marker key's colon.
//
// This function is the fix for the failure mode that shaped it. The value
// used to be tested with a single HasPrefix against the literal `true`, and
// EVERYTHING else — one space, a quoted boolean, any form not yet seen —
// fell to LoggedOut. LoggedOut is the only verdict the liveness arc acts on,
// so a serialisation change on YouTube's side, entirely outside our control,
// would have told every healthy install at once that its cookies were dead
// and sent its operator to re-export credentials that were fine. A false
// failure is worse than a missed one, and that was a false failure at fleet
// scale in the only direction that matters.
//
// So the rule is inverted: LoggedOut is claimed ONLY from a value that reads
// as an explicit false. LoggedIn only from an explicit true. Anything this
// reader cannot read is Unknown — including a value that is truncated,
// absent, or in a shape not listed here.
//
// Recognised forms, both spellings of the key, whitespace-tolerant up to
// sessionAuthMaxSpaceSkip: bare `true`/`false`, and the same two wrapped in
// single or double quotes. Numbers are deliberately NOT mapped: nothing
// in-tree or upstream establishes that YouTube ever emits this flag as 1/0,
// and a guess in the false direction is the alarm this fix exists to prevent.
// If that form is ever observed, adding it is two cases here.
//
// Unknown is safe on every path that can see it, verified from source rather
// than assumed:
//
//   - withAttestation (player_api_strategy.go) tests
//     `SessionAuth == SessionAuthLoggedIn` for the GVS content binding, so
//     Unknown and LoggedOut select the identical visitorData branch. No
//     behaviour change at all.
//   - StreamProcessor.checkPlayability (internal/worker) switches on it for
//     the members-only message. Unknown takes the default branch, which
//     keeps the plain "Member-only: <reason>" wording and the SAME
//     ErrCookiesRequired sentinel as the LoggedOut branch — identical job
//     routing, only a less specific sentence.
//   - The liveness probes route it through livenessFromProbe /
//     routeLivenessVerdict, both of which already treat Unknown as "learned
//     nothing" and observe no verdict at all.
//
// Note the direction of the two remaining risks, which is why this shape was
// chosen. Nothing that read LoggedIn before reads Unknown now (the accepted
// true forms only grew), so no authenticated download loses its datasyncID
// binding. And a marker that drifts into an unrecognised shape now goes
// quiet rather than alarming — a missed failure, the acceptable direction.
func sessionAuthValue[T sessionAuthBody](rest T) SessionAuthState {
	i := 0
	for i < len(rest) && i < sessionAuthMaxSpaceSkip && isSessionAuthSpace(rest[i]) {
		i++
	}
	if i >= len(rest) {
		return SessionAuthUnknown
	}

	if q := rest[i]; q == '"' || q == '\'' {
		v := i + 1
		switch {
		case sessionAuthWordAt(rest, v, sessionAuthTrue) && sessionAuthClosedBy(rest, v+len(sessionAuthTrue), q):
			return SessionAuthLoggedIn
		case sessionAuthWordAt(rest, v, sessionAuthFalse) && sessionAuthClosedBy(rest, v+len(sessionAuthFalse), q):
			return SessionAuthLoggedOut
		}
		return SessionAuthUnknown
	}

	switch {
	case sessionAuthWordAt(rest, i, sessionAuthTrue):
		return SessionAuthLoggedIn
	case sessionAuthWordAt(rest, i, sessionAuthFalse):
		return SessionAuthLoggedOut
	}
	return SessionAuthUnknown
}

// sessionAuthClosedBy reports whether b[i] is the closing quote q. A value
// whose quote never closes — the page was truncated there, or the opening
// quote was not a quote at all — is unreadable, not false.
func sessionAuthClosedBy[T sessionAuthBody](b T, i int, q byte) bool {
	return i < len(b) && b[i] == q
}

// watchPageSessionAuth reads YouTube's own login verdict off a watch page.
// Two ytcfg spellings have been observed for the same flag; either counts.
//
// A 200 is NOT by itself an observation of the session. Consent
// interstitials, edge error pages and A/B shells all answer 200 carrying no
// ytcfg at all, and treating "marker absent" as "logged out" would assert a
// dead session at an operator whose cookies are fine — the precise failure
// this whole change exists to remove, and one that only became user-visible
// once the state started being printed. So logged-out is claimed only from a
// recognisable watch-page shell: the explicit negative marker, or the ytcfg
// bootstrap that every genuine watch page carries. Anything else is unknown,
// which callers already render as the safe generic wording.
//
// A key that is PRESENT but whose value cannot be read returns straight away
// with sessionAuthValue's answer — it does not fall through to the camelCase
// spelling or to the ytcfg bootstrap. The marker is the signal we came for;
// having failed to read it is not a licence to answer from a weaker one, and
// the bootstrap branch would answer LoggedOut, which is exactly the false
// alarm sessionAuthValue exists to prevent. The fallbacks stay reachable for
// their real case: no key at all.
//
// Cost is one Index for the primary key plus, on pages that lack it, one
// scan each for the camelCase spelling and the ytcfg bootstrap, then a
// bounded read of a few bytes — no allocation and no regex, because this runs
// on every watch-page fetch including quality-monitor polling.
func watchPageSessionAuth(html string) SessionAuthState {
	if i := strings.Index(html, sessionAuthKey); i >= 0 {
		return sessionAuthValue(html[i+len(sessionAuthKey):])
	}
	if i := strings.Index(html, sessionAuthCamelKey); i >= 0 {
		return sessionAuthValue(html[i+len(sessionAuthCamelKey):])
	}
	// No login key, but a real watch-page shell: YouTube answered as a page
	// it would have stamped the key onto, so an anonymous session is the
	// sound reading.
	if strings.Contains(html, sessionAuthYtcfgMark) {
		return SessionAuthLoggedOut
	}
	return SessionAuthUnknown
}

// sessionAuthFromBytes is watchPageSessionAuth over raw response bytes.
//
// It exists because callers holding a ~1MB page as []byte must not pay a
// string copy just to read one flag — internal/youtube/channel_membership.go
// is explicit about that cost (98k → 32 allocs from lazy decoding). Three
// bytes.Index/Contains calls, no allocation.
//
// KEEP IN SYNC with watchPageSessionAuth. TestSessionAuthFromBytesMatchesStringVersion
// enforces it. The two remain separate functions because the Index calls
// differ and that difference IS the reason this one exists; the part most
// likely to drift — reading the value — is shared through the generic
// sessionAuthValue rather than written twice.
func sessionAuthFromBytes(b []byte) SessionAuthState {
	if i := bytes.Index(b, []byte(sessionAuthKey)); i >= 0 {
		return sessionAuthValue(b[i+len(sessionAuthKey):])
	}
	if i := bytes.Index(b, []byte(sessionAuthCamelKey)); i >= 0 {
		return sessionAuthValue(b[i+len(sessionAuthCamelKey):])
	}
	if bytes.Contains(b, []byte(sessionAuthYtcfgMark)) {
		return SessionAuthLoggedOut
	}
	return SessionAuthUnknown
}

// livenessVerdict is sessionAuthFromBytes with the ytcfg fallback removed.
//
// That fallback ("a shell carrying ytcfg.set but no login key is anonymous")
// is sound for watch pages, which may legitimately omit the key. It is NOT
// safe for the liveness probe: a consent interstitial carrying ytcfg would
// read as a dead session and alarm an operator whose cookies are fine —
// from an EU or datacenter IP, i.e. the Docker deployment this targets.
//
// The probe does not need it. Measured 2026-08-25, both probe pages stamp
// the explicit key in both directions (anonymous false / authenticated
// true), on /feed/subscriptions and /channel/<id>/membership alike. So
// requiring the explicit marker costs nothing real and makes the consent
// question moot: an unrecognised page is Unknown, which is the truthful
// answer for a page we cannot read.
//
// Both routes to the ytcfg branch are now closed, and by two independent
// mechanisms — so neither one alone is load-bearing. Key absent: the guard
// below returns Unknown before delegating at all. Key present but its value
// unreadable: sessionAuthFromBytes returns sessionAuthValue's Unknown from
// the key branch and never reaches the bootstrap. That second case is the
// one worth stating, because "unreadable" is a verdict this function must
// pass through unchanged — routing it into the bootstrap would answer
// LoggedOut off a shell, which is the alarm the value reader exists to
// prevent. TestUnreadableValueDoesNotFallThrough pins it here, not just one
// layer down.
func livenessVerdict(b []byte) SessionAuthState {
	if !bytes.Contains(b, []byte(sessionAuthKey)) && !bytes.Contains(b, []byte(sessionAuthCamelKey)) {
		return SessionAuthUnknown
	}
	return sessionAuthFromBytes(b)
}

// extractChatContinuation pulls the live-chat continuation token (and its
// isReplay flag) out of a watch page's ytInitialData blob. Returns an empty
// token + descriptive error when chat isn't available (most non-live videos,
// streams with chat disabled, etc.) — callers treat that as "no chat" rather
// than a hard failure. Shape mirrors chat.ExtractChatContinuation; duplicated
// here so the youtube package owns its own extraction and watch_page.go can
// drop the raw HTML before returning. json.Unmarshal allocates fresh strings,
// so the returned token does not alias the html backing array.
func extractChatContinuation(html string) (string, bool, error) {
	m := ytInitialDataRegex.FindStringSubmatch(html)
	if m == nil {
		return "", false, fmt.Errorf("ytInitialData not found")
	}

	var data map[string]any
	if err := json.Unmarshal([]byte(m[1]), &data); err != nil {
		return "", false, fmt.Errorf("parse ytInitialData: %w", err)
	}

	contents, _ := data["contents"].(map[string]any)
	twoCol, _ := contents["twoColumnWatchNextResults"].(map[string]any)
	convBar, _ := twoCol["conversationBar"].(map[string]any)
	chatRenderer, _ := convBar["liveChatRenderer"].(map[string]any)
	if chatRenderer == nil {
		return "", false, fmt.Errorf("no liveChatRenderer found")
	}

	isReplay, _ := chatRenderer["isReplay"].(bool)

	conts, _ := chatRenderer["continuations"].([]any)
	if len(conts) == 0 {
		return "", false, fmt.Errorf("no continuations found")
	}

	contKeys := []string{
		"reloadContinuationData",
		"invalidationContinuationData",
		"timedContinuationData",
		"liveChatReplayContinuationData",
	}
	for _, cont := range conts {
		contMap, _ := cont.(map[string]any)
		if contMap == nil {
			continue
		}
		for _, key := range contKeys {
			if contData, ok := contMap[key].(map[string]any); ok {
				if token, ok := contData["continuation"].(string); ok && token != "" {
					return token, isReplay, nil
				}
			}
		}
	}

	return "", false, fmt.Errorf("no continuation token found")
}

// normalizePlayerJSURL un-escapes a JSON-encoded URL (YouTube emits forward
// slashes as \/ inside its JSON blobs) and prefixes the YouTube base URL for
// absolute-path URLs. Keeps cipher compilation robust against literal
// backslashes that can trip some parsers downstream.
//
// The final Clone matters: raw is a regexp submatch of the full ~5 MB watch
// page, and when the URL needs neither un-escaping nor a base prefix,
// ReplaceAll returns it unchanged — an alias that would pin the whole page
// in memory for as long as the PlayerURL lives (job state, solver caches).
// Same discipline as the VisitorData Clone below.
func normalizePlayerJSURL(raw string) string {
	u := strings.ReplaceAll(raw, `\/`, "/")
	if strings.HasPrefix(u, "/") {
		return constants.YouTubeURLs.Base + u
	}
	return strings.Clone(u)
}

func extractYtcfgAndPlayerResponse(html string) (*YtcfgData, map[string]any) {
	ytcfg := &YtcfgData{}

	// Extract player URL
	if m := jsURLRegex.FindStringSubmatch(html); m != nil {
		ytcfg.PlayerURL = normalizePlayerJSURL(m[1])
	}

	// Extract visitor data. strings.Clone breaks the substring→backing-array
	// alias: regexp.FindStringSubmatch returns substrings of `html` (the full
	// ~5 MB watch-page response). Without Clone, storing m[1] anywhere
	// persistent (Service.visitorData, sessionCache keys, etc.) pins the
	// entire HTML in memory. Per-poll leak observed at ~5 MB/min in pprof.
	if m := visitorDataRegex.FindStringSubmatch(html); m != nil {
		ytcfg.VisitorData = strings.Clone(m[1])
	}

	// Extract session index
	if m := sessionIndexRegex.FindStringSubmatch(html); m != nil {
		if idx, err := strconv.Atoi(m[1]); err == nil {
			ytcfg.SessionIndex = &idx
		}
	}

	// Extract delegated session ID (Clone — see VisitorData comment).
	if m := delegatedSessionRegex.FindStringSubmatch(html); m != nil {
		ytcfg.DelegatedSessionID = strings.Clone(m[1])
	}

	// Extract datasync ID (Clone — see VisitorData comment).
	if m := dataSyncIDRegex.FindStringSubmatch(html); m != nil {
		ytcfg.DataSyncID = strings.Clone(m[1])
	}

	// Detect the experiment that switches GVS PO-token binding to the video
	// ID (see GvsContentBinding). Presence of the flag anywhere in the page's
	// player configs is sufficient — yt-dlp parses each config's
	// serializedExperimentFlags and takes the last value, but YouTube ships
	// the same value across all of them.
	ytcfg.GvsBindToVideoID = gvsBindVideoIDRegex.MatchString(html)

	// Extract ytInitialPlayerResponse (try multiple patterns)
	var playerResponse map[string]any
	for _, re := range playerResponsePatterns {
		if m := re.FindStringSubmatch(html); m != nil {
			if err := json.Unmarshal([]byte(m[1]), &playerResponse); err == nil {
				// Extract video metadata from response
				if vd, ok := playerResponse["videoDetails"].(map[string]any); ok {
					if title, ok := vd["title"].(string); ok {
						ytcfg.Title = title
					}
					if author, ok := vd["author"].(string); ok {
						ytcfg.Author = author
					}
					if channelID, ok := vd["channelId"].(string); ok {
						ytcfg.ChannelID = channelID
					}
					if desc, ok := vd["shortDescription"].(string); ok {
						ytcfg.Description = desc
					}
					if thumb, ok := vd["thumbnail"].(map[string]any); ok {
						if thumbs, ok := thumb["thumbnails"].([]any); ok && len(thumbs) > 0 {
							if last, ok := thumbs[len(thumbs)-1].(map[string]any); ok {
								if url, ok := last["url"].(string); ok {
									ytcfg.ThumbnailURL = url
								}
							}
						}
					}
				}
				break
			}
		}
	}

	return ytcfg, playerResponse
}

// DefaultYtcfg creates a default (empty) ytcfg.
func DefaultYtcfg() *YtcfgData {
	return &YtcfgData{}
}

func extractEncryptedHostFlags(html string) string {
	if m := encryptedHostFlagsRegex.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// EmbedPageResult contains data extracted from a YouTube embed page.
type EmbedPageResult struct {
	EncryptedHostFlags string
	PlayerURL          string
}

// FetchEmbedPage fetches a YouTube embed page and extracts encryptedHostFlags.
func FetchEmbedPage(ctx context.Context, videoID string) (*EmbedPageResult, error) {
	url := fmt.Sprintf("%s/%s?html5=1", constants.YouTubeURLs.Embed, videoID)

	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
	}

	body, err := utils.FetchBody(ctx, url, 30*time.Second, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch embed page: %w", err)
	}

	html := string(body)
	result := &EmbedPageResult{
		EncryptedHostFlags: extractEncryptedHostFlags(html),
	}

	// Also extract player URL from embed page
	if m := jsURLRegex.FindStringSubmatch(html); m != nil {
		result.PlayerURL = normalizePlayerJSURL(m[1])
	}

	return result, nil
}

// extractAttestationChallenge pulls the BotGuard bgChallenge out of a watch
// page's window.ytAtN(...) blob. The blob is a JS object literal whose "R"
// key holds a JSON string; inside that is bgChallenge. Returns compact JSON
// of bgChallenge, or "" on any miss/parse failure — absence is a normal
// result (the POT sidecar falls back to /att/get), never an error.
func extractAttestationChallenge(html string) (challenge, reason string) {
	loc := ytAtNOpenRe.FindStringIndex(html)
	if loc == nil {
		return "", atnNoCall
	}
	// The regex ends on the literal '{'; rescan from there so the object's
	// true extent comes from brace balancing, not from the first `})`.
	obj, ok := scanBalancedObject(html[loc[1]-1:])
	if !ok {
		return "", atnUnbalanced
	}
	jsonStr, err := utils.JSToJSON(obj, nil, false)
	if err != nil {
		return "", atnJSConvert
	}
	var outer map[string]any
	if json.Unmarshal([]byte(jsonStr), &outer) != nil {
		return "", atnOuterParse
	}
	rStr, ok := outer["R"].(string)
	if !ok {
		return "", atnNoRKey
	}
	var r struct {
		BgChallenge json.RawMessage `json:"bgChallenge"`
	}
	if json.Unmarshal([]byte(rStr), &r) != nil {
		return "", atnRParse
	}
	if len(r.BgChallenge) == 0 {
		return "", atnNoChallenge
	}
	canonical, reason := canonicalizeChallenge(r.BgChallenge)
	if reason != atnOK {
		return "", reason
	}
	return canonical, atnOK
}

// validateChallengeOrigin enforces that a page-sourced challenge points its
// interpreter at a Google host. A challenge carrying inline
// interpreterJavascript instead is REFUSED rather than honoured: bgutils-js
// treats the two fields as interchangeable, but inline script from HTML we
// scraped would be executed with no origin to check at all. Such a challenge
// falls back to the sidecar's /att/get flow, whose response is a real YouTube
// API result and may carry inline script safely.
// canonicalizeChallenge validates a page-sourced challenge and REBUILDS it
// from only the fields the sidecar actually consumes, rather than forwarding
// the attacker-adjacent original.
//
// Rebuilding is the point. Forwarding the raw object made the Go gate's
// guarantee false: Go's encoding/json matches field names case-insensitively
// and lets the LAST match win, so a decoy `PRIVATEDONOTACCESS…` key placed
// after the real one had Go validate www.google.com while the sidecar's
// JSON.parse (case-sensitive, also last-wins) read a different host from the
// same bytes. Every such parser differential — and any extra key, including
// an inline interpreterJavascript riding alongside a valid URL — disappears
// when the sidecar receives a freshly-marshaled object containing exactly the
// three fields it uses and nothing else.
//
// Keys are looked up EXACTLY (case-sensitively) via a RawMessage map, so a
// case-variant decoy is ignored rather than preferred.
func canonicalizeChallenge(raw json.RawMessage) (canonical, reason string) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return "", atnChallengeShape
	}

	var program, globalName string
	if json.Unmarshal(fields["program"], &program) != nil || program == "" {
		return "", atnChallengeShape
	}
	// globalName is optional in bgutils-js's type; absence is not fatal.
	_ = json.Unmarshal(fields["globalName"], &globalName)

	rawURL, ok := fields["interpreterUrl"]
	if !ok {
		return "", atnNoInterpURL
	}
	var urlFields map[string]json.RawMessage
	if json.Unmarshal(rawURL, &urlFields) != nil {
		return "", atnChallengeShape
	}
	var value string
	if json.Unmarshal(urlFields["privateDoNotAccessOrElseTrustedResourceUrlWrappedValue"], &value) != nil || value == "" {
		return "", atnNoInterpURL
	}
	// YouTube ships this protocol-relative ("//host/path"); the sidecar
	// prefixes "https:" before fetching, so parse it the same way.
	u, err := url.Parse("https:" + value)
	if err != nil || u.Hostname() == "" {
		return "", atnBadInterpHost
	}
	if u.User != nil {
		// Userinfo means the authority's host is not what a careless reader
		// sees; refuse rather than reason about it.
		return "", atnBadInterpHost + ": userinfo present"
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if !slices.Contains(allowedInterpreterHosts, host) {
		return "", atnBadInterpHost + ": " + host
	}
	// An allowlisted host is NOT the same as Google-authored bytes. www.google.com
	// serves JSONP endpoints that reflect an attacker-supplied callback back at
	// HTTP 200 — e.g. /complete/search?client=firefox&jsonp=<payload> — and the
	// sidecar executes whatever it fetches. That reached RCE through a fully
	// "valid" allowlisted host with no redirect involved (adversarial review,
	// round 2, 2026-08-15).
	//
	// The genuine interpreter is a static TrustedResourceUrl: a bare .js path
	// with no query and no fragment (observed //www.google.com/js/th/<hash>.js).
	// Requiring exactly that shape removes every reflection endpoint on the
	// allowlisted hosts, since reflection needs a query to reflect.
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", atnBadInterpPath + ": carries a query or fragment"
	}
	// Validate the RAW (still-encoded) path, not the decoded one. Go decodes
	// %3F into u.Path while JS keeps it encoded, so a URL like
	// /complete/search%3Fjsonp=<payload>.js reads as a query-less .js path to
	// Go and as a literal-percent path to the sidecar. That particular URL
	// 404s at Google today, which is the only reason it is not exploitable —
	// safety should not rest on the origin's decoding behaviour, so the
	// allowed alphabet simply excludes '%' (adversarial review round 3).
	if !staticScriptPathRe.MatchString(u.EscapedPath()) {
		return "", atnBadInterpPath + ": " + u.EscapedPath()
	}

	out, err := json.Marshal(map[string]any{
		"program":    program,
		"globalName": globalName,
		"interpreterUrl": map[string]string{
			"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue": value,
		},
	})
	if err != nil {
		return "", atnChallengeShape
	}
	return string(out), atnOK
}

// scanBalancedObject returns the complete `{...}` literal starting at s[0],
// tracking JS string state so braces inside quoted payloads never affect the
// depth count. Returns ok=false when the literal never closes.
func scanBalancedObject(s string) (string, bool) {
	if len(s) == 0 || s[0] != '{' {
		return "", false
	}
	depth := 0
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
}
