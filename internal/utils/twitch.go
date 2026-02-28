package utils

import (
	"net/url"
	"regexp"
	"strings"
)

// TwitchTargetType represents the type of Twitch content.
type TwitchTargetType string

const (
	TwitchChannel TwitchTargetType = "channel"
	TwitchVOD     TwitchTargetType = "vod"
	TwitchClip    TwitchTargetType = "clip"
)

// TwitchTarget represents a parsed Twitch URL or identifier.
type TwitchTarget struct {
	Type  TwitchTargetType
	Value string // login name, VOD ID, or clip slug
}

var (
	twitchLoginRegex    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,24}$`)
	twitchVODPrefixRe   = regexp.MustCompile(`^v(\d{1,12})$`)
	twitchVODBareRe     = regexp.MustCompile(`^\d{7,12}$`)
	twitchReservedPaths = map[string]bool{
		"directory": true, "downloads": true, "jobs": true,
		"settings": true, "videos": true, "search": true, "p": true,
	}
)

// ExtractTwitchTarget extracts a Twitch target from a URL or identifier.
func ExtractTwitchTarget(input string) *TwitchTarget {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}

	// Normalize bare twitch.tv/... input without protocol
	if strings.HasPrefix(input, "twitch.tv/") || strings.HasPrefix(input, "www.twitch.tv/") || strings.HasPrefix(input, "clips.twitch.tv/") {
		input = "https://" + input
	}

	// Try parsing as URL
	u, err := url.Parse(input)
	if err == nil {
		host := strings.ToLower(u.Host)
		if host == "www.twitch.tv" || host == "twitch.tv" || host == "m.twitch.tv" {
			return parseTwitchURL(u)
		}
		// clips.twitch.tv/{slug}
		if host == "clips.twitch.tv" {
			slug := strings.TrimPrefix(u.Path, "/")
			if slug != "" {
				return &TwitchTarget{Type: TwitchClip, Value: slug}
			}
		}
	}

	// Try as v-prefixed VOD ID (v123456)
	if matches := twitchVODPrefixRe.FindStringSubmatch(input); matches != nil {
		return &TwitchTarget{Type: TwitchVOD, Value: matches[1]}
	}

	// Try as bare numeric VOD ID (7-12 digits to distinguish from other numbers)
	if twitchVODBareRe.MatchString(input) {
		return &TwitchTarget{Type: TwitchVOD, Value: input}
	}

	// Try as bare login (must start with letter)
	if twitchLoginRegex.MatchString(input) {
		return &TwitchTarget{Type: TwitchChannel, Value: strings.ToLower(input)}
	}

	return nil
}

func parseTwitchURL(u *url.URL) *TwitchTarget {
	path := strings.TrimPrefix(u.Path, "/")
	parts := strings.Split(path, "/")

	if len(parts) == 0 || parts[0] == "" {
		return nil
	}

	// /videos/123456
	if parts[0] == "videos" && len(parts) >= 2 {
		return &TwitchTarget{Type: TwitchVOD, Value: parts[1]}
	}

	// /channel/clip/slug
	if len(parts) >= 3 && parts[1] == "clip" {
		return &TwitchTarget{Type: TwitchClip, Value: parts[2]}
	}

	// /channel/video/123456
	if len(parts) >= 3 && parts[1] == "video" {
		return &TwitchTarget{Type: TwitchVOD, Value: parts[2]}
	}

	// /channel (skip reserved paths)
	login := strings.ToLower(parts[0])
	if twitchReservedPaths[login] {
		return nil
	}
	if twitchLoginRegex.MatchString(login) {
		return &TwitchTarget{Type: TwitchChannel, Value: login}
	}

	return nil
}
