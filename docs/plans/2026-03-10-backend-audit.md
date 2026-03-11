# Full Backend Audit — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Audit all unaudited Go backend packages (~45,700 lines across 20 packages + main.go) for security issues, bugs, dead code, deduplication opportunities, anti-patterns, and consistency — matching the rigor of previous TUI and Web UI audits.

**Architecture:** Bottom-up audit through the dependency graph: leaf packages first (no internal deps), then data/config layer, platform services, download pipeline, web server, and finally the entry point. Each phase is one functional group producing one commit. Bugs found get tests added inline.

**Tech Stack:** Go 1.25, modernc/sqlite, chi/v5, goja, nhooyr.io/websocket, BurntSushi/toml

**Previous audit reference:** See commits `ac25e10` (Web UI, 14 fixes), `0279aa2` (TUI, 71 fixes), `e902d0f` (tests, 4 files strengthened) for the established audit pattern and commit message style.

---

## Audit Checklist (applied in every task)

Every file in every task is checked against these 8 categories:

1. **Security** — injection, auth bypass, cookie/token leaks, path traversal, unvalidated input, TOCTOU
2. **Bugs** — logic errors, race conditions, resource leaks (goroutines, file handles, connections), nil derefs, off-by-ones, integer overflow
3. **Error handling** — swallowed errors, missing cleanup on error paths, panic without recovery in goroutines, inconsistent error wrapping
4. **Dead code** — unused functions/methods/fields/constants, unreachable branches, stale comments
5. **Deduplication** — repeated patterns across files that should be extracted into shared helpers
6. **Anti-patterns** — goroutine leaks, mutex misuse (lock without unlock, nested locks), context.Background() where a real context should propagate, unbounded growth (maps/slices/channels)
7. **Consistency** — naming conventions, error message style, log level choices, parameter ordering, comment style
8. **Tests** — add tests for bugs found, strengthen weak existing tests, verify edge cases

---

## Task 1: Foundation Layer

**Packages:** goja, errors, constants, disk, logger (~3,125 lines)
**Why first:** These are leaf packages with zero internal dependencies. Every other package builds on them.

**Files to audit (read in this order):**

*goja/ (1,321 lines — JS VM wrapper):*
- `internal/goja/runtime.go` — VM factory, field mapper, RunString/CallFunction helpers
- `internal/goja/dom_shim.go` — window/document/navigator stubs for YouTube/BotGuard JS
- `internal/goja/encoding.go` — btoa/atob, TextEncoder/TextDecoder
- `internal/goja/timer.go` — setTimeout/setInterval/clear*, TimerManager lifecycle
- `internal/goja/runtime_test.go`
- `internal/goja/timer_test.go`

*errors/ (755 lines — typed errors):*
- `internal/errors/errors.go` — MoomboxError, YouTubeError, DownloadError, NetworkError, etc.
- `internal/errors/errors_test.go`

*constants/ (392 lines — global constants):*
- `internal/constants/constants.go` — User-Agents, API endpoints, client configs, limits

*disk/ (57 lines — Windows disk space):*
- `internal/disk/disk_windows.go` — kernel32 GetDiskFreeSpaceExW syscall

*logger/ (600 lines — structured logging):*
- `internal/logger/logger.go` — slog wrapper, rotation, ring buffer, per-job buffers, pub/sub
- `internal/logger/logger_test.go`

**Specific things to look for:**

| Package | Key concerns |
|---------|-------------|
| goja | Timer goroutine leaks (setInterval never cleared), DOM shim panic on unexpected JS calls, encoding edge cases (non-ASCII btoa), TimerManager cleanup on VM shutdown |
| errors | Consistent error wrapping (fmt.Errorf with %w), Error() method formatting, sentinel vs typed error usage, Is/As compatibility |
| constants | Stale URLs/versions that may have changed upstream, duplicated values, magic numbers that should be named constants, consistent naming |
| disk | Syscall error handling (what if drive letter is invalid or UNC path?), overflow on very large disks (>4TB), zero-value return on error |
| logger | Ring buffer concurrency safety (reads vs writes), per-job buffer unbounded growth, rotation file handle leaks, pub/sub subscriber cleanup, stdout suppression races |

