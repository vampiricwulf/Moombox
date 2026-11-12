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

// ExtractVideoID extracts a YouTube video ID from a bare 11-character ID
// (see videoIDRegex godoc for the Twitch v-prefixed VOD collision) or from
// a YouTube URL as ExtractVideoIDFromURL reads it. It is the one parser
// every add path uses: `moombox add` (ExtractMediaID), the TUI's add dialog
// and, through ExtractVideoIDFromURL, the URL-only POST /api/jobs. The
// dashboard's extractVideoId (web/public/app.js) mirrors it, and all of them
// are tested against testdata/youtube_urls.json.
func ExtractVideoID(input string) string {
	input = strings.TrimSpace(input)
	if videoIDRegex.MatchString(input) {
		return input
	}
	return ExtractVideoIDFromURL(input)
}

// urlSchemeRegex matches a leading URL scheme ("https://"); httpSchemeRegex
// the two this parser reads. The letters are spelled out in both cases
// because (?i) also folds non-ASCII letters (the long s) onto ASCII ones.
var (
	urlSchemeRegex  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	httpSchemeRegex = regexp.MustCompile(`^[Hh][Tt][Tt][Pp][Ss]?://`)
)

// embeddedLinkRegex finds a link inside other text: an optional http(s)
// scheme or "//", a run of host characters, a slash, and everything up to
// the next whitespace. Group 1 is the scheme and host. Which host it is, is
// videoIDFromLink's to judge.
var embeddedLinkRegex = regexp.MustCompile(`((?:[Hh][Tt][Tt][Pp][Ss]?://|//)?[A-Za-z0-9.-]+)/[^ \t\n\v\f\r]*`)

// youtubeVideoPathPrefixes are the path forms that carry the video ID as
// their next segment.
var youtubeVideoPathPrefixes = []string{"/live/", "/shorts/", "/embed/", "/v/"}

// ExtractVideoIDFromURL extracts the video ID from a YouTube URL, and returns
// "" for anything else, a bare ID included. The scheme (http or https) is
// optional, a leading "//" is read as no scheme, and the host is read in any
// case. Supported forms:
//   - a v= parameter anywhere in the query (or in the fragment) on any path:
//     /watch?v=ID, /watch?app=desktop&v=ID, /watch?list=PL...&v=ID
//   - youtu.be/ID
//   - /live/ID, /shorts/ID, /embed/ID, /v/ID
//
// on youtube.com, youtube-nocookie.com and youtubekids.com and their
// subdomains (www., m., music., gaming.), and on youtu.be. The host is
// matched on a dot boundary, so a lookalike ("notyoutube.com") is refused.
// An ID may be followed by anything that is not an ID character (a slash,
// a semicolon, a stray percent sign or closing bracket from a paste); twelve
// or more ID characters are not an ID.
//
// When the input as a whole is not such a URL, the first YouTube link inside
// it is read the same way: "<https://youtu.be/ID>", "see https://youtu.be/ID",
// a Markdown link, a second line. A link written without a scheme counts
// only at the start of the text or after a character that cannot continue a
// host or a path, so "example.com/youtube.com/watch?v=ID" stays refused.
//
// The link is taken apart by hand, not by net/url, so that this parser and
// its mirror in web/public/app.js give the same answer for every input:
// net/url and the browser's URL disagree on a stray "%", a ";" in the query,
// dot segments and escapes in the path. Nothing is unescaped: an ID holds
// nothing an escape could stand for.
func ExtractVideoIDFromURL(input string) string {
	input = strings.TrimSpace(input)
	if id := videoIDFromLink(input); id != "" {
		return id
	}
	for off := 0; off < len(input); {
		m := embeddedLinkRegex.FindStringSubmatchIndex(input[off:])
		if m == nil {
			break
		}
		start, end, headEnd := off+m[0], off+m[1], off+m[3]
		link := input[start:end]
		if httpSchemeRegex.MatchString(link) || !continuesLink(input, start) {
			if id := videoIDFromLink(link); id != "" {
				return id
			}
		}
		// Look again after this link's host: a link inside this one's
		// path or query ("?u=https://youtu.be/ID") is still found.
		off = headEnd + 1
	}
	return ""
}

// continuesLink reports whether the character before position i of s could
// be part of a host, a path or user info that the text at i then continues,
// which makes a scheme-less link starting at i part of something else.
func continuesLink(s string, i int) bool {
	if i == 0 {
		return false
	}
	c := s[i-1]
	if c == ':' && strings.HasPrefix(s[i:], "//") {
		return true // another scheme's "://"
	}
	return c >= 0x80 || isVideoIDByte(c) || c == '.' || c == '/' || c == '\\' || c == '@'
}

