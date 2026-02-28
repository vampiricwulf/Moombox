package utils

import (
	"net/url"
	"regexp"
	"strings"
)

var videoIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)

// ExtractVideoID extracts a YouTube video ID from various URL formats.
// Supported formats:
//   - youtube.com/watch?v=ID
//   - youtu.be/ID
//   - youtube.com/live/ID
//   - youtube.com/shorts/ID
//   - youtube.com/embed/ID
//   - youtube.com/v/ID
//   - bare 11-character ID
func ExtractVideoID(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}

	// Check if it's already a bare video ID
	if videoIDRegex.MatchString(input) {
		return input
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
		if strings.HasPrefix(path, prefix) {
			id := strings.TrimPrefix(path, prefix)
			// Remove trailing path segments
			if idx := strings.Index(id, "/"); idx >= 0 {
				id = id[:idx]
			}
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
