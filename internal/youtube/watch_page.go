package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

	return &WatchPageResult{
		Ytcfg:            ytcfg,
		IsLoggedIn:       isLoggedIn,
		PlayerResponse:   playerResponse,
		ChatContinuation: chatContinuation,
		ChatIsReplay:     chatIsReplay,
		ChatErr:          chatErr,
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