// videoIDFromLink reads the video ID from s when all of s is one YouTube
// link (see ExtractVideoIDFromURL for the forms).
func videoIDFromLink(s string) string {
	switch {
	case httpSchemeRegex.MatchString(s):
		s = s[strings.Index(s, "://")+3:]
	case urlSchemeRegex.MatchString(s):
		return ""
	case strings.HasPrefix(s, "//"):
		s = s[2:]
	}

	// host[:port], after any user info, up to the path, query or fragment.
	authority, rest := s, ""
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		authority, rest = s[:i], s[i:]
	}
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		authority = authority[i+1:]
	}
	host := authority
	if i := strings.LastIndexByte(authority, ':'); i >= 0 {
		host = authority[:i]
		for _, c := range []byte(authority[i+1:]) {
			if c < '0' || c > '9' {
				return ""
			}
		}
	}
	for _, c := range []byte(host) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return ""
		}
	}
	host = strings.ToLower(host)
	short := hostIsOrUnder(host, "youtu.be")
	if !short && !hostIsOrUnder(host, "youtube.com") &&
		!hostIsOrUnder(host, "youtube-nocookie.com") && !hostIsOrUnder(host, "youtubekids.com") {
		return ""
	}

	rest, fragment, _ := strings.Cut(rest, "#")
	path, query, _ := strings.Cut(rest, "?")
	if id := videoIDParam(query); id != "" {
		return id
	}
	// The old "#!v=ID" and "#v=ID" forms.
	if id := videoIDParam(strings.TrimPrefix(fragment, "!")); id != "" {
		return id
	}
	if short {
		return leadingVideoID(strings.TrimPrefix(path, "/"))
	}
	for _, prefix := range youtubeVideoPathPrefixes {
		if after, ok := strings.CutPrefix(path, prefix); ok {
			return leadingVideoID(after)
		}
	}
	return ""
}

// videoIDParam returns the video ID the first v= pair of a raw query string
// starts with. Pairs are split on "&" only and the value is read as written,
// so "v=ID;t=3" and "v=ID%" still name ID.
func videoIDParam(query string) string {
	for pair := range strings.SplitSeq(query, "&") {
		if value, ok := strings.CutPrefix(pair, "v="); ok {
			return leadingVideoID(value)
		}
	}
	return ""
}

// hostIsOrUnder reports whether host is domain or a subdomain of it.
func hostIsOrUnder(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// isVideoIDByte reports whether c can be part of a video ID.
func isVideoIDByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// leadingVideoID returns the video ID s starts with: exactly 11 ID
// characters, followed by nothing or by a character that cannot be part of
// an ID.
func leadingVideoID(s string) string {
	n := 0
	for n < len(s) && isVideoIDByte(s[n]) {
		n++
	}
	if n != 11 {
		return ""
	}
	return s[:n]
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

	// Normalize bare URLs without protocol. The host is case-insensitive,
	// here as in the check below: "YouTube.com/@foo".
	if lower := strings.ToLower(input); strings.HasPrefix(lower, "youtube.com/") || strings.HasPrefix(lower, "www.youtube.com/") ||
		strings.HasPrefix(lower, "m.youtube.com/") {
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

// resolveRetryWait waits out one of ResolveYouTubeChannel's backoffs, or
// returns ctx's error the moment ctx ends. A variable so a test can record
// the delays instead of sleeping through them.
var resolveRetryWait = func(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// YouTubeChannelInfo holds resolved YouTube channel information.
type YouTubeChannelInfo struct {
	ChannelID string
	Name      string
}

// ResolveYouTubeChannel fetches a YouTube channel page and extracts the channel ID and name.
// urlPath should be like "/@Handle", "/c/Name", or "/user/Name".
//
// Makes up to 3 attempts — two retries, after 1 s and then 2 s — on
// transient failures (network errors, HTTP 5xx surfaces as "HTTP 5xx" in
// the FetchBody error string). HTTP 4xx errors and ctx cancellation
// fast-fail. Audit reports/small-packages.md.
//
// The page goes out with the consent and language cookies every YouTube page
// carries (constants.SignedOutYouTubePageCookie: SOCS=CAI and an English
// PREF), as yt-dlp's tab extractor reads the same page (_extract_webpage →
// _download_webpage, yt_dlp/extractor/youtube/_tab.py:955-959) through a jar
// _real_initialize gave both (_base.py:675-704). Without SOCS an EU, EEA or
// UK address is redirected to consent.youtube.com, whose page names no
// channel, so adding a channel by @handle or custom URL failed "could not
// find channel ID on page". The read is anonymous by design and sits below
// internal/cookies in the import graph, so it carries the signed-out pair,
// not the jar's.
func ResolveYouTubeChannel(ctx context.Context, urlPath string) (*YouTubeChannelInfo, error) {
	pageURL := youtubeBaseURL + urlPath
	headers := map[string]string{
		"User-Agent": constants.UserAgents.Web,
		"Cookie":     constants.SignedOutYouTubePageCookie,
	}

	var body []byte
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			if err := resolveRetryWait(ctx, time.Duration(1<<(attempt-1))*time.Second); err != nil {
				return nil, err
			}
		}
		body, err = FetchBody(ctx, pageURL, 15*time.Second, headers)
		if err == nil {
			break
		}
		// Don't retry 4xx — those are deterministic ("not found", auth
		// missing, etc.) and won't fix themselves with a second try.
		// EXCEPT 429/408: rate-limiting and request-timeout are transient
		// by definition — exactly what this retry loop exists for (adding
		// several channels back-to-back can draw a 429 from YouTube).
		es := err.Error()
		if strings.Contains(es, "HTTP 4") &&
			!strings.Contains(es, "HTTP 429") && !strings.Contains(es, "HTTP 408") {
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
