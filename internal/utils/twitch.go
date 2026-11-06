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
	// The listing kinds name a page of many videos, not one: a channel's
	// video list (/<ch>/videos, /<ch>/profile — yt-dlp TwitchVideosIE), its
	// clip list (/<ch>/clips, /<ch>/videos?filter=clips — TwitchVideosClipsIE)
	// and a collection (/collections/<id> — TwitchCollectionIE). Value is the
	// login, or the collection ID. They parse so that none is taken for a
	// channel (which silently made a live-wait job). A video list and a
	// collection queue one VOD job per video (owner decision D18), and a clip
	// is a job of its own (owner decision D26); a clip list is refused
	// (Unsupported).
	TwitchVideosListing TwitchTargetType = "videos"
	TwitchClipsListing  TwitchTargetType = "clips"
	TwitchCollection    TwitchTargetType = "collection"
)

// Unsupported returns the refusal every add path shows for a target it cannot
// make a job from yet, or "" for a channel, a VOD, a clip, a channel's video
// list or a collection. The parse is kept here, in the one table every parser
// follows, so a later clip-list job only changes what this returns — as the
// clip job (owner decision D26) and the listing jobs (D18) did.
func (t *TwitchTarget) Unsupported() string {
	if t == nil {
		return ""
	}
	switch t.Type {
	case TwitchClipsListing:
		return "Twitch clip lists are not supported yet. Use a channel, VOD, clip, video list or collection URL."
	}
	return ""
}

// TwitchVideosQuery reads a /<ch>/videos URL's ?filter= and ?sort= (yt-dlp's
// TwitchVideosIE, twitch.py b77c9304e: filter defaults to "all", sort to
// "time"). Both are "" for input that is not a URL, or names neither.
// ?filter=collections makes the video list the channel's collection list
// (TwitchVideosCollectionsIE), which still parses as TwitchVideosListing: the
// add paths read the filter and queue every collection's VODs (owner decision
// FE-27).
func TwitchVideosQuery(input string) (filter, sort string) {
	// A scheme-less paste ("twitch.tv/<ch>/videos?filter=…") parses too:
	// the query is the query whatever the rest reads as.
	u, err := url.Parse(strings.TrimSpace(input))
	if err != nil {
		return "", ""
	}
	q := u.Query()
	return q.Get("filter"), q.Get("sort")
}

// ValidTwitchClipSlug reports whether slug can name a clip — the shape the
// URL table accepts from a clip URL, which the add route also holds a raw
// slug to (it becomes the job ID and the staging directory's name).
func ValidTwitchClipSlug(slug string) bool {
	return len(slug) <= 100 && twitchClipSlugRe.MatchString(slug)
}

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
	// twitchCollectionIDRe validates a collection ID ("o9zZer3IQBhTJw").
	twitchCollectionIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
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
		// /collections/<id> and /clip/<slug> are handled before this
		// lookup; bare, they name no channel either.
		"collections": true,
		"clip":        true,
	}

	// twitchSiteHosts are the hosts yt-dlp's Twitch extractors accept for the
	// site itself (_VALID_URL_BASE, twitch.py 35: www, go and m), plus the
	// bare domain.
	twitchSiteHosts = map[string]bool{
		"twitch.tv":     true,
		"www.twitch.tv": true,
		"go.twitch.tv":  true,
		"m.twitch.tv":   true,
	}
)

