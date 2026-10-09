# Architecture

## Scope

This document provides a comprehensive technical reference for Moombox's internal architecture: the process model, service initialization, package structure, data flow, download pipeline, concurrency model, error handling, and key type definitions. It is the deepest and most detailed document in the specification suite, intended to give an LLM or developer full context for understanding how the system works at every level. Read this before making changes to core infrastructure, the download pipeline, or cross-cutting service wiring.

## Rules and Constraints

These are hard requirements that must be followed in all code changes:

- **Launcher/supervisor pattern via `_MOOMBOX_CHILD` env var.** The binary operates in two modes. Without the env var it acts as a launcher that spawns itself as a child. With `_MOOMBOX_CHILD=1` it runs the full application. Exit code 42 (`exitCodeRestart`) signals the launcher to respawn. This enables seamless restarts for config changes and binary updates.
- **All goroutines MUST have panic recovery.** Every `go func()` must include an inline `defer func() { if r := recover(); ... }()`. No exceptions. HTTP handlers use `RecoveryMiddleware`. Database callbacks use `safeCallJobUpdate`/`safeCallJobsChange`. Monitor callbacks wrap `OnVideoFound`/`OnStreamFound` with deferred recovery.
- **Logger interface is anonymous per-struct -- NEVER extract to a named interface.** Each struct that needs logging declares its own anonymous `logger interface { Debug/Info/Warn/Error }` field. This is intentional for loose coupling. Do not create a shared `Logger` type or named interface in a common package. The one exception is `internal/bgutils/sidecar`, which declares a package-level `Logger` interface of the same four-method shape so the sidecar can be constructed without that package naming a logger type.
- **Database partial updates use `UpdateJobFields()` with dynamic SET clauses.** Pass a `map[string]any` of field names to values. The method dynamically builds the SQL SET clause, auto-updates `updated_at`, and triggers `OnJobUpdate` subscribers. Never write raw UPDATE SQL for job fields outside this pattern.
- **Callback closures for cross-cutting service wiring, NOT interfaces.** Services are wired together in `main.go` using function closures (`OnVideoFound`, `OnStreamFound`, `OnSchedule`, `OnCookieRefreshNeeded`, etc.) and struct-based dependency injection. There are no service registry patterns or interface-based DI containers.
- **JobStatus is `type JobStatus string`.** Timestamps are ISO 8601 strings (RFC3339). Optional numeric fields (sequence numbers, dimensions, file sizes) use pointers (`*int`, `*int64`, `*float64`) where nil means "not set." Boolean fields in the database use integer 0/1 but are exposed as Go `bool` in the `Job` struct.
- **Cross-platform via build tags.** Windows x64, Linux x64, and Linux arm64 are supported. Platform-specific behavior is isolated in per-package `_windows.go` / `_unix.go` files: `createNoWindow = 0x08000000` (launcher, Windows only), kernel32 disk queries (`internal/disk/disk_windows.go` vs `disk_unix.go` via statfs), flock-based single-instance locking (`single_instance_unix.go`), and the ping-based `.exe~` cleanup (`launcher_windows.go`). The connectivity monitor (`internal/connectivity`) needs no split: it dials TCP through a plain `net.Dialer`, so its three files carry no build tags. Linux stubs produce correct no-op or functional fallback behavior; Windows-only features degrade with clear UI messaging.

## Process Model

### Launcher/Supervisor Pattern

Moombox uses a two-process model controlled by the `_MOOMBOX_CHILD` environment variable:

**Launcher process** (no `_MOOMBOX_CHILD`):
- Executes `launchAndSupervise()` in `cmd/moombox/launcher.go`
- Ignores SIGINT (the child handles Ctrl+C)
- Spawns itself as a child with `_MOOMBOX_CHILD=1` added to the environment
- Passes through stdin/stdout/stderr so the child's TUI renders in the launcher's console
- When the child exits with code 42 (`exitCodeRestart`), the launcher respawns. This picks up any new binary (for self-updates via the swap: current kept at `.old`, `.new` -> current)
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
    webServer.StartDrain()        // 503 for new requests from here on
    after 5s:                     // let in-flight setup/save requests finish
        cancel()                  // cancel the main context
        quitTUI()                 // if TUI is running, unblock tea.Program.Run()
}
```
Called from: `routes.SetupRoutes` (setup wizard completion), `routes.UpdateRoutes` (after applying update), `routes.RestartRoute` (manual API restart), and the TUI (settings save, update apply, setup wizard — `cmd/moombox/tui_wiring.go`).

**Shutdown sequence** (`shutdown()` in `cmd/moombox/shutdown.go`; every stop is wrapped in `stopService` panic isolation, and SPEC.md's Shutdown Sequence section carries the same list):
1. Context cancellation propagates to all services; the TUI quits (if running)
2. A 15-second force-exit timer (`forceExitAfter` — the worker's 12-second stop budget, `worker.StopBudget`, plus a 3-second margin) is armed FIRST — if graceful shutdown stalls it closes the rate limiters, database and log itself and exits (code 42 when a restart was requested, else 0, so the launcher does not treat a slow quit as a crash)
3. Notifications switch to single-attempt mode
4. Monitors stop (Twitch, DECAPI, Feed)
5. Download worker stops (waits up to 10 seconds for in-flight jobs to save state, then cancels in-flight muxes and gives FFmpeg 2 seconds to die). The force-exit must outlast this whole budget: its clock starts first, and a backstop no longer than the worker's wait would exit before `CancelMuxes` ran
6. In-flight notifications are flushed
7. Cookie refresh and auto-cookie services stop
8. PotProvider is cleaned up and the BotGuard sidecar is stopped (when running)
9. Web server shuts down
10. Log forwarder and DB event subscribers are unsubscribed
11. Database closes (final WAL checkpoint)
12. `shutdown()` returns the restart flag to `run()`, whose deferred `closeLog` flushes the log file

### Subcommands and Flags

Before the launcher/child split, `main` parses the flags and dispatches a subcommand:

- `moombox [-config path] add <video_id_or_url>` -- CLI mode that adds a video to the database and exits (`cmd/moombox/addvideo.go`). Flags are parsed first and the subcommand is the first argument after them, so `-config` may come before `add` or after it; without one, `add` runs the same config search as the daemon. Any other positional argument is refused with the usage text (which `-h` prints too). It does not talk to the web API: it opens the daemon's SQLite file directly with `database.Open` — after a `FileSchemaVersion` check that refuses a database whose schema does not match the binary, rather than migrating the live file from a second process — inserts the row with `AddJob`, and the running daemon's `pollForJobs` safety net (every 60 seconds) picks it up.
- `-version` -- Prints version and commit hash, exits immediately.
- `-headless` / `-no-tui` -- Runs web-only mode (no BubbleTea TUI). Also activated by `MOOMBOX_NO_TUI=1` env var.
- `-log-level <LEVEL>` -- Overrides the log level for this run only (DEBUG, INFO, WARN, ERROR). It reaches the logger and nothing else: `effectiveLogLevel` in `cmd/moombox/helpers.go` picks it over the configured level when building the logger, and `cfg.Logs.LogLevel` is left as the file has it, so the boot auto-persist, the password auto-hash and every later settings save keep writing the CONFIGURED level. A settings save from either UI re-applies the configured level to the running logger only when that level changed (`applyConfiguredLogLevel`, `cmd/moombox/tui_wiring.go`) — an explicit level choice ends the override, a save of any other setting keeps it.
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
`updater.New()` creates the GitHub release checker. Its `CleanupOldBinary()` sweep of the previous update's `.old` (and `.failed`) binary does not run here but at the first-successful-boot milestone in `run()`, after the database opened and the web bind resolved, so a boot-crashing update still has its rollback artifact. Performs Ed25519 signature verification before applying binary swaps.

### 4. Database
`database.Open()` opens SQLite with WAL mode, 5-second busy timeout, foreign keys enabled, single-writer connection pool (`MaxOpenConns=1`). Runs schema migrations (`migrate()`, up to `schemaVersion` in `internal/database/migrations.go` — 20 at the time of writing; `appendix-metrics.md` tracks it). Prepares the hot-path `GetJob` statement (`prepareStatements`). Starts no goroutine: every job write is a synchronous `UpdateJobFields` call.

### 5. Cookie Jar
`cookies.NewCookieJar()` creates the cookie container. If `cfg.Cookies.CookieFile` is set, loads cookies from the Netscape-format file. Auto-detects platforms (YouTube/Twitch) from cookie domains if not explicitly configured.

### 6. YouTube Service
`youtube.NewService(jar, log)` creates the YouTube service. `Init(ctx)` fetches the YouTube homepage to extract visitor data and the Innertube API key. These are needed for all subsequent API calls.

### 7. Twitch Service
`twitch.NewService(jar, log)` creates the Twitch service. Initializes GQL API authentication from cookies (looks for `auth-token` cookie). Logs auth status at startup.

### 8. PO Token Provider + BotGuard Sidecar
`bgutils.NewPotProvider()` creates the BotGuard PO token provider with its triple-layer cache: session cache (6h TTL), minter cache (single-minter design, dynamic TTL from BotGuard response), and inflight dedup (concurrent requests share a single generation via channel synchronization). Immediately after, when `cfg.Bgutils.UseSidecar` is true (default), `sidecar.New(...)` constructs a `Sidecar` and `Start(ctx)` launches the embedded Node.js subprocess: extract the platform's `node-<goos>-<goarch>.gz` + `sidecar.tar.gz` from `go:embed` to `os.UserCacheDir()/Moombox/sidecar/`, apply user-only DACL, spawn `moombox-sidecar[.exe] src/server.js` pinned to a Windows Job Object, and wait for its `ready` event. `potProvider.SetSidecar(s)` attaches the handle before that first start, which puts `PotProvider` in sidecar mode: every mint goes to the sidecar, and while it is down a mint fails at once — the goja path mints no PO token in practice, so falling through to it only cost time. Failure to start the sidecar is non-fatal — Moombox logs a warning and the supervisor keeps retrying. The per-platform blob (`node-linux-amd64.gz`, `node-linux-arm64.gz`, `node-windows-amd64.gz`) is selected at build time by the `embed_<goos>_<goarch>.go` files; the extraction directory is `os.UserCacheDir()`'s (`%LOCALAPPDATA%` on Windows, `~/.cache` on Linux — `/data/.cache` in the Docker image, where `HOME=/data`).

### 9. Cipher Solver
`cipher.NewGojaResolver(cacheDir, log)` creates the goja cipher resolver (cache directory `%TEMP%/yt-cipher`, a 10-VM LRU keyed by `player.js` URL); `cipher.NewSidecarSolver` and `cipher.NewCompositeSolver` layer the BotGuard sidecar's V8 ejs in front of it. The resolver is wired to `ytService.PlayerAPI.SetCipherSolver()` for the signature timestamp, and the composite solver to the worker, which resolves each chosen format's URL after selection. Uses full AST parsing with regex fallback for extraction.

### 10. Notification Manager
`notifications.NewManager(cfg, log)` creates the notification dispatcher. Currently supports Discord webhooks. Sends notifications for: stream found, stream live, download starting, download finished, download error, auth required, trim created, update available. "Stream found" is sent for broadcasts and new VODs only — a backlog VOD is queued silently and announced by its "download starting" when the archive-slots scheduler admits it (`announcesJobFound`, `cmd/moombox/monitor_callbacks.go`), so a deep backfill does not send one notification per catalog VOD at once.

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

`checkChannel` runs four steps per channel per cycle: **FETCH** (RSS + membership, independently fallible), **STORE** (upsert every listed item into the persistent `feed_items` table, tracking which IDs are new this cycle), **WALK** (a serial probe pass over the store's archive scope — everything published within `archive_window_days`, default 3 and per-channel overridable, plus ALL upcoming/live rows regardless of age and the still-unresolved ones (`assumed`-dated rows, and RSS rows first seen inside the window, whose `<published>` is only the announcement time) — applying `HasActiveJob` dedup, term filtering, and probe-status rules, with per-source early exit once a date-ordered source falls entirely outside the window), and **ARCHIVE** (re-read the scope and decide job creation per row). ARCHIVE assigns each job a disposition: broadcasts (live/upcoming) and VODs first inserted this cycle — or by a cycle within the last 24 hours (`newVODCarry`, `internal/monitor/feed.go`) that has not jobbed them yet, so one failed first probe does not demote new content to the backlog; the carry is in-process, and a restart in between still does — are admitted immediately (`queue_priority` 0), while backlog VODs already known to the store are created as `Queued` (`queue_priority` 1) and paced by the worker's per-channel `archive_slots` scheduler — a backlog sweep never delays new or live content.

A companion `monitor.NewBackfillWorker()` owns the full-catalog backfill (channel add, window widening, or a manual `R B` / `POST /api/backfill/rescan` re-run): it scans the channel's `videos`, `streams`, and (when membership is active) `membership` tabs to window depth via YouTube's `/browse` continuation API, upserting into the same store. Scans are strictly serial across channels on a single consumer goroutine, globally paced at 1 tab page/second, resumable via a cursor persisted in `channel_state.backfill_state`, and report progress to both UIs. The cursor records the window its finished tabs were judged against, since a tab that stopped at the window's edge is finished only for that window: a retry at a wider one — the config widened between an interrupted scan and its retry, or while the process was down — scans from page 1, while a narrower one resumes it. The sweep that queues scans rides the feed-monitor cycle, so startup and `kickMonitors()` both trigger it.

### 14. DECAPI Monitor
`monitor.NewDecapiMonitor()` uses the DECAPI API to find the latest video for YouTube channels. Rate-limited to 60 requests/minute (reads rate limit headers from responses). Stagger of 1 second between per-channel requests. The request and the classification hold separate deadlines: `fetchLatestVideo` (`internal/monitor/decapi.go`) owns the 15-second request timeout and releases it as soon as the body is read, and the probe then runs under `decapiProbeBudget` (`internal/monitor/decapi.go`, 60 s) derived from the cycle context — so a slow DECAPI answer never shortens the probe. Because DECAPI reports a channel's NEWEST video, a dormant channel would otherwise re-probe the same finished VOD every cycle; a per-channel memo skips the probe when the video ID is unchanged, its last classification was terminal (`decapiTerminalStatus` in `internal/monitor/decapi.go` — `vod` or `not_a_stream`), and the reason it was not jobbed still holds: the processing history holds it (it was jobbed — clearing the history row re-opens the video on the next cycle), it was judged outside the same archive window, or it was skipped because the channel does not archive VODs and still does not. DECAPI writes no history itself: a skipped or given-up video is not a created job, and both monitors gate VODs on history, so the rows it used to write — after three failed probes, or on that skip — kept the video from ever being archived, even once the probe recovered or `include_non_live_content` was turned on.

### 15. Twitch Monitor
`monitor.NewTwitchMonitor()` polls Twitch GQL for live streams. Default 15-second interval. Channels are batched into GQL requests of up to 30 logins, with a 500 ms stagger between chunks — including after a chunk whose whole request failed (`checkChunk` in `internal/monitor/twitch.go`), since a 429 or 5xx is exactly when pacing matters. Uses the Twitch service's GQL client for stream info queries. A live channel creates no job while a manually added job has claimed the broadcast (`manualJobClaims`, `internal/monitor/twitch.go`, over `ManualTwitchJobs`, `internal/database/database_jobs.go`): one waiting in `waitForTwitchLive` takes whatever goes live next, and one downloading has claimed the broadcast its `stream_start_time` names. A manual add for an offline channel is `tw_manual_<login>_<ns>` and carries no stream ID, so the stream-ID dedupe never matched it, and the monitor recorded the broadcast a second time beside it. A manual job parked in `COOKIES?` or muxing an earlier broadcast claims nothing — standing aside for those blocked every later broadcast on the channel.

### 16. Cookie Refresh Service
`cookies.NewRefreshService()` validates cookies and checks auth status periodically — every 30 minutes (`defaultRefreshInterval`, `internal/cookies/refresh.go`); the `cookies.refresh_interval` setting (6h by default) drives only the separate browser-refresh timer in `AutoCookieService`. Detects auth loss by comparing current status against expected platforms. Triggers `OnRecoveryNeeded` callback when auth is lost, which attempts auto-cookie recovery.

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
- `db.OnJobChange` -> `wsHub.BroadcastJobProgress()` for a progress-only write, otherwise `wsHub.BroadcastJobUpdate()` (per-job WebSocket messages); re-syncs per-job log routing on a status write
- `db.OnJobAdded` / `db.OnTrimsChanged` -> `wsHub.BroadcastJobUpdate()` for the one job; `db.OnJobDeleted` -> `onJobDeleted` (clear the job's log buffer + `BroadcastJobDeleted`)
- `db.OnJobsChange` (the two bulk writers only) -> `wsHub.BroadcastJobsUpdate()` (full job list), re-sync and prune job logs
- `log.Subscribe()` -> `wsHub.BroadcastLog()` + `db.RouteLogToJobs()` (per-job log buffers)
- `cookieRefresh.OnRecoveryNeeded` -> `runCookieRecovery()` in a background goroutine: `autoCookieSvc.RefreshCookiesDetailed()`, then notifies on the triggering platform's own verdict (OK / Failed / Unknown)

All monitor `OnVideoFound`/`OnStreamFound` callbacks are wrapped with `defer func() { if r := recover() }()` for panic isolation.

## Package Dependency Graph

```
cmd/moombox/ (24 files)                -- launcher + orchestrator (~8,550 lines)
cmd/sign/main.go                       -- CI signing tool (Ed25519)

