// Package constants centralizes all hardcoded values used throughout Moombox.
package constants

import (
	"fmt"
	"math/rand/v2"
)

// ProjectRepoURL is the Moombox GitHub repository page — opened by the TUI's
// O G chord; the Web UI's version-indicator click mirrors it as a hardcoded
// string in web/public/app.js (keep the two in sync).
const ProjectRepoURL = "https://github.com/vampiricwulf/Moombox"

// =============================================================================
// HTTP CONSTANTS
// =============================================================================

// Chrome-major window for the randomized desktop Web UA. Keep in lockstep
// with yt-dlp's random_user_agent() range (utils/_utils.py — 145-151 as of
// 2026-07) so live Google/Twitch endpoints never see an implausibly stale or
// not-yet-released browser. Bump both bounds when yt-dlp's window moves.
const (
	chromeMajorMin = 145
	chromeMajorMax = 151
)

// randomizedWebUA builds the desktop Web UA with a Chrome major picked
// randomly ONCE per process start (this runs during package init). Per-start
// randomization spreads Moombox installs across the same version window real
// Chrome users occupy — a single fixed pin makes every install fingerprint
// identically, so one flagged UA+behavior tuple degrades everyone at once
// (yt-dlp randomizes for the same reason). The value must stay fixed for the
// process lifetime: BotGuard's DOM shim reports it as navigator.userAgent
// while the HTTP layer sends it on every request, and those two views must
// never disagree.
func randomizedWebUA() string {
	major := chromeMajorMin + rand.IntN(chromeMajorMax-chromeMajorMin+1)
	return fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", major)
}

// UserAgents contains User-Agent strings for different platforms.
var UserAgents = struct {
	Web       string
	WebSafari string
	Android   string
	AndroidVR string
	TV        string
	IOS       string
}{
	// Randomized per process start (see randomizedWebUA). This is the single
	// source of truth for the desktop Web UA — every other site references
	// it. The app UAs below stay PINNED: they embed app versions that must
	// exactly match the Innertube clientVersion each client config claims,
	// so randomizing them independently would create a contradictory
	// fingerprint rather than blend into one.
	Web:       randomizedWebUA(),
	WebSafari: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15",
	Android:   "com.google.android.youtube/21.26.364 (Linux; U; Android 11) gzip",
	AndroidVR: "com.google.android.apps.youtube.vr.oculus/1.65.10 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip",
	TV:        "Mozilla/5.0 (ChromiumStylePlatform) Cobalt/Version",
	IOS:       "com.google.ios.youtube/21.26.4 (iPhone16,2; U; CPU iOS 18_3_2 like Mac OS X;)",
}

// =============================================================================
// YOUTUBE URLS
// =============================================================================

// YouTubeURLs contains base URLs for YouTube services.
var YouTubeURLs = struct {
	Base      string
	API       string
	Watch     string
	Embed     string
	Feed      string
	Thumbnail string
}{
	Base:      "https://www.youtube.com",
	API:       "https://www.youtube.com/youtubei/v1",
	Watch:     "https://www.youtube.com/watch",
	Embed:     "https://www.youtube.com/embed",
	Feed:      "https://www.youtube.com/feeds/videos.xml",
	Thumbnail: "https://i.ytimg.com/vi",
}

// DefaultAPIKey is the default YouTube API key.
const DefaultAPIKey = "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8"

// =============================================================================
// YOUTUBE CLIENT CONFIGURATIONS
// =============================================================================

// YouTubeClientConfig holds configuration for a YouTube API client.
type YouTubeClientConfig struct {
	ClientName    string
	ClientVersion string
	ClientID      string
	UserAgent     string
	Context       map[string]any
}

// TVDowngradedClient is the primary authenticated client (TVHTML5).
var TVDowngradedClient = YouTubeClientConfig{
	ClientName:    "TVHTML5",
	ClientVersion: "5.20260707",
	ClientID:      "7",
	UserAgent:     UserAgents.TV,
	Context: map[string]any{
		"clientName":    "TVHTML5",
		"clientVersion": "5.20260707",
		"hl":            "en",
	},
}

