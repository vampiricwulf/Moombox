/**
 * Application-wide constants
 *
 * Centralizes all hardcoded values to make them easier to maintain.
 */

import ms from "ms";
import type { YouTubeClientConfig } from "./types/youtube.js";

// =============================================================================
// HTTP CONSTANTS
// =============================================================================

/**
 * Default User-Agent for web requests
 */
export const USER_AGENTS = {
  WEB: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
  ANDROID:
    "com.google.android.youtube/19.09.37 (Linux; U; Android 14; en_US) gzip",
  ANDROID_VR:
    "com.google.android.apps.youtube.vr.oculus/1.71.26 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip",
  TV: "Mozilla/5.0 (ChromiumStylePlatform) Cobalt/Version",
  IOS: "com.google.ios.youtube/19.29.1 (iPhone16,2; U; CPU iOS 17_5_1 like Mac OS X;)",
} as const;

/**
 * Default HTTP headers
 */
export const DEFAULT_HEADERS = {
  "Accept-Language": "en-US,en;q=0.9",
  Accept: "*/*",
} as const;

/**
 * YouTube API base URLs
 */
export const YOUTUBE_URLS = {
  BASE: "https://www.youtube.com",
  API: "https://www.youtube.com/youtubei/v1",
  WATCH: "https://www.youtube.com/watch",
  FEED: "https://www.youtube.com/feeds/videos.xml",
  THUMBNAIL: "https://i.ytimg.com/vi",
} as const;

/**
 * Default YouTube API key
 */
export const DEFAULT_API_KEY = "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8";

// =============================================================================
// YOUTUBE CLIENT CONFIGURATIONS
// =============================================================================

/**
 * TV Downgraded client - primary authenticated client (yt-dlp default)
 */
export const TV_DOWNGRADED_CLIENT: YouTubeClientConfig = {
  clientName: "TVHTML5",
  clientVersion: "5.20260114",
  clientId: "7",
  userAgent: USER_AGENTS.TV,
  context: {
    clientName: "TVHTML5",
    clientVersion: "5.20260114",
    hl: "en",
  },
};

/**
 * Web Creator client - fallback for member content
 */
export const WEB_CREATOR_CLIENT: YouTubeClientConfig = {
  clientName: "WEB_CREATOR",
  clientVersion: "1.20260120.01.00",
  clientId: "62",
  userAgent: USER_AGENTS.WEB,
  context: {
    clientName: "WEB_CREATOR",
    clientVersion: "1.20260120.01.00",
    hl: "en",
  },
};

/**
 * Web client - for watch page fetching
 */
export const WEB_CLIENT: YouTubeClientConfig = {
  clientName: "WEB",
  clientVersion: "2.20260120.01.00",
  clientId: "1",
  userAgent: USER_AGENTS.WEB,
  context: {
    clientName: "WEB",
    clientVersion: "2.20260120.01.00",
    hl: "en",
  },
};

/**
 * Android VR client - fallback for VOD downloads without cookies.
 * Returns full adaptive formats without requiring auth, JS player, or PO token.
 */
export const ANDROID_VR_CLIENT: YouTubeClientConfig = {
  clientName: "ANDROID_VR",
  clientVersion: "1.71.26",
  clientId: "28",
  userAgent: USER_AGENTS.ANDROID_VR,
  context: {
    clientName: "ANDROID_VR",
    clientVersion: "1.71.26",
    androidSdkVersion: 32,
    osVersion: "12L",
    deviceMake: "Oculus",
    deviceModel: "Quest 3",
  },
};

/**
 * Android SDK-less client - for public content without signature cipher
 */
export const ANDROID_SDKLESS_CLIENT: YouTubeClientConfig = {
  clientName: "ANDROID",
  clientVersion: "19.09.37",
  clientId: "3",
  userAgent: USER_AGENTS.ANDROID,
  context: {
    clientName: "ANDROID",
    clientVersion: "19.09.37",
    androidSdkVersion: 34,
    osVersion: "14",
    deviceMake: "Google",
    deviceModel: "Pixel 8 Pro",
  },
};

// =============================================================================
// TWITCH CONSTANTS
// =============================================================================

/**
 * Twitch API URLs
 */
export const TWITCH_URLS = {
  BASE: "https://www.twitch.tv",
  GQL: "https://gql.twitch.tv/gql",
  USHER_LIVE: "https://usher.ttvnw.net/api/channel/hls",
  USHER_VOD: "https://usher.ttvnw.net/vod",
  EMOTE_CDN: "https://static-cdn.jtvnw.net/emoticons/v2",
  PREVIEW_CDN: "https://static-cdn.jtvnw.net/previews-ttv",
  IRC_WS: "wss://irc-ws.chat.twitch.tv:443",
  DECAPI_UPTIME: "https://decapi.me/twitch/uptime",
  OAUTH_VALIDATE: "https://id.twitch.tv/oauth2/validate",
} as const;

/**
 * Public GQL Client-ID (same as yt-dlp / streamlink)
 */
export const TWITCH_GQL_CLIENT_ID = "kimne78kx3ncx6brgo4mv6wki5h1ko";

/**
 * GQL persisted query hashes (from yt-dlp twitch.py, battle-tested)
 */