internal/config     (7 files, ~2,490)  -- TOML config, FlexDuration, channel terms
internal/updater    (3 files, ~870)    -- GitHub release checker + self-updater + Ed25519
internal/ytdlpplugin (1 file,  ~330)   -- yt-dlp plugin file: status, install, generator (shared by the web route and the TUI E Y overlay)
internal/logger     (1 file,  ~760)    -- slog wrapper, file rotation, ring buffer, pub/sub
internal/database   (8 files, ~4,110)  -- SQLite/WAL, synchronous writes, pub/sub
internal/stats      (1 file,  ~70)     -- the figures both dashboards show, derived from the job aggregate + disk reading (imports only database)
internal/jobfilter  (2 files, ~470)    -- the dashboard's filter language (Parse/Match/Serialize), the TUI's / box
internal/cookies    (35 files, ~15,870) -- jar, refresh, auto-cookie (Firefox/Chromium)
internal/youtube    (13 files, ~6,710) -- Service, PlayerAPI, Auth, watch page, format selector
internal/twitch    (14 files, ~6,410)  -- Service, GQL API, auth, HLS, IRC chat, VOD chat, emotes
internal/bgutils   (6 files, ~2,050)   -- PO token: PotProvider + WebPoClient (sidecar; goja when it is turned off)
internal/bgutils/sidecar (7 files,~1,880) -- Node subprocess manager: extract, JSON-RPC mux, Job Object
internal/bgutils/embed   (4 files)      -- go:embed boundary for node-windows-amd64.gz + node-linux-amd64.gz + node-linux-arm64.gz + sidecar.tar.gz + version.txt
internal/cipher     (13 files, ~3,110) -- YouTube signature cipher: AST + regex, 10-VM LRU
internal/engine    (20 files, ~8,170)  -- SegmentDownloader (DASH/HLS/VOD), manifest, FFmpeg muxer
internal/chat       (3 files, ~3,030)  -- YouTube live chat downloader (polling + batching)
internal/worker    (39 files, ~18,070) -- Worker, Orchestrator, StreamProcessor, Queue, Trim, Quality
internal/monitor    (9 files, ~5,090)  -- FeedMonitor (RSS), DecapiMonitor, TwitchMonitor
internal/notif.    (11 files, ~3,790)  -- Manager + Discord webhook, batching, edit-mode message ids
internal/web       (34 files, ~11,750) -- chi router, WebSocket hub, auth, middleware (9 files) + routes/ (25 files)
internal/tui       (43 files, ~22,710) -- 2-over-1 panel layout, overlays, chord system
internal/goja       (5 files, ~1,470)  -- JS runtime shims (minimal DOM, TextEncoder, timers)
internal/connectivity (3 files, ~480)  -- reachability monitor (plain TCP dial); gates stream-end verdicts during outages
internal/httpx      (1 file,  ~110)    -- shared keep-alive-tuned http.Client/Transport shapes
internal/disk       (3 files, ~130)    -- Disk space queries: kernel32 on Windows, statfs on Linux
internal/constants  (1 file,  ~320)    -- Hardcoded values (API keys, URLs, timeouts)
internal/utils     (26 files, ~2,590)  -- HTTP helpers, formatters, YouTube URL parsing
```

Total: approximately 124,200 lines of Go across 321 source files under `internal/` (excluding tests, web assets, and `cmd/`). `appendix-metrics.md` is the maintained copy of these numbers and carries the script that regenerates them.

### Dependency Direction

Dependencies flow strictly downward. Lower-level packages never import higher-level ones:

The lists below are the `internal/` imports of each package as `go list -f '{{join .Imports " "}}' ./internal/<pkg>` prints them (regenerate the same way):

- `cmd/moombox` imports everything (orchestrator): `bgutils`, `bgutils/sidecar`, `cipher`, `config`, `connectivity`, `cookies`, `database`, `engine`, `jobfilter`, `logger`, `monitor`, `notifications`, `stats`, `tui`, `twitch`, `updater`, `utils`, `web`, `internal/web/routes`, `worker`, `youtube`
- `internal/worker` imports: `bgutils`, `chat`, `cipher`, `config`, `constants`, `database`, `engine`, `httpx`, `notifications`, `twitch`, `utils`, `youtube`
- `internal/web/routes` imports: `bgutils`, `bgutils/sidecar`, `config`, `cookies`, `database`, `disk`, `jobfilter`, `notifications`, `stats`, `updater`, `utils`, `web`, `worker`, `ytdlpplugin`
- `internal/web` imports: `config` only — the hub's `Broadcast*` methods take `any`, so the server, hub, auth and middleware never import the job types; the route handlers live in `internal/web/routes`
- `internal/tui` imports: `config`, `constants`, `cookies`, `database`, `httpx`, `jobfilter`, `notifications`, `stats`, `utils`, `ytdlpplugin` — NOT `web`: the TUI's HTTP calls use a plain `net/http` client carrying the internal token, and its live updates come straight from the database subscriptions
- `internal/monitor` imports: `config`, `database`, `httpx`, `twitch`, `worker`
- `internal/youtube` imports: `cipher`, `constants`, `cookies`, `httpx`, `utils`
- `internal/twitch` imports: `constants`, `cookies`, `httpx`, `utils`
- `internal/cipher` imports: `bgutils/sidecar`, `goja`, `httpx`, `utils`
- `internal/bgutils` imports: `bgutils/sidecar`, `constants`, `goja`, `httpx`
- `internal/cookies` imports: `constants`, `cookies/dpapi`, `httpx`, `utils`
- `internal/engine` imports: `constants`, `httpx`, `utils` (e.g. `DownloadChunkSize` is `constants.DownloadChunkSize`)
- `internal/chat` / `internal/notifications` import: `constants`/`config`, `httpx`, `utils`
- `internal/utils` imports: `connectivity`, `constants`, `httpx`
- `internal/database`, `internal/constants`, `internal/httpx`, `internal/connectivity` import nothing from internal

Cross-cutting concerns (logging, notifications, events) flow through callback closures wired in `cmd/moombox` (`services.go`, `monitor_callbacks.go`, `tui_wiring.go`), not through package imports.

## Key Data Flow

```
Monitors (RSS/DECAPI/Twitch)
    |
    v
Database (AddJob)  -->  OnJobAdded subscribers
    |                        |
    v                        v
Worker.EnqueueJob      WebSocket broadcast
    |
    v
JobQueue.Enqueue (priority: Live=1, everything else 0)
    |
    v
JobQueue.Dequeue (highest-priority pending job; no lifecycle gate)
    |
    v
StreamProcessor.Process (probe status, wait for live, start early chat)
    |
    v
JobQueue.AcquireDownloadSlot (VODs only; blocks until a slot is free, default max 10)
    |
    v
