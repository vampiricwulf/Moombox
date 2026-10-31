// Package constants centralizes all hardcoded values used throughout Moombox.
package constants

import (
	"fmt"
	"math/rand/v2"
)

// ProjectRepoURL is the Moombox GitHub repository page — opened by the TUI's
// O G chord; the Web UI's version-indicator click mirrors it as
// GITHUB_REPO_URL in web/public/modules/update-indicator.js (keep the two in
// sync).
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
	TVSamsung string
	VisionOS  string
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
	// A 2016 Samsung Tizen set, per Samsung's own platform-information guide;
	// see TVSamsungClient.
	TVSamsung: "Mozilla/5.0 (SMART-TV; Linux; Tizen 2.4.0) AppleWebKit/538.1 (KHTML, like Gecko) Version/2.4.0 TV Safari/538.1",
	VisionOS:  "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15",
}

// =============================================================================
// YOUTUBE URLS
// =============================================================================

// YouTubeURLs contains base URLs for YouTube services.
var YouTubeURLs = struct {
	Base  string
	API   string
	Watch string
	Embed string
}{
	Base:  "https://www.youtube.com",
	API:   "https://www.youtube.com/youtubei/v1",
	Watch: "https://www.youtube.com/watch",
	Embed: "https://www.youtube.com/embed",
}

// DefaultAPIKey is the default YouTube API key.
const DefaultAPIKey = "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8"

// =============================================================================
// YOUTUBE CLIENT CONFIGURATIONS
// =============================================================================
//
// Where a client below stands in yt-dlp's default requested-client tuples
// (yt-dlp master, _video.py, since #17805 "Set tv_samsung as a default
// client", 2026-10-10):
//
//	_DEFAULT_CLIENTS         = (tv_samsung, visionos, web)
//	_DEFAULT_JSLESS_CLIENTS  = (visionos,)
//	_DEFAULT_AUTHED_CLIENTS  = (web_embedded, tv_samsung, web)
//	_DEFAULT_PREMIUM_CLIENTS = (web_creator, tv_samsung, web)
//
// yt-dlp asks the premium tuple for a Premium subscriber, else the authed one
// for a signed-in session, else the jsless one when no JS runtime is
// available, else the default. How Moombox's format ranking follows them is
// the AuthLevel block in internal/youtube (types.go).
//
// Every Context states hl=en, timeZone=UTC and utcOffsetMinutes=0, the three
// fields yt-dlp's _extract_context forces into every Innertube context
// (_base.py:753-759): English reasons for the keyword classification, and
// times stated in UTC, the zone every stored time is in. The YouTube pages
// get the same language and zone from their PREF cookie
// (internal/cookies/page_cookies.go).

// YouTubeClientConfig holds configuration for a YouTube API client.
type YouTubeClientConfig struct {
	ClientName    string
	ClientVersion string
	ClientID      string
	UserAgent     string
	Context       map[string]any
}

// TVSamsungClient is the primary authenticated client: TVHTML5 identifying
// itself as a 2016 Samsung Tizen set (yt-dlp's tv_samsung, #17723; a default
// client since #17805, leading _DEFAULT_CLIENTS and second in the authed and
// premium tuples). The bare TVHTML5 identity
// (TVDowngradedClient) is answered "The page needs to be reloaded" for a
// signed-in session — YouTube now forces that identity onto a player JS
// variant the player call cannot satisfy — with no formats, on every
// video: verified 2026-10-14 against a public, a members-only and an
// age-gated VOD with the operator's cookies (TestLiveTVSamsung, since
// removed), where this identity answered ok with 30–42 formats to each.
// Upstream calls it a workaround until the JS variant is handled; the
// device fields are what YouTube keys on, and they are sent in the
// Innertube context as well as the User-Agent.
var TVSamsungClient = YouTubeClientConfig{
	ClientName:    "TVHTML5",
	ClientVersion: "5.20260707",
	ClientID:      "7",
	UserAgent:     UserAgents.TVSamsung,
	Context: map[string]any{
		"clientName":    "TVHTML5",
		"clientVersion": "5.20260707",
		"deviceMake":    "Samsung",
		"deviceModel":   "UKS9800",
		"osName":        "Tizen",
		"osVersion":     "2.4.0",
		"hl":            "en",

		"timeZone":         "UTC",
		"utcOffsetMinutes": 0,
	},
}

