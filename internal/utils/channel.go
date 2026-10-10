package utils

import (
	"context"
	"errors"
	"strings"
	"unicode"
)

// ResolvedChannel holds the result of resolving a channel URL or identifier.
type ResolvedChannel struct {
	ID       string // YouTube channel ID (UCxxx) or Twitch login
	Name     string // Display name (may be empty)
	Platform string // "youtube", "twitch", or "" (unknown)
}

// ErrNotChannelURL is NormalizeChannelID's refusal of an input that has to
// be resolved — a URL, or a bare @handle — but names no YouTube or Twitch
// channel (a watch URL, a twitch.tv/videos link). Its text is the one every
// writer shows: POST /api/config/channels' 400, PUT /api/config's field
// error, and both TUI channel editors.
var ErrNotChannelURL = errors.New("not a YouTube or Twitch channel URL")

// ResolveChannelInput parses a channel URL or identifier and resolves it to an ID.
// For YouTube handle/custom URLs, this makes a network request to resolve the channel ID.
// For direct channel IDs and Twitch URLs, no network request is needed.
// A bare @handle is a YouTube handle, resolved exactly as its
// youtube.com/@handle URL is — both channel dialogs advertise the form, and
// stored verbatim it named no channel the monitors could poll.
// Returns nil with no error if the input doesn't look like a URL needing resolution.
func ResolveChannelInput(ctx context.Context, input string) (*ResolvedChannel, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}
	if strings.HasPrefix(input, "@") {
		// A handle has no whitespace: "@foo bar" is no handle, and its URL
		// form would only 404.
		if strings.ContainsFunc(input, unicode.IsSpace) {
			return nil, nil
		}
		input = "https://www.youtube.com/" + input
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

// NeedsChannelResolve reports whether NormalizeChannelID has to resolve
// input rather than take it as typed: a youtube.com / youtu.be / twitch.tv
// URL, or a bare @handle. Resolving a YouTube handle is a page fetch with
// retries, so the writers decide on this whether to rate limit (POST
// /api/config/channels, PUT /api/config) or to go asynchronous (the TUI
// editors). The dashboard's channelInputNeedsResolve
// (web/public/modules/utils.js) is the same rule.
func NeedsChannelResolve(input string) bool {
	input = strings.TrimSpace(input)
	return LooksLikeURL(input) || strings.HasPrefix(input, "@")
}

// NormalizeChannelID is the one normaliser every channel-ID writer runs —
// POST /api/config/channels, PUT /api/config's channels[] (and the web
// wizard's /api/setup/complete, which shares that path), the TUI Settings
// channel editor and the TUI setup wizard — so that none of them stores an
// ID the monitors cannot poll. The ID is trimmed; a URL or a bare @handle
// (NeedsChannelResolve) is resolved to the channel's ID, carrying the name
// and platform the resolution found; one that names no channel is refused
// with ErrNotChannelURL, and a lookup that fails returns its error. Any other
// input comes back trimmed, with no name and no platform. An empty input
// comes back empty: each writer words its own "required".
func NormalizeChannelID(ctx context.Context, input string) (*ResolvedChannel, error) {
	id := strings.TrimSpace(input)
	if !NeedsChannelResolve(id) {
		return &ResolvedChannel{ID: id}, nil
	}
	resolved, err := ResolveChannelInput(ctx, id)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		return nil, ErrNotChannelURL
	}
	return resolved, nil
}