**Steps:**

1. Read every file listed above, taking notes on issues found per audit category
2. Fix all issues — prioritize security and bugs, then quality and consistency
3. Add or strengthen tests for any bugs fixed
4. Run: `go build ./...` — verify clean compilation
5. Run: `go test ./internal/goja/... ./internal/errors/... ./internal/logger/...` — verify all pass
6. Run: `go vet ./internal/goja/... ./internal/errors/... ./internal/constants/... ./internal/disk/... ./internal/logger/...` — verify clean
7. Commit with message following pattern: `fix: backend audit phase 1 (foundation) — N fixes across M files`
   - Body: group fixes by severity (Critical/Important/Minor/Quality/Tests)
   - List each fix concisely as in previous audit commits

---

## Task 2: Data & Config Layer

**Packages:** database, config, cookies (~6,153 lines)
**Why second:** These packages sit between foundation and business logic. database/ and config/ are used by nearly everything above them. cookies/ feeds into youtube/ and twitch/.

**Files to audit (read in this order):**

*database/ (2,339 lines — SQLite persistence):*
- `internal/database/types.go` — Job struct, JobStatus enum, Gap/Trim types
- `internal/database/migrations.go` — schema creation, version tracking, v1-v6 migrations
- `internal/database/database.go` — CRUD, UpdateJobFields, pub/sub, log buffers
- `internal/database/database_test.go`

*config/ (1,065 lines — TOML configuration):*
- `internal/config/types.go` — Config struct hierarchy (Network, Paths, Logs, Monitors, etc.)
- `internal/config/flex_duration.go` — duration parsing ("10m", "7d", plain numbers)
- `internal/config/channel_terms.go` — regex/named pattern filter terms
- `internal/config/config.go` — TOML loading, legacy migration, validation, template resolution
- `internal/config/config_test.go`

*cookies/ (2,749 lines — cookie management):*
- `internal/cookies/jar.go` — Netscape parsing, SAPISIDHASH, domain filtering, header construction
- `internal/cookies/refresh.go` — cookie file reload/refresh logic
- `internal/cookies/autocookies.go` — auto-detect cookie file from browser
- `internal/cookies/job_windows.go` — Windows-specific browser cookie extraction
- `internal/cookies/job_other.go` — non-Windows stub
- `internal/cookies/jar_test.go`

**Specific things to look for:**

| Package | Key concerns |
|---------|-------------|
| database | **UpdateJobFields field map** — verify field names can't inject SQL (even though parameterized, the column names are string-concatenated into SET clauses); migration idempotency (re-running same migration); subscriber slice leak (OnJobUpdate/OnJobsChange unsubscribe correctness); transaction safety on concurrent writes; prepared statement lifecycle |
| config | Template variable injection (`${...}` in user-provided strings), TOML edge cases (empty strings, missing sections, extra keys), migration correctness for renamed fields, validation completeness (e.g., negative intervals, invalid paths), FlexDuration zero/negative handling |
| cookies | SAPISIDHASH timing (time-based hash — clock skew sensitivity), Netscape format parsing (malformed lines, extra whitespace, encoding), cookie refresh race conditions (concurrent reload + read), file locking on Windows, auto-cookie browser detection reliability, sensitive data in logs |

**Steps:**

1. Read every file listed above, taking notes on issues found per audit category
2. Fix all issues — prioritize security (SQL injection surface in UpdateJobFields, cookie leaks) and bugs
3. Add or strengthen tests for any bugs fixed
4. Run: `go build ./...` — verify clean compilation
5. Run: `go test ./internal/database/... ./internal/config/... ./internal/cookies/...` — verify all pass
6. Run: `go vet ./internal/database/... ./internal/config/... ./internal/cookies/...` — verify clean
7. Commit: `fix: backend audit phase 2 (data & config) — N fixes across M files`

