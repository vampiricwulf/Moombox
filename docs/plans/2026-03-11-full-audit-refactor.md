# Full Code Audit & Refactor — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fresh deep audit of the entire Moombox codebase (~59K Go lines + ~245KB frontend) with structural refactoring at natural seams, inline test expansion, and one commit per phase.

**Architecture:** Bottom-up through the dependency graph (same proven order as previous audit). Each phase: read every file → audit against 9 categories → fix issues → refactor large files at natural seams → add/strengthen tests → verify build/test/vet → commit. Refactoring splits files only at clear domain boundaries, never at arbitrary line limits.

**Tech Stack:** Go 1.25, modernc/sqlite, chi/v5, goja, nhooyr.io/websocket, BurntSushi/toml, Charmbracelet (bubbletea/bubbles/huh/lipgloss), Shoelace v2.16

**Previous audit reference:** Commits `0703a62` through `f82274f` (6-phase backend audit, ~77 fixes). This plan re-audits everything from scratch and adds structural refactoring.

---

## Audit Checklist (applied to every file in every task)

9 categories:

1. **Security** — injection, auth bypass, token leaks, path traversal, unvalidated input, TOCTOU
2. **Bugs** — logic errors, race conditions, resource leaks, nil derefs, off-by-ones, overflow
3. **Error handling** — swallowed errors, missing cleanup on error paths, panic without recovery, inconsistent wrapping
4. **Dead code** — unused functions/methods/fields/constants, unreachable branches, stale comments
5. **Deduplication** — repeated patterns that should be extracted into shared helpers
6. **Anti-patterns** — goroutine leaks, mutex misuse, context.Background() misuse, unbounded growth
7. **Consistency** — naming, error message style, log levels, parameter ordering, comment style
8. **Tests** — add tests for bugs found, strengthen weak tests, verify edge cases
9. **Structural** — natural seams for file splitting, overly complex functions, readability improvements

---

## Refactoring Rules

- Split files only at clear domain boundaries, never at arbitrary line limits
- Extract helpers only when there's genuine reuse or a function does multiple distinct things
- Don't break CLAUDE.md patterns (anonymous logger interface, UpdateJobFields, chord system, etc.)
- Each refactoring must maintain all existing tests passing
- Prefer moving code to new files within the same package over creating new packages
- Target: no file over ~800 lines without clear justification after refactoring
- Move tests for split code into correspondingly-named test files

---

## Task 1: Foundation Layer

**Packages:** goja, errors, constants, disk, logger (~3,125 lines)
**Why first:** Leaf packages with zero internal dependencies. Everything else builds on them.

**Files to audit:**

*goja/ (1,321 lines):*
- `internal/goja/runtime.go` (55 lines) — VM factory, field mapper
- `internal/goja/dom_shim.go` (343 lines) — window/document/navigator stubs
- `internal/goja/encoding.go` (128 lines) — btoa/atob, TextEncoder/TextDecoder
- `internal/goja/timer.go` (228 lines) — setTimeout/setInterval, TimerManager
- `internal/goja/runtime_test.go` (326 lines)
- `internal/goja/timer_test.go` (282 lines)

*errors/ (785 lines):*
- `internal/errors/errors.go` (230 lines) — MoomboxError, typed errors, helpers
- `internal/errors/errors_test.go` (555 lines)

*constants/ (393 lines):*
- `internal/constants/constants.go` — User-Agents, API endpoints, limits

*disk/ (58 lines):*
- `internal/disk/disk_windows.go` — kernel32 GetDiskFreeSpaceExW

*logger/ (607 lines):*
- `internal/logger/logger.go` (452 lines) — slog wrapper, rotation, ring buffer, pub/sub
- `internal/logger/logger_test.go` (155 lines)

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| timer.go | **RACE CONDITION**: SetInterval/SetTimeout callbacks invoke Goja VM functions from Go goroutines. Goja is single-threaded — this corrupts VM state if BotGuard code modifies variables concurrently | Critical |
| timer.go | SetInterval with 0ms delay creates tight goroutine loop (clamped to 1ms but still aggressive) | Medium |
| timer.go | CancelAll doesn't prevent new timers created between stopped=true and channel close | Low |
| dom_shim.go | Large 292-line embedded JS string — unmaintainable | Structural |
| dom_shim.go | Temporary globals (`__moomboxUserAgent`, `__cryptoRandBytes`) set to Undefined after use but may still be referenced | Low |
| dom_shim.go | No error handling on shimCode execution — silently ignores JS syntax errors | Medium |
| encoding.go | Large embedded JS strings for TextEncoder/TextDecoder | Structural |
| errors.go | IsLoginRequired() hardcodes YouTubeError and AuthError types — won't catch new error types | Low |
| errors.go | Context map only used by VideoPlayabilityError — over-generalization | Low |
| constants.go | Mixed var and const — mutable slices (ThumbnailQualities) could be accidentally modified | Medium |
| constants.go | YouTube client versions hardcoded without update-date comments | Low |
| logger.go | Job log buffer pruning is O(n) — copies 100 lines each time; hardcoded value doesn't match maxJobLogLines (500) | Medium |
| logger.go | Broadcast drops slow subscribers silently — no warning logged | Low |
| logger.go | Unsubscribe doesn't close the channel — leaves it open forever | Low |
| logger.go | SetLevel silently falls back to Info on invalid level — no warning | Low |

**Refactoring targets:**
- None for splitting (all files are appropriately sized)
- Fix timer race condition (use channel-based callback queuing or Interrupt pattern)
- Consider extracting large JS strings to const blocks with clear names

**Test gaps to fill:**
- Timer concurrency tests (concurrent SetTimeout/ClearTimer)
- DOM element manipulation tests
- Console capture tests
- Logger file rotation tests
- Logger concurrent logging tests
- Logger unsubscribe race condition tests

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Fix all issues — prioritize the timer.go race condition (Critical)
3. Add or strengthen tests for bugs fixed and coverage gaps
4. Run: `go build ./...`
5. Run: `go test ./internal/goja/... ./internal/errors/... ./internal/logger/...`
6. Run: `go vet ./internal/goja/... ./internal/errors/... ./internal/constants/... ./internal/disk/... ./internal/logger/...`
7. Commit: `fix: audit & refactor phase 1 (foundation) — N fixes, Y refactors across Z files`

---

## Task 2: Data & Config Layer

**Packages:** database, config, cookies (~6,153 lines)
**Why second:** Between foundation and business logic. Used by nearly everything above.

**Files to audit:**

*database/ (2,390 lines):*
- `internal/database/types.go` (158 lines) — Job, JobStatus, Gap, Segment, TrimRecord
- `internal/database/migrations.go` (313 lines) — schema creation, v1-v6 migrations
- `internal/database/database.go` (1,401 lines) — CRUD, UpdateJobFields, pub/sub, batch updates
- `internal/database/database_test.go` (521 lines)

