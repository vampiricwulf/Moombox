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
- Executes `launchAndSupervise()` in `cmd/moombox/main.go`
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
- `--version` -- Prints version and commit hash, exits immediately.
- `--headless` / `--no-tui` -- Runs web-only mode (no BubbleTea TUI). Also activated by `MOOMBOX_NO_TUI=1` env var.
- `--log-level <LEVEL>` -- Overrides the config file log level (DEBUG, INFO, WARN, ERROR).
- `--config <path>` -- Specifies config file path. Default search order: `--config` flag, `./config.toml`, `./config/`, `~/.config/moombox/`.

TTY detection uses `go-isatty` on both stdin and stdout. If either is not a terminal, TUI is disabled automatically.

## Service Initialization Order

The `run()` function in `cmd/moombox/main.go` initializes services in this exact order. The order matters because later services depend on earlier ones.

### 1. Config
Load TOML configuration via `config.Load(configPath)`. Searches: explicit `--config` flag, `./config.toml`, then standard paths. If the config file does not exist, defaults are used and the setup wizard will be triggered via the web UI.

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
`cipher.NewSolver(cacheDir, log)` creates the YouTube signature cipher solver. Cache directory is `%TEMP%/yt-cipher`. Manages a 3-VM LRU cache keyed by `player.js` URL. Wired to `ytService.PlayerAPI.SetCipherSolver()` so format URL decryption is transparent. Uses full AST parsing with regex fallback for extraction.

### 10. Notification Manager
`notifications.NewManager(cfg, log)` creates the notification dispatcher. Currently supports Discord webhooks. Sends notifications for: stream found, stream live, download starting, download finished, download error, auth required, trim created, update available.

### 11. Download Worker
`worker.NewDownloadWorker()` creates the main job processing engine. Internally creates:
- `JobQueue` with configurable max parallel VOD downloads (default 10; broadcasts are never throttled by the pool) and 100 lifecycle slots
- `Scheduler` that admits backlog (`Queued`) jobs at most `archive_slots` at a time per channel — the only path out of `Queued`
- `StreamProcessor` for probing stream status and waiting for live
- `DownloadOrchestrator` for the full download lifecycle

Dependencies injected via `DownloadWorkerDeps` struct: cipher solver, PO token provider, Twitch service, notification manager.

The `OnCookieRefreshNeeded` callback is wired to `autoCookieSvc.RefreshCookies()` for automatic cookie recovery on auth failures.

### 12. Trim Service
`worker.NewTrimService()` creates the FFmpeg-based clip creation service. Prevents concurrent trim operations on the same job via `activeOps` mutex map.

### 13. Feed Monitor (YouTube RSS + members-only)
`monitor.NewFeedMonitor()` polls YouTube RSS feeds (`https://www.youtube.com/feeds/videos.xml?channel_id=...`) for new videos. Default interval from config. Immediate first check on startup, then timer-based with jitter. When `membership_discovery` is enabled (default) and YouTube auth cookies are present, `checkChannel` additionally fetches the channel's authenticated `/membership` tab (`youtube.FetchMembershipVideos`) — the only discovery source for members-only content, which RSS/DECAPI never list. Members-only candidates are probed with the authenticated `ProbeVideoAuth` closure so a members VOD classifies correctly instead of misfiring as "upcoming". A membership fetch failure is logged but never marks the RSS feed unhealthy (independent signals).