---

## Task 3: Platform Services

**Packages:** cipher, bgutils, youtube, twitch, chat (~11,791 lines)
**Why third:** These packages implement YouTube/Twitch extraction. They depend on goja/, cookies/, constants/ (already audited). They're consumed by worker/ (audited next).

**Files to audit (read in this order):**

*cipher/ (2,001 lines — YouTube cipher solving):*
- `internal/cipher/types.go` — Solvers, SignatureRequest/Response, ResolveURL types
- `internal/cipher/solver.go` — 2-tier cache (solver + disk), LRU eviction, serialized compilation
- `internal/cipher/extractor.go` — Player JS preprocessing, sig/n function extraction, regex-based + full-player approaches
- `internal/cipher/decrypt.go` — DecryptSignature, GetSts public API
- `internal/cipher/player_cache.go` — disk cache with SHA256 keys, 14-day TTL, HTTP download
- `internal/cipher/sts.go` — STS extraction, per-URL cache (150 entries)
- `internal/cipher/resolve_url.go` — URL resolver applying sig + n-param decryption
- `internal/cipher/solver_test.go`
- `internal/cipher/extractor_test.go`

*bgutils/ (2,441 lines — BotGuard/PO tokens):*
- `internal/bgutils/types.go` — BgConfig, DescrambledChallenge, IntegrityTokenData, TokenMinter, BGError
- `internal/bgutils/challenge.go` — challenge parsing/descrambling from WAA API
- `internal/bgutils/botguard.go` — BotGuard client, Goja VM lifecycle, Snapshot generation
- `internal/bgutils/webpo_client.go` — web proof-of-origin client, challenge fetch + BotGuard init
- `internal/bgutils/webpo_minter.go` — token minter, snapshot → PO token conversion
- `internal/bgutils/pot_provider.go` — triple-tier cache (session → minter → inflight dedup), TTL eviction
- `internal/bgutils/cold_start.go` — startup initialization
- `internal/bgutils/pot_provider_test.go`

*youtube/ (2,076 lines — YouTube API):*
- `internal/youtube/types.go` — StreamStatus, PlayabilityError, VideoInfo, Format, YtcfgData, auth levels
- `internal/youtube/auth.go` — SAPISIDHASH header, cookie validation, API header construction
- `internal/youtube/service.go` — service facade, visitor data caching, init-once homepage
- `internal/youtube/watch_page.go` — HTML fetch/parse, ytcfg extraction, player response JSON via regex
- `internal/youtube/player_api.go` — Innertube client, multi-client strategy (TV_DOWNGRADED, WEB, WEB_CREATOR, ANDROID_VR), format dedup/merge, STS, DASH manifest decryption
- `internal/youtube/format_selector.go` — codec scoring, resolution/FPS priority, bitrate optimization
- `internal/youtube/format_selector_test.go`

*twitch/ (3,407 lines — Twitch API):*
- `internal/twitch/types.go` — TwitchStreamInfo, AccessToken, HLSVariant, VodInfo, ChatMessage, EmoteRef
- `internal/twitch/auth.go` — OAuth token management, token validation
- `internal/twitch/service.go` — service facade, job ID builder (tw_ prefix)
- `internal/twitch/api.go` — GQL handler, batched queries, GetStreamInfo, GetVodInfo, GetHLSAccessToken, VOD comments pagination
- `internal/twitch/hls.go` — M3U8 master playlist parsing, bandwidth/resolution/FPS extraction
- `internal/twitch/emotes.go` — BTTV/FFZ/7TV resolver, LRU cache (200 entries), concurrent dedup
- `internal/twitch/chat.go` — IRC chat downloader, message recording, dedup by ID, resume state
- `internal/twitch/vod_chat.go` — VOD comment fetching via GQL pagination
- `internal/twitch/chat_test.go`

