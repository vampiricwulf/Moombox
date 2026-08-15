# Platform Services

## Scope

This document provides a comprehensive, implementation-level reference for every external platform integration in Moombox: YouTube (Innertube API, live chat, cipher decryption, BotGuard/PO tokens) and Twitch (GQL API, HLS streaming, IRC chat, VOD chat, third-party emotes). It covers API protocols, authentication mechanisms, caching strategies, error handling, retry policies, and the Goja JavaScript runtime environment that enables BotGuard and cipher operations. This is the authoritative reference for understanding how Moombox communicates with external services.

## Rules and Constraints

These are hard rules that govern all platform service integrations:

- **YouTube uses multi-client Innertube fallback.** The authenticated request order is: TV_DOWNGRADED (best format coverage) then WEB (DASH manifest) then WEB_CREATOR (member content) then ANDROID_VR (last-resort public). The public request order drops WEB_CREATOR. TV_DOWNGRADED and WEB are always tried; WEB_CREATOR and ANDROID_VR are conditional fallbacks (tried only when earlier clients return members-only, login-required, or no formats).
- **Format priority is lexicographic across five dimensions.** For video: resolution (higher wins) > FPS (prefer60fps setting) > codec score (higher wins) > bitrate (lower wins, indicating better compression) > auth level (lower preferred). For audio: codec score (higher wins) > bitrate (higher wins) > auth level (lower preferred). This ordering is absolute and implemented in `SelectBestFormats`.
- **Twitch uses GQL API with SHA256 persisted query hashing (version 1).** All structured queries (stream metadata, video metadata, VOD comments) use persisted queries with hardcoded SHA256 hashes. Access token queries use inline GraphQL. The Client-ID header (`kimne78kx3ncx6brgo4mv6wki5h1ko`) is required on every GQL request.
- **BotGuard has a triple cache with auto-eviction.** Session cache (6-hour TTL, keyed by contentBinding), minter cache (dynamic TTL from Google's API, keyed by contentBinding, auto-evicted via `time.AfterFunc`), and inflight dedup (concurrent requests for the same key wait on a shared channel). Minters hold live Goja VMs that must be explicitly shut down on eviction.
- **Cipher has a 3-VM LRU with AST + regex fallback.** Memory cache holds at most 3 compiled solver VMs keyed by SHA256 of the player URL. Disk cache (`~/.cache/yt-cipher/player_cache/`) has a 14-day TTL. Compilation is mutex-serialized to prevent thundering herd. The Goja VM inside each Solvers struct is mutex-protected because Goja is not thread-safe.
- **All API keys and client configurations live in `internal/constants/`.** No API keys, client IDs, hashes, or endpoint URLs are hardcoded outside that package. Any new platform integration must add its constants there.
- **Chat dedup uses recent IDs with deterministic eviction.** Both YouTube (`internal/chat/`) and Twitch (`internal/twitch/chat.go`) maintain a map of seen message IDs plus an ordered slice tracking insertion order. YouTube prunes aggressively: when the set exceeds 5000 entries, the oldest entries are removed immediately. Twitch uses lazy pruning: the set grows to 10,000 entries (2x the 5000 constant) before culling back to 5000. Both match JavaScript `Set` insertion-order semantics from the original TypeScript codebase.
- **All HTTP responses are size-limited.** GQL and API responses are capped at 5 MB via `io.LimitReader`. Challenge and integrity token responses are capped at 1 MB. This prevents unbounded memory allocation from malformed responses.

---

## YouTube Service (`internal/youtube/`)

### Architecture

The YouTube integration is a three-layer facade:

1. **Service** (`service.go`) -- Top-level API. Wraps PlayerAPI, Auth, and FormatSelector. Holds cached visitor data with RWMutex protection. Visitor data is sticky-with-TTL: writes are accepted only when no value is cached or the cached value is older than `visitorDataTTL` (6 h). Sticky semantics keep the POT session cache hitting across the per-30s quality probe; the TTL acts as a safety net for long-running 24/7 sessions. `InvalidateVisitorData()` forces an immediate refresh after a downstream 403 burst suggesting POT expiry. Provides `GetVideoInfo`, `ProbeVideoStatus`, `GetFormats`, and cipher decryption pass-throughs.
2. **PlayerAPI** (`player_api.go`) -- Innertube protocol handler. Makes HTTP POST requests to the player endpoint, manages multi-client fallback, parses responses, decrypts signatures and n-parameters.
3. **Auth** (`auth.go`) -- Cookie-based authentication. Generates SAPISIDHASH Authorization headers, manages YouTube session headers, and syncs cookies from the CookieJar.

Additionally:
- **FormatSelector** (`format_selector.go`) -- Pure function that selects best video/audio formats from a pool.
- **WatchPage** (`watch_page.go`) -- Fetches and regex-parses YouTube watch pages to extract ytcfg configuration.
- **Types** (`types.go`) -- All data structures: `VideoInfo`, `Format`, `YtcfgData`, `StreamStatus`, `PlayabilityError`, auth level constants.

### Authentication System

#### Cookie-Based OAuth

YouTube authentication relies entirely on cookies. The `Auth` struct wraps a `CookieJar` and provides:

- **SAPISIDHASH Authorization header**: Generated by `CookieJar.GenerateAuthorizationHeader("https://www.youtube.com")`. This is a hash of the SAPISID cookie value combined with the origin URL and a timestamp. It proves cookie ownership without transmitting the raw cookie.
- **Cookie header**: The full cookie string is attached to every authenticated request via the `Cookie` HTTP header.

#### Session Headers

When a watch page is fetched, the following values are regex-extracted from the HTML and included in subsequent API requests:

| Header | Source | Purpose |
|--------|--------|---------|
| `X-Goog-Visitor-Id` | `visitorData` or `visitor_data` key in page HTML | Identifies the browsing session. Required for ANDROID_VR client requests. |
| `X-Goog-PageId` | `DELEGATED_SESSION_ID` in page HTML | Identifies the delegated (brand) account session. |
| `X-Goog-AuthUser` | `SESSION_INDEX` in page HTML | Numeric index for multi-account support (0 = primary). |
| `X-Youtube-Bootstrap-Logged-In` | Set to `"true"` when auth cookies are present | Signals to the API that the request is authenticated. |
| `Authorization` | SAPISIDHASH hash | OAuth proof of cookie ownership. |
| `X-Origin` | `"https://www.youtube.com"` | Sent alongside Authorization header for CORS/origin validation. |

#### Auth Levels

Formats collected from different clients are tagged with an auth level. When deduplicating formats by itag, the format with the **lowest** auth level wins (lower = less privileged = more widely accessible URL):

| Level | Constant | Client | Description |
|-------|----------|--------|-------------|
| 0 | `AuthLevelAndroidVR` | ANDROID_VR | Public, no cookies required. Provides direct URLs without cipher. |
| 1 | `AuthLevelWatchPage` | Watch page embed | Extracted from ytInitialPlayerResponse in the watch page HTML. |
| 2 | `AuthLevelTVPublic` | TV_DOWNGRADED (no cookies) | Public TV client request. |
| 3 | `AuthLevelTVAuth` | TV_DOWNGRADED (with cookies) | Authenticated TV client request. |
| 4 | `AuthLevelWeb` | WEB | Standard web client. Provides DASH manifest URLs. |
| 5 | `AuthLevelWebCreator` | WEB_CREATOR | Creator Studio client. Access to member-only content. |

### PlayerAPI Multi-Client Strategy

#### Endpoint

All Innertube player requests go to:

```
POST https://www.youtube.com/youtubei/v1/player?key={apiKey}
```

The API key defaults to `AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8` and can be overridden by extracting `INNERTUBE_API_KEY` from the YouTube homepage.

#### Request Structure

Every player request includes this JSON body structure:

```json
{
  "context": {
    "client": {
      "clientName": "<CLIENT_NAME>",
      "clientVersion": "<CLIENT_VERSION>",
      "hl": "en",
      "visitorData": "<optional>"
    }
  },
  "videoId": "<VIDEO_ID>",
  "contentCheckOk": true,
  "racyCheckOk": true,
  "playbackContext": {
    "contentPlaybackContext": {
      "html5Preference": "HTML5_PREF_WANTS",
      "signatureTimestamp": "<optional STS integer>"
    }
  }
}
```

The `signatureTimestamp` (STS) is extracted from the player JavaScript file and is required for cipher-protected formats. Without it, YouTube returns formats with encrypted signatures that cannot be decrypted.

#### Client Configurations

All client configs are defined in `internal/constants/constants.go`:

| Client | ClientName | ClientID | ClientVersion | User-Agent | Primary Use |
|--------|-----------|----------|---------------|------------|-------------|
| TV_DOWNGRADED | `TVHTML5` | `7` | `5.20260114` | TV Cobalt | Best format coverage. Primary client for both auth and public paths. |
| WEB | `WEB` | `1` | `2.20260120.01.00` | Chrome desktop | DASH manifest URLs. Always tried alongside TV to get manifest. |
| WEB_CREATOR | `WEB_CREATOR` | `62` | `1.20260120.01.00` | Chrome desktop | Member-only content. Fallback when TV returns `members_only` or `login_required`. |
| ANDROID_VR | `ANDROID_VR` | `28` | `1.71.26` | Oculus Quest VR | Last resort. Provides direct URLs without cipher for public content. Does not use cookies. |

#### Authenticated Fallback Flow

`GetVideoInfoAuthenticated` executes the following sequence:

1. **Fetch watch page** -- GET `https://www.youtube.com/watch?v={videoID}` with cookies. Extracts `YtcfgData` (playerURL, visitorData, sessionIndex, delegatedSessionID) and `ytInitialPlayerResponse`.
2. **Parse watch page response** -- If `ytInitialPlayerResponse` was found (tried against 3 regex patterns), parse it as a player response. Collect formats with `AuthLevelWatchPage`.
3. **Extract STS** -- If a playerURL was found and a cipher solver is available, extract the `signatureTimestamp` from the player JavaScript.
4. **Try TV_DOWNGRADED** -- POST to Innertube with TV client, STS, and auth headers. Collect formats with `AuthLevelTVAuth`. If HTTP error occurs, log warning and continue (do not return).
5. **Try WEB** -- POST to Innertube with WEB client. Collect formats with `AuthLevelWeb`. If WEB returns a DASH manifest URL and TV did not, adopt it.
6. **Evaluate TV result** -- If TV returned `members_only`, `login_required`, or zero formats:
   a. **Try WEB_CREATOR** -- POST with WEB_CREATOR client. Collect formats with `AuthLevelWebCreator`.
   b. **If WEB_CREATOR also fails** (and the error is not `members_only`): **Try ANDROID_VR** -- POST without cookies but with visitorData. Collect formats with `AuthLevelAndroidVR`.
   c. **If all API clients fail**: Fall back to the watch page player response if available.
7. **ANDROID_VR DASH-only enrichment** -- If, after TV+WEB, no client returned a `DashManifestURL` and the stream is live or upcoming AND not members-only / age-restricted / login-required, fetch ANDROID_VR (cookieless). On success, adopt its `DashManifestURL` and merge its formats into the pool with auth-level dedup. This is a workaround for the YouTube account-based experiment that strips `dashManifestUrl` from cookied clients (yt-dlp issue #15274).
8. **Stream classification override** -- If TV says `not_a_stream` but the watch page disagrees, override the stream status while keeping TV's formats if they are adequate (have both video and audio).
9. **Merge metadata** -- Fill in missing fields (title, channel, description, thumbnails, timestamps) from the watch page response.
10. **Deduplicate formats** -- Across all collected format pools, deduplicate by itag. When the same itag appears from multiple clients, keep the one with the lowest auth level.

#### Public Fallback Flow

`GetVideoInfoPublic` follows the same pattern but without cookies:

1. Fetch watch page (no cookies) and extract ytcfg + player response.
2. Try TV_DOWNGRADED (public, with STS).
3. If TV fails or returns inadequate formats, try ANDROID_VR.
4. Apply the same DASH-only ANDROID_VR enrichment as the authenticated path when TV returned no `DashManifestURL` for a live/upcoming stream.
5. Fall back to watch page response.

#### Retry Policy

Both `fetchWithClient` and `fetchWithAndroidVR` implement identical retry logic:

- **Max attempts**: 4 (1 initial + 3 retries)
- **Retryable errors**: HTTP 5xx, HTTP 429 (rate limited), or network errors
- **Non-retryable errors**: Any other HTTP status (e.g., 403, 404) returns immediately
- **Backoff**: Exponential, factor 2, starting at 1 second: 1s, 2s, 4s
- **Context-aware**: Checks `ctx.Err()` before each retry. Uses `utils.Sleep` which respects cancellation.

#### ANDROID_VR Specifics

The ANDROID_VR client has its own dedicated method (`fetchWithAndroidVR`) because it:
- Never sends cookies or auth headers
- Always sends `X-Goog-Visitor-Id` if visitorData is available
- Uses the Oculus Quest VR user agent
- Sets `androidSdkVersion: 32`, `osVersion: "12L"`, `deviceMake: "Oculus"`, `deviceModel: "Quest 3"` in the client context
- Does not pass STS (no cipher support needed -- returns direct URLs)

### Channel Membership Tab

`FetchMembershipVideos` in `channel_membership.go` performs an authenticated GET of `https://www.youtube.com/channel/{id}/membership` (with the current YouTube cookies) and returns the members-only videos listed there — the only discovery source for members-only streams, which RSS/DECAPI never expose. It returns `(nil, nil)` when discovery isn't applicable: no auth cookies, or the account isn't a member (the page then falls back to a public tab with no selected membership tab).

Parsing (`parseMembershipTab`):
- Extracts `ytInitialData` via a brace-depth scan that respects string literals (`extractYtInitialData`) — robust on the megabyte-scale channel payload where a non-greedy regex under/over-matches. Handles both `var ytInitialData = {…}` and `window["ytInitialData"] = {…}` forms.
- Locates the membership tab by the stable `tabIdentifier` `TAB_ID_SPONSORSHIPS` (YouTube localizes the visible title), and only when that tab is the SELECTED one — a non-member's fallback page reports `(nil, false)` and is never deep-parsed. The large tab body stays a `json.RawMessage` until then, so the common non-member case is near-zero allocation.
- Walks the selected tab for video IDs across both the current `lockupViewModel` (`contentId`) and classic `videoRenderer`/`gridVideoRenderer` layouts (YouTube A/B-serves both), deduping by video ID.
- Estimates each item's recency (`itemAge`): a live badge or an item with no recognizable timestamp yields Age 0 ("now"), while a "Streamed N &lt;unit&gt; ago" text marks a past VOD ranked by that age. The monitor seeds the feed-history store's `published` estimate from this Age — a dated item is stored `coarse` at (now − Age), an ageless item is stored `assumed` at the current cycle time — so live/upcoming members items always land inside the archive window and get probed. Keying on the ABSENCE of a past-time signal (rather than the presence of a live badge) keeps live/upcoming catching robust to YouTube's badge DOM churn.

### Watch Page Parsing

`FetchWatchPage` in `watch_page.go` performs a GET request to `https://www.youtube.com/watch?v={videoID}` and extracts:

| Field | Regex | Purpose |
|-------|-------|---------|
| `playerURL` | `"(?:jsUrl\|PLAYER_JS_URL)":"([^"]+)"` | URL of the player JavaScript file. Required for cipher decryption. Prefixed with `https://www.youtube.com` if relative. |
| `visitorData` | `"visitorData":"([^"]+)"` | Session identifier for API requests. |
| `sessionIndex` | `"SESSION_INDEX":"?(\d+)"?` | Multi-account index. |
| `delegatedSessionID` | `"DELEGATED_SESSION_ID":"([^"]+)"` | Brand account session ID. |
| `dataSyncID` | `"datasyncId":"([^"]+)"` | Data sync identifier. |
| `ytInitialPlayerResponse` | Three patterns (see below) | Inline player API response embedded in the page. |

The `ytInitialPlayerResponse` is extracted using three regex patterns tried in order:
1. `var ytInitialPlayerResponse\s*=\s*({.+?});`
2. `window["ytInitialPlayerResponse"]\s*=\s*({.+?});`
3. `ytInitialPlayerResponse\s*=\s*({.+?});`

When found, it is JSON-parsed and the embedded `videoDetails` fields (title, author, channelId, description, thumbnail) are stored in `YtcfgData` for use as metadata fallbacks.

Logged-in state is detected by checking for `"LOGGED_IN":true` or `"isLoggedIn":true` in the HTML.

### Format Selector Algorithm

`SelectBestFormats` in `format_selector.go` implements a single-pass selection algorithm:

#### Video Selection

For each video format (identified by `mimeType` containing "video") with a non-empty URL:

1. **Resolution gate**: Skip if `MaxDimension()` (which is `max(width, height)`) exceeds `maxResolution` config setting.
2. **Resolution comparison**: Higher `MaxDimension` wins.
3. **FPS tiebreaker** (same resolution): If `prefer60fps` is true, higher FPS wins. If false, lower FPS wins.
4. **Codec score tiebreaker** (same resolution, same FPS): Higher score wins. Scores are assigned by regex pattern matching against the codec string extracted from the `mimeType` field:

| Pattern | Score |
|---------|-------|
| `^vp9\.2` (HDR) | 5 |
| `^vp9` or `^vp09` | 4 |
| `^av01` (AV1) | 3 |
| `^avc1` (H.264) | 2 |
| `^h264` | 1 |

5. **Bitrate tiebreaker** (same resolution, FPS, codec): **Lower** bitrate wins (indicates better compression at the same quality).
6. **Auth level tiebreaker** (same everything): Lower auth level wins (more accessible URL).

#### Audio Selection

For each audio format (identified by `mimeType` containing "audio") with a non-empty URL:

1. **Codec score**: Higher wins. Scores:

| Pattern | Score |
|---------|-------|
| `^opus` | 4 |
| `^mp4a\.40\.5` (HE-AAC) | 3 |
| `^mp4a\.40\.2` (AAC-LC) | 2 |
| `^mp4a` (generic AAC) | 1 |

2. **Bitrate tiebreaker**: **Higher** bitrate wins (audio quality scales with bitrate).
3. **Auth level tiebreaker**: Lower auth level wins.

#### Manual Override

`SelectWithOptions` allows manual itag selection. If a `videoItag` or `audioItag` is provided:
- Value `-1` means "skip this track entirely" (e.g., download audio only).
- Any other value selects that specific itag if found in the format pool.
- Unresolved tracks fall back to auto-selection.

### Stream Status Classification

The `classifyStream` function determines the stream's lifecycle state from multiple signals in the player response:

| Condition | Result |
|-----------|--------|
| `playabilityStatus.status == "LIVE_STREAM_OFFLINE"` | `UPCOMING` |
| `playabilityStatus.status == "UNPLAYABLE"` and reason contains "live event will begin" | `UPCOMING` |
| Premiere detected (has scheduled start, not live content, reason contains "premiere" or `isUpcoming=true`) and not live | `UPCOMING` |
| `videoDetails.isLive == true` OR `liveBroadcastDetails.isLiveNow == true` | `LIVE` |
| No `liveBroadcastDetails`, not `isLiveContent`, not a premiere | `NOT_A_STREAM` |
| No formats but has `liveBroadcastDetails` or `isLiveContent` | `UPCOMING` |
| `liveBroadcastDetails.endTimestamp` present and not live now | `POST_LIVE` (DVR) |
| Default | `VOD` |

### Playability Status Parsing

The `parsePlayabilityStatus` function classifies the video's accessibility:

| Status Code | Condition | PlayabilityError |
|-------------|-----------|-----------------|
| `OK` | -- | `ok` |
| `LIVE_STREAM_OFFLINE` | -- | `ok` (upcoming, not an error) |
| `UNPLAYABLE` + "live event will begin" | -- | `ok` (upcoming) |
| `LOGIN_REQUIRED` + "member"/"join" in reason | -- | `members_only` |
| `LOGIN_REQUIRED` | -- | `login_required` |
| `UNPLAYABLE` + "member" in reason | -- | `members_only` |
| `UNPLAYABLE` + "private" in reason | -- | `private` |
| `UNPLAYABLE` + "country"/"region"/"not available in your" | -- | `region_blocked` |
| `UNPLAYABLE` + "unavailable" | -- | `unavailable` |
| `AGE_VERIFICATION_REQUIRED` | -- | `age_restricted` |
| `ERROR` + "private"/"unavailable" | -- | `unavailable` |
| Anything else | -- | `unknown` |

### N-Parameter Decryption

YouTube applies download throttling to streams served without the n-parameter being decrypted. The `decryptNParam` method:

1. Parses the URL to extract the `n` query parameter value.
2. Calls `cipherSolver.GetSolvers(ctx, playerURL)` to get the compiled cipher VM.
3. Invokes `solvers.DecryptN(nParam)` to execute the JavaScript decryption function.
4. Replaces `n=<encrypted>` with `n=<decrypted>` in the raw URL string using `strings.Replace`. This preserves the original URL parameter order -- Go's `url.Values.Encode()` sorts parameters alphabetically, which breaks YouTube's URL signature verification.

The `DecryptNParamInUrl` method additionally handles path-based n-parameters (`/n/{encrypted_value}/`) by matching with regex `"/n/([a-zA-Z0-9_-]{10,})/"` and replacing the path segment.

### Signature Decryption

For formats that include a `signatureCipher` field instead of a direct URL, the `decryptSignatureCipher` method:

1. URL-decodes the `signatureCipher` string to extract: `url` (stream base URL), `s` (encrypted signature), `sp` (signature parameter name, defaults to "signature").
2. Gets the cipher solvers from the cache/compiler.
3. Calls `solvers.DecryptSig(encSig)` to decrypt the signature.
4. Appends the decrypted signature to the stream URL as `{sp}={decryptedSig}`.
5. Does NOT decrypt the n-parameter here -- the caller always calls `decryptNParam` separately. Doing both would cause double-decryption and HTTP 403 errors.

---

## Twitch Service (`internal/twitch/`)

### Architecture

The Twitch integration has five components:

1. **Service** (`service.go`) -- Top-level facade wrapping API, Auth, and EmoteResolver.
2. **API** (`api.go`) -- Low-level GQL and Usher HTTP client.
3. **Auth** (`auth.go`) -- OAuth token extraction from cookie jar and validation via Twitch's OAuth endpoint.
4. **HLS** (`hls.go`) -- Master playlist parsing and variant selection.
5. **EmoteResolver** (`emotes.go`) -- Third-party emote fetching (BTTV, FFZ, 7TV) with LRU cache.
6. **ChatDownloader** (`chat.go`) -- IRC WebSocket chat recording.
7. **VodChatDownloader** (`vod_chat.go`) -- GQL-based VOD chat archival.

### GQL API Protocol

#### Endpoint and Headers

All GQL requests go to:

```
POST https://gql.twitch.tv/gql
```

Required headers on every request:

| Header | Value |
|--------|-------|
| `Client-ID` | `kimne78kx3ncx6brgo4mv6wki5h1ko` (hardcoded public client ID) |
| `Content-Type` | `application/json` |
| `User-Agent` | Chrome desktop UA string |
| `Authorization` | `OAuth {token}` (optional, only when authenticated) |

#### Persisted Queries

Structured queries use SHA256 persisted query hashing (version 1). The request format is:

```json
{
  "operationName": "<OperationName>",
  "variables": { ... },
  "extensions": {
    "persistedQuery": {
      "version": 1,
      "sha256Hash": "<HASH>"
    }
  }
}
```

The hashes are stored in `constants.TwitchGQLHashes`:

| Operation | Hash |
|-----------|------|
| `StreamMetadata` | `b57f9b910f8cd1a4659d894fe7550ccc81ec9052c01e438b290fd66a040b9b93` |
| `ComscoreStreamingQuery` | `e1edae8122517d013405f237ffcc124515dc6ded82480a88daef69c83b53ac01` |
| `VideoMetadata` | `45111672eea2e507f8ba44d101a61862f9c56b11dee09a15634cb75cb9b9084d` |
| `VideoCommentsByOffsetOrCursor` | `b70a3591ff0f4e0313d126c6a1502d79a1c02baebb288227c582044aa76adf6a` |

#### Inline Queries

Access token requests and profile image lookups use inline GraphQL (not persisted queries). For example, the stream playback access token query:

```graphql
{
  streamPlaybackAccessToken(
    channelName: "{login}",
    params: {
      platform: "web",
      playerBackend: "mediaplayer",
      playerType: "site"
    }
  ) {
    value
    signature
  }
}
```

Input sanitization: channel logins are stripped of non-alphanumeric/underscore characters via regex `[^a-zA-Z0-9_]`, and VOD IDs are stripped of non-numeric characters via `[^0-9]`.

#### Batched Requests

`GetStreamInfo` sends two persisted queries in a single HTTP request as a JSON array:
1. `StreamMetadata` -- provides stream ID, title, viewer count, start time, game category, profile image.
2. `ComscoreStreamingQuery` -- provides title and game category as fallbacks.

The response is a JSON array where each element corresponds to one query. Both responses are merged: `StreamMetadata` takes priority, `ComscoreStreamingQuery` provides fallbacks for empty fields.

#### Error Handling

Twitch GQL returns HTTP 200 even for application-level errors. The `gqlRequest` method:
1. Checks if the response is a single object -- looks for `errors[0].message`.
2. Checks if the response is a batch array -- checks each element for errors.
3. Size-limits all response reads to 5 MB.

#### Stream Type Normalization

After parsing stream info, the `StreamType` field is normalized: anything that is not `"rerun"` is set to `"live"`.

### HLS Download

#### URL Construction

Live stream master playlist URL:
```
https://usher.ttvnw.net/api/channel/hls/{channel_login}.m3u8?{params}
```

VOD master playlist URL:
```
https://usher.ttvnw.net/vod/{vod_id}.m3u8?{params}
```

Both include these query parameters:
- `allow_source=true` -- Request source quality.
- `allow_audio_only=true` -- Include audio-only variant.
- `allow_spectre=true` -- Allow spectre (transcoded) variants.
- `fast_bread=true` -- Low-latency mode.
- `p={random}` -- Random integer (0 to 10 million) to bypass CDN caching.
- `player=twitchweb` -- Player identifier.
- `playlist_include_framerate=true` -- Include frame rate metadata.
- `sig={token.Signature}` -- Access token signature.
- `token={token.Value}` -- Access token value.
- `type=any` -- Accept any stream type.

#### Master Playlist Parsing

`ParseHLSMasterPlaylist` in `hls.go` parses `#EXT-X-STREAM-INF` tags using regex extraction:

| Attribute | Regex | Field |
|-----------|-------|-------|
| `BANDWIDTH` | `BANDWIDTH=(\d+)` | `Bandwidth` |
| `RESOLUTION` | `RESOLUTION=(\d+)x(\d+)` | `Width`, `Height` |
| `FRAME-RATE` | `FRAME-RATE=([\d.]+)` | `FPS` |
| `VIDEO` | `VIDEO="([^"]+)"` | `VideoGroup` |

Source quality detection: a variant `IsSource` is true if `VideoGroup` equals `"chunked"` or contains the string `"source"` (case-insensitive).

Variant naming: if `VideoGroup` is set, use it as the name. Otherwise, construct `{height}p{30|60}` from resolution and FPS.

#### Variant Selection Algorithm

`SelectBestVariant` in `hls.go` selects a variant given a quality preference string and max resolution:

1. **Audio-only**: If `qualityPref == "audio_only"`, find a variant with "audio_only" in its name. Fall back to the last variant (lowest quality).
2. **Filter**: Remove all audio_only variants from the candidate list.
3. **Resolution cap**: If `maxResolution > 0`, filter to variants where `max(width, height) <= maxResolution`. Only apply if it leaves at least one candidate.
4. **Quality preference matching**:
   - Parse preference string like `"1080p60"` into `(height=1080, fps=60)`.
   - Try exact height match. If FPS is specified, prefer highest bandwidth among FPS matches.
   - If no exact match, descend to the next lower available height.
   - If preference is a non-numeric string, do substring matching on variant names.
5. **Source preference**: If no quality preference matched, prefer the `IsSource` variant.
6. **Bandwidth fallback**: Select the variant with highest bandwidth.

### Twitch Authentication

`Auth` in `auth.go` extracts the `auth-token` cookie from the shared `CookieJar`. Token validation is performed against `https://id.twitch.tv/oauth2/validate` with an `Authorization: OAuth {token}` header. A 401 response means the token is invalid. A 200 response includes the user's login and user ID.

### IRC Chat (Live)

#### Connection Protocol

`ChatDownloader` in `chat.go` connects via WebSocket to `wss://irc-ws.chat.twitch.tv:443` using the `nhooyr.io/websocket` library. The read limit is set to 512 KB.

Authentication sequence:
1. **PASS**: `PASS oauth:{token}` (authenticated) or `PASS SCHMOOPIIE` (anonymous).
2. **NICK**: `NICK justinfan{random}` where random is 0-99999. Anonymous nick regardless of auth status.
3. **CAP REQ**: `CAP REQ :twitch.tv/tags twitch.tv/commands twitch.tv/membership` -- Requests rich metadata (emote tags, sub events, join/part events).
4. **JOIN**: `JOIN #{channel_login}` (lowercased).

#### Message Processing

The IRC parser handles two message types:

**PRIVMSG** (regular chat and bits):
- Tag fields extracted: `id`, `tmi-sent-ts` (epoch ms), `bits`, `display-name`, `login`, `user-id`, `badges`, `color`, `emotes`.
- If `bits > 0`, message type is `"bits"`, otherwise `"chat"`.
- Emote tags parsed from format `id:start-end,start-end/id:start-end` into `TwitchEmoteRef` structs with start/end as rune indices (not byte indices), matching Twitch's character offset convention.
- `OffsetMs` computed as `tmiSentTs - baseMs` where baseMs is the recording start time (or stream start time as fallback).

**USERNOTICE** (subs, raids, memberships):
- Tag fields extracted: same as PRIVMSG plus `msg-id`, `system-msg`, `msg-param-sub-plan`, `msg-param-recipient-display-name`, `msg-param-viewerCount`.
- `system-msg` is unescaped (`\s` to space).
- Message type normalization: `sub` -> `"sub"`, `resub` -> `"resub"`, `subgift`/`submysterygift` -> `"subgift"`, `raid` -> `"raid"`, everything else -> `"system"`.

**PING handling**: Responds with `PONG :tmi.twitch.tv`.

#### Deduplication

Messages are deduplicated by their `id` tag. The seen set is maintained as:
- `seenIDs map[string]struct{}` -- O(1) lookup.
- `seenOrder []string` -- Insertion order for deterministic eviction.

Pruning occurs when `len(seenOrder) > 5000 * 2` (10,000). The oldest entries are removed to keep only the most recent 5000.

#### Flush Strategy

Chat messages are written to disk using a **message-triggered timer** pattern:
1. When the first message arrives in a quiet period, a 1-second timer starts (`chatSaveInterval`).
2. All messages received during that 1-second window are batched together.
3. When the timer fires, `flush()` is called: first flush writes the complete JSON file atomically (via `.tmp` + rename); subsequent flushes use incremental append (truncate at `]`, append new messages).
4. The `messageCount` in the JSON header is padded to 20 characters with trailing whitespace, ensuring the header byte size stays constant during in-place updates.

#### Reconnection

- **Max reconnects**: 10.
- **Backoff**: Exponential, `1000 * 2^attempt` milliseconds, capped at 30 seconds.
- **Max consecutive errors**: 20 per session. Exceeding this triggers reconnection (not abort).
- **State preservation**: `flush()` is called before each reconnect. Resume state is saved to `{outputPath}.resume.json`.

#### Resume State

The sidecar `.resume.json` file contains:

```json
{
  "messageCount": 1234,
  "lastTimestampMs": 1709000000000,
  "timestamp": 1709000000,
  "streamId": "12345678",
  "recentIds": ["msg-id-1", "msg-id-2", ...]
}
```

On restart, if the `streamId` matches, the downloader resumes with the saved message count, last timestamp, and dedup set. The resume file is deleted on clean completion.

### VOD Chat

`VodChatDownloader` in `vod_chat.go` downloads chat from completed VODs using the GQL `VideoCommentsByOffsetOrCursor` persisted query.

#### Pagination

- Initial request: `contentOffsetSeconds = 0` (or resumed offset).
- Each response includes `hasNextPage` and edges with `contentOffsetSeconds`.
- After processing each page, `contentOffset` advances to the last edge's offset.
- Termination: no results returned, no new (non-duplicate) messages, or `hasNextPage == false`.

#### Error Handling

- **Max consecutive errors**: 5 (much lower than live chat's 20).
- **Backoff**: `2 * consecutiveErrors` seconds (linear, not exponential).
- Errors are logged and resume state is saved before returning.

#### Flush Interval

Periodic flush every 5 seconds (`vodChatFlushInterval`). Same incremental append strategy as IRC chat.

#### Resume State

Similar to IRC, but tracks `lastOffsetSeconds` instead of `lastTimestampMs`:

```json
{
  "messageCount": 5678,
  "lastOffsetSeconds": 3600.5,
  "timestamp": 1709000000,
  "streamId": "v1234567890",
  "recentIds": ["comment-id-1", ...]
}
```

Maximum 1000 recent IDs in VOD chat resume state (`vodChatResumeMaxRecentIDs`).

### Emote Resolution

`EmoteResolver` in `emotes.go` fetches and caches third-party emotes from BTTV, FFZ, and 7TV.

#### Fetch Strategy

All three providers are fetched in parallel using a `sync.WaitGroup`. Each has an 8-second timeout (`emoteTimeout`). Failures are logged at debug level and return nil (non-fatal).

#### Provider Details

**BTTV** (BetterTTV):
- Endpoint: `https://api.betterttv.net/3/cached/users/twitch/{channelID}`
- Response: `channelEmotes` + `sharedEmotes` arrays.
- CDN URL: `https://cdn.betterttv.net/emote/{id}/2x.webp`

**FFZ** (FrankerFaceZ):
- Endpoint: `https://api.frankerfacez.com/v1/room/id/{channelID}`
- Response: `sets` map containing `emoticons` arrays with `urls` maps.
- URL selection: prefer `urls["2"]` (2x), fall back to `urls["1"]` (1x).
- Protocol-relative URL fix: prepend `https:` if URL starts with `//`.

**7TV**:
- Endpoint: `https://7tv.io/v3/users/twitch/{channelID}`
- Response: `emote_set.emotes` array with `data.host.url` and `data.host.files` array.
- File selection: prefer `2x.webp`, fall back to `1x.webp`, fall back to first file.
- Protocol-relative URL fix: prepend `https:` if host URL starts with `//`.

#### Caching

- **Type**: LRU (Least Recently Used).
- **Max size**: 200 channels.
- **Key**: lowercased `channelLogin` (preferred) or `channelID` (fallback).
- **Eviction**: When full, the oldest entry (by insertion order) is removed.
- **Inflight dedup**: If a request for the same cache key is already in-flight, subsequent callers block on a channel until the first completes, then read from cache.

#### Emote Injection

After chat download completes, `EnrichWithEmotes` reads the chat JSON file, adds the resolved `TwitchEmoteData` to the `emotes` field, and rewrites the file atomically. This uses a fresh `context.Background()` context with a 30-second timeout, ensuring emote resolution completes even after the parent context is cancelled.

#### Per-Part Chat Rolling (live IRC)

Multi-part Twitch live recordings (gap/quality splits) roll the chat file at every part boundary via `ChatDownloader.RollFile(newPath, newRecordingStart)`: pending messages drain into the closing file, its resume state is deleted (the part is final), and recording redirects to the new part's staging dir with `OffsetMs` rebased to the new part's capture start — so each `{name} - partN.chat.json` replays in sync against its own video part. The dedup set and the cumulative message total survive the roll (`ChatResumeState` carries both the per-file `messageCount` and the job-wide `totalCount`; legacy single-file states fall back to `messageCount`). Closed parts are emote-enriched from the background part-mux goroutine using a cached resolve (`resolveEmotesCached` — the emote APIs are hit once per job, not once per part); the final part is enriched by the stream-end drain as before.

---

## BotGuard / PO Token (`internal/bgutils/`)

### Purpose

YouTube's BotGuard is a client verification system that generates Proof of Origin (PO) tokens. These tokens prove that a request originates from a legitimate browser environment. Without them, certain YouTube API responses (premium-quality formats, live streams) may be degraded or blocked.

### Why a Sidecar

BotGuard inspects the JS runtime's wall-clock timing as part of its fingerprint — its snapshot routine runs a sequence of operations and measures how long they take. Real Chrome + V8 takes 50–200 ms. The Goja interpreter, being a non-JIT pure-Go implementation, completes the same operations in ~552 µs — about 100× faster. BotGuard treats this speed disparity as a "this isn't a real browser" signal and refuses to mint a real `integrityToken`, returning only a `websafeFallbackToken`.

The hand-rolled real-class DOM shim work (test.50–test.55) raised the goja runtime's API fidelity to browser parity (`document instanceof HTMLDocument`, real event dispatch with capture/bubble, real `CSSStyleDeclaration`, real `URL`/`AbortController`, etc.) but couldn't bridge the timing gap because the gap is below the JS API surface — it lives in the V8-vs-interpreter speed difference itself.

The fix is to run BotGuard under real V8 + JSDOM. Moombox embeds a Node.js v22 binary plus `bgutils-js` + `jsdom` (both MIT-licensed) and runs them as a subprocess. This is the same combination `bgutil-ytdlp-pot-provider/server/` ships in production.

### Architecture

```
                  yt-dlp + bgutil-ytdlp-pot-provider plugin
                                  │
                                  │ HTTP GET http://localhost:774/get_pot
                                  ↓
                  ┌────────────────────────────────────────┐
                  │ Moombox HTTP server :774                │
                  │ /get_pot, /invalidate_caches,           │
                  │ /invalidate_it -- LoopbackOnly,         │
                  │ CSRF-exempt                             │
                  └────────────────┬───────────────────────┘
                                   ↓
                  ┌────────────────────────────────────────┐
                  │ internal/bgutils/PotProvider            │
                  │  ├── sessionCache (Go map)              │
                  │  ├── minterCache (Go map; goja-only)    │
                  │  ├── inflightDedup (Go sync.Map)        │
                  │  └── sidecar *Sidecar                   │
                  └────────────────┬───────────────────────┘
                                   ↓ (when sidecar healthy)
                  ┌────────────────────────────────────────┐
                  │ internal/bgutils/sidecar.Sidecar        │
                  │  ├── go:embed node.exe.gz               │
                  │  ├── go:embed sidecar.tar.gz            │
                  │  ├── extract-on-first-launch logic      │
                  │  ├── exec.Cmd + Job Object pinning      │
                  │  ├── stdin/stdout JSON-RPC channel      │
                  │  └── reqID -> chan response mux         │
                  └────────────────┬───────────────────────┘
                                   ↓ stdin/stdout pipes
                  ┌────────────────────────────────────────┐
                  │ Bundled node.exe                        │
                  │  └── bgutil-sidecar/src/server.js       │
                  │       └── BgUtils SessionManager        │
                  │             └── BgUtils +              │
                  │                 JSDOM +                │
                  │                 V8 (real)              │
                  └────────────────────────────────────────┘
```

`PotProvider.generateAndMint` branches on `pp.sidecar != nil && pp.sidecar.IsHealthy()`. On success, it caches the result in the session cache and returns. On any sidecar error, it logs a warning and falls through to the legacy goja-only path so token generation never goes completely dark.

### Sidecar lifecycle (`internal/bgutils/sidecar/`)

**Embed:** `internal/bgutils/embed/` is a standalone package exposing three `go:embed`'d package vars:
- `EmbeddedNode []byte` — gzipped Node.js v22 binary for the build's GOOS/GOARCH (~33-43 MB), produced by `tools/fetch-node` and selected via per-platform `embed_<goos>_<goarch>.go` build tags.
- `SidecarTarGz []byte` — gzipped tarball of `bgutil-sidecar/` production deps + JS source (~3.5 MB), produced by `bgutil-sidecar/build.mjs`.
- `Version string` — content of `internal/bgutils/embed/version.txt`, format `node@vX.Y.Z sha256@<sha>`. Used as the cache-invalidation key.

**First-launch extraction:** `extractIfNeeded(cacheDir)` resolves `cacheDir = os.UserCacheDir() + "/Moombox/sidecar"` (Windows: `%LOCALAPPDATA%/Moombox/sidecar`), tightens the dir's ACL via `utils.ApplyUserOnlyDACL` (always — even on cache-hit, so users upgrading from v2.5.x get the security benefit), then compares on-disk `version.txt` against the embedded `Version`. On match + key files present, the function returns immediately. On mismatch, it gunzip-extracts `node.exe`, gunzip+tar-extracts the sidecar payload using stdlib `archive/tar` + `compress/gzip` (end users do NOT need a system `tar` binary — that's a build-time-only requirement for `bgutil-sidecar/build.mjs`), and writes the new `version.txt` LAST so a partial extraction next time forces a redo. Tar-slip defense rejects entries whose target escapes `cacheDir`. File modes are clamped to `0o644` minimum to defend against tar variants that emit zero-mode headers.

**Subprocess:** `exec.Command(cacheDir+"/node.exe", cacheDir+"/src/server.js")` with `cmd.Dir = cacheDir`. Stdin/stdout are piped for JSON-RPC; stderr is piped to a goroutine that routes lines to Moombox's logger at Debug. The process is pinned to a Windows Job Object configured with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` so the child + any grandchildren die when Moombox exits — even on a hard parent crash. (Same pattern as `internal/cookies/job_windows.go`.)

**Handshake:** After `cmd.Start()`, the manager spawns `readPump` (consumes stdout JSON-RPC responses, routes to per-request channels via `reqID`) and `stderrPump` (logs at Debug). It then sends `{"id":1,"method":"ping"}` and waits up to `StartupTimeout` (default 5s on warm cache, 60s on first launch including extraction) for the `{"id":1,"result":"pong"}` reply. Failure here marks the sidecar unhealthy and falls back to goja.

**Per-request flow:** `Sidecar.GeneratePoToken(ctx, binding)` allocates a `reqID`, registers a buffered channel in `s.pending`, writes `{"id":N,"method":"generatePoToken","params":{"binding":"..."}}` to stdin under `s.writeMu`, then waits on either the channel or `ctx.Done()`. The readPump matches the response by `id` and forwards to the channel. Concurrent calls multiplex cleanly because each request has its own channel.

**Crash recovery:** If `readPump` observes stdout EOF (parent's view of child death), it calls `markUnhealthy("stdout EOF")` which atomically flips `s.healthy` to false and drains every pending request channel with an error. Subsequent `IsHealthy()` checks return false until either `Start` is re-attempted or the process restarts (a future enhancement; not in v2.6.0).

**Graceful shutdown:** `Sidecar.Stop()` is bounded at ~3 s total — 1 s for the JSON-RPC `shutdown` round-trip + 2 s for the process to exit on its own + Kill on timeout. The bound stays well under `cmd/moombox/shutdown.go`'s 10-s force-exit budget so a hung sidecar can't starve web-server / DB-close / pump-drain shutdown steps.

### IPC protocol

JSON-RPC-style, line-delimited (one JSON object per line, separated by `\n`). All fields ASCII-safe.

| Method | Params | Result | Notes |
|---|---|---|---|
| `ping` | (none) | `"pong"` | startup health check |
| `generatePoToken` | `{binding, challenge?, freshMinter?}` | `{poToken, binding, expiresAt, minterSource, minterFresh}` | hot path; both GVS and player mints send `challenge`, only GVS sets `freshMinter` (see "GVS (Segment-URL) PO Tokens" below). The interpreter URL inside `challenge` is origin-gated before any fetch |
| `invalidateCaches` | (none) | `"ok"` | wipes sidecar's session + minter caches |
| `invalidateIT` | (none) | `"ok"` | wipes only minter cache (force fresh BotGuard) |
| `getStats` | (none) | `{cachedMinters, cachedSessions, mintsTotal, mintsErrored}` | observability |
| `shutdown` | (none) | `"bye"` | graceful exit; sidecar JS calls `process.exit(0)` on next tick |

Errors are returned as `{"id":N,"error":"<message>"}`. Parse failures on stdout are logged at Warn and the line is dropped (defensive against partial writes during a parent crash).

### Sidecar JS (`bgutil-sidecar/`)

Lives at the repo root (peer to `cmd/`, `internal/`, `web/`). Production node_modules and `src/server.js` are tarred by `build.mjs` into the embed blob; the tarball is what gets shipped.

`src/server.js` is ~250 lines:
- Bootstrap JSDOM on `globalThis` (matching `bgutil-ytdlp-pot-provider/server/src/session_manager.ts`'s setup).
- Read JSON-RPC requests line-by-line from stdin via `readline`.
- Track inflight async dispatches so the process drains pending responses before exiting on stdin close (otherwise smoke tests would hang waiting for a response that never arrives because the parent already EOF'd stdin).
- Implement each method against `bgutils-js` directly (the actual BotGuard implementation by LuanRT; MIT-licensed). We deliberately do NOT depend on `bgutil-ytdlp-pot-provider` itself because that wrapper package is GPL-3.0-only and embedding it would force Moombox to GPL-3.0; we re-implement the ~100-line wrapper inline.
- Single-minter cache (mirrors PotProvider's CRIT-2 pattern) — one minter serves every binding until its TTL expires.

### Goja fallback path

When `[bgutils] use_sidecar = false` in config OR the sidecar fails to start OR `Sidecar.IsHealthy()` returns false mid-flight, `PotProvider.generateAndMint` falls through to the in-process flow:

1. **Fetch Challenge** (`challenge.go`):
   - POST to `https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/Create` (primary) or `https://www.youtube.com/api/jnn/v1/Create` (fallback, controlled by `config.UseYouTubeAPI`).
   - Request body: `["{requestKey}"]` where requestKey defaults to `O43z0dpjhgX20SCx4KAo`.
   - Headers: `Content-Type: application/json+protobuf`, `x-user-agent: grpc-web-javascript/0.1`, `x-goog-api-key: AIzaSyDyT5W0Jh49F30Pqqtyfdf7pDLFKLJoAnw` (Google endpoint only).
   - Result: `DescrambledChallenge` containing `Program`, `GlobalName`, `InterpreterScript`/`InterpreterURL`, `InterpreterHash`.

2. **Create BotGuard VM** (`botguard.go`):
   - Creates a new Goja runtime with the real-class DOM shim from `internal/goja/dom-real.js`, TextEncoder/TextDecoder, timers.
   - Fetches the interpreter JavaScript from `InterpreterURL` (or uses inline `InterpreterScript`).
   - Executes the interpreter in the VM. The interpreter registers a function on `globalThis[globalName]`.
   - Timeout: 10 seconds for BotGuard load.

3. **Take Snapshot** (`botguard.go`):
   - Creates a native JS array (`webPoSignalOutput`) in the VM.
   - Invokes the BotGuard function with the program string and the signal output array.
   - BotGuard populates the array with a callback function at index 0.
   - Returns the `botguardResponse` string (a snapshot of the BotGuard state).
   - Timeout: 30 seconds.
   - After snapshot, `ClearTimers()` is called to stop BotGuard's monitoring intervals that would leak approximately 1 MB every 30 seconds.

4. **Generate Integrity Token** (`webpo_client.go`):
   - POST to `https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/GenerateIT` (or YouTube variant).
   - Request body: `["{requestKey}", "{botguardResponse}"]`.
   - Response format: `[integrityToken, estimatedTtlSecs, mintRefreshThreshold, websafeFallbackToken]`.
   - On the goja path, `integrityToken` is typically null because BotGuard's timing fingerprint rejects the goja runtime — that's the whole reason the sidecar exists.

5. **Create Minter**:
   - **Path A (full)**: If `integrityToken` is present AND `webPoSignalOutput[0]` was populated by BotGuard, create a `WebPoMinter` that uses the callback to mint per-binding tokens. The Goja VM must stay alive for the minter's lifetime. Rare on the goja path.
   - **Path B (fallback)**: If `integrityToken` is null but `websafeFallbackToken` is present, use the fallback token directly as a static PO token for all content bindings. The VM is shut down immediately. This is the typical outcome on the goja-only path; works for most YouTube content but PO-token-gated formats may be unavailable.
   - Minter timeout for each mint operation: 3 seconds.

### Triple Cache (`pot_provider.go`)

`PotProvider` manages three in-process cache tiers that wrap both the sidecar and goja paths. The caches are agnostic to which path produced a token — once a session is cached, subsequent requests skip both paths entirely.

#### Session Cache
- **Type**: `map[string]*SessionData`
- **Key**: `contentBinding` (base64-encoded string, typically visitor data)
- **TTL**: 6 hours (`SessionCacheTTL`)
- **Content**: Complete `SessionData` with PO token, content binding, and expiry time.
- **Lookup**: Checked first on every `GeneratePoToken` call (unless `bypassCache` is true).
- **Authority**: Single source of truth for "is this token still fresh." Both the sidecar and goja paths populate this cache on success.

#### Minter Cache
- **Type**: `map[string]*TokenMinter`
- **Key**: `defaultMinterKey = "default"` (single-minter design — CRIT-2 audit fix; one minter serves every binding for its TTL).
- **TTL**: Dynamic, set by Google's `estimatedTtlSecs` in the GenerateIT response.
- **Content**: `TokenMinter` struct with `MintFunc` (closure over Goja VM), `ExpiresAt`, and `Cleanup` function.
- **Auto-eviction**: `time.AfterFunc(ttl, ...)` schedules exact-TTL cleanup. A second `time.AfterFunc(ttl - minterRefreshLead, ...)` fires 5 minutes before expiry to proactively regenerate the minter so user-facing calls don't pay the 2-10s BotGuard cost (FRESH-2 audit fix). When either AfterFunc fires, it acquires `pp.mu`, checks that the cached minter is still the same instance (pointer comparison), removes / replaces, then calls `Cleanup()` outside the lock to shut down the Goja VM.
- **Sidecar interaction**: Effectively unused under sidecar mode. The sidecar maintains its own internal minter cache inside the Node process; `PotProvider`'s minter cache is populated only when the sidecar path fails and the goja path generates a minter.
- **VM lifetime**: The `Cleanup` function is critical. The minter's `MintFunc` is a closure over the Goja runtime state. Calling `Cleanup()` shuts down the VM, invalidating the closure. This is why minter eviction is the only correct place to call it. `cleanupExpired` returns the slice of evicted minters and the caller (`GeneratePoToken`) runs `safeCleanup` outside `pp.mu` — holding the lock across `m.Cleanup()` could deadlock against a concurrent `mintPoToken` (CRIT-6 audit fix; same anti-pattern that was previously fixed in `InvalidateCaches` / `InvalidateIntegrityTokens`).

#### Inflight Dedup
- **Type**: `map[string]*inflightEntry`
- **Key**: `contentBinding`
- **Mechanism**: When a generation starts, an `inflightEntry` with a `done chan struct{}` is placed in the map. Concurrent requests for the same key block on `<-entry.done`. When generation completes, the result is stored on the entry, then `close(entry.done)` unblocks all waiters. Context cancellation is respected via `select`. Prevents thundering herd when many goroutines request the same binding simultaneously.

#### Cleanup fan-out

`InvalidateCaches()` and `InvalidateIntegrityTokens()` both call into the sidecar (when attached) via `Sidecar.InvalidateCaches(ctx)` / `Sidecar.InvalidateIT(ctx)` (5s ctx) so the sidecar's internal caches drop in lockstep with Moombox's. This is what backs the operator-facing `/invalidate_caches` and `/invalidate_it` HTTP routes used by yt-dlp's bgutil-pot-provider plugin.

#### Configuration

```toml
[bgutils]
use_sidecar = true   # default: true on Windows. Set to false to force goja-only.
```

Disabling the sidecar reverts to the websafe-fallback-only path. Most YouTube content keeps working but PO-token-gated formats become unavailable.

#### Cleanup Lifecycle

Expired entries are cleaned up in two ways:
1. **Lazy cleanup**: `cleanupExpired()` is called at the start of every `GeneratePoToken` call while the mutex is held. It iterates all session and minter entries, removing expired ones and calling `Cleanup()` on expired minters.
2. **Proactive auto-eviction**: `time.AfterFunc` on minter creation schedules exact-TTL cleanup.
3. **Manual invalidation**: `InvalidateCaches()` clears everything and shuts down all VMs. `InvalidateIntegrityTokens()` clears only the minter cache (forces BotGuard re-run but preserves session cache).

### GVS (Segment-URL) PO Tokens

Moombox mints two populations of PO token, and they follow different rules because upstream treats them differently.

**Player-API tokens** (`GeneratePlayerPoToken`, used by `fetchWithClient` / `fetchWithEmbedded`) bind to the **video ID** and are minted from the watch page's attestation challenge, but keep normal session caching. yt-dlp binds `PoTokenContext.PLAYER` to the video ID unconditionally (`pot/utils.py`), and moonarchive mints one challenge-sourced videoID-bound token that serves both the player request and GVS. Caching is retained deliberately: player calls fire on every probe and refresh (several per live job per hour, plus monitor polls), so fresh-minting each one would cost a multi-second BotGuard pass on the hot path. Note this token is injected whenever a video ID is available — it no longer requires visitor data, since the binding no longer derives from it.

**GVS (segment-URL) tokens** are minted under a deliberately cache-hostile policy — moonarchive parity, added 2026-08-14 (attestation POT coherence) after a premiere broadcast 403'd every segment for its full runtime because the minting session had no tie to the watch-page session that resolved the stream.

`PotProvider.GenerateGvsPoToken(ctx, binding, challenge) (GvsMint, error)` is called once per download start by each segment-download strategy (`internal/worker/strategy_youtube_dash.go`, `strategy_youtube_manifestless_dash.go`, `strategy_youtube_hls.go`):

- **Binding**: resolved by `youtube.GvsContentBinding` (`internal/youtube/pot_binding.go`), a port of yt-dlp's `get_webpo_content_binding`, and carried on `VideoInfo.GvsBinding`/`GvsBindingKind` so every strategy asks once and cannot drift. The rule, in order: the **video ID** when the page's player configs carry `html5_generate_content_po_token=true` (the experiment under which YouTube switches GVS binding to the video ID — active as of 2026-08-15, verified against a live watch page); otherwise the **datasync ID** for an authenticated session; otherwise **visitor data**. A last-resort video-ID/channel-ID fallback covers a session where none of those survived extraction, so a mint is never bound to an empty string. Moombox hardcoded the video ID until 2026-08-15; that was correct only while the experiment stays on, and a session with it off needs datasync binding or earns silent 403s.
- **Challenge**: `videoInfo.AttestationChallenge`, extracted from the watch page's own `window.ytAtN(...)` blob (see below). Empty when the page carried none, in which case the sidecar falls back to its own `/att/get` fetch — today's prior behavior, preserved exactly as the degraded case.
- **Cache policy**: bypasses the session cache entirely (no read, no write) — every call mints fresh, and the sidecar is told `freshMinter: true` so it regenerates its BotGuard minter for this call rather than reuse an already-cached one. The fresh minter **replaces** the sidecar's cached minter, so subsequent player-API mints passively pick up the more session-coherent one. Concurrent GVS mints share the sidecar's single in-flight regeneration (`minterPromise`); no provider-side inflight entry is added — per-binding minting off a shared minter is cheap, and adding provider-level dedup here would hand a stale (non-fresh) result to whichever caller lost the race.
- **Fallback**: sidecar unavailable → runs the existing goja mint-and-cache flow with the challenge ignored, reported as `minterSource=goja-fallback`.
- **Result**: `GvsMint{PoToken, MinterSource, MinterFresh, ViaSidecar}` — the fields the provenance log line below reports. `MinterSource` is `"challenge"` (built from the page's own challenge), `"att_get"` (sidecar fetched its own), or `"goja-fallback"`.
- **Counters**: `PotStats.GvsMints` (every attempt) and `GvsMintsChallenge` (the subset that carried a non-empty page challenge).

#### Watch-page challenge extraction (`internal/youtube/watch_page.go`)

`extractAttestationChallenge(html)` locates `window.ytAtN(` and then walks the argument with a **string-aware balanced-brace scan** (`scanBalancedObject`) rather than a non-greedy regex. moonarchive's `INITIAL_ATTESTATION_PATTERN` uses the regex form, but a `})` sequence anywhere inside the opaque payload truncates that match into an unbalanced fragment that then fails to parse — indistinguishable from "the page had no challenge". The captured literal runs through `JSToJSON` (a faithful Go port of yt-dlp's `js_to_json`, `internal/utils/jsjson.go`), is unmarshaled, and its `R` key — itself a JSON string, delivered with `\xNN` escapes on real pages — is unmarshaled again to pull out the top-level `bgChallenge`, re-marshaled compact as the challenge payload.

Every failure resolves to `""` with a **distinct reason** (the `atn*` constants, surfaced as `WatchPageResult.AttestationReason` and logged as `reason=`): no call on the page, unbalanced argument, JS-to-JSON failure, outer parse failure, no `R` key, `R` not JSON, no `bgChallenge`, bad challenge shape, no `interpreterUrl`, or disallowed interpreter host. A single catch-all reason would let a silently-broken extractor masquerade as a genuine absence, which is precisely the confusion this subsystem exists to eliminate. Absence is never an error — the sidecar's `/att/get` fallback handles it.

The value rides on `WatchPageResult.AttestationChallenge` → `VideoInfo.AttestationChallenge` via `withAttestation`, applied at every return path of both `GetVideoInfoAuthenticated` and `GetVideoInfoPublic` (including the ANDROID_VR / web_embedded / web_creator / watch-page-fallback routes that skip `mergeWatchPageMetadata`), which also resolves the GVS binding at the same point. The live quality-monitor refresh loop re-extracts every few minutes, so a re-mint after a downloader restart uses the freshest available challenge.

#### Interpreter-origin gate (security boundary)

The sidecar **executes** the interpreter body it fetches (`new Function(js)()`), and watch-page HTML embeds attacker-authored video metadata verbatim — JSON escaping leaves braces, parens and single quotes intact, so a crafted description can present itself as a `ytAtN` challenge, and on a real page the description precedes the genuine blob (verified 2026-08-15: description at byte ~740k, real call at ~802k). Before this gate, that reached `new Function()` with an attacker-chosen host.

Two independent checks now enforce the same rule — `validateChallengeOrigin` in `internal/youtube/watch_page.go` (so a hostile challenge never leaves the Go process) and `assertGoogleHost` in `bgutil-sidecar/src/server.js` (so the sidecar never trusts its caller):

- The interpreter URL must be `https:` on one of **eight exact hosts**: `www.google.com`, `google.com`, `www.gstatic.com`, `ssl.gstatic.com`, `gstatic.com`, `s.ytimg.com`, `www.youtube.com`, `youtube.com`. No suffix matching and no patterns — an adversarial review defeated both weaker forms. Suffix-matching `.googleapis.com` re-admitted `storage.googleapis.com` and `firebasestorage.googleapis.com`, which serve anyone's uploaded bucket objects (and `sites`/`script`/`drive.google.com`, which host third-party content); a `^google\.[a-z]{2,3}(\.[a-z]{2})?$` "regional Google" pattern matched *shape rather than ownership*, admitting live third-party domains such as `google.com.se` and `google.co.nl`. Both reached code execution end-to-end. Regional Google domains are consequently unsupported; the interpreter is served from a global host.
- The URL must be a **static script**: no query, no fragment, and an encoded path matching `^/[A-Za-z0-9._~/-]+\.[Jj][Ss]$`. An allowlisted host is *not* the same as Google-authored bytes — `www.google.com` serves JSONP endpoints (`/complete/search?client=firefox&jsonp=…`) that reflect an attacker-supplied callback at HTTP 200, which reached RCE through a genuinely allowlisted host with no redirect involved. Reflection requires a query to reflect, so demanding the static shape removes the class. Percent-encoding is excluded from the alphabet because Go decodes `%3F` into `url.Path` while JS's `URL` keeps it encoded: the two gates would otherwise disagree about what the path is, with safety resting on Google returning 404 for the crafted form.
- Redirects are refused outright (`redirect: "manual"`, any 3xx is an error) on all three sidecar fetches. undici follows redirects by default and only the pre-redirect host was gated, so an allowlisted host answering `302` delivered the body we execute. `/att/get` and `GenerateIT` get the same treatment: the `/att/get` response is the one trusted enough to execute inline.
- A page-sourced challenge carrying inline `interpreterJavascript` instead of a URL is **refused**, even though bgutils-js treats the two as interchangeable: inline script scraped from HTML has no origin to check at all. Those fall back to `/att/get`, whose response is a genuine YouTube API result; the sidecar honors inline script only for challenges it fetched itself (`trusted = minterSource === "att_get"`). Live YouTube ships `interpreterUrl` and never inline (verified 2026-08-15), so this costs nothing today.

A rejected host is reported by name in the reason string, so a genuinely-Google host missing from the list surfaces as "add this name" rather than an unexplained loss of session coherence.

#### Sidecar RPC additions

`generatePoToken` gained two optional params and three result fields (also reflected in the IPC protocol table above):

- `challenge` (param, string) — a `bgChallenge` JSON string. When present and well-formed (has `program` and `interpreterUrl`, and its host passes `assertGoogleHost`), `generateMinter` builds the BotGuard minter from it instead of fetching its own via `/att/get`. Malformed, disallowed, or absent challenges fall back to `/att/get` with a Warn-level log. Sent by both the GVS and player mints.
- `freshMinter` (param, bool) — forces `getOrCreateMinter` to regenerate even when the cached minter is still valid. Set by GVS mints; player mints omit it and take the cached minter.
- In-flight regenerations are **keyed by challenge**: a caller joins an in-flight BotGuard pass only when it supplied the same challenge. A caller with a different challenge waits and then regenerates its own rather than silently inheriting another session's minter — the earlier shared-promise behavior reported `minterSource=challenge` for a minter built from a *different* video's page, which is worse than no provenance at all. The cost is that two jobs starting within the same BotGuard window serialize.
- `minterSource` (result, string) — `"challenge"` or `"att_get"`, whichever input built the minter that served this specific mint.
- `minterFresh` (result, bool) — whether this mint triggered a fresh BotGuard regeneration (`true`) or reused an already-warm minter (`false`).

#### Mint provenance logging

Each GVS mint attempt logs one line at Info (`[POT] GVS mint`) on success or Warn (`[POT] GVS mint failed`) on error, emitted by the calling strategy immediately after `GenerateGvsPoToken` returns:

| Field | Meaning |
|---|---|
| `jobID` | the job that requested the mint |
| `binding` | which rule produced the content binding: `"videoID"` \| `"datasyncID"` \| `"visitorData"` \| `"channelID"` |
| `challenge` | `"page"` if the watch page carried a `ytAtN` challenge, else `"none"` (`challengeLabel` helper, `internal/worker/strategies.go`) |
| `minterSource` | `"challenge"` \| `"att_get"` \| `"goja-fallback"` |
| `minterFresh` | whether this call triggered a fresh BotGuard run |
| `sidecar` | whether the mint went through the sidecar (`true`) or the goja fallback (`false`) |
| `tokenLength` | length of the minted PO token string — a cheap sanity signal without logging the token itself |

The line exists so that if a future premiere still 403s, the log alone identifies the exact configuration in play — no reproduction needed. Datasync-ID binding is no longer a pending suspect: the full yt-dlp rule is implemented (see **Binding** above), so an authenticated session without the experiment already binds to its datasync ID.

A second deliberate divergence: `strategy_youtube_vod.go` attaches **no** GVS token to VOD format URLs, on the inherited claim that doing so causes 403s, while yt-dlp marks GVS POT `required=True` for HTTPS (progressive) formats on WEB clients. VOD downloads work today, so the working path is left alone rather than changed on upstream theory; revisit here first if VODs ever start 403ing.

The remaining known divergence from upstream, should POT-enforced media still 403: yt-dlp's `WEBPO_CLIENTS` list does **not** include ANDROID_VR, meaning upstream never mints a WebPO GVS token for formats sourced from that client — android_vr's policy is `not_required_with_player_token=True`, i.e. a *player* token satisfies GVS for it. Moombox's ANDROID_VR DASH-fallback formats do receive a WebPO GVS token. That is now paired with a correctly-bound player token, which is the combination upstream relies on, but it remains the first place to look.

#### Mid-job re-mint: behind-head 403 recovery

A stale-token or stale-URL 403 mid-download no longer requires a full downloader restart to fix. The recovery splits across two layers: the engine decides *when* to ask, the worker decides *how* to answer.

**Engine half** (`internal/engine/downloader.go`, `downloader_fetch.go`, `downloader_dash.go`) — `SegmentDownloader.poTokenOverride` mirrors the existing `baseURLOverride`: `SetPoToken`/`getPoToken` make the PO token replaceable in place, atomically, without tearing the downloader down. `DownloaderOptions.OnCredentialRefresh func() (baseURL, poToken string)` is the seam a strategy wires; `refreshCredentials()` calls it and installs whatever it returns (either return value may be `""` to leave that half alone). A 403 burst can hit every catch-up worker in the same instant, so the call is gated by `credentialRefreshCooldown` (5s) via `atomicTime.TryClaim` — exactly one player-response round trip per cooldown window, never one per failing segment.

`fetchSegmentWithRetry` (the parallel catch-up path) still treats 410 as immediately permanent, and still treats a 403 **at or past head** as immediately permanent — that is how a finished stream signals "no such segment," and VOD/post-live finalization depend on it staying that way. A 403 **below head** (the segment demonstrably exists, per the harvested `X-Head-Seqnum`) instead calls `refreshCredentials()` and retries, bounded by `forbiddenRefreshAttempts` (3) attempts on that segment; a refreshed base URL is rebuilt into the retry through the caller-supplied `rebuildURL` closure rather than reused stale, and exhausting the attempt budget still returns `ErrSegmentPermanent` — no caller's contract changed. The sequential DASH loop (`runDashLoop`/`handleGoneError`) never goes through `fetchSegmentWithRetry`, so it fires the same `refreshCredentials()` call directly once a behind-head gone burst persists past `postBytes403CipherThreshold` (5) consecutive failures — a pure side effect placed ahead of the existing end-of-stream verdict logic, so it cannot perturb that logic's finalize/warn paths.

A failure episode also throttles the next catch-up batch: `catchUpBatchLimit()` drops to 1 segment once `noteCatchUpFailureEpisode()` fires and regrows by 1 every `catchUpRegrowInterval` (10s), back up to `maxCatchupBatch` (48) — moonarchive's `batch_count = min(1 + time_since_check/10, batch_count)` — so a credential outage isn't answered with 48 simultaneous doomed requests while it's still recovering.

**Worker half** (`refreshGvsCredentials`, `internal/worker/strategies.go`) supplies the callback. Today only the manifestless DASH strategy wires it, into both the video and audio downloaders' `OnCredentialRefresh` (`strategy_youtube_manifestless_dash.go`); the manifest-based DASH and HLS strategies do not wire it yet. The function mints the GVS token FIRST, with `bypassCache: true` — handing back the cached token would make the refresh a no-op, since that cached token is the exact credential that just earned the 403 — spending the full remaining budget on the mint before the URL half gets whatever is left over. The URL half then re-fetches the player response via `job.YT.GetVideoInfo` rather than re-resolving the cached, already-stale `Format`: a cached Format re-run through a freshly invalidated cipher solver reproduces the same expired URL byte-for-byte, because the URL's `expire`/`ei` parameters come from the player response, not from cipher decryption. It falls back to the caller's cached formats if the re-fetch fails, and only runs at all when a player URL and a cipher solver are both available — mirroring `OnCipherFailure`'s own install guard. The content binding is resolved once at strategy setup (`poTokenBinding`/`gvsBinding`) and threaded into every refresh call rather than recomputed per call: `invalidate403Caches` unconditionally clears visitor data on every refresh, so recomputing would let the second downloader's refresh (or any later retry) silently drift onto the degraded channelID/videoID fallback while the first refresh used the real binding. The whole round trip — mint plus re-fetch plus resolve — is bounded by `min(45s, MaxTimeout/3)` (`credentialRefreshTimeoutFor`): the clamp exists because a job running near the 30s `MaximumTimeout` floor can't afford a flat 45s ceiling — that would let one refresh attempt consume the entire stall budget and flip `behindHeadTailPending` false at the exact moment working credentials arrived.

This mirrors both upstreams, which converged on the same shape independently. yt-dlp's `url_feed` callback re-fetches the player response and returns a fresh URL per itag, cooldown-gated at 5s once `fragment_retries` (default 10) starts failing. moonarchive's `frag_iterator` falls through to `_get_web_player_response` on 403 — *"stream access expired? retrieve a fresh manifest"* — retries the same sequence, and damps its request batch the same way `catchUpBatchLimit` does here. Neither treats 403 as terminal, and neither refreshes only the URL: both go back to the player response, which is what produces fresh PO-token context too.

---

## Cipher Solver (`internal/cipher/`)

### Purpose

YouTube protects stream URLs with two encryption layers:
1. **Signature cipher** (`s` parameter): The video URL's signature is encrypted using a function defined in the player JavaScript. Without decryption, the URL returns HTTP 403.
2. **N-parameter** (`n` parameter): A throttling countermeasure. Without decryption, downloads are severely bandwidth-limited.

### 2-Tier Caching

#### Disk Cache (`PlayerCache`)
- **Location**: `~/.cache/yt-cipher/player_cache/` (or custom directory).
- **Key**: `SHA256(playerURL)` with `.js` extension.
- **TTL**: 14 days. Checked on read via file modification time. Expired files are deleted.
- **Eviction**: `Evict()` scans the directory and removes files older than 14 days. Called on startup.
- **Atomic writes**: Uses `.tmp` file + rename pattern.

#### Memory Cache (LRU)
- **Type**: `map[string]*Solvers` with a `[]string` order slice.
- **Key**: `SHA256(playerURL)`.
- **Max size**: 3 entries (`solverCacheSize`).
- **Eviction**: LRU -- oldest entry (by insertion order) is removed when inserting beyond capacity.
- **Content**: Compiled `Solvers` struct containing `Sig` and `N` function closures over a Goja VM.

### Thread Safety

The `Solvers` struct wraps its function calls with a `sync.Mutex`:

```go
type Solvers struct {
    mu  sync.Mutex
    N   func(string) (string, error)
    Sig func(string) (string, error)
}
```

`DecryptN` and `DecryptSig` acquire the mutex before calling the underlying function. This is necessary because Goja runtimes are not thread-safe, and multiple goroutines may need to decrypt URLs concurrently.

### Compilation Pipeline

The `compileSolver` method executes this pipeline:

1. **Fetch player.js**: Download from URL (or retrieve from disk cache). The file is typically 1-3 MB of minified JavaScript.

2. **Preprocess**: Analyze the raw player JavaScript:
   - `findNArrayCandidates(playerJS)`: Find single-element array assignments (`varName=[funcName]`) that are n-parameter decryption function candidates. Two regex patterns handle main and ES6 variants.
   - `findSigCandidates(playerJS)`: Find signature decryption function candidates by matching the pattern `w+&&(w+=funcName(literal,decodeURIComponent(w+)))`.
   - `findAlrTransformChain(playerJS)`: Find the newer signature transform chain identified by the `set("alr","yes")` marker in the URL builder function.
   - `preprocessPlayer(playerJS)`: AST-based extraction of function definitions. If AST extraction fails, falls back to regex-based extraction. Produces a self-contained JavaScript string that sets `_result.sig` and `_result.n`.

3. **Compile**: Execute the preprocessed code in a fresh Goja VM:
   - A `_result` object is created in the VM.
   - The preprocessed code is executed, populating `_result.sig` and `_result.n` with JavaScript functions.
   - These are wrapped in Go closures with panic recovery.

### STS Extraction

`StsCache` in `sts.go` extracts the `signatureTimestamp` from player JavaScript using the regex `(?:signatureTimestamp|sts)\s*:\s*(\d+)`. The STS is a numeric value included in Innertube player requests to tell YouTube which cipher version to use for format URLs.

- **Cache size**: 150 entries.
- **Eviction**: Random entry removed when full (simple bounded cache, not LRU).

### Thundering Herd Prevention

The `Solver.compileMu sync.Mutex` serializes all compilation. When multiple goroutines request the same uncached player URL:
1. First goroutine acquires `compileMu`, starts compilation.
2. Other goroutines block on `compileMu`.
3. First goroutine completes, stores result in cache, releases lock.
4. Other goroutines acquire lock, re-check cache, find the result, return without compiling.

---

## Goja Runtime Shims (`internal/goja/`)

### Purpose

BotGuard and the cipher solver both execute YouTube's JavaScript in Goja, a pure-Go JavaScript engine. This JavaScript was written for browsers and expects DOM APIs, encoding APIs, and timer APIs. The shim package provides minimal stubs sufficient for execution without a full browser.

### Components

#### DOM Shim (`dom_shim.go`)

Provides a comprehensive browser-like environment via a single self-executing JavaScript function that sets globals on `globalThis`:

- **document**: `createElement`, `createElementNS`, `createTextNode`, `createDocumentFragment`, `getElementById`, `getElementsByTagName`, `querySelector`, `querySelectorAll`, `cookie`, `location`, `readyState`, etc.
- **Elements**: Each created element has stub methods for DOM manipulation (`appendChild`, `removeChild`, `setAttribute`, etc.), styling (`style`), and rendering (`getBoundingClientRect`).
- **Canvas**: `createElement('canvas')` returns an element with `getContext()` that provides 2D context stubs (`fillRect`, `clearRect`, `getImageData`, etc.).
- **navigator**: `userAgent`, `language`, `platform`, `hardwareConcurrency: 8`, `maxTouchPoints: 0`, etc.
- **window/self/top/parent/frames**: All point to `globalThis`.
- **screen**: 1920x1080, 24-bit color depth.
- **performance**: `now()` relative to initialization time.
- **localStorage/sessionStorage**: In-memory key-value stores.
- **XMLHttpRequest**: Stub that does nothing.
- **crypto**: `getRandomValues` using `Math.random`, `randomUUID` stub.
- **console**: Captures all log/warn/error calls into a `__consoleMessages` array for diagnostic access from Go.
- **MutationObserver, IntersectionObserver, ResizeObserver**: No-op stubs.
- **queueMicrotask**: Falls back to `Promise.resolve().then(fn)`.

#### TextEncoder/TextDecoder (`encoding.go`)

Inline JavaScript implementing UTF-8 multi-byte encoding:
- `TextEncoder.encode(string)` returns a `Uint8Array`.
- `TextDecoder.decode(uint8array)` returns a string.
- Also registers `atob` (base64 decode) and `btoa` (base64 encode) as global functions.

#### Timers (`timer.go`)

`TimerManager` provides `setTimeout`, `setInterval`, `clearTimeout`, `clearInterval`:

- **setTimeout**: Creates a `time.AfterFunc` that calls the JS callback after the delay. The timer entry is removed from the map before calling the callback.
- **setInterval**: Creates a `time.NewTicker` with a goroutine that reads from the ticker channel and calls the callback. An entry-specific `done` channel allows individual intervals to be stopped.
- **clearTimeout/clearInterval**: Both call `ClearTimer(id)` which stops the timer/ticker and closes the done channel.
- **CancelAll**: Stops all timers, closes the global `done` channel (unblocking all interval goroutines), and marks the manager as stopped so no new timers can be created.

The factory function `NewRuntimeWithShims(userAgent)` creates a fully configured Goja runtime with all three shim layers.

---

## YouTube Live Chat (`internal/chat/`)

### Architecture

- **ChatDownloader** (`downloader.go`) -- Manages the download lifecycle, dedup, disk IO, resume.
- **ChatAPI** (`api.go`) -- HTTP client for YouTube's live chat Innertube endpoints.
- **Types** (`types.go`) -- Data structures: `ChatMessage`, `MessagePart`, `SuperchatInfo`, `ChatData`, `ChatResumeState`.

### API Endpoints

| Endpoint | Path | Purpose |
|----------|------|---------|
| Live chat | `https://www.youtube.com/youtubei/v1/live_chat/get_live_chat` | Polling live chat messages. |
| Chat replay | `https://www.youtube.com/youtubei/v1/live_chat/get_live_chat_replay` | Downloading chat replay for VODs. |

Request body includes `context.client` with WEB client config and a `continuation` token. If `visitorData` is available, it is included in the client context. The API key is appended as a query parameter.

Authentication: `Cookie` header from the cookie jar, plus `Authorization` header (SAPISIDHASH) via `generateAuth` callback for member-gated chat.

### Continuation Lifecycle

1. **Initial extraction**: From the watch page HTML via `ExtractChatContinuation`. Navigates `ytInitialData.contents.twoColumnWatchNextResults.conversationBar.liveChatRenderer.continuations` and tries keys `reloadContinuationData`, `invalidationContinuationData`, `timedContinuationData`, `liveChatReplayContinuationData`.
2. **API responses**: Each response includes the next continuation token in `continuationContents.liveChatContinuation.continuations`.
3. **All Chat upgrade**: On the first response, the `header.liveChatHeaderRenderer.viewSelector.sortFilterSubMenuRenderer.subMenuItems[1]` contains the unfiltered "Live Chat" continuation token. YouTube defaults to "Top Chat" which can aggressively filter messages. The downloader switches to "All Chat" on the first response by using this alternative continuation token.
4. **Stale recovery**: When a continuation returns no data but the stream is still active, the downloader fetches a fresh continuation token from the watch page. If this fails, it retries with exponential backoff (10s initial, doubling, 5-minute cap, max 30 attempts).

### Message Types

The chat API response contains `actions` array items. Replay responses wrap actions in `replayChatItemAction` with a `videoOffsetTimeMsec`. Each action contains an `addChatItemAction.item` which may contain:

| Renderer | Description |
|----------|-------------|
| `liveChatTextMessageRenderer` | Regular chat messages. |
| `liveChatPaidMessageRenderer` | Super Chat (monetary donation with message). |
| `liveChatPaidStickerRenderer` | Super Sticker (monetary donation with sticker). |
| `liveChatMembershipItemRenderer` | Membership milestone messages. |

Super Chat tier colors are mapped from YouTube's internal `headerBackgroundColor` int64 values to tier numbers (1-7) and color names (blue, cyan, green, yellow, orange, magenta, red).

### Deduplication

Identical to Twitch chat: `seenIDs` map + `seenOrder` slice. Cull when exceeding 5000 entries by removing the oldest by insertion order.

### File IO Strategy

1. **First flush**: Atomic write via `.tmp` + rename. Complete JSON with all messages.
2. **Subsequent flushes**: Incremental append. Read last 10 bytes to find `]`, truncate there, append new messages + closing structure. Memory cost: O(new messages), not O(file size).
3. **Header updates**: `messageCount` and `downloadedAt` are updated in-place by reading only the first 1024 bytes of the file. The `messageCount` value is padded to 20 characters with trailing whitespace to keep byte offsets stable.
4. **Batching window**: Messages are batched within a 1-second window (`writeIntervalMs = 1000`).
5. **Fallback**: If incremental append fails (e.g., corrupt file), falls back to full rewrite.

### Error Limits

| Mode | Max Consecutive Errors | Backoff |
|------|----------------------|---------|
| Live | 20 | Linear, 5s × consecutive errors, cap 60s |
| VOD/Replay | 5 | Linear, 5s × consecutive errors, cap 30s |

### Resume State

Sidecar `.resume.json` file:

```json
{
  "messageCount": 1234,
  "continuation": "...",
  "timestamp": 1709000000,
  "videoId": "dQw4w9WgXcQ",
  "recentIds": ["msg-1", "msg-2", ...],
  "lastTimestampUsec": "1709000000000000"
}
```

Resume state is saved after each disk flush. On restart, the downloader loads the continuation token and dedup set, skips the All Chat switch (continuation is already mid-stream), and resumes. The resume file is deleted on clean completion but preserved on cancellation.

---

## Caching Strategy Summary

| Service | Cache Type | TTL | Size Limit | Eviction Strategy | Key |
|---------|-----------|-----|-----------|-------------------|-----|
| YouTube Visitor Data | Memory (single value) | None | 1 entry | Overwrite | N/A |
| YouTube STS | Memory map | None | 150 entries | Random eviction when full | SHA256(playerURL) |
| Twitch Emotes | Memory LRU | Unbounded (no expiry) | 200 channels | Oldest by insertion order | lowercased channelLogin |
| Cipher (Disk) | Disk files | 14 days | Unbounded | File age check on read; startup sweep | SHA256(playerURL) |
| Cipher (Memory) | Memory LRU | Unbounded (no expiry) | 3 solvers | Oldest by insertion order | SHA256(playerURL) |
| BotGuard Session | Memory map | 6 hours | Unbounded | TTL check at start of each generation | contentBinding |
| BotGuard Minter | Memory map (single-minter design) | Dynamic (from API) | 1 entry | TTL via `time.AfterFunc`; proactive refresh 5min before expiry | `defaultMinterKey` |
| BotGuard Inflight | Memory map | Request scope | Per-request | Removed on completion | contentBinding |
| Sidecar minter (in-Node) | Memory inside sidecar process | Dynamic (from API) | 1 entry | Restart on `invalidateIT` | (none — single-minter inside Node) |
| Sidecar extraction | Disk: `%LOCALAPPDATA%/Moombox/sidecar/` | Until `version.txt` mismatch | 1 install | Re-extract on Node-version bump | `version.txt` content |
| YouTube Chat Dedup | Memory set + ordered slice | Session lifetime | 5000 IDs | Oldest by insertion order | messageId |
| Twitch IRC Dedup | Memory set + ordered slice | Session lifetime | 5000 IDs | Oldest by insertion order | messageId |
| Twitch VOD Chat Dedup | Memory set + ordered slice | Session lifetime | 5000 IDs | Oldest by insertion order | commentId |

---

## Cross-References

### Related Documents
- `architecture.md` -- Download pipeline, concurrency model, service initialization order.
- `data-and-storage.md` -- Cookie handling, file output formats, database schema.
- `security.md` -- Auth tokens, CSRF for API calls, cookie security.

### Source Files
- `internal/youtube/` -- Service facade, PlayerAPI, Auth, FormatSelector, WatchPage, Types (7 files, ~2,000 lines).
- `internal/twitch/` -- Service, API, Auth, HLS, Chat, VodChat, Emotes, Types (9 files, ~3,200 lines).
- `internal/bgutils/` -- PotProvider, WebPoClient, Challenge, BotGuard, WebPoMinter, ColdStart, Types (8 files, ~1,400 lines).
- `internal/cipher/` -- Solver, PlayerCache, Extractor, STS, Decrypt, ResolveURL, Types (9 files, ~1,500 lines).
- `internal/goja/` -- Runtime, DOMShim, Encoding, Timer (4 files, ~700 lines).
- `internal/chat/` -- Downloader, API, Types (3 files, ~1,400 lines).
- `internal/constants/constants.go` -- All API keys, URLs, client configs, timeouts, limits.
