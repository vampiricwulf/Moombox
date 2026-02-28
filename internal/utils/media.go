package utils

import "strings"

// MediaTarget represents a parsed media target from user input.
type MediaTarget struct {
	Platform string        // "youtube" or "twitch"
	VideoID  string        // YouTube video ID (only for YouTube)
	Twitch   *TwitchTarget // Twitch target details (only for Twitch)
}

// ExtractMediaID extracts a media target from user input (URL or ID).
// Tries YouTube extraction first (more specific patterns), then Twitch.
func ExtractMediaID(input string) *MediaTarget {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil
	}

	// Check for explicit Twitch URLs first
	if strings.Contains(trimmed, "twitch.tv") || strings.Contains(trimmed, "clips.twitch.tv") {
		if target := ExtractTwitchTarget(trimmed); target != nil {
			return &MediaTarget{Platform: "twitch", Twitch: target}
		}
	}

	// Try YouTube
	if videoID := ExtractVideoID(trimmed); videoID != "" {
		return &MediaTarget{Platform: "youtube", VideoID: videoID}
	}

	// Try Twitch (raw usernames, VOD IDs, etc.)
	if target := ExtractTwitchTarget(trimmed); target != nil {
		return &MediaTarget{Platform: "twitch", Twitch: target}
	}

	return nil
}