*chat/ (1,866 lines — YouTube live chat):*
- `internal/chat/types.go` — ChatMessage, MessagePart, SuperchatInfo, ChatData, ChatProgress
- `internal/chat/api.go` — ChatAPI client, live chat + replay endpoints, continuation token extraction
- `internal/chat/downloader.go` — sequential/parallel collection, dedup (5000 ID cache), batched writes
- `internal/chat/downloader_test.go`

**Specific things to look for:**

| Package | Key concerns |
|---------|-------------|
| cipher | Goja VM lifecycle (are VMs properly shut down on eviction?), LRU cache concurrency (mutex coverage), extractor regex robustness (catastrophic backtracking risk?), player cache atomic write correctness, disk cache cleanup (stale entries beyond TTL), STS cache unbounded? (150 entries — is there eviction?) |
| bgutils | BotGuard VM cleanup on error paths, triple-tier cache coherence (what if session invalidated but minter cached?), minter TTL race (check-then-use), inflight dedup channel lifecycle, cold start error handling, Goja panic recovery |
| youtube | Multi-client fallback logic (does it correctly skip failing clients?), format dedup correctness (could it drop valid formats?), auth header generation timing, watch page regex brittleness, visitor data cache invalidation, DASH manifest decryption error handling |
| twitch | GQL error handling (rate limits, auth failures), HLS parsing edge cases (missing attributes, unusual playlists), IRC reconnection logic (does it handle disconnects gracefully?), emote LRU eviction correctness, chat dedup unbounded growth, VOD pagination termination |
| chat | Dedup cache growth (5000 IDs — is it bounded properly?), continuation token error handling, flush correctness (data loss on crash?), concurrent downloader coordination |

**Steps:**

1. Read every file listed above, taking notes on issues found per audit category
2. Fix all issues — this phase is likely to have the most security-relevant findings (auth, tokens, JS execution)
3. Add or strengthen tests for any bugs fixed
4. Run: `go build ./...` — verify clean compilation
5. Run: `go test ./internal/cipher/... ./internal/bgutils/... ./internal/youtube/... ./internal/twitch/... ./internal/chat/...` — verify all pass
6. Run: `go vet ./internal/cipher/... ./internal/bgutils/... ./internal/youtube/... ./internal/twitch/... ./internal/chat/...` — verify clean
7. Commit: `fix: backend audit phase 3 (platform services) — N fixes across M files`

---

## Task 4: Download Pipeline

**Packages:** engine, worker, monitor (~12,061 lines)
**Why fourth:** These are the highest-risk packages — bugs here mean lost recordings. All their dependencies (database, cipher, youtube, twitch, etc.) have been audited by now.

**Files to audit (read in this order):**

*engine/ (2,937 lines — FFmpeg + manifest parsing):*
- `internal/engine/manifest.go` — DashStream, HlsVariant, SegmentRange, segment timeline parsing
- `internal/engine/manifest_test.go`
- `internal/engine/downloader.go` — SegmentDownloader, HLS/DASH segment fetching, resume, parallel catch-up, VOD chunked download, retry logic
- `internal/engine/muxer.go` — Muxer wrapping FFmpeg/ffprobe, mux/trim/encode, inline progress parsing

*worker/ (7,437 lines — job orchestration):*
- `internal/worker/quality.go` — QualityInfo, format labels, preference parsing
- `internal/worker/time_utils.go` — time string parsing ("MM:SS", "HH:MM:SS", seconds)
- `internal/worker/time_utils_test.go`
- `internal/worker/format_utils.go` — DASH stream selection, progressive format detection
- `internal/worker/format_utils_test.go`
- `internal/worker/queue.go` — JobQueue, lifecycle/download concurrency separation
- `internal/worker/queue_test.go`
- `internal/worker/worker.go` — job lifecycle entry point
- `internal/worker/stream_processor.go` — stream status probing, wait-for-live, pre-start chat, VOD detection
- `internal/worker/strategies.go` — DownloadVod, HLS live, DASH manifest strategies, cipher/PO token integration
- `internal/worker/quality_monitor.go` — periodic quality polling, resolution/framerate change detection
- `internal/worker/progress.go` — ProgressTracker, segment counts/bytes/speed/ETA, throttled DB persistence, gap detection
- `internal/worker/orchestrator.go` — master coordinator: stream processing → download → mux → chat
- `internal/worker/mux_finalize.go` — HTTP file download utilities (VODs, thumbnails)
- `internal/worker/trim.go` — TrimService, DB records, FFmpeg trim/encode operations
- `internal/worker/orphans.go` — orphaned file scanner across staging/output/trim directories

