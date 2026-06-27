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
	// twitchLoginRegex matches a valid Twitch login name. Per Twitch's
	// account creation rules: starts with a letter, then up to 24 more
	// letters / digits / underscores (25-char total cap). Case-insensitive
	// is documented but accounts are stored lowercase server-side; callers
	// should ToLower before comparing for equality.
	twitchLoginRegex  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,24}$`)
	twitchVODPrefixRe = regexp.MustCompile(`^v(\d{1,12})$`)
	twitchVODBareRe   = regexp.MustCompile(`^\d{7,12}$`)
	// twitchVODIDRe validates a VOD ID taken from a URL path. URL context
	// already disambiguates it from other numbers, so unlike twitchVODBareRe
	// it accepts short legacy IDs (< 7 digits).
	twitchVODIDRe = regexp.MustCompile(`^\d{1,12}$`)
	// twitchClipSlugRe validates a clip slug ("AwkwardSalamander-271WrMRkSrlpFvOY"
	// style or legacy CamelCase). Rejects empty and path-garbage values so a
	// malformed URL surfaces as "unrecognized input" instead of a job that
	// fails downstream with an empty/garbled ID.
	twitchClipSlugRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	// twitchReservedPaths lists first-path-segment slugs on twitch.tv that
	// are site navigation / product pages rather than channel logins, so we
	// must not treat them as channel names when parsing /<slug>.
	twitchReservedPaths = map[string]bool{
		"directory":     true,
		"downloads":     true,
		"jobs":          true,
		"settings":      true,
		"videos":        true,
		"search":        true,
		"p":             true, // /p/... promotional/product pages
		"store":         true,
		"tag":           true, // legacy tag browsing
		"tags":          true,
		"turbo":         true,
		"drops":         true,
		"wallet":        true,
		"prime":         true,
		"subs":          true,
		"subscriptions": true,
		"friends":       true,
		"inventory":     true,
		"payments":      true,
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
		host := strings.ToLower(u.Hostname())
		if host == "www.twitch.tv" || host == "twitch.tv" || host == "m.twitch.tv" {
			return parseTwitchURL(u)
		}
		// clips.twitch.tv/{slug} — first path segment only
		if host == "clips.twitch.tv" {
			slug := strings.TrimPrefix(u.Path, "/")
			slug, _, _ = strings.Cut(slug, "/")
			if twitchClipSlugRe.MatchString(slug) {
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
		if twitchVODIDRe.MatchString(parts[1]) {
			return &TwitchTarget{Type: TwitchVOD, Value: parts[1]}
		}
		return nil
	}

	// /channel/clip/slug
	if len(parts) >= 3 && parts[1] == "clip" {
		if twitchClipSlugRe.MatchString(parts[2]) {
			return &TwitchTarget{Type: TwitchClip, Value: parts[2]}
		}
		return nil
	}

	// /channel/video/123456
	if len(parts) >= 3 && parts[1] == "video" {
		if twitchVODIDRe.MatchString(parts[2]) {
			return &TwitchTarget{Type: TwitchVOD, Value: parts[2]}
		}
		return nil
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