`checkChannel` runs four steps per channel per cycle: **FETCH** (RSS + membership, independently fallible), **STORE** (upsert every listed item into the persistent `feed_items` table, tracking which IDs are new this cycle), **WALK** (a serial probe pass over the store's archive scope — everything published within `archive_window_days`, default 3 and per-channel overridable, plus ALL upcoming/live rows regardless of age — applying `HasActiveJob` dedup, term filtering, and probe-status rules, with per-source early exit once a date-ordered source falls entirely outside the window), and **ARCHIVE** (re-read the scope and decide job creation per row). ARCHIVE assigns each job a disposition: broadcasts (live/upcoming) and VODs first inserted this cycle are admitted immediately (`queue_priority` 0), while backlog VODs already known to the store are created as `Queued` (`queue_priority` 1) and paced by the worker's per-channel `archive_slots` scheduler — a backlog sweep never delays new or live content.

A companion `monitor.NewBackfillWorker()` owns the full-catalog backfill (channel add, window widening, or a manual `R B` / `POST /api/backfill/rescan` re-run): it scans the channel's `videos`, `streams`, and (when membership is active) `membership` tabs to window depth via YouTube's `/browse` continuation API, upserting into the same store. Scans are strictly serial across channels on a single consumer goroutine, globally paced at 1 tab page/second, resumable via a cursor persisted in `channel_state.backfill_state`, and report progress to both UIs. The sweep that queues scans rides the feed-monitor cycle, so startup and `kickMonitors()` both trigger it.

### 14. DECAPI Monitor
`monitor.NewDecapiMonitor()` uses the DECAPI API to find the latest video for YouTube channels. Rate-limited to 60 requests/minute (reads rate limit headers from responses). Stagger of 1 second between per-channel requests.

### 15. Twitch Monitor
`monitor.NewTwitchMonitor()` polls Twitch GQL for live streams. Default 15-second interval. Stagger of 500ms between per-channel requests. Uses the Twitch service's GQL client for stream info queries.

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
- `cookieRefresh.OnRecoveryNeeded` -> triggers `autoCookieSvc.RefreshCookies()` in background goroutine

All monitor `OnVideoFound`/`OnStreamFound` callbacks are wrapped with `defer func() { if r := recover() }()` for panic isolation.

## Package Dependency Graph

```
cmd/moombox/ (5 files)                 -- launcher + orchestrator (~2,170 lines)
cmd/sign/main.go                       -- CI signing tool (Ed25519)

internal/config     (4 files, ~850)    -- TOML config, FlexDuration, channel terms
internal/updater    (3 files, ~450)    -- GitHub release checker + self-updater + Ed25519
internal/logger     (1 file,  ~470)    -- slog wrapper, file rotation, ring buffer, pub/sub
internal/database   (7 files, ~1,850)  -- SQLite/WAL, batch updates (100ms coalesce), pub/sub
internal/cookies    (9 files, ~2,700)  -- jar, refresh, auto-cookie (Firefox/Chromium)
internal/youtube    (8 files, ~1,950)  -- Service, PlayerAPI, Auth, watch page, format selector
internal/twitch    (10 files, ~3,200)  -- Service, GQL API, auth, HLS, IRC chat, VOD chat, emotes
internal/bgutils   (~10 files, ~1,800)  -- PO token: PotProvider + WebPoClient (sidecar primary, goja fallback)
internal/bgutils/sidecar (5 files,~700) -- Node subprocess manager: extract, JSON-RPC mux, Job Object
internal/bgutils/embed   (1 file)       -- go:embed boundary for node-windows-amd64.gz + node-linux-amd64.gz + node-linux-arm64.gz + sidecar.tar.gz + version.txt
internal/cipher     (9 files, ~1,500)  -- YouTube signature cipher: AST + regex, 3-VM LRU
internal/engine    (12 files, ~2,850)  -- SegmentDownloader (DASH/HLS/VOD), manifest, FFmpeg muxer
internal/chat       (3 files, ~1,400)  -- YouTube live chat downloader (polling + batching)
internal/worker    (23 files, ~6,500)  -- Worker, Orchestrator, StreamProcessor, Queue, Trim, Quality
internal/monitor    (4 files, ~1,450)  -- FeedMonitor (RSS), DecapiMonitor, TwitchMonitor
internal/notif.     (2 files, ~330)    -- Manager + Discord webhook
internal/web       (21 files, ~6,600)  -- chi router, WebSocket hub, auth, middleware, routes
internal/tui       (33 files, ~13,100) -- 2-over-1 panel layout, overlays, chord system
internal/goja       (4 files, ~800)    -- JS runtime shims (minimal DOM, TextEncoder, timers)
internal/disk       (2 files, ~60)     -- Disk space queries: kernel32 on Windows, statfs on Linux
internal/errors     (1 file,  ~230)    -- Typed error hierarchy, sentinel codes
internal/constants  (1 file,  ~400)    -- Hardcoded values (API keys, URLs, timeouts)
internal/utils     (14 files, ~1,150)  -- HTTP helpers, formatters, YouTube URL parsing
```

Total: approximately 49,400 lines of Go across 179 source files (excluding tests, web assets, and cmd/moombox).

### Dependency Direction

Dependencies flow strictly downward. Lower-level packages never import higher-level ones:

- `cmd/moombox/main.go` imports everything (orchestrator)
- `internal/worker` imports: `database`, `engine`, `chat`, `youtube`, `twitch`, `bgutils`, `cipher`, `config`, `constants`, `notifications`, `utils`
- `internal/web` imports: `database`, `config`, `worker` (route handlers), `youtube`, `twitch`
- `internal/tui` imports: `database`, `config`, `web` (HTTP client for API calls)
- `internal/monitor` imports: `database`, `config`, `twitch`
- `internal/engine` imports: nothing from internal (standalone download/mux logic)
- `internal/database` imports: nothing from internal
- `internal/errors` imports: nothing from internal
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
JobQueue.Dequeue (blocks until lifecycle slot available, max 100)
    |
    v
StreamProcessor.Process (probe status, wait for live, start early chat)
    |
    v
JobQueue.AcquireDownloadSlot (blocks until download slot available, max 2)
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
2. If chat download is enabled, attempts to start an early chat downloader (`tryStartEarlyChat()`) to capture pre-stream chat messages
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
11. Signal chat to finish, wait up to 2 minutes for chat completion
12. Release download slot (muxing is CPU-bound, not a download)
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
1. Starts quality monitor (30-second probe interval) if user hasn't manually selected itags. The probe routes through `ProbeVideoStatus` (ANDROID_VR, cookieless, no POT) for public streams; members-only / age-restricted / login-required streams use the authenticated `GetVideoInfo` path. If the cookieless probe returns successfully without a `DashManifestURL` (e.g., a YouTube experiment stripped DASH from ANDROID_VR), the probe falls back once to the authenticated path so quality changes don't go undetected.
2. Runs segment downloaders in a goroutine
3. Simultaneously listens for quality change signals on `qualityChangeCh`
4. When quality changes mid-stream:
   - Ignores if segment is shorter than 10 seconds (`minSegmentDuration`)
   - Cancels current downloaders
   - Muxes the current segment in a background goroutine (using `context.Background()`)
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

### SegmentDownloader

The `SegmentDownloader` in `internal/engine/downloader.go` handles the actual byte-level downloading. It supports four modes:

**DASH sequential mode (`runDashLoop`):**
- Downloads video/audio segments sequentially by sequence number
- Constructs segment URLs by appending `&sq={seq}` to the base URL
- Detects stream end via HTTP 404 with retry backoff
- Performs HEAD probes every 5 seconds to discover the head sequence number
- Saves resume state every 50 segments (`ResumeSeqInterval`)
- Gap detection: if a segment returns 204/empty, records it and continues

**HLS live mode (`runHlsLoop`):**
- Polls the HLS playlist URL periodically
- Downloads new segments as they appear
- Follows the live edge (media sequence numbers)
- Catch-up mode: if more than 10 segments behind (`CatchupThreshold`), launches 6 parallel workers

**VOD direct download mode (`runDirectDownload`):**
- Probes total file size via `Range: bytes=0-0` HEAD request
- Downloads in 5MB chunks (`DownloadChunkSize`) using Range requests
- Per-chunk retry (up to 3 attempts, `MaxChunkRetries`)
- Falls back to streaming download if server doesn't support Range
- Progress reported as percentage

**Resume capability:**
- `.resume.json` file stores: `lastSeq`, `bytesWritten`, `timestamp`, `baseUrl`, `streamId`
- On resume: validates IDENTITY via `resumeIdentityMismatch` (explicit StreamID first, then YouTube URL fingerprinting; opaque no-identity URLs are trusted — see data-and-storage.md), then file size vs saved bytes
- DB-level fallback: if resume file is lost but database has `last_video_seq`/`last_audio_seq`, uses file size as byte position
- File is truncated to known-good position before appending; Twitch live (`StopOnGap`) never truncates staged data on a bad sidecar — it gap-splits instead

**Key constants:**
| Constant | Value | Purpose |
|----------|-------|---------|
| `CatchupThreshold` | 10 | Segments behind before parallel catch-up |
| `MaxSegmentRetries` | 5 | Per-segment retry limit |
| `ParallelDownloads` | 6 | Concurrent workers during catch-up |
| `SegmentTimeout` | 30s | HTTP timeout per segment request |
| `DefaultMaxTimeout` | 10min | Fallback for `maximum_timeout` (force-finalize when no segment arrives for this long, even if YouTube reports live) |
| `streamStatusCheckInterval` | 30s | No-segment gap that triggers a stream-status check (re-checked at most once per interval) |
| `HeadProbeInterval` | 5s | Interval for HEAD probes to discover head seq |
| `ResumeSeqInterval` | 50 | Save resume state every N sequential segments |
| `ResumeCatchupInterval` | 10 | Save resume state every N catch-up segments |
| `DownloadChunkSize` | 5MB | Chunk size for VOD Range requests |
| `MaxChunkRetries` | 3 | Per-chunk retry limit for VOD |
| `ProgressThrottle` | 500ms | Throttle VOD progress emission |
| `DefaultRetryDelayCap` | 60s | Max retry delay (exponential backoff cap) |

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
2. Muxes the current segment in a `context.Background()` goroutine
3. Records the segment in the `segments` database table
4. Re-fetches video info and creates new downloaders at the new quality
5. Updates the monitor baseline to the new quality

### JobQueue

The `JobQueue` implements a two-tier concurrency model:

**Lifecycle tier (100 slots):**
- Gates how many jobs can be in the process/wait/download pipeline simultaneously
- A job holds a lifecycle slot from `Dequeue()` to `Complete()`
- This means up to 100 jobs can be probing, waiting for live, downloading, or muxing at once

**Download tier (configurable, default 10 slots):**
- Gates how many VOD jobs can be actively downloading segments in parallel — VODs ONLY. Broadcasts pass through ungated (`acquireDownloadSlot` with `isVod=false` is a no-op): a missed slot on a VOD delays a file that already exists, a missed slot on a live broadcast loses footage. Peak concurrent downloads is therefore (live broadcasts) + `num_parallel_downloads`
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
- `Dequeue(ctx) -> (jobID, jobCtx, ok)`: Blocks until a lifecycle slot is free and a pending job exists. Returns a per-job cancellable context.
- `AcquireDownloadSlot(ctx, jobID) -> bool`: Blocks until a download slot is free. Returns false if context cancelled.
- `ReleaseDownloadSlot(jobID)`: Frees the download slot. Signals waiting jobs.
- `Complete(jobID)`: Frees lifecycle slot and download slot (if held). Cancels the per-job context.
- `Cancel(jobID)`: User-initiated cancellation. Sets `cancelled` flag, cancels context, removes from pending queue.
- `WasCancelled(jobID) -> bool`: Returns and clears the cancellation flag. Used to distinguish user cancellation from shutdown.

**Signaling:**
- `notify` channel (capacity 1): signals that a pending job or lifecycle slot is available
- `dlNotify` channel (capacity 1): signals that a download slot is available
- Both use non-blocking sends to avoid producer blocking

### Backlog Scheduler

The worker-owned `Scheduler` (`internal/worker/scheduler.go`) admits backlog (`Queued`) jobs at most `archive_slots` at a time per channel — spec §10's archive-slots pacing. It is the only path out of `Queued`: `ShouldProcess(Queued)` is false by design, so neither startup recovery nor the worker's heartbeat poller ever touches a `Queued` row.

- Single goroutine, woken by `Wake()` (backlog-job creation, job completion) or the worker's 60s heartbeat; wake signals coalesce through a capacity-1 channel
- One admission sweep per wake: for each channel with `Queued` rows, admit `archive_slots − in-flight backlog jobs`, newest `published` first
- Admission writes `status = Upcoming` durably FIRST (the in-flight count observes the DB), then enqueues in the JobQueue — a crash between the two steps self-heals because startup recovery re-enqueues `Upcoming` rows
- `resolveSlots` is injected by `cmd/moombox` against the live config store, so per-channel `archive_slots` overrides hot-reload

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
        jobID, jobCtx := queue.Dequeue(ctx)     // blocks until lifecycle slot
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

The WebSocket hub does not throttle `job_update` broadcasts. The highest-frequency caller (`OnJobChange` driven by `ProgressTracker.maybeUpdate`) is already capped to ~60 Hz per job by `progressUpdateInterval = 16ms`; the other callers (`OnJobAdded`, `OnTrimsChanged`) are event-driven. A previous per-job throttle in the hub created an ordering race where the trailing edge could arrive after a `BroadcastJobDeleted` and resurrect a deleted row via the client's upsert handler.

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

### Cipher 3-VM LRU

YouTube signature decryption requires executing JavaScript from `player.js`. This is expensive (multi-MB Goja VM):

- Maximum 3 VMs cached simultaneously (memory-constrained)
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
- Quality-split segment muxing runs in `context.Background()` goroutines
- Each has its own panic recovery
- This ensures muxing completes even if the parent context is cancelled (shutdown)

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
| `COOKIES?` | `"COOKIES?"` | Terminal (but retriable). Authentication required -- cookies expired or members-only. |

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

Note: `Muxing` is **not** terminal. If muxing was interrupted by shutdown
(the muxer process was killed mid-encode), `enqueueExistingJobs` resets
the job's status back to `Downloading` on next launch so the orchestrator
re-runs the mux step. Mux is idempotent — partial output is overwritten.

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
- `ExecuteTwitch(ctx, jobCtx, variant, isVod, twitchChat) -> error`: Full Twitch download pipeline. For live streams this is a *session loop*: the engine runs with `StopOnGap` (Twitch has no DVR), so an unrecoverable playlist gap muxes the current capture as a finished part (`{name} - partN.mp4` + rolled per-part chat) and continues at the live edge in a new `seg_N` staging dir; a connectivity outage pauses the session and resumes the SAME job once the same broadcast (stream_start_time identity, rechecked post-outage) is reachable again — one job per broadcast. On daemon restart, `discoverResumeSegment` maps staged `seg_N` dirs + recorded segment rows to the correct part so a resume never appends into an already-muxed part's staging file. Seamless continuation (sequence numbers still covered by the playlist window) keeps appending — splits happen only where data was actually lost.

### worker.JobQueue

Key methods:
- `NewJobQueue(maxDownloads) -> *JobQueue`: Constructor (maxLifecycle fixed at 100).
- `Enqueue(jobID, status)`: Non-blocking add to pending queue.
- `Dequeue(ctx) -> (string, context.Context, bool)`: Blocking dequeue with lifecycle slot.
- `AcquireDownloadSlot(ctx, jobID) -> bool`: Blocking download slot acquisition.
- `ReleaseDownloadSlot(jobID)`: Non-blocking slot release.
- `Complete(jobID)`: Free all slots, cancel context.
- `Cancel(jobID)`: User cancellation.
- `WasCancelled(jobID) -> bool`: Check and clear cancellation flag.
- `SetMaxDownloads(n)`: Runtime update.
- `ActiveCount() -> int`: Current download slots in use.
- `LifecycleCount() -> int`: Current lifecycle slots in use.
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
- `internal/errors/errors.go` -- Complete error type hierarchy