*monitor/ (1,687 lines — stream discovery):*
- `internal/monitor/utils.go` — MetadataFailureTracker, Unicode normalization, probe failure limiting
- `internal/monitor/utils_test.go`
- `internal/monitor/feed.go` — FeedMonitor, YouTube RSS polling, dedup against DB
- `internal/monitor/decapi.go` — DecapiMonitor, DECAPI polling, rate limit respect
- `internal/monitor/twitch.go` — TwitchMonitor, GQL polling, OnStreamFound callback

**Specific things to look for:**

| Package | Key concerns |
|---------|-------------|
| engine | **FFmpeg process lifecycle** (zombie processes on context cancel, stdout/stderr draining), segment retry logic (does retry 5 actually work? backoff?), parallel download coordination (goroutine leak if one segment panics), manifest parsing edge cases (empty timelines, overlapping segments, missing attributes), VOD chunked download resume correctness, muxer progress regex parsing robustness |
| worker | **Orchestrator goroutine management** (are all spawned goroutines tracked and cleaned up?), queue concurrency limits (race between lifecycle and download slots), progress tracker accuracy (does throttled persistence lose final state?), quality monitor races (polling while download transitions), orphan scanner safety (TOCTOU — file deleted between scan and report), stream processor wait-for-live timeout handling, strategy selection correctness (wrong strategy = corrupt file), mux_finalize HTTP download error handling (partial downloads, disk full) |
| monitor | RSS/DECAPI polling interval drift (timer vs ticker), failure tracker map growth (is it bounded?), callback error propagation (does a panicking callback kill the monitor?), Twitch GQL rate limiting, feed dedup correctness (false positives/negatives), Unicode normalization consistency |

**Steps:**

1. Read every file listed above, taking notes on issues found per audit category
2. Fix all issues — prioritize bugs that could cause lost recordings or zombie processes
3. Add or strengthen tests for any bugs fixed
4. Run: `go build ./...` — verify clean compilation
5. Run: `go test ./internal/engine/... ./internal/worker/... ./internal/monitor/...` — verify all pass
6. Run: `go vet ./internal/engine/... ./internal/worker/... ./internal/monitor/...` — verify clean
7. Commit: `fix: backend audit phase 4 (download pipeline) — N fixes across M files`

---

## Task 5: Web Server Backend

**Packages:** web (server + routes), notifications, updater (~8,489 lines)
**Why fifth:** HTTP-facing code with auth, sessions, and rate limiting. The frontend was already audited; this covers the Go handlers serving it.

**Files to audit (read in this order):**

*web/ core (2,752 lines — server infrastructure):*
- `internal/web/server.go` — HTTP server setup, middleware stack, auth/TUI token management
- `internal/web/middleware.go` — CORS, security headers, CSRF, IP gating, max body, compression
- `internal/web/auth.go` — scrypt password hashing, session management, token validation, cleanup goroutine
- `internal/web/websocket.go` — WebSocket hub, throttling (leading+trailing edge), broadcasting, log buffer, auth
- `internal/web/rate_limiter.go` — per-IP sliding window, cleanup loop, retry-after
- `internal/web/tls.go` — TLS cert loading/validation
- `internal/web/auth_test.go`
- `internal/web/middleware_test.go`
- `internal/web/rate_limiter_test.go`

