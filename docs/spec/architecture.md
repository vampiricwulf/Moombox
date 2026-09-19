# Architecture

## Scope

This document provides a comprehensive technical reference for Moombox's internal architecture: the process model, service initialization, package structure, data flow, download pipeline, concurrency model, error handling, and key type definitions. It is the deepest and most detailed document in the specification suite, intended to give an LLM or developer full context for understanding how the system works at every level. Read this before making changes to core infrastructure, the download pipeline, or cross-cutting service wiring.

## Rules and Constraints

These are hard requirements that must be followed in all code changes:

- **Launcher/supervisor pattern via `_MOOMBOX_CHILD` env var.** The binary operates in two modes. Without the env var it acts as a launcher that spawns itself as a child. With `_MOOMBOX_CHILD=1` it runs the full application. Exit code 42 (`exitCodeRestart`) signals the launcher to respawn. This enables seamless restarts for config changes and binary updates.
- **All goroutines MUST have panic recovery.** Every `go func()` must include an inline `defer func() { if r := recover(); ... }()`. No exceptions. HTTP handlers use `RecoveryMiddleware`. Database callbacks use `safeCallJobUpdate`/`safeCallJobsChange`. Monitor callbacks wrap `OnVideoFound`/`OnStreamFound` with deferred recovery.
- **Logger interface is anonymous per-struct -- NEVER extract to a named interface.** Each struct that needs logging declares its own anonymous `logger interface { Debug/Info/Warn/Error }` field. This is intentional for loose coupling. Do not create a shared `Logger` type or named interface in a common package. The one exception is the `worker` package which declares a package-level `Logger` interface for internal reuse within that package only.
- **Database partial updates use `UpdateJobFields()` with dynamic SET clauses.** Pass a `map[string]any` of field names to values. The method dynamically builds the SQL SET clause, auto-updates `updated_at`, and triggers `OnJobUpdate` subscribers. Never write raw UPDATE SQL for job fields outside this pattern.
- **Callback closures for cross-cutting service wiring, NOT interfaces.** Services are wired together in `main.go` using function closures (`OnVideoFound`, `OnStreamFound`, `OnSchedule`, `OnCookieRefreshNeeded`, etc.) and struct-based dependency injection. There are no service registry patterns or interface-based DI containers.
- **JobStatus is `type JobStatus string`.** Timestamps are ISO 8601 strings (RFC3339). Optional numeric fields (sequence numbers, dimensions, file sizes) use pointers (`*int`, `*int64`, `*float64`) where nil means "not set." Boolean fields in the database use integer 0/1 but are exposed as Go `bool` in the `Job` struct.
- **Cross-platform via build tags.** Windows x64, Linux x64, and Linux arm64 are supported. Platform-specific behavior is isolated in per-package `_windows.go` / `_unix.go` files: `createNoWindow = 0x08000000` (launcher, Windows only), kernel32 disk queries (`internal/disk/disk_windows.go` vs `disk_unix.go` via statfs), TCP-dial connectivity monitor (`monitor_unix.go`), flock-based single-instance locking (`single_instance_unix.go`), and the ping-based `.exe~` cleanup (`launcher_windows.go`). Linux stubs produce correct no-op or functional fallback behavior; Windows-only features degrade with clear UI messaging.

## Process Model

### Launcher/Supervisor Pattern

Moombox uses a two-process model controlled by the `_MOOMBOX_CHILD` environment variable:

**Launcher process** (no `_MOOMBOX_CHILD`):
- Executes `launchAndSupervise()` in `cmd/moombox/launcher.go`
- Ignores SIGINT (the child handles Ctrl+C)
- Spawns itself as a child with `_MOOMBOX_CHILD=1` added to the environment
- Passes through stdin/stdout/stderr so the child's TUI renders in the launcher's console
- When the child exits with code 42 (`exitCodeRestart`), the launcher respawns. This picks up any new binary (for self-updates via the three-step rename dance: `.new` -> current -> `.old`)
- When the child exits with code 0 or any other code, the launcher exits with the same code
- On respawn, cleans up `.old` binary by renaming to `.exe~` (freeing the `.old` name for future updates)
- On final exit, spawns a detached `cmd /C ping ... & del` process to delete the `.exe~` file after the launcher releases its lock

**Application process** (with `_MOOMBOX_CHILD=1`):
- Runs the full service stack via `run()`
- Handles SIGINT/SIGTERM for graceful shutdown
- Returns `true` from `run()` to signal a restart request, causing `main()` to `os.Exit(42)`

**Restart mechanism:**
```
triggerRestart(source string) {
    restartRequested.Store(true)  // atomic bool
    cancel()                      // cancel the main context
    quitTUI()                     // if TUI is running, unblock tea.Program.Run()
}
```
Called from: `routes.SetupRoutes` (setup wizard completion), `routes.UpdateRoutes` (after applying update), `routes.RestartRoute` (manual API restart).

**Shutdown sequence:**
1. Context cancellation propagates to all services
2. TUI quits (if running)
3. Download worker stops (10-second timeout for in-flight jobs)
4. Monitors stop
5. Cookie refresh stops
6. Web server shuts down
7. Database closes (flushes pending batch updates)
8. Logger closes (flushes file)
9. Force-exit timer (10 seconds) kills the process if graceful shutdown stalls

### Subcommands and Flags

Before entering the main `run()` function, the child process checks for subcommands:

- `moombox add <video_id_or_url>` -- CLI mode that adds a video to the database and exits. Connects to the running instance's web API.
- `-version` -- Prints version and commit hash, exits immediately.
- `-headless` / `-no-tui` -- Runs web-only mode (no BubbleTea TUI). Also activated by `MOOMBOX_NO_TUI=1` env var.
- `-log-level <LEVEL>` -- Overrides the log level for this run only (DEBUG, INFO, WARN, ERROR). It reaches the logger and nothing else: `effectiveLogLevel` in `cmd/moombox/helpers.go` picks it over the configured level when building the logger, and `cfg.Logs.LogLevel` is left as the file has it, so the boot auto-persist, the password auto-hash and every later settings save keep writing the CONFIGURED level. A settings save re-applies that configured level to the running logger and drops the override.
- `-config <path>` -- Specifies the config file. An explicit path is AUTHORITATIVE: that file is the only one considered, and if it does not exist Moombox starts from defaults and a later save creates it there — it never falls through to another location. Without the flag the search order is `./config.toml`, `./config/config.toml`, `~/.config/moombox/config.toml`, then defaults.

TTY detection uses `go-isatty` on both stdin and stdout. If either is not a terminal, TUI is disabled automatically.

## Service Initialization Order

The `run()` function in `cmd/moombox/main.go` initializes services in this exact order. The order matters because later services depend on earlier ones.

### 1. Config
Load TOML configuration via `config.Load(configPath)`. An explicit `-config` path is the only file considered; only when the flag is absent does it search `./config.toml`, `./config/config.toml`, `~/.config/moombox/config.toml`. If no config file exists — either the named one or, searching, any of the three — defaults are used and the setup wizard will be triggered via the web UI; the path that was asked for stays the save target, so the first write creates the file exactly where it was named.

Auto-converts plaintext password to scrypt hash if detected (one-time migration on first run after setting a password).

### 2. Logger
`logger.New()` creates an slog-based logger with:
- File rotation (configurable max size and file count)
- Ring buffer for recent log lines (served to web UI and TUI)
- Pub/sub subscription system (log lines broadcast to WebSocket clients)
- Level filtering (DEBUG/INFO/WARN/ERROR, changeable at runtime)

### 3. Updater
`updater.New()` creates the GitHub release checker. Cleans up `.old` binary from previous update via `CleanupOldBinary()`. Performs Ed25519 signature verification before applying binary swaps.

### 4. Database
`database.Open()` opens SQLite with WAL mode, 5-second busy timeout, foreign keys enabled, single-writer connection pool (`MaxOpenConns=1`). Runs schema migrations (currently at v6). Starts the batch update coalescing goroutine. Prepares hot-path statements.

### 5. Cookie Jar
`cookies.NewCookieJar()` creates the cookie container. If `cfg.Cookies.CookieFile` is set, loads cookies from the Netscape-format file. Auto-detects platforms (YouTube/Twitch) from cookie domains if not explicitly configured.

### 6. YouTube Service
`youtube.NewService(jar, log)` creates the YouTube service. `Init(ctx)` fetches the YouTube homepage to extract visitor data and the Innertube API key. These are needed for all subsequent API calls.

### 7. Twitch Service
`twitch.NewService(jar, log)` creates the Twitch service. Initializes GQL API authentication from cookies (looks for `auth-token` cookie). Logs auth status at startup.

### 8. PO Token Provider + BotGuard Sidecar
`bgutils.NewPotProvider()` creates the BotGuard PO token provider with its triple-layer cache: session cache (6h TTL), minter cache (single-minter design, dynamic TTL from BotGuard response), and inflight dedup (concurrent requests share a single generation via channel synchronization). Immediately after, when `cfg.Bgutils.UseSidecar` is true (default), `sidecar.New(...)` constructs a `Sidecar` and `Start(ctx)` launches the embedded Node.js subprocess: extract `node.exe.gz` + `sidecar.tar.gz` from `go:embed` to `%LOCALAPPDATA%/Moombox/sidecar/`, apply user-only DACL, spawn `node src/server.js` pinned to a Windows Job Object, ping/pong handshake. On success, `potProvider.SetSidecar(s)` attaches it; `PotProvider.generateAndMint` then prefers the sidecar path and falls through to the goja-only path on any sidecar error so PO-token generation never goes completely dark. Failure to start the sidecar is non-fatal — Moombox logs a warning and continues with goja-fallback. On Linux the per-platform blob (`node-linux-amd64.gz` or `node-linux-arm64.gz`) is selected at runtime; the extraction directory is platform-appropriate (e.g. `~/.local/share/moombox/sidecar/` on Linux).

### 9. Cipher Solver
`cipher.NewSolver(cacheDir, log)` creates the YouTube signature cipher solver. Cache directory is `%TEMP%/yt-cipher`. Manages a 10-VM LRU cache keyed by `player.js` URL. Wired to `ytService.PlayerAPI.SetCipherSolver()` so format URL decryption is transparent. Uses full AST parsing with regex fallback for extraction.

### 10. Notification Manager
`notifications.NewManager(cfg, log)` creates the notification dispatcher. Currently supports Discord webhooks. Sends notifications for: stream found, stream live, download starting, download finished, download error, auth required, trim created, update available.

### 11. Download Worker
`worker.NewDownloadWorker()` creates the main job processing engine. Internally creates:
- `JobQueue` with configurable max parallel VOD downloads (default 10; broadcasts are never throttled by the pool) and 100 lifecycle slots, claimed at the download decision rather than at dequeue
- `Scheduler` that admits backlog (`Queued`) jobs at most `archive_slots` at a time per channel — the only path out of `Queued`
- `StreamProcessor` for probing stream status and waiting for live
- `DownloadOrchestrator` for the full download lifecycle