// WebCreatorClient is a fallback for member content.
var WebCreatorClient = YouTubeClientConfig{
	ClientName:    "WEB_CREATOR",
	ClientVersion: "1.20260708.06.00",
	ClientID:      "62",
	UserAgent:     UserAgents.Web,
	Context: map[string]any{
		"clientName":    "WEB_CREATOR",
		"clientVersion": "1.20260708.06.00",
		"hl":            "en",
	},
}

// WebClient is for watch page fetching.
var WebClient = YouTubeClientConfig{
	ClientName:    "WEB",
	ClientVersion: "2.20260708.00.00",
	ClientID:      "1",
	UserAgent:     UserAgents.Web,
	Context: map[string]any{
		"clientName":    "WEB",
		"clientVersion": "2.20260708.00.00",
		"hl":            "en",
	},
}

// WebSafariClient is the primary WEB client with Safari UA.
// Returns pre-merged HLS formats. Since 2026-07 YouTube only serves those
// HLS formats to some logged-in or "trusted" sessions (yt-dlp demoted
// web_safari from its defaults for this reason, 69ea20006); logged-out
// sessions get the same adaptive formats a plain WEB client would.
var WebSafariClient = YouTubeClientConfig{
	ClientName:    "WEB",
	ClientVersion: "2.20260708.00.00",
	ClientID:      "1",
	UserAgent:     UserAgents.WebSafari,
	Context: map[string]any{
		"clientName":    "WEB",
		"clientVersion": "2.20260708.00.00",
		"hl":            "en",
	},
}

// WebEmbeddedClient is for age-restricted content via embedded player.
// Uses a non-YouTube embedUrl per yt-dlp requirements.
var WebEmbeddedClient = YouTubeClientConfig{
	ClientName:    "WEB_EMBEDDED_PLAYER",
	ClientVersion: "2.20260708.00.00",
	ClientID:      "56",
	UserAgent:     UserAgents.Web,
	Context: map[string]any{
		"clientName":    "WEB_EMBEDDED_PLAYER",
		"clientVersion": "2.20260708.00.00",
		"hl":            "en",
	},
}

// AndroidVRClient is for VOD downloads without cookies.
// clientVersion is deliberately held at 1.65.10 — versions >1.65 may return
// SABR-only streams (yt-dlp keeps the same pin). Since 2026-07 yt-dlp
// reports intermittent/selective PO-token enforcement on android_vr's
// non-HLS formats (69ea20006 added a GVS PO-token policy for it); if VOD
// fetches through this client start failing with 403s, attaching a PO token
// is the fix to reach for.
var AndroidVRClient = YouTubeClientConfig{
	ClientName:    "ANDROID_VR",
	ClientVersion: "1.65.10",
	ClientID:      "28",
	UserAgent:     UserAgents.AndroidVR,
	Context: map[string]any{
		"clientName":        "ANDROID_VR",
		"clientVersion":     "1.65.10",
		"androidSdkVersion": 32,
		"osVersion":         "12L",
		"deviceMake":        "Oculus",
		"deviceModel":       "Quest 3",
	},
}

// IOSClient is the iOS native YouTube app client. Returns HLS streams
// distinct from the WEB DASH manifests, useful as a low-res-live fallback
// when WEB_SAFARI / WEB_CREATOR / ANDROID_VR all fail or return inadequate
// formats. Audit reports/youtube.md T2.
var IOSClient = YouTubeClientConfig{
	ClientName:    "IOS",
	ClientVersion: "21.26.4",
	ClientID:      "5",
	UserAgent:     UserAgents.IOS,
	Context: map[string]any{
		"clientName":    "IOS",
		"clientVersion": "21.26.4",
		"deviceMake":    "Apple",
		"deviceModel":   "iPhone16,2",
		"osName":        "iPhone",
		"osVersion":     "18.3.2.22D82",
		"hl":            "en",
	},
}

// WebRemixClient is the YouTube Music / mobile-web client. Useful as a
// low-priority fallback for VOD lookups when WEB / WEB_SAFARI both fail —
// returns its own subset of formats and rarely shares the same playability
// rejection reasons. Audit reports/youtube.md T2.
var WebRemixClient = YouTubeClientConfig{
	ClientName:    "WEB_REMIX",
	ClientVersion: "1.20260707.12.00",
	ClientID:      "67",
	UserAgent:     UserAgents.WebSafari,
	Context: map[string]any{
		"clientName":    "WEB_REMIX",
		"clientVersion": "1.20260707.12.00",
		"hl":            "en",
	},
}