// TVDowngradedClient is TVHTML5 under the bare Cobalt identity — yt-dlp's
// tv_downgraded, the primary authenticated client until TVSamsungClient
// replaced it. Kept as the definition upstream keeps; nothing calls it.
var TVDowngradedClient = YouTubeClientConfig{
	ClientName:    "TVHTML5",
	ClientVersion: "5.20260707",
	ClientID:      "7",
	UserAgent:     UserAgents.TV,
	Context: map[string]any{
		"clientName":    "TVHTML5",
		"clientVersion": "5.20260707",
		"hl":            "en",

		"timeZone":         "UTC",
		"utcOffsetMinutes": 0,
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

		"timeZone":         "UTC",
		"utcOffsetMinutes": 0,
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

		"timeZone":         "UTC",
		"utcOffsetMinutes": 0,
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

		"timeZone":         "UTC",
		"utcOffsetMinutes": 0,
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

		"timeZone":         "UTC",
		"utcOffsetMinutes": 0,
	},
}

// VisionOSClient is in yt-dlp's cookieless defaults since 2026.08.19
// (dae52d838 removed android_vr from every default list): second in
// _DEFAULT_CLIENTS, behind tv_samsung, and the whole of
// _DEFAULT_JSLESS_CLIENTS, the tuple for an install with no JS runtime.
// Cookieless, no JS player required, no PO-token policy. Serves direct-URL
// adaptive formats for VODs and HLS for live — verified 2026-08-24 that it
// does NOT return dashManifestUrl for live streams, so it cannot replace
// ANDROID_VR as the live-DASH enrichment source. Caveat per upstream: "Made
// for kids" videos aren't available.
var VisionOSClient = YouTubeClientConfig{
	ClientName:    "VISIONOS",
	ClientVersion: "1.02",
	ClientID:      "101",
	UserAgent:     UserAgents.VisionOS,
	Context: map[string]any{
		"clientName":    "VISIONOS",
		"clientVersion": "1.02",
		"deviceMake":    "Apple",
		"deviceModel":   "RealityDevice17,1",
		"osName":        "visionOS",
		"osVersion":     "26.5.23O471",
		// hl is not in yt-dlp's visionos entry because build_innertube_clients
		// applies `setdefault('hl', 'en')` to EVERY client. It is not optional
		// here: classifyStream matches English substrings in playability
		// reasons ("member", "age", "private", "live event will begin", …), so
		// a localized response misclassifies the stream.
		"hl": "en",

		"timeZone":         "UTC",
		"utcOffsetMinutes": 0,
	},
}

// AndroidVRClient is for status probes and as a live-DASH / VOD-format
// fallback without cookies.
// clientVersion is deliberately held at 1.65.10 — versions >1.65 may return
// SABR-only streams (yt-dlp keeps the same pin). yt-dlp reports escalating
// enforcement: intermittent/selective PO-token enforcement on non-HLS formats
// since 2026-07 (69ea20006), and "ALL formats (including live HLS and itag
// 18) are 403'd" since 2026-08-17 (dae52d838 removed it from every default
// client list). Verified 2026-08-24 from this vantage that the player API,
// live DASH manifests+segments, and VOD format URLs all still work — the
// enforcement is selective, not universal. The probe keeps using it (the
// player response itself is unaffected), VISIONOS now leads the VOD-format
// fallbacks, and this client remains the only cookieless source of live
// dashManifestUrl (VISIONOS is HLS-only for live), so the DASH enrichment
// fallback stays here until enforcement or a replacement forces a change.
var AndroidVRClient = YouTubeClientConfig{
	ClientName:    "ANDROID_VR",
	ClientVersion: "1.65.10",
	ClientID:      "28",
	UserAgent:     UserAgents.AndroidVR,
	Context: map[string]any{
		"clientName":        "ANDROID_VR",
		"clientVersion":     "1.65.10",
		"androidSdkVersion": 32,
		"osName":            "Android",
		"osVersion":         "12L",
		"deviceMake":        "Oculus",
		"deviceModel":       "Quest 3",
		// See VisionOSClient for why hl is mandatory rather than cosmetic.
		// It matters most on THIS client: ANDROID_VR serves ProbeVideoStatus,
		// whose playability reason is what classifyStream reads, so without
		// it a non-English locale misclassifies every probed stream.
		"hl": "en",

		"timeZone":         "UTC",
		"utcOffsetMinutes": 0,
	},
}

// =============================================================================
// TWITCH CONSTANTS
// =============================================================================

// TwitchURLs contains base URLs for Twitch services.
var TwitchURLs = struct {
	GQL           string
	UsherLive     string
	UsherVOD      string
	PreviewCDN    string
	IRCWS         string
	OAuthValidate string
}{
	GQL:           "https://gql.twitch.tv/gql",
	UsherLive:     "https://usher.ttvnw.net/api/channel/hls",
	UsherVOD:      "https://usher.ttvnw.net/vod",
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
	// values.
	DownloadChunkSize = 5 * 1024 * 1024
)