*web/routes/ (4,584 lines — HTTP handlers):*
- `internal/web/routes/auth.go` — login/logout, session validation, client token persistence
- `internal/web/routes/jobs.go` — job CRUD, pagination, filtering, WebSocket updates
- `internal/web/routes/cookies.go` — cookie refresh status, auto-cookie, platform tracking
- `internal/web/routes/stats.go` — disk usage, storage stats, activity metrics
- `internal/web/routes/files.go` — orphaned file scanning and deletion
- `internal/web/routes/ffmpeg.go` — FFmpeg validation, download/install, caching
- `internal/web/routes/ffmpeg_elevation_windows.go` — Windows privilege elevation
- `internal/web/routes/ytdlp.go` — yt-dlp format queries, PO token generation
- `internal/web/routes/update.go` — update check/apply/verify/dismiss

*notifications/ (527 lines — Discord webhooks):*
- `internal/notifications/manager.go` — dispatcher, webhook URL parsing, event filtering, async broadcast
- `internal/notifications/discord.go` — Discord embed formatting, HTTP posting
- `internal/notifications/manager_test.go`

*updater/ (626 lines — auto-update):*
- `internal/updater/updater.go` — GitHub Releases polling, binary download, rename strategy, sig verification
- `internal/updater/signing.go` — Ed25519 verification/generation, embedded public key
- `internal/updater/semver.go` — version parsing, comparison
- `internal/updater/signing_test.go`
- `internal/updater/semver_test.go`

**Specific things to look for:**

| Package | Key concerns |
|---------|-------------|
| web core | **Auth middleware chain** (can any route bypass auth?), CSRF validation completeness (all state-changing endpoints covered?), WebSocket connection lifecycle (leak on abnormal close?), rate limiter cleanup goroutine (does it actually run? interval correct?), session cleanup (expired sessions removed?), scrypt parameters (cost factor adequate?), security headers completeness, CORS origin validation strictness, IP gating bypass risk (X-Forwarded-For spoofing) |
| web/routes | **Input validation on all endpoints** (missing bounds checks on pagination, unvalidated IDs, path traversal in file operations), jobs.go response size (unbounded list returns?), file deletion safety (can it delete outside staging/output?), FFmpeg elevation security (privilege escalation risk), ytdlp route injection (user-provided URLs passed to shell?), update apply atomicity (partial apply = broken binary?), cookie route sensitive data exposure |
| notifications | Webhook URL validation (SSRF risk — can user-provided URL hit internal services?), async goroutine lifecycle (WaitGroup correctness), Discord API error handling, embed field length limits |
| updater | **Signature verification correctness** (can it be bypassed?), rename race conditions (crash between rename steps = no binary?), download timeout handling (partial file cleanup), semver edge cases (pre-release, build metadata), GitHub API rate limiting |

**Steps:**

1. Read every file listed above, taking notes on issues found per audit category
2. Fix all issues — prioritize auth/security (this is the internet-facing layer)
3. Add or strengthen tests for any bugs fixed
4. Run: `go build ./...` — verify clean compilation
5. Run: `go test ./internal/web/... ./internal/notifications/... ./internal/updater/...` — verify all pass
6. Run: `go vet ./internal/web/... ./internal/notifications/... ./internal/updater/...` — verify clean
7. Commit: `fix: backend audit phase 5 (web server backend) — N fixes across M files`

---

## Task 6: Entry Point & Utilities

**Packages:** cmd/moombox/main.go, utils (~4,103 lines)
**Why last:** main.go wires everything together — auditing it after all internal packages means we understand what it's coordinating. utils/ is cross-cutting and best reviewed with full project context.

**Files to audit (read in this order):**

