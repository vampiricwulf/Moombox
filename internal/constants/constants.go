// Package constants centralizes all hardcoded values used throughout Moombox.
package constants

// =============================================================================
// HTTP CONSTANTS
// =============================================================================

// UserAgents contains User-Agent strings for different platforms.
var UserAgents = struct {
	Web       string
	WebSafari string
	Android   string
	AndroidVR string
	TV        string
	IOS       string
}{
	Web:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	WebSafari: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15",
	Android:   "com.google.android.youtube/19.09.37 (Linux; U; Android 14; en_US) gzip",
	AndroidVR: "com.google.android.apps.youtube.vr.oculus/1.65.10 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip",
	TV:        "Mozilla/5.0 (ChromiumStylePlatform) Cobalt/Version",
	IOS:       "com.google.ios.youtube/19.29.1 (iPhone16,2; U; CPU iOS 17_5_1 like Mac OS X;)",
}

// =============================================================================
// YOUTUBE URLS
// =============================================================================

// YouTubeURLs contains base URLs for YouTube services.
var YouTubeURLs = struct {
	Base      string
	API       string
	Watch     string
	Feed      string
	Thumbnail string
}{
	Base:      "https://www.youtube.com",
	API:       "https://www.youtube.com/youtubei/v1",
	Watch:     "https://www.youtube.com/watch",
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
	Context       map[string]interface{}
}

// TVDowngradedClient is the primary authenticated client (TVHTML5).
var TVDowngradedClient = YouTubeClientConfig{
	ClientName:    "TVHTML5",
	ClientVersion: "5.20260114",
	ClientID:      "7",
	UserAgent:     UserAgents.TV,
	Context: map[string]interface{}{
		"clientName":    "TVHTML5",
		"clientVersion": "5.20260114",
		"hl":            "en",
	},
}

// WebCreatorClient is a fallback for member content.
var WebCreatorClient = YouTubeClientConfig{
	ClientName:    "WEB_CREATOR",
	ClientVersion: "1.20260120.01.00",
	ClientID:      "62",
	UserAgent:     UserAgents.Web,
	Context: map[string]interface{}{
		"clientName":    "WEB_CREATOR",
		"clientVersion": "1.20260120.01.00",
		"hl":            "en",
	},
}

// WebClient is for watch page fetching.
var WebClient = YouTubeClientConfig{
	ClientName:    "WEB",
	ClientVersion: "2.20260120.01.00",
	ClientID:      "1",
	UserAgent:     UserAgents.Web,
	Context: map[string]interface{}{
		"clientName":    "WEB",
		"clientVersion": "2.20260120.01.00",
		"hl":            "en",
	},
}

// WebSafariClient is the primary WEB client with Safari UA.
// Returns pre-merged HLS formats; more reliable for unauthenticated use.
var WebSafariClient = YouTubeClientConfig{
	ClientName:    "WEB",
	ClientVersion: "2.20260120.01.00",
	ClientID:      "1",
	UserAgent:     UserAgents.WebSafari,
	Context: map[string]interface{}{
		"clientName":    "WEB",
		"clientVersion": "2.20260120.01.00",
		"hl":            "en",
	},
}