Dependencies injected via `DownloadWorkerDeps` struct: cipher solver, PO token provider, Twitch service, notification manager.

The `OnCookieRefreshNeeded(platform string) bool` callback is wired to `autoCookieSvc.RefreshCookiesDetailed()` for automatic cookie recovery on auth failures. It answers per platform (`RefreshResult.Verdict`), not per service: a healthy Twitch must not tell a YouTube job to retry into the same failure.

### 12. Trim Service
`worker.NewTrimService()` creates the FFmpeg-based clip creation service. Prevents concurrent trim operations on the same job via `activeOps` mutex map.

### 13. Feed Monitor (YouTube RSS + members-only)
`monitor.NewFeedMonitor()` polls YouTube RSS feeds (`https://www.youtube.com/feeds/videos.xml?channel_id=...`) for new videos. Default interval from config. Immediate first check on startup, then timer-based with jitter. When `membership_discovery` is enabled (default) and YouTube auth cookies are present, `checkChannel` additionally fetches the channel's authenticated `/membership` tab (`youtube.FetchMembershipVideos`) — the only discovery source for members-only content, which RSS/DECAPI never list. `membershipConfirmedNonMember` (`cmd/moombox/monitor_callbacks.go`) collapses that fetch's session verdict and access bit into a single confirmed-non-member flag ONLY for a recognised `SessionAuthLoggedIn` session that also denies channel access; an unrecognised or ambiguous verdict, or a fetch error, answers neither question and is never memoized. A confirmed non-member is memoized for `membershipMemoTTL` (`internal/monitor/feed.go`, 6 h) and its authenticated fetch skipped until the horizon passes; a member is never memoized, because their tab is the only place a members-only live stream is ever listed. Every cycle still owes the session at least one membership fetch that RETURNS: `armMembershipLiveness` (`internal/monitor/feed.go`) nominates the memoized channel with the earliest horizon, rotating, and fails over to another memoized channel within the same cycle when the nominee's fetch errors, bounded by `membershipLivenessMaxTries` (`internal/monitor/feed.go`, 2 attempts) so a cycle where every fetch fails cannot walk the whole channel list — only nomination-driven fetches spend that budget. That fetch carries the YouTube login verdict the cookie health signal reads (`routeLivenessVerdict` in `cmd/moombox/monitor_callbacks.go`), so a cycle that fetched nothing would observe nothing; the nomination guarantees only a fetch that returns, not a verdict, so the cookie subsystem's tier-2 `FallbackLiveness` probe remains the backstop when no fetch has recently landed a conclusive observation. A repaired YouTube session or a changed account identity clears every non-member memo (`clearYouTubeMembershipMemo` in `cmd/moombox/monitor_callbacks.go`, wired to both `OnAuthRecovered` and `OnCredentialsChanged`) so members-only discovery does not wait out the TTL. Members-only candidates are probed with the authenticated `ProbeVideoAuth` closure so a members VOD classifies correctly instead of misfiring as "upcoming". A membership fetch failure is logged, writes no memo, and never marks the RSS feed unhealthy (independent signals).

