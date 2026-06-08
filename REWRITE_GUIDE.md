# Moombox Go + Goja Rewrite Guide

This document provides everything needed to rewrite [Moombox](../Moombox/) — a YouTube/Twitch live stream archiver — from TypeScript/Node.js to Go, using [Goja](https://github.com/nicholasgasior/goja) (pure-Go JavaScript engine) for the parts that require JS execution.

## Why Go + Goja?

The TypeScript version suffers from memory issues inherent to V8/Node.js:
- **JSDOM** (~30-50MB) needed solely for BotGuard VM execution
- **undici** (Node.js fetch) leaks AbortController references (known Node 20 bug)
- **React/Ink TUI** promotes objects to V8 old_space faster than GC can reclaim (~0.7MB/min during downloads)
- No way to tune V8 GC from userland — the only fix is to eliminate the runtime

Go provides deterministic memory management, excellent concurrency primitives, and compiles to a single static binary. Goja provides an ES5.1+ JS engine in pure Go — no CGo, no V8.

## Reference TypeScript Codebase

The complete TypeScript source lives at `D:\Git\Moombox\`. You can freely reference, copy, and adapt any file. Key documentation:
- `CLAUDE.md` — Complete architecture reference (read this first)
- `package.json` — Dependencies and scripts
- `src/constants.ts` — All hardcoded values (API keys, URLs, timeouts, client configs)

## Architecture Overview

### What Moombox Does

1. **Monitors** YouTube channels (RSS feeds + DECAPI API) and Twitch channels (GQL polling)
2. **Detects** when a stream goes live or a new VOD appears
3. **Downloads** the stream using YouTube's Innertube API (DASH/HLS segments) or Twitch HLS
4. **Records live chat** in parallel with the video download
5. **Muxes** audio + video into a final MP4 using FFmpeg
6. **Serves** a web dashboard (Express/WebSocket) and TUI (Ink/React) for monitoring

### Data Flow

```
RSS Feed Monitor ───→ Job Database → Download Worker → YouTube Innertube API
DECAPI Monitor ─────↗                       ↓
Twitch Monitor ────↗  Web Dashboard ← FFmpeg Muxer ← Segment Downloads (DASH/HLS/VOD)
                      (localhost:774)      ↑
                                     Chat Downloader (live chat polling)
```

### What Needs Goja (JS Execution)

Only two subsystems require JavaScript execution. Everything else is pure HTTP/data processing:

#### 1. BotGuard / PO Token Generation (`src/bgutils/`, `src/core/potProvider.ts`, `src/core/globalDom.ts`)

YouTube requires a "Proof of Origin" token for video playback. The flow:

```
1. POST to Google BotGuard API → receive challenge (interpreterJS + program + globalName)
2. Execute interpreterJS via new Function(js)() → creates VM object on globalThis[globalName]
3. Call VM.a(program, callback) → returns syncSnapshotFunction, sets vmFunctions via callback
4. Call asyncSnapshotFunction({webPoSignalOutput}) → BotGuard response string
5. POST BotGuard response to GenerateIT API → integrity token (with TTL)
6. webPoSignalOutput[0](integrityToken) → mint callback function
7. mintCallback(contentBinding) → PO token (base64url encoded bytes)
```

**Key files:**
- `src/bgutils/core/challengeFetcher.ts` — Fetches and descrambles challenge from Google API
- `src/bgutils/core/botGuardClient.ts` — Loads interpreter JS, runs VM, takes snapshots
- `src/bgutils/core/webPoClient.ts` — Orchestrates: challenge → snapshot → integrity token → mint
- `src/bgutils/core/webPoMinter.ts` — Mints PO tokens using callback from BotGuard VM
- `src/core/potProvider.ts` — Caching layer: minter cache (TTL from API), session cache (6hr), inflight dedup
- `src/core/globalDom.ts` — JSDOM setup/teardown, timer interception for BotGuard's leaked timers

**DOM requirements for BotGuard:** The BotGuard interpreter JS expects a browser-like environment. In TypeScript we use JSDOM. In Go/Goja, you need minimal DOM shims:
- `window`, `document`, `navigator`, `location` globals
- `document.createElement()`, `document.body.appendChild()` (BotGuard creates elements)
- `navigator.userAgent` (checked by BotGuard)
- `window.setTimeout`, `window.setInterval` (BotGuard creates telemetry timers)
- `TextEncoder`, `TextDecoder`, `atob`, `btoa`, `Uint8Array`

**Not needed:** Full DOM, CSS, layout, event dispatch, canvas, WebGL

**Constants:**
- `BOTGUARD_REQUEST_KEY`: `"O43z0dpjhgX20SCx4KAo"`
- Challenge API: `https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/Create`
- GenerateIT API: `https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/GenerateIT`
- YouTube variant: `https://www.youtube.com/api/jnn/v1/Create` and `/GenerateIT`
- Headers: `content-type: application/json+protobuf`, `x-goog-api-key: AIzaSyDyT68fXzCCl80aj_EK1VPRk4piVVYIT4I`
- User-Agent for requests: `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36`

**Challenge descramble algorithm** (from `challengeFetcher.ts`):
```
base64decode(challenge) → each byte + 97 → TextDecoder → JSON.parse
```

**Cold start token** (fallback, works while StreamProtectionStatus=2):
```go
// See generateColdStartToken() in webPoClient.ts
// Pure byte manipulation — no JS needed
```

#### 2. Cipher / Signature Decryption (`src/cipher/`, `src/ejs/`)

YouTube encrypts video URLs with signature ciphers. The flow:

```
1. Fetch YouTube player JS (~1.5MB JavaScript file)
2. Cache player JS on disk (14-day TTL, keyed by player URL hash)
3. Parse player JS with meriyah (JS parser) → AST
4. Extract signature cipher function and n-parameter function from AST
5. Generate standalone JS code (via astring) that runs these functions
6. Execute generated code via new Function("_result", code)(resultObj)
7. resultObj.sig(encryptedSig) → decrypted signature
8. resultObj.n(nParam) → decrypted n-parameter
```

**Key files:**
- `src/cipher/src/solver.ts` — 3-tier cache: disk → preprocessed (LRU 150) → solver (LRU 50)
- `src/cipher/src/handlers/decryptSignature.ts` — Entry point: takes SignatureRequest, returns SignatureResponse
- `src/cipher/src/handlers/getSts.ts` — Extracts STS (Signature Timestamp) from player JS via regex
- `src/cipher/src/handlers/resolveUrl.ts` — Replaces sig/n params in stream URL
- `src/cipher/src/playerCache.ts` — Disk cache at `~/.cache/yt-cipher/player_cache/` (14-day TTL)
- `src/ejs/yt/solver/solvers.ts` — AST parsing: `preprocessPlayer()` extracts n/sig functions, `getFromPrepared()` runs them
- `src/ejs/yt/solver/n.ts` — N-parameter function extractor (AST pattern matching)
- `src/ejs/yt/solver/sig.ts` — Signature cipher function extractor (AST pattern matching)
- `src/ejs/yt/solver/setup.ts` — Browser stub AST nodes (window, document, navigator stubs)

**For Go/Goja:** You can take two approaches:
1. **Port the AST extraction to Go** using a Go JS parser, then run the extracted functions in Goja
2. **Run the entire preprocessPlayer pipeline in Goja** (simpler, meriyah+astring are pure JS)

Option 2 is recommended. Bundle meriyah + astring + the solver code as a JS module, run it in Goja, get back the preprocessed code, then run that in a fresh Goja VM to get sig/n functions.

**Types:**
```go
type SignatureRequest struct {
    EncryptedSignature string
    NParam             string
    PlayerURL          string
}

type SignatureResponse struct {
    DecryptedSignature string
    DecryptedNSig      string
}

type Solvers struct {
    N   func(string) string // nil if not found
    Sig func(string) string // nil if not found
}
```

### What Does NOT Need Goja (Pure Go)

Everything else is HTTP requests, data processing, and I/O:

#### YouTube Innertube API (`src/engine/youtube/`)

Multi-client strategy for fetching video info:

| Priority | Client | ClientID | Purpose | Auth |
|----------|--------|----------|---------|------|
| 0 | ANDROID_VR | 28 | Probe, no auth needed | None |
| 1 | WATCH_PAGE | - | HTML scrape for ytcfg | Cookies |
| 2 | TV (TVHTML5) | 7 | Primary authenticated | Cookies |
| 3 | WEB | 1 | DASH manifests | Cookies |
| 4 | WEB_CREATOR | 62 | Members-only fallback | Cookies |

API endpoint: `https://www.youtube.com/youtubei/v1/player`

Request body format:
```json
{
  "videoId": "VIDEO_ID",
  "context": {
    "client": {
      "clientName": "ANDROID_VR",
      "clientVersion": "1.71.26",
      "androidSdkVersion": 32,
      "osVersion": "12L",
      "deviceMake": "Oculus",
      "deviceModel": "Quest 3"
    }
  },
  "playbackContext": {
    "contentPlaybackContext": {
      "signatureTimestamp": 20293
    }
  },
  "serviceIntegrityDimensions": {
    "poToken": "BASE64_PO_TOKEN"
  }
}
```

**Auth headers** (when cookies present):
- `Cookie: SID=...; HSID=...; SSID=...; APISID=...; SAPISID=...`
- `Authorization: SAPISIDHASH {timestamp}_{sha1(timestamp + " " + SAPISID + " " + origin)}`
- `X-Goog-AuthUser: 0`
- `X-Origin: https://www.youtube.com`

**Cookie format:** Netscape cookie file (`cookies.txt`), tab-separated

**User-Agents:**
```
WEB:        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
ANDROID_VR: "com.google.android.apps.youtube.vr.oculus/1.71.26 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip"
TV:         "Mozilla/5.0 (ChromiumStylePlatform) Cobalt/Version"
```

#### Download Engine (`src/engine/`)

**Segment Downloader** (`src/engine/downloader.ts`):
- Downloads DASH/HLS segments sequentially during live, parallel (6 concurrent) during catch-up
- Head probe every 5 seconds via `X-Head-Seqnum` response header
- Catch-up threshold: 10 segments behind → switch to parallel mode
- Max segment retries: 10 (catch-up), exponential backoff capped at config value
- Resume state saved every 10 (catch-up) or 50 (sequential) segments
- Extends EventEmitter: emits `start`, `progress`, `finish`, `gap`, `error`

**Manifest Parser** (`src/engine/manifest.ts`):
- DASH: XML parsing of MPD manifests → extract representations, segment templates
- HLS: M3U8 text parsing → extract variants, segments, media sequence numbers

**Muxer** (`src/engine/muxer.ts`):
- FFmpeg wrapper: combines video + audio segments → MP4
- Uses stdin pipe or file concatenation protocol
- Supports cancel via signal

**Download strategies** (`src/core/worker/downloadStrategies.ts`):
- `downloadVod()` — Parallel video + audio, each gets its own SegmentDownloader
- `downloadDash()` — Video + audio SegmentDownloaders, requires signature decryption for URLs
- `downloadHls()` — Single SegmentDownloader for HLS stream

**Progress tracking** (`src/core/worker/progressTracking.ts`):
- Display updates every 100ms (in-memory only)
- Disk persistence every 1000ms (crash recovery)
- Speed/ETA smoothing: EMA with alpha=0.7

#### Chat System (`src/engine/chat/`)

**ChatApi** (`src/engine/chat/chatApi.ts`):
- YouTube Innertube `live_chat/get_live_chat` and `live_chat/get_live_chat_replay` endpoints
- Continuation-based polling (server provides next continuation token + timeout)
- Parses: text messages, emojis, super chats, memberships, badges

**ChatDownloader** (`src/engine/chat/chatDownloader.ts`):
- Polling loop with server-specified timeout
- Memory bounding: flush to disk at 50k messages, keep last 5k in memory
- Dedup: bounded Set of 5,000 recent message IDs
- Resume: saves continuation + last timestamp + recent IDs
- Write batching: 1-second window
- Consecutive error tolerance: 20 (live), 5 (VOD)

#### Monitoring (`src/core/monitor.ts`, `src/core/decapiMonitor.ts`, `src/core/twitchMonitor.ts`)

**FeedMonitor** — RSS feed polling:
- URL: `https://www.youtube.com/feeds/videos.xml?channel_id=CHANNEL_ID`
- XML parsing with fast-xml-parser
- Term matching (regex) on video titles
- Interval: `feed_check_interval` config (default 10 minutes)

**DecapiMonitor** — DECAPI latest video polling:
- URL: `https://decapi.me/youtube/latest_video?id=CHANNEL_ID`
- Dynamic rate limiting based on response headers (`X-RateLimit-*`)
- 1-second stagger between requests within a cycle

**TwitchMonitor** — Twitch GQL polling:
- URL: `https://gql.twitch.tv/gql`
- Client-ID: `kimne78kx3ncx6brgo4mv6wki5h1ko`
- Persisted query hashes for stream metadata
- Interval: 15s default with ±5s jitter

#### Web Server (`src/web/`)

Express 5 HTTP + WebSocket server:
- Port 774 (configurable)
- REST API at `/api/`
- WebSocket for real-time updates
- Static file serving for dashboard
- CORS, IP gating, rate limiting, CSRF protection

**Go equivalent:** Use `net/http` + `gorilla/websocket` (or `nhooyr.io/websocket`)

See `CLAUDE.md` in the TypeScript project for the complete API route table.

#### Database (`src/core/database.ts`)

Currently lowdb (JSON file). For Go, use an embedded database:

**Current schema** (`moombox.json`):
```json
{
  "jobs": [Job...],
  "history": ["videoId1", "videoId2", ...],
  "lastVideos": { "channelId": "latestVideoId" }
}
```

`history` is pruned at 10,000 entries. Jobs have O(1) lookup via in-memory Map.

**Pub/Sub events:**
- `onJobUpdate(callback)` — fires after 100ms batch window, carries single Job
- `onJobsChange(callback)` — fires on add/delete, carries full Job array

**Go equivalent:** Consider bbolt, badger, or SQLite via `modernc.org/sqlite` (pure Go).

#### Configuration (`src/core/config.ts`)

TOML config file (`config.toml`), searched in: cwd → `./config/` → `~/.config/moombox/`

**Go equivalent:** Use `github.com/BurntSushi/toml`

See `src/types/config.ts` for the complete config interface (copied below in Types section).

## Complete Type Definitions

These are the core data types. Port them to Go structs:

### Job (Primary Data Model)

```go
type JobStatus string

const (
    StatusUpcoming    JobStatus = "Upcoming"
    StatusLive        JobStatus = "Live"
    StatusDownloading JobStatus = "Downloading"
    StatusMuxing      JobStatus = "Muxing"
    StatusFinished    JobStatus = "Finished"
    StatusError       JobStatus = "Error"
    StatusCancelled   JobStatus = "Cancelled"
    StatusCookies     JobStatus = "COOKIES?"
)

type Job struct {
    ID                string     `json:"id"`
    VideoID           string     `json:"videoId"`
    URL               string     `json:"url"`
    Title             string     `json:"title"`
    ChannelName       string     `json:"channelName"`
    Platform          string     `json:"platform,omitempty"` // "youtube" | "twitch"
    Status            JobStatus  `json:"status"`
    Progress          string     `json:"progress"`
    Percent           float64    `json:"percent"`
    ETA               string     `json:"eta"`
    Speed             string     `json:"speed"`
    CreatedAt         string     `json:"createdAt"`
    UpdatedAt         string     `json:"updatedAt"`
    Error             string     `json:"error,omitempty"`
    // Download state
    LastVideoSeq      *int       `json:"lastVideoSeq,omitempty"`
    LastAudioSeq      *int       `json:"lastAudioSeq,omitempty"`
    TotalVideoSeq     *int       `json:"totalVideoSeq,omitempty"`
    TotalAudioSeq     *int       `json:"totalAudioSeq,omitempty"`
    IsVod             bool       `json:"isVod,omitempty"`
    ManuallyAdded     bool       `json:"manuallyAdded,omitempty"`
    AllowNonStream    bool       `json:"allowNonStream,omitempty"`
    StreamStartTime   string     `json:"streamStartTime,omitempty"`
    LengthSeconds     *int       `json:"lengthSeconds,omitempty"`
    StreamEndTime     string     `json:"streamEndTime,omitempty"`
    // Media
    ThumbnailURL      string     `json:"thumbnailUrl,omitempty"`
    Description       string     `json:"description,omitempty"`
    OutputFile        string     `json:"outputFile,omitempty"`
    Filename          string     `json:"filename,omitempty"`
    OutputDirectory   string     `json:"outputDirectory,omitempty"`
    VideoWidth        *int       `json:"videoWidth,omitempty"`
    VideoHeight       *int       `json:"videoHeight,omitempty"`
    VideoFps          *int       `json:"videoFps,omitempty"`
    FileSize          *int64     `json:"fileSize,omitempty"`
    DownloadStartedAt string     `json:"downloadStartedAt,omitempty"`
    // Chat
    ChatStatus        string     `json:"chatStatus,omitempty"`
    TotalChatMessages *int       `json:"totalChatMessages,omitempty"`
    ChatFilename      string     `json:"chatFilename,omitempty"`
    // Gaps
    Gaps              []Gap      `json:"gaps,omitempty"`
    // Twitch
    TwitchQuality     string     `json:"twitchQuality,omitempty"`
    TwitchCategory    string     `json:"twitchCategory,omitempty"`
    ChannelAvatarURL  string     `json:"channelAvatarUrl,omitempty"`
    // Advanced options
    SelectedVideoItag *int       `json:"selectedVideoItag,omitempty"`
    SelectedAudioItag *int       `json:"selectedAudioItag,omitempty"`
    StartTime         *float64   `json:"startTime,omitempty"`
    EndTime           *float64   `json:"endTime,omitempty"`
    // Trims
    Trims             []TrimRecord `json:"trims,omitempty"`
}

type Gap struct {
    From   int    `json:"from"`
    To     int    `json:"to"`
    Stream string `json:"stream"` // "video" | "audio"
}

type TrimRecord struct {
    ID        string  `json:"id"`
    StartTime float64 `json:"startTime"`
    EndTime   float64 `json:"endTime"`
    Filename  string  `json:"filename"`
    CreatedAt string  `json:"createdAt"`
    Duration  float64 `json:"duration"`
    FileSize  *int64  `json:"fileSize,omitempty"`
}
```

### VideoInfo (YouTube Response)

```go
type StreamStatus string

const (
    StreamLive        StreamStatus = "live"
    StreamUpcoming    StreamStatus = "upcoming"
    StreamVOD         StreamStatus = "vod"
    StreamPostLive    StreamStatus = "post_live"
    StreamNotAStream  StreamStatus = "not_a_stream"
)

type VideoInfo struct {
    Title              string       `json:"title"`
    ChannelName        string       `json:"channelName"`
    ChannelID          string       `json:"channelId"`
    Description        string       `json:"description"`
    ThumbnailURL       string       `json:"thumbnailUrl,omitempty"`
    Formats            []Format     `json:"formats"`
    PlayerURL          string       `json:"playerUrl"`
    StreamStatus       StreamStatus `json:"streamStatus"`
    IsLive             bool         `json:"isLive"`
    IsUpcoming         bool         `json:"isUpcoming"`
    IsPostLiveDVR      bool         `json:"isPostLiveDVR"`
    LengthSeconds      *int         `json:"lengthSeconds,omitempty"`
    EndTimestamp        string       `json:"endTimestamp,omitempty"`
    ScheduledStartTime string       `json:"scheduledStartTime,omitempty"`
    DashManifestURL    string       `json:"dashManifestUrl,omitempty"`
    HlsManifestURL     string       `json:"hlsManifestUrl,omitempty"`
    PlayabilityError   string       `json:"playabilityError,omitempty"`
    PlayabilityReason  string       `json:"playabilityReason,omitempty"`
}

type Format struct {
    Itag           int    `json:"itag"`
    URL            string `json:"url,omitempty"`
    MimeType       string `json:"mimeType"`
    Bitrate        int    `json:"bitrate"`
    Width          *int   `json:"width,omitempty"`
    Height         *int   `json:"height,omitempty"`
    ContentLength  string `json:"contentLength,omitempty"`
    QualityLabel   string `json:"qualityLabel,omitempty"`
    AudioQuality   string `json:"audioQuality,omitempty"`
    AudioSampleRate string `json:"audioSampleRate,omitempty"`
    Fps            *int   `json:"fps,omitempty"`
    Source         string `json:"source,omitempty"`
    AuthLevel      *int   `json:"authLevel,omitempty"`
}
```

### Chat Message

```go
type ChatMessage struct {
    ID              string        `json:"id"`
    TimestampUsec   string        `json:"timestampUsec"`
    TimestampText   string        `json:"timestampText"`
    OffsetMs        int64         `json:"offsetMs"`
    AuthorName      string        `json:"authorName"`
    AuthorChannelID string        `json:"authorChannelId"`
    AuthorBadges    []string      `json:"authorBadges"`
    Message         []MessagePart `json:"message"`
    Superchat       *SuperchatInfo `json:"superchat,omitempty"`
    IsMembership    bool          `json:"isMembership,omitempty"`
}

type MessagePart struct {
    Type          string `json:"type"` // "text" | "emoji"
    Text          string `json:"text,omitempty"`
    EmojiID       string `json:"emojiId,omitempty"`
    EmojiURL      string `json:"emojiUrl,omitempty"`
    IsCustomEmoji bool   `json:"isCustomEmoji,omitempty"`
}

type SuperchatInfo struct {
    Amount   string `json:"amount"`
    Currency string `json:"currency"`
    Color    string `json:"color"`
    Tier     int    `json:"tier"`
}
```

### Config

```go
type MoomboxConfig struct {
    Port                 int              `toml:"port"`
    NetworkAccess        string           `toml:"network_access"`
    PasswordHash         string           `toml:"password_hash,omitempty"`
    LogLevel             string           `toml:"log_level"`
    LogFilePath          string           `toml:"log_file_path"`
    LogMaxFileSize       int              `toml:"log_max_file_size"`
    LogMaxFiles          int              `toml:"log_max_files"`
    DatabasePath         string           `toml:"database_path"`
    MaxFeedItems         int              `toml:"max_feed_items"`
    FeedCheckInterval    interface{}      `toml:"feed_check_interval"` // int (minutes) or string
    DecapiCheckInterval  *int             `toml:"decapi_check_interval,omitempty"`
    TwitchCheckInterval  *int             `toml:"twitch_check_interval,omitempty"`
    Downloader           DownloaderConfig `toml:"downloader"`
    Tasklist             *TasklistConfig  `toml:"tasklist,omitempty"`
    AutoCookies          *AutoCookiesConfig `toml:"auto_cookies,omitempty"`
    Notifications        []NotificationConfig `toml:"notifications,omitempty"`
    Channels             []ChannelConfig  `toml:"channels,omitempty"`
}

type DownloaderConfig struct {
    MaxVideoResolution     int    `toml:"max_video_resolution"`
    FfmpegPath             string `toml:"ffmpeg_path,omitempty"`
    StagingDirectory       string `toml:"staging_directory"`
    OutputDirectory        string `toml:"output_directory"`
    OutputTemplate         string `toml:"output_template"`
    NumParallelDownloads   int    `toml:"num_parallel_downloads"`
    PoToken                string `toml:"po_token,omitempty"`
    VisitorData            string `toml:"visitor_data,omitempty"`
    CookieFile             string `toml:"cookie_file"`
    PotProviderURL         string `toml:"pot_provider_url,omitempty"`
    DownloadChat           bool   `toml:"download_chat"`
    Prefer60fps            bool   `toml:"prefer_60fps"`
    SegmentRetryDelayCap   int    `toml:"segment_retry_delay_cap"`
    SegmentLiveCheckRetries int   `toml:"segment_live_check_retries"`
}

type ChannelConfig struct {
    ID                    string      `toml:"id"`
    Name                  string      `toml:"name,omitempty"`
    Platform              string      `toml:"platform,omitempty"` // "youtube" | "twitch"
    Enabled               *bool       `toml:"enabled,omitempty"`
    Terms                 interface{} `toml:"terms,omitempty"` // string or map[string]string
    NumDescLookbehind     *int        `toml:"num_desc_lookbehind,omitempty"`
    OutputDirectory       string      `toml:"output_directory,omitempty"`
    IncludeNonLiveContent bool        `toml:"include_non_live_content,omitempty"`
    MaxFeedItems          *int        `toml:"max_feed_items,omitempty"`
    QualityPreference     string      `toml:"quality_preference,omitempty"`
}
```

## Recommended Go Libraries

| Purpose | Go Library | Replaces |
|---------|-----------|----------|
| JS Engine | `github.com/dop251/goja` | V8/Node.js |
| TUI | `github.com/charmbracelet/bubbletea` | Ink (React for CLI) |
| TUI Components | `github.com/charmbracelet/lipgloss` | Ink Box/Text |
| HTTP Server | `net/http` (stdlib) | Express 5 |
| HTTP Router | `github.com/go-chi/chi/v5` | Express routing |
| WebSocket | `nhooyr.io/websocket` | ws |
| TOML Config | `github.com/BurntSushi/toml` | smol-toml |
| XML Parsing | `encoding/xml` (stdlib) | fast-xml-parser |
| JSON DB | `go.etcd.io/bbolt` or `encoding/json` | lowdb |
| Logging | `log/slog` (stdlib) | Custom Logger |
| Process Exec | `os/exec` (stdlib) | execa |
| Retry | `github.com/avast/retry-go/v4` | p-retry |
| Concurrency | Goroutines + channels + `sync` | p-queue, p-limit |
| File Utils | `os`, `io/fs` (stdlib) | fs-extra |
| ZIP | `archive/zip` (stdlib) | adm-zip |
| Validation | `github.com/go-playground/validator/v10` | zod |
| Cookie File | Custom parser (simple) | Custom parser |
| Rate Limiting | `golang.org/x/time/rate` | Custom rate limiter |

## Goja Integration Details

### Minimal DOM Shim for BotGuard

BotGuard's interpreter needs a browser-like environment. Create a minimal shim in Go:

```go
// This is conceptual — adapt based on what BotGuard actually accesses
func setupDOMShim(vm *goja.Runtime) {
    // navigator
    nav := vm.NewObject()
    nav.Set("userAgent", "Mozilla/5.0 ...")
    nav.Set("language", "en-US")
    nav.Set("languages", vm.NewArray("en-US", "en"))

    // document (minimal)
    doc := vm.NewObject()
    doc.Set("createElement", func(tag string) *goja.Object {
        el := vm.NewObject()
        el.Set("tagName", strings.ToUpper(tag))
        el.Set("appendChild", func(child goja.Value) goja.Value { return child })
        el.Set("setAttribute", func(name, value string) {})
        el.Set("style", vm.NewObject())
        return el
    })
    body := vm.NewObject()
    body.Set("appendChild", func(child goja.Value) goja.Value { return child })
    doc.Set("body", body)
    doc.Set("head", vm.NewObject())
    doc.Set("documentElement", vm.NewObject())

    // location
    loc := vm.NewObject()
    loc.Set("href", "https://www.youtube.com/")
    loc.Set("origin", "https://www.youtube.com")
    loc.Set("hostname", "www.youtube.com")
    loc.Set("protocol", "https:")

    // window
    window := vm.NewObject()
    window.Set("navigator", nav)
    window.Set("document", doc)
    window.Set("location", loc)
    window.Set("origin", "https://www.youtube.com")

    // Set globals
    vm.Set("window", window)
    vm.Set("document", doc)
    vm.Set("navigator", nav)
    vm.Set("location", loc)
    vm.Set("origin", "https://www.youtube.com")

    // Timer stubs (track IDs to clean up)
    vm.Set("setTimeout", func(fn goja.Callable, delay int64) int64 {
        // Run in goroutine with timer
        id := nextTimerID()
        go func() {
            time.Sleep(time.Duration(delay) * time.Millisecond)
            fn(goja.Undefined())
        }()
        return id
    })
    vm.Set("setInterval", func(fn goja.Callable, delay int64) int64 {
        // Run in goroutine with ticker — track for cleanup
        return startInterval(fn, delay)
    })
    vm.Set("clearTimeout", func(id int64) { cancelTimer(id) })
    vm.Set("clearInterval", func(id int64) { cancelTimer(id) })

    // TextEncoder/TextDecoder
    vm.Set("TextEncoder", func(call goja.ConstructorCall) *goja.Object {
        obj := call.This
        obj.Set("encode", func(s string) []byte { return []byte(s) })
        return obj
    })
    vm.Set("TextDecoder", func(call goja.ConstructorCall) *goja.Object {
        obj := call.This
        obj.Set("decode", func(buf []byte) string { return string(buf) })
        return obj
    })

    // atob/btoa
    vm.Set("atob", func(s string) string {
        decoded, _ := base64.StdEncoding.DecodeString(s)
        return string(decoded)
    })
    vm.Set("btoa", func(s string) string {
        return base64.StdEncoding.EncodeToString([]byte(s))
    })
}
```

**Important:** Goja is single-threaded per runtime. Create a new `goja.Runtime` for each BotGuard session. The minter callback (pure byte manipulation) can be extracted and called from Go after the VM is done.

### Cipher Solver in Goja

For the cipher solver, bundle the meriyah parser + astring generator + solver extraction code as a single JS file, then run it in Goja:

```go
func preprocessPlayer(playerJS string) (string, error) {
    vm := goja.New()
    // Load bundled meriyah + astring + solver code
    _, err := vm.RunString(bundledSolverJS)
    if err != nil {
        return "", err
    }
    // Call preprocessPlayer
    fn, _ := goja.AssertFunction(vm.Get("preprocessPlayer"))
    result, err := fn(goja.Undefined(), vm.ToValue(playerJS))
    if err != nil {
        return "", err
    }
    return result.String(), nil
}

func getFromPrepared(code string) (Solvers, error) {
    vm := goja.New()
    // Provide browser stubs that the extracted code expects
    setupCipherStubs(vm) // window, document, navigator stubs
    resultObj := vm.NewObject()
    resultObj.Set("n", goja.Null())
    resultObj.Set("sig", goja.Null())
    // Run: Function("_result", code)(resultObj)
    fn, err := vm.RunString("(function(_result) { " + code + " })")
    if err != nil {
        return Solvers{}, err
    }
    callable, _ := goja.AssertFunction(fn)
    _, err = callable(goja.Undefined(), resultObj)
    if err != nil {
        return Solvers{}, err
    }
    // Extract n and sig functions
    var solvers Solvers
    if nFn := resultObj.Get("n"); nFn != nil && !goja.IsNull(nFn) && !goja.IsUndefined(nFn) {
        nCallable, _ := goja.AssertFunction(nFn)
        solvers.N = func(val string) string {
            r, _ := nCallable(goja.Undefined(), vm.ToValue(val))
            return r.String()
        }
    }
    // Same for sig...
    return solvers, nil
}
```

## Job Processing Pipeline

```
Job pulled from DB
  → StreamProcessor.Process(job)
      → Probe via ANDROID_VR (no cookies)
      → If Upcoming: wait polling loop (30s + jitter, max 10 consecutive errors)
      → Optionally start ChatDownloader early
  → DownloadOrchestrator.Execute(job, videoInfo, isVOD, chatDl)
      → Strategy selection: VOD / DASH / HLS
      → SegmentDownloader(s) for video + audio
      → ChatDownloader for live chat
      → ProgressTracking: events → DB updates
      → MuxFinalize: FFmpeg mux + ffprobe metadata + file copy
      → Optional TrimService for timestamp ranges
```

### Job Status Flow

`Upcoming` → `Live` → `Downloading` → `Muxing` → `Finished`

Special states: `Error`, `Cancelled`, `COOKIES?`

**Job list sorting** (TUI + web): Status priority first (`Error` > `COOKIES?` > `Downloading` > `Muxing` > `Live` > `Upcoming` > `Cancelled` > `Finished`), then active jobs alphabetically by title, terminal jobs by most-recently-updated first.

## Initialization Order

Services must start in this exact order:

```
1. ConfigManager     → TOML config
2. Logger            → File rotation, pub/sub
3. Database          → JSON DB, indexes
4. FeedMonitor       → RSS polling
5. DecapiMonitor     → DECAPI polling
6. TwitchMonitor     → Twitch GQL polling
7. DownloadWorker    → Job queue
8. CookieRefresh     → 30-min cookie refresh
9. AutoCookieService → Browser cookie extraction
10. WebServer        → HTTP + WebSocket
11. TUI              → Terminal UI
```

### Shutdown Order (consumers first, infrastructure last)

```
TwitchMonitor → DecapiMonitor → FeedMonitor → DownloadWorker →
CookieRefresh → AutoCookies → PotProvider → WebServer → Logger.Flush()
```

10-second force-exit timer as safety net.

## Concurrency Model

In Go, replace Node.js concurrency primitives with goroutines + channels:

| TypeScript | Go Equivalent |
|-----------|---------------|
| `p-queue(concurrency: 2)` | Buffered channel semaphore or `errgroup` with limit |
| `p-limit(6)` | `semaphore.NewWeighted(6)` from `golang.org/x/sync` |
| `p-retry(fn, {retries: 3})` | `retry-go` or custom loop with `time.Sleep` |
| `AbortController` | `context.WithCancel()` |
| `EventEmitter` | Channels or callback functions |
| `setInterval` | `time.Ticker` |
| `setTimeout` | `time.After` or `time.Timer` |
| `Promise.all` | `errgroup.Group` |
| `async/await` | Goroutines + channels |

## WebSocket Protocol

Must be compatible with the existing web dashboard (so the same frontend works):

| Message | Direction | Payload |
|---------|-----------|---------|
| `initial_state` | → client | `{ jobs, logs, nextFeedCheck, nextDecapiCheck, nextTwitchCheck }` |
| `jobs_update` | → client | Full `Job[]` array |
| `job_update` | → client | Single `Job` (throttled: 10/sec per job) |
| `check_timers` | → client | `{ nextFeedCheck, nextDecapiCheck, nextTwitchCheck }` |
| `log` | → client | Log string |
| `ping` | client → | Heartbeat |
| `pong` | → client | Response |

WebSocket message format: `{"type": "message_type", "data": ...}`

Throttling: trailing-edge at 100ms per job to prevent flooding clients with progress updates.

## Constants Reference

All hardcoded values from `src/constants.ts`:

```go
const (
    // HTTP
    UserAgentWeb       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
    UserAgentAndroidVR = "com.google.android.apps.youtube.vr.oculus/1.71.26 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip"
    UserAgentTV        = "Mozilla/5.0 (ChromiumStylePlatform) Cobalt/Version"

    // YouTube
    YouTubeBaseURL      = "https://www.youtube.com"
    YouTubeAPIURL       = "https://www.youtube.com/youtubei/v1"
    YouTubeFeedURL      = "https://www.youtube.com/feeds/videos.xml"
    YouTubeThumbnailURL = "https://i.ytimg.com/vi"
    DefaultAPIKey       = "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8"

    // BotGuard
    BotGuardRequestKey = "O43z0dpjhgX20SCx4KAo"
    BotGuardAPIKey     = "AIzaSyDyT68fXzCCl80aj_EK1VPRk4piVVYIT4I"
    GoogleWaaBaseURL   = "https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa"
    YouTubeJnnBaseURL  = "https://www.youtube.com/api/jnn/v1"

    // Twitch
    TwitchGQLURL       = "https://gql.twitch.tv/gql"
    TwitchGQLClientID  = "kimne78kx3ncx6brgo4mv6wki5h1ko"
    TwitchUsherLive    = "https://usher.ttvnw.net/api/channel/hls"
    TwitchUsherVOD     = "https://usher.ttvnw.net/vod"

    // DECAPI
    DecapiYouTubeLatest = "https://decapi.me/youtube/latest_video"

    // Downloads
    DownloadChunkSize       = 5 * 1024 * 1024 // 5MB
    DownloadTimeoutMs       = 30_000
    ProgressUpdateIntervalMs = 100
    ProgressPersistIntervalMs = 1_000

    // Worker
    WorkerCheckIntervalMs      = 5_000
    StreamRecheckIntervalMs    = 30_000
    ProbeJitterMaxMs           = 30_000
    MaxConsecutiveProbeErrors   = 10
    StreamSegmentTimeoutMs     = 10 * 60_000
    StreamEndVerifyIntervalMs  = 5 * 60_000

    // Segment Downloader
    ParallelDownloads   = 6
    CatchupThreshold    = 10
    MaxSegmentRetries   = 10

    // Chat
    ChatDedupKeep           = 5_000
    ChatMaxConsecutiveErrors = 20
    ChatSurgeThreshold      = 30
    ChatSurgeWindowMs       = 15_000

    // DECAPI Monitor
    DecapiMinIntervalMs      = 15_000
    DecapiRequestStaggerMs   = 1_000
    DecapiDefaultRateLimit   = 60
    DecapiRequestTimeoutMs   = 15_000

    // Twitch Monitor
    TwitchCheckIntervalDefaultMs = 15_000
    TwitchCheckJitterMs          = 5_000
    TwitchRequestStaggerMs       = 500
    TwitchCheckMinIntervalMs     = 5_000

    // Default Port
    DefaultPort = 774
)
```

### YouTube Client Configurations

```go
var TVDowngradedClient = YouTubeClient{
    ClientName:    "TVHTML5",
    ClientVersion: "5.20260114",
    ClientID:      "7",
    UserAgent:     UserAgentTV,
}

var WebClient = YouTubeClient{
    ClientName:    "WEB",
    ClientVersion: "2.20260120.01.00",
    ClientID:      "1",
    UserAgent:     UserAgentWeb,
}

var WebCreatorClient = YouTubeClient{
    ClientName:    "WEB_CREATOR",
    ClientVersion: "1.20260120.01.00",
    ClientID:      "62",
    UserAgent:     UserAgentWeb,
}

var AndroidVRClient = YouTubeClient{
    ClientName:    "ANDROID_VR",
    ClientVersion: "1.71.26",
    ClientID:      "28",
    UserAgent:     UserAgentAndroidVR,
    AndroidSDK:    32,
    OSVersion:     "12L",
    DeviceMake:    "Oculus",
    DeviceModel:   "Quest 3",
}
```

### Twitch GQL Persisted Query Hashes

```go
var TwitchGQLHashes = map[string]string{
    "StreamMetadata":           "b57f9b910f8cd1a4659d894fe7550ccc81ec9052c01e438b290fd66a040b9b93",
    "ComscoreStreamingQuery":   "e1edae8122517d013405f237ffcc124515dc6ded82480a88daef69c83b53ac01",
    "VideoMetadata":            "45111672eea2e507f8ba44d101a61862f9c56b11dee09a15634cb75cb9b9084d",
    "VideoCommentsByOffsetOrCursor": "b70a3591ff0f4e0313d126c6a1502d79a1c02baebb288227c582044aa76adf6a",
}
```

### Thumbnail Quality Order

```go
var ThumbnailQualities = []string{
    "maxresdefault",
    "sddefault",
    "hqdefault",
    "mqdefault",
    "default",
}
```

### Format Selection Priority

```go
// Video codec priority (higher = better)
var VideoCodecPriority = map[string]int{
    "vp9.2": 5,
    "vp9":   4,
    "av01":  3,
    "avc1":  2,
    "h264":  1,
}

// Audio codec priority (higher = better)
var AudioCodecPriority = map[string]int{
    "opus":      4,
    "mp4a.40.5": 3,
    "mp4a.40.2": 2,
    "mp4a":      1,
}
```

Auto-selection: filter by `max_video_resolution` (max of width/height for portrait) → highest resolution → fps preference → codec score → lower bitrate → lower authLevel.

## Web Dashboard

The web frontend (`src/web/public/`) is vanilla JS with Shoelace components loaded from CDN. It requires NO build step. **Copy the entire `public/` directory as-is** — it connects to the WebSocket and REST API, which the Go server will provide with the same endpoints.

Key files:
- `index.html` — Shoelace dark theme, tab layout
- `moombox.css` — Dark theme CSS
- `app.js` — Main app: WebSocket, job rendering, API calls
- `modules/player.js` — Video player + chat overlay
- `modules/settings.js` — Config editor
- `modules/imports.js` — ZIP import
- `modules/setup.js` — First-run wizard

## TUI Architecture (for BubbleTea)

The TypeScript TUI uses Ink (React for CLI). Replace with BubbleTea:

### Layout
```
┌─────────────────────┬──────────────────────┐
│     TaskList         │     JobDetails       │  Top panel (70% when focused)
│  (virtual list)      │   (scrollable rows)  │
├─────────────────────┴──────────────────────┤
│                  LogViewer                   │  Bottom panel (25% default)
└─────────────────────────────────────────────┘
│                  StatusBar                   │  Fixed 1-row bottom bar
```

Tab cycles focus. Focused panel gets 70% height, unfocused gets 25%.

### Keyboard Controls
| Key | Action |
|-----|--------|
| Tab | Cycle focus: Tasks → Details → Logs |
| ↑/↓ | Navigate / scroll |
| Enter | Toggle archived section |
| A | Add Video dialog |
| C | Cancel job |
| R | Retry job |
| D | Delete (double-press confirm) |
| F | Cycle filter |
| T | Trim dialog |
| O | Open folder |
| W | Open web dashboard |
| ? | Help |
| Q | Quit |

### Mouse Support
The TS version uses VT mouse tracking escape sequences. BubbleTea has built-in mouse support via `tea.WithMouseAllMotion()` or `tea.WithMouseCellMotion()`.

## Error Handling

The TypeScript version has a custom error hierarchy. In Go, use typed errors:

```go
type MoomboxError struct {
    Code     string
    Message  string
    Expected bool  // User-facing vs developer error
    Context  map[string]interface{}
    Cause    error
}

func (e *MoomboxError) Error() string { return e.Message }
func (e *MoomboxError) Unwrap() error { return e.Cause }

// Specific error types
type YouTubeError struct{ MoomboxError }
type DownloadError struct{ MoomboxError; HTTPStatus int }
type NetworkError struct{ MoomboxError; HTTPStatus int; URL string }
type ConfigError struct{ MoomboxError }
type MuxingError struct{ MoomboxError; ExitCode int }
type AuthError struct{ MoomboxError }
```

## Implementation Order

Recommended order for the Go rewrite:

### Phase 1: Core Infrastructure
1. Config manager (TOML loading)
2. Logger (file rotation, pub/sub)
3. Database (JSON or bbolt)
4. HTTP client with retry

### Phase 2: YouTube Integration
5. Cookie parser (Netscape format)
6. YouTube auth (SAPISIDHASH generation)
7. Innertube player API (multi-client)
8. Goja integration + DOM shims
9. Cipher solver (signature decryption via Goja)
10. PO Token generation (BotGuard via Goja)

### Phase 3: Download Engine
11. Manifest parser (DASH + HLS)
12. Segment downloader (sequential + parallel)
13. Chat API + chat downloader
14. FFmpeg muxer wrapper
15. Download orchestrator + strategies
16. Job queue + worker

### Phase 4: Monitoring
17. Feed monitor (RSS)
18. DECAPI monitor
19. Twitch monitor (GQL)

### Phase 5: Web Server
20. HTTP server + API routes
21. WebSocket server
22. Static file serving (embed dashboard)
23. POT provider endpoints

### Phase 6: TUI
24. BubbleTea app with 3-panel layout
25. Job list + details + log viewer
26. Keyboard/mouse controls
27. Add video dialog / setup wizard

### Phase 7: Build & Package
28. `go build` → single binary
29. Embed static assets via `//go:embed`
30. Cross-platform testing

## Testing

The TypeScript version uses Vitest. For Go, use the standard `testing` package:
- Unit tests for cipher solver, manifest parser, format selector
- Integration tests for API endpoints (use `httptest.Server`)
- The TypeScript tests in `src/__tests__/` provide test cases you can port

## Key Behavioral Notes

1. **Atomic file writes**: Always write to `.tmp` then rename (segment files, chat, database)
2. **Batch DB updates**: Coalesce writes in 100ms windows to prevent disk thrashing
3. **Progress throttling**: WebSocket broadcasts throttled at 100ms per job (trailing edge)
4. **Cookie refresh**: Every 30 minutes, fetch youtube.com and update cookies from Set-Cookie headers
5. **Dedup**: Both RSS feed and DECAPI monitors check `history` set before creating jobs
6. **Graceful shutdown**: Stop consumers first, flush data, then stop infrastructure. 10s force-exit safety net.
7. **Stream end detection**: No new segments for 10 minutes → probe YouTube API to confirm → finish
8. **Catch-up mode**: When >10 segments behind, switch to parallel download (6 concurrent)
9. **Resume**: Segment downloaders save state periodically for crash recovery
10. **Jitter**: All polling intervals add random jitter to prevent thundering herd

## File Compatibility

The Go version should read/write the same `moombox.json` and `config.toml` formats, and the same `cookies.txt` (Netscape format). This allows users to migrate between versions without data loss.

The web dashboard (`public/` directory) should be embedded via `//go:embed` and served identically to the TypeScript version. The same WebSocket protocol ensures the frontend works without modification.

---

## Detailed API Routes

All routes below are under `/api/`. Error responses: `{ "error": "message" }`. Validation errors: `{ "error": "Validation failed", "details": { "field": ["messages"] } }`.

### Job Routes

| Method | Path | Rate Limit | Details |
|--------|------|-----------|---------|
| GET | `/jobs` | - | Query: `offset`, `limit`. No params = plain `[]Job`. With params = `{ data, pagination: { total, offset, limit, hasMore } }`. Excludes archived (finished > `hide_finished_age_days`). |
| GET | `/jobs/archived` | - | Same pagination. Archived jobs sorted by `updatedAt` descending. |
| GET | `/jobs/:id` | - | 404 if not found. Finished jobs: `Cache-Control: public, max-age=31536000, immutable`. Others: `no-cache, must-revalidate`. |
| GET | `/jobs/:id/video` | - | Range-request streaming. Path traversal guard. Content-Type by ext (`.webm`→`video/webm`, `.mkv`→`video/x-matroska`, else `video/mp4`). 206 for Range, 416 for invalid range. |
| GET | `/jobs/:id/chat` | - | Returns chat JSON file. Path traversal guard. 422 if corrupt. |
| GET | `/jobs/:id/logs` | - | Returns `[]string` from in-memory per-job log buffer. |
| GET | `/jobs/:id/trims` | - | Returns `{ trims: TrimRecord[] }`. |
| GET | `/formats/:videoId` | - | Validates `^[a-zA-Z0-9_-]{11}$`. Returns `{ videoId, title, channelName, lengthSeconds, streamStatus, videoFormats, audioFormats, bestItags }`. |
| POST | `/jobs` | 20/min | Body: `{ videoId, platform?, selectedVideoItag?, selectedAudioItag?, startTime?, endTime?, twitchType?, quality_preference? }`. 409 if duplicate. 201 with created job. |
| POST | `/jobs/:id/cancel` | - | Allowed: Downloading, Live, Upcoming, Muxing, COOKIES?. Sets status to Cancelled. |
| POST | `/jobs/:id/retry` | - | Allowed: Error, Cancelled, COOKIES?. Resets to Upcoming. |
| POST | `/jobs/:id/open-folder` | - | Loopback only (403 remote). Opens file explorer (platform-specific). |
| DELETE | `/jobs/:id` | - | Allowed: Finished, Error, Cancelled, COOKIES?. Removes from DB. |

### Trim Routes

| Method | Path | Details |
|--------|------|---------|
| POST | `/jobs/:id/trims` | Body: `{ startTime, endTime }` (seconds, duration >= 1s). Returns `{ trim: TrimRecord }`. Aborts on client disconnect. |
| DELETE | `/jobs/:id/trims/:trimId` | Deletes trim file + record. |

### Config Routes

| Method | Path | Details |
|--------|------|---------|
| GET | `/config` | Full config with `password_hash` stripped, `hasPassword: bool` added. |
| PUT | `/config` | Allowlisted keys only. Cannot set `network_access: "external"` without existing password. Hot-reloads log level and parallel downloads. |
| GET | `/status` | `{ status, uptime, timestamp, memory, cookieStatus, twitchAuthStatus, autoCookieReloginRequired, nextFeedCheck, nextDecapiCheck, nextTwitchCheck }`. |
| GET | `/logs` | Last 200 log lines from in-memory buffer. |
| POST | `/config/channels` | Upsert channel. Body: `{ id, name?, platform?, terms?, enabled?, quality_preference?, output_directory?, include_non_live_content? }`. |
| DELETE | `/config/channels/:id` | Remove channel. 404 if not found. |
| POST | `/restart` | Loopback only. Responds 200, restarts after 500ms. |

### Setup Routes

| Method | Path | Details |
|--------|------|---------|
| GET | `/setup/status` | `{ isFirstRun: bool }` — true if no config file. |
| POST | `/setup/complete` | Config + optional `password`. External access requires password >= 8 chars. |

### Cookie Routes

| Method | Path | Details |
|--------|------|---------|
| POST | `/cookies/recheck` | Full cookie validation cycle. Returns `{ success, cookieStatus, twitchAuthStatus, autoCookieReloginRequired }`. |
| POST | `/cookies/auto-setup/start` | Body: `{ platform?: "youtube"\|"twitch" }`. Launches browser. |
| POST | `/cookies/auto-setup/finish` | Extracts cookies from browser. Returns `{ success, authenticated, twitchAuthenticated }`. |
| POST | `/cookies/auto-setup/cancel` | Kills browser. |
| GET | `/cookies/auto-status` | `{ configured, setupInProgress, browser, lastRefresh, lastError, needsManualRelogin }`. |

### yt-dlp Plugin Routes

| Method | Path | Details |
|--------|------|---------|
| GET | `/ytdlp-plugin/status` | Plugin install status. |
| POST | `/ytdlp-plugin/install` | Body: `{ force?: bool }`. Returns `{ success, path?, alreadyInstalled? }`. |

### POT Provider Endpoints (root-mounted, NOT under /api)

| Method | Path | Auth | Rate Limit | Details |
|--------|------|------|-----------|---------|
| POST | `/get_pot` | Loopback | 10/min | Body: `{ content_binding?, bypass_cache? }`. Rejects deprecated `data_sync_id`/`visitor_data`. Returns PO token session. |
| POST | `/invalidate_caches` | Loopback | - | Clears all PO token caches. 204. |
| POST | `/invalidate_it` | Loopback | - | Clears integrity token caches. 204. |
| GET | `/ping` | None | - | `{ server_uptime, version: "1.0.0" }`. |
| GET | `/minter_cache` | None | - | Array of content binding keys in cache. |

### Import Route

| Method | Path | Rate Limit | Details |
|--------|------|-----------|---------|
| POST | `/import` | 5/min | Raw body (octet-stream), max 500MB. Zip bomb protection: max 1000 files, 2GB total, 100x ratio. Video ID extracted from `[{id}]` in filename. Headers: `X-Import-Title`, `X-Import-Channel`. 201 with job. |

### Rate Limiter Pattern

In-memory per-IP sliding window. Returns 429 with `{ error: "..." }` when exceeded.

---

## Twitch Pipeline (Separate from YouTube)

Twitch has a completely separate pipeline from YouTube. They share `SegmentDownloader`, `muxAndFinalize()`, database, config, and notifications — but the stream processing, download strategy, and chat system are entirely different.

### Job Creation

| Aspect | YouTube | Twitch |
|--------|---------|--------|
| Job ID format | YouTube video ID | `tw_{streamId}` (live) or `tw_v{vodId}` (VOD) |
| Metadata source | Innertube `/player` API | Twitch GQL API |
| Initial status | Set by StreamProcessor | `"Live"` immediately (stream confirmed live at creation) |
| Thumbnail | `https://i.ytimg.com/vi/{id}/maxresdefault.jpg` | Twitch preview CDN URL |
| Extra fields | None | `platform: "twitch"`, `channelAvatarUrl`, `twitchCategory`, `twitchQuality` |

### Stream Processing

**YouTube path:** Probe via ANDROID_VR → wait loop if Upcoming → classify status → start early chat

**Twitch live path:** Verify still live via GQL → fetch HLS variants → select best variant → create IRC chat downloader

**Twitch VOD path:** Fetch VOD info via GQL → fetch VOD HLS playlist → select best variant → create VOD chat downloader (GQL pagination)

### Download Execution

**YouTube:** Separate video + audio `SegmentDownloader` instances (DASH), or single HLS downloader, or parallel VOD download. Requires signature decryption and n-parameter manipulation.

**Twitch:** Single `SegmentDownloader` in HLS mode. No signature decryption needed — Twitch HLS URLs are plain. MPEG-TS is pre-muxed (video + audio together), so `audioPath` is `null` in muxFinalize. Stream end detection: `getStreamInfo()` returns null when offline.

### Variant Selection (Twitch HLS)

Parse `#EXT-X-STREAM-INF:` from master playlist:
- `BANDWIDTH=(\d+)`, `RESOLUTION=(\d+)x(\d+)`, `FRAME-RATE=([\d.]+)`, `VIDEO="([^"]+)"`
- `isSource = videoGroup == "chunked" || name.includes("source")`
- Preference: channel's `quality_preference`, then `max_video_resolution`, then prefer source, then highest bandwidth

### Twitch GQL Queries

**Stream info** — batched request with two operations:
```json
[
  {
    "operationName": "StreamMetadata",
    "variables": { "channelLogin": "username", "includeIsDJ": true },
    "extensions": { "persistedQuery": { "version": 1, "sha256Hash": "b57f9b910f..." } }
  },
  {
    "operationName": "ComscoreStreamingQuery",
    "variables": { "channel": "username", "clipSlug": "", "isClip": false, "isLive": true, "isVodOrCollection": false, "vodID": "" },
    "extensions": { "persistedQuery": { "version": 1, "sha256Hash": "e1edae8122..." } }
  }
]
```

Headers: `Client-ID: kimne78kx3ncx6brgo4mv6wki5h1ko`, `Content-Type: application/json`, optional `Authorization: OAuth {token}`.

**HLS access token:**
```json
{
  "query": "{ streamPlaybackAccessToken(channelName: \"name\", params: { platform: \"web\", playerBackend: \"mediaplayer\", playerType: \"site\" }) { value signature } }"
}
```

**Master playlist URL:** `https://usher.ttvnw.net/api/channel/hls/{login}.m3u8?allow_source=true&allow_audio_only=true&allow_spectre=true&fast_bread=true&p={random}&player=twitchweb&playlist_include_framerate=true&sig={signature}&token={value}&type=any`

**VOD info** — VideoMetadata persisted query (sha256: `45111672eea2...`):
```json
{
  "operationName": "VideoMetadata",
  "variables": { "channelLogin": "", "videoID": "vodId" },
  "extensions": { "persistedQuery": { "version": 1, "sha256Hash": "45111672eea2..." } }
}
```

**VOD chat** — VideoCommentsByOffsetOrCursor (sha256: `b70a3591ff0f...`):
```json
{
  "operationName": "VideoCommentsByOffsetOrCursor",
  "variables": { "videoID": "vodId", "contentOffsetSeconds": 0 },
  "extensions": { "persistedQuery": { "version": 1, "sha256Hash": "b70a3591ff0f..." } }
}
```
Always paginate by `contentOffsetSeconds` (not cursor — cursor triggers integrity check).

### Chat System Differences

| | YouTube | Twitch Live | Twitch VOD |
|--|---------|-------------|------------|
| Protocol | HTTP polling (Innertube) | IRC WebSocket (`irc-ws.chat.twitch.tv`) | GQL pagination |
| Auth | SAPISIDHASH cookies | `OAuth {token}` from cookies | `OAuth {token}` |
| Time reference | `offsetMs` from YouTube's `videoOffsetTimeMsec` | `offsetMs = tmi-sent-ts - recordingStartMs` | `offsetMs = contentOffsetSeconds * 1000` |
| IRC commands | N/A | PRIVMSG (chat/bits), USERNOTICE (sub/resub/subgift/raid) | N/A |
| Emotes | Inline in message parts | Post-download resolution via BTTV/FFZ/7TV APIs | Same |
| Output format | `ChatData` | `TwitchChatData` (has `platform: "twitch"`, `streamId`, `recordingStartTime`) | Same |
| Progress events | Every message | Every 100 messages | Every 500 messages |

### Twitch Emote APIs (for chat enrichment)

| Service | Global URL | Channel URL |
|---------|-----------|-------------|
| BTTV | `https://api.betterttv.net/3/cached/emotes/global` | `https://api.betterttv.net/3/cached/users/twitch/{channelId}` |
| FFZ | `https://api.frankerfacez.com/v1/set/global` | `https://api.frankerfacez.com/v1/room/id/{channelId}` |
| 7TV | `https://7tv.io/v3/emote-sets/global` | `https://7tv.io/v3/users/twitch/{channelId}` |

---

## Authentication Details

### SAPISIDHASH Algorithm (YouTube)

The `Authorization` header for YouTube API requests:

```
SAPISIDHASH {timestamp}_{sha1("{timestamp} {SAPISID} {origin}")}
```

Multiple hashes for different cookie variants, space-separated:
```
SAPISIDHASH {ts}_{sha1("{ts} {SAPISID} {origin}")} SAPISID1PHASH {ts}_{sha1("{ts} {__Secure-1PAPISID} {origin}")} SAPISID3PHASH {ts}_{sha1("{ts} {__Secure-3PAPISID} {origin}")}
```

Where:
- `timestamp` = Unix seconds (integer)
- `origin` = `"https://www.youtube.com"`
- `sha1` = lowercase hex SHA-1
- If `SAPISID` missing, use `__Secure-3PAPISID` as fallback for first hash
- Omit any hash whose cookie is absent

### Full YouTube API Headers

```go
headers := map[string]string{
    "Content-Type":             "application/json",
    "User-Agent":               client.UserAgent,
    "X-YouTube-Client-Name":    client.ClientID,
    "X-YouTube-Client-Version": client.ClientVersion,
    "Origin":                   "https://www.youtube.com",
}
if cookieHeader != "" {
    headers["Cookie"] = cookieHeader
}
if visitorData != "" {
    headers["X-Goog-Visitor-Id"] = visitorData
}
if delegatedSessionID != "" {
    headers["X-Goog-PageId"] = delegatedSessionID
}
if delegatedSessionID != "" || sessionIndex != nil {
    idx := 0
    if sessionIndex != nil { idx = *sessionIndex }
    headers["X-Goog-AuthUser"] = strconv.Itoa(idx)
}
if authHeader != "" {
    headers["Authorization"] = authHeader
    headers["X-Origin"] = "https://www.youtube.com"
}
if hasAuthCookies {
    headers["X-Youtube-Bootstrap-Logged-In"] = "true"
}
```

### Netscape Cookie File Format

Tab-delimited, 7 fields per line:
```
{domain}\t{subdomain_flag}\t{path}\t{secure}\t{expiry}\t{name}\t{value}
```

- Skip `#` comment lines EXCEPT `#HttpOnly_` prefix (those are data — strip prefix for domain)
- `subdomain_flag`: `TRUE` if domain starts with `.`, else `FALSE`
- `secure`: `TRUE`/`FALSE`
- `expiry`: Unix timestamp seconds (0 for session cookies)

Essential YouTube cookies: `SAPISID`, `__Secure-1PAPISID`, `__Secure-3PAPISID`, `SID`, `HSID`, `SSID`, `APISID`, `__Secure-1PSID`, `__Secure-3PSID`, `__Secure-1PSIDTS`, `__Secure-3PSIDTS`, `__Secure-1PSIDCC`, `__Secure-3PSIDCC`, `LOGIN_INFO`, `VISITOR_INFO1_LIVE`, `VISITOR_PRIVACY_METADATA`, `YSC`, `__Secure-ROLLOUT_TOKEN`, `CONSENT`, `PREF`

Essential Twitch cookies: `auth-token`, `twilight-user`, `login`, `name`

`hasAuthCookies()`: true if `SAPISID` (or `__Secure-3PAPISID`) AND `LOGIN_INFO` present.

---

## Watch Page Parsing

Fetch `https://www.youtube.com/watch?v={videoId}` with browser-like headers.

### Extraction Regexes (applied to raw HTML)

```
playerUrl:            /"(?:jsUrl|PLAYER_JS_URL)":"([^"]+)"/
                      (prepend "https://www.youtube.com" if starts with "/")
visitorData:          /"visitorData":"([^"]+)"/
sessionIndex:         /"SESSION_INDEX":"?(\d+)"?/
delegatedSessionId:   /"DELEGATED_SESSION_ID":"([^"]+)"/
dataSyncId:           /"datasyncId":"([^"]+)"/
isLoggedIn:           html.includes('"LOGGED_IN":true') || html.includes('"isLoggedIn":true')
```

### ytInitialPlayerResponse

Regex: `/var ytInitialPlayerResponse = ({.+?});/s` (dotAll mode)

Parse JSON, extract from `videoDetails`: `title`, `author`, `channelId`, `shortDescription`, `thumbnail.thumbnails[-1].url` (last = highest res).

---

## Cookie Refresh Service (30-Minute Cycle)

1. Reload cookies from file
2. Check YouTube auth: POST to `https://www.youtube.com/youtubei/v1/guide?prettyPrint=false` with auth headers → look for `logged_in: "1"` in `responseContext.serviceTrackingParams` or `mainAppWebResponseContext.loggedIn === true`
3. Check Twitch auth: GET `https://id.twitch.tv/oauth2/validate` with `Authorization: OAuth {token}` → HTTP 200 = valid
4. If YouTube authenticated: refresh session — POST guide endpoint, parse `Set-Cookie` headers, update cookie file with new values/expiries
5. If `auto_cookies.enabled` and auth failed: trigger headless browser refresh
6. Re-validate after auto-refresh
7. Flag platforms still failing as needing manual re-login

### Session Cookie Refresh

POST `https://www.youtube.com/youtubei/v1/guide?prettyPrint=false` with full auth headers.
Parse `Set-Cookie` response headers. For each: extract `NAME=VALUE`, `Domain=...`, `Expires=...`, `Max-Age=...`.
Only process cookies for `.youtube.com` or `.google.com` domains.
Update matching lines in Netscape file (tab fields: index 4 = expiry, index 6 = value).
Append new cookies not already in file.

---

## Auto-Cookie Service (Browser-Based Extraction)

### Browser Priority

`detectBrowser()` returns first found: Firefox > Edge > Chrome

### Firefox Flow

1. Write `user.js` to profile dir (suppresses first-run dialogs)
2. `spawn(firefox, ["-profile", profileDir, "-no-remote", loginUrl])`
3. User logs in manually
4. **Finish:** Close Firefox gracefully (Windows: `taskkill /T`, Unix: `kill -TERM`), wait up to 8s, force kill if needed
5. Read `{profileDir}/cookies.sqlite` using WebAssembly SQLite
6. SQL: `SELECT name, value, host, path, expiry, isHttpOnly, isSecure FROM moz_cookies`
7. Convert to Netscape format, write to `cookies.txt`

### Chromium Flow

1. Find free port (bind TCP `:0`, get port, close)
2. Clean lock files: `lockfile`, `SingletonLock`, `SingletonSocket`, `SingletonCookie`
3. Launch: `spawn(browser, ["--user-data-dir=...", "--no-first-run", "--disable-blink-features=AutomationControlled", "--remote-debugging-port={port}", url])`
4. Poll `http://127.0.0.1:{port}/json/version` every 500ms until 200 (timeout 15s)
5. Extract via CDP: `Storage.getCookies` or `Network.getAllCookies`
6. Convert to Netscape format

### Headless Refresh (automatic, no user interaction)

- Firefox: `firefox --screenshot {tempFile} -no-remote -profile {dir} {url}` (two sequential runs: YouTube then Twitch)
- Chromium: Launch `--headless=new`, navigate via CDP `Page.navigate`, extract cookies

---

## Notification System (Discord Webhooks)

### Config

```toml
[[notifications]]
url = "discord://WEBHOOK_ID/WEBHOOK_TOKEN"  # or full Discord webhook URL
events = ["download_start", "download_finish", "error", "cancelled"]  # optional filter
```

### Notification Types

| Type | Color | Hex |
|------|-------|-----|
| INFO | Blue | 0x3498db |
| SUCCESS | Green | 0x2ecc71 |
| WARNING | Yellow | 0xf1c40f |
| ERROR | Red | 0xe74c3c |
| DOWNLOAD | Teal | 0x1abc9c |
| MUXING | Purple | 0x9b59b6 |
| CANCELLED | Orange | 0xe67e22 |

### Discord Webhook Payload

```json
{
  "embeds": [{
    "title": "...",
    "description": "...",
    "color": 3447003,
    "url": "https://...",
    "fields": [{ "name": "...", "value": "...", "inline": true }],
    "thumbnail": { "url": "..." },
    "image": { "url": "..." },
    "footer": { "text": "Moombox" },
    "timestamp": "2026-02-22T00:00:00.000Z"
  }]
}
```

URL format handling:
- `discord://ID/TOKEN` → convert to `https://discord.com/api/webhooks/ID/TOKEN`
- `https://*.discord.com/api/webhooks/...` → use as-is

### Events Emitted

- `video_added` — Job manually added
- `download_start` — Download begins
- `download_finish` — Job reaches Finished status
- `error` — Job enters Error state
- `cancelled` — Job cancelled

---

## Format Selection Algorithm (YouTube)

### Codec Scores

**Video:** `vp9.2`=5, `vp9`=4, `av01`=3, `avc1`=2, `h264`=1, other=0

**Audio:** `opus`=4, `mp4a.40.5`=3, `mp4a.40.2`=2, `mp4a`=1, other=0

Codec extracted from `mimeType` via regex: `codecs="?([^",]+)` (first capture group).

### Video Auto-Selection

For each format with `mimeType.includes("video") && url != ""`:
1. Skip if `max(width, height) > max_video_resolution`
2. Higher `max(width, height)` wins
3. Tie: if `prefer_60fps` (default true) → higher fps wins, else lower fps wins
4. Tie: higher codec score wins
5. Tie: lower bitrate wins (better compression at same quality)
6. Tie: lower `authLevel` wins (prefer unauthenticated formats)

### Audio Auto-Selection

For each format with `mimeType.includes("audio") && url != ""`:
1. Higher codec score wins
2. Tie: higher bitrate wins
3. Tie: lower `authLevel` wins

### Manual Override

- `selectedVideoItag == -1`: explicitly no video (audio-only download)
- `selectedAudioItag == -1`: explicitly no audio (video-only download)
- Otherwise: look up itag in formats; if not found, fall back to auto-selection

---

## ZIP Import Details

### Zip Bomb Protection

- Max 1,000 files
- Max 2GB total uncompressed
- Max 100x compression ratio per file

### File Detection

- **Video:** First file matching `.mp4`, `.mkv`, `.webm`, `.ts`
- **Chat:** First file ending `.chat.json`, or fallback: first `.json` < 10MB where `messages[0].offsetMs` is a number
- **Video ID:** Regex `\[([a-zA-Z0-9_-]{11})\]` from filename, else `imp_{4 random hex bytes}`
- **Metadata override:** `X-Import-Title`, `X-Import-Channel` headers

---

## Term Matching (for Monitors)

Terms define which streams to auto-record. Used by both YouTube and Twitch monitors.

### Config Format

```toml
[[channels]]
id = "UCxxxxxxx"
terms = "term1, term2, /regex/i"  # comma-separated string

# OR structured (per-term settings, less common):
[channels.terms]
"term1" = "include"
"term2" = "include"
```

### Matching Logic

- If no terms configured: always matches (record everything)
- Each term checked against video title (YouTube) or title + game category (Twitch)
- Terms starting/ending with `/`: treated as regex (e.g., `/(?i)karaoke/`)
- Plain terms: case-insensitive substring match with diacritic normalization (`fuzzyMatch`)
- `fuzzyMatch`: NFD normalize → strip combining diacritics → case fold → includes check

### `processYouTubeVideo()` Additional Logic

- Checks RSS `<yt:videoId>` and `<title>` from feed
- `num_desc_lookbehind`: how many recent feed entries to check (default: `max_feed_items`)
- `include_non_live_content`: if false (default), only records if stream status is live/upcoming/post_live_dvr