*config/ (1,065 lines):*
- `internal/config/types.go` (136 lines) — Config struct hierarchy
- `internal/config/flex_duration.go` (153 lines) — FlexDuration parsing
- `internal/config/channel_terms.go` (58 lines) — regex/named pattern filters
- `internal/config/config.go` (475 lines) — TOML loading, migration, validation, templates
- `internal/config/config_test.go` (247 lines)

*cookies/ (2,763 lines):*
- `internal/cookies/jar.go` (251 lines) — Netscape parsing, SAPISIDHASH, headers
- `internal/cookies/refresh.go` (609 lines) — cookie reload/refresh, YouTube/Twitch auth check
- `internal/cookies/autocookies.go` (1,648 lines) — browser detection, cookie extraction
- `internal/cookies/job_windows.go` (116 lines) — Windows Job Object for browser cleanup
- `internal/cookies/job_other.go` (13 lines) — non-Windows stub
- `internal/cookies/jar_test.go` (130 lines)

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| database.go | `updateJobDirect()` and `updateJobInTx()` are IDENTICAL code — should be single function accepting executor | Important |
| database.go | Batch channel overflow (cap 100): fallback to sync update + subscriber notification can cause duplicate notifications | Medium |
| database.go | Subscriber slice nil-ing doesn't reclaim memory — unbounded growth on register/unregister | Low |
| database.go | `GetAllJobs()` uses unprepared query despite being called frequently | Low |
| migrations.go | Segments table defined TWICE — once in createSchema and again in migration v5 | Medium |
| migrations.go | Column name interpolation via string concatenation (safe since hardcoded, but anti-pattern) | Low |
| migrations.go | Migration v4 index doesn't fully optimize HasActiveJob() query (missing status column) | Low |
| config.go | Migration logic incomplete — if user has partial new section, other fields in old format are skipped | Medium |
| config.go | No atomic reads — reads config twice (TOML decoder + raw map), disk changes between could be inconsistent | Low |
| config.go | GetActivePlatforms() doesn't log which precedence level was used | Low |
| flex_duration.go | No validation for negative or zero values | Medium |
| jar.go | Assumes 7-tab-delimited fields — no validation for missing parts | Medium |
| jar.go | Header injection protection only strips \r and \n | Low |
| refresh.go | Hardcoded YouTube client version `"2.20260101.00.00"` — will break when YouTube updates | Medium |
| refresh.go | Set-Cookie parsing edge cases: empty value, missing date format, negative max-age | Medium |
| refresh.go | Race condition: updates prevYouTubeAuth after releasing lock, callback might read stale state | Medium |
| autocookies.go | **1,648 lines** — larger than entire database.go, unmaintainably large | Structural |
| autocookies.go | CDP WebSocket polling: hardcoded 500ms interval, 15s timeout, no backoff | Medium |
| autocookies.go | Cookie merge could corrupt state on partial failure — should use temp file + rename | Medium |
| autocookies.go | No timeout on browser launch — could hang forever | Medium |
| autocookies.go | Thread safety issues: setupProcess/refreshCmd pointers modified under different locks | Medium |
| job_windows.go | Handle leak if SetInformationJobObject fails after CreateJobObject succeeds | Low |

**Refactoring targets:**

*database.go (1,401 lines) → split into:*
- `database.go` (~500 lines) — Database struct, Open/Close, core helpers
- `database_jobs.go` (~400 lines) — Job CRUD (AddJob, GetJob, GetAllJobs, UpdateJob, DeleteJob)
- `database_subscribers.go` (~200 lines) — OnJobUpdate, OnJobsChange, pub/sub
- `database_batch.go` (~150 lines) — batchUpdateLoop, flushUpdates
- `database_extras.go` (~150 lines) — History, LastVideos, ClientToken, ImportFromJSON, Stats

*autocookies.go (1,648 lines) → split into:*
- `autocookies.go` (~300 lines) — AutoCookieService struct, lifecycle, orchestration
- `autocookies_detect.go` (~200 lines) — DetectBrowser, browser path lookup
- `autocookies_firefox.go` (~400 lines) — Firefox-specific logic (profile, cookies.sqlite)
- `autocookies_chromium.go` (~500 lines) — Chromium-specific logic (CDP, WebSocket)
- `autocookies_merge.go` (~150 lines) — Cookie merging logic

**Test gaps to fill:**
- database: UpdateJob async path, batch coalescing, segment operations, ClientToken, concurrent stress
- config: Save(), path search, GetActivePlatforms(), invalid TOML, partial migrations
- cookies: Reload(), concurrent access, malformed Netscape format, large files

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Refactor database.go into 5 files (split at natural domain boundaries)
3. Refactor autocookies.go into 5 files (split by browser/concern)
4. Fix all issues found — deduplicate updateJobDirect/updateJobInTx first
5. Add or strengthen tests
6. Run: `go build ./...`
7. Run: `go test ./internal/database/... ./internal/config/... ./internal/cookies/...`
8. Run: `go vet ./internal/database/... ./internal/config/... ./internal/cookies/...`
9. Commit: `fix: audit & refactor phase 2 (data & config) — N fixes, Y refactors across Z files`

---

## Task 3: YouTube Platform Services

**Packages:** cipher, bgutils, youtube (~6,524 lines)
**Why third:** These implement YouTube extraction. Depend on goja/, cookies/, constants/ (already audited). Consumed by worker/ (audited later).

**Files to audit:**

*cipher/ (2,001 lines):*
- `internal/cipher/types.go` (69 lines) — Solvers, request/response types
- `internal/cipher/solver.go` (221 lines) — 2-tier cache (disk + compiled), LRU, serialized compilation
- `internal/cipher/extractor.go` (813 lines) — player JS preprocessing, sig/n extraction
- `internal/cipher/decrypt.go` (50 lines) — DecryptSignature, GetSts API
- `internal/cipher/player_cache.go` (189 lines) — disk cache with 14-day TTL
- `internal/cipher/sts.go` (74 lines) — STS extraction, per-URL cache (150 entries)
- `internal/cipher/resolve_url.go` (66 lines) — URL resolver
- `internal/cipher/solver_test.go` (430 lines)
- `internal/cipher/extractor_test.go` (430 lines)

*bgutils/ (2,447 lines):*
- `internal/bgutils/types.go` (108 lines) — BgConfig, TokenMinter, BGError
- `internal/bgutils/challenge.go` (214 lines) — WAA API challenge fetch/descramble
- `internal/bgutils/botguard.go` (291 lines) — BotGuard client, Goja VM lifecycle
- `internal/bgutils/webpo_client.go` (274 lines) — challenge → BotGuard → GenerateIT flow
- `internal/bgutils/webpo_minter.go` (126 lines) — token minting
- `internal/bgutils/pot_provider.go` (295 lines) — triple-tier cache
- `internal/bgutils/cold_start.go` (60 lines) — fallback PO token
- `internal/bgutils/pot_provider_test.go` (1,087 lines)