*utils/ (2,029 lines — shared utilities):*
- `internal/utils/http.go` — FetchWithTimeout, FetchWithRetry, FetchBody, DrainBody, retry logic
- `internal/utils/http_test.go`
- `internal/utils/ip.go` — isPrivateIP, isLoopback (RFC 1918/loopback)
- `internal/utils/ip_test.go`
- `internal/utils/youtube.go` — video ID extraction/validation, channel URL parsing
- `internal/utils/youtube_test.go`
- `internal/utils/twitch.go` — target type detection, URL parsing
- `internal/utils/twitch_test.go`
- `internal/utils/sanitize.go` — SanitizeForFilename (invalid chars, Unicode CJK/Japanese)
- `internal/utils/sanitize_test.go`
- `internal/utils/media.go` — ExtractMediaID (YT/Twitch/URL detection)
- `internal/utils/media_test.go`
- `internal/utils/text.go` — NormalizeText, FuzzyMatch
- `internal/utils/text_test.go`
- `internal/utils/format.go` — FormatFileSize, FormatDurationHuman
- `internal/utils/time_format.go` — FormatDuration, FormatBytes, FormatSpeed, FormatETA
- `internal/utils/time_format_test.go`
- `internal/utils/smooth.go` — SmoothValue (EMA)
- `internal/utils/smooth_test.go`
- `internal/utils/async.go` — Sleep (context-aware), Jitter, SleepWithJitter
- `internal/utils/channel.go` — ResolveChannelInput
- `internal/utils/ffprobe.go` — ExtractVideoMetadata via ffprobe

*cmd/moombox/ (2,074 lines — application entry point):*
- `cmd/moombox/main.go` — flag parsing, logger/DB/config init, TUI/Web startup, signal handling, monitor services, worker queue, WebSocket state, HTTP server, disk monitoring, auto-update, graceful shutdown

**Specific things to look for:**

| Package | Key concerns |
|---------|-------------|
| utils | **HTTP retry logic** (exponential backoff correctness, response body leak on retry, context cancellation respected?), IP validation edge cases (IPv6, mapped IPv4, link-local), sanitize completeness (all Windows-invalid chars? reserved names like CON/PRN/NUL?), video ID regex edge cases, FuzzyMatch false positives, format overflow (very large byte counts, negative durations), EMA smoothing with zero/negative values, async Sleep context leak |
| main.go | **Startup ordering** (can a service start before its dependency is ready?), signal handling completeness (double SIGINT?), graceful shutdown (are all goroutines stopped? DB closed? file handles released?), goroutine lifecycle (every goroutine has panic recovery?), config reload safety (concurrent access during reload?), error propagation from subsystem init failures, exit code semantics (42 = restart), resource cleanup on early exit |

**Steps:**

1. Read every file listed above, taking notes on issues found per audit category
2. Fix all issues
3. Add or strengthen tests for any bugs fixed
4. Run: `go build ./...` — verify clean compilation
5. Run: `go test ./internal/utils/... ./cmd/moombox/...` — verify all pass (if main has tests)
6. Run: `go vet ./internal/utils/... ./cmd/moombox/...` — verify clean
7. Commit: `fix: backend audit phase 6 (entry point & utilities) — N fixes across M files`

---

## Execution Notes

**Build verification between phases:**
After each task's commit, run `go build -o /dev/null ./cmd/moombox` to confirm the full binary still compiles.

**Cross-phase issues:**
If auditing a later phase reveals an issue in an already-committed phase, fix it in the current phase's commit — don't go back and amend. Note in the commit body: "Also fixes [package] issue discovered during [current phase] audit."

**Issue severity tiers (for commit body organization):**
- **Critical** — security vulnerabilities, data loss risks, crash bugs
- **Important** — logic bugs, resource leaks, race conditions
- **Minor** — edge case handling, defensive checks, error messages
- **Quality** — dedup, dead code removal, naming, consistency
- **Tests** — new or strengthened tests

**Commit message format:**
```
fix: backend audit phase N (group name) — X fixes across Y files

Critical: [description]. Important: [description]. Minor: [description].
Quality: [description]. Tests: [description].

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>
```
