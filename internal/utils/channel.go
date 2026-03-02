package utils

import (
	"context"
	"strings"
)

// ResolvedChannel holds the result of resolving a channel URL or identifier.
type ResolvedChannel struct {
	ID       string // YouTube channel ID (UCxxx) or Twitch login
	Name     string // Display name (may be empty)
	Platform string // "youtube", "twitch", or "" (unknown)
}

// ResolveChannelInput parses a channel URL or identifier and resolves it to an ID.
// For YouTube handle/custom URLs, this makes a network request to resolve the channel ID.
// For direct channel IDs and Twitch URLs, no network request is needed.
// Returns nil with no error if the input doesn't look like a URL needing resolution.
func ResolveChannelInput(ctx context.Context, input string) (*ResolvedChannel, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}

	// Try Twitch URL extraction
	if target := ExtractTwitchTarget(input); target != nil && target.Type == TwitchChannel {
		return &ResolvedChannel{
			ID:       target.Value,
			Platform: "twitch",
		}, nil
	}

	// Try YouTube channel URL
	if parsed := ParseYouTubeChannelURL(input); parsed != nil {
		if parsed.ChannelID != "" {
			// Direct /channel/UCxxx — no network needed
			return &ResolvedChannel{
				ID:       parsed.ChannelID,
				Platform: "youtube",
			}, nil
		}
		// Handle/custom/user URL — fetch page to resolve
		info, err := ResolveYouTubeChannel(ctx, parsed.Path)
		if err != nil {
			return nil, err
		}
		return &ResolvedChannel{
			ID:       info.ChannelID,
			Name:     info.Name,
			Platform: "youtube",
		}, nil
	}

	// Not a recognized URL — return as-is with empty platform
	return nil, nil
}

// LooksLikeURL returns true if the input looks like a URL (contains a domain).
func LooksLikeURL(input string) bool {
	input = strings.TrimSpace(input)
	return strings.Contains(input, "youtube.com/") || strings.Contains(input, "youtu.be/") ||
		strings.Contains(input, "twitch.tv/")
}
