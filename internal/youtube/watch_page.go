package youtube

import (
	"context"
	"encoding/json"
	"fmt"
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
// The list holds only domains that serve GOOGLE-AUTHORED code. Google-owned
// domains whose bytes are USER-uploaded are deliberately excluded even though
// they are equally "Google": googleusercontent.com, ggpht.com (avatars),
// googlevideo.com (media), and i.ytimg.com (thumbnails) all let a third party
// choose the response body, and the fetched body is executed — an image/JS
// polyglot uploaded there would be indistinguishable from an interpreter.
// ytimg.com is therefore admitted by exact static hosts only (see
// allowedInterpreterHosts), never as a suffix.
var allowedInterpreterDomains = []string{
	"google.com",
	"gstatic.com",
	"googleapis.com",
	"youtube.com",
	"youtube-nocookie.com",
	"google.cn",
}

// allowedInterpreterHosts are exact hosts admitted individually because their
// parent domain also serves user content. s.ytimg.com is YouTube's static
// script origin (it serves base.js); i.ytimg.com, which serves user-uploaded
// thumbnails, is intentionally absent.
var allowedInterpreterHosts = []string{
	"s.ytimg.com",
	"www.ytimg.com",
}

// regionalGoogleRe matches Google's country domains: google.de, google.co.uk,
// google.com.au, google.co.jp. Anchored at both ends and applied to the
// registrable domain only, so it can never match "google.de.evil.tld".
var regionalGoogleRe = regexp.MustCompile(`^google\.[a-z]{2,3}(\.[a-z]{2})?$`)

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
	Ytcfg            *YtcfgData
	IsLoggedIn       bool
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

	body, err := utils.FetchBody(ctx, url, 30*time.Second, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch watch page: %w", err)
	}

	html := string(body)
	isLoggedIn := strings.Contains(html, `"LOGGED_IN":true`) || strings.Contains(html, `"isLoggedIn":true`)

	ytcfg, playerResponse := extractYtcfgAndPlayerResponse(html)
	chatContinuation, chatIsReplay, chatErr := extractChatContinuation(html)
	attestationChallenge, attestationReason := extractAttestationChallenge(html)

	return &WatchPageResult{
		Ytcfg:                ytcfg,
		IsLoggedIn:           isLoggedIn,
		PlayerResponse:       playerResponse,
		ChatContinuation:     chatContinuation,
		ChatIsReplay:         chatIsReplay,
		ChatErr:              chatErr,
		AttestationChallenge: attestationChallenge,
		AttestationReason:    attestationReason,
	}, nil
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
	if reason := validateChallengeOrigin(r.BgChallenge); reason != atnOK {
		return "", reason
	}
	return string(r.BgChallenge), atnOK
}

// validateChallengeOrigin enforces that a page-sourced challenge points its
// interpreter at a Google host. A challenge carrying inline
// interpreterJavascript instead is REFUSED rather than honoured: bgutils-js
// treats the two fields as interchangeable, but inline script from HTML we
// scraped would be executed with no origin to check at all. Such a challenge
// falls back to the sidecar's /att/get flow, whose response is a real YouTube
// API result and may carry inline script safely.
func validateChallengeOrigin(raw json.RawMessage) string {
	var ch struct {
		InterpreterURL *struct {
			Value string `json:"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue"`
		} `json:"interpreterUrl"`
	}
	if json.Unmarshal(raw, &ch) != nil {
		return atnChallengeShape
	}
	if ch.InterpreterURL == nil || ch.InterpreterURL.Value == "" {
		return atnNoInterpURL
	}
	// YouTube ships this protocol-relative ("//host/path"); the sidecar
	// prefixes "https:" before fetching, so parse it the same way.
	u, err := url.Parse("https:" + ch.InterpreterURL.Value)
	if err != nil || u.Hostname() == "" {
		return atnBadInterpHost
	}
	if isGoogleOwnedHost(u.Hostname()) {
		return atnOK
	}
	return atnBadInterpHost + ": " + u.Hostname()
}

// isGoogleOwnedHost reports whether host is, or is a subdomain of, a
// Google-owned domain (allowedInterpreterDomains or a regional google.<tld>).
// Shared by the challenge-origin gate; kept as one predicate so the Go and
// sidecar copies stay comparable line-for-line.
func isGoogleOwnedHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if slices.Contains(allowedInterpreterHosts, host) {
		return true
	}
	for _, d := range allowedInterpreterDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	// Regional Google: match the registrable tail so subdomains
	// (www.google.co.uk) pass while lookalikes (google.co.uk.evil.tld) do not.
	labels := strings.Split(host, ".")
	for i := range labels {
		if regionalGoogleRe.MatchString(strings.Join(labels[i:], ".")) {
			return true
		}
	}
	return false
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
