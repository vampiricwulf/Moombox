package utils

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// videoIDRegex matches a YouTube video ID: exactly 11 characters from
// [a-zA-Z0-9_-]. Note that this regex also matches some strings that
// resemble Twitch v-prefixed VOD IDs (e.g., "v1234567890"), so when an
// input could be either, prefer disambiguating by host or surrounding
// context before falling back to ExtractVideoID. See utils/media.go's
// ExtractMediaID and the associated "v-prefixed VOD on bare input"
// test in media_test.go for the resolution policy.
var videoIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)

// ExtractVideoID extracts a YouTube video ID from various URL formats.
// Supported formats:
//   - youtube.com/watch?v=ID
//   - youtu.be/ID
//   - youtube.com/live/ID
//   - youtube.com/shorts/ID
//   - youtube.com/embed/ID
//   - youtube.com/v/ID
//   - bare 11-character ID (see videoIDRegex godoc for the Twitch
//     v-prefixed VOD collision)
func ExtractVideoID(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}

	// Check if it's already a bare video ID
	if videoIDRegex.MatchString(input) {
		return input
	}

	// Normalize bare URLs without protocol (e.g., youtube.com/watch?v=... or youtu.be/...)
	if strings.HasPrefix(input, "youtube.com/") || strings.HasPrefix(input, "www.youtube.com/") ||
		strings.HasPrefix(input, "m.youtube.com/") || strings.HasPrefix(input, "youtu.be/") {
		input = "https://" + input
	}

	// Try parsing as URL
	u, err := url.Parse(input)
	if err != nil {
		return ""
	}

	host := strings.ToLower(u.Hostname())

	// youtu.be/ID
	if host == "youtu.be" {
		id := strings.TrimPrefix(u.Path, "/")
		if videoIDRegex.MatchString(id) {
			return id
		}
		return ""
	}

	// youtube.com variants
	if host != "www.youtube.com" && host != "youtube.com" && host != "m.youtube.com" {
		return ""
	}

	path := u.Path

	// /watch?v=ID
	if path == "/watch" {
		v := u.Query().Get("v")
		if videoIDRegex.MatchString(v) {
			return v
		}
		return ""
	}

	// /live/ID, /shorts/ID, /embed/ID, /v/ID
	prefixes := []string{"/live/", "/shorts/", "/embed/", "/v/"}
	for _, prefix := range prefixes {
		if id, ok := strings.CutPrefix(path, prefix); ok {
			// Remove trailing path segments
			id, _, _ = strings.Cut(id, "/")
			if videoIDRegex.MatchString(id) {
				return id
			}
		}
	}

	return ""
}

// IsVideoID returns true if the string is a valid YouTube video ID.
func IsVideoID(s string) bool {
	return videoIDRegex.MatchString(s)
}

var channelIDRegex = regexp.MustCompile(`^UC[a-zA-Z0-9_-]{22}$`)

// YouTubeChannelInput represents a parsed YouTube channel URL.
type YouTubeChannelInput struct {
	ChannelID string // Set if /channel/UCxxx (no network needed)
	Path      string // Set if /@Handle, /c/Name, /user/Name (needs resolution)
}