JobQueue.AcquireLifecycleSlot (blocks until a lifecycle slot is free, max 100)
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
ProgressTracker (one job-row write per report, one report per progress_interval_ms — 16ms default; gap rows flushed at most once a second)
    |
    v
Database.UpdateJobFields (synchronous UPDATE + row read-back under db.mu)
    |
    v
OnJobUpdate subscribers
    |
    v
WebSocket hub (no per-job throttle — ProgressTracker's per-job gate caps the rate upstream)
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
   - More than 1 hour away: 10-minute interval (`probeIntervalDistant`)
   - 5 minutes to 1 hour: 5-minute interval (`probeIntervalNear`)
   - Less than 5 minutes: 30-second interval (`probeIntervalImminent`)
   - No scheduled start time: the 5-minute "near" interval, since the stream can go live at any moment
   - Plus random jitter up to 30 seconds
4. Polls via lightweight `ProbeVideoStatus()` (ANDROID_VR client for speed). Persists metadata from each probe (title, thumbnail, description, scheduled start time, etc.) using change detection — only writes to DB when values actually differ, at zero additional network cost since the probe already returns this data
5. Chat surge detection: if 30+ new messages arrive within a 15-second window, triggers an immediate probe (the stream may have gone live early)
6. Members-only detection: if the probe returns `PlayabilityMembersOnly` or `PlayabilityLoginRequired` and auth cookies are available, switches to authenticated probing
7. On transition to `StreamLive`: performs full multi-client fetch, updates metadata, sends notification, passes pre-started chat downloader to the orchestrator
8. If the scheduled start time changes between probes, sends a "Schedule Changed" notification (event: `rescheduled`)
9. Maximum 10 consecutive probe errors before giving up

**Twitch path (`processTwitch()`):**
- VOD jobs (video ID prefix `tw_v`): fetches VOD info and HLS playlist, selects best variant, optionally creates VOD chat downloader
- Live jobs: checks if channel is live via GQL. If offline and manually added, enters `waitForTwitchLive()` polling loop (15s interval + 5s jitter). If live, fetches HLS master playlist, selects best variant, and constructs the IRC chat downloader (`chat_status = pending`); `ExecuteTwitch` starts it once the download begins

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
10. After download completes: finalize progress, sync total sequence counts. Both branches write `Muxing` here, ahead of the chat wait, so a restart inside that wait re-muxes from staging (`muxOnRestart`) instead of re-downloading a complete recording — the live branch at `streamEnded`, the VOD branch with its final progress write
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
   - A refresh that comes back at the download's own quality (and itags) continues in place; the monitor is re-baselined to the download's quality so an early refresh is retried on the next signal — until the SAME probed quality has been answered "unchanged" three times running (`sameQualityStrikeLimit`), when the probe and the download disagree for good (an HLS download beside the probe's DASH ladder) and the monitor takes the probe's reading as its baseline instead (`ReconcileSameQuality`, `internal/worker/quality_monitor.go`); before that bound, such a job cancelled and rebuilt its downloaders every 30 s for the rest of the broadcast
   - Cancels current downloaders
   - Muxes the current segment in a background goroutine parented by the orchestrator's mux root (`launchBackgroundSegmentMux` in `internal/worker/quality_split_common.go`), not `context.Background()`
   - Records segment metadata in database
   - Re-fetches video info with new format selection
   - Creates new downloaders at the new quality
   - Continues download loop
5. When download ends naturally (stream ended, quality lost, or error):
   - Verifies stream has actually ended via YouTube API (up to 6 checks, 5-minute intervals). A look that fails (a bot wall, a 429, a 5xx) is not a verdict: it spends the same 6-check budget, and the capture is only given up once that budget is spent and no segment has arrived for `streamSegmentTimeout` (`unreadableStatusEndsCapture`) — before, one failed look after the engine's `maximum_timeout` finalize ended the job, since both default to ten minutes
   - If stream is still live, re-fetches formats and restarts download. The refresh is judged before it is adopted: a stream that comes back from the stall at a different quality (an encoder restart) — or at the same size in a different rendition, a video or audio itag the DASH strategies record (`VideoItag`/`AudioItag`, compared by `streamIdentityChanged` in `internal/worker/orchestrator.go`, which the VOD refresh guard `refreshFormatMatches` applies too) — takes the same split step 4 does (`splitPart`, `internal/worker/orchestrator_youtube.go`), because the refreshed downloaders continue the current part — through its sidecar, or from the stopped downloaders' position, which the still-live refresh now forces so a sidecar the engine already cleared cannot leave it to a stale DB seq — and would otherwise append the new rendition under the old init segment
   - `ErrQualityLost` triggers format re-fetch and restart at available quality. A refresh there — or the one a quality split makes for its new part — that fails while YouTube still reports the stream live is retried (`refreshWhileLive`, `internal/worker/orchestrator_youtube.go`) on the verify cadence and within its 6-check budget, re-reading the stream status before each attempt, rather than ending the recording on one transient manifest or cipher fetch; only a stream no longer reported live or a spent budget ends it
6. Stream-end verification prevents premature termination from transient network issues. It is not entered for a failure to write the staged recording (`engine.ErrLocalWrite` — opening the output file or appending a segment: a full disk, a permission, a vanished staging dir): the live loop stops with that error, so the job lands in Error with staging and the resume sidecar kept, instead of re-verifying a stream it cannot write for up to an hour and then finishing as if it had ended

**VOD-branch bounded refresh loop (`runVodDownloadWithRefresh`):** googlevideo URLs live ~6h; a post-live download whose wall clock outlives that grant finalizes with `FinalizedBehindHead()` true on the video and/or audio downloader instead of erroring outright. The live branch has had URL-expiry recovery via `ErrQualityLost` -> `refreshDownload` from the start; the VOD branch used to run `runDownloaders()` exactly once. It now re-extracts via `GetVideoInfo`, seeds `VideoStartSeq`/`AudioStartSeq` from the last written sequence, and rebuilds through the same `refreshDownload` — provided the prior attempt actually made progress and the fresh extraction still offers manifestless DASH formats (a stream that finished processing into a true VOD is left to the incomplete-tail flag + manual retry instead). Bounded by `maxVodRefreshAttempts` (4, ~24h of wall clock); past that, or on no progress, the loop stops and returns whatever was captured.

**Eviction diagnosis (`diagnoseEvictedStart`):** both branches run this check after a nil download error. If a YouTube manifestless download finished having written zero bytes and its `HeadSeq()` is implausibly deep (past `minEvictionHead`, ~28h of segments), an ordinary failed start is an unlikely explanation — YouTube's ~120h retention window may have scrolled segment 0 out from under a marathon stream. The check bisects `[0, head]` for the oldest segment the CDN still serves (`engine.FindOldestAvailableSeq`), fetches that boundary segment, and inspects its box structure (`engine.InspectSegment`) to log a full diagnosis. A confirmed eviction (oldest available segment > 0) fails the job with a precise "exceeds YouTube's retention window" error instead of the generic empty-download failure; a dead-URL bisection or an oldest of 0 leaves the ordinary failure path to run unchanged. Diagnosis only — no download jump; that is gated future work (docs/plans/2026-08-05-incomplete-tail-and-marathon-streams.md Phase D).

**Interruption resume — finalize deferral (`stallForPossibleResume`, `internal/engine/downloader.go`):** a live YouTube `SegmentDownloader`'s `MayResume` callback, when installed, gates both of the DASH loop's MaxTimeout-backstop finalizes — `handleGoneError`'s gone-burst verification path and `handleHTTPError`'s no-segment maximum-timeout backstop (both `internal/engine/downloader_dash.go`) — but never a confirmed-ended verdict (`streamEndVerified` always finalizes immediately, MayResume unconsulted). The first `MayResume()==true` observation for a stall latches a per-episode clock (`interruptionStallStart`); `downloader.interruption_timeout` (config minutes, default 120 — see the config table in data-and-storage.md) bounds how long deferral continues, retrying every `interruptionStallRetryDelay` (5s). When `MayResume` flips false or the ceiling expires, `finalizedDuringInterruption` latches and the finalize proceeds without setting `streamEnded` — mirroring the existing behind-head-tail contract — so the resume sidecar (`.resume.json`) survives for a later in-place continuation instead of being cleared. `InterruptionTimeout == InterruptionNoStall` (a `-1` sentinel, distinct from the ordinary `0` = "no ceiling"/unbounded meaning) is a third mode: consult `MayResume` exactly once, latch `finalizedDuringInterruption` when it's true, and always return `false` — finalize proceeds on that same call, with no stall and no clock. The worker maps its own `interruption_timeout=0` ("stall disabled" per the config contract) onto this sentinel (`engineInterruptionTimeout`, `internal/worker/interruption.go`) rather than passing `0` straight through, so a disabled-stall job still latches Tier 2 evidence without ever blocking finalize. This Tier 1 engine-level stall is DASH-only — the HLS live strategy (`runHlsLoop`) has no `stallForPossibleResume` call site at all, so an HLS live YouTube download gets only the worker-level Tier 2/3 preservation described below, not this deferral.

**Interruption resume — auto-resume valve (`shouldWaitForResume`, `resumeOnRedetect`):** on the worker side, an `interruptionSignal` (`internal/worker/interruption.go`) tracks the last time a player-response fetch showed the broadcast-interrupted signature (StreamStatus live with zero formats), observed at `refreshGvsCredentials`'s re-fetch, the live loop's own post-exit refetches (`orchestrator_youtube.go`), and each YouTube strategy's `CheckStreamStatus` closure via `observeYouTubeStatusProbe` — the only site that fires while the engine is still internally stalling. `buildMayResume` reports resume-plausible when that signal is fresh (90s) OR the job's chat downloader still has its live continuation open. `runLiveStreamDownload` feeds that evidence into `noteRefreshFailure`, which splits it into two independent questions: `resumeEvidence` (does the evidence hold at all) always latches the finalize-scoped `resumeWaitLatch` when it does, while `shouldWaitForResume` (evidence AND `interruptionTimeout > 0`) is the PERMISSION gate deciding whether the loop actually waits instead of finalizing immediately — for up to `interruption_timeout`, one episode (`waitDeadline`) shared by the quality-loss branch, which waits once, and the stream-end verify branch the cancelled downloaders bring every later look to, which asks the same episode and refunds the live check it spent while the wait goes on (a look whose status fetch fails is a wait tick there too, not a spent check), its still-live sleep labelled `ActivityWaitingResume` rather than `ActivityVerifyingEnd`. `maxConsecutiveLiveChecks` (6) bounds only that branch's retries outside a wait; it used to end every wait after about half an hour, whatever `interruption_timeout` said. So a `interruption_timeout=0` job never actually waits, but a genuinely-interrupted one still latches `resumeWaitLatch`, which ORs into `incomplete_tail` (`finalizeIncompleteTail`) even on a run where the engine itself never latched `FinalizedDuringInterruption`. If the ceiling (when enabled) is still reached, the job finalizes Finished with staging preserved; a later live re-detection of that same video ID reopens the valve from outside the download loop entirely — `resumeOnRedetect` (`cmd/moombox/monitor_callbacks.go`) calls `DownloadWorker.ResumeJob` when the re-detected job is Finished with `incomplete_tail` set and its staging files still exist on disk, coalesced to at most one auto-resume attempt per 5 minutes per job and never for a job a human Cancelled.

**Interruption resume — part merge (`mergeSameFormatParts`, `internal/worker/part_merge.go`):** `finalizeMultiSegmentJob` runs `mergeSameFormatParts` (Tier 4) before deciding the finalize shape — including the single-part-takes-the-plain-name rename — so a job whose parts all losslessly recombine ends up named exactly like a never-split job. It probes each part via ffprobe, groups contiguous runs of identical stream parameters, and concat-copies each run; any probe, concat, or db-replace failure leaves that run's segments untouched, since the merge is opportunistic and never a finalize gate. A chat-merge failure aborts that run specifically — video included, not just chat — leaving its parts/rows/chat files exactly as they were while other runs in the same finalize still merge. No production run reaches the chat merge today: `finalizeMultiSegmentJob` merges YouTube jobs only (Twitch parts never merge), and only Twitch live IRC chat rolls per part, so no merged run carries a `ChatFile`; the rule stands so a future per-part chat cannot be dropped by a merge. A successful run commits every segment row — merged and untouched alike — in one `database.ReplaceJobSegments` call, then best-effort renames the merged output onto the run's first part's original name and removes the now-superseded later parts' output/chat files and their pre-mux `seg_N` staging directories. Those directories are tombstoned (`mergeTombstoneFile`) BEFORE the commit and the tombstones taken back if it fails, so a crash between the commit and the removal cannot leave row-less media that the next finalize re-muxes as a new part and merges a second time; a tombstone on a still-recorded part's dir changes nothing. A superseded dir that still holds a set-aside recording is kept rather than removed — an aside is never merged content — and the aside scan (`stagedAsideRecordings`) looks inside tombstoned dirs, so the finalize's recovery and every staging shield still see it.

**A split job that comes back as a VOD (`claimStagingRootForVod`, `supersedePartsWithVod`, `internal/worker/vod_supersede.go`):** a live capture that quality-split, was interrupted, and is restarted after the broadcast ended is re-classified VOD, and a VOD run records from the start into the staging ROOT — restart part-discovery is live-only. The root is part 0 unless `seg_0` exists, so the multi-segment finalize ignored the complete download once part 0 was recorded, finished the job as the partial live parts, and the staging cleanup deleted it. Now the run claims the root first: part 0's live capture (and sidecar) moves into `seg_0`, created even when empty because it is what stops every later finalize reading the root as part 0 (a root span the split skipped as too short, when `seg_0` already exists, is set aside by the `.restart-` convention instead), and a `vodRootMarkerFile` in the root records the run's state — `downloading`, then `complete` once the download ends with nothing missing (`markVodRootComplete`, called by `settleVodDownload` from the same verdict that sets `incomplete_tail`, and written before the `Muxing` status so a restart mux sees it too). A `complete` root makes the finalize supersede the parts after `muxUnrecordedSegments` has recorded any still unmuxed: each part's file and per-part chat move beside the archive as `<stem>.restart-<part start>.mp4` siblings (`asideOutputPath`), which the output sweep folds under the archive's stem rather than offering as strays, every recorded part's `seg_N` dir is tombstoned (`mergeTombstoneFile`) — `seg_0` included, which still keeps the root from reading as part 0, because that rule (`rootIsPartZero`, `internal/worker/orchestrator_mux.go`, consulted by `muxUnrecordedSegments` and `hasUnmuxedSegmentParts`) asks whether the dir exists, not whether it is a live part: read through the tombstone-skipping part list, a part dir left with no row (an empty `seg_N` from an interrupted split, or one whose media FFmpeg cannot read) made the complete download an unmuxed part 0 again, kept staging for it and let each Mux write the whole VOD out as one more full-length sibling — and `ClearJobSegmentsAndGaps` drops the rows — then the single-file path muxes the download as the archive. Nothing is deleted, because a live part is not guaranteed to be inside the VOD (YouTube trims very long archives, and a creator can edit one). It runs before the archive mux, since a part an earlier finalize renamed to the plain name sits exactly where the archive is about to be written, and it is re-entrant: a part whose file already moved is skipped. A download that ended incomplete leaves the parts as the archive and Resume comes back for the tail. Whatever root recording a finalize did not use, `cleanupStagingAfterMux` keeps the staging dir for (`unusedRootRecording`, `internal/worker/vod_supersede.go`), naming it in its Warn: beside a job that finalized as parts, a claimed root or the whole-file `video.mp4`/`audio.m4a` pair no part is ever made from; beside a single-file finalize, a whole-file download (`video.mp4` or `audio.m4a`) sharing the root with any live-shape capture (`video_stream`, `audio_stream` or `video.ts` — an audio-only job's stale `audio_stream` outranks its complete `audio.m4a` in discovery just as `video_stream` outranks `video.mp4`) — the layout `setAsideLiveShapesForVod` now prevents, which a staging dir from before it can still hold. The orphan sweep's `jobNeedsStaging` (`internal/worker/orphans.go`) applies the same shield, with no age rule, so the Files tab never offers that dir for deletion either.

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
- The opposite shape to the live loop: a fixed worker pool fetches the whole playlist in parallel and a byte-bounded reorder buffer writes the segments in ascending order as they land (`runHlsVodParallel` in `internal/engine/downloader_hls.go`). The ceiling is the same operator-settable per-job ceiling the DASH catch-up path uses (`downloader.reorder_buffer_mb`; `catchUpBufferBytes` in `internal/engine/downloader.go` is only what applies before `ConfigureReorder` has run), and it is what stops the other workers holding the rest of the VOD in RAM while the head-of-order segment works through its retry ladder — a worker waits for room instead of buffering past it, and the head is always admitted so the flush position cannot deadlock. Failed segments become nil gap sentinels so the consumer never wedges on an index that is not coming

**VOD direct download mode (`runDirectDownload`):**
- Probes total file size with a `Range: bytes=0-0` **GET** (`probeFileSize` in `internal/engine/downloader_fetch.go` — never a HEAD; a 200 answer means the origin ignored the Range header, and its body is dropped unread rather than pulled). The probe is retried up to three times with backoff before the caller gives up on Range support (`probeFileSizeWithRetry`), so one transient failure cannot route a resumable download into the streaming fallback
- Downloads in 5MB chunks (`DownloadChunkSize`) using Range requests
- Per-chunk retry (up to 3 attempts, `MaxChunkRetries`, at 1 s / 2 s) for a 5xx or a failure with no complete answer — no response at all, or a 206 whose body broke off mid-read, which used to fail the job on its first occurrence (`fetchChunkWithRetry` in `internal/engine/downloader_fetch.go`)
- Two failures are not charged against those attempts, as on the segmented path. A 403 or 410 — a googlevideo URL lives about six hours, and a long transfer outlives it — asks `OnCredentialRefresh` for a fresh URL and token and retries the chunk on it, at most `directRefreshAttempts` (2) times per chunk; the fresh URL must carry the same fingerprint as the current one (`refreshDirectURL` in `internal/engine/downloader_direct.go`), so a refresh can never append another rendition to the partial. A failure while `IsOnline` reports the device offline waits for connectivity and retries; and before a failure with no complete answer is charged as the last attempt, the monitor is given three of its polls to call an outage (`awaitOutageVerdict`) — a reset, a refused connection or a DNS miss exhausts the ladder in three seconds, long before the monitor's second failed poll. A cancel during that wait is returned as the cancel. Before this, an expired URL's 403 ended the job after one attempt and an outage longer than ~3 s did too. Any other 4xx still fails the chunk at once
- The size probe and the streaming fallback answer the same two failures the same way. The probe (`probeFileSizeWithRetry`) refreshes on a 403 or 410, at most `directRefreshAttempts` times, and waits out a probe that got no answer while the device is offline — asking for the outage verdict before its last attempt — none of it charged against its three attempts; a refresh it cannot use, a URL still refused once the refreshes are spent, or a cancel ends the download as an error that keeps the sidecar. The fallback (`runDirectDownloadFallback`, each request in `streamDirectOnce`) refreshes on a 403 or 410, at most `directRefreshAttempts` times without a byte written between them, and waits out a request that got no complete answer — none at all, or a body that broke off — when the device is offline or the monitor, given its verdict window, calls an outage; either way it asks again from wherever the file stands, with the resume Range. A URL that expired before the first request used to spend the probe's three attempts on its 403 and reach the fallback, whose one request then ended the job on the same 403; an outage as the download started did the same, and one connection dropped mid-stream ended a fallback transfer however much had streamed. Pinned by `TestDirectSizeProbeRefusedRefreshesTheURL`, `TestDirectSizeProbeRefreshIsBounded`, `TestDirectSizeProbeWaitsOutAnOutage`, `TestDirectSizeProbeChargesNeitherARefreshNorAnOutage`, `TestDirectSizeProbeOutageVerdictHonoursCancel`, `TestDirectFallbackRefusedRefreshesTheURL`, `TestDirectFallbackRefreshIsBounded`, `TestDirectFallbackWaitsOutAnOutage` and `TestDirectFallbackOutageVerdictHonoursCancel` (`internal/engine/downloader_direct_probe_fallback_test.go`)
- A 416 or an empty 206 while the offset is still below the probed total is a short origin, not the end of the file: the loop returns an error (`directShortFileError` in `internal/engine/downloader_direct.go`) and keeps the resume sidecar, where it used to break, clear the sidecar and hand a truncated file to a header-only validation. The streaming fallback reads a 416 at its resume offset as "already complete" only when no total it knows says otherwise — the one the 416 states (`bytes */<total>`) or the one the sidecar recorded; a total past the offset is the same short origin, and one naming another file restarts it (below). Pinned by `TestDirectChunkedLoopShortOriginIsAnError` (`internal/engine/downloader_direct_resume_test.go`) and `TestDirectFallbackResume416HoldsThePartialToItsFile` (`internal/engine/downloader_direct_identity_test.go`)
- Falls back to streaming download if server doesn't support Range — one response for the whole file, bounded by the read-progress deadline alone, asked again only after a refusal it refreshed or an outage it waited out (above)
- Saves a resume sidecar every 50 MB (`directResumeInterval` in `internal/engine/downloader_direct.go`) on both the chunked and the streaming path; before Arc E the whole-file path wrote none at all, so an interrupted VOD restarted from byte 0 however far it had got
- The sidecar records the probed total (`totalSize`), and a resumed partial is held to it: when the resume's own probe answers a different total, or the partial is already longer than the file, the bytes are a prefix of a different file and the download starts over (`discardStagedMedia`) instead of appending (`differentFileReason` in `internal/engine/downloader_direct.go`). A resume whose probe failed reaches the streaming fallback instead, which holds the partial to the total its resume Range's 206 or 416 states (`parseContentRangeTotal`) and on a different file discards and streams from byte 0 (`restartDirectFallback`). A discard forgets the recorded total with the bytes. The YouTube VOD strategy also sets a per-stream `StreamID` (video, itag, `clen`), so a restart whose format selection moved never splices one rendition onto another (see The Whole-File VOD Path in platform-services.md)
- Progress reported as percentage

**Resume capability:**
- `.resume.json` file stores: `lastSeq`, `bytesWritten`, `timestamp`, `baseUrl`, `streamId`, and for a whole-file download `totalSize`
- On resume: validates IDENTITY via `resumeIdentityMismatch` (explicit StreamID first, then YouTube URL fingerprinting — a finished VOD's `id=o-…` URL by its itag and `clen`; opaque no-identity URLs are trusted — see data-and-storage.md), then file size vs saved bytes
- DB-level fallback: if resume file is lost but database has `last_video_seq`/`last_audio_seq`, uses file size as byte position
- The media file is fsync'd before every sidecar save (`syncMediaFile` in `internal/engine/downloader_resume.go`) and the sidecar itself is written fsync+rename, so neither durable position can lead the durable bytes after a power loss; a failed media fsync skips that one save rather than recording a position it cannot back
- With a usable sidecar the file is truncated to the saved byte position and the missing tail appended. With NO usable sidecar the engine never truncates non-empty staged media: `StopOnGap` callers (Twitch live) get `ErrGapDetected` and gap-split instead, and every other caller gets `ErrStagedMediaPresent` so the orchestrator decides. One deliberate exemption: a whole-file VOD download (`IsDirectURL`) is out of the guard's scope — its partial is not segmented staged media and is always re-fetchable from the same static URL, so restarting it costs bandwidth rather than footage (and the 50 MB sidecar cadence above bounds even that)
- The one caller that genuinely requires a file starting at sequence 0 — the manifest-free DASH restart, via `DiscardStaged` — cannot refuse, so it preserves instead: `preserveStagedRecording` in `internal/engine/downloader.go` renames the headed recording ASIDE as `<file>.restart-<unix ts>` (its sidecar follows as `.restart-<unix ts>.resume.json`) and opens the fresh file beside it. Only bytes carrying no container header are discarded
- The whole-file VOD download sets aside by the same convention, from the worker side: a job that comes back as a finished VOD after an earlier live or post-live attempt finds that attempt's `video_stream` / `audio_stream` / `video.ts` still in its staging dir, and `discoverStagingMedia` (`internal/worker/orchestrator_mux.go`) ranks those names above `video.mp4`, so the restart mux and the Mux action used to archive the stale partial capture and delete the complete download with staging. `DownloadVod` (`internal/worker/strategy_youtube_vod.go`) therefore calls `setAsideLiveShapesForVod` (`internal/worker/staging_shapes.go`) once its formats are resolved: every live-shape file is renamed `<file>.restart-<unix ts>` under one stamp shared by the video and audio halves, its sidecar moved with it (`setAsideStagedMedia`) — a `video.ts` left by an HLS interlude in another session takes the same stamp, and is still its own recording, because the grouping keys on the recording a name belongs to as well as the stamp; an empty one is removed; a rename that fails fails the run rather than download beside a capture discovery would prefer. The dir then holds one current recording, and the earlier one is an ordinary aside — muxed to its own sibling at finalize, never into the archive. Ranking the shapes by mtime was the alternative and answers the wrong question: the newest file is not the complete one
- An aside is recovered at finalize and never merged: it overlaps the fresh recording from sequence 0, so `muxStagedAsides` in `internal/worker/orchestrator_mux.go` muxes each restart's GROUP of asides into its own sibling file beside the archive (`{name}.restart-<ts>.mp4`) and records no segment row. One group per (staging dir, timestamp, recording): a DASH restart sets aside the video and audio halves together, normally under the same second, and `groupStagedAsides` recombines that pair into a single file rather than two. The recording a stem belongs to (`asideRecordingOf`) is part of the key because a stamp alone is not one recording: `video_stream`/`audio_stream` are one DASH capture and `video.mp4`/`audio.m4a` one whole-file download, while `video.ts` is always its own — grouped by the stamp alone, a `video.ts` set aside beside a DASH pair under the pair's stamp had no slot in the pair's group, and the finalize removed it with the pair without ever muxing it. Each half is stamped by its own downloader's clock read, so a restart that straddles a second boundary stamps them a second apart; `pairStraddledHalves` rejoins a video-only and an audio-only group of the same dir and recording one second apart (two separate restarts are never that close, and a `video.ts` is never an `audio_stream`'s other half). A recovered aside gets a `.recovered` marker beside it, holding the sibling's path (`asideRecoveredMarker`), once its copy is verified and BEFORE it is removed; the marker outlives the aside only when the removal fails (a Windows handle on it) or the process dies in between: every aside scan skips a marked aside, so the next finalize or recovery does not mux it again to `-2`, and `removeRecoveredAsides` retries the removal at the start of each `muxStagedAsides` pass. Until it is muxed it counts as an unmuxed part, which keeps the whole staging dir from being swept, and the orphan sweep (`internal/worker/orphans.go`) lists any it finds under that dir's asides. Neither shield ages an aside out — unlike the tail and chat keeps beside them, which expire on `incomplete_staging_expiry_days` — because finalize muxes every READABLE aside, so one still in staging is footage FFmpeg could not read — or could not copy whole: a sibling shorter than its aside fails the same ENGINE-9 duration check the main and part muxes apply (`verifyMuxedDuration`), the short copy is discarded and the aside kept, since it is deleted only once its sibling is verified — and that exists nowhere else. Recovery is also reachable ON DEMAND, which is what a staging directory holding nothing BUT asides needs: `RecoverAsides` (`internal/worker/worker.go`) runs `recoverAsides` (`internal/worker/orchestrator_mux.go`) for one job — the same sibling naming, beside the job's recorded archive and under its stem when it has one (`recordedArchiveLocation`, the part base for a split job, where the finalize put any it recovered — re-resolving from the current title sent them to a stem no archive has, which the output sweep then offered as strays; the sweep also knows a split job's part base as a stem now), the fresh template name only for a job with no archive yet, the same best-effort-per-group behaviour, under the same mux root and the same download slot `MuxJob` (`internal/worker/worker.go`) takes — then copies the chat capture a preserved directory is still holding (`findKeptChatCapture`, `internal/worker/orchestrator_mux.go`, matched with `keepOnlyChatCapture`'s own rule in `internal/worker/worker.go`) beside the FIRST recovered file as `<stem>.chat.json` (`copyKeptChatSidecar`, `internal/worker/orchestrator_mux.go`), unless the job's own finalize already wrote that archive. A partial success is reported, never swallowed: a group FFmpeg could not read stays in staging, where the shield keeps it. **Staging is reclaimed only when it holds no recognised media (`discoverStagingMedia`, `internal/worker/orchestrator_mux.go`) and no unmuxed part** — and only once a fresh read of the job's row still finds it not active (`IsActiveJobStatus`, `internal/worker/orphans.go`), because a revival can hand that same directory to a new download before its first files take a name `discoverStagingMedia` recognises — `cleanupStagingAfterMux` (`internal/worker/worker.go`) is then left to apply its own incomplete-tail and chat-capture carve-outs, so a chat-incomplete job is pruned down to its chat rather than deleted. The guard is not belt-and-braces: with the asides consumed, none of that function's shields — set-aside recordings, an unmuxed part (`hasUnmuxedSegmentParts`), a root recording the finalize did not use (`unusedRootRecording`), the incomplete tail, the incomplete chat capture — covers a Cancelled job whose staging still holds the fresh recording beside the aside (`hasUnmuxedSegmentParts` is false with no `seg_N` dirs, and a single-file root holding one recording has none unused), and an unguarded reclaim would delete the very recording the Mux action exists to rescue. This is its OWN verb and not a widening of the Mux action: `HasSegmentFiles` (`internal/worker/staging.go`) deliberately does not know the suffix, so `MuxJob` and `POST /api/jobs/{id}/mux` still mean "mux the recording". It refuses on an active job (`IsActiveJobStatus`, `internal/worker/orphans.go`), on a job with nothing set aside, and while another off-queue operation holds the job's staging — a per-job claim (`claimJobOperation`, `internal/worker/worker.go`) that `MuxJob` takes too, because both verbs reach `muxStagedAsides` with their own `asideOutputPath` (`internal/worker/orchestrator_mux.go`) collision map and would write the same name. The job's status is never written: recovery is not part of the job's lifecycle. That is also why the run brackets itself with `TrackJobForLogs` and `restoreLogRouting` (`internal/worker/worker.go`) — `RouteLogToJobs` (`internal/database/database_jobs.go`) scans only the tracked set and `SyncJobLogTracking` (`internal/database/database_jobs.go`) removes every terminal job from it, so without the bracket a verb that runs exclusively on terminal jobs would log into nothing. `ScanAsides` (`internal/worker/orchestrator_mux.go`) is the read side both UIs render

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
- **Reorder buffer ceilings** (`downloader.reorder_buffer_mb` per job, `downloader.reorder_budget_mb` process-wide; defaults 1024/4096 MB, or 256/1024 on arm64) — bound the buffers by bytes rather than segment count, because a wider `segment_workers` pool would otherwise scale buffered memory with a throughput setting (at 1080p60's 3.7-6.2 MB segments, sixteen workers on a count-bounded buffer held ~250 MB regardless of the count chosen). The per-job ceiling is what one download may hold; the process-wide budget is the sum across every live buffer, which nothing bounded before — at the per-job ceiling alone, N concurrent downloads could hold N ceilings. A worker blocks rather than buffering past either; the head-of-window segment is always admitted at BOTH levels so the flush position can never deadlock (nothing frees either ceiling without a flush, and nothing flushes without its head), and its bytes are still charged so a teardown frees exactly what it took; segments above the lowest known-failed sequence are dropped rather than held, since they cannot flush until that failure resolves. Resident memory per download is therefore roughly the per-job ceiling + `segment_workers` × segment size, and across the process roughly the budget + one head segment per download — not a function of the claim window width. Both values reach the engine through `ConfigureReorder` (`internal/engine/reorder_budget.go`), called once at boot and again on every config save; `0` means unbounded on either. The arm64-only defaults come from `platformDefaults` (`internal/config/config.go`), which is the config package's only platform-conditional default and states the rule for any future one: arm-motivated caps apply only on arm64.
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
2. Re-fetches video info FIRST, to tell a real change from a transient blip. On YouTube (`qualityChangeInfo` in `internal/worker/orchestrator_youtube.go`) a failed fetch is retried every 5 minutes (`streamEndVerifyInterval`) until segments have been quiet for 10 (`streamSegmentTimeout`) — the verify branch's own clock — and only then ends the job in Error with staging intact; one transient API fault used to do that at once. Inside a resume wait that clock always reads too long (the downloaders are cancelled), so there the wait's own `interruption_timeout` deadline bounds the retries instead (`qualityChangeQuiet`)
3. If the refreshed quality matches, continues in the same staging directory with fresh downloaders — no split
4. Otherwise muxes the current segment in a goroutine parented by the orchestrator's mux root (`muxRoot` in `internal/worker/quality_split_common.go`), not `context.Background()`, and records it in the `segments` database table
5. Creates new downloaders at the new quality in the next part's staging directory
6. Updates the monitor baseline to the new quality

### JobQueue

The `JobQueue` implements a two-tier concurrency model:

**Lifecycle tier (100 slots):**
- Gates how many jobs can be in the DOWNLOAD half of the pipeline simultaneously — downloading plus muxing, which is exactly what `LifecycleCount()` reports
- The slot is claimed at the download decision, not at dequeue: `processJob` calls `AcquireLifecycleSlot` in `internal/worker/queue.go` only once `StreamProcessor.Process` has answered "should download", and every exit path from there releases it through the deferred `Complete` (owner decision O-F)
- Stream probing and the wait for a stream to go live therefore run slot-free. They used to hold a slot for the whole wait — hours to days for an `Upcoming` job or a manually-added offline Twitch channel — and at 100 waiters a newly live stream was never started at all
- A VOD takes its download slot FIRST and only then the lifecycle slot, so one queueing for the download pool holds no lifecycle slot. The other order let enough admitted backlog (Σ `archive_slots` across channels) fill the lifecycle pool with VODs that were merely waiting, and a live broadcast — which never waits on the download pool — blocked behind them and lost footage
- A wait that outlasts `lifecycleWaitWarnAfter` in `internal/worker/queue.go` (30 s) logs one line, once per wait, naming the job and the slots held: at the cap the symptom is a capture that simply does not start, and until that line existed nothing explained it

**Download tier (configurable, default 10 slots):**
- Gates how many VOD jobs can be actively downloading segments in parallel — VODs ONLY. Broadcasts pass through ungated (`acquireDownloadSlot` with `isVod=false` is a no-op): a missed slot on a VOD delays a file that already exists, a missed slot on a live broadcast loses footage. Peak concurrent downloads is therefore (live broadcasts) + `num_parallel_downloads`
- **Not the same knob as `downloader.segment_workers`.** `num_parallel_downloads` gates concurrent VOD **jobs**; `segment_workers` (default 12, no upper limit — see the constants table under SegmentDownloader below) gates concurrent **segment fetches within one download**, live or VOD. Because broadcasts bypass the download tier entirely, `num_parallel_downloads` has zero effect on a live stream's catch-up rate — confusing the two cost real debugging time when the owner had it set to 1000 with no observable change to catch-up throughput. Segment-level concurrency is what `segment_workers` controls.
- A VOD job acquires a download slot via `AcquireDownloadSlot()` after stream processing. It tries first (`TryAcquireDownloadSlot`), and only a VOD that actually has to queue shows it: its progress line reads `vodSlotWaitProgress` (`internal/worker/vod_slot_wait.go`) for the length of the wait and is cleared however the wait ends. Stream processing writes no status for a VOD (`vodStatusUpdates`, `internal/worker/stream_processor.go`; `processTwitchVod`, `internal/worker/stream_processor_twitch.go`, for a Twitch one) — the row keeps the one it came in with, Upcoming for a freshly classified VOD — and `ExecuteWithChat` (`ExecuteTwitch` for Twitch) writes Downloading once the slot is held; before, every queued VOD read Downloading through the whole wait, and a Twitch VOD went on doing so after a YouTube one had stopped
- The format URLs were extracted BEFORE both waits, and a googlevideo URL lives ~6 h, so a VOD that queued longer started a whole-file download on an expired URL and failed its first request with a 403 nothing retried. Once both slots are held, `processJob` re-extracts a stale extraction (`refreshStaleVodInfo` → `StreamProcessor.RefreshVodInfo`): one `vodInfoMaxAge` (1 h) old, or whose format URLs say through their own `expire=` (query or `/expire/<unix>/` path form) that they lapse within that hour (`vodInfoStale`). The re-extraction applies the same playability verdict as the first, and refuses a stream no longer classified as finished; either answer is returned marked as the verdict it is (`vodRefreshVerdict`, via `judgeRefreshedVodInfo`), so it ends the job where it sends it — `COOKIES?` or Error — as the same answer up front would. Stale is a precaution, not a verdict, though: a re-extraction that fails transiently (`classifyProbeErr`) while every format URL the extraction holds states an expiry still ahead (`vodURLsUnexpired`) leaves the extraction in place, and the download goes ahead on it — an expiry it then runs into is the downloader's to refresh (`OnCredentialRefresh`), as one partway through any long transfer is. Such a failure used to end the run, and a VOD whose URLs still worked ended in Error over a fetch it did not need. URLs already expired or carrying no expiry to read still take the failure
- Released via `ReleaseDownloadSlot()` after download completes but before muxing
- This allows muxing to proceed without blocking download slots (muxing is CPU-bound, not network-bound)

**Priority system:**
- `Live` = 1 (highest priority)
- everything else (`Upcoming`, `Downloading`) = 0 — every retry path writes one of those before it enqueues
- FIFO among jobs with equal priority

**Queue operations:**
- `Enqueue(jobID, status)`: Adds to pending queue. O(1) duplicate detection via `pendingSet`. Backlog limit of 100 pending jobs; drops with warning if full.
- `Dequeue(ctx) -> (jobID, jobCtx, ok)`: Blocks until a pending job exists — there is no lifecycle gate here. Returns a per-job cancellable context.
- `AcquireLifecycleSlot(ctx, jobID) -> bool`: Blocks until one of the 100 lifecycle slots is free, then claims it for the job. Returns false if context cancelled. Warns once per wait past 30 s.
- `AcquireDownloadSlot(ctx, jobID) -> bool`: Blocks until a download slot is free. Returns false if context cancelled.
- `TryAcquireDownloadSlot(jobID) -> bool`: Takes a free download slot without waiting; `AcquireDownloadSlot` is this in a loop.
- `ReleaseDownloadSlot(jobID)`: Frees the download slot. Signals waiting jobs.
- `ReleaseSlots(jobID)`: Frees the lifecycle and download slots (if held) without ending the run. `setJobError` and `handleCancellation` call it before their tails (notifications, an automatic cookie refresh), so the next download need not wait on them.
- `Complete(jobID)`: Ends the run — frees any slot still held, cancels the per-job context, unregisters it and closes its `Done` channel. Called once per run, by `processJob`'s defer, when the goroutine returns. An early call used to unregister a run still in its tail, so a re-enqueue in that window started a second run, which the first run's deferred `Complete` then tore down; whatever re-enqueues a job from inside its own run (the automatic cookie refresh's resume, `AutoReinitializeJob`) now waits for the exit through `afterJobExit`.
- `Cancel(jobID)`: User-initiated cancellation. Sets `cancelled` flag, cancels context, removes from pending queue. Returns whether it flagged a run — and a run that has settled its outcome is not flagged (`settle`, below), so its caller sends the Job Cancelled notification itself.
- `WasCancelled(jobID) -> bool`: Returns and clears the cancellation flag. Used to distinguish user cancellation from shutdown. Settles the run.
- `settle(jobID) -> bool`: The point past which a run no longer reports a Cancel. `setJobError` and the backlog requeue call it as they record their outcome, and it reports whether a Cancel flagged the run first — the run then ends as a cancelled one (`handleCancellation`) and sends the Job Cancelled its canceller left to it. Otherwise the run is settled, and `Cancel` no longer flags it. The queue's lock decides which came first: checked and written apart, a failure written between `CancelJob`'s flag and its `Cancelled` write sent Job Failed for the operator's Cancel, and a Cancel that flagged a run already past reading the flag — a failure's tail, which an automatic cookie refresh can hold for minutes, or a requeue — was reported by nobody.

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
- The admission write is a compare-and-set on `Queued` (`UpdateJobFieldsIf`, `internal/database/database.go`). Written unconditionally, it overwrote an operator's Cancel that landed between `NextQueuedJobs` and the write, and the cancelled job downloaded after all. A row that left `Queued` in that window is not admitted and takes no slot: the next row the query returned gets it, or the next sweep does — not a re-query or a `Wake`, which a database that cannot write would turn into a spin. The worker's own failure writes take the same care: `setJobError` and the backlog requeue write with `UpdateJobFieldsUnless(..., Cancelled, ...)`, and a failure that a Cancel flagged first (`JobQueue.settle`), or that finds the row Cancelled, ends the run as a cancelled one (`handleCancellation`) instead of turning it into Error or back into `Queued`. The Job Cancelled notification is sent once either way: by the run when the Cancel flagged it first, and by the cancel route or the TUI when the run had already settled
- `resolveSlots` is injected by `cmd/moombox` against the live config store, so per-channel `archive_slots` overrides hot-reload
- A **disabled** channel resolves to 0 slots, so disabling it PAUSES its queued backlog (owner decision O-J, 2026-09-17). Every discovery path already reads `enabled = false` as a pause — the feed, DECAPI and Twitch monitors skip the channel, and the backfill keeps it in `active` while never scanning it — and the resolver was the one place that did not, so a disabled channel went on starting downloads M at a time. In-flight jobs are untouched: they have already left `Queued`, and this number is an admission budget rather than a kill switch. A channel with **no config entry at all** still gets the global default, so a removed channel's leftover `Queued` rows are not stranded.
- **No admissions during an outage.** A sweep admits nothing while the connectivity monitor (`Scheduler.conn`, the worker's `Connectivity`) reports offline, and `Run` subscribes to its `OnStateChange` so the moment it reports online again is a wake. An admission made offline is a backlog VOD sent to fail its first fetch, and each one that failed freed its slot for the next: a channel's whole `Queued` backlog drained into Error in the minutes the network was down, and the archive pass never re-creates a video it has history for
- **No admissions onto a full disk.** A sweep with a backlog to admit first reads the volume the jobs write to and admits nothing while it is at or past `disk_critical_percent` (`Scheduler.diskGateClosed`, `internal/worker/scheduler.go`). The reading is the disk alerts' own — `paths.output_directory` through `disk.GetDiskSpace`, judged by `DiskConfig.AtCritical` (`internal/config/types.go`), the rule `ComputeWarnLevel` (`internal/web/routes/stats.go`) applies — taken fresh by `DownloadWorker.readOutputDisk` (`internal/worker/disk_gate.go`) rather than from the dashboard's last reading, which can be six minutes old. There is no setting of its own. Every sweep reads again, the 60 s heartbeat's included, and nothing signals space being freed, so a held backlog resumes within a heartbeat of the volume dropping below the threshold — on the first reading below it, with none of the alert's 2-point recovery margin (`diskRecoveryMargin`). The gate's close and its reopen are each logged once; a reading that fails leaves the gate where the last good one put it, as the alerts freeze on theirs. Only backlog admission waits: live, upcoming and manually added jobs never pass through the scheduler, and a backlog VOD already admitted runs on. A sweep with nothing `Queued` takes no reading. The reading is the output directory's volume only — a staging directory or a per-channel output directory on another volume is not what it measures, as it is not what the alerts measure
- **Transient pre-download failures go back to `Queued`.** A backlog job whose stream-processing fetch — or the re-extraction a stale VOD makes once its slots are held, when the URLs it would replace have expired or state no expiry — fails transiently (`classifyProbeErr`, `internal/worker/probe_classify.go`: network, timeout, 429/5xx and whatever it cannot place) returns to `Queued` instead of Error (`requeueBacklogAfterTransientFailure`, `internal/worker/backlog_retry.go`), only where `CookieResumeStatus` would send it there — priority 1 with its `feed_items` partner — and held from re-admission for a backoff (5, 10, 20 minutes: `backlogRetryBackoff` doubling). A sweep skips a held job without costing its channel the turn, asking `NextQueuedJobs` for enough rows that every hold could be among them. After `backlogRetryLimit` (3) runs in a row that each ended back in `Queued` the job ends in Error, saying so, so a video that is really gone still gets there; a definitive refusal (a 404, a playability verdict) is not retried — the re-extraction's verdict included, which reaches the worker as an error whose text `classifyProbeErr` cannot place and is recognised first (`isVodRefreshVerdict`). Holds and counts live in memory: a restart forgets a backoff and grants a fresh budget, never loses the job
- **A download that runs out of disk goes back to `Queued` too.** A backlog job whose download — or the mux after it — fails because the disk is full (`isDiskFull`, `internal/worker/disk_full.go`: `ENOSPC` on unix, `ERROR_DISK_FULL` / `ERROR_HANDLE_DISK_FULL` on Windows anywhere in the chain, which is how `engine.ErrLocalWrite` carries the engine's writes; or the out-of-space text, which is how FFmpeg's stderr tail and the engine's `%v`-flattened HLS init write carry it) takes the same requeue, backoff and budget (`requeueBacklogAfterDiskFull`, `internal/worker/backlog_retry.go`), and after the backoff waits on the disk gate above for as long as the volume reads critical. Staging is kept: a whole-file VOD resumes from its last resume checkpoint, while one whose MUX ran out of space downloads again, since a completed download clears its resume state. The budget is one streak for both kinds: it counts the runs that ended back in `Queued`, and only a run that ends otherwise — finished, Error or `COOKIES?`, cancelled — resets it (`forgetBacklogRetries`, `internal/worker/backlog_retry.go`). A job waiting in `Queued` has no run to end, so the operator's verbs end its streak themselves — Cancel, Retry and Resume, and a deleted row with it, drop the count and the hold (`endBacklogStreak`): a backlog VOD cancelled while it waited for space and then retried used to give up into Error on its first requeue, its old count still standing. A fetch that succeeds no longer resets it, as it did when only fetches were retried: every run whose download then runs out of disk follows one, and the budget would never be spent. A broadcast or a manually added video that runs out of disk still ends in Error
- `Run` performs one admission sweep before entering its wait: the wake sites are all event-driven — backlog creation, job completion, a cookie repair — and a restart has none of them, so leftover `Queued` rows used to wait for the 60 s heartbeat. The sweep is gated to the first pass of `Run`, so a `sweep()` that panics deterministically restarts on the heartbeat cadence rather than every second

### ProgressTracker

The `ProgressTracker` aggregates progress from video, audio, and chat downloaders and persists to the database:

- Update throttling: one report per `downloader.progress_interval_ms` (16ms by default), with gap rows flushed to the database at most once a second (the job row is written on every report). The TUI's own progress tick is finer — 8ms, one per frame at its 120 fps renderer — so the engine's gate, not the tick, is what bounds the rate
- Progress string format (`buildProgressString`, `internal/worker/progress.go`): `"(V: 1234/1300 A: 1234/1300 C: 900)"` for a DASH pair (segment / head per stream, the `/head` part only once it is known, chat messages last and only once there are some); `"Seq: 1234 C: 900"` for a single HLS stream; `"V:95.3% C: 900"` for a byte-ranged VOD. A YouTube VOD that completes writes `"V:100% A:100% C: 900"`, and one finalized behind head writes the DASH shape with its real counts instead (`incompleteProgressString`, `internal/worker/orchestrator.go`). While a download waits on something, the line is that wait instead (`activityMessage`, e.g. `"Verifying stream ended... (2m 10s)"`)
- Speed calculation: bytes/second averaged over a 5 s sliding window (`speedAvgWindow`); the earlier EMA over report-cadence deltas jittered with each segment's arrival
- ETA calculation (`calculateETA`): a segmented download divides the segments still to come by the segment rate since the tracker started. A whole-file VOD instead counts only the bytes THIS session transferred (`vodStreamBytes`) — each stream's forward movement from its offset at the session's first progress event, so a resume's inherited bytes are never read as speed — measured from that first event, against what video and audio together still have to fetch, each against its own probed total. It used to subtract both streams' bytes, resume baseline included, from the video total alone and divide by the tracker's age, so a resumed VOD showed seconds where minutes remained. A stream opened by an event carrying its total goes on counting the streaming fallback's events, which carry none (`noteVodBytesLocked`): the chunked loop hands its transfer to that fallback on a mid-download 200, and reading only the chunked events froze the offset at the handoff while the clock ran on — the ETA climbed as the file neared completion, and the `V:` percent stayed where the handoff left it (it is now read off the total last stated). An offset below the last one — the fallback restarting the file — moves nothing. Pinned by `TestCalculateETAVODCountsOnlyThisSessionsBytes` and `TestVODProgressFollowsTheStreamingHandoff` (`internal/worker/progress_test.go`)
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

### Database Write Path

There is one job writer and it is synchronous. `UpdateJobFields()` (`internal/database/database.go`):
1. Builds the dynamic SET clause from the `fieldToColumn` map and executes the `UPDATE` immediately under `db.mu` (no channel, no batching goroutine, no coalescing window)
2. Re-reads the full job row through the prepared `stmtGetJob` in the same critical section (subscribers need all fields)
3. Releases `db.mu` BEFORE notifying, so a subscriber may call back into the database without deadlocking
4. Notifies `OnJobUpdate` and `OnJobChange` subscribers synchronously on the caller's goroutine; if the row vanished between the write and the read-back, fires `OnJobDeleted` instead

Its two conditional forms, `UpdateJobFieldsIf` and `UpdateJobFieldsUnless` (`internal/database/database.go`), are the same writer with a status condition ANDed into the `UPDATE`: a write the condition refuses stops after step 1 and reports false (see [data-and-storage.md](data-and-storage.md), Conditional writes).

The only goroutine the package ever starts is the `OnJobsChange` fan-out (`dispatchJobsChange`), used by the two bulk writers. Write amplification during a download is bounded upstream, not here: `ProgressTracker` (`internal/worker/progress.go`) reports at most once per job per configured progress interval (`downloader.progress_interval_ms`, 16 ms default) and flushes gap rows at most once a second, and every other `UpdateJobFields` caller is event-driven. When nothing is being written, nothing runs.

### WebSocket Broadcast Rate

The WebSocket hub throttles nothing. The highest-frequency caller (`OnJobChange` driven by `ProgressTracker.maybeUpdate`, capped to one report per job per configured progress interval — `progressUpdateInterval` is its 16ms default, and `downloader.progress_interval_ms` overrides it per install) now broadcasts the slim `job_progress` frame; `job_update` carries the state transitions, and `OnJobAdded`/`OnTrimsChanged` are event-driven. A previous per-job throttle in the hub created an ordering race where the trailing edge could arrive after a `BroadcastJobDeleted` and resurrect a deleted row via the client's upsert handler.

### TUI Async Updates

The TUI uses non-blocking channel sends to prevent the event loop from blocking:

- Job updates, log lines, and status changes are sent via buffered channels
- If a channel is full, the send is dropped and a drop counter is incremented
- Drop counters are logged periodically but are non-fatal
- Key timing intervals:
  - Main tick: 1 second
  - Progress tick (active download): 8ms (one per frame at the 120 fps renderer)
  - Progress tick (idle): 500ms
  - Marquee animation: 150ms
  - Log flush window: 250ms, ring buffer max 200 lines

### BotGuard Triple Cache

The PO token system uses three in-process cache layers to minimize expensive BotGuard operations. Caches are agnostic to which path (the sidecar, or goja when the sidecar is turned off) produced the token:

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
  COOKIES?                      COOKIES?
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
- `Upcoming` / `Downloading` -> `COOKIES?`: Auth failure detected (login required, members-only, cookies expired) — before any download too: the playability check that classifies a stream, its credential half re-read on the go-live fetch when that fetch serves no formats (`completeStreamTransition`, `internal/worker/stream_processor_youtube.go` — a members-only job whose cookies died during the wait used to reach "no download strategy available", a plain Error), and a members-only answer while waiting for it to go live, park an `Upcoming` row. Mid-capture, a live YouTube refresh whose player response is credential-walled with no formats (`liveCredentialFailure`, `internal/worker/orchestrator_youtube.go` — the quality-loss refresh, a part split's refresh and the stream-end verify's refresh) stops the capture with staging kept and parks the `Downloading` row the same way — once the wall has been read twice in a row (one re-read, on the retry cadence, before it is believed: a single walled answer to a healthy session would park a live capture for good, and a membership park waits for an account change nothing will make); it used to be retried and then finished like an ended stream
- `COOKIES?` -> `Upcoming` (priority 0) / `COOKIES?` -> `Queued` (priority 1, backlog, **when its `feed_items` row still exists**): a credential-recovery sweep resumed the job. A backlog row returns to `Queued` rather than `Upcoming` so the archive-slots scheduler re-admits it `archive_slots` at a time instead of releasing a channel's whole parked backlog at once. A parked backlog job of a REMOVED channel returns to `Upcoming` so it can finish at all: `CancelAndPrune` deletes the channel's `feed_items` rows but deliberately leaves a running download alone, and `NextQueuedJobs` INNER-JOINs `feed_items`, so a partnerless `Queued` row would never be admitted and `Queued` has no other exit (`/retry` and `/resume` both refuse it). Every resume — both sweeps (`resumeCookieParkedJobs`, `cmd/moombox/monitor_callbacks.go`) and the worker's own after an automatic cookie refresh succeeds (`attemptCookieRefresh`) — is a compare-and-set on `COOKIES?` (`UpdateJobFieldsIf`): both UIs offer Cancel on a parked row, and the refresh holds the parked run's tail for up to two minutes, so a resume written unconditionally turned the operator's `Cancelled` back into a download. The worker's resume still hands on a row a sweep resumed first — the refresh's own re-check sets the sweeps off before it returns — so it restarts when the parked run exits rather than on the heartbeat. Two sweeps exist and they are not interchangeable:
  - **Auth recovered** (`RefreshService.OnAuthRecovered`) fires when a platform goes from not-authenticated to authenticated, and offers the sweep no account identity. It resumes every park EXCEPT `park_reason = 'membership'`.
  - **Credential observation** (`RefreshService.OnCredentialsChanged`, both platforms — only YouTube can park a job on an account question) hands the sweep the account identity currently in the cookie file. A membership park then resumes if and only if that identity differs from the `park_identity` it recorded when it was refused.
  A membership park is excluded from the first sweep because it happened while the session was ALREADY authenticated, so that transition cannot be the event that fixes it -- resuming there bought a guaranteed-identical failure once per auth cycle. Only a different account can help.
- **Account identity** is `SHA-256(SAPISID || NUL || LOGIN_INFO)` (`cookies.CookieJar.YouTubeIdentity`). `LOGIN_INFO` is the load-bearing half: SAPISID identifies a Google *session*, not an account, which is why `internal/youtube/auth.go` must select the account separately via `X-Goog-AuthUser`/`X-Goog-PageId`. Switching the browser's active account -- the exact remedy Moombox prints for a not-a-member failure -- rewrites `LOGIN_INFO` and leaves SAPISID untouched, so a SAPISID-only fingerprint would be blind to it.
- **The resume decision is durable, not edge-triggered.** The per-job `park_identity` comparison is what moves a job; the observation callback only decides *when* to look. A missed observation therefore costs a delay until the next account change or restart, never a permanent strand. Its process-local baseline advances only on a check that both concluded AND authenticated -- otherwise a stale intermediate export (dead on arrival, then re-exported working) would consume the edge -- and its zero value fires once per process, which is how an offline swap (stop, replace cookies, start) is noticed at all.

### Cancellation Semantics

- **User cancellation:** `WasCancelled(jobID)` returns true. Status set to `Cancelled`. Notification sent.
- **Shutdown cancellation:** `WasCancelled(jobID)` returns false. Status is preserved (not changed). Job will resume on next startup.
- **Cancel of a settled run:** a run that has recorded its outcome — `setJobError`'s failure, a backlog requeue, or the end of a cancelled or interrupted run — is settled (`JobQueue.settle`), and a Cancel that reaches it in what is left of the run is not flagged: `CancelJob` answers false and its caller (the cancel route, the TUI) sends the notification. That covers a Cancel of a `COOKIES?` row whose run is still in its automatic cookie refresh, of a row just requeued to `Queued`, and the cancel route's own order — it writes `Cancelled` before it calls `CancelJob`, and that write alone can end the run first.

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

## Error Classification

There is no typed error hierarchy and no errors package under `internal/`. Errors are plain Go errors, wrapped with `fmt.Errorf("...: %w", err)` as they travel up, and the only classification anywhere is sentinel matching with `errors.Is`. The sentinels that matter to a job's fate:

| Sentinel | Declared in | Meaning |
|----------|-------------|---------|
| `ErrCookiesRequired` | `internal/worker/worker.go` | Player-API "login required" / "member-only" failure, or any explicit cookies-needed signal |
| `ErrNotAMember` | `internal/worker/worker.go` | Members-only content refused to a session YouTube confirmed was SIGNED IN — a membership problem, not a credential one |
| `ErrNonActionable` | `internal/worker/worker.go` | Nothing the user can do: age-restricted content, exhausted probe budgets |
| `ErrTwitchAuthExpired` | `internal/twitch/api.go` | Twitch auth token expired or invalid |
| `ErrSubscriberOnly` | `internal/twitch/api.go` | Subscriber-only content the logged-in account cannot reach |

### Error-to-Status Mapping

In `DownloadWorker.setJobError()` (`internal/worker/worker.go`):
```
cookiesStatusError(err)  ->  StatusCookies      // errors.Is against ErrCookiesRequired, ErrNotAMember,
                                                //   twitch.ErrTwitchAuthExpired, twitch.ErrSubscriberOnly
anything else            ->  StatusError
```
Cancellation is not an error class: `handleCancellation` asks the queue whether the user cancelled (`WasCancelled`) and writes `Cancelled`, or leaves the status untouched on a shutdown so the job resumes on restart. A Cancel that lands after `processJob` last read its context reaches `setJobError` all the same, with the failure the run was already returning. When it flagged the run before the failure settled (`JobQueue.settle`) — its `Cancelled` write may still be on the way — or its write already reads `Cancelled` (`UpdateJobFieldsUnless(..., Cancelled, ...)` does not apply over it), `setJobError` hands the run to `handleCancellation` instead of recording Error or sending Job Failed. A Cancel after that finds the run settled and is its caller's to report. The error's text becomes the job's `error` column, and `park_reason`/`park_identity` are written on every error transition so the credential sweeps can tell a dead-cookie park from a membership one.

### Notification and Recovery Suppression

- `errors.Is(err, ErrNonActionable)` suppresses the failure notification (an age-restricted stream or an exhausted probe budget was never going to succeed) and skips the automatic cookie refresh — recovery would re-queue the job and reset its retry budget. The report is suppressed, not the close: the "Job Failed" embed still goes out marked `SendOptions.EditOnly`, which on an edit-mode target holding the job's lifecycle message edits it to its terminal look and posts nothing else, without the mention, and everywhere else sends nothing (`dispatchOne`, `internal/notifications/lifecycle.go`). Suppressing the send outright left that message reading "Found" or "Downloading" for good. The Twitch retry suppression beside it sends nothing at all: the monitor restarts that job, and its next event goes on editing the same message.
- A Twitch flap still inside its auto-retry budget (`AutoRetryCount > 0` and the exact offline message with no delivered segments) is also silent, because the monitor will `AutoReinitializeJob` on its next poll; a terminal failure on a retried job does notify.
- A live Twitch capture that stops on its unconfirmed-end latch DOES notify — the job is in Error, and nothing yet says its broadcast is over — and the automatic mux that follows once the monitor confirms the end (D-T4, `AutoMuxEndedBroadcast`) is the Mux action's path: it notifies on Finished, as that path does, and a failure writes the row's error without a second embed.
- `ErrNotAMember` parks the job but skips the automatic cookie refresh (`cookieRefreshWorthAttempting`): the cookies are alive, they belong to the wrong account, and only a different account's credentials resume it.

## Key Types and Public API

### database.Job

The primary data model. See `internal/database/types.go` for the complete struct. Key fields:

- `ID` (string): Primary key. For YouTube: video ID. For Twitch: `tw_{streamID}` or `tw_manual_{login}_{timestamp}`.
- `VideoID` (string): Platform-specific video identifier.
- `Platform` (string): `"youtube"` or `"twitch"`.
- `Status` (JobStatus): Current lifecycle status.
- `Progress` (string): Human-readable progress (e.g., `"(V: 1234/1300 A: 1234/1300 C: 900)"`; the shapes are listed under ProgressTracker above).
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
- `Open(dbPath, logger) -> (*Database, error)`: Opens/creates database, runs migrations, prepares the hot-path statement.
- `AddJob(job) -> (bool, error)`: INSERT OR IGNORE. Returns false if duplicate.
- `GetJob(id) -> (*Job, error)`: Single job with gaps, trims, segments loaded.
- `GetAllJobs() -> ([]*Job, error)`: All jobs ordered by `updated_at DESC`.
- `UpdateJobFields(jobID, map[string]any)`: Dynamic partial update with auto `updated_at`. Triggers subscribers.
- `UpdateJobFieldsIf(jobID, expected, map[string]any) -> bool` / `UpdateJobFieldsUnless(jobID, unwanted, map[string]any) -> bool`: The same write, applied only while the row's status is `expected` (a compare-and-set) or is not `unwanted`; reports whether it applied, and triggers subscribers only when it did.
- `DeleteJob(id) -> error`: Hard delete with cascading gap/trim/segment cleanup.
- `OnJobUpdate(fn) -> unsubscribe` / `OnJobChange(fn) -> unsubscribe`: Subscribe to per-job update events; the latter also receives the list of columns written.
- `OnJobAdded(fn)` / `OnJobDeleted(fn)` / `OnTrimsChanged(fn) -> unsubscribe`: Lifecycle events of `AddJob`, `DeleteJob` and `AddTrim`/`DeleteTrim`.
- `OnJobsChange(fn) -> unsubscribe`: Subscribe to full-list refreshes. Fired only by the bulk writers `BatchSetWatched` and `DeleteJobsAndHistoryForChannel`, never by a single add or delete.
- `AddToHistory(videoID)`: Records video ID to prevent re-downloading.
- `IsInHistory(videoID) -> bool`: Checks if video was previously downloaded.
- `JobExists(id) -> bool`: O(1) existence check.

### worker.DownloadWorker

Key methods:
- `NewDownloadWorker(db, yt, cfg, logger, deps) -> *DownloadWorker`: Constructor.
- `Start(ctx)`: Main loop. Blocks (run in goroutine). Enqueues existing pending jobs, then dequeues and processes.
- `Stop()`: Signals stop, waits up to 10 seconds for in-flight jobs.
- `EnqueueJob(jobID)`: Adds a job to the queue with priority from its current status.
- `CancelJob(jobID) -> bool`: User-initiated cancellation. True when it flagged a run that will send the Job Cancelled notification; false — no run, or one that has settled its outcome — leaves it to the caller.
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
- `ExecuteTwitch(ctx, jobCtx, variant, isVod, twitchChat) -> error`: Full Twitch download pipeline. For live streams this is a *session loop*: the engine runs with `StopOnGap` (Twitch has no DVR), so an unrecoverable playlist gap muxes the current capture as a finished part (`{name} - partN.mp4` + rolled per-part chat) and continues at the live edge in a new `seg_N` staging dir; a connectivity outage pauses the session and resumes the SAME job once the same broadcast (stream_start_time identity, rechecked post-outage) is reachable again — one job per broadcast. Only a CONFIRMED end finalizes it: a post-outage recheck that gets no answer, or a master-playlist refresh that still fails after its retries (`postOutageRefreshAttempts`) while the broadcast is live or unverifiable, takes the same unknown-verdict exit as the in-loop refresh (which retries a failed master-playlist fetch `liveRefreshAttempts` times over about 100 s first — checking the stream before each pause and finalizing at once on a confirmed end — since nothing continues a Twitch capture from Error: `/resume` is YouTube-only, Retry starts over with fresh staging and the monitor recovers offline flaps only) — Error, staging kept — rather than marking a capture of a still-running broadcast Finished, which the monitor would then never re-archive. The exit is gated on the job's own context and the operator's cancel, never on the session's: the post-outage exits latch from the recovery loop, whose session context the outage has already cancelled, and gated on that they fell through to the shutdown path and wrote "context canceled" as the job's error. Since that gate no longer sees the outage, a latch the outage cut short inside its own re-verify is discarded where the outage branch takes over: only a latch the recovery itself takes, or the resumed session's own, reaches the exit, so a stale one neither errors a capture the resumed session completes nor overrides a recovery that confirms the broadcast over (which finalizes). That exit marks its error (`ErrTwitchEndUnconfirmed`, `internal/worker/twitch_end_unconfirmed.go`; the text is the download failure's own) and `setJobError` records the mark as `park_reason = 'twitch_end_unconfirmed'`, so once the Twitch monitor's poll shows that broadcast over — the channel offline, or live with a different broadcast (`TwitchBroadcastOver`: by stream ID when the job ID carries one, else by `stream_start_time`) — `DownloadWorker.AutoMuxEndedBroadcast` confirms it with the same two samples (`confirmTwitchLiveness`) and muxes the kept staging exactly as the Mux action does (owner decision D-T4). Once per failure: the marker is cleared as the mux starts (`muxJob`'s `Muxing` write, once it holds the job's staging claim) or with the failure a mux that cannot start writes, so a mux that fails leaves the row in Error with an error saying the automatic mux failed, and no later poll tries again. The one refusal that is not a failure is `ErrStagingBusy` — another operation, such as a set-aside recovery (`A S`), which leaves the row in Error while it runs, holds the job's staging — so no mux was attempted: the row keeps its marker and its own error, and the next poll that finds the broadcast over tries again. Only the live broadcast-end latch marks — a VOD's latch and a failed part advance do not — and the Twitch auto re-initialisation refuses a marked row (`isRecoverableTwitchError`), so a row is never both re-initialised and muxed. A job whose channel the config no longer holds is never polled, and only the Mux action archives it. A VOD has no live end to confirm, so an engine failure before its download completes returns an error before the `Muxing` write, and the job lands in Error with staging intact instead of being finalized as a truncated `Finished` file. A connectivity outage is not such a failure: the offline cancel is registered for live captures only, so on a VOD the engine's `IsOnline` wiring waits the outage out and the download carries on from where it stopped, as on the YouTube paths. That includes a segment that fails because of the outage: `fetchSegmentWithRetry` (the parallel VOD path, `internal/engine/downloader_fetch.go`) does not charge an attempt that fails offline, and the sequential HLS loop waits instead of counting it toward the stuck-segment skip, whose count restarts after every outage (`hlsOutages`) — both used to turn the segments in flight during a long outage into gaps — cancelling it used to end the job in Error, and Retry then downloaded the whole VOD again. On daemon restart, `discoverResumeSegment` maps staged `seg_N` dirs + recorded segment rows to the correct part so a resume never appends into an already-muxed part's staging file. Seamless continuation (sequence numbers still covered by the playlist window) keeps appending — splits happen only where data was actually lost. Both TS and fMP4/CMAF delivery are handled: on an fMP4 playlist the engine writes the `#EXT-X-MAP` init segment at the head of each part file before its first fragment (`ensureHlsInit`), tolerates token-rotated init URIs by content hash, and returns `ErrInitSegmentChanged` when the init genuinely changes mid-part (transcode restart) — handled like a gap split minus the lost-data notification, since the successor part re-requests the exact segment the engine refused to write. A quality split seeds its successor the same way, from the first sequence the closed part does not hold (Twitch variants share one sequence numbering), so the new part never repeats the closed part's last seconds; only a short part that is discarded rather than muxed leaves the successor to replay the playlist window, which keeps what the window still holds of the dropped span.

### worker.JobQueue

Key methods:
- `NewJobQueue(maxDownloads) -> *JobQueue`: Constructor (maxLifecycle fixed at 100).
- `Enqueue(jobID, status)`: Non-blocking add to pending queue.
- `Dequeue(ctx) -> (string, context.Context, bool)`: Blocking dequeue of the highest-priority pending job. No lifecycle gate.
- `AcquireLifecycleSlot(ctx, jobID) -> bool`: Blocking lifecycle-slot acquisition, taken at the download decision.
- `AcquireDownloadSlot(ctx, jobID) -> bool`: Blocking download slot acquisition.
- `ReleaseDownloadSlot(jobID)`: Non-blocking slot release.
- `ReleaseSlots(jobID)`: Free the slots, keep the run registered.
- `Complete(jobID)`: End the run — free all slots, cancel context, close `Done`.
- `Cancel(jobID) -> bool`: User cancellation; flags only a run that has not settled its outcome.
- `WasCancelled(jobID) -> bool`: Check and clear cancellation flag, and settle the run.
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
- [data-and-storage.md](data-and-storage.md) -- Database schema, migrations, config format, write path and pub/sub internals
- [security.md](security.md) -- Middleware stack, CSRF, auth flow, Ed25519 update verification
- [user-interfaces.md](user-interfaces.md) -- Web UI SPA architecture, TUI chord system, WebSocket protocol
- [operations.md](operations.md) -- Build process, release workflow, runtime requirements
- [appendix-metrics.md](appendix-metrics.md) -- All timing constants, thresholds, and limits in one place

### Key Source Files

- `cmd/moombox/main.go` -- Launcher and `run()` (~800 lines); service initialization is `services.go`, event wiring `monitor_callbacks.go` / `tui_wiring.go` / `ws_wiring.go`, shutdown `shutdown.go`
- `internal/worker/worker.go` -- DownloadWorker, processJob loop
- `internal/worker/queue.go` -- JobQueue with two-tier concurrency
- `internal/worker/stream_processor.go` -- StreamProcessor, waitForLive, Twitch processing
- `internal/worker/orchestrator.go` -- DownloadOrchestrator, `ExecuteWithChat` (the YouTube pipeline), VOD refresh loop
- `internal/worker/orchestrator_youtube.go` -- YouTube live loop (`runLiveStreamDownload`), stall refresh, probes
- `internal/worker/orchestrator_twitch.go` -- `ExecuteTwitch`, the Twitch session loop, gap/quality splits, outage recovery
- `internal/worker/orchestrator_mux.go` -- Mux and finalize (single file and multi-part), staging media discovery, set-aside recovery
- `internal/worker/part_merge.go` -- Tier 4 same-format part merge
- `internal/worker/orphans.go`, `internal/worker/output_claims.go` -- Files-tab orphan sweep and the in-flight output claims it honours
- `internal/worker/strategies.go` -- Strategy selection and shared download plumbing; the strategies themselves are `strategy_youtube_{vod,dash,manifestless_dash,hls}.go`
- `internal/worker/quality_monitor.go` -- QualityMonitor for live resolution tracking
- `internal/worker/quality.go` -- QualityInfo, quality label parsing
- `internal/worker/progress.go` -- ProgressTracker for throttled DB updates
- `internal/worker/trim.go` -- TrimService for clip creation
- `internal/worker/mux_finalize.go` -- `DownloadFileMinSize`, the asset (thumbnail) download
- `internal/engine/downloader.go` -- SegmentDownloader (DASH/HLS/VOD/Direct modes)
- `internal/engine/muxer.go` -- FFmpeg muxer and ffprobe wrapper
- `internal/database/database.go` -- Database open, `UpdateJobFields`, CRUD operations
- `internal/database/database_subscribers.go` -- The six subscriber kinds, `safeCall*` wrappers, `dispatchJobsChange`
- `internal/database/types.go` -- Job, Gap, Segment, TrimRecord, ClientToken types
- `internal/monitor/feed.go` -- YouTube RSS feed monitor
- `internal/monitor/decapi.go` -- DECAPI latest-video monitor
- `internal/monitor/twitch.go` -- Twitch GQL stream monitor