*youtube/ (2,078 lines):*
- `internal/youtube/types.go` (132 lines) — StreamStatus, VideoInfo, Format, YtcfgData
- `internal/youtube/auth.go` (105 lines) — SAPISIDHASH, cookie validation
- `internal/youtube/service.go` (206 lines) — service facade
- `internal/youtube/watch_page.go` (144 lines) — HTML fetch, ytcfg extraction
- `internal/youtube/player_api.go` (1,084 lines) — multi-client strategy, format parsing
- `internal/youtube/format_selector.go` (300 lines) — codec scoring, format selection
- `internal/youtube/format_selector_test.go` (105 lines)

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| extractor.go | **813 lines** with two competing strategies (full player vs legacy regex) mixed together | Structural |
| extractor.go | findAlrTransformChain() uses manual parenthesis depth tracking — fragile for edge cases | Medium |
| extractor.go | Large embedded JS setup code constants (fullPlayerSetupCode ~368 lines, setupCode ~786 lines) | Structural |
| player_cache.go | `http.DefaultClient.Do()` uses no timeout — inherits global client with no explicit timeout | Medium |
| player_cache.go | `Fetch()` downloads without size limit — uses io.ReadAll with implicit limits only | Low |
| sts.go | Random eviction instead of LRU — could lose recent STS values | Medium |
| challenge.go | Magic array indices [0,1,2,3,4,5,7] for challenge parsing — fragile if API changes | Medium |
| botguard.go | 30-second timeout for interpreter load is hardcoded — no way to tune | Low |
| pot_provider.go | Minter eviction scheduled per-request via time.AfterFunc — could schedule hundreds of timers | Medium |
| pot_provider.go | cleanupExpired() only called on GeneratePoToken entry — stale entries accumulate | Low |
| webpo_minter.go | Base64 decoding tries 3 strategies (standard, URL-safe, raw) — overly permissive | Low |
| cold_start.go | Max identifier 118 bytes is arbitrary with no comment explaining limit | Low |
| cold_start.go | XOR key generation falls back to index 0 on crypto/rand error | Low |
| player_api.go | **1,084 lines** — GetVideoInfoAuthenticated is 230 lines of nested conditionals | Structural |
| player_api.go | classifyStream() has complex boolean logic (lines 854-904) | Medium |
| player_api.go | parseFormatsWithCipher → decryptNParam → GetSolvers: potential double lookup | Low |
| player_api.go | No unit tests — only integration tests | Test gap |
| format_selector_test.go | No tests for auth level tiebreaker, empty formats, all-invalid formats | Test gap |

**Refactoring targets:**

*extractor.go (813 lines) → split into:*
- `extractor.go` (~200 lines) — shared types, public API, setup code constants
- `extractor_full_player.go` (~300 lines) — modern full-player approach (preprocessPlayerFull, findAlrTransformChain, findNArrayCandidates, findSigCandidates)
- `extractor_legacy.go` (~300 lines) — legacy regex-based extraction (preprocessPlayerLegacy, extractSigFunction, extractNFunction, extractFunctionByName, extractObjectByName)

*player_api.go (1,084 lines) → split into:*
- `player_api.go` (~400 lines) — PlayerAPI struct, public methods, client config
- `player_api_strategy.go` (~350 lines) — GetVideoInfoAuthenticated/Public, multi-client fallback logic
- `player_api_parsing.go` (~350 lines) — parsePlayerResponse, format parsing, classifyStream, helper functions

**Test gaps to fill:**
- player_api.go: unit tests for parsePlayerResponse, classifyStream, format dedup
- format_selector: auth level tiebreaker, edge cases
- STS cache eviction behavior
- Challenge parsing with malformed responses

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Refactor extractor.go into 3 files
3. Refactor player_api.go into 3 files
4. Fix all issues found
5. Add or strengthen tests
6. Run: `go build ./...`
7. Run: `go test ./internal/cipher/... ./internal/bgutils/... ./internal/youtube/...`
8. Run: `go vet ./internal/cipher/... ./internal/bgutils/... ./internal/youtube/...`
9. Commit: `fix: audit & refactor phase 3 (YouTube services) — N fixes, Y refactors across Z files`

---

## Task 4: Twitch Platform Services

**Packages:** twitch, chat (~5,293 lines)
**Why fourth:** Twitch extraction and chat. Depends on cookies/, constants/ (audited). Consumed by worker/.

**Files to audit:**

*twitch/ (3,428 lines):*
- `internal/twitch/types.go` (130 lines) — TwitchStreamInfo, VodInfo, ChatMessage, EmoteData
- `internal/twitch/auth.go` (~105 lines) — OAuth token management
- `internal/twitch/service.go` (117 lines) — service facade
- `internal/twitch/api.go` (~700 lines) — GQL handler, batched queries, stream/VOD info
- `internal/twitch/hls.go` (~200 lines) — M3U8 playlist parsing
- `internal/twitch/emotes.go` (~400 lines) — BTTV/FFZ/7TV resolver, LRU cache
- `internal/twitch/chat.go` (1,031 lines) — IRC chat downloader, message recording
- `internal/twitch/vod_chat.go` (~300 lines) — VOD comment fetching
- `internal/twitch/chat_test.go` (228 lines)

*chat/ (1,865 lines):*
- `internal/chat/types.go` (74 lines) — ChatMessage, SuperchatInfo, ChatProgress
- `internal/chat/api.go` (~700 lines) — ChatAPI, live/replay endpoints, continuation tokens
- `internal/chat/downloader.go` (~850 lines) — sequential/parallel collection, dedup, batched writes
- `internal/chat/downloader_test.go` (228 lines)

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| hls.go | Silent error ignoring: ParseInt errors for bandwidth, resolution, FPS | Medium |
| hls.go | Missing attributes in variant parsing edge cases | Low |
| chat.go | 1,031 lines — IRC reconnection logic, dedup tracking, message recording | Structural |
| chat.go | Dedup via seenIDs map keeps last 5,000 — growth pattern unclear | Medium |
| emotes.go | LRU cache 200 entries — eviction correctness untested | Low |
| vod_chat.go | Pagination termination — edge cases on empty or final pages | Low |
| api.go (twitch) | GQL error handling — rate limits, auth failures | Medium |
| api.go (chat) | ParseInt error on line 290 silently ignored | Medium |
| downloader.go (chat) | Dedup cache at 5,000 IDs — properly bounded? | Medium |
| downloader.go (chat) | Continuation token error handling — what happens on malformed token? | Medium |

**Refactoring targets:**