`checkChannel` runs four steps per channel per cycle: **FETCH** (RSS + membership, independently fallible), **STORE** (upsert every listed item into the persistent `feed_items` table, tracking which IDs are new this cycle), **WALK** (a serial probe pass over the store's archive scope — everything published within `archive_window_days`, default 3 and per-channel overridable, plus ALL upcoming/live rows regardless of age — applying `HasActiveJob` dedup, term filtering, and probe-status rules, with per-source early exit once a date-ordered source falls entirely outside the window), and **ARCHIVE** (re-read the scope and decide job creation per row). ARCHIVE assigns each job a disposition: broadcasts (live/upcoming) and VODs first inserted this cycle are admitted immediately (`queue_priority` 0), while backlog VODs already known to the store are created as `Queued` (`queue_priority` 1) and paced by the worker's per-channel `archive_slots` scheduler — a backlog sweep never delays new or live content.

A companion `monitor.NewBackfillWorker()` owns the full-catalog backfill (channel add, window widening, or a manual `R B` / `POST /api/backfill/rescan` re-run): it scans the channel's `videos`, `streams`, and (when membership is active) `membership` tabs to window depth via YouTube's `/browse` continuation API, upserting into the same store. Scans are strictly serial across channels on a single consumer goroutine, globally paced at 1 tab page/second, resumable via a cursor persisted in `channel_state.backfill_state`, and report progress to both UIs. The sweep that queues scans rides the feed-monitor cycle, so startup and `kickMonitors()` both trigger it.

### 14. DECAPI Monitor
`monitor.NewDecapiMonitor()` uses the DECAPI API to find the latest video for YouTube channels. Rate-limited to 60 requests/minute (reads rate limit headers from responses). Stagger of 1 second between per-channel requests. The request and the classification hold separate deadlines: `fetchLatestVideo` (`internal/monitor/decapi.go`) owns the 15-second request timeout and releases it as soon as the body is read, and the probe then runs under `decapiProbeBudget` (`internal/monitor/decapi.go`, 60 s) derived from the cycle context — so a slow DECAPI answer never shortens the probe. Because DECAPI reports a channel's NEWEST video, a dormant channel would otherwise re-probe the same finished VOD every cycle; a per-channel memo skips the probe when the video ID is unchanged, its last classification was terminal (`decapiTerminalStatus` in `internal/monitor/decapi.go` — `vod` or `not_a_stream`), and the processing history still holds it. Clearing the history row re-opens the video on the next cycle.

### 15. Twitch Monitor
`monitor.NewTwitchMonitor()` polls Twitch GQL for live streams. Default 15-second interval. Channels are batched into GQL requests of up to 30 logins, with a 500 ms stagger between chunks — including after a chunk whose whole request failed (`checkChunk` in `internal/monitor/twitch.go`), since a 429 or 5xx is exactly when pacing matters. Uses the Twitch service's GQL client for stream info queries.

### 16. Cookie Refresh Service
`cookies.NewRefreshService()` validates cookies and checks auth status periodically (6h default). Detects auth loss by comparing current status against expected platforms. Triggers `OnRecoveryNeeded` callback when auth is lost, which attempts auto-cookie recovery.

### 17. Auto-Cookie Service
`cookies.NewAutoCookieService()` extracts cookies directly from Firefox/Chromium browser profiles. Handles the full flow: find browser profile, decrypt cookies, verify auth via API callbacks (`VerifyYouTubeAuth`, `VerifyTwitchAuth`), write to cookie file, persist verified platforms to config.

### 18. Web Server
`web.NewServer()` creates the chi-based HTTP server with:
- WebSocket hub for real-time updates
- Auth middleware (session-based with scrypt password hashing)
- Rate limiters (API: 20/min, PO token: 10/min, login: 5/min, password change: 3/min)
- CSRF protection (Origin/Referer validation)
- Static file serving (embedded via `go:embed`)
- SPA fallback routing

Route registration happens in `main.go` by calling `routes.*Routes()` functions, each receiving their specific dependencies.

### 19. TUI
If TTY is detected and `--headless` is not set, creates and runs the BubbleTea terminal UI. The TUI receives updates via channels and database pub/sub callbacks.

### Event Wiring

After all services are created, `main.go` wires the event callbacks:

- `feedMon.OnVideoFound` / `decapiMon.OnVideoFound` -> creates YouTube job in database per the callback's `JobDisposition` (broadcasts and new VODs enqueue in the worker immediately; backlog VODs are created `Queued` for the scheduler), broadcasts via WebSocket
- `feedMon.BackfillSweep` -> queues backfill scans for channels needing one; `s.backfillRescan` (TUI `R B` chord + `POST /api/backfill/rescan`) forces the same sweep for every channel
- `twitchMon.OnStreamFound` -> creates Twitch job, enqueues, broadcasts
- `feedMon.OnSchedule` / `decapiMon.OnSchedule` / `twitchMon.OnSchedule` -> broadcasts all three monitor timer values via WebSocket
- `db.OnJobUpdate` -> `wsHub.BroadcastJobUpdate()` (per-job WebSocket messages)
- `db.OnJobsChange` -> `wsHub.BroadcastJobsUpdate()` (full job list), prune job logs
- `log.Subscribe()` -> `wsHub.BroadcastLog()` + `db.RouteLogToJobs()` (per-job log buffers)
- `cookieRefresh.OnRecoveryNeeded` -> `runCookieRecovery()` in a background goroutine: `autoCookieSvc.RefreshCookiesDetailed()`, then notifies on the triggering platform's own verdict (OK / Failed / Unknown)

All monitor `OnVideoFound`/`OnStreamFound` callbacks are wrapped with `defer func() { if r := recover() }()` for panic isolation.

## Package Dependency Graph

```
cmd/moombox/ (21 files)                -- launcher + orchestrator (~8,020 lines)
cmd/sign/main.go                       -- CI signing tool (Ed25519)

internal/config     (6 files, ~2,110)  -- TOML config, FlexDuration, channel terms
internal/updater    (3 files, ~870)    -- GitHub release checker + self-updater + Ed25519
internal/ytdlpplugin (1 file,  ~330)   -- yt-dlp plugin file: status, install, generator (shared by the web route and the TUI R Y overlay)
internal/logger     (1 file,  ~760)    -- slog wrapper, file rotation, ring buffer, pub/sub
internal/database   (8 files, ~3,920)  -- SQLite/WAL, batch updates (100ms coalesce), pub/sub
internal/stats      (1 file,  ~70)     -- the figures both dashboards show, derived from the job aggregate + disk reading (imports only database)
internal/jobfilter  (2 files, ~470)    -- the dashboard's filter language (Parse/Match/Serialize), the TUI's / box
internal/cookies    (35 files, ~15,870) -- jar, refresh, auto-cookie (Firefox/Chromium)
internal/youtube    (13 files, ~6,430) -- Service, PlayerAPI, Auth, watch page, format selector
internal/twitch    (14 files, ~6,400)  -- Service, GQL API, auth, HLS, IRC chat, VOD chat, emotes
internal/bgutils   (6 files, ~2,050)   -- PO token: PotProvider + WebPoClient (sidecar primary, goja fallback)
internal/bgutils/sidecar (7 files,~1,840) -- Node subprocess manager: extract, JSON-RPC mux, Job Object
internal/bgutils/embed   (4 files)      -- go:embed boundary for node-windows-amd64.gz + node-linux-amd64.gz + node-linux-arm64.gz + sidecar.tar.gz + version.txt
internal/cipher     (13 files, ~3,110) -- YouTube signature cipher: AST + regex, 10-VM LRU
internal/engine    (19 files, ~7,690)  -- SegmentDownloader (DASH/HLS/VOD), manifest, FFmpeg muxer
internal/chat       (3 files, ~2,910)  -- YouTube live chat downloader (polling + batching)
internal/worker    (38 files, ~16,600) -- Worker, Orchestrator, StreamProcessor, Queue, Trim, Quality
internal/monitor    (9 files, ~5,030)  -- FeedMonitor (RSS), DecapiMonitor, TwitchMonitor
internal/notif.     (3 files, ~820)    -- Manager + Discord webhook
internal/web       (32 files, ~11,340) -- chi router, WebSocket hub, auth, middleware, routes
internal/tui       (43 files, ~21,760) -- 2-over-1 panel layout, overlays, chord system
internal/goja       (5 files, ~1,470)  -- JS runtime shims (minimal DOM, TextEncoder, timers)
internal/disk       (3 files, ~130)    -- Disk space queries: kernel32 on Windows, statfs on Linux
internal/constants  (1 file,  ~320)    -- Hardcoded values (API keys, URLs, timeouts)
internal/utils     (25 files, ~2,510)  -- HTTP helpers, formatters, YouTube URL parsing
```

Total: approximately 116,660 lines of Go across 306 source files (excluding tests, web assets, and cmd/moombox).

### Dependency Direction

Dependencies flow strictly downward. Lower-level packages never import higher-level ones:

- `cmd/moombox/main.go` imports everything (orchestrator)
- `internal/worker` imports: `database`, `engine`, `chat`, `youtube`, `twitch`, `bgutils`, `cipher`, `config`, `constants`, `notifications`, `utils`
- `internal/web` imports: `database`, `config`, `worker` (route handlers), `youtube`, `twitch`
- `internal/tui` imports: `database`, `config`, `web` (HTTP client for API calls)
- `internal/monitor` imports: `database`, `config`, `twitch`
- `internal/engine` imports: nothing from internal (standalone download/mux logic)
- `internal/database` imports: nothing from internal
- `internal/utils` imports: nothing from internal
- `internal/constants` imports: nothing from internal

Cross-cutting concerns (logging, notifications, events) flow through callback closures wired in `main.go`, not through package imports.

## Key Data Flow

```
Monitors (RSS/DECAPI/Twitch)
    |
    v
Database (AddJob)  -->  OnJobsChange subscribers
    |                        |
    v                        v
Worker.EnqueueJob      WebSocket broadcast
    |
    v
JobQueue.Enqueue (priority: Live=1, Upcoming/Downloading=0, Error=-1)
    |
    v
JobQueue.Dequeue (highest-priority pending job; no lifecycle gate)
    |
    v
StreamProcessor.Process (probe status, wait for live, start early chat)
    |
    v
JobQueue.AcquireLifecycleSlot (blocks until a lifecycle slot is free, max 100)
    |
    v
JobQueue.AcquireDownloadSlot (VODs only; blocks until a slot is free, default max 10)
    |
    v
DownloadOrchestrator.ExecuteWithChat (strategy selection, parallel download + chat)
    |
    +--> SegmentDownloader (DASH/HLS/VOD/Direct)
    +--> ChatDownloader (YouTube polling / Twitch IRC / Twitch VOD)
    +--> QualityMonitor (30s probe, split on resolution change)
    |
    v
JobQueue.ReleaseDownloadSlot (free slot for next download)
    |
    v
FFmpeg Muxer (video + audio + chat -> output file)
    |
    v
Database.UpdateJobFields(status=Finished, output_file=..., file_size=..., etc.)
    |
    v
OnJobUpdate subscribers --> WebSocket broadcast --> Web UI + TUI update
```

### Progress Update Flow (during active download)

```
SegmentDownloader.OnProgress callback
    |
    v
ProgressTracker (throttles to 1s DB persist, 16ms callback rate)
    |
    v
Database.UpdateJobFields (batched via 100ms coalesce window)
    |
    v
OnJobUpdate subscribers
    |
    v
WebSocket hub (no per-job throttle — ProgressTracker's 16ms gate caps the rate upstream)
    |
    v
Web UI / TUI (render updated progress)
```

## Download Pipeline

### StreamProcessor

The `StreamProcessor` is the first stage of job processing. It determines what a video is (live, VOD, upcoming, not a stream) and whether it should be downloaded.

**Entry point:** `Process(ctx, job) -> (StreamProcessResult, error)`

**YouTube path:**
1. Performs a full multi-client fetch via `yt.GetVideoInfo()` (WEB + TV clients for accurate playability and complete metadata)
2. Updates job metadata in the database (title, channel, thumbnail, description, scheduled start time, length)
3. Checks playability (members-only, login required, age restricted, etc.)
4. Routes based on `StreamStatus`:
   - `StreamLive` -> Updates status to Live, sends notification, returns `ShouldDownload=true, IsVod=false`
   - `StreamVOD` / `StreamPostLive` -> Updates status to Downloading, returns `ShouldDownload=true, IsVod=true`
   - `StreamNotAStream` -> Downloads as VOD only if `ManuallyAdded` or `AllowNonStream` is true
   - `StreamUpcoming` -> Enters `waitForLive()` polling loop

**waitForLive() loop:**
1. Updates job status to `Upcoming`
2. If chat download is enabled, attempts to start an early chat downloader (`tryStartEarlyChat()`) to capture pre-stream chat messages. Each probe re-evaluates the decision through `earlyChatNeedsRestart(chatDl, finished, lastRestart, now)` (`internal/worker/stream_processor_youtube.go`), which restarts when there is no downloader OR when the current one's run has ENDED and the previous start is at least `earlyChatMinRestartInterval` (5 min) behind — a run that dies immediately on dead cookies would otherwise cost one full watch-page fetch per probe for the rest of the wait, while the ~50-minute stale-exhaustion case the restart exists for is never delayed — YouTube resets a waiting-room chat after a period of inactivity, and the resulting downloader stays non-nil, so the old `chatDl == nil` gate left the waiting room uncaptured for the rest of the wait. `finished` is recorded by the run goroutine's own `defer` (`runEarlyChat`, called as the `go` statement in `tryStartEarlyChat`) once `Start` has returned by any route, never from `ChatDownloader.OnFinish` — `Start`'s recovery defer returns before `OnFinish` is reached, so a run that PANICKED would never be restarted — and never from `IsRunning()`, which is also false between `NewChatDownloader` and the goroutine reaching `Start`. The replaced downloader is untracked before the new one starts, and the new run picks up the chat file through the chat downloader's completion + adoption rules (platform-services.md) rather than overwriting it
3. Calculates probe interval based on time until scheduled start:
   - More than 1 hour away: 10-minute interval
   - 5 minutes to 1 hour: 5-minute interval
   - Less than 5 minutes: 1-minute interval
   - Plus random jitter up to 30 seconds
4. Polls via lightweight `ProbeVideoStatus()` (ANDROID_VR client for speed). Persists metadata from each probe (title, thumbnail, description, scheduled start time, etc.) using change detection — only writes to DB when values actually differ, at zero additional network cost since the probe already returns this data
5. Chat surge detection: if 30+ new messages arrive within a 15-second window, triggers an immediate probe (the stream may have gone live early)
6. Members-only detection: if the probe returns `PlayabilityMembersOnly` or `PlayabilityLoginRequired` and auth cookies are available, switches to authenticated probing
7. On transition to `StreamLive`: performs full multi-client fetch, updates metadata, sends notification, passes pre-started chat downloader to the orchestrator
8. If the scheduled start time changes between probes, sends a "Schedule Changed" notification (event: `rescheduled`)
9. Maximum 10 consecutive probe errors before giving up

**Twitch path (`processTwitch()`):**
- VOD jobs (video ID prefix `tw_v`): fetches VOD info and HLS playlist, selects best variant, optionally creates VOD chat downloader
- Live jobs: checks if channel is live via GQL. If offline and manually added, enters `waitForTwitchLive()` polling loop (15s interval + 5s jitter). If live, fetches HLS master playlist, selects best variant, starts IRC chat downloader

### DownloadOrchestrator

The `DownloadOrchestrator` manages the complete download lifecycle for a single job after the `StreamProcessor` has determined it should be downloaded.

**Entry point:** `ExecuteWithChat(ctx, jobCtx, videoInfo, isVod, existingChat) -> error`

**Sequence:**
1. Pre-execution cancellation check (job may have been cancelled between queuing and execution)
2. Subscribe to database job updates for cancellation detection
3. Update status to `Downloading`, record `download_started_at`
4. Send "Download Starting" notification
5. Create staging directory
6. Select download strategy (see below)
7. Set up progress tracking via `ProgressTracker`
8. Start chat downloader in parallel (or adopt pre-started one from `StreamProcessor`)
9. Execute download:
   - VOD: `runVodDownloadWithRefresh()` — wraps `runDownloaders()` (one-shot via `errgroup`) in a bounded re-extraction loop for YouTube jobs that finalize behind head (see below)
   - Live: `runLiveStreamDownload()` (loop with stream-end verification and quality monitoring); returns the final `*DownloadResult`, since a quality refresh/split reassigns the downloader pair inside the loop and the caller's original pointer would otherwise go stale
10. After download completes: finalize progress, sync total sequence counts
11. Signal chat to finish and wait for it. A live job waits `chatWaitTimeout` (2 minutes) — a live chat stops when the broadcast does. A VOD goes through `resolveVodChatOutcome` in `internal/worker/orchestrator_chat.go`, which releases the download slot FIRST and only then waits `vodChatWaitTimeout`: the video's own length, floored at 30 minutes and capped at 6 hours (owner decision O-A), because a VOD's comment pager is still paging long after the video finished downloading
12. Release download slot (muxing is CPU-bound, not a download) — already released, and therefore a no-op, for a VOD that came through the chat wait above
13. Mux and finalize (FFmpeg combines video + audio + metadata, ffprobe extracts dimensions/duration)
14. Post-download trim if job has `StartTime`/`EndTime` set
15. Clean up staging directory

**Strategy selection logic:**
```
if isVod AND (not_a_stream OR no DASH manifest) AND formats exist:
    DownloadVod()      -- direct format URL download, 5MB chunked Range requests
elif DASH manifest exists:
    DownloadDash()     -- sequential DASH segment download
elif HLS manifest exists:
    DownloadHls()      -- HLS playlist polling
elif formats exist:
    DownloadVod()      -- fallback: direct download
else:
    error: no download strategy available
```

**Live stream download loop (`runLiveStreamDownload`):**
1. Starts quality monitor (30-second probe interval) if user hasn't manually selected itags. Every tick starts with one cheap player call (`probeVideoInfo`, `internal/worker/orchestrator_youtube.go`): `ProbeVideoStatus` (ANDROID_VR, cookieless, no POT, no watch page) for public streams, and `ProbeVideoStatusAuthenticated` (TV_DOWNGRADED with cookies, no watch page, no STS, no POT) for members-only / age-restricted / login-required ones, where the cookieless probe would 401. When the cookieless probe comes back with nothing selectable and the install has cookies, one TV-with-cookies call is tried next — O-H's "recovery streams" half, gated on `HasAuthCookies` because a cookieless TV call is bot-walled. Only if nothing cheaper produced a `DashManifestURL` or a split-adaptive format pool does the tick fall back, ONCE, to the full `GetVideoInfo` cascade — including when a probe errored, so one 403 never ends a tick that the cascade's three-to-seven clients could still answer. TWO errors buy nothing there and end the tick where they were seen: a cancelled context, and a substituted video (`*youtube.VideoIDMismatchError`) — a substitution is IP-level, so every client is substituted alike and the cascade would only re-ask a blocking server seven more times, nine player calls per 30 s tick for as long as the block lasts. Before owner decision O-H that cascade was the FIRST call on the auth-walled branch and ran on EVERY tick: a 1-5 MB cookied watch page plus three to seven player calls, ~120 pages/hour/job.
2. Runs segment downloaders in a goroutine
3. Simultaneously listens for quality change signals on `qualityChangeCh`
4. When quality changes mid-stream:
   - Ignores if segment is shorter than 10 seconds (`minSegmentDuration`)
   - Cancels current downloaders
   - Muxes the current segment in a background goroutine parented by the orchestrator's mux root (`launchBackgroundSegmentMux` in `internal/worker/quality_split_common.go`), not `context.Background()`
   - Records segment metadata in database
   - Re-fetches video info with new format selection
   - Creates new downloaders at the new quality
   - Continues download loop
5. When download ends naturally (stream ended, quality lost, or error):
   - Verifies stream has actually ended via YouTube API (up to 6 checks, 5-minute intervals)
   - If stream is still live, re-fetches formats and restarts download
   - `ErrQualityLost` triggers format re-fetch and restart at available quality
6. Stream-end verification prevents premature termination from transient network issues

**VOD-branch bounded refresh loop (`runVodDownloadWithRefresh`):** googlevideo URLs live ~6h; a post-live download whose wall clock outlives that grant finalizes with `FinalizedBehindHead()` true on the video and/or audio downloader instead of erroring outright. The live branch has had URL-expiry recovery via `ErrQualityLost` -> `refreshDownload` from the start; the VOD branch used to run `runDownloaders()` exactly once. It now re-extracts via `GetVideoInfo`, seeds `VideoStartSeq`/`AudioStartSeq` from the last written sequence, and rebuilds through the same `refreshDownload` — provided the prior attempt actually made progress and the fresh extraction still offers manifestless DASH formats (a stream that finished processing into a true VOD is left to the incomplete-tail flag + manual retry instead). Bounded by `maxVodRefreshAttempts` (4, ~24h of wall clock); past that, or on no progress, the loop stops and returns whatever was captured.

**Eviction diagnosis (`diagnoseEvictedStart`):** both branches run this check after a nil download error. If a YouTube manifestless download finished having written zero bytes and its `HeadSeq()` is implausibly deep (past `minEvictionHead`, ~28h of segments), an ordinary failed start is an unlikely explanation — YouTube's ~120h retention window may have scrolled segment 0 out from under a marathon stream. The check bisects `[0, head]` for the oldest segment the CDN still serves (`engine.FindOldestAvailableSeq`), fetches that boundary segment, and inspects its box structure (`engine.InspectSegment`) to log a full diagnosis. A confirmed eviction (oldest available segment > 0) fails the job with a precise "exceeds YouTube's retention window" error instead of the generic empty-download failure; a dead-URL bisection or an oldest of 0 leaves the ordinary failure path to run unchanged. Diagnosis only — no download jump; that is gated future work (docs/plans/2026-08-05-incomplete-tail-and-marathon-streams.md Phase D).

**Interruption resume — finalize deferral (`stallForPossibleResume`, `internal/engine/downloader.go`):** a live YouTube `SegmentDownloader`'s `MayResume` callback, when installed, gates both of the DASH loop's MaxTimeout-backstop finalizes — `handleGoneError`'s gone-burst verification path and `handleHTTPError`'s no-segment maximum-timeout backstop (both `internal/engine/downloader_dash.go`) — but never a confirmed-ended verdict (`streamEndVerified` always finalizes immediately, MayResume unconsulted). The first `MayResume()==true` observation for a stall latches a per-episode clock (`interruptionStallStart`); `downloader.interruption_timeout` (config minutes, default 120 — see the config table in data-and-storage.md) bounds how long deferral continues, retrying every `interruptionStallRetryDelay` (5s). When `MayResume` flips false or the ceiling expires, `finalizedDuringInterruption` latches and the finalize proceeds without setting `streamEnded` — mirroring the existing behind-head-tail contract — so the resume sidecar (`.resume.json`) survives for a later in-place continuation instead of being cleared. `InterruptionTimeout == InterruptionNoStall` (a `-1` sentinel, distinct from the ordinary `0` = "no ceiling"/unbounded meaning) is a third mode: consult `MayResume` exactly once, latch `finalizedDuringInterruption` when it's true, and always return `false` — finalize proceeds on that same call, with no stall and no clock. The worker maps its own `interruption_timeout=0` ("stall disabled" per the config contract) onto this sentinel (`engineInterruptionTimeout`, `internal/worker/interruption.go`) rather than passing `0` straight through, so a disabled-stall job still latches Tier 2 evidence without ever blocking finalize. This Tier 1 engine-level stall is DASH-only — the HLS live strategy (`runHlsLoop`) has no `stallForPossibleResume` call site at all, so an HLS live YouTube download gets only the worker-level Tier 2/3 preservation described below, not this deferral.

**Interruption resume — auto-resume valve (`shouldWaitForResume`, `resumeOnRedetect`):** on the worker side, an `interruptionSignal` (`internal/worker/interruption.go`) tracks the last time a player-response fetch showed the broadcast-interrupted signature (StreamStatus live with zero formats), observed at `refreshGvsCredentials`'s re-fetch, the live loop's own post-exit refetches (`orchestrator_youtube.go`), and each YouTube strategy's `CheckStreamStatus` closure via `observeYouTubeStatusProbe` — the only site that fires while the engine is still internally stalling. `buildMayResume` reports resume-plausible when that signal is fresh (90s) OR the job's chat downloader still has its live continuation open. `runLiveStreamDownload` feeds that evidence into `noteRefreshFailure`, which splits it into two independent questions: `resumeEvidence` (does the evidence hold at all) always latches the finalize-scoped `resumeWaitLatch` when it does, while `shouldWaitForResume` (evidence AND `interruptionTimeout > 0`) is the PERMISSION gate deciding whether the loop actually retries up to `maxConsecutiveLiveChecks` (6) instead of finalizing immediately. So a `interruption_timeout=0` job never actually waits, but a genuinely-interrupted one still latches `resumeWaitLatch`, which ORs into `incomplete_tail` (`finalizeIncompleteTail`) even on a run where the engine itself never latched `FinalizedDuringInterruption`. If the ceiling (when enabled) is still reached, the job finalizes Finished with staging preserved; a later live re-detection of that same video ID reopens the valve from outside the download loop entirely — `resumeOnRedetect` (`cmd/moombox/monitor_callbacks.go`) calls `DownloadWorker.ResumeJob` when the re-detected job is Finished with `incomplete_tail` set and its staging files still exist on disk, coalesced to at most one auto-resume attempt per 5 minutes per job and never for a job a human Cancelled.

**Interruption resume — part merge (`mergeSameFormatParts`, `internal/worker/part_merge.go`):** `finalizeMultiSegmentJob` runs `mergeSameFormatParts` (Tier 4) before deciding the finalize shape — including the single-part-takes-the-plain-name rename — so a job whose parts all losslessly recombine ends up named exactly like a never-split job. It probes each part via ffprobe, groups contiguous runs of identical stream parameters, and concat-copies each run; any probe, concat, or db-replace failure leaves that run's segments untouched, since the merge is opportunistic and never a finalize gate. A chat-merge failure aborts that run specifically — video included, not just chat — leaving its parts/rows/chat files exactly as they were while other runs in the same finalize still merge; this is the only path a Twitch gap-split run's chat (`twitch.TwitchChatData`) reaches, since it always fails to unmarshal as the YouTube-shaped `chat.ChatData` the merge expects, so Twitch parts never merge today. A successful run commits every segment row — merged and untouched alike — in one `database.ReplaceJobSegments` call, then best-effort renames the merged output onto the run's first part's original name and removes the now-superseded later parts' output/chat files and their pre-mux `seg_N` staging directories.

### SegmentDownloader

The `SegmentDownloader` in `internal/engine/downloader.go` handles the actual byte-level downloading. It supports four modes:

**DASH sequential mode (`runDashLoop`):**
- Downloads video/audio segments sequentially by sequence number
- Constructs segment URLs by appending `&sq={seq}` to the base URL
- Detects stream end via HTTP 404 with retry backoff
- Performs HEAD probes every 5 seconds to discover the head sequence number
- Saves resume state every 50 segments (`ResumeSeqInterval`)
- Gap detection: if a segment returns 204/empty, records it and continues
- Catch-up mode (`runParallelCatchUp`): once `stayBehindSegments`+`CatchupThreshold` (40) segments behind the live head, hands off to a rolling window of `segment_workers` parallel fetches (default 12, configurable via `downloader.segment_workers`, no upper limit — see the config table in data-and-storage.md) instead of `ParallelDownloads`' historical fixed 6. Workers claim sequences continuously and flush completed segments in strict ascending order as they arrive — there is no per-batch barrier. See "Catch-up: rolling window and byte-bounded buffer" below for the buffer and damping mechanics.

**HLS live mode (`runHlsLoop`):**
- Polls the HLS playlist URL periodically
- Downloads new segments as they appear
- Follows the live edge (media sequence numbers); no parallel catch-up path — a stalled poller simply requests the next playlist snapshot, which already reflects whatever segments the CDN still has
- End verdict: the loop asks `CheckStreamStatus` at three playlist-failure sites — a playlist 404/410, the consecutive-fetch-failure escalation and the consecutive-parse-failure escalation — and one shared helper, `consultStreamEnd` in `internal/engine/downloader_hls.go`, classifies the answer for all three. A confirmed `ended` finalizes cleanly from ANY of them (`streamEnded` set, so the loop's exit defer clears the resume sidecar); a confirmed "still live" returns `ErrQualityLost` for the orchestrator's variant refresh. They differ only when no verdict comes back: a check ERROR at the 404/410 site defers — the 404 rejoins the consecutive-error retry budget and the next reload re-asks — while the two escalation sites have already spent that budget, so they exit with their fetch/parse failure and leave `streamEnded` unset. An unwired check finalizes at the 404/410 site (the variant is gone and nothing can say otherwise) and keeps the failure at the escalations. Three further consults — consecutive stuck skips, init-segment-fetch exhaustion and the stale window — are not routed through the helper; each latches only on a confirmed `ended`. Same rule as the DASH gone-burst verification above (`internal/engine/downloader_hls.go`). What an unset `streamEnded` then MEANS differs by platform: on YouTube the orchestrator finalizes what was captured and the resume sidecar survives for a later Resume; on Twitch that exit used to finalize the job Finished, after which the staging dir — and the sidecar in it — was deleted, so `ExecuteTwitch` in `internal/worker/orchestrator_twitch.go` now re-verifies the broadcast once and, absent a confirmed end, returns the download error so the job lands in Error with its staging intact. That verdict comes from two `GetStreamInfo` samples ~5 s apart (`confirmTwitchLiveness` in `internal/worker/worker.go`), so one transient StreamMetadata flap can no longer end a live recording at any of the six consult sites above.

**HLS VOD mode (`runHlsVodParallel`):**
- The opposite shape to the live loop: a fixed worker pool fetches the whole playlist in parallel and a byte-bounded reorder buffer writes the segments in ascending order as they land (`runHlsVodParallel` in `internal/engine/downloader_hls.go`). The ceiling is the same 256 MB `catchUpBufferBytes` (`internal/engine/downloader.go`) the DASH catch-up path uses, and it is what stops the other workers holding the rest of the VOD in RAM while the head-of-order segment works through its retry ladder — a worker waits for room instead of buffering past it, and the head is always admitted so the flush position cannot deadlock. Failed segments become nil gap sentinels so the consumer never wedges on an index that is not coming

**VOD direct download mode (`runDirectDownload`):**
- Probes total file size with a `Range: bytes=0-0` **GET** (`probeFileSize` in `internal/engine/downloader_fetch.go` — never a HEAD; a 200 answer means the origin ignored the Range header, and its body is dropped unread rather than pulled). The probe is retried up to three times with backoff before the caller gives up on Range support (`probeFileSizeWithRetry`), so one transient failure cannot route a resumable download into the streaming fallback
- Downloads in 5MB chunks (`DownloadChunkSize`) using Range requests
- Per-chunk retry (up to 3 attempts, `MaxChunkRetries`)
- Falls back to streaming download if server doesn't support Range — one response for the whole file, bounded by the read-progress deadline alone
- Saves a resume sidecar every 50 MB (`directResumeInterval` in `internal/engine/downloader_direct.go`) on both the chunked and the streaming path; before Arc E the whole-file path wrote none at all, so an interrupted VOD restarted from byte 0 however far it had got
- Progress reported as percentage

**Resume capability:**
- `.resume.json` file stores: `lastSeq`, `bytesWritten`, `timestamp`, `baseUrl`, `streamId`
- On resume: validates IDENTITY via `resumeIdentityMismatch` (explicit StreamID first, then YouTube URL fingerprinting; opaque no-identity URLs are trusted — see data-and-storage.md), then file size vs saved bytes
- DB-level fallback: if resume file is lost but database has `last_video_seq`/`last_audio_seq`, uses file size as byte position
- The media file is fsync'd before every sidecar save (`syncMediaFile` in `internal/engine/downloader_resume.go`) and the sidecar itself is written fsync+rename, so neither durable position can lead the durable bytes after a power loss; a failed media fsync skips that one save rather than recording a position it cannot back
- With a usable sidecar the file is truncated to the saved byte position and the missing tail appended. With NO usable sidecar the engine never truncates non-empty staged media: `StopOnGap` callers (Twitch live) get `ErrGapDetected` and gap-split instead, and every other caller gets `ErrStagedMediaPresent` so the orchestrator decides. One deliberate exemption: a whole-file VOD download (`IsDirectURL`) is out of the guard's scope — its partial is not segmented staged media and is always re-fetchable from the same static URL, so restarting it costs bandwidth rather than footage (and the 50 MB sidecar cadence above bounds even that)
- The one caller that genuinely requires a file starting at sequence 0 — the manifest-free DASH restart, via `DiscardStaged` — cannot refuse, so it preserves instead: `preserveStagedRecording` in `internal/engine/downloader.go` renames the headed recording ASIDE as `<file>.restart-<unix ts>` (its sidecar follows as `.restart-<unix ts>.resume.json`) and opens the fresh file beside it. Only bytes carrying no container header are discarded
- An aside is recovered at finalize and never merged: it overlaps the fresh recording from sequence 0, so `muxStagedAsides` in `internal/worker/orchestrator_mux.go` muxes each restart's GROUP of asides into its own sibling file beside the archive (`{name}.restart-<ts>.mp4`) and records no segment row. One group per (staging dir, timestamp): a DASH restart sets aside the video and audio halves together under the same second, and `groupStagedAsides` recombines that pair into a single file rather than two. Until it is muxed it counts as an unmuxed part, which keeps the whole staging dir from being swept, and the orphan sweep (`internal/worker/orphans.go`) lists any it finds under that dir's asides. Neither shield ages an aside out — unlike the tail and chat keeps beside them, which expire on `incomplete_staging_expiry_days` — because finalize muxes every READABLE aside, so one still in staging is footage FFmpeg could not read and that exists nowhere else

**Key constants:**
| Constant | Value | Purpose |
|----------|-------|---------|
| `CatchupThreshold` | 10 | Segments behind `stayBehindSegments` (30) before parallel catch-up engages |
| `MaxSegmentRetries` | 5 | Per-segment retry limit |
| `ParallelDownloads` | 6 | Fallback catch-up worker count when a caller leaves `DownloaderOptions.SegmentWorkers` unset (0). Live downloads are always given the operator's `downloader.segment_workers` (default 12) by the worker layer, so this constant is effectively only a test/library default now. |
| `SegmentTimeout` | 30s | Read-progress (idle) deadline on one segment/chunk fetch — cancelled only after this long with NO bytes arriving, so a slow-but-moving transfer runs as long as it keeps progressing. Also the whole-file streaming fallback's only bound |
| `segmentHardCeiling` | 15min | Absolute lifetime of one segment/chunk fetch, layered under the idle deadline so a body trickling just fast enough to keep resetting it still ends. Bounds `fetchSegment`/`fetchChunk` only |
| `DefaultMaxTimeout` | 10min | Fallback for `maximum_timeout` (force-finalize when no segment arrives for this long, even if YouTube reports live) |
| `streamStatusCheckInterval` | 30s | No-segment gap that triggers a stream-status check (re-checked at most once per interval) |
| `HeadProbeInterval` | 5s | Interval for HEAD probes to discover head seq |
| `ResumeSeqInterval` | 50 | Save resume state every N sequential segments |
| `ResumeCatchupInterval` | 10 | Save resume state every N catch-up segments |
| `DownloadChunkSize` | 5MB | Chunk size for VOD Range requests |
| `MaxChunkRetries` | 3 | Per-chunk retry limit for VOD |
| `ProgressThrottle` | 500ms | Throttle VOD progress emission |
| `DefaultRetryDelayCap` | 60s | Max retry delay (exponential backoff cap) |

Every retry and backoff wait the live loops sleep on is a field of the unexported `delays` struct (`internal/engine/delays.go`), defaulting to the named constants and pinned by `TestDefaultDelaysMatchConstants` (the first-segment hunt, the direct-download backoffs and the eviction probe keep their own constants); the engine tests poke `fastDelays()` (÷20) so `go test ./internal/engine/` runs in seconds while production timing is untouched.

#### Catch-up: rolling window and byte-bounded buffer

Parallel catch-up (`runParallelCatchUp`, `internal/engine/downloader_parallel.go`) is a rolling window, not per-batch barriers: workers claim sequences continuously off a shared cursor and completed segments flush to disk in strict ascending order as they arrive, rather than waiting for an entire batch of `segment_workers` fetches to land before the next batch starts. The prior per-batch design paid a full HEAD-probe round trip between every batch; the synthetic-latency harness for this path (`TestCatchUpRollingWindowThroughput`, `downloader_parallel_test.go`) measured a batched run at 4.888s dropping to 0.84s for the same segment count under the rolling window.

- **Claim window width** (`maxCatchupBatch`, `catchUpBatchLimit`) — normally `8 * segmentWorkers()`, so a wider configured pool gets a proportionally deeper pipeline instead of starving against a fixed ceiling (the pre-`segment_workers` code had this fixed at 48, i.e. `8 * ParallelDownloads`). After a failure episode (`noteCatchUpFailureEpisode`, fired on 403/permanent-error bursts) the window damps to a floor of one full parallel wave — `segmentWorkers()` — and regrows by one segment per `catchUpRegrowInterval` (1s) back to the full width. The floor and 1s interval were retuned 2026-08-15 after field evidence: the original floor of 1 segment regrowing at 10s (copied from moonarchive's heartbeat-driven damping) collapsed real catch-up throughput to 1-3 segments wide for the whole duration of a 403 storm, because refresh-and-retry (see "Mid-job re-mint" in platform-services.md) already answers 403 bursts directly — the hard damp no longer needed to shoulder that job alone.
- **Reorder buffer ceiling** (`catchUpBufferBytes`, 256 MB) — bounds the buffer by bytes rather than segment count, because a wider `segment_workers` pool would otherwise scale buffered memory with a throughput setting (at 1080p60's 3.7-6.2 MB segments, sixteen workers on a count-bounded buffer held ~250 MB regardless of the count chosen). A worker blocks rather than buffering past the ceiling; the head-of-window segment is always admitted so the flush position can never deadlock; segments above the lowest known-failed sequence are dropped rather than held, since they cannot flush until that failure resolves. Resident memory is therefore roughly ceiling + `segment_workers` × segment size, not a function of the claim window width.
- **Head freshness** — no longer solely the per-batch HEAD probe (`HeadProbeInterval`, 5s). Every segment response's `X-Head-Seqnum` is harvested opportunistically to update the known head, since the rolling window has no natural per-batch checkpoint to probe from; the interval probe still runs as a backstop.
- **HTTP transport ceiling** (`engineMaxIdleConnsPerHost`, 64, `downloader_fetch.go`) is a fixed idle-connection-pool size per host, not derived from `segment_workers`, because the HTTP client is a package-level singleton built before any worker count is known. A configured `segment_workers` comfortably above this (past `config.SegmentWorkersWarnThreshold`, 16) degrades to per-segment TCP+TLS handshakes instead of connection reuse rather than failing outright — the first place to look if throughput stops scaling with a higher `segment_workers` value.

### QualityMonitor

The `QualityMonitor` runs alongside a live stream download and detects resolution changes:

- Probes every 30 seconds (`qualityMonitorInterval`) via a format re-fetch
- Compares width and height against the current baseline
- FPS-only changes are intentionally ignored (splitting for framerate alone adds overhead without saving storage)
- Sends new `QualityInfo` to a channel when a change is detected
- Thread-safe baseline updates via mutex
- Probe errors are logged and skipped (never trigger false positives)

When a quality change is detected and the current segment has been running for at least 10 seconds, the orchestrator:
1. Cancels current downloaders
2. Muxes the current segment in a goroutine parented by the orchestrator's mux root (`muxRoot` in `internal/worker/quality_split_common.go`), not `context.Background()`
3. Records the segment in the `segments` database table
4. Re-fetches video info and creates new downloaders at the new quality
5. Updates the monitor baseline to the new quality

### JobQueue

The `JobQueue` implements a two-tier concurrency model:

**Lifecycle tier (100 slots):**
- Gates how many jobs can be in the DOWNLOAD half of the pipeline simultaneously — downloading plus muxing, which is exactly what `LifecycleCount()` reports
- The slot is claimed at the download decision, not at dequeue: `processJob` calls `AcquireLifecycleSlot` in `internal/worker/queue.go` only once `StreamProcessor.Process` has answered "should download", and every exit path from there releases it through the deferred `Complete` (owner decision O-F)
- Stream probing and the wait for a stream to go live therefore run slot-free. They used to hold a slot for the whole wait — hours to days for an `Upcoming` job or a manually-added offline Twitch channel — and at 100 waiters a newly live stream was never started at all
- A wait that outlasts `lifecycleWaitWarnAfter` in `internal/worker/queue.go` (30 s) logs one line, once per wait, naming the job and the slots held: at the cap the symptom is a capture that simply does not start, and until that line existed nothing explained it

**Download tier (configurable, default 10 slots):**
- Gates how many VOD jobs can be actively downloading segments in parallel — VODs ONLY. Broadcasts pass through ungated (`acquireDownloadSlot` with `isVod=false` is a no-op): a missed slot on a VOD delays a file that already exists, a missed slot on a live broadcast loses footage. Peak concurrent downloads is therefore (live broadcasts) + `num_parallel_downloads`
- **Not the same knob as `downloader.segment_workers`.** `num_parallel_downloads` gates concurrent VOD **jobs**; `segment_workers` (default 12, no upper limit — see the constants table under SegmentDownloader below) gates concurrent **segment fetches within one download**, live or VOD. Because broadcasts bypass the download tier entirely, `num_parallel_downloads` has zero effect on a live stream's catch-up rate — confusing the two cost real debugging time when the owner had it set to 1000 with no observable change to catch-up throughput. Segment-level concurrency is what `segment_workers` controls.
- A VOD job acquires a download slot via `AcquireDownloadSlot()` after stream processing
- Released via `ReleaseDownloadSlot()` after download completes but before muxing
- This allows muxing to proceed without blocking download slots (muxing is CPU-bound, not network-bound)

**Priority system:**
- `Live` = 1 (highest priority)
- `Upcoming` / `Downloading` = 0
- `Error` = -1 (lowest, for retries)
- FIFO among jobs with equal priority

**Queue operations:**
- `Enqueue(jobID, status)`: Adds to pending queue. O(1) duplicate detection via `pendingSet`. Backlog limit of 100 pending jobs; drops with warning if full.
- `Dequeue(ctx) -> (jobID, jobCtx, ok)`: Blocks until a pending job exists — there is no lifecycle gate here. Returns a per-job cancellable context.
- `AcquireLifecycleSlot(ctx, jobID) -> bool`: Blocks until one of the 100 lifecycle slots is free, then claims it for the job. Returns false if context cancelled. Warns once per wait past 30 s.
- `AcquireDownloadSlot(ctx, jobID) -> bool`: Blocks until a download slot is free. Returns false if context cancelled.
- `ReleaseDownloadSlot(jobID)`: Frees the download slot. Signals waiting jobs.
- `Complete(jobID)`: Frees lifecycle slot and download slot (if held). Cancels the per-job context.
- `Cancel(jobID)`: User-initiated cancellation. Sets `cancelled` flag, cancels context, removes from pending queue.
- `WasCancelled(jobID) -> bool`: Returns and clears the cancellation flag. Used to distinguish user cancellation from shutdown.

**Signaling:**
- `notify` channel (capacity 1): signals that a pending job is available
- `lifeNotify` channel (capacity 1): signals that a lifecycle slot is free
- `dlNotify` channel (capacity 1): signals that a download slot is available
- All three use non-blocking sends to avoid producer blocking. On the two SLOT channels (`lifeNotify`, `dlNotify`) each successful acquirer re-signals while capacity remains, so a burst of releases collapsing into one signal cannot leave a waiter asleep beside a free slot. `notify` needs no such cascade: `Dequeue` re-checks the pending queue under the lock every time it is called and only parks when the queue is empty, so a coalesced signal costs nothing

### Backlog Scheduler

The worker-owned `Scheduler` (`internal/worker/scheduler.go`) admits backlog (`Queued`) jobs at most `archive_slots` at a time per channel — spec §10's archive-slots pacing. It is the only path out of `Queued`: `ShouldProcess(Queued)` is false by design, so neither startup recovery nor the worker's heartbeat poller ever touches a `Queued` row.

- Single goroutine, woken by `Wake()` (backlog-job creation, job completion, a cookie repair that returns parked backlog to `Queued`) or the worker's 60s heartbeat; wake signals coalesce through a capacity-1 channel
- One admission sweep per wake: for each channel with `Queued` rows, admit `archive_slots − in-flight backlog jobs`, newest `published` first
- Admission writes `status = Upcoming` durably FIRST (the in-flight count observes the DB), then enqueues in the JobQueue — a crash between the two steps self-heals because startup recovery re-enqueues `Upcoming` rows
- `resolveSlots` is injected by `cmd/moombox` against the live config store, so per-channel `archive_slots` overrides hot-reload
- A **disabled** channel resolves to 0 slots, so disabling it PAUSES its queued backlog (owner decision O-J, 2026-09-17). Every discovery path already reads `enabled = false` as a pause — the feed, DECAPI and Twitch monitors skip the channel, and the backfill keeps it in `active` while never scanning it — and the resolver was the one place that did not, so a disabled channel went on starting downloads M at a time. In-flight jobs are untouched: they have already left `Queued`, and this number is an admission budget rather than a kill switch. A channel with **no config entry at all** still gets the global default, so a removed channel's leftover `Queued` rows are not stranded.
- `Run` performs one admission sweep before entering its wait: the wake sites are all event-driven — backlog creation, job completion, a cookie repair — and a restart has none of them, so leftover `Queued` rows used to wait for the 60 s heartbeat. The sweep is gated to the first pass of `Run`, so a `sweep()` that panics deterministically restarts on the heartbeat cadence rather than every second

### ProgressTracker

The `ProgressTracker` aggregates progress from video, audio, and chat downloaders and persists to the database:

- Update throttling: 16ms callback rate (matching TUI's ~60fps tick), 1-second database persist interval
- Progress string format: `"V:1234 A:5678 C:900"` (video seq, audio seq, chat messages)
- Speed calculation: smoothed exponential average (factor 0.7) of bytes/second
- ETA calculation: based on elapsed time and progress percentage
- VOD progress: percentage-based (from chunked download byte position)
- Gap tracking: accumulates `DownloadGap` events from segment downloaders

## Concurrency Model

### Worker Concurrency

The download worker uses a goroutine-per-job model:

```
DownloadWorker.Start(ctx):
    for {
        jobID, jobCtx := queue.Dequeue(ctx)     // blocks until a job is pending
        go processJob(jobCtx, jobID)              // one goroutine per job
    }
```

Each `processJob` goroutine:
1. Has panic recovery (`defer func() { if r := recover() }()`)
2. Calls `queue.Complete(jobID)` on exit (deferred)
3. Tracks in-flight count via `sync.WaitGroup`
4. Is cancellable via the per-job context from `Dequeue()`

The `pollForJobs` goroutine runs a safety-net check every 60 seconds to catch any missed jobs. Normal job discovery is signal-driven via `EnqueueJob()` calls.

### Database Batch Coalescing

Rapid job updates (progress, sequence numbers, etc.) are coalesced into batched writes:

Two update mechanisms serve different needs:

**`UpdateJob()` — batched via channel (full job writes):**
1. Writes the `Job` object to `updateCh` (buffered channel, capacity 100), non-blocking
2. If channel is full, falls back to synchronous direct write
3. `batchUpdateLoop()` goroutine:
   - Blocks on `updateCh` until first item arrives (zero CPU when idle)
   - Starts a 100ms coalesce timer
   - Accumulates updates in a `map[string]*Job` (latest update per job wins)
   - On timer fire: opens a transaction, writes all pending updates, commits
   - Notifies `OnJobUpdate` subscribers for each successfully persisted job
4. On database close: channel is closed, remaining items are flushed

**`UpdateJobFields()` — synchronous direct writes (partial updates):**
1. Executes SQL immediately under `db.mu` lock (no channel, no batching)
2. Builds dynamic SET clause from `fieldToColumn` map
3. Re-reads the full job row after write (subscribers need all fields)
4. Notifies `OnJobUpdate` subscribers synchronously

This pattern reduces SQLite write transactions from potentially hundreds per second (during active downloads) to approximately 10 per second.

### WebSocket Broadcast Rate

The WebSocket hub throttles nothing. The highest-frequency caller (`OnJobChange` driven by `ProgressTracker.maybeUpdate`, capped to ~60 Hz per job by `progressUpdateInterval = 16ms`) now broadcasts the slim `job_progress` frame; `job_update` carries the state transitions, and `OnJobAdded`/`OnTrimsChanged` are event-driven. A previous per-job throttle in the hub created an ordering race where the trailing edge could arrive after a `BroadcastJobDeleted` and resurrect a deleted row via the client's upsert handler.

### TUI Async Updates

The TUI uses non-blocking channel sends to prevent the event loop from blocking:

- Job updates, log lines, and status changes are sent via buffered channels
- If a channel is full, the send is dropped and a drop counter is incremented
- Drop counters are logged periodically but are non-fatal
- Key timing intervals:
  - Main tick: 1 second
  - Progress tick (active download): 16ms (~60fps)
  - Progress tick (idle): 500ms
  - Marquee animation: 150ms
  - Log flush window: 250ms, ring buffer max 200 lines

### BotGuard Triple Cache

The PO token system uses three in-process cache layers to minimize expensive BotGuard operations. Caches are agnostic to which path (sidecar primary, goja fallback) produced the token:

1. **Session cache (6h TTL):** Caches the complete PO token for a given content binding. Avoids both the sidecar IPC and the goja flow entirely on hits. Single source of truth for "is this token still fresh."
2. **Minter cache (dynamic TTL, single-minter design):** Caches the compiled BotGuard "minter" which can stamp multiple tokens. TTL comes from the BotGuard challenge response. Effectively unused under sidecar mode (the sidecar maintains its own internal minter cache inside the Node process). Populated only when the sidecar fails and the goja path generates a minter. CRIT-2 audit fix: ONE minter under `defaultMinterKey="default"` serves every binding for its TTL — the per-binding cache that pre-existed wasted a BotGuard run on every new content binding. FRESH-2 audit fix: a `time.AfterFunc` 5min before expiry proactively regenerates so user-facing calls don't pay the 2-10s BotGuard cost.
3. **Inflight dedup:** When multiple goroutines request a PO token simultaneously, the first one starts generation and all others wait on the same channel. Prevents thundering herd.

Each cache layer uses `time.AfterFunc` for automatic eviction on TTL expiry. The session cache key is the content binding string. The minter cache key is the challenge hash.

### Cipher 10-VM LRU

YouTube signature decryption requires executing JavaScript from `player.js`. This is expensive (multi-MB Goja VM):

- Maximum 10 VMs cached simultaneously (`solverCacheSize`; ~30-50 MB each, so ~500 MB worst case)
- Keyed by `player.js` URL (changes when YouTube deploys new player versions)
- Mutex-serialized compilation: only one goroutine compiles a given player.js at a time, others wait
- Extraction: full AST parsing of the JavaScript to find cipher functions, with regex fallback if AST fails
- VMs are evicted LRU when the cache is full

## Panic Recovery Patterns

Every goroutine in the codebase must have panic recovery. The patterns used vary by context:

**General goroutine pattern:**
```go
go func() {
    defer func() {
        if r := recover(); r != nil {
            logger.Error("panic in <context>", "panic", fmt.Sprint(r))
        }
    }()
    // ... work ...
}()
```

**HTTP middleware (`RecoveryMiddleware`):**
- Catches panics in HTTP handlers
- Returns HTTP 500 with a JSON error body
- Logs the panic with stack trace

**Database subscriber callbacks:**
- `safeCallJobUpdate(fn, job)` wraps each `OnJobUpdate` subscriber call
- `safeCallJobsChange(fn, jobs)` wraps each `OnJobsChange` subscriber call
- A panic in one subscriber does not prevent other subscribers from being notified

**Monitor callbacks:**
- `OnVideoFound` and `OnStreamFound` callbacks in `main.go` are wrapped with `defer func() { if r := recover() }()`
- Prevents a bug in job creation from crashing the monitor's polling goroutine

**Background mux goroutines:**
- Quality- and gap-split segment muxing runs under the orchestrator's mux root, not `context.Background()`
- Each has its own panic recovery
- The root outlives the JOB's context, so a user cancel never orphans a half-written output; but `Stop` cancels the root itself (`CancelMuxes` in `internal/worker/quality_split_common.go`) once its 10-second wait for in-flight jobs runs out, so a shutdown kills FFmpeg rather than leaving it writing into a staging dir the restarted child re-muxes with `-y`. A mux cancelled that way leaves its row `Muxing` with staging intact, and the next start re-muxes it

**Download worker:**
- Each `processJob` goroutine has panic recovery
- On panic: sets job status to `Error` with message `"internal panic: <details>"`

## Job Status Lifecycle

```
Queued (backlog VODs only)
    |  admitted by the archive-slots scheduler
    v
Upcoming -----> Live ------> Downloading ------> Muxing ------> Finished
    |              |              |                  |
    |              |              |                  |
    v              v              v                  v
  Error          Error          Error              Error
  Cancelled      Cancelled      Cancelled          Cancelled
                                COOKIES?
                                   |
                                   +--(cookie repair, priority 0)--------> Upcoming
                                   +--(cookie repair, backlog, priority 1)-> Queued
```

### Status Definitions

| Status | Type | Meaning |
|--------|------|---------|
| `Queued` | `"Queued"` | Backlog VOD resting state: waiting for one of the channel's `archive_slots`. Only the scheduler moves it forward — `ShouldProcess` is false, so startup recovery and the heartbeat poller skip it. |
| `Upcoming` | `"Upcoming"` | Stream is scheduled but not yet live. StreamProcessor is polling. |
| `Live` | `"Live"` | Stream is confirmed live. About to download or actively downloading. |
| `Downloading` | `"Downloading"` | Actively downloading segments. |
| `Muxing` | `"Muxing"` | Download complete, FFmpeg is combining tracks. |
| `Finished` | `"Finished"` | Terminal. Output file is ready. |
| `Error` | `"Error"` | Terminal. Something went wrong. Error message in `error` field. |
| `Cancelled` | `"Cancelled"` | Terminal. User explicitly cancelled. |
| `COOKIES?` | `"COOKIES?"` | Terminal (but retriable). Authentication required -- cookies expired or members-only. The `park_reason` column records which, and gates automatic resume (see below). |

### Transition Rules

- `Queued` -> `Upcoming`: Scheduler admitted the backlog VOD into one of its channel's `archive_slots` (backlog jobs only — broadcasts and newly discovered VODs are created directly as `Upcoming`/`Live`)
- `Upcoming` -> `Live`: Stream went live (detected by probe)
- `Upcoming` -> `Downloading`: Stream is a VOD or non-stream allowed for download
- `Live` -> `Downloading`: Download slot acquired, segments being fetched
- `Downloading` -> `Muxing`: All segments downloaded, FFmpeg muxing started
- `Muxing` -> `Finished`: Mux complete, output file written
- Any -> `Error`: Unrecoverable error at any stage
- Any -> `Cancelled`: User-initiated cancellation
- `Downloading` -> `COOKIES?`: Auth failure detected (login required, members-only, cookies expired)
- `COOKIES?` -> `Upcoming` (priority 0) / `COOKIES?` -> `Queued` (priority 1, backlog, **when its `feed_items` row still exists**): a credential-recovery sweep resumed the job. A backlog row returns to `Queued` rather than `Upcoming` so the archive-slots scheduler re-admits it `archive_slots` at a time instead of releasing a channel's whole parked backlog at once. A parked backlog job of a REMOVED channel returns to `Upcoming` so it can finish at all: `CancelAndPrune` deletes the channel's `feed_items` rows but deliberately leaves a running download alone, and `NextQueuedJobs` INNER-JOINs `feed_items`, so a partnerless `Queued` row would never be admitted and `Queued` has no other exit (`/retry` and `/resume` both refuse it). Two sweeps exist and they are not interchangeable:
  - **Auth recovered** (`RefreshService.OnAuthRecovered`) fires when a platform goes from not-authenticated to authenticated, and offers the sweep no account identity. It resumes every park EXCEPT `park_reason = 'membership'`.
  - **Credential observation** (`RefreshService.OnCredentialsChanged`, both platforms — only YouTube can park a job on an account question) hands the sweep the account identity currently in the cookie file. A membership park then resumes if and only if that identity differs from the `park_identity` it recorded when it was refused.
  A membership park is excluded from the first sweep because it happened while the session was ALREADY authenticated, so that transition cannot be the event that fixes it -- resuming there bought a guaranteed-identical failure once per auth cycle. Only a different account can help.
- **Account identity** is `SHA-256(SAPISID || NUL || LOGIN_INFO)` (`cookies.CookieJar.YouTubeIdentity`). `LOGIN_INFO` is the load-bearing half: SAPISID identifies a Google *session*, not an account, which is why `internal/youtube/auth.go` must select the account separately via `X-Goog-AuthUser`/`X-Goog-PageId`. Switching the browser's active account -- the exact remedy Moombox prints for a not-a-member failure -- rewrites `LOGIN_INFO` and leaves SAPISID untouched, so a SAPISID-only fingerprint would be blind to it.
- **The resume decision is durable, not edge-triggered.** The per-job `park_identity` comparison is what moves a job; the observation callback only decides *when* to look. A missed observation therefore costs a delay until the next account change or restart, never a permanent strand. Its process-local baseline advances only on a check that both concluded AND authenticated -- otherwise a stale intermediate export (dead on arrival, then re-exported working) would consume the edge -- and its zero value fires once per process, which is how an offline swap (stop, replace cookies, start) is noticed at all.

### Cancellation Semantics

- **User cancellation:** `WasCancelled(jobID)` returns true. Status set to `Cancelled`. Notification sent.
- **Shutdown cancellation:** `WasCancelled(jobID)` returns false. Status is preserved (not changed). Job will resume on next startup.

This distinction is critical: on shutdown, jobs in `Downloading` status keep that status so they are re-enqueued on restart. User cancellations are permanent.

### Terminal Status Check

```go
func isTerminalStatus(status database.JobStatus) bool {
    switch status {
    case database.StatusFinished, database.StatusError,
         database.StatusCancelled:
        return true
    default:
        return false
    }
}
```

Note: `Muxing` is **not** terminal. If muxing was interrupted by shutdown (the
muxer process was killed mid-encode), `enqueueExistingJobs` in
`internal/worker/worker.go` re-muxes the job from what is already staged —
`MuxJob` hands it to the Mux action's own path, `muxFromStaging` in
`internal/worker/orchestrator_mux.go`, under the same download slot a queued
job takes, and reclaims staging once the archive file exists. Two rows are the
exception and still reset to `Downloading`: one whose staging holds nothing
muxable, and one flagged `incomplete_tail`, whose recording is known to be
short and still needs the post-live VOD-refresh loop that only the download
path runs.

Muxing is NOT idempotent against the recording itself, which is what the
previous reset assumed: re-processing re-probed the stream as post-live, the
manifest-free strategy seeded sequence 0, and the engine truncated the
complete staged file. The engine now refuses that outright —
`ErrStagedMediaPresent` in `internal/engine/downloader.go` — so no path
truncates non-empty staged media unless the caller explicitly asked to
discard it, and the one caller that does (the manifest-free restart's
`DiscardStaged`) preserves the recording aside instead. A whole-file VOD
download (`IsDirectURL`) is outside the guard by design: its partial is
re-fetchable from the same static URL, so a restart costs bandwidth, not
footage.

## Error Hierarchy

All errors in Moombox extend `MoomboxError`, which provides:

```go
type MoomboxError struct {
    Code     string                 // Machine-readable error code
    Message  string                 // Human-readable description
    Expected bool                   // true = user-facing, false = internal/developer
    Context  map[string]interface{} // Additional structured data
    Cause    error                  // Wrapped underlying error
}
```

### Error Types

| Type | Base | Extra Fields | Purpose |
|------|------|--------------|---------|
| `MoomboxError` | -- | Code, Message, Expected, Context, Cause | Base error |
| `YouTubeError` | MoomboxError | -- | YouTube API errors |
| `DownloadError` | MoomboxError | HTTPStatus | Segment/manifest/resume failures |
| `NetworkError` | MoomboxError | HTTPStatus, URL | HTTP/DNS/TLS/connection errors |
| `ConfigError` | MoomboxError | -- | Invalid configuration (always Expected=true) |
| `MuxingError` | MoomboxError | ExitCode | FFmpeg failures |
| `AuthError` | MoomboxError | -- | Cookie/token authentication errors (always Expected=true) |
| `VideoPlayabilityError` | MoomboxError | PlayabilityStatus, Reason | Members-only, age-restricted, geo-blocked, copyright (always Expected=true) |

### Error Codes

**YouTube:** `LOGIN_REQUIRED`, `UNPLAYABLE`, `LIVE_NOT_STARTED`, `NOT_A_STREAM`, `MEMBERS_ONLY`, `AGE_RESTRICTED`, `PRIVATE`, `COPYRIGHT`, `GEO_RESTRICTED`, `STREAM_ENDED`

**Download:** `SEGMENT_FAILED`, `MANIFEST_FAILED`, `FORMAT_NOT_FOUND`, `RESUME_CORRUPTED`, `DISK_FULL`, `TIMEOUT`

**Network:** `HTTP_FAILED`, `DNS_FAILED`, `CONNECTION_RESET`, `TLS_FAILED`

**Muxing:** `FFMPEG_NOT_FOUND`, `FFMPEG_FAILED`, `INVALID_INPUT`

**Auth:** `COOKIES_EXPIRED`, `COOKIES_INVALID`, `TOKEN_EXPIRED`

### Expected vs Unexpected

- `Expected=true`: User-facing errors that the user can potentially act on (fix cookies, change config, wait for geo-restriction to lift). Displayed prominently in the UI.
- `Expected=false`: Internal/developer errors (bugs, unexpected API changes). Logged at Error level with full context.

Helper function `IsExpected(err)` checks all error types. `IsLoginRequired(err)` specifically detects auth-related errors for cookie refresh triggering.

### Error-to-Status Mapping

In `DownloadWorker.setJobError()`:
```
"login required" or "member-only" or "members only" or "cookies?" -> StatusCookies
all other errors -> StatusError
```

Notification suppression for non-actionable errors:
- `"age restricted"` -> suppressed (nothing user can do)
- `"max probe errors"` -> suppressed (transient, stream may have ended naturally)

## Key Types and Public API

### database.Job

The primary data model. See `internal/database/types.go` for the complete struct. Key fields:

- `ID` (string): Primary key. For YouTube: video ID. For Twitch: `tw_{streamID}` or `tw_manual_{login}_{timestamp}`.
- `VideoID` (string): Platform-specific video identifier.
- `Platform` (string): `"youtube"` or `"twitch"`.
- `Status` (JobStatus): Current lifecycle status.
- `Progress` (string): Human-readable progress (e.g., `"V:1234 A:5678 C:900"`).
- `Percent` (float64): 0-100 for VOD downloads.
- `IsVod` (bool): True for VOD/non-live downloads.
- `ManuallyAdded` (bool): True if added via CLI or UI (vs. monitor discovery).
- `AllowNonStream` (bool): True if non-stream content should be downloaded as VOD.
- `SelectedVideoItag` / `SelectedAudioItag` (*int): Manual format override. -1 = skip track.
- `StartTime` / `EndTime` (*float64): Post-download trim boundaries in seconds.
- `QualityPreference` (string): e.g., `"1080p60"`, `"720p"`, `"best"`, `"audio_only"`.
- `Gaps` ([]Gap): Missing segment ranges detected during download.
- `Segments` ([]Segment): Part records for multi-part downloads — quality splits (both platforms) and Twitch live gap splits. Each row carries the part's video path and (Twitch) its per-part chat file.
- `Trims` ([]TrimRecord): Clips created from this job.

### database.Database

Key methods:
- `Open(dbPath, logger) -> (*Database, error)`: Opens/creates database, runs migrations, starts batch loop.
- `AddJob(job) -> (bool, error)`: INSERT OR IGNORE. Returns false if duplicate.
- `GetJob(id) -> (*Job, error)`: Single job with gaps, trims, segments loaded.
- `GetAllJobs() -> ([]*Job, error)`: All jobs ordered by `updated_at DESC`.
- `UpdateJobFields(jobID, map[string]any)`: Dynamic partial update with auto `updated_at`. Triggers subscribers.
- `DeleteJob(id) -> error`: Hard delete with cascading gap/trim/segment cleanup.
- `OnJobUpdate(fn) -> unsubscribe`: Subscribe to per-job update events.
- `OnJobsChange(fn) -> unsubscribe`: Subscribe to job list change events (add/delete).
- `AddToHistory(videoID)`: Records video ID to prevent re-downloading.
- `IsInHistory(videoID) -> bool`: Checks if video was previously downloaded.
- `JobExists(id) -> bool`: O(1) existence check.

### worker.DownloadWorker

Key methods:
- `NewDownloadWorker(db, yt, cfg, logger, deps) -> *DownloadWorker`: Constructor.
- `Start(ctx)`: Main loop. Blocks (run in goroutine). Enqueues existing pending jobs, then dequeues and processes.
- `Stop()`: Signals stop, waits up to 10 seconds for in-flight jobs.
- `EnqueueJob(jobID)`: Adds a job to the queue with priority from its current status.
- `CancelJob(jobID)`: User-initiated cancellation.
- `SetParallelDownloads(n)`: Runtime update of max concurrent downloads.

### worker.StreamProcessor

Key methods:
- `NewStreamProcessor(yt, tw, cfg, db, logger) -> *StreamProcessor`: Constructor.
- `Process(ctx, job) -> (*StreamProcessResult, error)`: Main entry point. Routes to YouTube or Twitch path.
- `SetNotifier(nm)`: Wire notification manager.
- `Stop()`: Gracefully stops active chat downloaders.

### worker.DownloadOrchestrator

Key methods:
- `NewDownloadOrchestrator(db, queue, ffmpegPath, logger, cs, pp, nm) -> *DownloadOrchestrator`: Constructor.
- `Execute(ctx, jobCtx, videoInfo, isVod) -> error`: YouTube download without pre-existing chat.
- `ExecuteWithChat(ctx, jobCtx, videoInfo, isVod, existingChat) -> error`: Full YouTube download pipeline.
- `ExecuteTwitch(ctx, jobCtx, variant, isVod, twitchChat) -> error`: Full Twitch download pipeline. For live streams this is a *session loop*: the engine runs with `StopOnGap` (Twitch has no DVR), so an unrecoverable playlist gap muxes the current capture as a finished part (`{name} - partN.mp4` + rolled per-part chat) and continues at the live edge in a new `seg_N` staging dir; a connectivity outage pauses the session and resumes the SAME job once the same broadcast (stream_start_time identity, rechecked post-outage) is reachable again — one job per broadcast. On daemon restart, `discoverResumeSegment` maps staged `seg_N` dirs + recorded segment rows to the correct part so a resume never appends into an already-muxed part's staging file. Seamless continuation (sequence numbers still covered by the playlist window) keeps appending — splits happen only where data was actually lost. Both TS and fMP4/CMAF delivery are handled: on an fMP4 playlist the engine writes the `#EXT-X-MAP` init segment at the head of each part file before its first fragment (`ensureHlsInit`), tolerates token-rotated init URIs by content hash, and returns `ErrInitSegmentChanged` when the init genuinely changes mid-part (transcode restart) — handled like a gap split minus the lost-data notification, since the successor part re-requests the exact segment the engine refused to write.

### worker.JobQueue

Key methods:
- `NewJobQueue(maxDownloads) -> *JobQueue`: Constructor (maxLifecycle fixed at 100).
- `Enqueue(jobID, status)`: Non-blocking add to pending queue.
- `Dequeue(ctx) -> (string, context.Context, bool)`: Blocking dequeue of the highest-priority pending job. No lifecycle gate.
- `AcquireLifecycleSlot(ctx, jobID) -> bool`: Blocking lifecycle-slot acquisition, taken at the download decision.
- `AcquireDownloadSlot(ctx, jobID) -> bool`: Blocking download slot acquisition.
- `ReleaseDownloadSlot(jobID)`: Non-blocking slot release.
- `Complete(jobID)`: Free all slots, cancel context.
- `Cancel(jobID)`: User cancellation.
- `WasCancelled(jobID) -> bool`: Check and clear cancellation flag.
- `SetMaxDownloads(n)`: Runtime update.
- `ActiveCount() -> int`: Current download slots in use.
- `LifecycleCount() -> int`: Jobs holding a lifecycle slot — downloading + muxing.
- `PendingCount() -> int`: Jobs waiting in queue.
- `IsProcessing(jobID) -> bool`: Check if job is active.

### engine.SegmentDownloader

Key methods:
- `NewSegmentDownloader(opts) -> *SegmentDownloader`: Constructor.
- `Start(ctx) -> error`: Main download loop. Routes to DASH/HLS/VOD/Direct based on options.
- `Cancel()`: User-initiated cancel (atomic flag).
- `LastSeq() -> int`: Last successfully downloaded sequence number.
- `BytesWritten() -> int64`: Total bytes written (atomic, lock-free).

Callback fields:
- `OnStart func(seq int, resuming bool)`: Called when download begins.
- `OnProgress func(p DownloadProgress)`: Called per segment/chunk with progress data.
- `OnGap func(g DownloadGap)`: Called when a gap (missing segment) is detected.
- `OnFinish func()`: Called when download completes.

### monitor.FeedMonitor / DecapiMonitor / TwitchMonitor

All three monitors share a similar interface:
- `Start(ctx)`: Begin monitoring loop.
- `Stop()`: Cancel and clean up.
- `CheckNow()`: Trigger an immediate check (used when channels change).
- `GetNextCheckAt() -> int64`: Next scheduled check in epoch milliseconds.

Callback fields:
- `OnVideoFound func(videoID, title, url string, channel *config.ChannelConfig)` (Feed/DECAPI)
- `OnStreamFound func(info *twitch.TwitchStreamInfo, channel *config.ChannelConfig)` (Twitch)
- `OnSchedule func(nextCheckAt int64)`: Called when next check time is determined.
- `ProbeVideo VideoProbeFunc` (Feed/DECAPI): Pre-creation metadata check to filter non-streams.

## Cross-references

- [design-philosophy.md](design-philosophy.md) -- Why these architectural patterns exist (loose coupling rationale, cross-platform build-tag approach, no-CGo constraint)
- [platform-services.md](platform-services.md) -- YouTube multi-client auth, Twitch GQL, BotGuard/PO tokens, cipher solving details
- [data-and-storage.md](data-and-storage.md) -- Database schema, migrations, config format, batch update internals
- [security.md](security.md) -- Middleware stack, CSRF, auth flow, Ed25519 update verification
- [user-interfaces.md](user-interfaces.md) -- Web UI SPA architecture, TUI chord system, WebSocket protocol
- [operations.md](operations.md) -- Build process, release workflow, runtime requirements
- [appendix-metrics.md](appendix-metrics.md) -- All timing constants, thresholds, and limits in one place

### Key Source Files

- `cmd/moombox/main.go` -- Launcher, service initialization, event wiring (~2,074 lines)
- `internal/worker/worker.go` -- DownloadWorker, processJob loop
- `internal/worker/queue.go` -- JobQueue with two-tier concurrency
- `internal/worker/stream_processor.go` -- StreamProcessor, waitForLive, Twitch processing
- `internal/worker/orchestrator.go` -- DownloadOrchestrator, strategy selection, live loop, mux
- `internal/worker/strategies.go` -- DownloadVod, DownloadDash, DownloadHls implementations
- `internal/worker/quality_monitor.go` -- QualityMonitor for live resolution tracking
- `internal/worker/quality.go` -- QualityInfo, quality label parsing
- `internal/worker/progress.go` -- ProgressTracker for throttled DB updates
- `internal/worker/trim.go` -- TrimService for clip creation
- `internal/worker/mux_finalize.go` -- Post-download file operations
- `internal/engine/downloader.go` -- SegmentDownloader (DASH/HLS/VOD/Direct modes)
- `internal/engine/muxer.go` -- FFmpeg muxer and ffprobe wrapper
- `internal/database/database.go` -- Database open, batch coalesce, CRUD operations
- `internal/database/types.go` -- Job, Gap, Segment, TrimRecord, ClientToken types
- `internal/monitor/feed.go` -- YouTube RSS feed monitor
- `internal/monitor/decapi.go` -- DECAPI latest-video monitor
- `internal/monitor/twitch.go` -- Twitch GQL stream monitor
