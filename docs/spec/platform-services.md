# Platform Services

## Scope

This document provides a comprehensive, implementation-level reference for every external platform integration in Moombox: YouTube (Innertube API, live chat, cipher decryption, BotGuard/PO tokens) and Twitch (GQL API, HLS streaming, IRC chat, VOD chat, third-party emotes). It covers API protocols, authentication mechanisms, caching strategies, error handling, retry policies, and the Goja JavaScript runtime environment that enables BotGuard and cipher operations. This is the authoritative reference for understanding how Moombox communicates with external services.

## Rules and Constraints

These are hard rules that govern all platform service integrations:

- **YouTube uses multi-client Innertube fallback.** The authenticated request order is: WEB_EMBEDDED (format/DASH contributor) then TV_DOWNGRADED (best format coverage, and the playability authority) then WEB (DASH manifest) then WEB_CREATOR (member content) then the cookieless chain VISIONOS → ANDROID_VR (last resort). The public request order is TV_DOWNGRADED, then the cookieless chain, with WEB_EMBEDDED only for age-restricted content (see §Public Fallback Flow). On the authenticated path WEB_EMBEDDED, TV_DOWNGRADED and WEB are always tried; WEB_CREATOR and the cookieless chain are conditional fallbacks (tried only when earlier clients return members-only, login-required, or no formats — and never at all while TV reports an upcoming stream with `PlayabilityOK`, which is a waiting room rather than a format problem).
- **Format priority is lexicographic.** For video, across five dimensions: resolution (higher wins) > FPS (prefer60fps setting) > codec score (higher wins) > bitrate (higher wins — yt-dlp's `size`/`br` sort prefers the higher-quality / Premium stream at the same res/fps/codec) > auth level (lower preferred). For audio, across four: audio track identity (`audioTrackScore` — original > default > unlabelled > descriptive, the clean rendition ahead of its DRC twin) > codec score (higher wins) > bitrate (higher wins) > auth level (lower preferred). This ordering is absolute and implemented in `SelectBestFormats`.
- **Twitch uses GQL API with SHA256 persisted query hashing (version 1).** All structured queries (stream metadata, video metadata, VOD comments) use persisted queries with hardcoded SHA256 hashes. Access token queries use inline GraphQL. The Client-ID header (`kimne78kx3ncx6brgo4mv6wki5h1ko`) is required on every GQL request.
- **BotGuard has a triple cache with auto-eviction.** Session cache (6-hour TTL, keyed by contentBinding), minter cache (dynamic TTL from Google's API, a single entry under `defaultMinterKey` — one minter serves every content binding — auto-evicted via `time.AfterFunc`), and inflight dedup (concurrent requests for the same key wait on a shared channel). Minters hold live Goja VMs that must be explicitly shut down on eviction.
- **Cipher has a 10-VM LRU with AST + regex fallback.** Memory cache holds at most 10 compiled solver VMs keyed by SHA256 of the player URL (`solverCacheSize`). Disk cache (`~/.cache/yt-cipher/player_cache/`) has a 24-hour offline TTL (`playerCacheTTL`) behind conditional-GET revalidation. Compilation is mutex-serialized to prevent thundering herd. The Goja VM inside each Solvers struct is mutex-protected because Goja is not thread-safe.
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

YouTube authentication relies entirely on cookies — there is no OAuth token flow and no refresh token, despite the heading. The `Auth` struct wraps a `CookieJar` and provides:

- **SAPISIDHASH Authorization header**: `CookieJar.GenerateAuthorizationHeader(origin)` (`internal/cookies/jar.go`). The origin is checked against `allowedSAPISIDHASHOrigins` first and an unlisted origin returns `""` — Google treats the origin as a shared secret between client and server, so no caller may mint a hash bound to an origin it supplied. The header then carries up to three space-separated schemes, one per SAPISID variant the jar holds: `SAPISIDHASH` (`SAPISID`, falling back to `__Secure-3PAPISID`), `SAPISID1PHASH` (`__Secure-1PAPISID`) and `SAPISID3PHASH` (`__Secure-3PAPISID`). One timestamp is captured before the first hash and shared by all of them. Each part is `makeSidAuthorization`'s `{scheme} {timestamp}_{SHA1(timestamp + " " + sid + " " + origin)}` — the raw cookie is never transmitted.
- **Cookie header**: `CookieJar.GetCookieHeaderFor(PlatformYouTube)` (`GetCookieHeader()` is the unqualified YouTube alias). The jar is partitioned per platform, so a Twitch `auth-token` no longer rides along to youtube.com, and pairs are emitted sorted by name so two calls on the same jar produce the same header.

Every long-lived downloader reads its credential at USE time rather than capturing it at construction — the YouTube chat API's `cookieHeader` / `generateAuth` getters (`internal/chat/api.go`), the Twitch VOD chat's `AuthToken` getter (`internal/twitch/vod_chat.go`), and the IRC session's `sessionCredentials` (`internal/twitch/chat.go`) — so a mid-job refresh or re-import is picked up without restarting anything. The jar itself, its admission and expiry rules, and every browser-based acquisition path are in [data-and-storage.md](data-and-storage.md) § Cookies. Host-gating the YouTube cookie header off `googlevideo.com` segment requests was proposed for the same rotation problem and REJECTED (Arc 5, 2026-08-28): its premise was inferred from yt-dlp's request shape and never measured, and its failure would land on members-only captures. The use-time getter is the fix on every host — the segment downloader's `CookieHeader` getter (`internal/engine/downloader.go`) is read once per outbound request and is deliberately not host-scoped.

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

Formats collected from different clients are tagged with an auth level. When deduplicating formats by itag, the format with the **lowest** auth level wins. The tier order mirrors yt-dlp's client priority (`build_innertube_clients`: tv > web > mweb > android > ios), NOT "least privileged first". TV leads because upstream ranks tv first (priority 40) and its URLs need no GVS token; WEB-family formats beat the cookieless clients because Moombox mints and attaches the GVS token their URLs require, while android_vr sits under selective enforcement with no token that applies to it — so TV and WEB formats must win same-itag ties against the cookieless clients:

| Level | Constant | Client | Description |
|-------|----------|--------|-------------|
| 0 | `AuthLevelTVPublic` | TV_DOWNGRADED (no cookies) | Public TV client request. |
| 1 | `AuthLevelTVAuth` | TV_DOWNGRADED (with cookies) | Authenticated TV client request. |
| 2 | `AuthLevelWatchPagePublic` | Watch page (no cookies) | Extracted from ytInitialPlayerResponse in the watch page HTML. |
| 3 | `AuthLevelWatchPageAuth` | Watch page (with cookies) | Same, from an authenticated page load. |
| 4 | `AuthLevelWebSafari` | WEB (Safari UA) | Pre-merged HLS formats when YouTube serves them. |
| 5 | `AuthLevelWeb` | WEB | Standard web client. Provides DASH manifest URLs. |
| 6 | `AuthLevelWebEmbedded` | WEB_EMBEDDED_PLAYER | Embedded player. Age-gate bypass; leads the authed cascade. |
| 7 | `AuthLevelWebCreator` | WEB_CREATOR | Creator Studio client. Access to member-only content. |
| 8 | `AuthLevelVisionOS` | VISIONOS | Cookieless last resort. Direct URLs without cipher. |
| 9 | `AuthLevelAndroidVR` | ANDROID_VR | Cookieless last resort, ranked below VISIONOS (see below). |

VISIONOS ranks above ANDROID_VR deliberately. yt-dlp removed android_vr from every default client list in 2026.08.19 after YouTube began 403'ing all of its format URLs (2026-08-17). Moombox retains it because that enforcement is selective — verified still fully working 2026-08-24 — but under selective enforcement a 403-dead android_vr URL must never displace a working VISIONOS one.

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
| TV_DOWNGRADED | `TVHTML5` | `7` | `5.20260707` | TV Cobalt | Best format coverage. Primary client for both auth and public paths. |
| WEB | `WEB` | `1` | `2.20260708.00.00` | Chrome desktop | DASH manifest URLs. Always tried alongside TV to get manifest. |
| WEB_SAFARI | `WEB` | `1` | `2.20260708.00.00` | Safari desktop | The WEB client under a Safari UA, which used to yield pre-merged HLS. Since 2026-07 YouTube serves those only to some logged-in / "trusted" sessions (yt-dlp demoted it for the same reason, 69ea20006); a logged-out session gets the plain WEB adaptive formats. |
| WEB_CREATOR | `WEB_CREATOR` | `62` | `1.20260708.06.00` | Chrome desktop | Member-only content. Fallback when TV returns `members_only` or `login_required`. |
| WEB_EMBEDDED | `WEB_EMBEDDED_PLAYER` | `56` | `2.20260708.00.00` | Chrome desktop | Age-gate bypass, and first call of the authed cascade (yt-dlp's `_DEFAULT_AUTHED_CLIENTS` lead since 2026.08.19). Needs no PO token. |
| VISIONOS | `VISIONOS` | `101` | `1.02` | Safari / visionOS | Cookieless last resort, yt-dlp's lead default since 2026.08.19. Direct URLs without cipher. HLS-only for live (no `dashManifestUrl`). "Made for kids" videos unavailable. |
| ANDROID_VR | `ANDROID_VR` | `28` | `1.65.10` | Oculus Quest VR | Cookieless last resort behind VISIONOS, and the only cookieless source of a live `dashManifestUrl`. Pinned at 1.65.10 (>1.65 may return SABR-only streams). |

#### Authenticated Fallback Flow

`GetVideoInfoAuthenticated` executes the following sequence:

1. **Fetch watch page** -- GET `https://www.youtube.com/watch?v={videoID}&bpctr=9999999999&has_verified=1` with cookies. The two extra parameters are yt-dlp's age-gate bypass (`_video.py:3809`): without them an age-restricted video answers with the age-gate shell and its embedded player response is lost. Extracts `YtcfgData` (playerURL, visitorData, sessionIndex, delegatedSessionID) and `ytInitialPlayerResponse`.
2. **Parse watch page response** -- If `ytInitialPlayerResponse` was found (tried against 3 regex patterns), parse it as a player response. Collect formats with `AuthLevelWatchPageAuth`.
3. **Extract STS** -- If a playerURL was found and a cipher solver is available, extract the `signatureTimestamp` from the player JavaScript.
4. **Try WEB_EMBEDDED** -- POST with the embedded client (no embed-page fetch; `thirdParty.embedUrl` only). Collect formats with `AuthLevelWebEmbedded`. Purely a format-pool and DASH contributor: WEB_EMBEDDED reports "unavailable" for any embedding-disabled channel, so it must **never** drive playability classification — TV below stays the authority. Failure is logged at Debug and ignored.
5. **Try TV_DOWNGRADED** -- POST to Innertube with TV client, STS, and auth headers. Collect formats with `AuthLevelTVAuth`. If HTTP error occurs, log warning and continue (do not return).
6. **Try WEB** -- POST to Innertube with WEB client. Collect formats with `AuthLevelWeb`. If WEB returns a DASH manifest URL and TV did not, adopt it; failing that, adopt WEB_EMBEDDED's if it has one.
7. **Evaluate TV result** -- Skipped ENTIRELY when TV says the stream is upcoming with `PlayabilityOK` (owner decision O-I, 2026-09-17): an upcoming stream has no formats by definition, so the clients below cannot find any, and a waiting room re-ran all of them on every full fetch it made. That is not a 30-second cadence: the waiting loop's own poll is the lightweight `ProbeVideoStatus`, tiered at 30 s inside the last 5 minutes before the scheduled start, 5 min inside the hour and 10 min beyond it (`calculateProbeInterval`, `internal/worker/stream_processor.go`), and the cascade runs only on a FULL fetch — the periodic metadata refresh every `fullFetchInterval` (30 min), plus a confirmatory `GetVideoInfo` each time the probe reports live/VOD. The confirmatory one is what made it expensive: YouTube's waiting-room slate can fool the probe every poll, so in the imminent tier the whole cascade did re-run every 30 s for as long as the misread lasted. Otherwise, if TV returned `members_only`, `login_required`, or zero formats:
   a. **Try WEB_CREATOR** -- POST with WEB_CREATOR client. Collect formats with `AuthLevelWebCreator`.
   b. **If WEB_CREATOR also fails** (and the error is not `members_only`): **run the cookieless chain** (`tryCookielessFallbacks`) -- VISIONOS then ANDROID_VR, POSTed without cookies but with visitorData, collected at their respective auth levels. An ANDROID_VR result the DASH enrichment below already fetched is handed in rather than re-fetched (`cookielessPrefetch`); before that, an upcoming stream fetched ANDROID_VR twice per poll, because VISIONOS fails `hasAdequateFormats` on an upcoming stream so the chain never broke early.
   c. **If all API clients fail**: Fall back to the watch page player response if available.
8. **ANDROID_VR DASH-only enrichment** -- Skipped on the same waiting-room verdict as step 7. Otherwise, if after WEB_EMBEDDED+TV+WEB no client returned a `DashManifestURL` and the stream is live or upcoming AND not members-only / age-restricted / login-required, fetch ANDROID_VR (cookieless). On success, adopt its `DashManifestURL` and merge its formats into the pool with auth-level dedup. This is a workaround for the YouTube account-based experiment that strips `dashManifestUrl` from cookied clients (yt-dlp issue #15274). ANDROID_VR remains the client here because VISIONOS returns no live `dashManifestUrl` and anonymous TV / WEB / WEB_EMBEDDED refuse live streams outright, and it remains in the roster for every verdict other than the waiting room. Note this step only matters for pools without split adaptive URLs — anything with them takes the manifest-free path and never reads the manifest.
9. **Stream classification override** -- If TV says `not_a_stream` but the watch page disagrees, override the stream status while keeping TV's formats if they are adequate (have both video and audio).
10. **Merge metadata** -- Fill in missing fields (title, channel, description, thumbnails, timestamps) from the watch page response.
11. **Deduplicate formats** -- Across all collected format pools, deduplicate by yt-dlp's stream identity `(itag, audioTrack.id, isDrc)` (`get_stream_id`, `_video.py:3396-3397`) — NOT by itag alone: one client lists several itag-140 entries for a dubbed video that differ only by track, and a DRC rendition is a separate stream rather than a variant. When the same stream appears from multiple clients, keep the one with the lowest auth level. Then, as this port's one deliberate divergence from yt-dlp (which keeps every rendition and carries the identity into its format ids), each itag is **collapsed to a single rendition** — the one the audio-track preference would choose — so `VideoInfo.Formats` holds at most one entry per itag: every consumer downstream (`SelectBestDashStream`, `resolveFormatURLByItag` at setup and on each 403 credential refresh, the format-exclusion retry, the Web picker) looks formats up by itag alone, and several renditions under one itag would let the live path archive a dub or splice a second language into a half-written file. DRM-protected formats (`drmFamilies`) never reach the pool: they are dropped in `parseFormats` with one counted Warn line per extraction, naming the client the way upstream's message embeds `client_name` (upstream reports the same skip with `report_warning(..., only_once=True)`; the count is per response, the line is deduped through the extraction state the cascade carries on its context), because an account-level experiment applies DRM to every video on the tv client (yt-dlp issue #12563) and muxing encrypted samples yields an unplayable archive. A PROBE-ONLY call reports that skip at Debug instead — see the Player-API tokens paragraph under GVS PO Tokens.

**Both cascades exit through `finishExtraction`,** which raises upstream's two terminal verdicts in upstream's order. First the IP block — at least one client was served a player response about a DIFFERENT video and nothing survived (`_video.py:3181-3187`), reported as `ErrAllClientsMismatched` with the substitute's id in the Warn. Then plain exhaustion — no client produced a response and the watch page carried none either (`elif not prs: raise ExtractorError('Failed to extract any player response')`, `_video.py:3188-3189`), reported as `failed to extract any player response: <the last client error>`. The last error is **wrapped**, not replaced, so the `HTTP <code>` that `internal/worker/probe_classify.go` keys on survives into the string the worker stores; that classifier reads only the text ahead of the appended `" — "` body, since YouTube's own `error.message` is not a classification signal.

**Format diagnostics.** What a client's format list LOST is recorded on `VideoInfo.FormatDiag`, because "formats 0" alone cannot tell a dead client from an empty response. `URLlessFormats` and `SabrForced` (`streamingData.serverAbrStreamingUrl`) describe the one response the info was parsed from — together they are the signal that YouTube has forced that client onto SABR (`_video.py:3527-3548`), and the per-client result log lines print both. `URLlessFormats` also feeds stream classification, exactly as the DRM count does: a SABR-forced response still PROVES the broadcast has media, so classifying on the surviving-format count alone would report a finished stream as `upcoming` — the stall the single-client probe path never recovers from. `DRMSkipped` and `CollapsedRenditions` describe the whole extraction, stamped by the cascade's single exit (`finishExtraction`) onto the info it returns: DRM entries never reach a pool, so their count is SUMMED over the responses, while alternate renditions do, so theirs is counted where the collapse happens — at `deduplicateFormats`, after the cross-client merge, so a rendition three clients each returned counts once.

#### Public Fallback Flow

`GetVideoInfoPublic` follows the same pattern for a jar that holds no complete logged-in session (`HasAuthCookies` false). It is not cookieless: only the watch page is fetched without cookies. The Innertube calls below build their headers through `Auth.GenerateAPIHeaders`, which attaches whatever YouTube cookies the jar holds, and a `SAPISIDHASH` Authorization when SAPISID is present — so a half-cleared jar (SAPISID kept, LOGIN_INFO gone) sends its remaining credentials alongside the anonymous watch page's visitor data. Only the cookieless chain (step 3) is credential-free.

1. Fetch watch page (no cookies) and extract ytcfg + player response.
2. Try TV_DOWNGRADED (public, with STS). A TV failure — an HTTP error or a substituted response alike — is logged and the cascade carries on with an empty result, the authenticated path's shape exactly. It used to return TV's error (or the watch-page parse) with VISIONOS and ANDROID_VR never asked.
3. If TV fails or returns inadequate formats, run the cookieless chain (VISIONOS then ANDROID_VR) — unless TV says upcoming with `PlayabilityOK`, which short-circuits both this step and step 4 (owner decision O-I).
4. Apply the same DASH-only ANDROID_VR enrichment as the authenticated path when TV returned no `DashManifestURL` for a live/upcoming stream — reading the answer the chain in step 3 already fetched, when it got that far (see the prefetch slot below), rather than re-fetching android_vr.
5. Fall back to watch page response.

#### The Cookieless Chain (`tryCookielessFallbacks`)

Shared by both paths. Tries VISIONOS, then ANDROID_VR, pooling every fetched format at its own auth level, and returns the first result with OK playability and adequate formats (video **and** audio present).

The slot a caller shares with the chain is a `cookielessPrefetch` (client name, result, error, and whether its formats are already pooled), and it works in BOTH directions because the two paths fetch in opposite orders. HAND-IN: the authenticated path’s ANDROID_VR DASH enrichment runs BEFORE the chain, so it passes the result it already has instead of paying for a second round trip. A failed prefetch is reused too — the retry would be the same request in the same extraction — and a prefetch the caller already pooled is not collected again. WRITE-BACK: the public path’s enrichment runs AFTER the chain, so it hands in an EMPTY slot naming ANDROID_VR and the chain fills it with whatever it fetched; until then that path asked android_vr twice in one extraction on the degraded shape (TV OK-but-inadequate, neither cookieless client adequate, no watch-page response). A slot whose result and error are both nil therefore means “not attempted”, never “skip this client”.

**VISIONOS serves live streams over HLS only and never returns a `dashManifestUrl`** (verified 2026-08-24), while ANDROID_VR does — so the chain will consult the next client for a manifest to adopt into the already-chosen result, but **only when that result cannot already be segment-addressed without one**.

That qualifier is the whole point. A live response carrying split video+audio adaptive URLs routes to the manifest-free `&sq=N` path (`HasSplitAdaptiveFormats`, which `worker.HasManifestlessDashFormats` delegates to), and the strategy switch selects that **ahead of** the `dashManifestUrl` case — it is the primary live path since yt-dlp 8c1f07d81, which skips live DASH manifests entirely. VISIONOS supplies exactly those formats, so a VISIONOS-only live result already has full `--live-from-start` addressability and needs no manifest. The DASH manifest survives only as the fallback for pools **lacking** usable split adaptive URLs, and that is the one case worth another request.

Getting this wrong in both directions is instructive and is why the rule is spelled out here. The first implementation stopped at the first adequate result, and `TestLivePublicExtraction` flagged the missing manifest. The fix then over-corrected — always continuing — on the false premise that a missing manifest meant lost addressability, which cost a needless ANDROID_VR round trip on every anonymous live extraction. The live test now asserts the **capability** (addressable via either source) rather than the mechanism, so it cannot pin ANDROID_VR — the client upstream declared 403-dead — as though it were an invariant. `TestCookielessFallbackDashOnlyWhenNeeded` is the offline gate.

#### Retry Policy

Both `fetchWithClient` and `fetchWithCookielessClient` (used by VISIONOS and ANDROID_VR) implement identical retry logic:

- **Max attempts**: 4 (1 initial + 3 retries)
- **Retryable errors**: HTTP 5xx, HTTP 429 (rate limited), or network errors
- **Non-retryable errors**: Any other HTTP status (e.g., 403, 404) returns immediately
- **Backoff**: Exponential, factor 2, starting at 1 second: 1s, 2s, 4s
- **Context-aware**: Checks `ctx.Err()` before each retry. Uses `utils.Sleep` which respects cancellation.
- **Deadline-bounded**: `doRetryRequest` in `internal/youtube/player_api_strategy.go` skips a sleep that would not leave `delay + 1 s` before the caller's deadline and returns the last real error — the HTTP status the caller is actually being told no by — instead of `context.DeadlineExceeded`, because the ladder alone is 7 s against mid-download 403 credential recovery's 10 s floor.

#### Cookieless Client Specifics

VISIONOS and ANDROID_VR share a dedicated method (`fetchWithCookielessClient`, with `fetchWithAndroidVR` as a thin named wrapper) because they:
- Never sends cookies or auth headers
- Always sends `X-Goog-Visitor-Id` if visitorData is available
- Uses the Oculus Quest VR user agent
- Sets `androidSdkVersion: 32`, `osVersion: "12L"`, `deviceMake: "Oculus"`, `deviceModel: "Quest 3"` in the client context
- Does not pass STS (no cipher support needed -- returns direct URLs)

### Channel Membership Tab

`FetchMembershipVideos` in `channel_membership.go` performs an authenticated GET of `https://www.youtube.com/channel/{id}/membership` (with the current YouTube cookies) and returns the members-only videos listed there — the only discovery source for members-only streams, which RSS/DECAPI never expose. `FetchMembershipVideos` returns four values, not two — `(videos, auth, hasAccess, err)` — and the last three answer different questions. `auth` is a `SessionAuthState`: YouTube's own verdict on whether the page it served was a signed-in session, and the credential-health signal this probe exists to harvest (`SessionAuthUnknown`, never logged-out, whenever no answer was obtained). `hasAccess` says only whether the selected `TAB_ID_SPONSORSHIPS` tab was there — it is the bit the feed monitor's non-member memo keys on, and it is NOT derivable from `videos`: a member whose tab currently lists nothing returns `(nil, …, true, nil)` while a non-member returns `(nil, …, false, nil)`. Memoizing the two as one would delay members-only live discovery by the memo's whole TTL. `videos` is nil when discovery isn't applicable: no auth cookies, or the account isn't a member (the page then falls back to a public tab with no selected membership tab).

Parsing (`parseMembershipTab`):
- Extracts `ytInitialData` via a brace-depth scan that respects string literals (`extractYtInitialDataInto`) — robust on the megabyte-scale channel payload where a non-greedy regex under/over-matches. Handles both `var ytInitialData = {…}` and `window["ytInitialData"] = {…}` forms. Candidates are iterated rather than first-matched (`FindJSONObjectCandidate`, `internal/utils/jsoncandidates.go`), so a forged assignment that scans but is empty or is not JSON is skipped instead of denying the real document. The consumer's own typed decode IS the acceptance test — the caller passes it in, and only the emptiness half of the old predicate (`utils.IsNonEmptyJSONBody`) runs ahead of it — so the literal is no longer `json.Valid`-scanned once and decoded again.
- Locates the membership tab by the stable `tabIdentifier` `TAB_ID_SPONSORSHIPS` (YouTube localizes the visible title), and only when that tab is the SELECTED one — a non-member's fallback page reports `(nil, false)` and is never deep-parsed. The large tab body stays a `json.RawMessage` until then, so the common non-member case is near-zero allocation.
- Walks the selected tab for video IDs across both the current `lockupViewModel` (`contentId`) and classic `videoRenderer`/`gridVideoRenderer` layouts (YouTube A/B-serves both), deduping by video ID.
- Estimates each item's recency (`itemAge`): a live badge, a live item's own "Started streaming N ago" wording (badge or not), or an item with no recognizable timestamp yields Age 0 ("now"), while a "Streamed N &lt;unit&gt; ago" text marks a past VOD ranked by that age. The monitor seeds the feed-history store's `published` estimate from this Age — a dated item is stored `coarse` at (now − Age), an ageless item is stored `assumed` at the current cycle time — so live/upcoming members items always land inside the archive window and get probed. Keying on the ABSENCE of a past-time signal (rather than the presence of a live badge) keeps live/upcoming catching robust to YouTube's badge DOM churn.

### Watch Page Parsing

`FetchWatchPage` in `watch_page.go` performs a GET request to `https://www.youtube.com/watch?v={videoID}&bpctr=9999999999&has_verified=1` (the URL is built by `watchPageURL`; see the authenticated flow's step 1 for the two extra parameters) and extracts:

| Field | Regex | Purpose |
|-------|-------|---------|
| `playerURL` | `"(?:jsUrl\|PLAYER_JS_URL)":"([^"]+)"` | URL of the player JavaScript file. Required for cipher decryption. Prefixed with `https://www.youtube.com` if relative. |
| `visitorData` | `"visitorData":"([^"]+)"` | Session identifier for API requests. |
| `sessionIndex` | `"SESSION_INDEX":"?(\d+)"?` | Multi-account index. |
| `delegatedSessionID` | `"DELEGATED_SESSION_ID":"([^"]+)"` | Brand account session ID. |
| `dataSyncID` | `"datasyncId":"([^"]+)"` | Data sync identifier. |
| `ytInitialPlayerResponse` | Three anchors + brace scan (see below) | Inline player API response embedded in the page. |

The `ytInitialPlayerResponse` is located by an assignment-PREFIX anchor and then brace-scanned
(`extractPlayerResponse`, `internal/youtube/watch_page.go`), never captured by a non-greedy regex.
The anchors are tried in order:
1. `var ytInitialPlayerResponse\s*=\s*\{`
2. `window["ytInitialPlayerResponse"]\s*=\s*\{`
3. `ytInitialPlayerResponse\s*=\s*\{`

Each match ends on the opening brace; `ScanBalancedJSONObject` (`internal/utils/jsoncandidates.go`) then walks the literal tracking JS string
state, so a `};` inside `shortDescription` cannot truncate it. The lazy `({.+?});` form this replaces
stopped at the first `};` in the page and failed identically on all three patterns, losing the watch
page's `ScheduledStartTime` and format pool with nothing in the log.

When found, it is JSON-parsed and the embedded `videoDetails` fields (title, author, channelId, description, thumbnail) are stored in `YtcfgData` for use as metadata fallbacks.

Logged-in state is detected by checking for `"LOGGED_IN":true` or `"isLoggedIn":true` in the HTML.

### Format Selector Algorithm

`SelectBestFormats` in `format_selector.go` implements a single-pass selection algorithm:

#### Video Selection

For each video format (identified by `mimeType` containing "video") with a non-empty URL:

1. **Resolution cap**: `max_video_resolution` compares the SHORTER frame dimension (`Format.CapDimension`, `internal/youtube/types.go`, over `CapDimension` in `internal/utils/resolution.go`), so `2160` recognises a 3840x2160 source and a 2160x3840 portrait one alike. `SelectByCap` (`internal/utils/resolution.go`) resolves the cap to one size — the largest at or below it, or the CLOSEST size above it when a video offers nothing that small — and `0` means unbounded. Formats at any other size are skipped. The cap is a preference among the qualities YouTube offered, never a filter that can leave a job with no video.
2. **Resolution comparison**: within the chosen size, the higher `MaxDimension` wins — the rung that separates an anamorphic 2560x1080 from a 1920x1080.
3. **FPS tiebreaker** (same resolution): If `prefer60fps` is true, higher FPS wins. If false, lower FPS wins.
4. **Codec score tiebreaker** (same resolution, same FPS): Higher score wins. Scores are assigned by regex pattern matching against the codec string extracted from the `mimeType` field:

Patterns are tested in order, so the more-specific `vp9.2` pattern precedes `vp9`. The order mirrors yt-dlp's default `FormatSort` vcodec preference (`utils/_utils.py`): av01 > vp9.2 > vp9 > h265/hevc > h264/avc > vp8.

| Pattern | Score |
|---------|-------|
| `^av0?1` (AV1) | 6 |
| `^vp0?9\.0?2` (HDR) | 5 |
| `^vp0?9` | 4 |
| `^([hx]265\|he?vc?)` (HEVC) | 3 |
| `^([hx]264\|avc)` (H.264) | 2 |
| `^vp0?8` | 1 |

5. **Bitrate tiebreaker** (same resolution, FPS, codec): **Higher** bitrate wins. This matches yt-dlp's `size`/`br` sort, which prefers the higher-quality / Premium stream at the same res/fps/codec (e.g. the AV1 Premium itag 721 over the base AV1 399). YouTube only serves a Premium format's URL to an entitled account, so a non-entitled session never sees one here.
6. **Auth level tiebreaker** (same everything): Lower auth level wins (more accessible URL).

#### Audio Selection

For each audio format (identified by `mimeType` containing "audio") with a non-empty URL:

1. **Audio track identity**: Decided before codec and bitrate — `audioTrackScore` ports yt-dlp's `get_language_code_and_preference` (original > YouTube's default > unlabelled > descriptive), and within one track the clean rendition beats its `isDrc` (loudness-normalised) twin.
2. **Codec score**: Higher wins. Scores:

| Pattern | Score |
|---------|-------|
| `^opus` | 4 |
| `^mp4a\.40\.[25]` (AAC-LC or HE-AAC) | 2 |
| `^mp4a` (generic AAC) | 1 |

The two AAC profiles share a score, as they share yt-dlp's `mp4a` rank, so the bitrate tiebreaker below decides between them: with HE-AAC scored above AAC-LC, a pool with no Opus took itag 139 (48 kbps) over itag 140 (128 kbps).

3. **Bitrate tiebreaker**: **Higher** bitrate wins (audio quality scales with bitrate).
4. **Auth level tiebreaker**: Lower auth level wins.

#### Manual Override

`SelectWithOptions` allows manual itag selection. If a `videoItag` or `audioItag` is provided:
- Value `-1` means "skip this track entirely" (e.g., download audio only).
- Any other value selects that specific itag if found in the format pool.
- Unresolved tracks fall back to auto-selection.

#### The Whole-File VOD Path

`selectVodFormats` (`internal/worker/strategy_youtube_vod.go`) runs `SelectBestFormats` with the job's `quality_preference` folded into its two knobs (`vodSelectionBounds`): a preferred size below `max_video_resolution` becomes the cap — the selector then takes that size or the largest below it, as the live paths' preference matching does — and an explicit `…p60` asks for 60 fps whatever `prefer_60fps` says, while a suffix-less preference leaves the setting in charge. Before this the path read the preference only for `audio_only`, so a `720p` channel's uploads and finished VODs downloaded at the global cap. When a chosen format's URL will not resolve (`resolveVodURLs`), the alternate is that same selection re-run over the pool without it (`reselectVodWithout`), and the video arm adopts the re-selection's whole stream shape — a progressive primary replaced by a video-only alternate gets the audio the selection paired with it, and an audio-only job's alternate is another audio format. Pinned by `TestVodSelectionHonoursQualityPreference` and `TestVodReselectionKeepsTheJobsSelection` (`internal/worker/vod_selection_test.go`).

Whatever the selection picks stages to the same two names — `video.mp4` and `audio.m4a` — so a resume has to be told WHICH file the staged bytes are a prefix of. Each downloader `DownloadVod` builds carries an engine `StreamID` of the video, the itag and the format's `ContentLength` (`vodStreamID`, `internal/worker/strategy_youtube_vod_wiring.go`), which the sidecar records and `resumeIdentityMismatch` (`internal/engine/downloader_resume.go`) compares first; a mismatch starts the whole-file download over (the path is outside the no-truncate guard — its partial is re-fetchable). Before this a restart or Resume whose selection had moved — a changed `max_video_resolution` or `prefer_60fps`, a missing_pot degrade, a different format pool — appended the new rendition to the old one's checkpoint, and the spliced file muxed with `-c copy` and finished clean as a short, garbled archive. Two engine checks back the StreamID up for a sidecar written before it existed and for any caller that sets none: `streamIdentity` reads a finished VOD's `id=o-…` URL as its itag plus `clen` (the opaque `o-` token and every session parameter may rotate between extractions), and the sidecar records the probed total (`ResumeState.TotalSize`), so a resume whose own probe answers another total — or a partial already longer than the file — starts over through `discardStagedMedia` (`internal/engine/downloader_direct.go`). A resume whose probe failed (an outage as it starts, a refused or `*`-total probe) is held to the same rule by the streaming fallback, of the total its resume Range's answer states (`differentFileReason`, `parseContentRangeTotal`): a 206 or a `bytes */<total>` 416 naming another file discards the partial and streams the file again from byte 0, and a 416 short of the recorded total is a short origin — an error that keeps the checkpoint — rather than "already complete". Before this an outage that failed the probe let a resume append the new file's tail to the old one's checkpoint, or finish a partial of a longer file as the archive. A discard also forgets the recorded total, so the checkpoints of what is fetched next never hold it to the discarded file's. Pinned by `TestVodResumeChecksTheRendition` (`internal/worker/vod_resume_identity_test.go`), `TestResumeIdentityWholeFileVodURL`, `TestDirectResumeRefusesADifferentTotal`, `TestDirectResumeRefusesAPartialLongerThanTheFile`, `TestDifferentFileReason`, `TestDirectFallbackResumeRefusesADifferentTotal` and `TestDirectFallbackResume416HoldsThePartialToItsFile` (`internal/engine/downloader_direct_identity_test.go`).

A googlevideo URL lives about six hours, and a long whole-file transfer outlives it: the next chunk answers 403, which used to end the job after one attempt. Each downloader `DownloadVod` builds now carries an `OnCredentialRefresh` (`vodURLRefresh`, `internal/worker/strategy_youtube_vod_wiring.go`) that the engine calls on a chunk refused with 403 or 410. `refreshVodURL` re-fetches the player response through the live refresh's seam (`refreshVideoInfo`) and resolves the format serving the SAME file (`sameVodFile`): the same itag, audio track and `ContentLength`, from a client of the same GVS token class — behind a winner of the other class it takes the winner's `TokenFreeAlternate`, the copy a missing_pot stream rides. A token-free stream the fresh pool does not serve is asked of the cookieless chain again (`fetchCookielessFormats`, under the same bound): a stream `DownloadVod`'s missing_pot re-extract served came from there because the extraction's web_creator pool was adequate and carried no shadow, and the re-fetch runs the same cascade and carries none either, so without it the 2026-09-29 incident's stream could never be refreshed and its expired URL ended the job on the 403. A stream that needs a token is never in that chain and does not ask it. A matching itag of another length is a re-encode and returns nothing, so the engine's 403 stands rather than a different rendition being appended; the engine checks the fresh URL's fingerprint again before installing it (`refreshDirectURL`, `internal/engine/downloader_direct.go`) and asks at most twice per chunk. The token half runs first and follows the SERVED stream's client, not the setup pool's winner, so a stream riding a shadow stays bare; a tokenised one is re-minted past the cache. The round trip is bounded by `credentialRefreshTimeoutFor`, as on the live paths. The strategy also hands both downloaders the orchestrator's `IsOnline` — `DownloadVod` takes it as its last parameter, like the live strategies — so a failure during an outage is waited out instead of charged; the engine half (the chunk ladder, and the outage verdict it waits for before charging a last attempt) is described in architecture.md. Pinned by `TestRefreshVodURLServesTheSameFile`, `TestVodDownloadersWireRefreshAndConnectivity` and `TestVodMissingPotStreamRefreshesItsURL` (`internal/worker/vod_url_refresh_test.go`), and on the engine side by `TestDirectChunkRefusedRefreshesTheURL`, `TestDirectRefreshRefusesAnotherFile`, `TestDirectRefreshIsBounded`, `TestDirectChunkWaitsOutAnOutage` and `TestDirectOutageVerdictHonoursCancel` (`internal/engine/downloader_direct_refresh_test.go`).

#### Live Selection

The live strategies choose from the DASH representations or the manifestless pool (`SelectBestDashStream`, `internal/worker/format_utils.go`) or from an HLS master playlist (`selectHlsVariant`, `internal/worker/strategy_youtube_hls.go`), and the 30 s quality probe (`buildYouTubeProbeFn`, `internal/worker/orchestrator_youtube.go`) selects with exactly the same inputs — a probe that ranked differently from the downloader would report a quality change on every tick. Sizes are the frame's SHORTER edge throughout, the cap's own measure, so a portrait stream's `1080p` is its 1080x1920 rendition. A pinned itag wins outright; then the cap resolves to one size as above; then a `quality_preference` is matched at its size, descending to the next lower size when that one is missing; otherwise the renditions AT the chosen size are ranked. Within one size the frame rate decides before bandwidth: an explicit `…p60` asks for 60 fps, and otherwise `prefer_60fps` does — 50 fps and up when set, a stated 31 and below when not — exactly as the VOD selector's FPS rung honours it. A rate the manifest does not state is never "preferred", so a ladder without frame rates ranks by bandwidth alone. Pinned by `TestLiveSelectorsHonourPrefer60fps`, `TestPortraitPreferenceMatchesTheShortEdge` and `TestEveryVideoSelectionCarriesPrefer60fps` (`internal/worker/fps_preference_test.go`).

### Stream Status Classification

The `classifyStream` function determines the stream's lifecycle state (`youtube.StreamStatus`: `upcoming`, `live`, `post_live`, `vod`, `not_a_stream`) from multiple signals in the player response:

The rules run in this order; the first that matches decides (`classifyStream`, `internal/youtube/player_api_parsing.go`):

| Condition | Result |
|-----------|--------|
| `playabilityStatus.status == "LIVE_STREAM_OFFLINE"`, or `"UNPLAYABLE"` with a reason saying the live event will begin (`isUpcomingFromPlayability`) | `upcoming` |
| Premiere (has a scheduled start, not live content, reason contains "premiere" or `videoDetails.isUpcoming`), not live now, no formats | `upcoming` |
| `videoDetails.isUpcoming == true` and no formats — overrides `isLive`, which YouTube sets on a waiting room once the scheduled time passes | `upcoming` |
| `videoDetails.isLive == true` OR `liveBroadcastDetails.isLiveNow == true` (a live premiere included) | `live` |
| `playabilityStatus.liveStreamability` renderer present and no formats — a scheduled stream some clients report without `isUpcoming` | `upcoming` |
| No `liveBroadcastDetails`, not `isLiveContent`, not a premiere | `not_a_stream` |
| No formats but has `liveBroadcastDetails` or `isLiveContent` | `upcoming` |
| `liveBroadcastDetails.endTimestamp` present and not live now | `post_live` (DVR) |
| Default | `vod` |

### Playability Status Parsing

The `parsePlayabilityStatus` function classifies the video's accessibility:

| Status Code | Condition | PlayabilityError |
|-------------|-----------|-----------------|
| `OK` | -- | `ok` |
| `LIVE_STREAM_OFFLINE` | -- | `ok` (upcoming, not an error) |
| `UNPLAYABLE` + "live event will begin" | -- | `ok` (upcoming) |
| any status but `OK` | `desktopLegacyAgeGateReason` is truthy | `age_restricted` |
| any status but `OK` | reason contains "confirm your age" / "age-restricted" / "inappropriate" | `age_restricted` |
| `AGE_VERIFICATION_REQUIRED`, `AGE_CHECK_REQUIRED` | -- | `age_restricted` |
| `LOGIN_REQUIRED` + "member"/"join" in reason | -- | `members_only` |
| `LOGIN_REQUIRED` | -- | `login_required` |
| `UNPLAYABLE` + "member" in reason | -- | `members_only` |
| `UNPLAYABLE` + "private" in reason | -- | `private` |
| `UNPLAYABLE` + "country"/"region"/"not available in your" | -- | `region_blocked` |
| `UNPLAYABLE` + "unavailable" | -- | `unavailable` |
| `ERROR` + "private"/"unavailable" | -- | `unavailable` |
| Anything else | -- | `unknown` |

The two age rows sit above the status switch because the shapes they catch are spread across `AGE_CHECK_REQUIRED`, `UNPLAYABLE` and `LOGIN_REQUIRED`. They sit below the upcoming rows because a waiting room is not an error, and they exclude `OK` for the same reason: a response YouTube says is playable is not an error either, and `checkPlayability` aborts the job on every non-`ok` verdict — with the notification suppressed for `age_restricted`, so an `OK` response reclassified this way would end a downloadable stream in silence. Both exclusions are conditions in the code, not merely row order, so the table reads the same whichever way it is scanned. They port yt-dlp's `_is_agegated` (`_video.py:2894-2904`), whose own consumers only ever append clients (`_video.py:3157-3175`) rather than override a playability verdict; the reason substrings are the load-bearing half, since upstream's lower-case status entries are substring-matched against the raw upper-case `status` and so only ever match through the reason. The verdict matters because the web_embedded age bypass gates literally on `age_restricted`.

### N-Parameter and Signature Decryption

Extraction does not solve ciphers: the parser keeps each format's raw URL and, for a `signatureCipher` entry, its encrypted signature, and `PlayerAPI` holds the goja resolver only for `GetSts` (the signature timestamp a player request carries). The chosen format is resolved after selection, by the worker, through `cipher.ResolveFormatURL` (`internal/cipher/decrypt.go`) — so only the formats actually downloaded pay for solving.

`ResolveFormatURL` hands `RoutedResolveURL` the stream URL, the player URL and any encrypted signature:

1. **Signature** (when present): solved by the routed solver (the sidecar's V8 ejs). If that fails the whole URL falls back to the goja resolver, which also handles n, so the two are never partially applied. The decrypted value is appended as `{sp}={sig}` (`sp` defaults to `signature`).
2. **n-parameter**: solved by the routed solver, falling back to goja for that parameter alone. The encrypted value is swapped for the decrypted one by string replacement in the raw URL, which keeps the original parameter order — `url.Values.Encode()` sorts parameters alphabetically, and a reordered URL fails YouTube's signature check with HTTP 403. A failed n solve is a deliberate degrade, not an error: an encrypted n still serves, only throttled, so it is logged and the URL is used as is.

Manifest and refreshed URLs that are not a parsed format go through `RoutedDecryptNInURL`, which also handles the path-encoded form (`/n/{value}/`, at least 10 characters).

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
| `StreamMetadata` | `ad022ca32220d5523d03a23cbcb5beaa1e0999889c1f8f78f9f2520dafb5cae6` |
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
- `platform=web` -- Enhanced-broadcast opt-in (see below).
- `player=twitchweb` -- Player identifier.
- `playlist_include_framerate=true` -- Include frame rate metadata.
- `sig={token.Signature}` -- Access token signature.
- `supported_codecs=av1,h265,h264` -- Enhanced-broadcast opt-in (see below).
- `token={token.Value}` -- Access token value.
- `type=any` -- Accept any stream type.

`platform` and `supported_codecs` are byte-for-byte what yt-dlp sends (`references/yt-dlp/yt_dlp/extractor/twitch.py`, `_extract_twitch_m3u8_formats`), on the live and the VOD URL alike. They are the opt-in to Twitch **enhanced broadcasts**: a channel that multi-encodes then offers an HEVC or AV1 source alongside the H.264 one, typically at a higher resolution, and without them Usher never lists it and the capture takes the H.264 transcode. The short names here are the REQUEST spelling; the playlist answers in RFC 6381 codec ids (`av01…`, `hev1…`/`hvc1…`, `avc1…`). No container work follows from an AV1 source: yt-dlp forces `-f mp4` on its own ffmpeg downloader because that downloader would otherwise keep the mpegts container end to end, whereas Moombox writes raw segments (`internal/engine`) and always muxes to MP4 with `-c copy` (`Muxer.buildArgs` in `internal/engine/muxer.go`) — the state that flag exists to force.

#### Master Playlist Parsing

`ParseHLSMasterPlaylist` in `hls.go` parses `#EXT-X-STREAM-INF` tags using regex extraction:

| Attribute | Regex | Field |
|-----------|-------|-------|
| `BANDWIDTH` | `BANDWIDTH=(\d+)` | `Bandwidth` |
| `RESOLUTION` | `RESOLUTION=(\d+)x(\d+)` | `Width`, `Height` |
| `FRAME-RATE` | `FRAME-RATE=([\d.]+)` | `FPS` |
| `VIDEO` | `VIDEO="([^"]+)"` | `VideoGroup` |
| `CODECS` | `CODECS="([^"]*)"` | `Codecs`, and `VideoCodec` via `videoCodecFamily` |

`videoCodecFamily` (`internal/twitch/hls.go`) normalizes the raw list to `"av01"`, `"hevc"`, `"avc1"` or `""`. It scans the list in order rather than reading its first entry: the video codec is not always first, and an audio-only rendition's single `mp4a` entry must not be read as one. Twitch usher playlists DO carry `CODECS` without the enhanced-broadcast opt-in (`internal/engine/manifest_test.go`'s Twitch fixture; the live gate reads `avc1` on five of six variants) — a pre-enhanced playlist simply lists only H.264 renditions, so every source in one reports `"avc1"`. The fields stay empty only for a variant with no video track, or a playlist that sends no `CODECS` at all.

Source quality detection: a variant `IsSource` is true if `VideoGroup` equals `"chunked"` or contains the string `"source"` (case-insensitive).

Variant naming: if `VideoGroup` is set, use it as the name. Otherwise, construct `{height}p{30|60}` from resolution and FPS.

#### Variant Selection Algorithm

`SelectBestVariant` in `hls.go` selects a variant given a quality preference string, the max resolution and `prefer_60fps`. The preference is always the job's `twitch_quality_preference` (owner decision D-T9; `docs/spec/data-and-storage.md` has the column), at the capture start and at every re-selection during it. It used to be `twitch_quality` at the start — a column the stream start overwrote with the picked variant's name, so a restarted job re-selected by "chunked" or "720p60" instead of by what it was created to record — and `quality_preference` at the re-selections. `twitch_quality` is now only the variant being recorded, rewritten whenever a split moves the capture to another one. Pinned by `TestTwitchSelectionReadsOnlyThePreference` and `TestTwitchQualitySplitRecordsTheNewVariant` (`internal/worker/twitch_quality_preference_test.go`).

1. **Audio-only**: If `qualityPref == "audio_only"`, find a variant with "audio_only" in its name. Fall back to the last variant (lowest quality).
2. **Filter**: Remove all audio_only variants from the candidate list.
3. **Resolution cap**: `utils.SelectByCap` (`internal/utils/resolution.go`) resolves `max_video_resolution` — compared against the SHORTER frame dimension, `0` = unbounded — to one size: the largest at or below the cap, or the CLOSEST size above it when the channel offers nothing that small. Everything at or below that size is kept, so the quality preference below still has a ladder to search. The cap can never empty the list; it used to compare the long edge and, when the filter emptied, silently fall through to the UNFILTERED variants.
4. **Quality preference matching**:
   - Parse preference string like `"1080p60"` into `(height=1080, fps=60)`.
   - Try exact height match. If FPS is specified, prefer highest bandwidth among FPS matches.
   - If no exact match, descend to the next lower available height.
   - If preference is a non-numeric string, do substring matching on variant names.
5. **Chosen-size ranking** (codec-aware): if no quality preference matched, rank the variants AT the size the cap resolved to (`rankAtChosenSize`, `internal/twitch/hls.go`) by video family (`codecRank`: AV1 > HEVC > H.264 > absent), then the frame rate `prefer_60fps` asks for (`preferredFrameRate`: 50 fps and up when set, a stated 31 and below when not — the YouTube selectors' thresholds), then a `IsSource` rendition over a transcode, then the higher bandwidth. Ties keep the earlier variant, so two indistinguishable renditions resolve in playlist order. Owner ruling R2 (2026-09-19) put the codec ahead of the source flag: an enhanced AV1 rendition is the better archive whether or not Twitch flags it `chunked`. Pinned by `TestSelectBestVariantCodecRankAtTheChosenSize` and `TestSelectBestVariantWithoutCodecsRanksBySizeThenBandwidth` (`internal/twitch/hls_test.go`).

   The frame-rate rung is owner decision D-Y2 (2026-10): Twitch read `prefer_60fps` nowhere, so a broadcaster's 1080p60 source beat a 1080p30 transcode on the source flag whatever the setting said. A variant's rate is the master playlist's `FRAME-RATE` attribute (`TwitchHLSVariant.FPS`), which Twitch sends on every rendition, live and VOD; a rendition without one is preferred under neither setting, as an unstated rate is on the YouTube side. The rung sits below the codec — an AV1 30 fps rendition still beats an H.264 60 fps one — and above the source flag. Every Twitch selection site is handed the setting: the capture start (`StreamProcessor.selectTwitchVariant`, live and VOD, read fresh from the config) and every re-selection during a capture — the 30 s quality probe and `refreshBestVariant` both call `TwitchVariantInfo.selectFrom`, whose inputs `newTwitchVariantInfo` copies from the job context. Pinned by `TestSelectBestVariantFrameRateFollowsPrefer60fps` (`internal/twitch/hls_framerate_test.go`), `TestTwitchCaptureStartHonoursPrefer60fps` and `TestTwitchReselectionCarriesPrefer60fps` (`internal/worker/twitch_prefer60fps_test.go`).
6. **Bandwidth fallback**: select the variant with the highest bandwidth. Defensive only — `rankAtChosenSize` answers from the same list `SelectByCap` resolved the size from, so some variant always matches it (a playlist with no `RESOLUTION` at all resolves to size 0 and every variant matches). Kept so the function has a total answer if either rule is ever changed independently.

The source flag is a tie-break below the codec and the frame rate now, not the reverse (ruling R2 inverted the codec and source rungs; D-Y2 put the frame rate between them), and none of them overrides the quality preference: an operator who asked for `1080p60` still gets a 1080-high variant, pinned by `TestSelectBestVariantHonoursAnExplicitHeightOverTheEnhancedSource` (`internal/twitch/hls_test.go`).

**The resolution cap no longer decides whether an enhanced source is a candidate.** Step 3 still runs FIRST — before the quality preference and before the chosen-size ranking — but since owner ruling R1 (2026-09-19) it compares the SHORTER frame dimension (`utils.CapDimension`, `internal/utils/resolution.go`), so a 2560x1440 enhanced source measures 1440 and a 3840x2160 one measures 2160: both are admitted at `max_video_resolution`'s shipped default of 2160, and nobody has to raise the key to receive an enhanced rendition. The same rule is what makes a 720x1280 portrait stream count as 720p rather than 1280p, and `internal/youtube/format_selector.go` applies it identically. The cap also cannot empty the candidate list any more: `utils.SelectByCap` resolves it to the largest size at or below the cap, or — when a channel offers nothing that small — the CLOSEST size above it, and `0` means unbounded. Every Twitch caller passes the config value unconditionally (`internal/worker/stream_processor_twitch.go` for live and VOD, `internal/worker/orchestrator_twitch.go` for the 30 s quality probe and `refreshBestVariant`). Pinned by `TestSelectBestVariantEnhancedSourceUnderTheDefaultCap` (`internal/twitch/hls_test.go`): at a cap of `2160` the 2560x1440 HEVC source is selected, at `1080` the 1920x1080 H.264 source is.

### Twitch Authentication

Twitch auth is a single bearer token, and Moombox is **validate-only** on it: nothing in the process ever mints, rotates or writes back a Twitch credential.

`Auth` (`auth.go`) reads `auth-token` out of the shared `CookieJar` via `GetAuthToken`. `GetCredentials` reads the token and the `login` account name TOGETHER, through `CookieJar.GetTwitchCredentials` and one `RLock` — a pair read under two locks is not a pair, and a concurrent jar `Reload` could otherwise hand the IRC handshake a token from one account beside a login from another. The local `login` cookie is preferred over the authoritative one in Twitch's validate response because the IRC connect path must work when the network is flaky.

`ValidateToken` GETs `https://id.twitch.tv/oauth2/validate` with an `Authorization: OAuth {token}` header and treats exactly two statuses as answers: 401 is "invalid", 200 is "valid". Anything else is an error, not a verdict. The 200 body carries `login` and `client_id` as well as `user_id`, and **only `user_id` is decoded** — a field that does not exist cannot be added to a log line by accident, and the success line at Debug names that opaque numeric id alone. An unexpected status renders through `validateErrorDetail`, which reports the PARSED and clamped media type (≤64 chars) plus at most 200 bytes of body, and only for `text/plain` or `application/json`; every other type is named with its body omitted, and the remainder is drained bounded so the connection stays reusable. The bound is not fastidiousness: the two bodies that actually appear on this path are an intermediary's HTML sign-in page and a service error page that echoes the request — including the `Authorization` header.

`RefreshService.checkTwitchAuth` (`internal/cookies/refresh_twitch.go`) is the same endpoint from the cookie side, and is likewise a check with no rotation. An empty `auth-token` returns "not authenticated" WITHOUT making a request: with no credential, nothing the endpoint replied could be a verdict about this install. There is no Twitch counterpart to the YouTube `Set-Cookie` capture — `processYouTubeSetCookies` is YouTube-only and `trackedCookieName` refuses any origin off the Google platform.

**The tier-2 entitlement probe.** `Service.ProbeSessionLiveness` (`internal/twitch/liveness_probe.go`) answers the question validate cannot: it requests the stream playback access token for one channel — the same `API.GetStreamAccessToken` call `GetHLSMasterPlaylist` makes, with no playlist fetched afterwards — and classifies the token document with `PlaybackTokenSession`. `user_id` present is signed in, JSON null is anonymous, anything else is inconclusive. It returns `(signedIn, conclusive bool, err error)` and NEVER the token: the error is synthesised rather than passed through, because `gqlRequest` interpolates the response body into its 4xx and auth errors and an intermediary's error page can echo the `Authorization` header — the same body `validateErrorDetail` clamps for above. Two guards refuse before any request goes out: no `auth-token` in the jar (a cookieless install gets an anonymous token by design and must never be told its credentials failed) and no usable channel login (`safeLoginRe`, the same rule the query builder applies). The token is read ONCE and used for both the request and the guard, the discipline `GetHLSMasterPlaylist` documents. `cmd/moombox` wires it to `cookies.RefreshService.TwitchFallbackLiveness` under a 20-second timeout, reading the configured channel list live on every call; see `data-and-storage.md` § Refresh Service for the conditions and the pilot gate, armed since 2026-09-03.

**There is no in-process Twitch keepalive, and that is a fact about the platform rather than a gap in this code.** yt-dlp never writes `auth-token` back; `references/chatterino7` refreshes Kick tokens and only DETECTS expiry for Twitch. Those two readings are the whole basis for the conclusion, and it is a conclusion rather than something this code calls: no in-process issuer of a fresh `auth-token` was found in either, so a browser sign-in on Twitch's own login page is the only issuer observed. Moombox's own code never names that endpoint; yt-dlp's extractor does (`_LOGIN_POST_URL` in `references/yt-dlp/yt_dlp/extractor/twitch.py`), for a username/password login flow Moombox does not implement. The headless refresh does navigate to `https://www.twitch.tv` on every periodic pass in which the jar still holds a Twitch cookie — `refreshPlatforms` (`internal/cookies/autocookies.go`) includes the platform only when `HasAnyTwitchAuthCookie()` is true, and `platformRefreshURLs` is what maps it to that URL — and **whether that navigation renews the token is UNMEASURED**. The settling observation costs one run and now has a surface: **read the two log lines.** `twitchAuthHorizon` appears on the startup `Cookies loaded` line and again on `cookie refresh succeeded` — the refresh's SUCCESS arm, and the only one of its three outcome-switch arms that carries a horizon at all — after a browser refresh has written and reloaded the jar; same producer (`CookieJar.HorizonLogFields`), same ISO-8601 UTC formatting, so comparing them across one refresh answers it directly. The observation is only available when the refresh lands on that arm: the historically common Firefox outcome this file documents, "cookies still verify, but this pass could not confirm the browser refreshed the profile", logs no horizon (`data-and-storage.md §Cookie Jar`). A timestamp, never a value. No UI carries a horizon and none is planned; this is a diagnostic to read out of the log, not a measurement that needs a test harness.

### IRC Chat (Live)

#### Connection Protocol

`ChatDownloader` in `chat.go` connects via WebSocket to `wss://irc-ws.chat.twitch.tv:443` using `github.com/coder/websocket`. The read limit is set to 512 KB.

**The handshake is a PAIR, decided once per session.** `runIRCSession` (`chat_irc.go`) reads the credentials through `sessionCredentials()` — ONE call, so the token and the login always describe the same session even if a jar `Reload` lands mid-handshake — and passes them to `ircHandshakeLines`, which renders BOTH wire lines from one decision:

| Handshake | PASS | NICK |
|-----------|------|------|
| Authenticated | `PASS oauth:{token}` | `NICK {login}`, lowercased |
| Anonymous | `PASS SCHMOOPIIE` (`anonymousIRCPass`) | `NICK justinfan{0-99999}` |

Authenticated requires a non-empty token AND a non-empty login AND a login with no row-breaking character (`hasRowBreakingChar`: space, tab, CR, LF, NUL — a value that cannot be spoken as one IRC parameter is not a usable identity). Anything else falls all the way back to the anonymous pair. The hybrid — a real token beside the `justinfan` nickname — is what Twitch answers with `Login authentication failed` or silently downgrades, and the handshake decision does not depend on parsing NOTICE (`ircIsLoginFailureNotice` in `chat_irc.go` only classifies the reply afterwards), so the two lines must not come from two conditions that can drift apart. Before this pairing existed the NICK was always `justinfan`, so a session holding a perfectly good token authenticated as nobody. Upstream shape: `references/chatterino7/src/providers/twitch/TwitchIrcServer.cpp`.

Then `ircCapRequest` (`internal/twitch/chat_irc.go`) — `CAP REQ :twitch.tv/tags twitch.tv/commands` — and `JOIN #{channel_login}` (lowercased). `twitch.tv/tags` carries the emote ranges, badges, message ids and timestamps the archive is made of; `twitch.tv/commands` carries USERNOTICE, NOTICE and RECONNECT. `twitch.tv/membership` is deliberately NOT requested (owner decision O-S): it delivers JOIN/PART bursts for channels under 1,000 chatters, `parseLine` drops both, and chatterino asks for it only because it renders a user list (`references/chatterino7` TwitchIrcServer.cpp).

#### Anonymous Fallback and the Downgrade Report

**One-shot anonymous fallback.** A credentialed session that ends having HEARD from Twitch but never received `001` (RPL_WELCOME) had its login refused. `noteHandshakeOutcome` (`chat.go`) latches `authRefused`, `sessionCredentials` then returns an empty pair, and every later reconnect in this job uses the anonymous handshake rather than spending the reconnect budget on a login that cannot succeed — unless the credentials change. The latch is one-shot per CREDENTIAL PAIR rather than per job: `Reauthenticate()` clears `authRefused`, `downgradeReported` and `warnedNoLogin` together and drops the live session, so a repaired `cookies.txt` gets one fresh authenticated handshake and a second refusal latches and reports again. Its own drop is charged nothing against the reconnect budget below and skips the backoff, and `runIRCSession`'s handshake-outcome defer refuses to read it as a refusal (`reauthPending`). The trigger is the ABSENCE of `001`, not the NOTICE text — `ircIsLoginFailureNotice` only decides which of two Warns is written, and `001` covers every way a login can be refused. `heardFromServer` is the second half of the trigger and separates a REFUSAL from a DROP: a session that read nothing at all learned nothing about the login, so it must not latch. That distinction is load-bearing rather than fastidious — the orchestrator relaunches chat the moment a connectivity outage is declared over, precisely when the link is least trustworthy, so without it one unlucky reconnect on a marathon stream would turn subscriber-only chat off for days. The fallback is skipped when the caller cancelled or the downloader was stopped, it never names a credential, and it does not change the reconnect budget below.

**Four states a job WITH credentials can go anonymous in CHAT.** The `AuthDowngrade*` vocabulary (`chat.go`) has a FIFTH member beyond these four — the playback-token route, documented below under "A job with chat recording off" — but only these four reach `ChatDownloaderOptions.OnAuthDowngrade`. Each of the four below is one opaque token per route — deliberately not sentences, and with no format verb to interpolate a credential into, so a consumer can put a reason anywhere including a notification body:

| Reason | Meaning | Reported by |
|--------|---------|-------------|
| `login-refused` | Twitch answered with one of the two refusal NOTICEs | `noteHandshakeOutcome` |
| `login-never-acknowledged` | Twitch spoke but never sent `001` and named no reason | `noteHandshakeOutcome` |
| `no-login-cookie` | an `auth-token` with no `login` beside it, so the authenticated handshake is never attempted at all | `noteMissingLogin` |
| `unusable-login-cookie` | an `auth-token` beside a `login` that cannot be sent as a nickname | `noteMissingLogin` |

`noteMissingLogin` Warns on an ABSENT **or** an UNSENDABLE `login` — one predicate, `hasRowBreakingChar`, decides both the handshake and the report, so a handshake that goes anonymous for a reason the report does not recognise cannot happen. Its Warn is the only thing that can see those two states: `HasTwitchAuthCookies` reads true (the token IS there) and both UIs show green while the capture silently drops every subscriber-only message and badge for the whole job. Both inputs are reachable on day one — a hand-written `cookies.txt` carrying only `auth-token`, and the same file with `login` filled in as a display name with a space in it. A cookieless install is not a degradation and is never warned about; the token check at the top of the function is load-bearing, not defensive.

**One notification per credential pair.** `reportAuthDowngrade` latches across every trigger site (a THIRD flag, not a reuse of the per-site `warnedNoLogin` or the behaviour-changing `authRefused`), and the worker does TWO things with the one report: it turns it into "Twitch chat is anonymous for {channel}", and it marks the PLATFORM through `cookies.RefreshService.NoteTwitchAuthLoss` (the injected `StreamProcessor.onTwitchAuthLoss` seam, so `internal/worker`'s production code still does not import `internal/cookies` — only its tests do, which is what pins the two vocabularies against drift). The notice names the job; the mark names the platform, fires `OnRecoveryNeeded("twitch")` through the ordinary dedupe, and sticks against a validate 200 until the credential pair changes — see `data-and-storage.md` § Cookies. Neither replaces the other: without the notice nobody knows WHICH capture is degraded, and without the mark nothing attempts recovery. The latch is per credential pair, not per job, because `Reauthenticate` resets it.

`twitchChatDowngradeNotice` (`internal/worker/stream_processor_twitch.go`) renders the title, the `auth` event and the Channel / Job / Reason fields, and `sendTwitchChatDowngrade` is what sends it, at `TypeWarning`. The notice names the VIDEO consequence as well as the chat one, and that sentence is the part an operator acts on: THIS download is unaffected, in the sense that nothing about the chat handshake's own refusal changes the video the job is already capturing. What is precise, not "fetched once and never rechecked": `Service.GetHLSMasterPlaylist` decides its own anonymous-or-not verdict once per call, and `processTwitchLive` calls it once at capture start; every downloader (re)start — a quality or init change, a gap, or a post-outage resume (`FetchVariantsFn`, `internal/worker/worker.go`) — calls it again under whatever credentials the jar holds at that moment, but discards the verdict rather than re-marking on every re-probe — so a credential that dies mid-capture is not itself marked until the NEXT capture's own call, whatever the re-probe traffic in between. The NEXT capture starts anonymous — an anonymous token is served stitched ads, which the downloader correctly skips (the real feed is not served during the break), leaving a timestamp jump in the archive, and is refused outright on subscriber-only content. Dedup ACROSS jobs is deliberately absent: a second job an hour later with the same dead cookies must notify again, because by then the operator may believe they fixed it.

**A job with chat recording off is covered by the playback token.** `OnAuthDowngrade` is wired only when `liveDownloadChat` is true (`internal/worker/stream_processor_twitch.go`), so the CHAT detector sees only jobs that capture chat. The other detector is `Service.GetHLSMasterPlaylist`, which decodes the playback access token's `Value` (a JSON document — `PlaybackTokenSession`, `internal/twitch/playback_token.go`) and returns whether credentials were sent and Twitch answered with a token issued to nobody; `StreamProcessor.noteAnonymousPlayback` (`internal/worker/stream_processor_twitch.go`) marks the platform with `playback-token-anonymous` on that verdict, through the SAME `twitchAuthLossReporter` seam the chat downgrade's callback resolves — one mark, one vocabulary, two detectors. It refuses to guess in both directions: a cookieless install is never reported (it is anonymous by design), and an UNREADABLE document — a renamed key, a non-JSON body — is inconclusive rather than anonymous, because folding a Twitch field rename into "the credential is dead" would alarm every install on the day of the change. The verdict is anonymous-vs-signed-in, never entitlement — a signed-in token that cannot fetch subscriber-only content still reads signed-in (a live measurement's authenticated reply carried a numeric `user_id` beside `subscriber=false`). And a token Twitch rejects outright fails earlier, inside the GQL call, as `ErrTwitchAuthExpired` (`internal/twitch/api.go`); which of the two a real token expiry produces has not been measured.

#### Message Processing

The IRC parser handles two message types:

**PRIVMSG** (regular chat and bits):
- Tag fields extracted: `id`, `tmi-sent-ts` (epoch ms), `bits`, `display-name`, `login`, `user-id`, `badges`, `color`, `emotes`.
- If `bits > 0`, message type is `"bits"`, otherwise `"chat"`.
- Emote tags parsed from format `id:start-end,start-end/id:start-end` into `TwitchEmoteRef` structs. **Two index spaces, and the conversion between them is the point.** The WIRE offsets count Unicode CODE POINTS of the message text — inclusive, zero-based, and neither bytes nor UTF-16 units. That was re-measured on 2026-09-15 over the raw IRC lines of 18 real archives (120 of 120 ranges preceded by a non-BMP character slice to a whole-word token by code point, 0 of 120 by UTF-16) after this file and `parseEmoteTags` (`internal/twitch/chat_irc.go`) had asserted UTF-16 since 2026-04-22; every reference client indexes by code point (`references/chatterino7/src/providers/twitch/TwitchIrc.cpp` `codepointToUtf16Idx`, gempir/go-twitch-irc, robotty/twitch-irc-rs). The EMITTED `TwitchEmoteRef.Start`/`End` are UTF-16 code units, because the only consumer is JavaScript: the player slices the span with `String.prototype.substring`, and the VOD path emits UTF-16 already (`utf16Len`, `internal/twitch/api.go`). A range that is inverted or runs past the end of the message keeps its raw wire offsets and gets an empty `Name` — one unrendered emote, never a panic inside the read loop.
- A `/me` message arrives as the CTCP form `\x01ACTION <text>\x01` and its offsets index the UNWRAPPED text (measured the same way). `stripActionWrapper` (`internal/twitch/chat_irc.go`) unwraps it BEFORE the emote tags are read and the fact is reported as `IsAction` (`internal/twitch/types.go`, JSON `isAction`); `Raw` still carries the verbatim wire line. PRIVMSG only — a USERNOTICE body is never wrapped, and a strip there would eat the head of any system message that began with the marker.
- `OffsetMs` computed as `tmiSentTs - baseMs` (signed; negative before the recording base) where baseMs is the part's recording base (or stream start time as fallback). For a part whose video starts a fresh file, that base is the `#EXT-X-PROGRAM-DATE-TIME` of the first segment the part's video writes (owner decision D-T8; see Per-Part Chat Rolling) — except that a part RESUMED after a daemon restart takes baseMs from the base its own chat file already carries in `recordingStartTime` rather than from the restart the orchestrator passes in (`adoptPartRecordingBase`, `internal/twitch/chat.go`): one file, one epoch, because the resumed part's video is appended to and so its timeline still starts where it did, and rebasing would drop every post-restart message onto the head of the part; a part file with no such header offers nothing to adopt and the run's own base stands.

**USERNOTICE** (subs, raids, memberships):
- Tag fields extracted: same as PRIVMSG plus `msg-id`, `system-msg`, `msg-param-sub-plan`, `msg-param-recipient-display-name`, `msg-param-viewerCount`.
- Message type normalization: `sub` -> `"sub"`, `resub` -> `"resub"`, `subgift`/`submysterygift` -> `"subgift"`, `raid` -> `"raid"`, everything else -> `"system"`.

IRCv3 tag values are decoded through `unescapeIRCTag` (`internal/twitch/chat_irc.go`) — `\:` to `;`, `\s` to a space, `\\`, `\r` and `\n`, with an unknown escape yielding its character and a lone trailing backslash dropped. It is applied to `system-msg`, `display-name` and `msg-param-recipient-display-name`, the same three chatterino decodes (`references/chatterino7` IrcHelpers.hpp). The reachable case is a system message that quotes a semicolon: `;` separates tags on the wire, so Twitch must escape it.

**PING handling**: a server `PING` is answered with `PONG :tmi.twitch.tv`.

**The session also speaks first.** `ircReadDeadline` (6 minutes) is the OUTER bound only. A half-open socket — one the OS still believes is connected — used to cost up to six minutes of chat, and Twitch IRC has no replay, so those messages are absent from the archive rather than late to it. A keepalive goroutine inside `runIRCSession` (`internal/twitch/chat_irc.go`) therefore sends `ircKeepalivePing` (`internal/twitch/chat.go` — the line `PING :moombox`) after `ircKeepaliveIdle` (45 s) without ANY inbound frame, and declares the socket dead if no inbound frame of any kind arrives within `ircKeepalivePongWait` (10 s); both windows are evaluated every `ircKeepaliveCheck` (15 s), so the detection bound is about 70 seconds. Any frame answers — a PONG, a chat line, a server PING — because the question is whether the IRC layer is still serving us, not which verb it used. It is a goroutine rather than a shorter read deadline because `coder/websocket` CLOSES the connection when a read context fires, so a 15-second read deadline would kill the socket on every quiet fifteen seconds. The verdict is a SENTINEL, not merely an error. The keepalive closes `keepaliveFailed`, the read loop wakes on it and returns `errKeepaliveTimeout` (`internal/twitch/chat.go`), and `Start` (`internal/twitch/chat.go`) reconnects on that value WITHOUT charging `reconnectAttempts` — which is the whole reason the sentinel exists. A keepalive verdict lands at about 70 seconds, BELOW the `reconnectResetUptime` (5 minutes) that clears the counter, so charging it would walk a PONG-swallowing middlebox through all ten reconnects in about fifteen minutes and drop chat to the slow cadence a spent budget falls back to — on a network the old six-minute detector retried against forever. (A spent budget used to end chat for the rest of the job: `Start` returned "exceeded max IRC reconnects" and nothing relaunched it outside a connectivity outage. It now keeps retrying every `ircExhaustedRetry` (2 minutes) until the job stops it. That backoff is the one wait with no session for `sessionCancel` to interrupt, so `Stop` and `MarkStreamEnded` also send on the downloader's `wake` channel, which the backoff selects on — a finalize or a cancel never waits out the cadence — and `ExecuteTwitch`'s outage resume calls `RetryNow`, which does the same, so chat reconnects as soon as the network is back rather than up to two minutes later.) A faster detector must not turn a recoverable network into a surrendered one. It is not the reauth path's `immediate` either: at budget 0 the `continue` re-dials at once and the ~70 s the verdict itself took is the only wait, while a budget carried from EARLIER real failures is applied unchanged by the loop head's backoff. The session flushes first, so the tail of the lost session's chat reaches disk instead of waiting for the next session's flusher tick.

**And the ORDER inside the verdict is load-bearing.** The verdict is PUBLISHED — `keepaliveFailed` closed — before anything closes the socket: everything that ends a session also closes it, and the read loop wakes from a closed socket instantly, burning all of `chatMaxConsecutiveErrs` on `net.ErrClosed` reads in the time one log line takes to format. It would then return "too many IRC errors", which is not `errors.Is`-able to the sentinel and IS charged — inverting the one guarantee the mechanism exists to make. So the channel closes first, `sessionCancel` second, the Warn last. For the same reason the PING write is bounded by a timer this code owns rather than by a deadline on the context handed to the library: `coder/websocket` installs a write deadline as a `context.AfterFunc` that CLOSES the connection on expiry, from another goroutine, so the timer declares the verdict FIRST and cancels SECOND. The durations live in `chatDelays` (`internal/twitch/delays.go`) so the tests drive the whole cycle in milliseconds. Upstream shape: `references/chatterino7/src/providers/twitch/IrcConnection2.cpp`.

**A server-requested reconnect is a sentinel for the same reason.** Twitch sends a bare `RECONNECT` line when it takes a chat edge out of service. `runIRCSession` (`internal/twitch/chat_irc.go`) returns `errServerReconnect` (`internal/twitch/chat.go`) on it, and `Start` reconnects on that value WITHOUT charging `reconnectAttempts` and without the reauth path's `immediate` — the keepalive's accounting exactly, not a second mechanism. It previously returned `nil`, which is `Start`'s CLEAN-EXIT value: the loop returned, the chat goroutine in `ExecuteTwitch` (`internal/worker/orchestrator_twitch.go`) closed its done channel, and nothing relaunched chat for the rest of the job, because that orchestrator relaunches chat only when a connectivity outage is declared over. A routine maintenance message therefore ended chat capture on a live stream with no error anywhere to say so. Charging the directive would be worse than charging a keepalive verdict: a server rotating its edges can issue several in one marathon stream, none of them after the five minutes of uptime that clears the counter, so ten would exhaust `maxReconnects`. The socket is force-closed with `CloseNow` before the sentinel is returned, because `coder/websocket`'s ordinary `Close` — the deferred one at the top of the session — waits up to five seconds (a library constant) for a peer close-frame ack the departing edge need not send, which would be a real chat gap on every directive. The session flushes first, as every other re-dialling path does, and the directive is logged once — at the loop, where the budget decision is made. The directive is also not a verdict on OUR credentials: `runIRCSession` remembers that it arrived, and the anonymous-fallback check the same function runs on its way out (`noteHandshakeOutcome`, `internal/twitch/chat.go`) is skipped when it did. A directive that beats RPL_WELCOME otherwise looks exactly like a refused login — heard from Twitch, never welcomed — and, now that chat SURVIVES the directive, would run the rest of the job anonymously (the latch is cleared only by `Reauthenticate`) while reporting a downgrade Twitch never rendered.

The read loop reports progress with the chat mutex RELEASED: `addMessage` (`internal/twitch/chat.go`) snapshots the running total inside `cd.mu` and calls the progress callback outside it. That callback reaches the job row through `ProgressTracker` under the database's FULL sync, up to ~60 times a second on a busy channel, and holding the mutex across it queued the flush ticker, `RollFile` and every `MessageCount()` behind an fsync. The YouTube twin (`internal/chat/downloader.go`) has always released first.

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
- **State preservation**: `flush()` is called before each reconnect. Resume state is written to `{outputPath}.resume.json`, at most once per `ircResumeSaveFloor` (`internal/twitch/chat.go`, 5 s) — `saveResumeStateThrottled` (same file) is what the periodic flush calls, so a busy channel no longer pays a ~39 KB marshal, fsync and rename once a second beside the chat.json append fsync. The floor is cleared at every part boundary (`RollFile`, `internal/twitch/chat_recording.go`) so a new part's first flush always writes its own sidecar, and the DEFERRED save on stop is never throttled — that one is what a restart reads. A crash inside the floor window leaves the sidecar short by the messages flushed since its last save, and `restoreResumeState` makes that up from the part file's own header `messageCount` (`utils.ReadChatFileMessageCount`, `internal/utils/chatfile.go` — 1 KB, never the whole file), which every append refreshes and fsyncs: `fileCount` and `totalCount` both gain the difference, and a header BEHIND the sidecar never lowers either. Without it the deficit stayed in that part's header count until the next part roll and in the JOB's chat total for the life of the job — `totalCount` is cumulative, survives `RollFile` by design, and is what `MessageCount()` reports as `total_chat_messages` — because a restored sidecar sets `flushedToDisk`, which makes `Start` skip `adoptExistingPartFile`, the only path that re-counts the file. No message is lost either way; the chat file itself is written every flush regardless.

#### Resume State

The sidecar `.resume.json` file contains:

```json
{
  "messageCount": 1234,
  "lastTimestampMs": 1709000000000,
  "timestamp": 1709000000000,
  "streamId": "12345678",
  "recentIds": ["msg-id-1", "msg-id-2", ...]
}
```

`timestamp` is epoch MILLISECONDS on both chat paths (`ChatResumeState`, `internal/twitch/types.go`). Nothing loads it — it is there so a human reading a sidecar can see when it was written — and until sweep 2 the IRC writer used milliseconds while the VOD writer used seconds, so two files in the same staging tree disagreed about the unit by a factor of a thousand.

On restart, if the `streamId` matches, the downloader resumes with the saved message count, last timestamp, and dedup set. The resume file is deleted on clean completion. A part whose chat file is on disk but whose sidecar is GONE or REFUSED — for example a re-go-live, which changes the stream ID so `loadResumeState` refuses the old sidecar, when the job resumes into the same part; or a crash in the window between the file write and the sidecar write, a sidecar cleared by a stream-end drain, or one deleted by hand — is adopted rather than overwritten (`adoptExistingPartFile`, `internal/twitch/chat.go`): the file is streamed to count its messages array, `flushedToDisk` is set so the first write appends instead of rewriting the part from the new batch alone, and the tail of its IDs seeds the dedup; a file whose bytes read fine but are not chat JSON is preserved beside itself as `<file>.corrupt` and the part starts fresh, never silently overwritten, while a file that could not be READ at all (a lock, a directory in its place) is left exactly where it is — and remembered (`partUnread`): the first flush adopts it then (`adoptUnreadPart`), and holds the batch while it still cannot be read, rather than taking the first-write path that used to replace its history with the new batch. That adoption takes the part's recording base first, as `Start` does, and rebases the pending messages onto it: the base read had failed with the file, so they were offset against the restart, and appending them as they were put two clocks in one file. The adoption runs only when no sidecar restored the part, so an ordinary resume neither pays the full read nor can reach the rename; a restored part is instead checked to still end the way an append needs (`repairDamagedPart`, `utils.ChatFileEndIntact`) and, if a crash left it torn or cut, salvaged — under `flushMu`, like every other write to a part, so a gap split's `RollFile` that lands during the salvage waits for it and closes the repaired part, where it used to land inside it and leave the closed part's count change on the new part (a header of -1 over one message). A roll that lands BEFORE the repair, with nothing pending to drain, salvages the closed part itself (`salvageDamagedPartLocked`), since no drain's `writeBatch` will read it — it used to hand the torn part to the mux and enrichment.

**Write failures** (`writeBatch`, `internal/twitch/chat_recording.go`): a failed append whose end was put back (`utils.ErrChatFilePartialWrite`, a full disk) keeps the batch pending for the next flush — it used to be dropped while `fileCount`/`totalCount` kept it. Any other append failure rewrites the part whole (`rewriteChatFileWithHistory`, shared with the VOD writer): the history is read by `utils.SalvageChatMessages`, so a damaged file (`utils.ErrChatFileDamaged`) keeps every intact message and its original bytes as `<file>.corrupt`, where the full Unmarshal it replaced failed on exactly those files and left the batch retrying every second; a batch message the file already holds is not written twice, and the counters follow what was written. A part whose file is gone takes its messages out of the job total as well. If the stream ends with the final flush failing, the pending messages are spilled to `<file>.lostbatch.json` (appended to a spill already there — a part resumed after a restart, or rolled with a spill, can spill twice, and the second used to overwrite the first; a file at that name that is not a spill is kept aside under a timestamped name), the sidecar is kept, and `Start` returns the error so `chat_status` reads "incomplete" — they used to be dropped with the sidecar cleared and nil returned. Every sidecar save counts what the FILE holds — `fileCount` and `totalCount` less the messages still pending (`saveResumeState`) — because the exit paths save right after a flush that may have failed: a sidecar counting the unwritten batch was restored by the next run as history the part did not have, and since the header may raise a restored count but never lowers one, the header over-counted the array for good (6 over 4).

### VOD Chat

`VodChatDownloader` in `vod_chat.go` downloads chat from completed VODs using the GQL `VideoCommentsByOffsetOrCursor` persisted query.

The Twitch credential here is a GETTER, not a value: the downloader holds `AuthToken func() string` and calls `currentAuthToken()` on every page, so a VOD download running for hours picks up a re-imported token instead of carrying the one it started with to the end. A nil getter is an anonymous comment fetch, exactly as an empty token behaved. `doGQLOnce` (`api.go`) sets `Authorization: OAuth {token}` only when the token is non-empty; the `Client-ID` header is unconditional.

#### Pagination

The operation is `VideoCommentsByOffsetOrCursor` and the OR is load-bearing: `contentOffsetSeconds` selects the page containing a moment, `cursor` selects the page AFTER a given edge, and they are mutually exclusive (`GetVodComments`, `internal/twitch/api.go`, sends exactly one). The persisted-query hash is the same for both.

- **Entering** a VOD — a fresh start, or a resume from the sidecar's `lastOffsetSeconds` — uses the offset.
- **Every later page** uses the previous page's LAST edge cursor (`Cursor`, `internal/twitch/types.go`). A page holds 59 edges and `contentOffsetSeconds` is an integer second, so a second with 59 or more comments made an offset-based next request ask for the page just read; the loop saw only duplicates, found the offset unmoved, and stopped with the rest of the VOD's chat unarchived. The cursor is not persisted — it is opaque and undocumented as durable across sessions, and a stale one answers with an empty page the loop would read as the end of the VOD.
- `contentOffset` still tracks the last edge's offset, because that is what the resume sidecar and the progress line are written from.
- **Only `hasNextPage == false` completes the archive.** A page that arrives with `hasNextPage == true` can never mark the VOD chat finished, even an empty one — it can only keep paging or STALL. An empty last-edge cursor falls back to the pre-cursor offset paging, past that edge's own `contentOffsetSeconds` (one warning). A stuck (repeated) cursor, a page with zero edges, or an offset fallback that fails to advance past the last request are genuine stalls: `pagingStalled` (`internal/twitch/vod_chat.go`) flushes and PRESERVES the resume sidecar instead of deleting it, so the download errors out without ever marking the VOD chat complete, and the sidecar lets a restart of a still-`Downloading` job continue from the saved offset rather than the archive silently ending short — for as long as the job's STAGING, and the resume sidecar written beside `chat.json` inside it, survives. The job row now carries that verdict too: `chatStatusForOutcome` (`internal/worker/orchestrator_chat.go`) derives `chat_status` from the downloader's OUTCOME rather than its message count, so a stall records `incomplete` — rendered verbatim by both UIs — and `chatFileStatus` keeps the mux path's chat-file copy from writing `finished` over it. A chat downloader that was still RUNNING when the job finalized records the same verdict for the same reason: `resolveChatOutcome` (same file) waits a bounded window for the chat goroutine and, once that wait has expired, never returns nil — every downloader's `Stop()` exit does, and reading it through would write `finished` over a capture that was cut short. What the verdict does NOT do is open a recovery gate: `/resume` is restricted to YouTube jobs that are `Finished` with `IncompleteTail` and still have staging (`internal/web/routes/jobs.go`), and a job that finalizes removes its staging (`processJob` in `internal/worker/worker.go`). Past that point the resume sidecar is gone and the stall survives as `pagingStalled`'s Warn, a short chat count, and the `incomplete` row that tells an operator the archive is short.
- **A resumed run re-reads the chat file.** The sidecar is saved right after each flush, so a process killed between the two left `chat.json` holding a page the sidecar's offset and IDs did not cover, and the resumed run appended that page a second time. `adoptExistingFile` (`internal/twitch/vod_chat.go`) therefore seeds the dedup with the IDs of the file's last `chatDedupMax` messages and lifts the total to its count before the first page — the same reconciliation the YouTube and Twitch IRC downloaders make on resume. It also reports how far into the VOD the file's newest message reaches, and the run continues from there when that is past the sidecar's offset or there is no sidecar — and, once the file had to be salvaged, wherever the sidecar says (next point): a finished VOD chat removes its sidecar, and a restart that re-processed the job (its video still downloading) re-paged the whole VOD from offset 0 and appended everything beyond the dedup's last 5000 IDs a second time.
- **The sidecar follows the file.** `checkpoint` saves it only after a flush that wrote everything; saved after a failed one, it recorded an offset and IDs past messages only memory held, and the next run never asked for those pages again. A failed append whose end was put back (`utils.ErrChatFilePartialWrite`) keeps its batch; any other append failure rewrites the file through the salvage reader shared with the IRC writer (`rewriteChatFileWithHistory`), and `repairDamagedFile` salvages a crash-torn file at `Start`, keeping the original as `<file>.corrupt`. When it does, or when a sidecar that counted messages finds its file gone, the sidecar's offset, IDs and count describe history the disk no longer holds, so the run continues from the FILE: the dedup is cleared and reseeded from the file's IDs, the count is the file's, the offset is its newest message's even when that is below the sidecar's (0 with no file), and the sidecar is saved over at once. Kept, a salvage that lost comments below the sidecar's offset resumed past them with their IDs still in the dedup, never asked for them again, and returned nil — a file cut inside its fourth comment ended with six of twelve while the job read "finished". A final flush that fails is returned as the run's error (`chat_status` "incomplete") with the sidecar kept, instead of being ignored on the way to "download complete".
- **A failed page fetch spends the error budget only while the device is online.** `vodChatMaxConsecutiveErrors` (5) with its linear backoff gives up in about a minute, which a connectivity outage outlasts while the video beside it simply waits — the Twitch VOD download is not cancelled by an outage (`ExecuteTwitch`, `internal/worker/orchestrator_twitch.go`). `ExecuteTwitch` therefore installs the connectivity probe (`SetIsOnline`), and a fetch that fails while it reports offline waits for the network (`waitOnline`, polling every `vodChatOnlinePoll`) and then asks for the same page again with the budget untouched.

#### Error Handling

- **Max consecutive errors**: 5 (much lower than live chat's 20).
- **Backoff**: `2 * consecutiveErrors` seconds (linear, not exponential).
- Errors are logged and resume state is saved before returning (when the flush before it wrote everything — see `checkpoint`).

#### Flush Interval

Periodic flush every 5 seconds (`vodChatFlushInterval`). Same incremental append strategy as IRC chat.

#### Resume State

Similar to IRC, but tracks `lastOffsetSeconds` instead of `lastTimestampMs`:

```json
{
  "messageCount": 5678,
  "lastOffsetSeconds": 3600.5,
  "timestamp": 1709000000000,
  "streamId": "v1234567890",
  "recentIds": ["comment-id-1", ...]
}
```

Both chat downloaders cap their sidecar at the newest 1000 dedup IDs — one constant, `chatResumeIDCap` (`internal/twitch/chat.go`). The IRC path used to snapshot its whole 5000-entry set on every flush (about once a second on a busy channel: a ~200 KB marshal, fsync and rename), for a window an IRC reconnect replay can only overlap by seconds.

A VOD downloader the orchestrator re-Starts on the same instance after a connectivity outage (`startChat`, `internal/worker/orchestrator_twitch.go`) first drops whatever its previous run left in memory — a batch its last flush could not write, the dedup and the count — and resumes from the sidecar and the file exactly as a fresh downloader does, fetching that batch again from the resume offset. Kept, the batch sat ahead of the re-fetched copy of the same comments and both were written (13 records, 7 distinct); and with no sidecar to replace them, the kept IDs filtered out every comment fetched again in the batch's place.

### Emote Resolution

`EmoteResolver` in `emotes.go` fetches and caches third-party emotes from BTTV, FFZ, and 7TV.

#### Fetch Strategy

All three providers are fetched in parallel using a `sync.WaitGroup`. Each has an 8-second timeout (`emoteTimeout`). Each returns its emotes AND whether it ANSWERED. Two shapes are answers: a 200 listing no emotes, and a **404** — BTTV, FFZ and 7TV all answer 404 for a channel that never registered with them, and a channel registered with none of the three answers 404 on all three (`errEmoteProviderNotFound`, `internal/twitch/emotes.go`). Only a provider that could not be reached, that answered 5xx, or whose body could not be parsed is a failure. Reading the 404 as a failure meant nothing was cached for such a channel, so `Resolve` re-fired three requests and four Warn lines on every part roll and stream end of every job on it. Failures are logged at warn level and are non-fatal; a 404 logs at debug level.

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

- **Type**: LRU (Least Recently Used) with a time-to-live.
- **Max size**: 200 channels.
- **Key**: lowercased `channelLogin` (preferred) or `channelID` (fallback).
- **Eviction**: When full, the oldest entry (by insertion order) is removed.
- **A failure is not a result**: the set is cached only when at least one provider answered. Three providers failing together — one outage, one flaky minute — used to be written to the cache and served for the rest of the process lifetime, and the per-downloader cache one layer up (`resolveEmotesCached`, `internal/twitch/chat_recording.go`) latched the same empty set for the life of the job. A resolve in which nothing answered now returns nil, and both layers retry.
- **An answer goes stale**: an entry older than `emoteCacheTTL` (`internal/twitch/emotes.go` — 24 hours) is refetched on the next resolve. Moombox runs for weeks and 7TV/BTTV/FFZ sets change daily. If that refetch fails outright the STALE set is served and kept — yesterday's emotes beat none.
- **Inflight dedup**: If a request for the same cache key is already in-flight, subsequent callers block on a channel until the first completes, then read from cache.

#### Emote Injection

After chat download completes, `EnrichWithEmotes` reads the chat JSON file, adds the resolved `TwitchEmoteData` to the `emotes` field, and rewrites the file atomically. This uses a fresh `context.Background()` context with a 30-second timeout, ensuring emote resolution completes even after the parent context is cancelled.

#### Per-Part Chat Rolling (live IRC)

Multi-part Twitch live recordings (gap/quality splits) roll the chat file at every part boundary that keeps a part via `ChatDownloader.RollFile(newPath, newRecordingStart)` (a short span the quality split discards rather than muxes is the exception: when the next part reuses the same staging dir nothing rolls, so that part's chat file keeps the dropped span's messages and its offset base and runs up to `minSegmentDuration` early; when it moves from the root to `seg_0` the roll happens but the closed file, holding under ten seconds of chat, is dropped with the span): pending messages drain into the closing file, its resume state is deleted (the part is final), and recording redirects to the new part's staging dir with `OffsetMs` rebased to the new part's capture start — so each `{name} - partN.chat.json` replays in sync against its own video part.

**A part's chat base is its video's first frame** (owner decision D-T8). The orchestrator hands each part the local clock at its start, but the part's video starts at the first segment of the live window the downloader joined — up to a window's length earlier — so every message used to replay late by that much. Now the local clock is only provisional: a part whose video starts a fresh file waits for that file's first segment, and the base it settles on is that segment's program date-time. The engine reports it once per downloader (`DownloaderOptions.OnFirstSegment`, `internal/engine/downloader.go`) when it writes the first media segment into an output file it started empty — never on a resume or an append, and stitched-ad segments are skipped rather than written, so it is the first CONTENT segment — with the date from `HlsSegment.ProgramDateTime` (`internal/engine/manifest.go`: the segment's own tag, or the last one advanced by the durations in between). `createDownloader` (`internal/worker/orchestrator_twitch.go`) routes the report to `ChatDownloader.SettlePartBase` for the chat file of the SAME part directory, so a late report from a replaced downloader cannot reach another part. The first part is marked waiting before the chat starts (`AwaitPartBase`, only when the part has no staged video), and every later one atomically with its roll (`RollFileAwaitingBase`). While a part waits and has no file yet, the periodic flush holds its messages, so the file's header is written ONCE, with the base its offsets were computed against — one file, one epoch; on the report the held messages are rebased onto it under `cd.mu`, as `adoptUnreadPart` rebases a held batch. A playlist with no program date-times reports the zero time, which ends the wait on the local-clock base (the old behaviour); so does a wait longer than `ircPartBaseWait` (60 s, `internal/twitch/chat.go`), the final flush at the chat's exit (`flushFinal`), and a roll that closes a part still waiting. A part whose file is already on disk — a resumed part, adopted at `Start` — keeps its file's base whatever is reported. Pinned by `TestPartBaseSettlesOnTheFirstSegmentsTime`, `TestPartBaseKeepsOneEpochPerFile`, `TestPartBaseWaitIsBounded`, `TestRollFileAwaitingBase`, `TestPartBaseKeepsItsMilliseconds` (`internal/twitch/chat_part_base_test.go`), `TestHlsLiveReportsTheFirstWrittenSegmentsTime` and `TestHlsLiveAppendReportsNoFirstSegment` (`internal/engine/downloader_hls_firstsegment_test.go`) and, end to end through a quality split, `TestTwitchPartChatIsBasedOnItsFirstSegment` (`internal/worker/twitch_chat_part_base_test.go`). The header's `recordingStartTime` is written with `time.RFC3339Nano`, so a program date-time's milliseconds survive into it and a resumed part adopts exactly the base its offsets were computed against; a whole-second base prints as it always did. A closing part `Start` could not read (`partUnread`) is adopted first, exactly as its first flush would adopt it; while it still cannot be read the pending messages are spilled to `<file>.lostbatch.json` and the file is left alone, never written over from the batch — the drain used to take the first-write path the flush is forbidden for such a part, leaving a 40-message part holding its 5-message boundary batch — and its path is still returned as the closed part, since it holds that part's history and the part's mux must copy it or fail and retry. A boundary batch the roll could not write — spilled for an unread part, or after a drain whose write failed — leaves the job total, which follows what the part files hold, and is counted in `rollUnwritten` (carried in the sidecar as `rollUnwritten`, like `totalCount`, so a restart keeps it); the stream's end then reports the capture incomplete (`Start` returns an error naming the count), so the staging cleanup keeps the spill (`keepOnlyChatCapture` keeps every `chat.json.*` at any depth). It used to say nothing: the stream ended "finished" and the cleanup deleted the spill with the part's dir. The mux checks that it can open the part's chat before ffmpeg runs (`muxSegment`, `internal/worker/orchestrator_mux.go`), so a file that is still locked fails the part without throwing a finished mux away — it used to find out at the copy, after the mux, on every attempt — and a directory in the file's place counts as no chat, as the finalize's `muxUnrecordedSegments` already read it. The dedup set and the cumulative message total survive the roll (`ChatResumeState` carries both the per-file `messageCount` and the job-wide `totalCount`; legacy single-file states fall back to `messageCount`). Closed parts are emote-enriched from the background part-mux goroutine using a cached resolve (`resolveEmotesCached` — the emote APIs are hit once per job, not once per part); the final part is enriched by the stream-end drain as before.

---

## BotGuard / PO Token (`internal/bgutils/`)

### Purpose

YouTube's BotGuard is a client verification system that generates Proof of Origin (PO) tokens. These tokens prove that a request originates from a legitimate browser environment. Without them, certain YouTube API responses (premium-quality formats, live streams) may be degraded or blocked.

### Why a Sidecar

BotGuard inspects the JS runtime's wall-clock timing as part of its fingerprint — its snapshot routine runs a sequence of operations and measures how long they take. Real Chrome + V8 takes 50–200 ms. The Goja interpreter, being a non-JIT pure-Go implementation, completes the same operations in ~552 µs — about 100× faster. BotGuard treats this speed disparity as a "this isn't a real browser" signal and refuses to mint a real `integrityToken`, returning only a `websafeFallbackToken`.

The hand-rolled real-class DOM shim work (test.50–test.55) raised the goja runtime's API fidelity to browser parity (`document instanceof HTMLDocument`, real event dispatch with capture/bubble, real `CSSStyleDeclaration`, real `URL`/`AbortController`, etc.) but couldn't bridge the timing gap because the gap is below the JS API surface — it lives in the V8-vs-interpreter speed difference itself.

The fix is to run BotGuard under real V8 + JSDOM. Moombox embeds a Node.js v24 binary plus `bgutils-js` + `jsdom` (both MIT-licensed) and runs them as a subprocess. This is the same combination `bgutil-ytdlp-pot-provider/server/` ships in production.

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
                  │  ├── inflight (Go map, under pp.mu)     │
                  │  └── sidecar *Sidecar                   │
                  └────────────────┬───────────────────────┘
                                   ↓ (when sidecar healthy)
                  ┌────────────────────────────────────────┐
                  │ internal/bgutils/sidecar.Sidecar        │
                  │  ├── go:embed node-<goos>-<goarch>.gz   │
                  │  ├── go:embed sidecar.tar.gz            │
                  │  ├── extract-on-first-launch logic      │
                  │  ├── exec.Cmd + Job Object pinning      │
                  │  ├── stdin/stdout JSON-RPC channel      │
                  │  └── reqID -> chan response mux         │
                  └────────────────┬───────────────────────┘
                                   ↓ stdin/stdout pipes
                  ┌────────────────────────────────────────┐
                  │ Bundled Node (moombox-sidecar[.exe])    │
                  │  └── bgutil-sidecar/src/server.js       │
                  │       └── BgUtils SessionManager        │
                  │             └── BgUtils +              │
                  │                 JSDOM +                │
                  │                 V8 (real)              │
                  └────────────────────────────────────────┘
```

`PotProvider.generateAndMint` (and `GenerateGvsPoToken`) branch on `pp.sidecar != nil` — sidecar mode, which `cmd/moombox` enters by attaching the handle whenever `[bgutils] use_sidecar` is on, before the first start, so a failed first start is an outage like any other. On success the result is cached in the session cache and returned. While the sidecar is down a mint fails at once with `errSidecarDown`, and a sidecar mint that errors is returned as that error: there is no goja fallback in sidecar mode. It used to fall through to the in-process path, which mints no PO token in practice (step 5 below), so every request during an outage paid seconds to minutes of doomed BotGuard work and three Google round trips, serialised behind the minter-creation lock.

### Sidecar lifecycle (`internal/bgutils/sidecar/`)

**Embed:** `internal/bgutils/embed/` is a standalone package exposing three `go:embed`'d package vars:
- `EmbeddedNode []byte` — gzipped Node.js v24 binary for the build's GOOS/GOARCH (~34-44 MB), produced by `tools/fetch-node` and selected via per-platform `embed_<goos>_<goarch>.go` build tags.
- `SidecarTarGz []byte` — gzipped tarball of `bgutil-sidecar/` production deps + JS source (~4 MB), produced by `bgutil-sidecar/build.mjs`.
- `Version string` — content of `internal/bgutils/embed/version.txt`: the Node pin, `node@vX.Y.Z` plus one `<goos>-<goarch>@<sha>` per platform. One half of the cache-invalidation key; `buildCacheStamp` appends `sidecar@<sha256 of SidecarTarGz>` to it.

**First-launch extraction:** `extractIfNeeded(cacheDir)` resolves `cacheDir = os.UserCacheDir() + "/Moombox/sidecar"` (Windows: `%LOCALAPPDATA%/Moombox/sidecar`), tightens the dir's ACL via `utils.ApplyUserOnlyDACL` (always — even on cache-hit, so users upgrading from v2.5.x get the security benefit), then compares on-disk `version.txt` against the stamp from `buildCacheStamp` (the embedded `Version` plus the tarball's SHA-256). On match + key files present, the function returns immediately. On mismatch, it deletes the old `version.txt` FIRST (so an interrupted re-extract is never left under a stamp the previous binary still matches), gunzip-extracts the Node binary as `moombox-sidecar[.exe]` (orphans under older names, such as a bare `node.exe`, are removed), gunzip+tar-extracts the sidecar payload using stdlib `archive/tar` + `compress/gzip` (end users do NOT need a system `tar` binary — that's a build-time-only requirement for `bgutil-sidecar/build.mjs`), and writes the new `version.txt` LAST so a partial extraction next time forces a redo. `extractTarGz` replaces rather than merges: the first time an entry lands under a top-level name (`src`, `node_modules`, `vendor`, the two manifests), whatever the dir held under that name is removed, so files a newer payload dropped — whole packages, nested `node_modules` copies that would shadow the hoisted one — do not survive an upgrade. Only names the tarball writes are cleared. Tar-slip defense rejects entries whose target escapes `cacheDir`. File modes are clamped to `0o644` minimum to defend against tar variants that emit zero-mode headers.

**Subprocess:** `exec.Command(cacheDir+"/moombox-sidecar[.exe]", <V8 flags>..., cacheDir+"/src/server.js")` with `cmd.Dir = cacheDir` — the V8 flags (`--max-old-space-size` from `memory.sidecar_hard_limit_mb`, `--expose-gc`) must precede the script. Stdin/stdout are piped for JSON-RPC; stderr is piped to `stderrPump`, which logs `[bgutil-sidecar:error]` lines at Warn and everything else at Debug. The process is pinned to a Windows Job Object configured with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` so the child + any grandchildren die when Moombox exits — even on a hard parent crash. (Same pattern as `internal/cookies/job_windows.go`.) On Linux, `PR_SET_PDEATHSIG` (SIGKILL) covers the crash path instead, and the sidecar runs in its own process group (`Setpgid`, `job_linux.go`): a terminal's Ctrl+C or a supervisor's group signal otherwise reached it at the same instant as Moombox, killing it out from under the graceful `Stop()` in shutdown and logging a stdout-EOF warning on every ordinary quit.

**Handshake:** After `cmd.Start()`, the manager spawns `readPump` (consumes stdout JSON-RPC responses, routes to per-request channels via `reqID`) and `stderrPump`, then waits for server.js to emit `{"event":"ready"}` once its synchronous init (module parse, JSDOM construction) is done; readPump closes `readyCh` on it. `StartupTimeout` (default 60 s) is only a backstop for a hung child — the earlier ping/pong with a 5 s deadline raced jsdom's cold start. A failed or timed-out handshake tears the child down and Start returns the error, ending in the child's last few informative stderr lines (`stderrTail` — stack frames, brackets and the `Node.js vX` footer left out): Node's own fatal errors are unprefixed and logged at Debug, and "sidecar exited before ready" alone was all the Warn, the health reason and the alert ever said. The FIRST child of a process that exits before ready also drops `version.txt`, so the next start re-extracts — a tree broken under a valid stamp (a payload cut short by a crash) was otherwise reused by every supervisor retry; a child that still fails after that is the environment, not the files, and is not rewritten again.

**Per-request flow:** `Sidecar.GeneratePoToken(ctx, binding)` allocates a `reqID`, registers a buffered channel in `s.pending`, writes `{"id":N,"method":"generatePoToken","params":{"binding":"..."}}` to stdin while holding the one-slot write semaphore (`writeSem`, waited for under `ctx`), then waits on either the channel or `ctx.Done()`. The write itself runs on its own goroutine, so a child that has stopped draining stdin cannot hold the caller past its context; a write stalled longer than `RequestTimeout` marks the sidecar unhealthy. The readPump matches the response by `id` and forwards to the channel. Concurrent calls multiplex cleanly because each request has its own channel.

**Crash recovery:** If `readPump` observes stdout EOF (parent's view of child death), it calls `markUnhealthy("stdout EOF")` which atomically flips `s.healthy` to false and drains every pending request channel with an error. `markUnhealthy` then hands the reason to `Config.OnUnhealthy`, which is how the supervisor learns the child is gone; `IsHealthy()` stays false until a restart succeeds. See **Supervision** below.

**Graceful shutdown:** `Sidecar.Stop()` is bounded at ~3 s total — 1 s for the JSON-RPC `shutdown` round-trip + 2 s for the process to exit on its own + Kill on timeout. The bound stays well under `cmd/moombox/shutdown.go`'s 15-s force-exit budget so a hung sidecar can't starve web-server / DB-close / pump-drain shutdown steps.

### IPC protocol

JSON-RPC-style, line-delimited (one JSON object per line, separated by `\n`). All fields ASCII-safe.

| Method | Params | Result | Notes |
|---|---|---|---|
| `ping` | (none) | `"pong"` | liveness probe (tests); the startup handshake is the `ready` event, not a ping |
| `generatePoToken` | `{binding, challenge?, freshMinter?}` | `{poToken, binding, expiresAt, minterSource, minterFresh}` | hot path; both GVS and player mints send `challenge`, only GVS sets `freshMinter` (see "GVS (Segment-URL) PO Tokens" below). The interpreter URL inside `challenge` is origin-gated before any fetch |
| `invalidateCaches` | (none) | `"ok"` | wipes sidecar's session + minter caches |
| `invalidateIT` | (none) | `"ok"` | wipes only minter cache (force fresh BotGuard) |
| `getStats` | (none) | `{cachedMinters, cachedSessions, mintsTotal, mintsErrored}` | observability |
| `solveCipher` | `{playerID, playerJS?, sigChallenges, nChallenges, forceReload?}` | `{sigResults, nResults}` (challenge → answer maps) | ejs signature/n solving. `playerJS` is attached on a player's first solve per sidecar lifetime — one solve ships it while concurrent first solves wait (`claimFirstSend`, `internal/cipher/solver_sidecar.go`) — and on the retry paths; an unknown player answers `"player not loaded"` (`ErrPlayerNotLoaded`), which the Go side retries once with the JS attached |
| `getMemoryStats` | (none) | `process.memoryUsage()` (`{rss, heapTotal, heapUsed, external, arrayBuffers}`) | read for the soft memory limit (`memory.sidecar_soft_limit_mb`) |
| `triggerGC` | (none) | `{before, after}` (two `memoryUsage()` snapshots) | a full V8 GC (needs `--expose-gc`), sent when RSS passes the soft limit |
| `shutdown` | (none) | `"bye"` | graceful exit; sidecar JS calls `process.exit(0)` on next tick |

Errors are returned as `{"id":N,"error":"<message>"}`. Parse failures on stdout are logged at Warn and the line is dropped (defensive against partial writes during a parent crash).

### Sidecar JS (`bgutil-sidecar/`)

Lives at the repo root (peer to `cmd/`, `internal/`, `web/`). Production node_modules and `src/server.js` are tarred by `build.mjs` into the embed blob; the tarball is what gets shipped.

`src/server.js` (about 900 lines; ejs cipher solving lives in `src/cipher.js`, homepage challenge extraction in `src/homepage.js`):
- Bootstrap JSDOM on `globalThis` (matching `bgutil-ytdlp-pot-provider/server/src/session_manager.ts`'s setup).
- Read JSON-RPC requests line-by-line from stdin via `readline`.
- Track inflight async dispatches so the process drains pending responses before exiting on stdin close (otherwise smoke tests would hang waiting for a response that never arrives because the parent already EOF'd stdin).
- Implement each method against `bgutils-js` directly (the actual BotGuard implementation by LuanRT; MIT-licensed). We deliberately do NOT depend on `bgutil-ytdlp-pot-provider` itself because that wrapper package is GPL-3.0-only and embedding it would force Moombox to GPL-3.0; we re-implement the ~100-line wrapper inline.
- Single-minter cache (mirrors PotProvider's CRIT-2 pattern) — one minter serves every binding until its TTL expires.

### Goja fallback path

When `[bgutils] use_sidecar = false` in config (no sidecar attached), `PotProvider.generateAndMint` runs the in-process flow. Minter creation is serialised by a one-slot lock (`lockMinterCreation`) that a waiter gives up on when its own context ends, rather than waiting out the holder's whole BotGuard run:

1. **Fetch Challenge** (`challenge.go`):
   - POST to `https://jnn-pa.googleapis.com/$rpc/google.internal.waa.v1.Waa/Create` — the only endpoint; the `www.youtube.com/api/jnn/v1/Create` alternative and its `UseYouTubeAPI` switch were removed (`internal/bgutils/types.go`).
   - Request body: `["{requestKey}"]` where requestKey defaults to `O43z0dpjhgX20SCx4KAo`.
   - Headers: `Content-Type: application/json+protobuf`, `x-user-agent: grpc-web-javascript/0.1`, `x-goog-api-key: AIzaSyDyT5W0Jh49F30Pqqtyfdf7pDLFKLJoAnw` (Google endpoint only).
   - Result: `DescrambledChallenge` containing `Program`, `GlobalName`, `InterpreterScript`/`InterpreterURL`, `InterpreterHash`.

2. **Create BotGuard VM** (`botguard.go`):
   - Creates a new Goja runtime with the real-class DOM shim from `internal/goja/js/dom-real.js`, TextEncoder/TextDecoder, timers.
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
   - **No integrity token**: If `integrityToken` is null — the typical outcome on the goja path — the VM is shut down and the mint fails with an `ErrIntegrity` `BGError`, even when a `websafeFallbackToken` came back. That token used to be cached as a static PO token for every binding ("Path B"); it was removed because YouTube does not accept it for authenticated player requests and it masked BotGuard VM failures (upstream bgutil-ytdlp-pot-provider errors the same way). The goja path therefore mints no PO token in practice.
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
- **Sidecar interaction**: Effectively unused under sidecar mode. The sidecar maintains its own internal minter cache inside the Node process; `PotProvider`'s minter cache is populated only on the goja-only path (`use_sidecar = false`).
- **VM lifetime**: The `Cleanup` function is critical. The minter's `MintFunc` is a closure over the Goja runtime state. Calling `Cleanup()` shuts down the VM, invalidating the closure. This is why minter eviction is the only correct place to call it. `cleanupExpired` returns the slice of evicted minters and the caller (`GeneratePoToken`) runs `safeCleanup` outside `pp.mu` — holding the lock across `m.Cleanup()` could deadlock against a concurrent `mintPoToken` (CRIT-6 audit fix; same anti-pattern that was previously fixed in `InvalidateCaches` / `InvalidateIntegrityTokens`).

#### Inflight Dedup
- **Type**: `map[string]*inflightEntry`
- **Key**: `contentBinding`, with `bypassInflightSuffix` appended for a `bypassCache` mint. A bypass caller (the 403 credential refresh) joins only another bypass mint — an ordinary one in flight is minting with the cached minter, whose token is exactly what the bypass exists to replace, and joining it made the refresh a no-op. An ordinary caller joins a bypass mint in flight first (its token is fresher), else an ordinary one.
- **Mechanism**: When a generation starts, an `inflightEntry` with a `done chan struct{}` is placed in the map. Concurrent requests for the same key block on `<-entry.done`. When generation completes, the result is stored on the entry, then `close(entry.done)` unblocks all waiters. Context cancellation is respected via `select`. A waiter whose leader ended on its OWN context (`leaderGone`: the leader's context had ended when its mint returned a context error) asks again while the waiter's context is live, rather than inheriting a cancellation it never asked for. Any other failure is the waiters' answer too — a timeout inside the mint included (a sidecar `RequestTimeout`, an HTTP client timeout), which is a `DeadlineExceeded` as well: keyed on the error's type alone, every waiter re-ran a mint that had just timed out, one after another, the last held for all of them. Prevents thundering herd when many goroutines request the same binding simultaneously.
- **Cache generation** (`cacheGen`, under `pp.mu`): bumped by `InvalidateCaches`, `InvalidateIntegrityTokens` and the start of every bypass mint, and recorded on each entry. A mint that began before one of those still answers its own waiters but does not write its session to the cache, where it would have outlived the refresh that replaced it.

#### Cleanup fan-out

`InvalidateCaches()` and `InvalidateIntegrityTokens()` both call into the sidecar (when attached) via `Sidecar.InvalidateCaches(ctx)` / `Sidecar.InvalidateIT(ctx)` (5s ctx) so the sidecar's internal caches drop in lockstep with Moombox's. This is what backs the operator-facing `/invalidate_caches` and `/invalidate_it` HTTP routes used by yt-dlp's bgutil-pot-provider plugin.

#### Configuration

```toml
[bgutils]
use_sidecar = true   # default: true. Set to false to force goja-only.
```

Disabling the sidecar leaves only the goja path, which mints no PO token (see step 5 above) and has no signature solver for current players, so PO-token-gated and signature-ciphered formats become unavailable; formats that need neither keep working.

#### Supervision

The sidecar is supervised (`internal/bgutils/sidecar/supervisor.go`). A child that crashes or is OOM-aborted by V8 — the shipped `memory.sidecar_hard_limit_mb` of 512 sits just above the documented 400-500 MB BotGuard burst — fires `Config.OnUnhealthy` from `readPump`, and `Supervisor` restarts it IN PLACE (`Sidecar.Restart`, which preserves the handle `cmd/moombox` holds for shutdown and `PotProvider` holds for minting) on a 5 s / 15 s / 60 s / 5 min ladder whose last step repeats. The rung carries ACROSS outages while the child is flapping and resets only once a child has stayed up longer than the ladder's ceiling (5 min), so a one-off crash on a healthy install is not made to wait out the previous outage's ceiling. Measured against the rung the child came back on — 5 s on the first, shorter than a cold mint — a child that died on its first mint a few seconds in always read as healthy, and Node was respawned every ~11 s for the rest of the run, each outage below the `sidecar_down` alert's debounce. After each success the two consumers are re-wired by the `SetOnUp` callback: `PotProvider.SetSidecar` and a freshly built `cipher.NewSidecarSolver` installed through `cipher.SidecarSwappable.SetSidecar` — rebuilt rather than reused, because the old solver's per-player "already sent" map describes the dead child's memory.

Without it the failure was permanent for the life of a 24/7 process: `markUnhealthy` latched, and because sig is sidecar-only (no goja fallback — see the Cipher Solver's routing policy) every signature-ciphered format became unresolvable while PO tokens fell to the goja path, which rejects the websafe-only response. The outage is now visible while it lasts — `sidecar.PublishHealth` writes a package-level snapshot that `/api/status` reads through `sidecar.CurrentHealth` and publishes as its `botguardSidecar` object (`healthy` / `reason` / `restarts`, ABSENT rather than false when nothing was ever published, which is what `[bgutils] use_sidecar = false` looks like) and that, through `SubscribeHealth`, drives the TUI status bar's red `SIDECAR DOWN` chip (abbreviated to `POT` at tight widths).

#### Cleanup Lifecycle

Expired entries are cleaned up in three ways:
1. **Lazy cleanup**: `cleanupExpired()` is called at the start of every `GeneratePoToken` call while the mutex is held. It iterates all session and minter entries and removes expired ones; the expired minters it returns are torn down by `safeCleanup` AFTER `pp.mu` is released (a `Cleanup()` under the lock could deadlock against a concurrent mint).
2. **Proactive auto-eviction**: `time.AfterFunc` on minter creation schedules exact-TTL cleanup.
3. **Manual invalidation**: `InvalidateCaches()` clears everything and shuts down all VMs. `InvalidateIntegrityTokens()` clears only the minter cache (forces BotGuard re-run but preserves session cache).

### GVS (Segment-URL) PO Tokens

Moombox mints two populations of PO token, and they follow different rules because upstream treats them differently.

**Player-API tokens** (used by `fetchWithClient` / `fetchWithEmbedded`) bind to the **video ID** — yt-dlp binds `PoTokenContext.PLAYER` to the video ID unconditionally (`pot/utils.py`) — and are minted via the sidecar's cached minter (`GeneratePoTokenString`) with normal session caching — that minter is now built from the homepage (ytcfg, ytAtN) pair, with `/att/get` only as its fallback. Caching is deliberate: player calls fire on every probe and refresh (several per live job per hour, plus monitor polls), so fresh-minting each one would cost a multi-second BotGuard pass on the hot path. The mint still gates on visitor data being present — not as the binding (it no longer derives from it) but as the "session established" precondition it always was.
PROBE-ONLY calls mint none (`fetchWithClientProbe`, owner decision O-R, 2026-09-17): yt-dlp's WEB `PLAYER_PO_TOKEN_POLICY` is `required=False, recommended=False` (`_base.py:90`), and Moombox was minting one per probed video ID on the monitor's date probes — parking a 6 h session-cache entry, for a video that may never be downloaded, that every later mint then sweeps. The same marking rides the request context down to `parseFormats`, where it moves that response's DRM-skip report from Warn to Debug: `ProbeVideoStatusAuthenticated` is the call the 30 s quality monitor makes on an auth-walled stream (owner decision O-H) and the call a members-only waiting room makes on its own tiered interval — as tight as 30 s near the scheduled start — and an account in the tv-client DRM experiment gets DRM formats in nearly every one of those responses.
A challenge-sourced variant — the watch page's own ytAtN attestation, threaded into a fresh mint — is
NOT wired: it exceeded what yt-dlp does and had no caller, so the Go-side entry point was deleted
(owner ruling R1, 2026-09-15). The sidecar protocol that would carry it is untouched, and
`generatePoTokenChallenge` still takes the challenge through to `generateAndMint`, so restoring the
path is one call. `extractAttestationChallenge` keeps running and keeps logging its reason string,
which is the diagnostic premiere 403s would be read from.

**GVS (media-URL) tokens** — segment and manifest URLs on the live paths, whole-file format URLs on the VOD direct path — are minted under a deliberately cache-hostile policy — moonarchive parity, added 2026-08-14 (attestation POT coherence) after a premiere broadcast 403'd every segment for its full runtime because the minting session had no tie to the watch-page session that resolved the stream.

`PotProvider.GenerateGvsPoToken(ctx, binding, challenge) (GvsMint, error)` implements the policy below, but no production code calls it — it is exercised only by `internal/bgutils`'s own tests. Each download strategy instead mints its once-per-download-start GVS token through the ordinary cached path, `PotProvider.GeneratePoTokenString` (`internal/bgutils/pot_provider.go`) — the same call the player-API path above uses — via the `mintGvsPoToken` seam (`internal/worker/strategies.go`, a package var so strategy tests can fake it; production never writes it): `DownloadVod` (`strategy_youtube_vod.go`), and the DASH, HLS and manifestless strategies (`strategy_youtube_dash.go`, `strategy_youtube_hls.go`, `strategy_youtube_manifestless_dash.go`). Whether a mint happens at all, and which URLs carry the result, is the per-client policy in the next subsection. The one production route to a fresh GVS mint is the mid-download 403 credential refresh in `refreshGvsCredentials` (`internal/worker/strategies.go`), which calls `GeneratePoTokenString` with `bypassCache=true`; `generateAndMint`'s bypass branch then mints through the sidecar's own `GenerateGvsPoToken` (`internal/bgutils/sidecar/sidecar.go`), a distinct method from `PotProvider`'s of the same name:

- **Binding**: resolved by `youtube.GvsContentBinding` (`internal/youtube/pot_binding.go`), a port of yt-dlp's `get_webpo_content_binding`, and carried on `VideoInfo.GvsBinding`/`GvsBindingKind` so every strategy asks once and cannot drift. The rule, in order: the **video ID** when the page's player configs carry `html5_generate_content_po_token=true` (the experiment under which YouTube switches GVS binding to the video ID — active as of 2026-08-15, verified against a live watch page); otherwise the **datasync ID** for an authenticated session; otherwise **visitor data**. A last-resort video-ID/channel-ID fallback covers a session where none of those survived extraction, so a mint is never bound to an empty string. Moombox hardcoded the video ID until 2026-08-15; that was correct only while the experiment stays on, and a session with it off needs datasync binding or earns silent 403s.
- **Challenge**: the sidecar's own **homepage (ytcfg, ytAtN) pair** when it can build one (see below), else a challenge the caller supplies — this route has never had a supplier for that parameter, independent of owner ruling R1 (2026-09-15), which deleted the unrelated player-API-side challenge entry point described above — else the sidecar's `/att/get` fetch. Each step down is logged with a distinct reason and is never worse than the step it replaced.
- **Cache policy**: bypasses the session cache entirely (no read, no write) — every call mints fresh, and the sidecar is told `freshMinter: true` so it regenerates its BotGuard minter for this call rather than reuse an already-cached one. The fresh minter **replaces** the sidecar's cached minter, so subsequent player-API mints passively pick up the more session-coherent one. Concurrent GVS mints with the same challenge share the sidecar's in-flight regeneration (`minterInflight`, keyed by challenge, every generation ordered on `serializeChain`); no provider-side inflight entry is added — per-binding minting off a shared minter is cheap, and adding provider-level dedup here would hand a stale (non-fresh) result to whichever caller lost the race.
- **Without the sidecar**: with no sidecar attached (`[bgutils] use_sidecar = false`) it runs the goja mint flow with the challenge ignored, reported as `minterSource=goja-fallback`. An attached sidecar that is down fails the mint at once (`errSidecarDown`) — there is no goja fallback in sidecar mode.
- **Result**: `GvsMint{PoToken, MinterSource, MinterFresh, ViaSidecar}` — the fields the provenance log line below reports. `MinterSource` is `"homepage"` (the sidecar's own ytcfg+ytAtN pair — the expected value), `"challenge"` (built from the page's own challenge), `"att_get"` (sidecar fetched its own), or `"goja-fallback"`.
- **Counters**: `PotStats.GvsMints` (every attempt) and `GvsMintsChallenge` (the subset that carried a non-empty page challenge).

#### Homepage (ytcfg + ytAtN) pair minting (`bgutil-sidecar/src/homepage.js`)

**Why it exists.** YouTube binds the initial attestation challenge to the webpage session via `yt.config_.EVENT_ID` and **rejects WebPO tokens minted from `/att/get` challenges** when the session is enrolled in the experiment. The symptom is precise and was Moombox's 2026-08-14 premiere failure exactly: player requests pass, googlevideo 403s every segment. Upstream `bgutil-ytdlp-pot-provider` 495a47f (2026-08-21) and LuanRT/BgUtils#44 diagnose and fix it; Moombox's cached `/att/get` minter was the rejected configuration.

**What it does.** `fetchHomepageChallenge` (server.js) fetches `https://www.youtube.com/` once and extracts a **self-consistent pair from that single page**: the `ytcfg` blob and the page's own `window.ytAtN(...)` challenge. It installs `globalThis.yt = { config_: ytcfg }` (and `window.yt`) so the BotGuard snapshot reads the session's real `EVENT_ID`, then mints from that page's challenge. Pairing is the whole point — a challenge from one page and a ytcfg from another is exactly the incoherence YouTube rejects, which is why an RPC-supplied watch-page challenge (which arrives without its originating page's ytcfg) now ranks *below* the homepage pair.

**Extraction hardening.** `homepage.js` deliberately does not use upstream's regexes; it mirrors the rules `watch_page.go` already learned:

- **String-aware balanced-brace scanning**, not non-greedy regexes. Upstream's `/ytcfg\.set\(({.+?})\);/s` truncates on a `});` inside any string value, and its `/window\.ytAtN\(\s*({[\s\S]*?})\s*\)/` truncates on a `})` inside the opaque program — both yielding an unparseable fragment indistinguishable from "no challenge".
- **A tolerant recursive-descent parser** (`parseLooseJSON`) for the outer JS object literal — never `eval` or `new Function`. It accepts unquoted keys, single quotes, trailing commas, and `\xNN`/`\uNNNN` escapes, and **throws** on anything else (identifiers, calls, stray backslashes). That last property is load-bearing: attacker text smuggled through a video title arrives backslash-escaped inside a JSON string, and a backslash outside a string is a parse error rather than a challenge.
- **Canonicalization** to exactly the three fields the minter consumes (`program`, `globalName`, `interpreterUrl`), matching Go's `canonicalizeChallenge`. Extra keys — including an inline `interpreterJavascript` riding alongside a valid URL — are dropped, which also closes the parser-differential class described under the interpreter gate below.
- **Origin gating before any fetch**: the extracted `interpreterUrl` runs through `assertGoogleHost` inside `fetchHomepageChallenge`, so a disallowed host degrades to the next challenge source instead of failing the mint.

Two shapes the live homepage actually emits broke the first implementation and are now pinned by regression tests: the `R` payload is `\xNN` hex-escaped (`\x7b` = `{`), and the outer object carries a trailing comma.

**Security boundary.** The interpreter-origin gate (`assertGoogleHost`) is the control, **not** the parser. `parseLooseJSON` throws on identifiers, calls and stray backslashes, which keeps page text from being *executed* — but it does not keep page text from *producing a challenge*: JSON string escaping touches none of `'`, `{`, `}`, `:` or bare identifiers, so a single-quoted payload smuggled through a video title parses cleanly (verified 2026-08-24; an earlier comment and test claimed otherwise and both were wrong). What contains this is that the genuine `window.ytAtN(` call precedes `ytInitialData` on the live page, plus the host gate — which an adversarial probe could not defeat across ~30 host-confusion payloads (userinfo, backslash authorities, IDNA homoglyphs, control characters, percent-encoding). Residual risk if YouTube's emission order ever flipped is bounded to **denial of POT minting**, not code execution. Both extractors also try **every** candidate rather than the first: taking only the first match meant one crafted title containing `ytcfg.set({` silently reverted an install to `/att/get`, the exact condition this module prevents.

**Failure handling.** Every miss returns `null` with a distinct reason (the `REASONS` table, mirroring the `atn*` constants) logged as a `homepage-challenge:` warning, and falls through to the next source. Behavior is never worse than the pre-homepage flow. Verified live 2026-08-24: `minterSource=homepage`, no fallback warnings.

#### Watch-page challenge extraction (`internal/youtube/watch_page.go`)

`extractAttestationChallenge(page)` locates `window.ytAtN(` and then walks the argument with a **string-aware balanced-brace scan** (`ScanBalancedJSONObject`, `internal/utils/jsoncandidates.go`) rather than a non-greedy regex. moonarchive's `INITIAL_ATTESTATION_PATTERN` uses the regex form, but a `})` sequence anywhere inside the opaque payload truncates that match into an unbalanced fragment that then fails to parse — indistinguishable from "the page had no challenge". The captured literal runs through `JSToJSON` (a faithful Go port of yt-dlp's `js_to_json`, `internal/utils/jsjson.go`), is unmarshaled, and its `R` key — itself a JSON string, delivered with `\xNN` escapes on real pages — is unmarshaled again to pull out the top-level `bgChallenge`, re-marshaled compact as the challenge payload.

Every failure resolves to `""` with a **distinct reason** (the `atn*` constants, surfaced as `WatchPageResult.AttestationReason` and logged as `reason=`): no call on the page, unbalanced argument, JS-to-JSON failure, outer parse failure, no `R` key, `R` not JSON, no `bgChallenge`, bad challenge shape, no `interpreterUrl`, or disallowed interpreter host. A single catch-all reason would let a silently-broken extractor masquerade as a genuine absence, which is precisely the confusion this subsystem exists to eliminate. Absence is never an error — the sidecar's `/att/get` fallback handles it.

The value stops at `WatchPageResult.AttestationChallenge`, whose only readers are the two "no attestation challenge from watch page" Debug lines that report `AttestationReason` — the `VideoInfo` field it used to ride on was write-only and went with the rest of the challenge path (owner ruling R1, 2026-09-15). `withAttestation` still runs at every return path of both `GetVideoInfoAuthenticated` and `GetVideoInfoPublic` (including the ANDROID_VR / web_embedded / web_creator / watch-page-fallback routes that skip `mergeWatchPageMetadata`), where it resolves the GVS binding and the session verdict. The reason string is refreshed only when a watch page is actually fetched: since owner decision O-H the 30 s quality monitor answers on ONE player call (`probeVideoInfo`, `internal/worker/orchestrator_youtube.go` — ANDROID_VR, or TV with cookies on an auth-walled stream) and never touches the watch page, so on a healthy live job the value dates from the extraction that set the job up and is re-read only when the monitor falls through to its one-shot full fetch, or on a 403 credential refresh.

#### Interpreter-origin gate (security boundary)

The sidecar **executes** the interpreter body it fetches (`new Function(js)()`), and watch-page HTML embeds attacker-authored video metadata verbatim — JSON escaping leaves braces, parens and single quotes intact, so a crafted description can present itself as a `ytAtN` challenge, and on a real page the description precedes the genuine blob (verified 2026-08-15: description at byte ~740k, real call at ~802k). Before this gate, that reached `new Function()` with an attacker-chosen host.

Two independent checks now enforce the same rule — `canonicalizeChallenge` in `internal/youtube/watch_page.go` (so a hostile challenge never leaves the Go process) and `assertGoogleHost` in `bgutil-sidecar/src/server.js` (so the sidecar never trusts its caller):

- The interpreter URL must be `https:` on one of **eight exact hosts**: `www.google.com`, `google.com`, `www.gstatic.com`, `ssl.gstatic.com`, `gstatic.com`, `s.ytimg.com`, `www.youtube.com`, `youtube.com`. No suffix matching and no patterns — an adversarial review defeated both weaker forms. Suffix-matching `.googleapis.com` re-admitted `storage.googleapis.com` and `firebasestorage.googleapis.com`, which serve anyone's uploaded bucket objects (and `sites`/`script`/`drive.google.com`, which host third-party content); a `^google\.[a-z]{2,3}(\.[a-z]{2})?$` "regional Google" pattern matched *shape rather than ownership*, admitting live third-party domains such as `google.com.se` and `google.co.nl`. Both reached code execution end-to-end. Regional Google domains are consequently unsupported; the interpreter is served from a global host.
- The URL must be a **static script**: no query, no fragment, and an encoded path matching `^/[A-Za-z0-9._~/-]+\.[Jj][Ss]$`. An allowlisted host is *not* the same as Google-authored bytes — `www.google.com` serves JSONP endpoints (`/complete/search?client=firefox&jsonp=…`) that reflect an attacker-supplied callback at HTTP 200, which reached RCE through a genuinely allowlisted host with no redirect involved. Reflection requires a query to reflect, so demanding the static shape removes the class. Percent-encoding is excluded from the alphabet because Go decodes `%3F` into `url.Path` while JS's `URL` keeps it encoded: the two gates would otherwise disagree about what the path is, with safety resting on Google returning 404 for the crafted form.
- Redirects are refused outright (`redirect: "manual"`, any 3xx is an error) on all three sidecar fetches. undici follows redirects by default and only the pre-redirect host was gated, so an allowlisted host answering `302` delivered the body we execute. `/att/get` and `GenerateIT` get the same treatment: the `/att/get` response is the one trusted enough to execute inline.
- A page-sourced challenge carrying inline `interpreterJavascript` instead of a URL is **refused**, even though bgutils-js treats the two as interchangeable: inline script scraped from HTML has no origin to check at all. Those fall back to `/att/get`, whose response is a genuine YouTube API result; the sidecar honors inline script only for challenges it fetched itself (`trusted = minterSource === "att_get"`). Live YouTube ships `interpreterUrl` and never inline (verified 2026-08-15), so this costs nothing today. Homepage-sourced challenges cannot reach that branch at all — `extractHomepageChallenge` rebuilds them from exactly `program`/`globalName`/`interpreterUrl`, so inline script cannot survive extraction.

A rejected host is reported by name in the reason string, so a genuinely-Google host missing from the list surfaces as "add this name" rather than an unexplained loss of session coherence.

#### Sidecar RPC additions

`generatePoToken` gained two optional params and three result fields (also reflected in the IPC protocol table above):

- `challenge` (param, string) — a `bgChallenge` JSON string. Since the homepage-pair change this is the **second** preference, not the first: `generateMinter` builds from its own homepage (ytcfg, ytAtN) pair when it can, uses this challenge when the homepage pair is unavailable and this one is well-formed (has `program` and `interpreterUrl`, and its host passes `assertGoogleHost`), and falls back to `/att/get` otherwise — each step logged with a distinct reason. Sent by both the GVS and player mints.
- `freshMinter` (param, bool) — forces `getOrCreateMinter` to regenerate even when the cached minter is still valid. Set by GVS mints; player mints omit it and take the cached minter.
- In-flight regenerations are **keyed by challenge**: a caller joins an in-flight BotGuard pass only when it supplied the same challenge. A caller with a different challenge waits and then regenerates its own rather than silently inheriting another session's minter — the earlier shared-promise behavior reported `minterSource=challenge` for a minter built from a *different* video's page, which is worse than no provenance at all. The cost is that two jobs starting within the same BotGuard window serialize. A caller that did NOT ask for a fresh minter re-checks the cache when its queued turn comes and takes the minter the generation ahead of it just cached (`{ m, reused: true }`, reported not fresh) instead of paying for its own pass; for that reason a caller that needs a fresh minter never joins such a generation (`minterInflight` records `mayReuse` beside each in-flight promise).
- `minterSource` (result, string) — `"challenge"` or `"att_get"`, whichever input built the minter that served this specific mint.
- `minterFresh` (result, bool) — whether this mint triggered a fresh BotGuard regeneration (`true`) or reused an already-warm minter (`false`).

#### Mint provenance logging

Each GVS mint attempt logs one line at Info (`[POT] GVS mint`) on success or Warn (`[POT] GVS mint failed`) on error (`[POT] generator returned empty token` when it comes back empty), emitted by the calling strategy immediately after `mintGvsPoToken` returns:

| Field | Meaning |
|---|---|
| `jobID` | the job that requested the mint |
| `binding` | which rule produced the content binding: `"videoID"` \| `"datasyncID"` \| `"visitorData"` \| `"channelID"` |
| `tokenLength` | (Info line) length of the minted PO token string — a cheap sanity signal without logging the token itself |
| `source` | (Info line, DASH and HLS) the manifest source label the token was minted for |
| `videoSource` / `audioSource` | (Info line, VOD and manifestless) the `Format.Source` of the chosen video / audio stream; `""` when that stream is absent |
| `err` | (Warn line) the mint error |

On VOD the mint-failure Warn is the `[POT] missing_pot` line (see **missing_pot** below) rather than `[POT] GVS mint failed`.

The challenge-sourced minters stay dormant (the mint-path comment in `internal/worker/strategy_youtube_dash.go` says why), so the line carries no challenge / minter-source / sidecar provenance today; if those minters are ever trialled, those fields join the line then.

The line exists so that if a future premiere still 403s, the log alone identifies the exact configuration in play — no reproduction needed. Datasync-ID binding is no longer a pending suspect: the full yt-dlp rule is implemented (see **Binding** above), so an authenticated session without the experiment already binds to its datasync ID.

#### Which URLs carry a GVS token (per-client policy)

One predicate in `internal/youtube/types.go` (beside the `AuthLevel` block) encodes the policy for every download path — VOD, live DASH/HLS/manifestless and the 403 re-mint — over the `Format.Source` client label (`watch_page`, `web`, `web_safari`, `web_creator`, `web_embedded`, `tv_auth`, `tv_public`, `visionos`, `android_vr`, `android_vr_dash_fallback`). It is an allowlist: an empty or unrecognised label answers false.

| Predicate | True for | Meaning |
|---|---|---|
| `GvsTokenRequired` | `watch_page`, `web`, `web_safari`, `web_creator` | does a direct or DASH URL from this client 403 without a GVS token (yt-dlp's per-client GVS policy is `required=True` for HTTPS/DASH and `recommended` for HLS, where yt-dlp fetches and attaches one too, so the HLS gate asks the same question; Moombox has no Premium detection, so the not-required-for-premium carve-out never applies) |

The rest ride bare, as upstream does: `tv_auth`, `tv_public`, `web_embedded` and `visionos` have no `GVS_PO_TOKEN_POLICY` entry in yt-dlp, so its default (`required=False`) applies and it fetches no GVS token for them; `android_vr` / `android_vr_dash_fallback` are required-unless-player-token upstream and never get a WebPO (a WebPO on an android_vr URL is the 2026-08-15 403 cause the `AuthLevel` block names).

**VOD direct path** (`DownloadVod`, `internal/worker/strategy_youtube_vod.go`). Once the video and audio formats are chosen, the strategy asks `GvsTokenRequired` of each stream's own `Source`. If either needs one and a provider is present, it mints ONE token (through `mintGvsPoToken`, bound by `gvsBinding` (`internal/worker/strategies.go`) like the live mints) and attaches it per stream: a video from `web_creator` and an audio from `tv_auth` get the token on the video only. The token rides as `?pot=` (`&pot=` when the URL has a query) through `applyPoTokenQuery` (`internal/engine/downloader_fetch.go`), which all three whole-file request builders use — `probeFileSize` (the 1-byte Range probe), `fetchChunk` (every chunk) and `runDirectDownloadFallback` (the streaming fallback, `internal/engine/downloader_direct.go`) — so the probe cannot pass a URL the chunks then 403. `tv_auth`, `tv_public` and `web_embedded` formats stay bare on VOD (no upstream requirement); `visionos` and `android_vr*` never get one. A `nil` provider is a test-only shape (`cmd/moombox/services.go` always constructs one) and sends the URLs bare, never degrading. The success line is `[POT] GVS mint` (fields below).

**missing_pot** (yt-dlp's name). When a token is required but the mint fails or returns empty (an empty token is reported as `errEmptyGvsToken`, `internal/worker/strategy_youtube_vod.go`, where the helpers below also live), sending the URL bare is a certain 403, so the strategy degrades instead of failing: one Warn, `[POT] missing_pot: no GVS token — dropping web-family formats`, with fields `jobID`, `binding`, `err` (says whether the mint errored or came back empty), `dropped` (count of WEB-family formats removed) and `swapped` (how many of those were replaced by their token-free shadow). It replaces the old `[POT] GVS mint failed` / `[POT] generator returned empty token` pair on this path. The `GvsTokenRequired` formats are dropped from a filtered COPY of the pool (`withoutGvsRequiredFormats` — the caller's `VideoInfo.Formats` is never filtered in place), format selection and URL resolution (`selectVodFormats`, `resolveVodURLs`) re-run on what is left, and neither stream then gets a token. A dropped format whose `TokenFreeAlternate` (`internal/youtube/types.go`) is set is not lost: that alternate takes its place in the filtered pool — same itag, a bare URL from a client that needs no token. A manual video or audio itag whose format was swapped is honoured on the alternate, with the Info line `[FormatSelector] Manual video itag N served from <source> after missing_pot` (`audio` for audio). A manual itag whose format was dropped with no alternate falls back to auto with the Warn `[FormatSelector] Manual video itag N requires a GVS token none could be minted; falling back to auto` (`audio` for audio), distinct from the plain "not found" line. The cookieless re-extract (owner ruling 2026-09-29): when the degraded pool has LOST a chosen stream — the original selection's video, or its audio (matched on itag and audio track, `poolHasStream`), is absent because it was dropped with no shadow to swap in — and the job has a `*youtube.Service` and its context is not already cancelled, the strategy runs the cookieless chain ONCE before re-selecting: the Info `[POT] missing_pot: re-extracting with the cookieless clients` (`jobID`, `lostVideo`, `lostAudio`), then `fetchCookielessFormats` (`internal/worker/strategies.go`, the test seam over `CookielessFormats` in `internal/youtube/service.go`, bounded by the 403 refresh's `credentialRefreshTimeoutFor`) runs the cookieless chain with the Service's cached visitor data — visionos, falling through to android_vr only when visionos fails or comes back inadequate — and pools every format either client returned; a token-free itag the pool never had joins the merge as a plain selectable winner. On success `MergeFormatPool` (`internal/youtube/player_api_parsing.go`) folds those formats into a fresh copy of `VideoInfo.Formats` (never mutated) and re-deduplicates, which attaches each token-free copy to its WEB-family winner as the shadow; the degrade then re-runs on the merged pool and logs the Info `[POT] missing_pot: cookieless re-extract merged` (`jobID`, `added`, `dropped`, `swapped`). On failure the Warn `[POT] missing_pot: cookieless re-extract failed — continuing with the degraded pool` (`jobID`, `err` — on an empty pool it wraps the last client failure, so a timeout, cancel or HTTP status reads as itself) and the selection runs on the first degraded pool. Either way the selection and URL resolution run once, on the final pool, and the Info `[POT] missing_pot: serving token-free formats` (`jobID`, `videoSource`, `audioSource`) names the client each stream now rides. The re-extract never runs on a successful mint, nor when the shadows covered both chosen streams. (The whole-file URL refresh asks the chain again later, for a token-free stream whose expired URL the re-fetched pool cannot replace — see The Whole-File VOD Path.) If nothing selectable remains — the cookieless clients had nothing either — the download fails with `VOD: no formats usable without a GVS PO token (mint failed: <cause>)`.

The dedup shadow. yt-dlp decides the GVS token before it deduplicates, so a WEB-family copy skipped for a missing token never hides the next client's copy of that stream; Moombox dedups first and learns about a failed mint only at download time. So `deduplicateFormats` (`internal/youtube/player_api_parsing.go`) keeps the lowest-`AuthLevel` winner per stream identity exactly as before AND, when that winner's client requires a GVS token (`GvsTokenRequired`, `internal/youtube/types.go`), keeps the lowest-`AuthLevel` token-free copy of the same stream on the winner's `Format.TokenFreeAlternate` (`json:"-"`, never serialised — a resume re-extracts; the alternate never carries one of its own). A token-free winner carries none: a tv copy (`AuthLevel` 0/1) always wins outright, and a `web_embedded` copy (6) beats a `web_creator` one (7). The shadow reaches the shapes where the extraction cascade (`internal/youtube/player_api_strategy.go`) actually fetched a token-free copy of a WEB-family winner's stream: a `watch_page` (2/3), `web_safari` (4) or `web` (5) winner over a `web_embedded` copy (6, always queried on the authenticated path), or a `web_creator` winner that was inadequate on its own, so the cookieless chain ran and left a `visionos`/`android_vr` copy (8/9). An adequate `web_creator` response skips that chain, so an itag only `web_creator` returned (the 2026-09-29 VOD 403 shape, where TV returned nothing) carries no shadow: the first missing_pot degrade logs `swapped=0` there, and the cookieless re-extract (above) is what supplies the copy — its merge re-runs this dedup, so the fetched `visionos`/`android_vr` formats become the shadows the second degrade swaps to. `MergeFormatPool` also feeds each shadow the pool already carried back in as a candidate, so a merge never loses one. `collapseToPreferredRendition` (same file) carries the shadow with the kept rendition, and the collapse count is unchanged.

**Live paths** (DASH, HLS, manifestless). A live strategy mints and attaches a GVS token iff `GvsTokenRequired` answers true for the URL's client — the same rule as the VOD path: a `watch_page`, `web`, `web_safari` or `web_creator` manifest or stream is tokenised; a `tv_auth`, `tv_public`, `web_embedded`, `visionos` or `android_vr*` one rides bare:

- DASH and HLS decide once per download by `VideoInfo.DashManifestSource` / `VideoInfo.HlsManifestSource` (`internal/youtube/types.go`, JSON `dashManifestSource` / `hlsManifestSource`, both `omitempty`) — the client that served the manifest URL. For DASH the token goes on the manifest path (`/pot/<token>`) and on both downloaders; for HLS on the master and variant paths and the downloader.
- The manifestless strategy decides per stream by the chosen format's `Source` (`formatSourceByItag`, `internal/worker/strategies.go`, read after selection and cipher re-selection settle the itags). It mints once when either stream requires the token and hands it only to that stream's downloader.
- Every bare decision logs one Info line, `[POT] GVS token skipped`, and mints nothing (`logGvsTokenSkipped`, `internal/worker/strategies.go`): fields `jobID`, `source` (the label, or `unknown`), `reason` (`no GVS token required for this client`, or `source not recorded` when the label is empty), plus `stream` (`video`/`audio`) on the manifestless path. An unrecorded manifest source therefore means no token (`source=unknown`) — every extraction site stamps one, so an empty label is a bug to read in the log, not a token to guess at.
- The **403 re-mint** applies the same gate. `refreshGvsCredentials` (`internal/worker/strategies.go`) derives the refreshed itag's source from the setup formats; a stream whose client needs no token logs `[POT] GVS token skipped` (extra field `tag`), re-mints nothing and returns an empty token so the engine keeps its existing one, while the URL half still refreshes. The URL half keeps the current URL when the re-fetched itag's dedup winner now comes from a client whose `GvsTokenRequired` answer differs from the setup client's, in either direction (`tokenClassChanged`, same file): a bare setup cannot give a WEB-family URL the token it needs, and the engine cannot clear a token it already holds from a URL that needs none. It logs the Warn `[POT] credential refresh: itag now served by a client of a different token class — keeping the current URL` with `setupSource`, `freshSource` and a `reason` naming which side requires the token. The DASH and HLS `OnCipherFailure` (`internal/engine/downloader.go`) paths re-mint nothing: DASH invalidates the caches and re-resolves the segment URL, and HLS only invalidates the caches.

The manifest sources are stamped by the extraction cascade in `internal/youtube/player_api_strategy.go` (`stampManifestSources`), everywhere a manifest URL lands on a returnable `VideoInfo`, together with the URL: the watch-page parse (`watch_page`; also in the public path, and the watch-page fallback return); the TV result (`tv_auth` authenticated, `tv_public` public); `fetchWithEmbedded` (`web_embedded`, also the age-restricted embed results); the authenticated WEB / WEB Safari DASH adoption into the TV result (`web` / `web_safari`); the WEB_EMBEDDED DASH adoption (`web_embedded`); the ANDROID_VR DASH adoption (`android_vr_dash_fallback`, authenticated fallback and public enrichment, fresh or reused from the chain's prefetch); WEB_CREATOR (`web_creator`); the cookieless fallback chain's result and DASH adoption (`visionos` / `android_vr`); `mergeWatchPageMetadata` (copies both sources with their URLs); and the two probes (`ProbeVideoStatus` → `android_vr`, `ProbeVideoStatusAuthenticated` → `tv_auth`).

#### Mid-job re-mint: behind-head 403 recovery

A stale-token or stale-URL 403 mid-download no longer requires a full downloader restart to fix. The recovery splits across two layers: the engine decides *when* to ask, the worker decides *how* to answer.

**Engine half** (`internal/engine/downloader.go`, `downloader_fetch.go`, `downloader_dash.go`) — `SegmentDownloader.poTokenOverride` mirrors the existing `baseURLOverride`: `SetPoToken`/`getPoToken` make the PO token replaceable in place, atomically, without tearing the downloader down. `DownloaderOptions.OnCredentialRefresh func() (baseURL, poToken string)` is the seam a strategy wires; `refreshCredentials()` calls it and installs whatever it returns (either return value may be `""` to leave that half alone). A 403 burst can hit every catch-up worker in the same instant, so the call is gated by `credentialRefreshCooldown` (5s) via `atomicTime.TryClaim` — exactly one player-response round trip per cooldown window, never one per failing segment.

`fetchSegmentWithRetry` (the parallel catch-up path) still treats 410 as immediately permanent. For 403 the branch point is `behindHeadTailPending()` — true only when the segment is below the known head **and** the per-segment stall budget (`MaxTimeout` since the last written segment) hasn't been exhausted. A 403 that fails that check — because it's at or past head, or because the stall budget already ran out while still below head — is immediately permanent: that is how a finished stream signals "no such segment," and VOD/post-live finalization depend on it staying that way. A 403 that passes it (the segment demonstrably exists, per the harvested `X-Head-Seqnum`, and the stall clock hasn't expired) instead calls `refreshCredentials()` and retries, bounded by `forbiddenRefreshAttempts` (5) attempts on that segment; a refreshed base URL is rebuilt into the retry through the caller-supplied `rebuildURL` closure rather than reused stale, and exhausting the attempt budget still returns `ErrSegmentPermanent` — no caller's contract changed. Any other failure that lands while `IsOnline` reports the device offline is not charged at all: the call waits for connectivity and retries the same attempt, so an outage longer than the ~50 s backoff no longer exhausts the budget and writes a gap. The sequential DASH loop (`runDashLoop`/`handleGoneError`) never goes through `fetchSegmentWithRetry`, so it fires the same `refreshCredentials()` call directly through the same `behindHeadTailPending()` gate, once a gone burst persists past `postBytes403CipherThreshold` (5) consecutive failures — a pure side effect placed ahead of the existing end-of-stream verdict logic, so it cannot perturb that logic's finalize/warn paths.

A failure episode also throttles the next catch-up window: `catchUpBatchLimit()` drops to a floor of one full parallel wave — `segmentWorkers()` segments (12 by default via `downloader.segment_workers`, configurable) — once `noteCatchUpFailureEpisode()` fires, and regrows by 1 segment every `catchUpRegrowInterval` (1s) back up to `maxCatchupBatch()` (`8 * segmentWorkers()`, 96 at the default) — moonarchive's `batch_count = min(1 + time_since_check/10, batch_count)` provided the shape, though the floor and interval were retuned 2026-08-15: the original 1-segment floor regrowing at 10s (a closer copy of moonarchive's heartbeat-driven damping) left real catch-up running 1-3 segments wide for the duration of a 403 storm, ~20-40x slower than the un-damped width. Refresh-and-retry (above) is what actually stops a 403 storm now — 403 volume fell from ~1100 in 18s to ~12/minute once it landed — so the damped floor only needs to avoid dispatching a full-width wave of doomed requests while recovery is still in flight, not carry the whole recovery itself.

**Worker half** (`refreshGvsCredentials`, `internal/worker/strategies.go`) supplies the callback. Among the live strategies only the manifestless DASH strategy wires it, into both the video and audio downloaders' `OnCredentialRefresh` (`strategy_youtube_manifestless_dash.go`); the manifest-based DASH and HLS strategies do not wire it yet. The whole-file VOD strategy wires its own answer, `refreshVodURL` (see The Whole-File VOD Path), because a whole-file itag is exactly what `refreshGvsCredentials` refuses to install (`formatBecameWholeFile`) and a VOD stream may ride a missing_pot shadow the setup pool does not name. `refreshGvsCredentials` mints the GVS token FIRST, with `bypassCache: true` — handing back the cached token would make the refresh a no-op, since that cached token is the exact credential that just earned the 403 — spending the full remaining budget on the mint before the URL half gets whatever is left over. The URL half then re-fetches the player response via `job.YT.GetVideoInfo` rather than re-resolving the cached, already-stale `Format`: a cached Format re-run through a freshly invalidated cipher solver reproduces the same expired URL byte-for-byte, because the URL's `expire`/`ei` parameters come from the player response, not from cipher decryption. It falls back to the caller's cached formats if the re-fetch fails, and only runs at all when a player URL and a cipher solver are both available — mirroring `OnCipherFailure`'s own install guard. The content binding is resolved once at strategy setup (`gvsBinding`) and threaded into every refresh call rather than recomputed per call: `invalidate403Caches` unconditionally clears visitor data on every refresh, so recomputing would let the second downloader's refresh (or any later retry) silently drift onto the degraded channelID/videoID fallback while the first refresh used the real binding. The whole round trip — mint plus re-fetch plus resolve — is bounded by `min(45s, MaxTimeout/3)` (`credentialRefreshTimeoutFor`): the clamp exists because a job running near the 30s `MaximumTimeout` floor can't afford a flat 45s ceiling — that would let one refresh attempt consume the entire stall budget and flip `behindHeadTailPending` false at the exact moment working credentials arrived.

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
- **TTL**: 24 hours (`playerCacheTTL`). Checked on read via file modification time. Expired files are deleted. The TTL is only an offline backstop: every fetch revalidates the cached player with a conditional GET, so an in-flight player rotation is caught regardless of file age, and the previous 14-day value was meaningless because the player rotates long before then.
- **Eviction**: `Evict()` scans the directory and removes files older than the TTL. Called on startup.
- **Atomic writes**: Uses `.tmp` file + rename pattern.

#### Memory Cache (LRU)
- **Type**: `map[string]*Solvers` with a `[]string` order slice.
- **Key**: `SHA256(playerURL)`.
- **Max size**: 10 entries (`solverCacheSize`).
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

Authentication: both the `Cookie` header and the SAPISIDHASH `Authorization` header come from CALLBACKS (`cookieHeader` / `generateAuth`, `api.go`) invoked at request time, not from values captured when the downloader was built — a chat download outlives several cookie refreshes, and a snapshot would keep presenting whatever the job started with. The `Authorization` header is what member-gated chat needs.

**A 401 is a PERMANENT exit for the job, by design.** `handleFetchError` (`downloader.go`) treats `ErrAuthRequired` as a reason to stop polling for good rather than to burn the consecutive-error budget on a credential state that cannot recover without a refresh, and it closes the live-continuation signal on the way out: a downloader that has stopped polling carries no information any more, and leaving that signal latched true would hand the engine permanent — and wrong — resume evidence. There is deliberately no restart-on-recovery. A chat downloader still running re-reads its credential per request and picks a repaired cookie up on its own; one that has already exited stays exited for that job. The exit is a give-up: `Start` returns `errChatAuthLost`, so the job's `chat_status` reads "incomplete", not "finished".

### Continuation Lifecycle

1. **Initial extraction**: From the watch page HTML via `ExtractChatContinuation`. Navigates `ytInitialData.contents.twoColumnWatchNextResults.conversationBar.liveChatRenderer.continuations` and tries keys `reloadContinuationData`, `invalidationContinuationData`, `timedContinuationData`, `liveChatReplayContinuationData`.
2. **API responses**: Each response includes the next continuation token in `continuationContents.liveChatContinuation.continuations`.
3. **All Chat upgrade**: On the first response, the `header.liveChatHeaderRenderer.viewSelector.sortFilterSubMenuRenderer.subMenuItems[1]` contains the unfiltered "Live Chat" continuation token. YouTube defaults to "Top Chat" which can aggressively filter messages. The downloader switches to "All Chat" on the first response by using this alternative continuation token.
4. **Stale recovery**: When a continuation returns no data but the stream is still active, the downloader fetches a fresh continuation token from the watch page (`FetchFreshContinuation`, whose URL carries the same `bpctr=9999999999&has_verified=1` pair the extraction path's watch-page fetch does — an age-gate shell has no `liveChatRenderer`, so without them an age-restricted stream reads as "no chat"). If this fails, it retries with exponential backoff (10 s initial, doubling, 5-minute cap, `maxStaleContinuationAttempts` = 12 attempts ≈ 35 min — 11 sleeps of 10, 20, 40, 80, 160 then 300 s × 6). The same bound applies a second time OUTSIDE that call: `runChatLoop` counts consecutive recoveries that succeeded and were reported complete on the very next poll — no progress under a new token — sleeps its own 5 s→5 min ladder between them, and gives up at the same 12. Both give-ups keep the resume sidecar and make `Start` return `errStaleRecoveryExhausted` — the inner one only when the loop was NOT asked to stop while the ladder was sleeping, since `recoverStaleContinuation` returns the same `false` for a `Stop`/`MarkStreamEnded` and that is an ordinary end, not a give-up; see the completion rule under **Resume State**.

### Message Types

The chat API response contains `actions` array items. Replay responses wrap actions in `replayChatItemAction` with a `videoOffsetTimeMsec`. YouTube reports `videoOffsetTimeMsec = 0` for every message sent before the stream started; `parseAction` recovers the real signed offset from `timestampText` (e.g. `"-2:30"`) via `parseNegativeTimestampText` in that case, so pre-stream waiting-room chat is not piled onto 0:00. Each action contains an `addChatItemAction.item` which may contain:

| Renderer | Description |
|----------|-------------|
| `liveChatTextMessageRenderer` | Regular chat messages. |
| `liveChatPaidMessageRenderer` | Super Chat (monetary donation with message). |
| `liveChatPaidStickerRenderer` | Super Sticker (monetary donation with sticker). |
| `liveChatMembershipItemRenderer` | New-member and milestone messages. |
| `liveChatSponsorshipsGiftPurchaseAnnouncementRenderer` | A gifted-membership purchase. Its author, badges and "Gifted N memberships" line sit one level down in `header.liveChatSponsorshipsHeaderRenderer` and are hoisted into the flat layout by `giftPurchaseFields`. |
| `liveChatSponsorshipsGiftRedemptionAnnouncementRenderer` | The recipient's side of a gifted membership. Flat, and its line is its `message`. |

Every membership shape is archived with `isMembership` set, and the renderer's own header line is archived beside the typed message parts as `ChatMessage.MembershipText` (`membershipText,omitempty` — absent from every file written before the field existed): `headerPrimaryText` ("Member for 6 months") when present, else `headerSubtext` ("Welcome to Member!"), and the gift purchase's hoisted `primaryText` ("Gifted 5 memberships"). It is deliberately NOT folded into `message` — the video overlay renders `message` alone and must stay silent, while the sidebar's member card shows the line beside the author (2026-09-25 ruling K4). A gift redemption carries no `membershipText` because its line IS its `message`.

Super Chat tiers (1-7; blue, cyan, green, yellow, orange, magenta, red) are resolved from YouTube's ARGB renderer colors:

- One table in `internal/chat/types.go` holds both colors of every tier: the header color (`headerBackgroundColor` on a paid message; `moneyChipBackgroundColor` on a Super Sticker, inferred from the field name) and the body color (`bodyBackgroundColor`; sticker `backgroundColor`). A renderer color is looked up regardless of which field carried it, header field first, so a swapped field still resolves.
- Colors arrive as float64 or, like other numeric fields here, as decimal strings; any other shape is reported at Debug and treated as absent.
- Every record carries the raw colors as `headerColor` / `bodyColor` (`#RRGGBB`) and `kind` (`message` or `sticker`).
- A color matching no row is archived as tier 0 (the unknown marker; there is no label field) with color `gray`, which no row uses, and logged ONCE per distinct raw pair at Warn (`chat: unknown superchat tier color`: kind, both hex colors, both raw ARGB decimals, amount; the hex drops alpha, the raw values do not), so a color change can be added from the archive alone. Before 2026-09-05 the table held only the body colors but was looked up with the header value, so every archived Super Chat was tier 1 blue and every sticker tier 0.

### Deduplication

Identical to Twitch chat: a `utils.OrderedDedup` (a set that keeps insertion order). After every successful fetch it is culled back to the newest 5000 IDs (`dedupKeepSize`) once it holds more.

### File IO Strategy

1. **First flush**: Atomic write (`utils.WriteChatFileAtomic`: a uniquely named temp file, fsync, rename). Complete JSON with all messages.
2. **Subsequent flushes**: Incremental append (`utils.AppendChatMessages`). Read the last 256 bytes to find the messages array's `]`, write the new messages plus the closing structure from there, then truncate. Memory cost: O(new messages), not O(file size). The `]` must be the messages array's own: followed by nothing but the object's closing `}`, and where the writers put it — on its own line at the top level's two-space indent (`\n  ]`), or straight after its `[` when the array is empty. A file whose tail is zero-filled (a crash after the size grew but before the data reached disk) has no `]` at all; one cut mid-record has its last `]` inside the final message's own array; and one cut right after a message ends with that message's own `]` and `}` (`]}` compact, `\n      ]\n    }` indented). Splicing into any of them used to report success over a file that no longer parsed. Each refuses with `utils.ErrChatFileDamaged` and writes nothing.
3. **Header updates**: `messageCount` and `downloadedAt` are updated in-place by reading only the first 1024 bytes of the file. The `messageCount` value is padded to 20 characters with trailing whitespace to keep byte offsets stable.
4. **Batching window**: Messages are batched within a 1-second window (`writeIntervalMs = 1000`).
5. **A failed append's write** (`utils.ErrChatFilePartialWrite`, a full disk say): the append puts the file's `]\n}` back, so the file holds what it held before, and the batch stays buffered for the next flush. It used to be dropped while `messageCount` went on counting it. If putting the end back fails too, the file ends in whatever part of the batch was written: the next append sees `ErrChatFileDamaged`, the salvage below keeps every intact message, and the batch's messages the file already holds are not written a second time.
6. **Fallback** (`rewriteWithHistory`): any other append failure rewrites the file whole — its history, then the batch. The history is read by `utils.SalvageChatMessages` (`internal/utils/chatfile.go`, shared with the Twitch writers): a file that no longer parses keeps every message before the damage, the damaged original is kept beside it as `chat.json.corrupt` (a hard link, else a copy) and the damage is reported. It used to read such a file as nothing at all and replace the whole history with one batch while the header kept counting it. A file that cannot be read at all is not rewritten; the batch waits. The count becomes the length of the array written, an empty salvage writes `[]` rather than `null`, and the dedup and the replay high-water mark are rebuilt from what the rewrite holds: the history the damage took is no longer committed, so nothing may filter a replay pass's copy of it. A sidecar whose file is gone clears both for the same reason.
7. **Resume over a damaged file**: a sidecar resume checks that the file still ends the way an append needs (`utils.ChatFileEndIntact`) and, if not, runs the same salvage at `Start`, so a run that gets no new message still leaves a parseable file. A sidecar whose chat file is gone starts its count from 0, since the first flush writes the file whole from this run's buffer.

### Error Limits

The run gives up when the consecutive-error count EXCEEDS the limit (`>`, matching the TypeScript original), i.e. on the 21st live or the 6th replay error in a row.

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
  "streamStartMs": 1709000000000,
  "mode": "live",
  "replayHighWaterUsec": 1709003600000000
}
```

Resume state is saved after each disk flush. On restart, the downloader loads the count, the dedup set and the epoch; the continuation and the All Chat switch follow the continuation-preference rule below (a live run with a fresh watch-page token keeps it and still upgrades to All Chat; a replay, or a live run with no fresh token, resumes the sidecar's token and skips the switch). `replayHighWaterUsec` is the run's replay high-water mark — the highest absolute `timestampUsec` committed — and is restored with the rest, as the adoption rule reads it off the file's newest message when there is no sidecar: a run that starts on a replay token (the broadcast ended while no run was polling) pages the archive from the top, and only the mark keeps what the live half already committed from being appended again, since the 5000-ID window cannot span the archive. It was per-run state reset at `Start`, and such a run re-appended a 6000-message capture whole. `mode` is `"live"` or `"replay"` — which kind of run wrote the sidecar: the mode rule is that a replay run refuses a live-tagged sidecar and starts from scratch instead of adopting its count/continuation/dedup/epoch, since those all describe the live half of the file; a sidecar with no `mode` (written before the field existed) is adopted as before.

**Continuation-preference rule (`Start`'s resume block, `internal/chat/downloader.go`):** a LIVE/upcoming run whose caller already supplied an `InitialContinuation` (the token `FetchWatchPage` just returned) keeps that fresh token and takes only `MessageCount`, `RecentIDs`, `flushedToDisk` and `StreamStartMs` from the sidecar. The rule skips an assignment rather than installing anything: on a freshly constructed downloader what survives is the caller's `InitialContinuation`, and on a reused instance (the orchestrator re-`Start`ing a finished early downloader) it is the token that instance's previous run last used — the same value the sidecar was saved from. The sidecar's continuation is by definition the one the previous run left off at, and the exit the completion rule preserves it for is stale-continuation exhaustion — so adopting it costs a wasted poll at best and, when the expired token errors rather than completing, the whole consecutive-error budget. Because a watch-page token is a Top Chat token, such a run is NOT `resuming` as far as `runChatLoop` is concerned: it is entered with `resuming = false` so the All Chat upgrade still happens. For a REPLAY the sidecar's continuation IS the position in the archive (a fresh token would restart the VOD from the top), so it always wins; a live run with no `InitialContinuation` also keeps today's behaviour.

**Completion rule (`Start` / `applyCompletionRule` / `clearResume`, `internal/chat/downloader.go`):** the sidecar is deleted only on a GENUINE completion — the orchestrator's `MarkStreamEnded` verdict, or a replay/VOD run (`!IsLiveOrUpcoming`) whose loop reached the end of the archive, i.e. left with no terminal error — and only when no IO error fired during the run (`ioErrorOccurred`). A replay that GIVES UP — `handleFetchError`'s budget (the 6th consecutive failure, transient ones included, after 75 s of backoff between them) or an auth loss — keeps its sidecar, like a cancelled or shut-down one: a replay's sidecar continuation IS its position in the archive (the continuation-preference rule below never overrides it), so the next run resumes there instead of paging from the top. Clearing it on a give-up used to be the rule, on the grounds that a VOD's chat is re-fetchable in full — but the next run then found the archive with nothing describing it and rewrote it from its own first page. Every other exit of a live/upcoming run KEEPS the sidecar and refreshes it (`saveResume`) on the way out: stale-continuation exhaustion, `handleFetchError`'s consecutive-error budget, and `ErrAuthRequired` all mean "this run stopped", not "the stream ended". **Stale-continuation exhaustion is two caps, not one.** The inner one is `recoverStaleContinuation` giving up after `maxStaleContinuationAttempts` tries at fetching a fresh token; the outer one is `runChatLoop`'s own `staleRecoveries` counter, which reaches the same bound through recoveries that each "succeeded" and were reported complete on the very next poll — the case the inner, per-call `contRetries` could never see. Both keep the sidecar, and both are GIVE-UPS rather than completions: the loop records `errStaleRecoveryExhausted` through `setTerminalErr`, `Start` returns it, and `chatStatusForOutcome` (`internal/worker/orchestrator_chat.go`) turns any non-nil outcome into `chat_status = "incomplete"` so a capture that stopped short of a still-live broadcast is not shown as finished. The inner cap is armed only when the loop was not asked to stop mid-retry: `recoverStaleContinuation` answers `false` for a `Stop`/`MarkStreamEnded` too, and labelling that incomplete would mis-report every ordinary shutdown. `handleFetchError`'s budget closes the same way one branch over, with `errChatFetchExhausted`, and so does its `ErrAuthRequired` exit, with `errChatAuthLost` (which wraps `ErrAuthRequired`). A panic `Start` recovers is a give-up too, in all three chat downloaders (this one, `twitch.ChatDownloader` and `twitch.VodChatDownloader`): `Start` returns it as an error rather than the nil it once fell through to, and this downloader records it through `setTerminalErr` before `done` closes, so the early-chat handoff waiter reads the same verdict. Cancellation/shutdown is preserved as before, and its sidecar save is made in `Start` after the final flush, like every other kept-sidecar exit — so a `Stop` that lands after the loop has already left on a give-up still finds the save made. The rule exists because the sidecar is the only record telling the NEXT run that `chat.json` already holds history: a waiting-room chat that YouTube resets after inactivity used to lose its whole archive, because the next run found no sidecar, started at count 0, and full-wrote the file on its first message.

**Adoption rule (`Start` / `adoptExistingChatFile`, `internal/chat/downloader.go`):** the completion rule protects future runs; adoption protects the ones whose sidecar is already gone. LIVE/upcoming runs ONLY — a replay/VOD continuation restarts the archive from the top and the loop culls the dedup to 5000 IDs on its first fetch, so adopting there would re-append everything older than the retained window; on that path a full rewrite is the correct behaviour, made safe by the re-run rule below. When a live/upcoming `Start` finds no usable sidecar but `OutputFile` exists, it adopts that file as history instead of ignoring it — `messageCount` becomes the length of the file's messages ARRAY (deliberately, not its header count — the array is the data, and taking its length self-heals a lying header on the first flush), the dedup is seeded with its message IDs so an overlapping poll cannot duplicate them, `flushedToDisk` is set and the in-memory buffer cleared, so the first flush APPENDS. `OnStart` reports the adopted count with `resuming = true` (the counts the run reports stay cumulative, so `total_chat_messages` never drops), but `runChatLoop` is still entered with `resuming = false`: an adopted file comes with a freshly-fetched continuation that still defaults to Top Chat and so still needs the All Chat upgrade a sidecar resume skips. A missing file is a fresh start as before, and a file that parses but holds no messages is not adopted. A file that does NOT parse is reported through `OnError` (latching `ioErrorOccurred`, so this run's sidecar survives) and moved aside to `<chat.json>.corrupt` — overwriting it in place would destroy the only copy of something salvageable, and refusing to write at all would make a multi-hour waiting room buffer its whole chat in memory. A failed rename is itself reported and the run proceeds, so the first full write does then overwrite those bytes: an unreadable file must not stop the job archiving for good. **One file, one epoch:** every `offsetMs` already on disk was computed against whatever `streamStartTime` that file's FIRST run wrote to its header — typically a stream's SCHEDULED start, since early chat begins before the ACTUAL start is known — and a restarted or adopted run must keep computing against that same epoch, not the newer one its own `StreamStartTime` option carries. The epoch travels with the sidecar (`ChatResumeState.StreamStartMs`, taken above whenever a usable sidecar loads) and is adopted from the file header on the sidecar-less path (`adoptExistingChatFile` parses the adopted file's `streamStartTime` and takes it over `cd.streamStartMs` when it differs); `epochRFC3339()` then renders whichever epoch is in effect back into the header on every full rewrite, so the header never drifts from the offsets it labels. When the adopted file's header epoch is absent or fails to parse, the run keeps its OWN epoch instead — a file with no usable `streamStartTime` was written without offsets in the first place, so there is no existing offset for a wrong epoch to mis-label. The epoch also holds across the live→replay flip inside ONE live/upcoming run (`adoptFreshContinuation` switching to the replay endpoint once the broadcast ends, or a run that began on a replay token): the replay pass is handed YouTube's `videoOffsetTimeMsec`, which counts from the ACTUAL start, so in a live/upcoming run `processBatch` derives every offset from `timestampUsec` against the file's epoch instead. Keeping YouTube's offsets there put the recovered post-live tail (actual − scheduled) early relative to the live half, under the one bias the player applies per file. A replay/VOD run keeps YouTube's offsets — its file is replay-only (the mode rule).

**Re-run rule (`beginReplayRerun` / `finishReplayRerun`, `internal/chat/downloader.go`):** the replay half of the adoption rule's protection. A replay run that finds an archive on disk (a header counting at least one message) and no usable sidecar — the archive a completed replay left, a live capture whose sidecar the mode rule refuses, or one an older build left — writes to `<chat.json>.rerun` (`replayRerunSuffix`) with its own sidecar beside it, not over the archive. When it ends, its file replaces the archive only if the run completed or holds at least as many messages (and its flushes succeeded); then the completion rule applies to the archive's own paths as usual, so a re-run that got further but gave up leaves the sidecar the next run resumes from. Otherwise the `.rerun` file is removed, the archive stays, the run's count goes back to the archive's (so `MessageCount` and the job row describe the file that is there), and no sidecar is written — one would describe the discarded re-run. Before the rule a re-run's first flush rewrote `chat.json` from its own buffer, and a re-run that then gave up after one page left 200 messages where a complete 2000-message archive had been.

**VOD start refresh (`vodStatusUpdates`, `internal/worker/stream_processor.go`):** a job created `Upcoming` still carries the SCHEDULED start in `job.StreamStartTime`. Once the stream is classified VOD or post-live, `vodStatusUpdates` overwrites `stream_start_time` with `info.ScheduledStartTime` — which for a finished stream is YouTube's report of the ACTUAL start, the same clock the replay chat's `videoOffsetTimeMsec` offsets count from. This is what lets the player's chat-to-video bias (`computeChatBiasMs`, `web/public/modules/chat-timeline.js`) land at 0 for a finished job even though the chat file's own header epoch is the scheduled start the first (often early-chat) run wrote: the job row, not the chat file, is what gets corrected.

---

## Caching Strategy Summary

| Service | Cache Type | TTL | Size Limit | Eviction Strategy | Key |
|---------|-----------|-----|-----------|-------------------|-----|
| YouTube Visitor Data | Memory (single value) | None | 1 entry | Overwrite | N/A |
| YouTube STS | Memory map | None | 150 entries | Random eviction when full | SHA256(playerURL) |
| Twitch Emotes | Memory LRU + TTL | 24 h (`emoteCacheTTL`, `internal/twitch/emotes.go`) | 200 channels | Oldest by insertion order | lowercased channelLogin |
| Cipher (Disk) | Disk files | 24 h (`playerCacheTTL`), revalidated by conditional GET on every fetch | Unbounded | File age check on read; startup sweep | SHA256(playerURL) |
| Cipher (Memory) | Memory LRU | Unbounded (no expiry) | 10 solvers | Oldest by insertion order | SHA256(playerURL) |
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
- `internal/youtube/` -- Service facade, PlayerAPI (strategy + parsing), Auth, WatchPage, Browse, FormatSelector, Types (13 files, ~6,700 lines).
- `internal/twitch/` -- Service, API, Auth, HLS, Chat (IRC + recording + file), VodChat, Emotes, PlaybackToken, LivenessProbe, Delays, Types (14 files, ~6,400 lines).
- `internal/bgutils/` -- PotProvider, WebPoClient, Challenge, BotGuard, WebPoMinter, Types (6 files, ~2,050 lines; the sidecar manager is the `sidecar/` subpackage, 7 files, ~1,900 lines).
- `internal/cipher/` -- Solver, PlayerCache, Extractor, STS, Decrypt, ResolveURL, sidecar routing, Types (13 files, ~3,100 lines).
- `internal/goja/` -- Runtime, DOMShim, Encoding, Timer (5 files, ~1,500 lines).
- `internal/chat/` -- Downloader, API, Types (3 files, ~3,000 lines).
- `internal/constants/constants.go` -- All API keys, URLs, client configs, timeouts, limits.