// =============================================================================
// TWITCH CONSTANTS
// =============================================================================

// TwitchURLs contains base URLs for Twitch services.
var TwitchURLs = struct {
	Base          string
	GQL           string
	UsherLive     string
	UsherVOD      string
	EmoteCDN      string
	PreviewCDN    string
	IRCWS         string
	OAuthValidate string
}{
	Base:          "https://www.twitch.tv",
	GQL:           "https://gql.twitch.tv/gql",
	UsherLive:     "https://usher.ttvnw.net/api/channel/hls",
	UsherVOD:      "https://usher.ttvnw.net/vod",
	EmoteCDN:      "https://static-cdn.jtvnw.net/emoticons/v2",
	PreviewCDN:    "https://static-cdn.jtvnw.net/previews-ttv",
	IRCWS:         "wss://irc-ws.chat.twitch.tv:443",
	OAuthValidate: "https://id.twitch.tv/oauth2/validate",
}

// TwitchGQLClientID is the public GQL Client-ID.
//
// This is the *classic* Twitch web Client-ID. Upstream yt-dlp now defaults
// to "ue6666qo983tsx6so1t0vnawi233wa" (see references/yt-dlp twitch.py
// _CLIENT_ID), driven by integrity-token enforcement on the old ID. The
// classic ID still works for our persisted queries and Usher access tokens
// today, but if Twitch tightens GQL access this is the first thing to
// rotate. Audit-finding #9.
const TwitchGQLClientID = "kimne78kx3ncx6brgo4mv6wki5h1ko"

// TwitchGQLHashes contains persisted query hashes for Twitch GQL.
//
// Last verified against references/yt-dlp twitch.py _OPERATION_HASHES on
// 2026-04-23. When syncing yt-dlp, diff the four hashes below against
// upstream and bump as needed — Twitch invalidates persisted queries on
// schema changes, so a stale hash returns a "PersistedQueryNotFound"
// error and breaks the relevant code path silently. Audit-finding #33.
var TwitchGQLHashes = struct {
	StreamMetadata                string
	ComscoreStreamingQuery        string
	VideoMetadata                 string
	VideoCommentsByOffsetOrCursor string
}{
	StreamMetadata:                "ad022ca32220d5523d03a23cbcb5beaa1e0999889c1f8f78f9f2520dafb5cae6",
	ComscoreStreamingQuery:        "e1edae8122517d013405f237ffcc124515dc6ded82480a88daef69c83b53ac01",
	VideoMetadata:                 "45111672eea2e507f8ba44d101a61862f9c56b11dee09a15634cb75cb9b9084d",
	VideoCommentsByOffsetOrCursor: "b70a3591ff0f4e0313d126c6a1502d79a1c02baebb288227c582044aa76adf6a",
}

// TwitchEmoteAPIs contains third-party emote API endpoints.
//
// Only the per-channel endpoints are referenced today; global-emote
// endpoints were removed because they had no callers (audit-finding #44).
// Re-add BTTVGlobal/FFZGlobal/SevenTVGlobal if global emote merging is
// ever wired into emotes.go.
var TwitchEmoteAPIs = struct {
	BTTVChannel string
	FFZChannel  string
	SevenTVUser string
}{
	BTTVChannel: "https://api.betterttv.net/3/cached/users/twitch",
	FFZChannel:  "https://api.frankerfacez.com/v1/room/id",
	SevenTVUser: "https://7tv.io/v3/users/twitch",
}

// =============================================================================
// DOWNLOAD CONSTANTS
// =============================================================================

const (
	// DownloadChunkSize is 5 MB — VOD Range-request chunk size used by
	// internal/engine/downloader_direct.go. Only shared constant that is still
	// actively consumed from this package; the rest of the earlier catalog was
	// aspirational and has been removed in favour of consumers owning their own
	// values (see the #30 cleanup note in DECISIONS.md).
	DownloadChunkSize = 5 * 1024 * 1024
)