*twitch/chat.go (1,031 lines) → split into:*
- `chat.go` (~400 lines) — ChatDownloader struct, lifecycle, main loop
- `chat_irc.go` (~350 lines) — IRC connection, parsing, message handling
- `chat_recording.go` (~280 lines) — file writing, dedup, batched persistence

**Test gaps to fill:**
- HLS parsing: edge cases (missing attributes, unusual playlists)
- Emote LRU: eviction behavior under cache pressure
- IRC reconnection: disconnects, partial messages
- Chat dedup: boundary conditions at 5,000 entries
- VOD pagination: empty pages, malformed continuation tokens

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Refactor twitch/chat.go into 3 files
3. Fix all issues found — prioritize silent error ignoring in hls.go
4. Add or strengthen tests
5. Run: `go build ./...`
6. Run: `go test ./internal/twitch/... ./internal/chat/...`
7. Run: `go vet ./internal/twitch/... ./internal/chat/...`
8. Commit: `fix: audit & refactor phase 4 (Twitch services) — N fixes, Y refactors across Z files`

---

## Task 5: Download Engine & Monitoring

**Packages:** engine, monitor (~4,624 lines)
**Why fifth:** Engine handles segment downloads and FFmpeg. Monitor discovers streams. Their dependencies are now audited.

**Files to audit:**

*engine/ (2,972 lines):*
- `internal/engine/manifest.go` (657 lines) — DASH/HLS manifest parsing
- `internal/engine/manifest_test.go` (212 lines)
- `internal/engine/downloader.go` (1,430 lines) — segment download orchestration
- `internal/engine/muxer.go` (673 lines) — FFmpeg wrapper