// ExtractTwitchTarget extracts a Twitch target from a URL or identifier.
//
// The URL forms are yt-dlp's Twitch extractors' (twitch.py b77c9304e):
//
//   - site hosts www., go., m. and the bare domain (TwitchBaseIE
//     _VALID_URL_BASE);
//   - a VOD as /videos/<id>, /<ch>/v/<id>, /<ch>/video/<id> and
//     /<ch>/schedule?vodID=<id> (TwitchVodIE, 238-246), and
//     player.twitch.tv/?video=v<id>;
//   - a channel as /<ch> (TwitchStreamIE, 954-961) and
//     player.twitch.tv/?channel=<ch>;
//   - a clip as /clip/<slug> with or without a channel before it, and
//     clips.twitch.tv/<slug>, clips.twitch.tv/<ch>/<slug> (the LAST segment
//     is the slug) and clips.twitch.tv/embed?clip=<slug> (TwitchClipsIE,
//     1131-1138);
//   - the listings (see TwitchVideosListing).
//
// The dashboard (web/public/app.js extractTwitchTarget) mirrors this and the
// TUI calls it; internal/utils/testdata/twitch_urls.json is the one table
// all three are tested against.
func ExtractTwitchTarget(input string) *TwitchTarget {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}

	// Normalize a twitch URL pasted without its scheme. The host is
	// case-insensitive, here as in the parse below: "Twitch.tv/shroud".
	if hasTwitchHostPrefix(input) {
		input = "https://" + input
	}

	// Try parsing as URL
	u, err := url.Parse(input)
	if err == nil {
		switch host := strings.ToLower(u.Hostname()); {
		case twitchSiteHosts[host]:
			return parseTwitchURL(u)
		case host == "player.twitch.tv":
			return parseTwitchPlayerURL(u)
		case host == "clips.twitch.tv":
			return parseTwitchClipsURL(u)
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

// hasTwitchHostPrefix reports whether input starts with a Twitch host and a
// path separator, scheme omitted.
func hasTwitchHostPrefix(input string) bool {
	lower := strings.ToLower(input)
	for _, host := range []string{"twitch.tv", "www.twitch.tv", "go.twitch.tv", "m.twitch.tv", "player.twitch.tv", "clips.twitch.tv"} {
		if strings.HasPrefix(lower, host+"/") {
			return true
		}
	}
	return false
}

// parseTwitchPlayerURL reads player.twitch.tv's embed query: ?video=v<id> (or
// a bare <id>) is a VOD, ?channel=<login> a channel. video wins when both are
// present, as TwitchStreamIE.suitable defers to TwitchVodIE.
func parseTwitchPlayerURL(u *url.URL) *TwitchTarget {
	q := u.Query()
	if v := q.Get("video"); v != "" {
		if id := strings.TrimPrefix(v, "v"); twitchVODIDRe.MatchString(id) {
			return &TwitchTarget{Type: TwitchVOD, Value: id}
		}
		return nil
	}
	if login := strings.ToLower(q.Get("channel")); twitchLoginRegex.MatchString(login) {
		return &TwitchTarget{Type: TwitchChannel, Value: login}
	}
	return nil
}

// parseTwitchClipsURL reads clips.twitch.tv: embed?clip=<slug>, or the last
// path segment, so the legacy /<ch>/<slug> yields the slug and not the
// channel (yt-dlp's `clips\.twitch\.tv/(?:[^/]+/)*(?P<id>…)`). A trailing
// /edit — the clip editor's page, /<slug>/edit — is dropped first, where
// yt-dlp would take "edit" for the slug.
func parseTwitchClipsURL(u *url.URL) *TwitchTarget {
	parts := splitTwitchPath(u.Path)
	if n := len(parts); n >= 2 && parts[n-1] == "edit" {
		parts = parts[:n-1]
	}
	slug := ""
	if len(parts) == 1 && parts[0] == "embed" {
		slug = u.Query().Get("clip")
	} else if len(parts) > 0 {
		slug = parts[len(parts)-1]
	}
	if twitchClipSlugRe.MatchString(slug) {
		return &TwitchTarget{Type: TwitchClip, Value: slug}
	}
	return nil
}

// splitTwitchPath splits a URL path into its non-empty segments.
func splitTwitchPath(p string) []string {
	var parts []string
	for seg := range strings.SplitSeq(p, "/") {
		if seg != "" {
			parts = append(parts, seg)
		}
	}
	return parts
}

func parseTwitchURL(u *url.URL) *TwitchTarget {
	parts := splitTwitchPath(u.Path)
	if len(parts) == 0 {
		return nil
	}
	vodID := func(id string) *TwitchTarget {
		if twitchVODIDRe.MatchString(id) {
			return &TwitchTarget{Type: TwitchVOD, Value: id}
		}
		return nil
	}
	clip := func(slug string) *TwitchTarget {
		if twitchClipSlugRe.MatchString(slug) {
			return &TwitchTarget{Type: TwitchClip, Value: slug}
		}
		return nil
	}

	switch parts[0] {
	case "videos": // /videos/<id>
		if len(parts) >= 2 {
			return vodID(parts[1])
		}
		return nil
	case "clip": // /clip/<slug>, no channel
		if len(parts) >= 2 {
			return clip(parts[1])
		}
		return nil
	case "collections": // /collections/<id>
		if len(parts) >= 2 && twitchCollectionIDRe.MatchString(parts[1]) {
			return &TwitchTarget{Type: TwitchCollection, Value: parts[1]}
		}
		return nil
	}

	login := strings.ToLower(parts[0])
	if twitchReservedPaths[login] {
		return nil
	}
	if len(parts) >= 2 {
		switch parts[1] {
		case "v", "video": // /<ch>/v/<id>, /<ch>/video/<id>
			if len(parts) >= 3 {
				return vodID(parts[2])
			}
			return nil
		case "clip": // /<ch>/clip/<slug>
			if len(parts) >= 3 {
				return clip(parts[2])
			}
			return nil
		case "schedule": // /<ch>/schedule?vodID=<id>; without one, the channel's page
			if id := u.Query().Get("vodID"); id != "" {
				return vodID(id)
			}
		}
	}
	if !twitchLoginRegex.MatchString(login) {
		return nil
	}
	if len(parts) >= 2 {
		switch parts[1] {
		case "videos", "profile":
			// yt-dlp's ?filter=clips makes the video list a clip list
			// (TwitchVideosClipsIE).
			if parts[1] == "videos" && u.Query().Get("filter") == "clips" {
				return &TwitchTarget{Type: TwitchClipsListing, Value: login}
			}
			return &TwitchTarget{Type: TwitchVideosListing, Value: login}
		case "clips":
			return &TwitchTarget{Type: TwitchClipsListing, Value: login}
		}
	}
	return &TwitchTarget{Type: TwitchChannel, Value: login}
}