// ParseYouTubeChannelURL detects YouTube channel-path URLs and extracts the channel reference.
// Returns nil for non-channel URLs (video/watch URLs, bare IDs, etc.).
func ParseYouTubeChannelURL(input string) *YouTubeChannelInput {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}

	// Normalize bare URLs without protocol
	if strings.HasPrefix(input, "youtube.com/") || strings.HasPrefix(input, "www.youtube.com/") ||
		strings.HasPrefix(input, "m.youtube.com/") {
		input = "https://" + input
	}

	u, err := url.Parse(input)
	if err != nil {
		return nil
	}

	host := strings.ToLower(u.Hostname())
	if host != "www.youtube.com" && host != "youtube.com" && host != "m.youtube.com" {
		return nil
	}

	path := u.Path

	// /channel/UCxxxxxxxx → extract directly
	if id, ok := strings.CutPrefix(path, "/channel/"); ok {
		id, _, _ = strings.Cut(id, "/")
		if channelIDRegex.MatchString(id) {
			return &YouTubeChannelInput{ChannelID: id}
		}
		return nil
	}

	// /@Handle → needs resolution
	if strings.HasPrefix(path, "/@") {
		handle := strings.TrimPrefix(path, "/")
		handle, _, _ = strings.Cut(handle, "/")
		if len(handle) > 1 { // at least "@X"
			return &YouTubeChannelInput{Path: "/" + handle}
		}
		return nil
	}

	// /c/CustomName → needs resolution
	if name, ok := strings.CutPrefix(path, "/c/"); ok {
		name, _, _ = strings.Cut(name, "/")
		if name != "" {
			return &YouTubeChannelInput{Path: "/c/" + name}
		}
		return nil
	}

	// /user/Username → needs resolution
	if name, ok := strings.CutPrefix(path, "/user/"); ok {
		name, _, _ = strings.Cut(name, "/")
		if name != "" {
			return &YouTubeChannelInput{Path: "/user/" + name}
		}
		return nil
	}

	return nil
}

var (
	canonicalChannelRe = regexp.MustCompile(`<link\s+rel="canonical"\s+href="https://www\.youtube\.com/channel/(UC[a-zA-Z0-9_-]{22})"`)
	externalIDRe       = regexp.MustCompile(`"externalId"\s*:\s*"(UC[a-zA-Z0-9_-]{22})"`)
	ogTitleRe          = regexp.MustCompile(`<meta\s+property="og:title"\s+content="([^"]+)"`)
	titleRe            = regexp.MustCompile(`<title>([^<]+)</title>`)

	// youtubeBaseURL is the base for ResolveYouTubeChannel page fetches.
	// Tests override this to point at an httptest server. Production
	// always points at the real YouTube origin.
	youtubeBaseURL = "https://www.youtube.com"
)

// YouTubeChannelInfo holds resolved YouTube channel information.
type YouTubeChannelInfo struct {
	ChannelID string
	Name      string
}

// ResolveYouTubeChannel fetches a YouTube channel page and extracts the channel ID and name.
// urlPath should be like "/@Handle", "/c/Name", or "/user/Name".
//
// Retries up to 3 times with 1s/2s exponential backoff on transient
// failures (network errors, HTTP 5xx surfaces as "HTTP 5xx" in the
// FetchBody error string). HTTP 4xx errors and ctx cancellation
// fast-fail. Audit reports/small-packages.md.
func ResolveYouTubeChannel(ctx context.Context, urlPath string) (*YouTubeChannelInfo, error) {
	pageURL := youtubeBaseURL + urlPath
	headers := map[string]string{
		"User-Agent": constants.UserAgents.Web,
	}

	var body []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		body, err = FetchBody(ctx, pageURL, 15*time.Second, headers)
		if err == nil {
			break
		}
		// Don't retry 4xx — those are deterministic ("not found", auth
		// missing, etc.) and won't fix themselves with a second try.
		es := err.Error()
		if strings.Contains(es, "HTTP 4") {
			return nil, fmt.Errorf("failed to fetch channel page: %w", err)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch channel page after retries: %w", err)
	}

	page := string(body)

	// Extract channel ID from canonical link
	var channelID string
	if m := canonicalChannelRe.FindStringSubmatch(page); m != nil {
		channelID = m[1]
	} else if m := externalIDRe.FindStringSubmatch(page); m != nil {
		// Fallback: extract from ytInitialData JSON
		channelID = m[1]
	}

	if channelID == "" {
		return nil, fmt.Errorf("could not find channel ID on page %s", pageURL)
	}

	// Extract display name
	var name string
	if m := ogTitleRe.FindStringSubmatch(page); m != nil {
		name = html.UnescapeString(m[1])
	} else if m := titleRe.FindStringSubmatch(page); m != nil {
		name = html.UnescapeString(m[1])
		name = strings.TrimSuffix(name, " - YouTube")
		name = strings.TrimSpace(name)
	}

	return &YouTubeChannelInfo{
		ChannelID: channelID,
		Name:      name,
	}, nil
}