*monitor/ (1,705 lines):*
- `internal/monitor/utils.go` (330 lines) — MetadataFailureTracker, Unicode normalization
- `internal/monitor/utils_test.go` (232 lines)
- `internal/monitor/feed.go` (409 lines) — YouTube RSS feed monitor
- `internal/monitor/decapi.go` (456 lines) — DECAPI monitor
- `internal/monitor/twitch.go` (278 lines) — Twitch GQL monitor

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| downloader.go | **1,430 lines, no tests** — critical test coverage gap for core download logic | Critical |
| downloader.go | `runDashLoop()` is 240+ lines with 6+ nesting levels and multiple state machines mixed together | Structural |
| downloader.go | `streamEnded` (bool) accessed from main loop + defer without atomic — race condition | Important |
| downloader.go | Many hardcoded delays (500ms, 1s, 2s, 5s, 10m, 60s) with no explanation | Medium |
| downloader.go | Resume file safety: temp file not cleared on crash | Medium |
| downloader.go | 30-segment stay-behind on catch-up undocumented | Low |
| muxer.go | **673 lines, no tests** | Important |
| muxer.go | Hardcoded encoder settings (libx264 preset "slow"/"fast") — not configurable | Low |
| muxer.go | Concat demuxer path escaping fragile on Windows | Medium |
| muxer.go | No ffprobe availability check — derives from ffmpeg path without verification | Low |
| muxer.go | Two-pass encode may not handle audio codec correctly (pass 2 only copies, doesn't re-encode) | Medium |
| manifest.go | Silent XML parse failures — returns nil on errors in some paths | Medium |
| manifest.go | Hard-coded defaultSegmentDuration = 2.0 — not configurable | Low |
| manifest.go | Magic safety limit of 100,000 on expanded segments | Low |
| feed.go | No jitter on feed poll interval — thundering herd if multiple monitors start | Medium |
| feed.go | 5 consecutive errors trigger channel pause — unclear recovery mechanism | Low |
| decapi.go | Rate limit state fragile — resets remaining on each request, no time-based backoff | Medium |
| decapi.go | Regex for video ID extraction doesn't handle playlist URLs | Low |
| twitch.go | Hardcoded 15s poll + 500ms stagger — no jitter | Low |
| utils.go | Max failures = 3, map eviction at 500 entries — hardcoded | Low |
| utils.go | Random eviction when map full — could prioritize oldest/most-failed | Low |

**Refactoring targets:**

*downloader.go (1,430 lines) → split into:*
- `downloader.go` (~350 lines) — SegmentDownloader struct, Start/Cancel, core helpers
- `downloader_dash.go` (~300 lines) — runDashLoop() with its retry/status tracking
- `downloader_hls.go` (~250 lines) — runHlsLoop(), runHlsVodParallel()
- `downloader_direct.go` (~200 lines) — runDirectDownload(), chunk logic
- `downloader_parallel.go` (~200 lines) — runParallelCatchUp(), out-of-order buffering
- `downloader_resume.go` (~130 lines) — loadResume(), saveResume(), ClearResume()

*muxer.go (673 lines) → split into:*
- `muxer.go` (~300 lines) — Muxer struct, Mux/MuxCopy/MuxEncode, runFFmpeg
- `muxer_trim.go` (~200 lines) — Trim(), TrimWithAudio(), TrimAndConcat()
- `muxer_progress.go` (~100 lines) — runFFmpegWithProgress(), parseFFmpegTime(), cappedBuffer
- `muxer_twopass.go` (~80 lines) — runTwoPassEncode()

**Test gaps to fill:**
- downloader: DASH live streaming state transitions, HLS refresh edge cases, resume corruption, parallel catch-up errors
- muxer: scale/fps filter, two-pass encoding, concat with missing segments, progress parsing
- monitor: feed jitter behavior, failure tracker eviction, concurrent polling

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Refactor downloader.go into 6 files
3. Refactor muxer.go into 4 files
4. Fix all issues — prioritize the streamEnded race condition and silent parse failures
5. Add or strengthen tests
6. Run: `go build ./...`
7. Run: `go test ./internal/engine/... ./internal/monitor/...`
8. Run: `go vet ./internal/engine/... ./internal/monitor/...`
9. Commit: `fix: audit & refactor phase 5 (engine & monitoring) — N fixes, Y refactors across Z files`

---

## Task 6: Download Pipeline (Worker)

**Packages:** worker (~7,465 lines, 16 files)
**Why sixth:** Highest-risk package — bugs here mean lost recordings. All dependencies now audited. Biggest refactoring target.

**Files to audit:**

- `internal/worker/quality.go` (72 lines) — QualityInfo, format labels
- `internal/worker/time_utils.go` (82 lines) + `time_utils_test.go` (59 lines)
- `internal/worker/format_utils.go` (164 lines) + `format_utils_test.go` (357 lines)
- `internal/worker/queue.go` (300 lines) + `queue_test.go` (579 lines)
- `internal/worker/worker.go` (595 lines) — job lifecycle entry point
- `internal/worker/stream_processor.go` (1,046 lines) — stream probing, wait-for-live
- `internal/worker/strategies.go` (703 lines) — download strategy dispatch
- `internal/worker/quality_monitor.go` (80 lines) — periodic quality probing
- `internal/worker/progress.go` (~170 lines) — ProgressTracker
- `internal/worker/orchestrator.go` (2,055 lines) — master coordinator
- `internal/worker/mux_finalize.go` (108 lines) — HTTP file download utilities
- `internal/worker/trim.go` (569 lines) — TrimService
- `internal/worker/orphans.go` (401 lines) — orphaned file scanner

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| orchestrator.go | **2,055 lines, no tests** — `ExecuteWithChat()` is 500+ lines with deeply nested sections | Critical/Structural |
| orchestrator.go | Chat wait timeout (2 min) and stream end verify interval (5 min) hardcoded | Low |
| orchestrator.go | Twitch integration (`ExecuteTwitch()`) duplicates logic from YouTube path | Medium |
| stream_processor.go | **1,046 lines, no tests** — complex state machine for probing | Important/Structural |
| stream_processor.go | `activeChats` slice modified by Start() and chat error handler without full sync | Medium |
| stream_processor.go | Hardcoded probe intervals: chat surge 15s, jitter 30s | Low |
| strategies.go | **703 lines, no tests** — download strategy dispatch | Important |
| strategies.go | Manual format override iterates format list searching by itag | Low |
| worker.go | notifyJob channel only wakes heartbeat — could be cleaner | Low |
| worker.go | HeartbeatInterval (60s) hardcoded | Low |
| queue.go | Backlog limit 100 with warn-only — jobs silently discarded | Low |
| quality_monitor.go | Non-blocking send drops change notifications if consumer slow | Low |
| progress.go | Smooth speed factor 0.7 is magic number | Low |
| trim.go | Segment selection logic undocumented | Low |
| orphans.go | No tests — cleanup heuristics undocumented | Low |

**Refactoring targets:**

*orchestrator.go (2,055 lines) → split into:*
- `orchestrator.go` (~400 lines) — DownloadOrchestrator struct, Execute(), core dispatch
- `orchestrator_youtube.go` (~400 lines) — runLiveStreamDownload(), quality monitoring integration
- `orchestrator_twitch.go` (~300 lines) — ExecuteTwitch(), Twitch-specific helpers
- `orchestrator_mux.go` (~350 lines) — muxAndFinalize(), muxSegment(), finalizeMultiSegmentJob()
- `orchestrator_chat.go` (~200 lines) — setupChatDownloader(), waitForChat(), cleanup
- `orchestrator_utils.go` (~200 lines) — runFFprobe(), parseFpsString(), copyFile(), runDownloaders()

*stream_processor.go (1,046 lines) → split into:*
- `stream_processor.go` (~350 lines) — StreamProcessor struct, Process() dispatch, shared probing
- `stream_processor_youtube.go` (~350 lines) — YouTube-specific probing, waitUpcoming(), probeLiveAndDownload()
- `stream_processor_twitch.go` (~200 lines) — ProcessTwitch(), probeTwitch()
- `stream_processor_chat.go` (~150 lines) — setupChatDownloader(), chat lifecycle tracking

*strategies.go (703 lines) → split into:*
- `strategies.go` (~150 lines) — DownloadResult type, shared helpers, buildPotToken()
- `strategy_youtube_dash.go` (~200 lines) — DownloadLiveManifestDash()
- `strategy_youtube_hls.go` (~100 lines) — YouTube HLS strategy
- `strategy_youtube_vod.go` (~150 lines) — DownloadVod()
- `strategy_twitch.go` (~100 lines) — DownloadTwitch()

**Test gaps to fill:**
- orchestrator: quality change mid-download, multi-segment muxing, chat timeout/failure, FFprobe failure
- stream_processor: state machine transitions, Twitch probing
- strategies: format selection, PO token building
- orphans: scanner safety, TOCTOU scenarios

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Refactor orchestrator.go into 6 files
3. Refactor stream_processor.go into 4 files
4. Refactor strategies.go into 5 files
5. Fix all issues — prioritize the activeChats race condition
6. Add or strengthen tests
7. Run: `go build ./...`
8. Run: `go test ./internal/worker/...`
9. Run: `go vet ./internal/worker/...`
10. Commit: `fix: audit & refactor phase 6 (worker pipeline) — N fixes, Y refactors across Z files`

---

## Task 7: Web Server & Supporting Services

**Packages:** web (server + routes), notifications, updater (~8,489 lines)
**Why seventh:** HTTP-facing code with auth, sessions, and rate limiting. Internet-facing layer.

**Files to audit:**

*web/ core (2,777 lines):*
- `internal/web/server.go` (586 lines) — HTTP server, middleware stack, auth management
- `internal/web/middleware.go` (306 lines) — CORS, security headers, CSRF, IP gating
- `internal/web/auth.go` (276 lines) — scrypt password hashing, session management
- `internal/web/websocket.go` (474 lines) — WebSocket hub, throttling, broadcasting
- `internal/web/rate_limiter.go` (136 lines) — per-IP sliding window
- `internal/web/tls.go` (158 lines) — TLS cert loading
- `internal/web/auth_test.go`, `middleware_test.go`, `rate_limiter_test.go`

*web/routes/ (4,584 lines):*
- `internal/web/routes/auth.go` (431 lines) — login/logout, sessions, client tokens
- `internal/web/routes/jobs.go` (2,525 lines) — job CRUD, pagination, config, channels, etc.
- `internal/web/routes/cookies.go` (~150 lines) — cookie refresh status
- `internal/web/routes/stats.go` (~122 lines) — disk/activity stats
- `internal/web/routes/files.go` (~92 lines) — orphan file management
- `internal/web/routes/ffmpeg.go` (649 lines) — FFmpeg validation/install
- `internal/web/routes/ffmpeg_elevation_windows.go` (133 lines) — Windows elevation
- `internal/web/routes/ytdlp.go` (352 lines) — yt-dlp plugin
- `internal/web/routes/update.go` (~146 lines) — update check/apply

*notifications/ (527 lines):*
- `internal/notifications/manager.go` (194 lines) — dispatcher
- `internal/notifications/discord.go` (121 lines) — Discord webhook
- `internal/notifications/manager_test.go` (212 lines)

*updater/ (629 lines):*
- `internal/updater/updater.go` (323 lines) — GitHub Releases polling, binary download
- `internal/updater/signing.go` (84 lines) — Ed25519 verification
- `internal/updater/semver.go` (51 lines) — version parsing
- `internal/updater/signing_test.go`, `semver_test.go`

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| jobs.go | **2,525 lines** — JobRoutes() is 850+ lines of inline handler code; applyConfigUpdates() is 213 lines of if chains; validateConfigUpdates() is 181 lines | Critical/Structural |
| websocket.go | `throttleTimers` map never cleaned up — memory leak for throttled jobs that never fire again | Important |
| websocket.go | No backpressure handling — sends to slow clients can block | Medium |
| websocket.go | readPump() doesn't distinguish connection errors from normal close | Low |
| server.go | Mixed concerns in AuthMiddleware() — session validation + client token fallback | Low |
| server.go | gzipResponseWriter complex wrapper with Hijacker/Pusher/Flusher interfaces | Low |
| middleware.go | IP detection scattered across multiple functions | Low |
| auth.go | Session cleanup goroutine started in Start() — no guarantee called before first use | Low |
| routes/auth.go | AuthMiddleware in routes package should be in web package | Low |
| routes/ffmpeg.go | `pendingInstall` map is global mutable state — not protected by mutex | Important |
| routes/ffmpeg.go | `cleanExpiredPending()` never called — pending token entries leak | Important |
| routes/ffmpeg.go | Windows-only logic but file isn't `*_windows.go` build-tagged | Low |
| routes/ytdlp.go | Plugin code is hardcoded Python string — port/HTTPS detection at install time, not runtime | Low |
| notifications/manager.go | `Send()` spawns goroutine per notification — no rate limiting | Low |
| notifications/manager.go | `Wait()` has no timeout | Low |
| updater.go | `ApplyUpdate()` overwrites binary in-place — could corrupt on write failure | Medium |
| updater.go | No retry logic for download failures | Low |

**Refactoring targets:**

*jobs.go (2,525 lines) → split into:*
- `jobs.go` (~300 lines) — JobRoutes() (route registration only, handlers extracted)
- `jobs_crud.go` (~400 lines) — GET/POST/PUT/DELETE job handlers
- `jobs_filters.go` (~150 lines) — filterJobsByAge(), sendPaginated(), sorting
- `config_routes.go` (~300 lines) — ConfigRoutes(), ConfigRoutesCallbacks
- `config_validator.go` (~200 lines) — validateConfigUpdates(), helper validators
- `config_applier.go` (~220 lines) — applyConfigUpdates() split by domain
- `channel_routes.go` (~200 lines) — ChannelRoutes()
- `trim_routes.go` (~150 lines) — TrimRoutes()
- `pot_routes.go` (~150 lines) — PotRoutes(), PotProviderInterface
- `setup_routes.go` (~150 lines) — SetupRoutes(), SetupDeps
- `import_routes.go` (~200 lines) — ImportRoutes()
- `log_routes.go` (~50 lines) — LogRoutes()
- `format_routes.go` (~100 lines) — FormatRoutes()
- `restart_routes.go` (~50 lines) — RestartRoute()

**Test gaps to fill:**
- WebSocket throttling: cleanup, backpressure, concurrent broadcasts
- Route handlers: config validation/application, job CRUD edge cases
- FFmpeg pending install: mutex protection, cleanup
- Updater: download failure, partial write recovery

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Refactor jobs.go into 14 files (split by route group and concern)
3. Fix all issues — prioritize WebSocket memory leak and FFmpeg pending install mutex
4. Add or strengthen tests
5. Run: `go build ./...`
6. Run: `go test ./internal/web/... ./internal/notifications/... ./internal/updater/...`
7. Run: `go vet ./internal/web/... ./internal/notifications/... ./internal/updater/...`
8. Commit: `fix: audit & refactor phase 7 (web server) — N fixes, Y refactors across Z files`

---

## Task 8: TUI

**Packages:** tui (~13,009 lines, 22 files)
**Why eighth:** Largest package. Depends on everything else. Complex state machines.

**Files to audit:**

*Large files (>500 lines):*
- `internal/tui/app.go` (2,486 lines) — main TUI model, Update(), dispatchAction(), buildMenuItems()
- `internal/tui/settings.go` (2,143 lines) — settings UI, 3 sub-editors, form builders
- `internal/tui/setup_wizard.go` (1,309 lines) — first-run setup
- `internal/tui/add_video.go` (1,122 lines) — URL input, format selection
- `internal/tui/job_details.go` (918 lines) — job detail view
- `internal/tui/task_list.go` (793 lines) — job list, filtering
- `internal/tui/ffmpeg_check.go` (630 lines) — FFmpeg setup dialog
- `internal/tui/action_menu.go` (623 lines) — command palette
- `internal/tui/trim_dialog.go` (609 lines) — trim UI

*Smaller files (<500 lines):*
- `internal/tui/client_tokens_dialog.go` (365 lines)
- `internal/tui/files_dialog.go` (354 lines)
- `internal/tui/keys.go` (316 lines)
- `internal/tui/log_viewer.go` (310 lines)
- `internal/tui/import_dialog.go` (278 lines)
- `internal/tui/help.go` (260 lines)
- `internal/tui/status_bar.go` (240 lines)
- `internal/tui/text_input.go` (188 lines)
- `internal/tui/styles.go` (157 lines)
- `internal/tui/marquee.go` (121 lines)
- `internal/tui/progress_store.go` (43 lines)
- `internal/tui/mouse.go` (30 lines)

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| app.go | **2,486 lines** — Update() is 800+ lines handling all message types inline | Critical/Structural |
| app.go | dispatchAction() is 150+ lines — huge switch with 20+ actions | Structural |
| app.go | App type has 30+ fields — mixing UI state, component refs, callbacks, channels | Structural |
| app.go | listenForUpdates() goroutine never exits — leaks when App closes | Important |
| app.go | API client recreated per request despite cachedClient field | Low |
| app.go | buildMenuItems() regenerated every call — not cached | Low |
| settings.go | **2,143 lines** — 3 separate sub-editors each duplicate mode logic | Critical/Structural |
| settings.go | loadValues() is 60 lines of if chains | Medium |
| settings.go | applyValues() is 100 lines of if chains | Medium |
| settings.go | HandleKey() dispatches to 5 handlers based on nested mode states | Structural |
| settings.go | Rendering is 600+ lines across 7 functions | Structural |
| settings.go | Save status and security mode enums should be types not ints | Low |
| job_details.go | Time parsing errors (lines 410, 413, 416) silently ignored | Medium |
| keys.go | Chord definitions should be near buildMenuItems() or in central registry | Low |
| styles.go | Duplicate lipgloss.NewStyle() calls could be cached | Low |

**Refactoring targets:**

*app.go (2,486 lines) → split into:*
- `app.go` (~400 lines) — App struct, NewApp(), Init(), View() core layout
- `app_update.go` (~400 lines) — Update() split by message type
- `app_keyboard.go` (~300 lines) — handleKey(), handleChord(), chord state machine
- `app_mouse.go` (~100 lines) — handleMouse()
- `app_commands.go` (~400 lines) — all async *Cmd() functions
- `app_actions.go` (~300 lines) — dispatchAction(), buildMenuItems()
- `app_layout.go` (~200 lines) — cycleFocus(), setFocus(), recalcLayout()
- `app_api.go` (~200 lines) — apiBaseURL(), apiClient(), token transport
- `app_messages.go` (~100 lines) — all message type definitions

*settings.go (2,143 lines) → split into:*
- `settings.go` (~300 lines) — SettingsModel struct, lifecycle, main HandleKey dispatch
- `settings_fields.go` (~250 lines) — field editing (toggle, cycle, text input), loadValues(), applyValues()
- `settings_channels.go` (~350 lines) — channel sub-editor (list, edit, resolve, delete)
- `settings_notifications.go` (~250 lines) — notification sub-editor
- `settings_security.go` (~250 lines) — security sub-editor (password set/remove/manage)
- `settings_render.go` (~400 lines) — all rendering functions
- `settings_render_channels.go` (~200 lines) — renderChannels(), renderChannelEdit()
- `settings_render_security.go` (~150 lines) — renderSecurity() and sub-modes

**Test gaps to fill:**
- No TUI tests exist at all — this is expected for bubbletea apps (hard to unit test)
- Consider adding tests for pure logic functions (buildMenuItems, format helpers, state transitions)

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Refactor app.go into 9 files
3. Refactor settings.go into 8 files
4. Fix all issues — prioritize the listenForUpdates goroutine leak
5. Add tests for pure logic functions where feasible
6. Run: `go build ./...`
7. Run: `go test ./internal/tui/...` (if any tests exist)
8. Run: `go vet ./internal/tui/...`
9. Commit: `fix: audit & refactor phase 8 (TUI) — N fixes, Y refactors across Z files`

---

## Task 9: Entry Point & Utilities

**Packages:** cmd/moombox, utils (~4,148 lines)
**Why ninth:** main.go wires everything together — best audited last with full project context. utils/ is cross-cutting.

**Files to audit:**

*utils/ (2,029 lines):*
- `internal/utils/http.go` (162 lines) + `http_test.go`
- `internal/utils/ip.go` (60 lines) + `ip_test.go`
- `internal/utils/youtube.go` (237 lines) + `youtube_test.go`
- `internal/utils/twitch.go` (113 lines) + `twitch_test.go`
- `internal/utils/sanitize.go` (55 lines) + `sanitize_test.go`
- `internal/utils/media.go` (40 lines) + `media_test.go`
- `internal/utils/text.go` (35 lines) + `text_test.go`
- `internal/utils/format.go` (35 lines)
- `internal/utils/time_format.go` (55 lines) + `time_format_test.go`
- `internal/utils/smooth.go` (30 lines) + `smooth_test.go`
- `internal/utils/async.go` (34 lines) — Sleep, Jitter, SleepWithJitter
- `internal/utils/channel.go` (50 lines) — ResolveChannelInput
- `internal/utils/ffprobe.go` (101 lines) — ExtractVideoMetadata

*cmd/moombox/ (2,119 lines):*
- `cmd/moombox/main.go` — flag parsing, init, TUI/Web startup, signal handling, shutdown

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| main.go | **2,119 lines** — `run()` is 1,509 lines, single massive function | Critical/Structural |
| main.go | 12 numbered service initialization sections with implicit ordering dependencies | Structural |
| main.go | 30+ callback assignments to TUI app (app.On*) | Structural |
| main.go | Shutdown sequence depends on initialization order but coupling is implicit | Medium |
| main.go | No graceful degradation — one service init failure kills entire startup | Low |
| main.go | Adapter types at bottom (ytFormatAdapter, etc.) should be near usage or own file | Low |
| ffprobe.go | ParseInt errors on duration/size (lines 68, 73) silently ignored | Medium |
| http.go | Response body leak possible on retry (response body must be closed before retry) | Medium |
| ip.go | IPv6, mapped IPv4, link-local edge cases | Low |
| sanitize.go | Reserved Windows names (CON, PRN, NUL) not fully handled | Low |

**Refactoring targets:**

*main.go (2,119 lines) → split into:*
- `main.go` (~200 lines) — main(), launchAndSupervise(), flag parsing
- `bootstrap.go` (~300 lines) — loadConfig(), initLogger(), openDatabase()
- `services.go` (~300 lines) — YouTube, Twitch, PotProvider, cipher service init
- `server_setup.go` (~250 lines) — HTTP server, route registration, middleware
- `websocket_setup.go` (~150 lines) — WebSocket hub, client auth
- `monitors_setup.go` (~200 lines) — Feed/DECAPI/Twitch monitor init + callbacks
- `worker_setup.go` (~200 lines) — DownloadWorker, TrimService init
- `tui_setup.go` (~250 lines) — TUI app init + 30+ callback assignments
- `shutdown.go` (~100 lines) — graceful shutdown sequence
- `adapters.go` (~150 lines) — ytFormatAdapter, twitchMetadataAdapter, etc.

**Test gaps to fill:**
- ffprobe: silent parse errors
- http: response body leak on retry
- sanitize: Windows reserved names
- main.go: generally untestable monolith, but extracting services makes individual init testable

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Refactor main.go into 10 files
3. Fix all issues found
4. Add or strengthen tests
5. Run: `go build ./...`
6. Run: `go test ./internal/utils/...`
7. Run: `go vet ./internal/utils/... ./cmd/moombox/...`
8. Commit: `fix: audit & refactor phase 9 (entry point & utils) — N fixes, Y refactors across Z files`

---

## Task 10: Web Frontend

**Files:** web/public/ (~245KB total), web/embed.go
**Why last:** Frontend depends on backend API contract. Best audited after all backend is stable.

**Files to audit:**

- `web/public/app.js` (2,631 lines) — main dashboard controller
- `web/public/index.html` — dashboard HTML (Shoelace v2.16)
- `web/public/login.html` (154 lines) — login page
- `web/public/moombox.css` (2,121 lines) — styling
- `web/public/modules/imports.js` (212 lines) — ZIP import UI
- `web/public/modules/player.js` (844 lines) — video player, chat replay, Niconico overlay
- `web/public/modules/segments.js` (120 lines) — multi-segment playback
- `web/public/modules/settings.js` (1,682 lines) — config UI
- `web/public/modules/setup.js` (788 lines) — first-run wizard
- `web/public/modules/stats.js` (161 lines) — health dashboard
- `web/public/modules/trimmer.js` (465 lines) — video trimming UI
- `web/public/modules/utils.js` (73 lines) — shared formatters
- `web/embed.go` (8 lines) — go:embed directive

**Known issues from analysis:**

| File | Issue | Severity |
|------|-------|----------|
| app.js | **2,631 lines** — mixing job management, log rendering, UI state, keyboard shortcuts, utilities | Critical/Structural |
| app.js | Full job list re-render on every `jobs_update` even for single job changes | Medium |
| app.js | Log rendering rebuilds entire DOM on every message (debounced 100ms) | Medium |
| app.js | Job details HTML template: 285 lines of nested ternaries | Structural |
| app.js | Keyboard shortcut conflicts: player shortcuts active when global shortcuts also fire | Medium |
| app.js | Silent reload on 401 — user doesn't see they were logged out | Low |
| player.js | **XSS risk**: `contentSpan.innerHTML = this.renderChatMessageParts(...)` — uses innerHTML after template rendering | Important |
| player.js | Niconico lane calculation O(n²) per message — performance issue on high-chat streams | Medium |
| player.js | Chat messages not accessibility-friendly (no role, aria-label, semantic HTML) | Low |
| settings.js | **1,682 lines** — handles config, channels, cookies, notifications, yt-dlp plugin | Structural |
| settings.js | `populateConfigForm()` assumes specific config structure — silent failure if schema changes | Low |
| setup.js | `insertAdjacentHTML` with inline HTML for restart dialog — safer to use createElement | Low |
| trimmer.js | Timeline drag not cancelled on window blur — dragging state persists | Low |
| trimmer.js | No minimum duration validation (endTime > startTime + 1s) | Low |
| moombox.css | Status colors use hex instead of Shoelace `--sl-color-*` vars | Low |
| moombox.css | No `prefers-reduced-motion` support | Low |
| login.html | Fixed 380px width — not responsive on mobile < 380px | Low |

**Refactoring targets:**

*app.js (2,631 lines) → extract classes:*
- `MoomboxApp` (~800 lines) — init, WebSocket, config/status, module lifecycle, event delegation
- New `modules/jobs.js` (~600 lines) — job rendering, filtering, sorting, card updates, CRUD actions
- New `modules/logs.js` (~200 lines) — log add/filter/render/clear
- New `modules/keyboard.js` (~200 lines) — keyboard shortcuts, player conflicts

*settings.js (1,682 lines) → split into:*
- `settings.js` (~400 lines) — SettingsController, config form population
- New `modules/channels.js` (~400 lines) — channel management UI
- New `modules/cookies-ui.js` (~300 lines) — cookie management UI
- New `modules/notifications-ui.js` (~300 lines) — notification config UI

**Test gaps:** No frontend tests exist (manual testing only). Adding frontend tests is out of scope for this audit but note the gap.

**Steps:**

1. Read every file listed above, noting issues per audit category
2. Extract JobManager, LogManager, KeyboardManager from app.js
3. Split settings.js into sub-controllers
4. Fix all issues — prioritize the innerHTML XSS risk in player.js
5. Fix performance: incremental job updates instead of full re-render
6. Run: `go build ./...` (to verify embed still works after file changes)
7. Manual smoke test: load dashboard, verify all tabs work, check WebSocket
8. Commit: `fix: audit & refactor phase 10 (frontend) — N fixes, Y refactors across Z files`

---

## Execution Notes

**Build verification between phases:**
After each task's commit, run `go build -o /dev/null ./cmd/moombox` to confirm the full binary still compiles.

**Cross-phase issues:**
If auditing a later phase reveals an issue in an already-committed phase, fix it in the current phase's commit — don't amend. Note in the commit body: "Also fixes [package] issue discovered during [current phase] audit."

**Refactoring verification:**
After every file split, immediately run `go build ./...` and `go vet ./...` before continuing. A broken refactoring is harder to debug once mixed with other changes.

**Issue severity tiers (for commit body organization):**
- **Critical** — security vulnerabilities, data loss risks, crash bugs
- **Important** — logic bugs, resource leaks, race conditions
- **Minor** — edge case handling, defensive checks, error messages
- **Quality** — dedup, dead code removal, naming, consistency
- **Structural** — file splits, function extraction, readability
- **Tests** — new or strengthened tests

**Commit message format:**
```
fix: audit & refactor phase N (group name) — X fixes, Y refactors across Z files

Critical: [description]. Important: [description]. Minor: [description].
Quality: [description]. Structural: [description]. Tests: [description].

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>
```

**Summary of planned refactoring splits:**

| Phase | Source File | Lines | Split Into | New Files |
|-------|-----------|-------|------------|-----------|
| 2 | database.go | 1,401 | 5 files | database_jobs.go, database_subscribers.go, database_batch.go, database_extras.go |
| 2 | autocookies.go | 1,648 | 5 files | autocookies_detect.go, autocookies_firefox.go, autocookies_chromium.go, autocookies_merge.go |
| 3 | extractor.go | 813 | 3 files | extractor_full_player.go, extractor_legacy.go |
| 3 | player_api.go | 1,084 | 3 files | player_api_strategy.go, player_api_parsing.go |
| 4 | twitch/chat.go | 1,031 | 3 files | chat_irc.go, chat_recording.go |
| 5 | downloader.go | 1,430 | 6 files | downloader_dash.go, downloader_hls.go, downloader_direct.go, downloader_parallel.go, downloader_resume.go |
| 5 | muxer.go | 673 | 4 files | muxer_trim.go, muxer_progress.go, muxer_twopass.go |
| 6 | orchestrator.go | 2,055 | 6 files | orchestrator_youtube.go, orchestrator_twitch.go, orchestrator_mux.go, orchestrator_chat.go, orchestrator_utils.go |
| 6 | stream_processor.go | 1,046 | 4 files | stream_processor_youtube.go, stream_processor_twitch.go, stream_processor_chat.go |
| 6 | strategies.go | 703 | 5 files | strategy_youtube_dash.go, strategy_youtube_hls.go, strategy_youtube_vod.go, strategy_twitch.go |
| 7 | jobs.go | 2,525 | 14 files | jobs_crud.go, jobs_filters.go, config_routes.go, config_validator.go, config_applier.go, channel_routes.go, trim_routes.go, pot_routes.go, setup_routes.go, import_routes.go, log_routes.go, format_routes.go, restart_routes.go |
| 8 | app.go | 2,486 | 9 files | app_update.go, app_keyboard.go, app_mouse.go, app_commands.go, app_actions.go, app_layout.go, app_api.go, app_messages.go |
| 8 | settings.go | 2,143 | 8 files | settings_fields.go, settings_channels.go, settings_notifications.go, settings_security.go, settings_render.go, settings_render_channels.go, settings_render_security.go |
| 9 | main.go | 2,119 | 10 files | bootstrap.go, services.go, server_setup.go, websocket_setup.go, monitors_setup.go, worker_setup.go, tui_setup.go, shutdown.go, adapters.go |
| 10 | app.js | 2,631 | 4 modules | modules/jobs.js, modules/logs.js, modules/keyboard.js |
| 10 | settings.js | 1,682 | 4 modules | modules/channels.js, modules/cookies-ui.js, modules/notifications-ui.js |

**Total: 17 large files split into ~98 smaller files**