// AndroidVRClient is for VOD downloads without cookies.
var AndroidVRClient = YouTubeClientConfig{
	ClientName:    "ANDROID_VR",
	ClientVersion: "1.65.10",
	ClientID:      "28",
	UserAgent:     UserAgents.AndroidVR,
	Context: map[string]interface{}{
		"clientName":       "ANDROID_VR",
		"clientVersion":    "1.65.10",
		"androidSdkVersion": 32,
		"osVersion":        "12L",
		"deviceMake":       "Oculus",
		"deviceModel":      "Quest 3",
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
const TwitchGQLClientID = "kimne78kx3ncx6brgo4mv6wki5h1ko"

// TwitchGQLHashes contains persisted query hashes for Twitch GQL.
var TwitchGQLHashes = struct {
	StreamMetadata               string
	ComscoreStreamingQuery       string
	VideoMetadata                string
	VideoCommentsByOffsetOrCursor string
}{
	StreamMetadata:               "b57f9b910f8cd1a4659d894fe7550ccc81ec9052c01e438b290fd66a040b9b93",
	ComscoreStreamingQuery:       "e1edae8122517d013405f237ffcc124515dc6ded82480a88daef69c83b53ac01",
	VideoMetadata:                "45111672eea2e507f8ba44d101a61862f9c56b11dee09a15634cb75cb9b9084d",
	VideoCommentsByOffsetOrCursor: "b70a3591ff0f4e0313d126c6a1502d79a1c02baebb288227c582044aa76adf6a",
}

// TwitchEmoteAPIs contains third-party emote API endpoints.
var TwitchEmoteAPIs = struct {
	BTTVGlobal    string
	BTTVChannel   string
	FFZGlobal     string
	FFZChannel    string
	SevenTVUser   string
	SevenTVGlobal string
}{
	BTTVGlobal:    "https://api.betterttv.net/3/cached/emotes/global",
	BTTVChannel:   "https://api.betterttv.net/3/cached/users/twitch",
	FFZGlobal:     "https://api.frankerfacez.com/v1/set/global",
	FFZChannel:    "https://api.frankerfacez.com/v1/room/id",
	SevenTVUser:   "https://7tv.io/v3/users/twitch",
	SevenTVGlobal: "https://7tv.io/v3/emote-sets/global",
}

// Twitch IRC constants.
const (
	TwitchIRCDedupIDs           = 5_000
	TwitchIRCMaxConsecutiveErrs = 20
)

// =============================================================================
// DOWNLOAD CONSTANTS
// =============================================================================

const (
	// DownloadChunkSize is 5 MB.
	DownloadChunkSize = 5 * 1024 * 1024

	// DownloadTimeoutMs is the download timeout in milliseconds.
	DownloadTimeoutMs = 30_000

	// ProgressUpdateIntervalMs is the in-memory progress update interval (~60fps).
	ProgressUpdateIntervalMs = 16

	// ProgressPersistIntervalMs is the disk persistence interval for crash recovery.
	ProgressPersistIntervalMs = 1_000
)

// =============================================================================
// WORKER CONSTANTS
// =============================================================================

const (
	// WorkerCheckIntervalMs is the worker queue check interval.
	WorkerCheckIntervalMs = 5_000

	// StreamRecheckIntervalMs is the stream status recheck interval.
	StreamRecheckIntervalMs = 30_000

	// ProbeJitterMaxMs is the maximum random jitter added to probe intervals.
	ProbeJitterMaxMs = 30_000

	// MaxConsecutiveProbeErrors before moving job to Error state.
	MaxConsecutiveProbeErrors = 10

	// StreamSegmentTimeoutMs is the duration with no segments before considering stream ended (10 min).
	StreamSegmentTimeoutMs = 10 * 60_000

	// StreamEndVerifyIntervalMs is how often to verify stream status via API (5 min).
	StreamEndVerifyIntervalMs = 5 * 60_000

	// ChatSurgeThreshold is the number of new messages triggering a status probe.
	ChatSurgeThreshold = 30

	// ChatSurgeWindowMs is the sliding window for surge detection (15 sec).
	ChatSurgeWindowMs = 15_000
)

// =============================================================================
// DECAPI CONSTANTS
// =============================================================================

const (
	// DecapiYouTubeLatestURL is the DECAPI latest video endpoint.
	DecapiYouTubeLatestURL = "https://decapi.me/youtube/latest_video"

	// DecapiMinIntervalMs is the minimum interval between DECAPI cycles (15 sec).
	DecapiMinIntervalMs = 15_000

	// DecapiRequestStaggerMs is the stagger between requests within a cycle (1 sec).
	DecapiRequestStaggerMs = 1_000

	// DecapiDefaultRateLimit is the assumed rate limit until learned from headers.
	DecapiDefaultRateLimit = 60

	// DecapiRequestTimeoutMs is the per-request timeout (15 sec).
	DecapiRequestTimeoutMs = 15_000
)

// =============================================================================
// TWITCH MONITOR CONSTANTS
// =============================================================================

const (
	// TwitchCheckIntervalDefaultMs is the default Twitch GQL check interval (15 sec).
	TwitchCheckIntervalDefaultMs = 15_000

	// TwitchCheckJitterMs is the max random jitter for Twitch checks (±5 sec).
	TwitchCheckJitterMs = 5_000

	// TwitchRequestStaggerMs is the stagger between channel checks (500ms).
	TwitchRequestStaggerMs = 500

	// TwitchCheckMinIntervalMs is the minimum check interval (5 sec).
	TwitchCheckMinIntervalMs = 5_000
)

// =============================================================================
// BOTGUARD CONSTANTS
// =============================================================================

const (
	// BotguardRequestKey is the API key for PO token generation.
	BotguardRequestKey = "O43z0dpjhgX20SCx4KAo"

	// BotguardChallengeURL is the challenge API endpoint.
	BotguardChallengeURL = "https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/Create"

	// BotguardGenerateITURL is the GenerateIT API endpoint.
	BotguardGenerateITURL = "https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/GenerateIT"

	// BotguardYouTubeChallengeURL is the YouTube variant challenge endpoint.
	BotguardYouTubeChallengeURL = "https://www.youtube.com/api/jnn/v1/Create"

	// BotguardYouTubeGenerateITURL is the YouTube variant GenerateIT endpoint.
	BotguardYouTubeGenerateITURL = "https://www.youtube.com/api/jnn/v1/GenerateIT"

	// BotguardAPIKey is the Google API key for BotGuard requests.
	BotguardAPIKey = "AIzaSyDyT5W0Jh49F30Pqqtyfdf7pDLFKLJoAnw"
)

// =============================================================================
// THUMBNAIL QUALITY ORDER
// =============================================================================

// ThumbnailQualities lists YouTube thumbnail qualities in order of preference.
// This is a function (not a var) to prevent accidental mutation of the shared slice.
func ThumbnailQualities() []string {
	return []string{
		"maxresdefault",
		"sddefault",
		"hqdefault",
		"mqdefault",
		"default",
	}
}

// =============================================================================
// LIMITS
// =============================================================================

const (
	// MaxParallelSegments is the max concurrent segment downloads during catch-up.
	MaxParallelSegments = 6

	// MaxSegmentRetries is the max retry attempts for failed segment downloads.
	MaxSegmentRetries = 10

	// SegmentBehindThreshold triggers parallel mode when this many segments behind.
	SegmentBehindThreshold = 10

	// ChatDedupIDs is the max recent chat message IDs for deduplication.
	ChatDedupIDs = 5_000

	// MaxItemsPerPage is the max items per API pagination page.
	MaxItemsPerPage = 100

	// DefaultItemsPerPage is the default items per API pagination page.
	DefaultItemsPerPage = 50

	// HistoryMaxEntries is the max history entries before pruning.
	HistoryMaxEntries = 10_000

	// LogRingBufferSize is the number of recent log lines kept in memory.
	LogRingBufferSize = 200

	// DefaultPort is the default HTTP server port.
	DefaultPort = 774

	// ChatMaxConsecutiveErrors is the max consecutive errors before stopping chat.
	ChatMaxConsecutiveErrors = 20

	// ChatVodMaxConsecutiveErrors is the max consecutive errors for VOD chat replay.
	ChatVodMaxConsecutiveErrors = 5

	// ChatFlushThreshold is the number of messages that triggers a disk flush.
	ChatFlushThreshold = 50_000

	// ChatKeepInMemory is the number of recent messages kept in memory after flush.
	ChatKeepInMemory = 5_000

	// ChatWriteBatchWindow is the write batching window in milliseconds.
	ChatWriteBatchWindowMs = 1_000

	// ChatStaleContinuationRetries is the max retries for stale continuation tokens.
	ChatStaleContinuationRetries = 30

	// SegmentRetryDelayCap is the max delay between segment retries in seconds.
	SegmentRetryDelayCap = 30

	// ResumeStateSaveInterval is how often to save resume state during sequential download.
	ResumeStateSaveIntervalSeq = 50

	// ResumeStateSaveIntervalCatchup is how often to save resume state during catch-up.
	ResumeStateSaveIntervalCatchup = 10

	// ImportMaxFileSize is the max upload size for imports (500MB).
	ImportMaxFileSize = 500 * 1024 * 1024

	// ImportMaxFiles is the max number of files in an import archive.
	ImportMaxFiles = 1000

	// ImportMaxTotalSize is the max total extracted size (2GB).
	ImportMaxTotalSize = 2 * 1024 * 1024 * 1024

	// ImportMaxRatio is the max compression ratio (zip bomb protection).
	ImportMaxRatio = 100
)

// =============================================================================
// CODEC PRIORITY
// =============================================================================

// VideoCodecPriority returns codec-to-priority scores (higher = better).
// This is a function (not a var) to prevent accidental mutation of the shared map.
func VideoCodecPriority() map[string]int {
	return map[string]int{
		"vp9.2": 5,
		"vp09":  5, // alternate name
		"vp9":   4,
		"av01":  3,
		"avc1":  2,
		"h264":  1,
	}
}

// AudioCodecPriority returns audio codec-to-priority scores.
// This is a function (not a var) to prevent accidental mutation of the shared map.
func AudioCodecPriority() map[string]int {
	return map[string]int{
		"opus": 3,
		"mp4a": 2,
		"mp3":  1,
	}
}