export const TWITCH_GQL_HASHES = {
  StreamMetadata: "b57f9b910f8cd1a4659d894fe7550ccc81ec9052c01e438b290fd66a040b9b93",
  ComscoreStreamingQuery: "e1edae8122517d013405f237ffcc124515dc6ded82480a88daef69c83b53ac01",
  PlaybackAccessToken: "ed230aa1e33e07eebb8928504583da78a5173989fadfb11c9e8d60b96e87e3e8",
  VideoMetadata: "45111672eea2e507f8ba44d101a61862f9c56b11dee09a15634cb75cb9b9084d",
} as const;

/**
 * Twitch third-party emote API endpoints
 */
export const TWITCH_EMOTE_APIS = {
  BTTV_GLOBAL: "https://api.betterttv.net/3/cached/emotes/global",
  BTTV_CHANNEL: "https://api.betterttv.net/3/cached/users/twitch",
  FFZ_GLOBAL: "https://api.frankerfacez.com/v1/set/global",
  FFZ_CHANNEL: "https://api.frankerfacez.com/v1/room/id",
  SEVENTV_USER: "https://7tv.io/v3/users/twitch",
  SEVENTV_GLOBAL: "https://7tv.io/v3/emote-sets/global",
} as const;

/**
 * Twitch IRC constants
 */
export const TWITCH_IRC = {
  /** Maximum messages before flushing to disk */
  FLUSH_THRESHOLD: 50_000,
  /** Messages to keep in memory after flush */
  KEEP_IN_MEMORY: 5_000,
  /** Consecutive errors before giving up */
  MAX_CONSECUTIVE_ERRORS: 20,
} as const;

// =============================================================================
// DOWNLOAD CONSTANTS
// =============================================================================

/**
 * Download chunk size (5 MB)
 */
export const DOWNLOAD_CHUNK_SIZE = 5 * 1024 * 1024;

/**
 * Maximum retry attempts for downloads
 */
export const MAX_DOWNLOAD_RETRIES = 3;

/**
 * Delay between retries (ms)
 */
export const RETRY_DELAY_MS = 2000;

/**
 * Download timeout (ms)
 */
export const DOWNLOAD_TIMEOUT_MS = 30000;

/**
 * Progress update interval (ms)
 */
export const PROGRESS_UPDATE_INTERVAL_MS = 3000;

// =============================================================================
// WORKER CONSTANTS
// =============================================================================

/**
 * Worker queue check interval (ms)
 */
export const WORKER_CHECK_INTERVAL_MS = 5000;

/**
 * Stream status recheck interval (ms)
 */
export const STREAM_RECHECK_INTERVAL_MS = 30000;

/**
 * Maximum random jitter added to probe intervals (ms)
 * Staggers concurrent upcoming stream polls to avoid rate limiting
 */
export const PROBE_JITTER_MAX_MS = 30_000;

/**
 * Maximum consecutive probe errors before moving job to Error state
 */
export const MAX_CONSECUTIVE_PROBE_ERRORS = 10;

/**
 * Stream segment timeout - if no new segment received for this duration,
 * consider the stream potentially ended (10 minutes)
 */
export const STREAM_SEGMENT_TIMEOUT_MS = ms("10m");

/**
 * Stream end verification interval - how often to check YouTube API
 * to confirm stream status when no segments are being received (5 minutes)
 */
export const STREAM_END_VERIFY_INTERVAL_MS = ms("5m");

// =============================================================================
// COOKIE REFRESH CONSTANTS
// =============================================================================

/**
 * Cookie refresh interval (30 minutes)
 */
export const COOKIE_REFRESH_INTERVAL_MS = ms("30m");

// =============================================================================
// LOGGING CONSTANTS
// =============================================================================

/**
 * Default log file size limit (10 MB)
 */
export const DEFAULT_LOG_MAX_SIZE = 10 * 1024 * 1024;

/**
 * Default number of log files to keep
 */
export const DEFAULT_LOG_MAX_FILES = 5;

// =============================================================================
// TUI CONSTANTS
// =============================================================================

/**
 * Maximum visible jobs in the TUI
 */
export const MAX_VISIBLE_JOBS = 10;

/**
 * Maximum visible log lines in the TUI
 */
export const MAX_VISIBLE_LOGS = 6;

/**
 * Job details log limit
 */
export const MAX_JOB_LOGS = 100;

// =============================================================================
// DATABASE CONSTANTS
// =============================================================================

/**
 * Default days to show finished jobs
 */
export const DEFAULT_HIDE_FINISHED_DAYS = 7;

// =============================================================================
// FEED MONITOR CONSTANTS
// =============================================================================

/**
 * Default feed check interval (5 minutes)
 */
export const FEED_CHECK_INTERVAL_MS = ms("5m");

/**
 * Default max feed items to process
 */
export const DEFAULT_MAX_FEED_ITEMS = 15;

// =============================================================================
// BOTGUARD CONSTANTS
// =============================================================================

/**
 * BotGuard request key for PO token generation
 * This is a hardcoded API key that has been used by YouTube for years
 */
export const BOTGUARD_REQUEST_KEY = "O43z0dpjhgX20SCx4KAo";

// =============================================================================
// THUMBNAIL QUALITY ORDER
// =============================================================================

/**
 * YouTube thumbnail quality names in order of preference
 */
export const THUMBNAIL_QUALITIES = [
  "maxresdefault",
  "sddefault",
  "hqdefault",
  "mqdefault",
  "default",
] as const;
