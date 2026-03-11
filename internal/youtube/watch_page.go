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
	jsURLRegex             = regexp.MustCompile(`"(?:jsUrl|PLAYER_JS_URL)":"([^"]+)"`)
	visitorDataRegex       = regexp.MustCompile(`"visitorData":"([^"]+)"`)
	sessionIndexRegex      = regexp.MustCompile(`"SESSION_INDEX":"?(\d+)"?`)
	delegatedSessionRegex  = regexp.MustCompile(`"DELEGATED_SESSION_ID":"([^"]+)"`)
	dataSyncIDRegex        = regexp.MustCompile(`"datasyncId":"([^"]+)"`)
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
)

// WatchPageResult contains data extracted from a YouTube watch page.
type WatchPageResult struct {
	HTML           string
	Ytcfg          *YtcfgData
	IsLoggedIn     bool
	PlayerResponse map[string]interface{}
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

	return &WatchPageResult{
		HTML:           html,
		Ytcfg:          ytcfg,
		IsLoggedIn:     isLoggedIn,
		PlayerResponse: playerResponse,
	}, nil
}

func extractYtcfgAndPlayerResponse(html string) (*YtcfgData, map[string]interface{}) {
	ytcfg := &YtcfgData{}

	// Extract player URL
	if m := jsURLRegex.FindStringSubmatch(html); m != nil {
		ytcfg.PlayerURL = m[1]
		if strings.HasPrefix(ytcfg.PlayerURL, "/") {
			ytcfg.PlayerURL = constants.YouTubeURLs.Base + ytcfg.PlayerURL
		}
	}

	// Extract visitor data
	if m := visitorDataRegex.FindStringSubmatch(html); m != nil {
		ytcfg.VisitorData = m[1]
	}

	// Extract session index
	if m := sessionIndexRegex.FindStringSubmatch(html); m != nil {
		if idx, err := strconv.Atoi(m[1]); err == nil {
			ytcfg.SessionIndex = &idx
		}
	}

	// Extract delegated session ID
	if m := delegatedSessionRegex.FindStringSubmatch(html); m != nil {
		ytcfg.DelegatedSessionID = m[1]
	}

	// Extract datasync ID
	if m := dataSyncIDRegex.FindStringSubmatch(html); m != nil {
		ytcfg.DataSyncID = m[1]
	}

	// Extract ytInitialPlayerResponse (try multiple patterns)
	var playerResponse map[string]interface{}
	for _, re := range playerResponsePatterns {
		if m := re.FindStringSubmatch(html); m != nil {
			if err := json.Unmarshal([]byte(m[1]), &playerResponse); err == nil {
				// Extract video metadata from response
				if vd, ok := playerResponse["videoDetails"].(map[string]interface{}); ok {
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
					if thumb, ok := vd["thumbnail"].(map[string]interface{}); ok {
						if thumbs, ok := thumb["thumbnails"].([]interface{}); ok && len(thumbs) > 0 {
							if last, ok := thumbs[len(thumbs)-1].(map[string]interface{}); ok {
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
		playerURL := m[1]
		if strings.HasPrefix(playerURL, "/") {
			playerURL = constants.YouTubeURLs.Base + playerURL
		}
		result.PlayerURL = playerURL
	}

	return result, nil
}
