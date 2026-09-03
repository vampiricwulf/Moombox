# SPEC.md — Moombox Technical Specification

Comprehensive AI-first reference for the Moombox project. Written for machine comprehension — explicit, unambiguous, no assumed context. Each section stands alone; an LLM reading just one section should understand that subsystem well enough to modify its code correctly. For deeper implementation details, follow the deep-dive pointer at the end of each section.

Module: `github.com/vampiricwulf/Moombox` — Go 1.26, single binary. Windows x64 + Linux x64 + Linux arm64.

---

## 1. Vision & Purpose

Moombox is a YouTube/Twitch live stream archiver built as a personal tool to product quality. It monitors channels for live streams, downloads video segments (DASH/HLS) and live chat in real time, muxes the results into final files with FFmpeg, and serves both a web dashboard and a terminal UI for managing everything. The target user is the owner first — someone technical enough to run a binary and configure a TOML file — and second, anyone wanting set-and-forget archival of YouTube and Twitch streams.

The full workflow is: monitors detect new streams via RSS feeds, DECAPI polling, and Twitch GQL queries. When a matching stream is found, a job enters the queue. A stream processor probes the video status via YouTube's Innertube API or Twitch's GQL API, waits for it to go live if it is upcoming, then hands off to a download orchestrator. The orchestrator runs segment downloaders (DASH sequential for YouTube, HLS playlist polling for Twitch) and a chat downloader concurrently. When the stream ends, segments are muxed into a final container via FFmpeg. The web dashboard and TUI display real-time progress via database pub/sub and WebSocket broadcasts.

Moombox is a standalone Go reimplementation. It is not a yt-dlp wrapper — it reimplements YouTube extraction, cipher decryption, BotGuard/PO token generation, format selection, and Twitch GQL/HLS/IRC from scratch. It tracks upstream changes in yt-dlp, BgUtils, and ejs for awareness, porting relevant logic when YouTube or Twitch change their protocols. The `references/` directory (gitignored) holds clones of these upstream repos for diffing.

What Moombox is not: it is not a general-purpose video downloader (it handles YouTube and Twitch only), not a hosted/multi-user service (single-operator deployment), and not designed for massive scale (it is a 24/7 appliance, not a batch processing system). macOS is not supported (deferred); Windows x64 and Linux x64/arm64 are the supported platforms.

Deployment is a single binary plus FFmpeg on PATH. First-run triggers a setup wizard (in both the web dashboard and the TUI) that walks through FFmpeg installation, cookie configuration, channel setup, and optional password. Self-updates check GitHub Releases daily, verify Ed25519 signatures, apply a three-step binary swap, and restart via exit code 42. The launcher/supervisor pattern ensures clean restarts without process chain buildup.

The application listens on port 774 by default. Configuration lives in `config.toml` searched in: current directory, `./config/`, `~/.config/moombox/`. The database is SQLite in WAL mode. Output files go to a configurable directory with per-channel subdirectories.

**CLI interface:** `moombox` (run the application), `moombox add <url_or_id>` (add a video/stream to the queue from the command line without starting the full app), `moombox --version` (show version), `moombox --headless` or `--no-tui` (web-only mode without TUI). The `MOOMBOX_NO_TUI=1` environment variable also disables TUI. TTY detection automatically falls back to headless mode when stdin/stdout are not terminals.

**Key dependencies:**

| Library | Purpose |
|---------|---------|
| `go-chi/chi/v5` | HTTP router with middleware chaining |
| `charm.land/bubbletea/v2` + `bubbles/v2` + `huh/v2` + `lipgloss/v2` | TUI framework (Charm ecosystem) |
| `dop251/goja` | Pure-Go JavaScript engine (cipher solving, BotGuard fallback) |
| `modernc.org/sqlite` | Pure-Go SQLite driver (no CGo) |
| `nhooyr.io/websocket` | WebSocket (RFC 6455 compliant) |
| `BurntSushi/toml` | TOML config parsing |
| `golang.org/x/crypto/scrypt` | Password hashing |
| `golang.org/x/sync/errgroup` | Concurrent download coordination |
| Shoelace v2.16 (CDN) | Web UI component library |
| Node.js v22 LTS (embedded) | Real V8 + JSDOM for the BotGuard sidecar. Pinned per-platform Node binaries (`node-windows-amd64.gz`, `node-linux-amd64.gz`, `node-linux-arm64.gz`) are `go:embed`'d and extracted on first launch — users do not need a Node install. |
| `bgutils-js` (npm, embedded) | LuanRT's BotGuard JS implementation (MIT). Bundled inside the sidecar payload. Used directly; the higher-level `bgutil-ytdlp-pot-provider` wrapper is GPL-3.0 and deliberately not depended on. |
| `jsdom` (npm, embedded) | DOM implementation for the sidecar's globalThis bootstrap. Bundled inside the sidecar payload. |

**Deep-dive:** [docs/spec/vision-and-purpose.md](docs/spec/vision-and-purpose.md)

---

## 2. Design Philosophy

### Priority Ordering

Moombox follows a strict priority hierarchy for all design decisions. When two concerns conflict, the higher-priority one wins:

1. **Correctness** — The archived output must be bit-perfect. A corrupt archive is worse than no archive. Download gaps are detected and logged. Muxing uses FFmpeg's container format handling rather than manual byte manipulation. Resume state is persisted so crashes do not lose hours of segments.

2. **Reliability** — The application must not crash, must not silently lose data, and must recover from transient failures automatically. Every goroutine has inline `defer/recover`. Network errors trigger exponential backoff with jitter. Stream-end detection uses a verification loop (up to 6 checks at 5-minute intervals) rather than trusting a single API response. Cookie auth loss triggers automatic refresh attempts.

3. **Resource Efficiency** — Moombox runs 24/7 unattended. All concurrency is signal-driven rather than polling-driven. The database uses 100ms batch coalescing so idle periods produce zero I/O. The BotGuard sidecar runs as a single long-lived Node subprocess (one V8 heap, not per-request); the goja cipher VMs auto-evict when idle (3-VM LRU cap). WebSocket broadcasts rely on upstream rate-limiting (ProgressTracker's 16ms gate caps progress writes at ~60 Hz/job) — no extra hub-level throttle. The TUI uses non-blocking channel sends with drop counters to prevent event loop blocking.

4. **Simple Deployment & UX** — Single binary, no containers, no service managers. FFmpeg is the only runtime dependency. A first-run wizard handles initial setup. Sensible defaults mean the app works out of the box for the common case. Configuration changes that require restart are handled via exit code 42 and the launcher respawns automatically.

5. **Polish** — UI interactions should feel responsive and complete. Status bars show real-time progress. Error messages are actionable. The TUI has a full chord/keybinding system. The web UI has mobile-responsive breakpoints.

6. **Feature Completeness** — New features are added when needed, not speculatively. The dual UI (web + TUI) means features must be implemented in both before being considered complete.

7. **Performance** — Raw throughput is not a priority. Correctness and resource efficiency outrank performance. The no-CGo constraint means pure Go SQLite (modernc.org/sqlite) instead of the faster cgo-sqlite3.

### Code Complexity

Complexity is acceptable when the solution demands it — YouTube's cipher obfuscation, BotGuard's VM execution, and multi-client Innertube fallback chains are inherently complex. The goal is to match solution complexity to problem complexity, not to problem complexity. Complex logic is contained behind clean interfaces: the cipher solver exposes `DecryptSignature(url)` and `DecryptN(url)` regardless of whether it used AST parsing or regex fallback internally.

### Platform Constraints

- **Cross-platform via build tags** — Windows x64, Linux x64, and Linux arm64 are supported. Platform-specific behavior (CreateNoWindow process spawning, kernel32 disk queries, DPAPI cookie decryption) is isolated in `_windows.go` / `_unix.go` / `_other.go` files per package. Linux gets functional fallbacks; Windows-only features degrade gracefully with clear UI messaging. macOS is deferred.
- **No CGo** — Pure Go dependencies only. This means modernc.org/sqlite instead of mattn/go-sqlite3, and dop251/goja instead of V8 bindings. The tradeoff is simpler cross-compilation and build toolchain at the cost of raw performance.
- **Single binary** — Web assets are embedded via `go:embed`. No external templates, no asset directories to manage.

### Dual UI Parity

Both the web dashboard and TUI are first-class citizens with full feature parity. They serve different strengths: the TUI excels at real-time monitoring, keyboard-driven workflows, and runs in any terminal. The web UI excels at rich media (video player, chat overlay, thumbnail previews) and accessibility from any device.

### Error Philosophy

Never crash. Degrade gracefully. Always inform the user. Silent failures are bugs. Every error path either retries with backoff, reports to the user via status updates, or both. The `Expected` field on `MoomboxError` distinguishes user-facing errors (auth expired, stream unavailable) from internal errors (nil pointer, parse failure).

### TUI Design Rule

Always check Charm's ecosystem (`charm.land/bubbletea/v2`, `bubbles/v2`, `huh/v2`, `lipgloss/v2`) for existing components before building custom ones. Prefer extending their building blocks over rolling custom implementations.

**Deep-dive:** [docs/spec/design-philosophy.md](docs/spec/design-philosophy.md)

---

## 3. Architecture

### Process Model

Moombox uses a launcher/supervisor pattern controlled by the `_MOOMBOX_CHILD` environment variable:

- **Without `_MOOMBOX_CHILD`** — The process acts as a launcher. It spawns itself as a child process with `_MOOMBOX_CHILD=1`, waits for it to exit, and respawns if the exit code is 42 (restart requested). The launcher uses `CreateNoWindow` (0x08000000) to prevent console window flashing on Windows. This keeps one stable parent process holding the console so the child's TUI can restore terminal state cleanly.

- **With `_MOOMBOX_CHILD=1`** — The process runs the full application stack. When a restart is needed (config change, update applied, setup wizard completion, or API request), `triggerRestart(source)` sets an atomic flag, cancels the main context, and optionally quits the TUI. The `run()` function returns `true`, and `main()` calls `os.Exit(42)`.

- **Shutdown** — Context cancellation propagates to all services. A 10-second force-exit timer (`time.AfterFunc`) ensures the process terminates even if a service hangs. Shutdown order is: monitors, download worker, notifications flush, cookie services, PO token cleanup, web server, event subscribers, database close.

### Service Initialization Order

Services are initialized sequentially in `run()` inside `cmd/moombox/main.go`. The order matters because later services depend on earlier ones:

1. **Config** — `config.Load()` reads TOML, applies defaults, runs legacy migrations
2. **Logger** — slog wrapper with file rotation, ring buffer, pub/sub
3. **Updater** — GitHub release checker, cleans up `.old` binary from previous update
4. **Database** — SQLite WAL, 1 connection, migrations to schema v16, batch update goroutine
5. **CookieJar** — Netscape cookie file parsing, in-memory cookie store
6. **YouTube Service** — PlayerAPI + Auth + format selector, fetches homepage for visitor data and API key
7. **Twitch Service** — GQL API + Auth + EmoteResolver
8. **PotProvider + Sidecar** — BotGuard/PO token generation. Primary path via embedded Node.js + JSDOM + bgutils-js subprocess (real integrity tokens); goja-only fallback when sidecar disabled or unhealthy. Triple in-process cache (session, minter, inflight).
9. **CipherSolver** — YouTube signature/n-parameter decryption, 3-VM LRU, disk cache
10. **NotificationManager** — Discord webhook dispatch
11. **DownloadWorker** — Job queue (100 lifecycle + N VOD download slots), backlog scheduler, stream processor, orchestrator
12. **TrimService** — FFmpeg-based clip extraction from finished recordings
13. **FeedMonitor + BackfillWorker** — YouTube RSS/membership discovery into the persistent feed-history store; serial full-catalog backfill scans
14. **DECAPIMonitor** — DECAPI live-check polling for YouTube
15. **TwitchMonitor** — Twitch GQL stream polling
16. **CookieRefresh** — Periodic auth validation (30-minute interval)
17. **AutoCookieService** — Browser cookie extraction (Firefox/Chromium profiles, DPAPI on Windows)
18. **WebServer** — chi router, middleware stack, route registration, WebSocket hub, static file serving
19. **TUI** — BubbleTea application (or headless mode blocks on context)

Services are wired together via callback closures and struct-based dependency injection, all orchestrated in `main.go`.

### Package Dependency Graph

```
cmd/moombox/main.go                     <- launcher + orchestrator
cmd/sign/main.go                        <- CI signing tool (Ed25519)
internal/
  config/          <- TOML config, FlexDuration, channel terms, migrations
  updater/         <- GitHub release checker, Ed25519 verification, self-update
  logger/          <- slog wrapper, file rotation, 200-line ring buffer, pub/sub
  database/        <- SQLite/WAL, batch coalescing, pub/sub, schema migrations
  cookies/         <- Netscape jar, refresh service, auto-cookie (Firefox/Chromium/DPAPI)
  youtube/         <- Service facade, PlayerAPI, Auth, watch page, format selector
  twitch/          <- Service facade, GQL API, Auth, HLS, IRC chat, VOD chat, emotes
  bgutils/         <- PotProvider, WebPoClient, Challenge, BotGuard, WebPoMinter
                  -- triple cache (session/minter/inflight) wrapping both paths
    sidecar/       <- Node + JSDOM + bgutils-js subprocess manager: extraction,
                  -- Job Object pinning, JSON-RPC mux (primary PO-token path)
    embed/         <- go:embed of node-windows-amd64.gz + node-linux-amd64.gz + node-linux-arm64.gz + sidecar.tar.gz + version.txt
  cipher/          <- Signature + n-param decryption: AST + regex, 3-VM LRU, disk cache
  engine/          <- SegmentDownloader (DASH/HLS/VOD), manifest parser, FFmpeg muxer
  chat/            <- YouTube live chat downloader (polling + batching + resume)
  worker/          <- DownloadWorker, DownloadOrchestrator, StreamProcessor, JobQueue,
                      TrimService, QualityMonitor, orphaned file scanner
  monitor/         <- FeedMonitor (RSS), DecapiMonitor, TwitchMonitor
  notifications/   <- Manager + Discord webhook sender
  web/             <- chi server, WebSocket hub, auth, middleware, rate limiter
  web/routes/      <- HTTP route handlers (jobs, auth, config, cookies, update, etc.)
  tui/             <- 2-over-1 panel layout, 10 overlays, chord system, Charm ecosystem
  goja/            <- JS runtime shims (minimal DOM, TextEncoder, timers)
  disk/            <- Disk space queries: kernel32 GetDiskFreeSpaceExW on Windows, statfs on Linux
  errors/          <- Typed error hierarchy with Expected/internal distinction
  constants/       <- Hardcoded values (API keys, URLs, client configs, user agents)
  utils/           <- HTTP helpers, formatters, YouTube/Twitch URL parsing, sanitization
```

### Key Data Flow

```
                    +-----------+     +---------------+     +--------------+
                    | RSS Feed  |     | DECAPI        |     | Twitch GQL   |
                    | Monitor   |     | Monitor       |     | Monitor      |
                    +-----+-----+     +-------+-------+     +------+-------+
                          |                   |                     |
                          v                   v                     v
                    +-----+-------------------+---------------------+----+
                    |              Database: AddJob()                     |
                    |              (pub/sub triggers UI updates)          |
                    +---------------------------+------------------------+
                                                |
                                                v
                    +---------------------------+------------------------+
                    |            DownloadWorker: processJob()            |
                    | +------------------+   +------------------------+ |
                    | | StreamProcessor  |-->| DownloadOrchestrator   | |
                    | | (probe, wait,    |   | (strategy select,      | |
                    | |  auth upgrade)   |   |  segment DL + chat,    | |
                    | +------------------+   |  mux, verify, trim)    | |
                    |                        +------------------------+ |
                    +---------------------------+------------------------+
                                                |
                    +---------------------------+----+
                    | SegmentDownloader  | ChatDownloader |
                    | (DASH/HLS/VOD)     | (YT/Twitch)   |
                    +--------------------+----------------+
                                                |
                                                v
                    +---------------------------+------------------------+
                    |                  FFmpeg Muxer                       |
                    |            (video + audio + chat -> .mkv)           |
                    +----------------------------------------------------+

    UI Data Flow:
    Database pub/sub -> WebSocket Hub -> Web clients
                     -> TUI channels  -> BubbleTea model
```

### Monitoring Pipeline

Three independent monitors detect new streams and create jobs:

**FeedMonitor** (YouTube RSS + members-only) — Polls YouTube's RSS feed endpoint (`/feeds/videos.xml?channel_id={id}`) for each monitored YouTube channel. Runs on a configurable interval (default 10 minutes) with jitter. When `membership_discovery` is enabled (default on) and YouTube auth cookies are present, it ALSO fetches each channel's authenticated `/membership` tab — RSS and DECAPI never list members-only content, so this is the only discovery source for members-only live/upcoming streams (and, with `include_non_live_content`, their VODs). Discovery is store-driven: every item either source lists is upserted into the persistent per-channel `feed_items` table, so nothing is ever "crowded out" of a transient per-cycle list. Each cycle the monitor walks the store's archive scope — everything published within `archive_window_days` (default 3, per-channel overridable) plus ALL upcoming/live items regardless of age — serially probing candidates with `ProbeVideo()` — or the authenticated `ProbeVideoAuth()` for members-only items, so a members VOD isn't misfired as "upcoming" — to classify each (live/upcoming/VOD/regular), applying channel term filtering (include/exclude), and skipping anything already tracked (`HasActiveJob` dedup). Items that pass are archived via `OnVideoFound()`: broadcasts (live/upcoming) and VODs first seen this cycle become jobs immediately, while backlog VODs — older items already known to the store — are created as `Queued` and admitted at most `archive_slots` (default 3, per-channel overridable) at a time per channel by the worker's scheduler, so a backlog sweep never starves new or live content. The monitor uses a `MetadataFailureTracker` to stop retrying videos that consistently fail metadata probes.

**Catalog backfill** — When a YouTube channel is added (or a manual re-scan is forced via the TUI `R B` chord or `POST /api/backfill/rescan`), a dedicated backfill worker scans the channel's `videos`, `streams`, and (when membership discovery is active) `membership` tabs down to the archive-window depth, feeding everything found into the `feed_items` store — the full-catalog seed that RSS's ~15-entry feed can never provide. Scans run strictly serially across channels, paced at one tab page per second, resume from a persisted cursor after interruption, and report per-channel progress in both UIs. An every-cycle sweep re-queues channels that were never backfilled or whose archive window has since widened.

**DECAPIMonitor** — Polls DECAPI for the latest video from each monitored YouTube channel. This is a secondary detection mechanism that catches streams the RSS feed might miss (RSS updates can be delayed by minutes). Extracts video IDs from DECAPI responses, runs the same ProbeVideo + term filtering pipeline as FeedMonitor. Has its own rate limit tracking (respects DECAPI rate limit headers) and configurable check interval.

**TwitchMonitor** — Polls Twitch GQL for each monitored Twitch channel. Uses the `UseLive` persisted query to check if the channel is live. When a live stream is detected, extracts stream metadata (title, category, start time, thumbnail, profile image) and calls `OnStreamFound()`. Twitch jobs are created with `StatusLive` immediately (the monitor already confirmed live status), unlike YouTube jobs which start as `StatusUpcoming` and are probed by the StreamProcessor.

All monitors share these patterns:
- **Signal-driven scheduling** — Use `time.Timer` (not `time.Ticker`) so the next check is scheduled after the current one completes, preventing overlap
- **CheckNow()** — Forces an immediate check cycle, used when channel configuration changes
- **OnSchedule callback** — Reports the next check time (epoch ms) for display in the status bar
- **Context-based cancellation** — `Start(ctx)` and `Stop()` for clean lifecycle management
- **No channels = idle** — Monitors with no configured channels of their type skip polling entirely

### Download Pipeline Detail

**StreamProcessor** handles the pre-download phase. For YouTube: probes video status using `ProbeVideoStatus()` (ANDROID_VR client, no cookies needed), classifies the result as live/upcoming/VOD/not-a-stream/members-only. For upcoming streams, enters a `waitForLive` loop: polls at a dynamic interval based on time until scheduled start (10 minutes if >1h away, 5 minutes if ≤1h, 1 minute if ≤5min) plus random jitter (up to 30s), persists metadata from each probe (title, thumbnail, description, scheduled start time) with change detection so rescheduled streams and title changes are picked up automatically at zero extra network cost. Starts chat download during the wait phase (so chat messages from the "waiting room" are captured), and uses chat surge detection (30 messages within a 15-second window) to trigger early re-probing — a burst of chat messages often indicates the stream just went live. Sends a "Schedule Changed" notification if the scheduled start time shifts between probes. When authenticated, uses `TV_DOWNGRADED` client for members-only upcoming stream polling. The processor tracks consecutive probe errors and gives up after 10 failures. For Twitch: manual adds poll the channel via GQL every 15 seconds (plus 5s jitter) until the channel goes live or context is cancelled. Monitor-discovered Twitch streams skip this phase since the monitor already confirmed live status.

**DownloadOrchestrator** manages the full download lifecycle after the stream processor confirms it is ready. It selects a download strategy based on the stream type:
- **YouTube live DASH** — Sequential segment polling with head-probing
- **YouTube VOD** — Parallel chunked download (6 workers, 5MB chunks)
- **YouTube VOD segmented** — Sequential segment download with known total
- **Twitch live HLS** — Playlist re-fetching with variant selection
- **Twitch VOD** — HLS segment download

The orchestrator starts the segment downloader and chat downloader concurrently via an `errgroup`. When the segment download completes, it runs a verification loop: up to 6 checks at 5-minute intervals (`streamEndVerifyInterval`) querying the YouTube API to confirm the stream actually ended. This guards against premature termination from transient 404s or temporary CDN outages. Only after verification confirms the stream is truly over does the orchestrator proceed to muxing.

Quality splits occur when the `QualityMonitor` (probing every 30 seconds) detects a resolution change mid-stream. The orchestrator creates a new segment file and continues downloading, resulting in multi-part recordings that the video player handles seamlessly. Segments shorter than 10 seconds are not split.

Twitch live recordings are additionally **gap-split**: Twitch has no DVR, so segments that leave the playlist window are unrecoverable. The engine appends while HLS sequence numbers stay continuous (a fast daemon restart or network blip resumes seamlessly with zero loss) and stops at any true discontinuity (`ErrGapDetected`); the orchestrator then muxes the current capture as a finished, internally-gapless part (`{name} - partN.mp4`) and continues at the live edge in a new part. The live IRC chat file rolls at every part boundary with offsets rebased to that part's start, and each part's chat is copied beside its video (`{name} - partN.chat.json`, recorded in `segments.chat_file`). Connectivity outages pause the job rather than finalizing it — one job per broadcast: when the connection returns and the same broadcast (stream_start_time identity) is still live, the same job resumes; the job finalizes only when the stream ends or the broadcast changes. A job that finalizes with exactly one part is renamed back to the plain template name. Both MPEG-TS and fMP4/CMAF delivery are supported: on fMP4 playlists the engine writes the `#EXT-X-MAP` init segment at the head of each part file (recognizing token-rotated init URIs by content hash), and a genuine mid-part init change (transcode restart) or an fMP4→TS reversion part-splits via `ErrInitSegmentChanged` — handled like a gap split, minus the lost-data notification.

Muxing runs on `context.Background()` goroutines so it completes even if the parent context is cancelled (user quits during download). This ensures that partially downloaded content is still muxed into a usable file rather than being abandoned as raw segments.

**SegmentDownloader** has three modes:
- **DASH sequential** — Increments segment number, fetches `{base_url}/sq/{n}`, handles 404 with exponential backoff. Saves resume state every 50 sequential segments. Verification is time-based: once the gap since the last segment crosses 30s, calls `checkStreamStatus()` (re-checked at most once per 30s) to verify whether the stream is still live. If the stream ended, exits cleanly; if still live, keeps waiting. A configurable `maximum_timeout` (default 600s, YouTube only) force-finalizes the recording if no segment arrives for that long even while YouTube still reports the stream live (its status can lag or stick); the clock resets whenever a segment lands, and offline time pauses it.
- **HLS polling** — Re-fetches the media playlist, identifies new segments by URL comparison, downloads them in order. YouTube HLS honors the same `maximum_timeout` backstop; Twitch HLS relies on its GQL end-detection instead. Saves resume state at the same interval as DASH.
- **VOD parallel** — Knows the total size, downloads in 5MB chunks with up to 3 retries per chunk. Reports percentage progress throttled to 500ms intervals to avoid flooding the UI.

**Catch-up mode** activates when the downloader falls more than 10 segments behind the live head (`CatchupThreshold`), past a 30-segment (`stayBehindSegments`) buffer that avoids racing in-flight segments. It hands off to a rolling window of `segment_workers` parallel workers (default 12, configurable, no upper limit — distinct from `num_parallel_downloads`, which gates concurrent VOD jobs and never applies to a live broadcast) until caught up, then resumes sequential downloading. Workers claim sequences continuously and flush completed segments in strict ascending order as they arrive, rather than waiting on per-batch barriers. This prevents permanent drift during transient slowdowns.

**Resume state** (`.resume.json` sidecar) stores `lastSeq`, `bytesWritten`, `timestamp`, and `baseUrl`. On startup, the downloader checks for a resume file, validates it, and resumes from the last checkpoint rather than starting over.

**JobQueue** implements dual-layer concurrency: 100 lifecycle slots (`maxLifecycle`) gate how many jobs can be in the probe/wait/download pipeline simultaneously, while a configurable download semaphore (`maxDownloads`, default 10) gates how many VOD jobs can be actively downloading segments. The pool gates VODs ONLY: a broadcast is never made to wait for a slot — missing a slot on a VOD delays a file that already exists, while missing it on a live broadcast loses footage — so peak concurrent downloads is (live broadcasts) + `num_parallel_downloads`. This design means stream probing, waiting-for-live, and auth negotiation do not consume download slots — only active segment downloading does. The download slot is acquired when the orchestrator begins segment downloads and released when it finishes (before muxing).

The worker also owns the **backlog Scheduler**: a single admission goroutine that is the only path out of `Queued`. Woken by backlog-job creation and job completion (with a heartbeat safety net), it admits per channel at most `archive_slots` minus that channel's in-flight backlog jobs, newest published first — writing `Upcoming` durably before enqueueing so a crash between the two steps self-heals on restart. `ShouldProcess(Queued)` is false by design, so neither startup recovery nor the heartbeat poller ever touches a `Queued` row.

Priority ordering: Live=1 (highest), Upcoming/Downloading=0, Error=-1 (lowest). Live streams are always processed before upcoming or retried jobs. The pending queue caps at 100 entries; jobs beyond that are dropped with a warning log. Duplicate detection uses both the pending set and the processing map — a job that is already pending or actively processing is not re-enqueued.

**processJob flow** (DownloadWorker.processJob, runs in a goroutine per job):
1. Acquire lifecycle slot (blocks if 100 slots are in use)
2. Fetch job from database, verify it is still in a processable state
3. Call StreamProcessor.Process() — probes, waits for live, handles auth
4. If result says "should download": acquire download slot, run DownloadOrchestrator.Execute()
5. Release download slot after orchestrator returns
6. Update job status to Finished or Error
7. Send notification (download complete / error)
8. Release lifecycle slot

The worker also runs a 60-second heartbeat poll (`heartbeatInterval`) as a safety net to catch any jobs that were missed by signal-driven notification. Normal job discovery is signal-driven via the `notifyJob` channel — when `EnqueueJob()` is called, it sends a non-blocking signal to wake the worker's dispatch loop.

### Concurrency Patterns

| Component | Pattern | Parameters |
|-----------|---------|------------|
| Worker | Dual semaphore | 100 lifecycle slots + 2 download slots (configurable) |
| WebSocket | No hub throttle | Rate bounded upstream by ProgressTracker (~60 Hz/job) |
| Database | Signal-driven batch coalesce | 100ms window, zero idle I/O |
| TUI | Non-blocking sends | Drop counters for diagnostics |
| TUI logs | Batched flush | 250ms flush interval |
| BotGuard | Sidecar + triple cache | Sidecar minter (internal, ~6h TTL); session cache (6h TTL); minter cache (dynamic TTL, goja-fallback only); inflight dedup |
| Cipher | LRU with mutex | 3-VM cache, mutex-serialized compilation |
| Cookie refresh | Periodic | 30-minute interval with immediate-check capability |
| Update check | Periodic | 5s initial delay, then 24-hour interval |
| Disk check | Piggyback on memory ticker | Every 3rd tick of 2-minute memory diagnostic |

### Panic Recovery

Every goroutine has inline `defer func() { if r := recover(); ... }()` as the first statement. The HTTP server uses `RecoveryMiddleware` that catches panics, logs the full stack trace, and returns a 500 Internal Server Error response. Database subscriber callbacks use `safeCallJobUpdate`/`safeCallJobsChange` wrappers that isolate individual subscriber panics so one bad callback does not prevent other subscribers from receiving updates. Monitor callbacks (`OnVideoFound`, `OnStreamFound`) have explicit `defer recover()` blocks wrapping the job creation logic. The shutdown sequence uses `stopService()` which wraps each individual service stop in panic isolation — if one service panics during shutdown, the remaining services still get their stop calls.

### Logger Interface Pattern

The logger is defined as an anonymous interface repeated in every struct:

```go
logger interface {
    Debug(msg string, args ...any)
    Info(msg string, args ...any)
    Warn(msg string, args ...any)
    Error(msg string, args ...any)
}
```

This pattern is intentional for loose coupling — each package defines its own logger interface rather than importing a shared one. Do not extract to a named shared interface. The actual implementation (`internal/logger.Logger`) satisfies all of these interfaces.

### Job Status Lifecycle

```
Queued (backlog VODs only, admitted by the archive-slots scheduler)
    |
    v
Upcoming -> Live -> Downloading -> Muxing -> Finished
    |         |         |            |
    +----+----+----+----+            +----> Error
         |         |
         v         v
     Cancelled   Error
                   |
                   v
                COOKIES?  (special error: auth needed)
```

`JobStatus` is `type JobStatus string`. Timestamps are ISO 8601 strings (RFC3339). Optional numeric fields use pointers. The `COOKIES?` status indicates the stream requires authentication that is not currently available. `Queued` is entered only by backlog VODs (older store items archived by the feed monitor); broadcasts and newly discovered content enter directly as `Upcoming`/`Live` and never wait in `Queued`.

A YouTube post-live download that still finalizes behind head after the VOD-branch refresh loop exhausts its retries does not become `Error` — it completes as `Finished` with `Job.IncompleteTail` set. The flag is not a status: staging directory and resume sidecar are preserved instead of being cleaned up, and Resume (only — not Retry, which gates the flagged job out because it deletes staging via ReinitializeJob) is permitted on the flagged job (normally gated to `Error`/`Cancelled`/`COOKIES?`); a clean re-run appends the missing tail and self-clears the flag via the same unconditional write that set it.

### Error Hierarchy

```go
MoomboxError          // Base: Code, Message, Expected bool, Context map, Cause error
  YouTubeError        // YouTube API errors
  DownloadError       // Download failures (with HTTPStatus)
  NetworkError        // Network-level failures (with HTTPStatus, URL)
  ConfigError         // Configuration errors (Expected=true)
  MuxingError         // FFmpeg muxing errors
  CookieError         // Cookie-related errors
  AuthError           // Authentication errors
```

`Expected=true` means the error is a normal operational condition the user can act on (expired cookies, unavailable stream). `Expected=false` means an internal error that indicates a bug or unexpected state.

**Deep-dive:** [docs/spec/architecture.md](docs/spec/architecture.md)

---

## 4. Platform Services

### YouTube

YouTube integration reimplements yt-dlp's extraction logic in Go. The core is a multi-client Innertube strategy that fetches video info from multiple YouTube API clients and merges their format pools.

**Client fallback chain for authenticated fetches** (`GetVideoInfoAuthenticated`):
1. Fetch watch page (WEB client) — extracts `ytcfg` (visitor data, API key, player URL), inline player response
2. **TV_DOWNGRADED** (TVHTML5, clientID 7) — primary authenticated client, sends cookies
3. **WEB** (clientID 1) — for DASH manifest URL (TV client sometimes lacks it)
4. **WEB_CREATOR** (clientID 62) — fallback for members-only content
5. **ANDROID_VR** (clientID 28) — fallback for VOD without cookies, no cipher needed

Each client's formats are tagged with an auth level: AuthLevelAndroidVR(0), AuthLevelWatchPage(1), AuthLevelTVPublic(2), AuthLevelTVAuth(3), AuthLevelWeb(4), AuthLevelWebCreator(5). Formats from different clients are pooled and deduplicated by itag. Format selection priority: resolution > FPS (if prefer60fps enabled) > video codec score (vp9.2=5 > vp9/vp09=4 > av01=3 > avc1=2 > h264=1) > bitrate > auth level (lower = less likely to require cookies for playback). Audio codec priority: opus=4 > mp4a.40.5=3 > mp4a.40.2=2 > mp4a=1.

**Probe vs. full fetch:** Two distinct code paths serve different needs. `ProbeVideoStatus()` uses ANDROID_VR (lightweight, no cookies, no cipher, no watch page fetch) to quickly classify a video as live/upcoming/VOD/offline/members-only. It is used by monitors for pre-filtering and by the stream processor for polling. `GetVideoInfoAuthenticated()` runs the full multi-client chain: fetches the watch page, extracts ytcfg, tries multiple Innertube clients, decrypts signatures and n-parameters, and returns the merged format pool. It is used when actual download URLs are needed.

**Watch page parsing:** The watch page HTML is fetched with the WEB user agent and cookies. It yields: `ytcfg` (visitor data, API key, player.js URL, client versions), inline player response (can contain formats directly), and initial chat continuation tokens (for live chat download).

**Cipher decryption:** YouTube obfuscates streaming URLs with a signature cipher and an n-parameter throttle. Without decryption, URLs return 403 or are throttled to unusable speeds. The cipher solver downloads `player.js` from YouTube's CDN, extracts the transformation function chain via AST parsing of the JavaScript (identifying the function by structural patterns in the obfuscated code). If AST parsing fails, it falls back to regex pattern matching against known obfuscation patterns. The extracted JavaScript is compiled into Goja VMs and cached. Two-tier cache: memory (3-VM LRU keyed by player URL) for instant reuse, and disk (14-day TTL) to avoid re-downloading player.js. The solver also extracts `signatureTimestamp` (STS) from player.js — this value must be sent in Innertube API requests or the returned formats will have invalid URLs.

**N-parameter decryption:** Separate from signature cipher but using the same extraction infrastructure. The `n` parameter in YouTube URLs controls throttling — the obfuscated value triggers aggressive rate limiting. The n-parameter function is extracted from player.js, compiled to a Goja VM, and used to transform the parameter. Same caching as signature cipher.

**Stream status classification:** The player response is parsed into one of: `StreamNotAStream` (regular video, not live), `StreamLive` (currently broadcasting), `StreamUpcoming` (scheduled, not yet live), `StreamProcessing` (recently ended, being processed), `StreamOffline` (ended or unavailable). Classification uses `playabilityStatus.status`, `videoDetails.isLiveContent`, and `videoDetails.isLive` from the player response. Members-only streams are detected via `playabilityStatus.reason` containing membership-related text.

### Twitch

Twitch integration uses the GQL API with persisted query hashes (SHA256). No REST API — everything goes through `https://gql.twitch.tv/gql`.

**GQL operations:** `UseLive` (stream info), `PlaybackAccessToken` (access tokens for HLS), `VideoCommentsByOffsetOrCursor` (VOD chat), `GetStreamInfo` (stream metadata). Each operation is identified by its SHA256 persisted query hash, not the query text — Twitch's GQL endpoint routes requests by hash for caching. The `Client-ID` header is required on all GQL requests. Auth token (from cookies, specifically the `auth-token` cookie) is sent in the `Authorization: OAuth {token}` header when available.

**HLS variant selection:** The flow is: get stream/VOD access token via GQL, build Usher URL with the token, fetch the master playlist, parse `#EXT-X-STREAM-INF` lines into variant structs (resolution, frame rate, bandwidth, codecs, group ID). Selection uses the quality preference string (e.g., "1080p60", "best", "720p", "audio_only") matched against variant names, with fallback to max resolution config. If the preferred quality is unavailable, falls back to the best available variant.

**IRC chat:** Connects to `wss://irc-ws.chat.twitch.tv:443` via WebSocket. PASS and NICK are a PAIR rendered from one decision per session: authenticated is `PASS oauth:{token}` **with** `NICK {login}` (the account's own name, from the `login` cookie), anonymous is `PASS SCHMOOPIIE` with `NICK justinfan{random}`. A token beside the `justinfan` nickname is the hybrid Twitch refuses, so anything short of a complete, sendable pair falls all the way back to anonymous. A credentialed session Twitch never welcomes falls back to anonymous once per credential pair — for the rest of the job unless the cookie file's Twitch pair changes or Twitch auth recovers, when every live chat session is told to reconnect with the current credentials (`DownloadWorker.ReauthenticateTwitchChats`) — and notifies once per pair. Joins with `JOIN #{channel}`; requests capabilities (`CAP REQ :twitch.tv/tags twitch.tv/commands twitch.tv/membership`) for rich message metadata. Parses IRC messages into structured chat events, handling: PRIVMSG (chat messages with badges, emotes, color), USERNOTICE (subscriptions, raids, gifts), CLEARCHAT (bans/timeouts), CLEARMSG (single message deletions), ROOMSTATE (slow mode, emote-only, etc.). Maintains PING/PONG keepalive. See [docs/spec/platform-services.md](docs/spec/platform-services.md) § IRC Chat (Live).

**VOD chat:** Paginated GQL queries using `VideoCommentsByOffsetOrCursor`. Each page returns comments and a cursor for the next page. Comments are fetched in chronological order by content offset (seconds into the VOD). The pagination continues until no more comments are returned or the VOD end is reached.

**Emote resolution:** Fetches third-party emotes from three providers: BTTV (`https://api.betterttv.net/3/cached/users/twitch/{id}`), FFZ (`https://api.frankerfacez.com/v1/room/id/{id}`), and 7TV (`https://7tv.io/v3/users/twitch/{id}`). Each provider returns channel-specific and global emotes. Results are merged into a unified emote map. The resolver uses a 200-channel LRU cache to avoid redundant API calls for channels that appear in multiple concurrent streams.

### Goja Runtime Shims

Cipher solving runs YouTube's `player.js` in a Goja VM, and the goja-fallback BotGuard path also executes the BotGuard interpreter under Goja. YouTube's code expects browser APIs that don't exist in a bare JS runtime. The `internal/goja/` package provides a real-class DOM shim embedded as `dom-real.js`:

- **DOM class hierarchy** — Real `EventTarget`, `Node`, `Element`, `HTMLElement` + 25 specific subclasses (HTMLDivElement, HTMLBodyElement, etc.) so `instanceof` chains return true. Tree-aware `dispatchEvent` with full capture → target → bubble propagation. Supports the full WHATWG event model including `preventDefault` / `stopPropagation` / signals / once / passive.
- **Document + Window** — Real `Document` / `HTMLDocument` / `Window` classes with `createElement` (returns the right HTML subclass per `_htmlTagMap`), `querySelector` / `querySelectorAll` (small selector parser: tag/#id/.class/[attr]/conjunction), `getElementById`, initial `<html><head/><body/></html>` tree at startup.
- **CSS** — `CSSStyleDeclaration` (Proxy-wrapped for camelCase ↔ dashed property accessors) with ~70 spec defaults baked in. `getComputedStyle` returning a read-only mirror.
- **Web platform** — `URL` + `URLSearchParams` (WHATWG-shape), real `AbortController` + `AbortSignal` as proper `EventTarget` subclasses, `DOMTokenList` for `classList`, `dataset` Proxy with camelCase ↔ dashed mirroring.
- **TextEncoder/TextDecoder** — UTF-8 encoding/decoding.
- **Timers** — `setTimeout`/`setInterval` implemented via goroutines. The timer goroutine fires into a channel that the VM polls during execution.
- **Navigator** — `navigator.userAgent` matching Chrome's UA string.

The DOM shim is ~1500 lines of JavaScript (test.50–test.55 milestone work). It's API-complete enough that BotGuard's fingerprint probes pass, but the goja interpreter still completes BotGuard's snapshot in ~552 µs vs Chrome's 50–200 ms — real V8 timing characteristics aren't reachable without a real V8, which is why the production path is the Node sidecar. The shim is retained because cipher player.js execution does not have BotGuard's timing fingerprint problem.

### BotGuard / PO Tokens

YouTube requires Proof of Origin (PO) tokens for certain requests, particularly for premium-quality formats and live streams. Moombox runs BotGuard via an **embedded Node.js sidecar** that produces real integrity tokens — the goja-only path is kept as a fallback for environments where the sidecar fails to start.

**Architecture (sidecar primary, goja fallback):**

1. **Sidecar path (preferred)** — A bundled Node.js v22 binary plus `bgutils-js` + JSDOM are extracted from `go:embed`'d blobs to `%LOCALAPPDATA%/Moombox/sidecar/` on first launch (~36 MB embed: ~33 MB gzipped node.exe + ~3.5 MB tarball of production node_modules + src/server.js). Moombox spawns the subprocess pinned to a Windows Job Object (so the child dies with the parent), pipes JSON-RPC requests over stdin/stdout, and consumes real PO tokens. First mint hits Google's WAA endpoint in ~460 ms; subsequent mints with the same binding hit the sidecar's internal minter cache in ~500 µs.

2. **Goja fallback path** — When the sidecar is disabled (`[bgutils] use_sidecar = false` in config), fails to start, or dies mid-flight, `PotProvider` falls through to the legacy in-process flow:
   1. Fetch challenge (POST to `jnn-pa.googleapis.com` or YouTube fallback)
   2. Load BotGuard interpreter JavaScript
   3. Execute the interpreter in a Goja VM with full DOM shims (real-class hierarchy: `EventTarget`, `Node`, `Element`, `Document`, `Window`, `CSSStyleDeclaration`, `URL`, `AbortController`, `DOMTokenList`)
   4. Take a snapshot, POST to `GenerateIT`
   5. Mint per-binding tokens via the returned minter callback (Path A) or fall back to the websafe-fallback token (Path B). The websafe-fallback works for most YouTube content but PO-token-gated formats may be unavailable.

**Why the sidecar exists:** The goja interpreter runs ~100× faster than V8's JIT, and BotGuard uses snapshot wall-time as a "this isn't a real browser" signal. The hand-rolled real-class DOM shimming (test.50–test.55) raised goja's API fidelity to browser parity but couldn't bridge the timing gap. Real Node + V8 + JSDOM passes the timing fingerprint.

**Triple cache (in-process, applies to both paths):** Session cache (6-hour TTL, keyed by content binding) caches the final PO token. Minter cache (single minter per process, dynamic TTL from BotGuard response) caches the goja-side minter — effectively unused under sidecar mode since the sidecar has its own internal minter cache. Inflight dedup (concurrent requests for the same key share a channel) prevents thundering herd. The session cache is the authoritative "is this token still fresh" surface; the sidecar's internal caches handle BotGuard VM reuse.

### Cipher Solver

Dual extraction approach for YouTube's obfuscated `player.js`:

1. **AST parsing** (primary) — Parses the JavaScript, finds the signature transformation function chain and n-parameter function by structural patterns
2. **Regex fallback** — Pattern-matches known obfuscation patterns when AST parsing fails

Results are compiled into Goja VMs. Memory cache: 3-VM LRU keyed by player.js URL. Disk cache: raw extracted JavaScript with 14-day TTL. The compile mutex serializes compilation to prevent thundering herd when multiple goroutines need the same player.

### YouTube Live Chat

Polls YouTube's `live_chat/get_live_chat` endpoint using continuation tokens. The lifecycle: initial continuation from watch page HTML, then follow continuation chains. Supports "Top Chat" and "All Chat" (upgrades automatically when available). Messages are deduplicated via a 5000-ID sliding window. Written to disk incrementally (JSON format) with a flush interval of 1 second. Resume sidecar (`.resume.json`) stores continuation token and message count for crash recovery.

### Caching Summary

| Resource | Cache Type | TTL/Size | Key |
|----------|-----------|----------|-----|
| PO token sessions | In-memory map | 6 hours | Content binding |
| PO token minters | In-memory map | Dynamic (from VM) | Request key |
| Cipher VMs | In-memory LRU | 3 VMs max | Player.js URL |
| Cipher JS | Disk files | 14 days | Player.js URL hash |
| Twitch emotes | In-memory LRU | 200 channels | Channel ID |
| YouTube visitor data | In-memory | Process lifetime | Singleton |
| Chat dedup IDs | In-memory set | 5000 IDs max | Message ID |

**Deep-dive:** [docs/spec/platform-services.md](docs/spec/platform-services.md)

---

## 5. User Interfaces

### Web UI

The web UI is a vanilla JavaScript SPA using Shoelace v2.16 (loaded from CDN). Static assets live in `web/public/` and are embedded via `go:embed` in `web/embed.go`. Changes to web assets require `go build` to take effect.

**Module structure:**

| File | Purpose |
|------|---------|
| `app.js` | Main SPA: job list, unified filter, log viewer, status bar, WebSocket client, settings integration |
| `modules/player.js` | Video player with niconico-style chat overlay, multi-segment seeking |
| `modules/setup.js` | First-run setup wizard + FFmpeg install flow |
| `modules/settings.js` | Settings dialog: config editing, channel management, cookies, integrations |
| `modules/trimmer.js` | Trim clip creation with timeline visualization |
| `modules/stats.js` | Statistics dashboard |
| `modules/imports.js` | Zip archive import for migrating recordings |
| `modules/filter-parser.js` | Filter query parser (booru-style tag syntax) |
| `modules/filter-engine.js` | Filter engine (evaluates parsed tokens against jobs) |
| `modules/utils.js` | Shared formatting helpers |
| `moombox.css` | All styles including mobile responsive |
| `login.html` | Authentication page |
| `index.html` | SPA shell (loads Shoelace + modules) |

**Mobile breakpoints:** 992px (tablet layout), 768px (phone layout), `hover: none` (touch interaction adjustments).

### TUI

The TUI uses Charmbracelet's full suite: bubbletea for the Elm architecture, bubbles for pre-built components, huh for form/dialog wizards, and lipgloss for styling.

**Layout:** Two-over-one split — two panels side-by-side on top, full-width logs on bottom. The focused panel's row expands vertically (top focused = 70% height, logs focused = 75% height). Width split depends on focus: tasks focused = 45%/55%, details focused = 35%/65%, logs focused = 50%/50%:
- **TaskList** (top left) — Job list with status icons, channel names, titles. Scrollable, filterable (TUI uses single-key `F` cycle; Web UI has a unified tag-based filter).
- **JobDetails** (top right) — Selected job's metadata, progress, segment counts, file info.
- **Logs** (bottom, full width) — Real-time log output, 250ms batched flush, scrollable viewport.

**Overlays (10):** Action Menu, Help, Add Video, Import, Trim, Orphaned Files & History, Client Tokens, Settings, Setup Wizard, FFmpeg Check.

### Chord System

The TUI uses a chord-based keybinding system with a single source of truth:

- `buildMenuItems()` in `app.go` defines all chords, their display text, action menu entries, hint bar text, and help text in one place.
- `dispatchAction(chord, job)` is the unified handler that executes the action for any chord.

**Chord prefixes:** A=Action (AC=Cancel, AD=Delete, AR=Retry, AA=Add), R=Request (RC=Recheck Cookies, RF=Force Refresh), O=Open (OF=Folder, OS=Stream Page, OW=Web UI), Q=Quit (QQ=Quit confirm). **Single keys:** F=Filter, M=Action Menu, `=Settings, ?=Help. **Confirm chords** require a third keypress within 3 seconds (e.g., "Q" then "Q" within 3s to quit).

### WebSocket Protocol

The WebSocket connects on any path (upgrade handler intercepts before static file serving). Message format: `{"type": string, "payload": any}`.

**Server-to-client message types:**
- `job_update` — Single job changed (payload: job object with job ID as key)
- `jobs_update` — Full job list refresh (payload: array of all visible jobs)
- `job_deleted` — A job row was removed (payload: `{id}`)
- `config_update` — A config setting that affects client-side rendering changed (payload: partial config; currently `{hideFinishedAgeDays}`)
- `log` — Log line (payload: string)
- `check_timers` — Monitor schedule update (payload: `{nextFeedCheck, nextDecapiCheck, nextTwitchCheck}`)
- `initial_state` — Sent on connect (payload: `{jobs, logs, nextFeedCheck, nextDecapiCheck, nextTwitchCheck, hideFinishedAgeDays}`)
- `update_available` — New version found (payload: release info)
- `disk_status` — Disk space update (payload: `{free, total, usedPct, warnLevel}`)
- `cookie_status` — Cookie auth change (payload: auth status)

The `hideFinishedAgeDays` field in `initial_state` and `config_update` drives the Web UI's client-side archive re-evaluation: on every `job_update`/`jobs_update` and on a 60-second idle sweep, the Web UI moves Finished jobs that have aged past the threshold from the active panel into the Archived panel. This mirrors the TUI's `isJobArchived` reclassification (`internal/tui/task_list.go`) so the active panel stays in sync with wall-clock time without a page refresh.

**Broadcast rate:** No hub-level throttle. The high-frequency caller (`OnJobChange` driven by `ProgressTracker.maybeUpdate`) is already capped to ~60 Hz per job by `progressUpdateInterval` (16 ms gate in `internal/worker/progress.go`); the other callers are event-driven, not loops. A previous per-job throttle in the hub was removed because it raced against the (unthrottled) `BroadcastJobDeleted` and could resurrect deleted rows on the trailing edge.

**Connection management:** 30-second ping interval, 10-second write timeout, 1MB max message size, 256KB backpressure limit.

### API Route Catalog

All API routes use the `/api/` prefix (no version). Non-API routes exist for POT provider compatibility and health checks.

**Jobs:**
- `GET /api/jobs` — List all active jobs
- `GET /api/jobs/archived` — List archived (old finished) jobs
- `GET /api/jobs/{id}` — Get single job
- `POST /api/jobs` — Create new job (add video/stream)
- `POST /api/jobs/{id}/cancel` — Cancel a job
- `POST /api/jobs/{id}/retry` — Retry a failed job
- `DELETE /api/jobs/{id}` — Delete a job and its files
- `GET /api/jobs/{id}/video` — Stream video file (supports Range)
- `GET /api/jobs/{id}/segments` — List multi-segment recordings
- `GET /api/jobs/{id}/segments/{index}/video` — Stream specific segment
- `GET /api/jobs/{id}/chat` — Get chat data
- `GET /api/jobs/{id}/trims` — List trim clips
- `POST /api/jobs/{id}/trims` — Create trim clip
- `DELETE /api/jobs/{id}/trims/{trimId}` — Delete trim clip
- `GET /api/jobs/{id}/logs` — Get per-job log lines

**Formats:**
- `GET /api/formats/{videoId}` — Fetch available formats for a video

**Status & Config:**
- `GET /api/status` — Server status (version, uptime, cookie status, monitor timers)
- `GET /api/config` — Get current config
- `PUT /api/config` — Update config
- `POST /api/config/channels` — Add monitored channel
- `DELETE /api/config/channels/{id}` — Remove monitored channel
- `POST /api/resolve-channel` — Resolve channel URL to ID/name

**Auth:**
- `GET /api/auth/status` — Auth state (public)
- `POST /api/auth/login` — Login (rate limited: 5/60s)
- `POST /api/auth/logout` — Logout
- `GET /api/client-tokens` — List persistent client tokens
- `DELETE /api/client-tokens/{id}` — Revoke client token

**Cookies:**
- `POST /api/cookies/recheck` — Force cookie auth recheck
- `POST /api/cookies/auto-refresh` — Trigger browser cookie refresh
- `POST /api/cookies/auto-setup/start` — Start auto-cookie browser setup
- `POST /api/cookies/auto-setup/finish` — Complete auto-cookie setup
- `POST /api/cookies/auto-setup/cancel` — Cancel auto-cookie setup
- `GET /api/cookies/auto-status` — Auto-cookie service status

**Updates:**
- `GET /api/update/status` — Current update info
- `POST /api/update/check` — Check for updates
- `POST /api/update/apply` — Apply pending update
- `POST /api/update/verify` — Verify binary signature
- `POST /api/update/dismiss` — Dismiss update notification

**Setup & System:**
- `GET /api/setup/status` — First-run wizard state
- `POST /api/setup/complete` — Complete first-run setup
- `GET /api/logs` — Recent log lines
- `POST /api/restart` — Restart application (any client allowed by `network_access` + auth; not loopback-restricted, unlike `/api/update/apply`)
- `POST /api/import` — Import zip archive
- `GET /api/stats` — Statistics data
- `GET /api/ffmpeg/check` — Check FFmpeg availability
- `GET /api/ffmpeg/install-options` — FFmpeg install methods
- `GET /api/files/orphaned` — List orphaned files
- `DELETE /api/files/orphaned` — Delete orphaned files
- `GET /api/history/orphaned` — List orphaned processing-history rows (no matching job; block re-discovery until removed)
- `DELETE /api/history/orphaned` — Remove given history video IDs to unblock re-discovery
- `GET /api/ytdlp-plugin/status` — yt-dlp plugin status
- `POST /api/ytdlp-plugin/install` — Install yt-dlp POT plugin

**Non-API routes:**
- `GET /ping` — Health check (POT provider compatibility)
- `GET /minter_cache` — Minter cache status (POT provider compatibility)
- `POST /get_pot` — Generate PO token (loopback only, CSRF exempt)
- `POST /invalidate_caches` — Invalidate POT caches (loopback only, CSRF exempt)
- `POST /invalidate_it` — Invalidate integrity token (loopback only, CSRF exempt)

### TUI Backend Communication

The TUI communicates with the web server via HTTP to `localhost:{port}`. A custom `http.RoundTripper` injects the `X-Internal-Token` header (16-byte random hex, generated at server startup) on every request. This bypasses CSRF validation and authenticates the TUI as a same-process client.

For real-time updates, the TUI does NOT use WebSocket. Instead, it subscribes directly to database pub/sub callbacks since it runs in the same process. This is more efficient than serializing to JSON and deserializing — the TUI receives typed Go structs directly. Updates are forwarded via buffered channels with non-blocking sends:

```
Database.OnJobUpdate()  -> jobUpdateCh (cap 100) -> tea.Cmd -> TUI model
Database.OnJobsChange() -> jobsUpdateCh (cap 10) -> tea.Cmd -> TUI model
Logger.Subscribe()      -> logCh (cap 200)        -> tea.Cmd -> TUI model (250ms batch)
CookieRefresh.OnAuthChange -> cookieStatusCh      -> tea.Cmd -> TUI model
Monitor.OnSchedule      -> checkTimersCh          -> tea.Cmd -> TUI model
```

When a channel is full, the send is dropped and a drop counter is incremented. On TUI exit, drop counts are logged to help diagnose missed updates. This non-blocking design prevents slow TUI rendering from back-pressuring database writes or monitor callbacks.

### Shared UI Patterns

Both the web UI and TUI implement the same user-facing features:
- **Status bar** — Shows version, uptime, cookie status (per-platform icons), disk usage (warning/critical thresholds), connection count, monitor check timers
- **Job list** — Sortable/filterable list of all jobs with status icons, channel names, titles, progress indicators
- **Job details** — Full metadata: video/audio segment counts, chat message count, file sizes, format info, quality, timestamps
- **Add video** — URL input, platform detection, format selection (optional), quality preference
- **Settings** — Config editing, channel management (add/remove/edit), cookie management, integration setup
- **Trim creation** — Start/end time selection, progress tracking, file management
- **Update flow** — Check for updates, view release notes, apply update, verify signature
- **Import** — Zip archive import for migrating recordings from other systems

**Deep-dive:** [docs/spec/user-interfaces.md](docs/spec/user-interfaces.md)

---

## 6. Data & Storage

### Database

SQLite in WAL mode, single connection (`SetMaxOpenConns(1)`), 5-second busy timeout, foreign keys enabled. DSN: `file:{path}?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)` — modernc/sqlite ONLY honors `_pragma=name(value)` parameters; the mattn-style `_journal_mode=`/`_busy_timeout=`/`_foreign_keys=` forms are silently ignored (that exact mistake once disabled WAL and FK enforcement; the v14 migration cleans up its fallout).

**Schema version:** v15 (v5 added `segments` table, v6 added `client_tokens` table, v15 added per-part `segments.chat_file`; see `docs/spec/data-and-storage.md` for the full migration table). Migrations run automatically on startup via `db.migrate()`.

**Batch update coalescing:** `UpdateJobFields()` sends the job to a channel. A background goroutine collects updates in a 100ms signal-driven window, then writes them in a single transaction. This coalesces rapid segment progress updates (which fire every few hundred milliseconds during download) into batched writes, producing zero I/O during idle periods.

**`UpdateJobFields` pattern:**
```go
db.UpdateJobFields(jobID, map[string]any{
    "status":   database.StatusDownloading,
    "progress": "V:1234 A:1234 C:5678",
})
```
Dynamically builds `SET` clauses from the map using `fieldToColumn` (a 48-entry whitelist over `jobs` columns). Auto-updates `updated_at`. Triggers `OnJobUpdate` subscribers after write. Returns the updated `*Job`.

**Pub/sub:** `OnJobUpdate(func(*Job))` fires when any field of a single job changes. `OnJobsChange(func([]*Job))` fires when the job list changes (add/delete). Both return an unsubscribe function. Multiple subscribers are supported — the WebSocket hub, TUI, and notification manager all subscribe independently. Callback invocation uses `safeCallJobUpdate`/`safeCallJobsChange` wrappers with panic recovery so one misbehaving subscriber does not affect others. The subscriber list is protected by a separate `subMu` RWMutex to avoid contention with the main database mutex.

**Per-job log buffers:** The database maintains in-memory log buffers per job (max 200 lines, trimmed to 100 when exceeded). `RouteLogToJobs(line)` scans each log line for known job IDs and appends matching lines to the corresponding buffer. `TrackJobForLogs(jobID)` registers a job ID for log routing. `PruneJobLogs(activeIDs)` removes buffers for jobs that no longer exist. These per-job logs are served via `GET /api/jobs/{id}/logs` and displayed in the TUI job details panel.

**Job table columns (55 fields):** id, video_id, url, title, channel_name, platform, status, progress, percent, eta, speed, error, created_at, updated_at, last_video_seq, last_audio_seq, total_video_seq, total_audio_seq, is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time, length_seconds, download_started_at, thumbnail_url, description, output_file, filename, output_directory, video_width, video_height, video_fps, file_size, chat_status, total_chat_messages, chat_filename, chat_file, thumbnail_file, description_file, twitch_quality, twitch_category, channel_avatar_url, selected_video_itag, selected_audio_itag, start_time, end_time, last_recheck_at, quality_preference, watched, resume_position, chat_offset, auto_retry_count, channel_id, queue_priority.

**Additional tables:** `history` (video IDs seen by monitors, prevents re-adding), `segments` (multi-segment recordings, schema v5), `client_tokens` (persistent auth tokens, schema v6), `trims` (clip extractions from finished recordings), `feed_items` (persistent per-channel discovery store, schema v16), `channel_state` (per-channel backfill/RSS bookkeeping, schema v16).

### Config

TOML format parsed by `BurntSushi/toml`. Search order: custom path (CLI flag), `./config.toml`, `./config/config.toml`, `~/.config/moombox/config.toml`. Falls back to defaults if no file found.

**Sections:** `[network]` (port, access level, TLS, password, trusted proxies), `[paths]` (database, log, output, staging, ffmpeg), `[logs]` (level, rotation), `[monitors]` (intervals, archive window/slots, hide threshold, probe cooldown, membership discovery), `[downloader]` (template, resolution, parallelism, chat, retry), `[cookies]` (file, auto, browser profile, platforms, refresh interval), `[disk]` (warn/critical percent), `[updates]` (auto-check), `[[channels]]` (array of monitored channels), `[[notifications]]` (array of webhook configs).

**FlexDuration:** Custom type that accepts either a bare number (interpreted in the field's documented unit — minutes for `feed_check_interval`, seconds for `probe_cooldown`) or a structured duration in TOML. Used for `feed_check_interval`, `hide_finished_age_days`, `probe_cooldown`, `refresh_interval`.

**Non-destructive migrations:** `migrateOldFormat()` handles backward compatibility — migrates flat fields into current sections, converts legacy flags (e.g., `allow_lan`/`allow_external` to `network_access`). Only applies when the new section does not already exist.

**Runtime hot-reload:** Log level, parallel download count, and `network.trusted_proxies` can be changed at runtime via the API or TUI without restart (the client-IP middleware re-reads the store per request, so `trusted_proxies` is deliberately absent from both restart-required lists). Channel changes trigger monitor re-evaluation via `kickMonitors()`, which calls `CheckNow()` on all three monitors to wake them from idle sleep.

**Channel configuration:** Each `[[channels]]` entry has: `id` (YouTube channel ID or Twitch login), `name` (display name), `platform` ("youtube" or "twitch", defaults to "youtube"), `enabled` (optional, defaults to true), `terms` (title/description match terms for filtering), `output_directory` (per-channel override), `include_non_live_content` (archive regular videos too), `archive_window_days`/`archive_slots` (per-channel overrides), `quality_preference` (Twitch quality string).

**Channel terms:** The `terms` field supports include/exclude filtering. Include terms: video title or description must match at least one. Exclude terms: video is skipped if any match. Terms support regex when prefixed with `/`. Example: `terms = { include = ["karaoke", "/singing.*stream/"], exclude = ["clip"] }`. `num_desc_lookbehind` controls how many feed items are checked for description-based matching.

### Cookies

Netscape-format cookie file (`cookies.txt`). The `CookieJar` parses it into TWO per-platform in-memory maps, not one — YouTube rows never reach a Twitch request and vice versa. Essential YouTube cookies include SAPISID (or `__Secure-3PAPISID`), SID, HSID, `__Secure-1PSID`, LOGIN_INFO; essential Twitch cookies (`essentialTwitchCookies`): `auth-token`, `twilight-user`, `login`, `name` — `login` is load-bearing, not decorative, since it is the IRC NICK above. Expiry is captured per entry and reported per platform but never filtered — the only operator-visible surface for it is the `Cookies loaded` line at startup.

**Cookie refresh service (`RefreshService`).** Always runs — on its own 30-minute timer, and on demand from either UI (`R C` / `POST /api/cookies/recheck`, plus the re-check that follows EVERY gesture which may have rewritten `cookies.txt`: both browser-refresh buttons, both setup-wizard finishes, the automatic recovery whatever its verdict, the worker's job-triggered refresh, the startup browser-profile seed, and the `auto_enabled` periodic timer) — and is never gated on any config flag. No monitor triggers it; the monitors' only channel into the service is `ObserveLiveness`, whose recovery arm is disarmed. It validates YouTube by POSTing the Innertube `guide` endpoint (`youtubeGuideURL`, `internal/cookies/refresh.go`) and Twitch by GETting `id.twitch.tv/oauth2/validate`, reports changes through `OnAuthChange`, and fires `OnRecoveryNeeded` when auth is lost. **It rotates YouTube cookies only.** Google's `Set-Cookie` responses are admitted back into `cookies.txt` under `admitSetCookie`'s rules; there is no Twitch refresh anywhere in the process, and none appears to be possible in-process — reading yt-dlp's Twitch extractor and chatterino7 turned up no client that renews an `auth-token`, only ones that read it and detect its expiry, so a browser sign-in is the only thing observed to issue a new one. The Twitch side is a check with no rotation — plus a MARK. Anything that finds Twitch credentials refused where they are actually used calls `NoteTwitchAuthLoss(reason)`, which writes the Twitch triple under the same mutex, fires `OnRecoveryNeeded("twitch")` through the ordinary dedupe, and sticks against a `oauth2/validate` 200 until the credential pair's fingerprint changes (`CookieJar.TwitchIdentity`). Today's callers are the IRC chat handshake's four downgrade routes and `StreamProcessor.noteAnonymousPlayback` (`internal/worker/stream_processor_twitch.go`), which marks on `Service.GetHLSMasterPlaylist`'s verdict — whether the playback access token was issued to a signed-in session — through the same seam; it is the one detector a job with chat capture off still gets. A credential change also fires `OnCredentialsChanged("twitch")`, which tells every live chat session to re-read its credentials and reconnect in place rather than waiting for the next job; `OnAuthRecovered("twitch")` does the same on its own edge, for a transient refusal that heals with the fingerprint unchanged and so never reaches `OnCredentialsChanged` at all.

**Auto-cookie service (`AutoCookieService`).** Acquires credentials four ways: an interactive browser login, a headless browser refresh (Firefox reads `cookies.sqlite` **together with its `-wal` sidecar**; Chromium is driven over CDP, with an opt-in Windows DPAPI read of the user's real profile as a fallback, off by default), browser-free by importing a mounted browser profile, or from an operator-supplied Netscape file posted to `POST /api/cookies/import`. It manages a dedicated profile directory and refuses one that points inside a real installed browser's profile tree — for LAUNCHING. `cookies.acquisition` (`auto` | `profile`) selects how a refresh acquires credentials; `"profile"` never launches, reads the configured profile directory read-only, and is the explicit opt-in that lets that read proceed against a real browser's profile. The launch guard itself is never lifted, in any mode.

**What `cookies.auto_enabled` does.** It owns the headless-browser refresh TIMER, the one automatic browser recovery attempt, and the `SetExpectedPlatforms` seeding — and nothing else. It is not a master switch: `RefreshService` never consults it, `R C` / `POST /api/cookies/recheck` never consults it, and `R F` / the dashboard header's shift+click / the Settings page's "Refresh cookies from browser profile" button only let it decide WHICH rung of the refresh ladder runs — the flag never causes a nil-error decline, though a pass with the flag off AND no browser profile directory still fails with `ErrNoBrowserFound`. Flipping it off at runtime does not stop the already-running timer, which is why both UIs label it restart-required.

**What `cookies.acquisition` does.** It picks the PATH a refresh takes; `auto_enabled` picks whether a browser may run at all. The two compose and neither replaces the other. `"auto"` is the default and is the behaviour that shipped before the setting existed: a resolvable browser launches, a host with none imports. `"profile"` forces the browser-free import even on a desktop with a browser installed, which is the only route to reading a real signed-in profile on Windows; under it the flag's timer and its one automatic recovery attempt import instead of launching, and the timer's import stays behind `automaticImportGuard` like every automatic import. Two values by ruling — the audit's `"browser"` behaved exactly like `"auto"` and was dropped. It is read live (`AutoCookieService.AcquisitionMode`) and is not restart-required. `StartSetup` never consults it — the interactive login is acquisition, and gating it would leave a fresh install in `"profile"` mode unable to create the profile it is told to read.

**Docker works, with one manual step.** Leave `auto_enabled` off (the image ships no browser), mount a Firefox profile, and press `R F` / shift+click / the Settings button after each host-side profile refresh. The very first import is automatic — but only when there is no `cookies.txt` to lose. When the profile itself goes stale, `POST /api/cookies/import` takes a pasted or uploaded Netscape file from any authenticated client, merges it into `cookies.txt`, reloads the jar and answers with a live verdict — no browser, no shell, no volume access. It has no GET, deliberately and permanently.

**The two-tier cookie liveness pilot is OFF.** `livenessRecoveryArmed` is `false`, so tier 2 observes, dedupes and logs but sends no notification and triggers no recovery. Arming it is a deliberate, separate change.

**Deep-dive:** [docs/spec/data-and-storage.md](docs/spec/data-and-storage.md) § Cookies for the jar, the refresh service and every acquisition path; [docs/spec/operations.md](docs/spec/operations.md) § Browser Cookie Acquisition for the platform differences (the reap — a Job Object on Windows, a process group on Linux, nothing on darwin — `AbandonSetup`, the drain) and § Credential Notifications for what an operator is actually told.

### File Output

**Staging directory:** Active downloads write segments to `staging/{jobID}/`. Contains video segments, audio segments, chat JSON, and resume state files.

**Output template:** Default `${channel}/${start_date} ${title} [${id}]`. Variables are expanded at mux time. Creates per-channel subdirectories.

**Chat JSON format:** Header with metadata (video ID, title, channel, start time, message count), followed by an array of chat messages. The message count in the header uses fixed-width padding (20 chars) so it can be updated in-place without rewriting the file. Messages are appended incrementally.

**Resume state:** `.resume.json` sidecar next to each segment file. Stores `lastSeq`, `bytesWritten`, `timestamp`, `baseUrl`. Checked on startup to resume interrupted downloads.

### Logger

slog-based wrapper with dual output (file + stdout). File rotation by size (default 10MB, 5 rotated files). A 200-line ring buffer provides recent log history for WebSocket initial state and TUI backfill. Pub/sub allows multiple subscribers (WebSocket forwarder, TUI forwarder) to receive log lines. Stdout output is suppressed via `switchableWriter` when the TUI is running (BubbleTea owns the alternate screen).

**Deep-dive:** [docs/spec/data-and-storage.md](docs/spec/data-and-storage.md)

---

## 7. Security

### Middleware Stack

Applied in `NewServer()` in this exact order (order matters). `chimiddleware.RequestID` and `DrainMiddleware` run first — non-security, so unnumbered here; Drain sits ahead of Recovery so its shutdown 503 cannot be disturbed by a later panic:

1. **RecoveryMiddleware** — Catches panics, logs stack trace, returns 500
2. **CORSMiddleware** — Validates Origin based on `network_access` config
3. **SecurityHeaders** — X-Frame-Options: DENY, X-Content-Type-Options: nosniff, Referrer-Policy: no-referrer, Permissions-Policy, CSP
4. **CSRFMiddleware** — Origin/Referer validation on mutating requests
5. **IPGateMiddleware** — Enforces `network_access` level (localhost/lan/external/public)
6. **MaxBodySize** — Default 1MB body limit (import endpoint overrides to 500MB)
7. **CompressionMiddleware** — Gzip response compression
8. **AuthMiddleware** — Session/token validation for external connections (registered separately on the router)

### CSRF Protection

Uses Origin and Referer header validation (not CSRF tokens). Mutating requests (POST/PUT/DELETE) must have a valid Origin or Referer matching the server. GET/HEAD/OPTIONS are exempt. POT provider endpoints (`/get_pot`, `/invalidate_caches`, `/invalidate_it`) are exempt because they are called by yt-dlp scripts that do not send these headers — those routes enforce loopback-only access instead.

**Internal token bypass:** Same-process clients (TUI) send `X-Internal-Token` header. The token is 16 bytes of crypto/rand hex, generated at server startup, compared with `crypto/subtle.ConstantTimeCompare`. This bypasses CSRF checks entirely — the TUI is trusted as it runs in the same process.

### Authentication

This section is about DASHBOARD authentication — who may use the web UI and the API. It is unrelated to the platform credentials in [§ Cookies](#cookies), which is why the two never share a payload, a status field or a notification.

**Password hashing:** scrypt with parameters N=16384, r=8, p=1, key length=64, salt length=16. Stored format: `scrypt:{salt_hex}:{hash_hex}`. Plaintext passwords in config are auto-converted to scrypt hashes on startup.

**Sessions:** 24-hour TTL, stored in-memory (not persisted across restarts). 1-hour cleanup ticker evicts expired sessions. Session tokens are random hex strings stored in `moombox_session` cookie.

**Persistent client tokens:** Stored in the database (`client_tokens` table). Token format includes a prefix for O(1) lookup. Token hash is stored, not the raw token. Used for "remember this device" — if the session cookie is expired but a valid client token cookie exists, a new session is created automatically.

### IP-Based Access Control

The `network_access` config field controls who can connect:
- `localhost` — Only loopback (127.0.0.1, ::1)
- `lan` — Loopback + private IP ranges (10.x, 172.16-31.x, 192.168.x)
- `external` — All IPs
- `public` — All IPs; a config-file-only synonym for `external` marking a deployment behind an authenticating reverse proxy. Not offered in any dropdown and rejected as an API input value.

Loopback and private IP connections skip authentication entirely — this means local access always works without a password, which is the expected use case for a personal archiving appliance. Authentication (session + client token) is only required for external connections when a password is configured.

**Client IP resolution.** `isLoopback()` and `isPrivateIP()` classify whatever address `EffectiveClientIP()` returns — the direct peer from `RemoteAddr` unless that peer is listed in `network.trusted_proxies` (empty by default), in which case `X-Forwarded-For` is walked right-to-left past trusted hops. Without a declared trusted proxy the header is never read. Every trust decision — IP gate, auth skip, WebSocket upgrade, all five rate limiters — routes through it; loopback-gated endpoints deliberately keep using the direct peer address. The setting is hot-reloadable (no restart).

**Passwordless external access is block set, warn boot.** The TUI, the config API, and the setup wizard all refuse to enable `external`/`public` with no password, and removing the password resets `network_access` to `localhost`. A config file that already carries the combination still boots — it warns at startup, sets `passwordlessExternal` on `/api/auth/status`, and shows a persistent red banner in both the web UI and the TUI. It never hard-fails, because a deployment behind an authenticating proxy must keep working.

### Rate Limiting

Sliding window rate limiter keyed on the effective client IP (trusted-proxy aware). Per-route limits:
- Login: 5 attempts per 60 seconds
- Password change: 3 attempts per 60 seconds
- General API: 20 requests per 60 seconds
- POT provider: 10 requests per 60 seconds

### Content Security Policy

```
default-src 'self';
script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net;
style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net;
font-src 'self' https://cdn.jsdelivr.net;
img-src 'self' data: https://i.ytimg.com https://yt3.ggpht.com https://*.jtvnw.net https://*.ttvnw.net https://cdn.jsdelivr.net https://fonts.gstatic.com;
connect-src 'self' ws: wss: https://cdn.jsdelivr.net data:;
frame-src https://www.youtube-nocookie.com https://player.twitch.tv;
object-src 'none';
base-uri 'self';
form-action 'self'
```

### TLS

Optional. When enabled (`https_enabled = true`), uses configured cert/key paths. If paths are not provided, auto-generates a self-signed certificate.

### Update Signing

Binary releases are signed with Ed25519. The public key is embedded in the binary at compile time. The private key is stored as a GitHub Actions secret and never leaves CI. During update: download new binary, download `.sig` file, verify Ed25519 signature of the binary against the embedded public key. If verification fails, the update is rejected and the downloaded file is deleted. The three-step binary swap: write new binary as `{exe}.new`, rename current `{exe}` to `{exe}.old`, rename `{exe}.new` to `{exe}`. The `.old` file is cleaned up on next startup by `CleanupOldBinary()`. This three-step approach is atomic on Windows (rename is atomic within the same volume) and allows rollback if something goes wrong.

The `cmd/sign/main.go` tool is used in CI to sign the binary. It reads the Ed25519 private key from an environment variable, signs the binary, and writes the `.sig` file. The `POST /api/update/verify` endpoint allows users to verify the signature of the currently running binary at any time.

**Deep-dive:** [docs/spec/security.md](docs/spec/security.md)

---

## 8. Operations

### Build

```bash
go build -o moombox.exe ./cmd/moombox    # Build binary
go test ./...                              # Run all tests
go vet ./...                               # Static analysis
```

Go 1.26 required. Runtime requires FFmpeg on PATH. Windows resource embedding (exe icon, version info) via `go-winres`: `go install github.com/tc-hib/go-winres@latest && cd cmd/moombox && go-winres make`. This generates `.syso` files in `cmd/moombox/winres/` — CI generates these at build time, none are committed to the repo.

### CI/CD

GitHub Actions (`.github/workflows/release.yml`) triggers on tag push. Builds Windows exe, generates `.syso` for icon/version, signs with Ed25519 (private key in GitHub secret), uploads binary + signature to GitHub Release. Release body is read from `RELEASE_NOTES.md` in the repo.

### Release Process

1. Generate `RELEASE_NOTES.md` — `git log --oneline <prev-tag>..HEAD`, group by Features/Improvements/Bug Fixes/Internal (skip empty sections). No heading.
2. Bump `version` in `cmd/moombox/main.go` (line: `version = "x.y.z"`).
3. Commit both: `chore: bump version to x.y.z — short summary`.
4. Tag: `git tag vx.y.z`.
5. Push: `git push && git push origin vx.y.z`.

### Self-Update Flow

1. **Check** — Query GitHub Releases API for latest release. Compare semver against `version` constant. Skip if current is equal or newer.
2. **Download** — Fetch `Moombox.exe` asset and `Moombox.exe.sig` signature asset.
3. **Verify** — Ed25519 signature verification against embedded public key. Reject if invalid.
4. **Swap** — Three-step rename: write downloaded binary as `{exe}.new`, rename current `{exe}` to `{exe}.old`, rename `{exe}.new` to `{exe}`.
5. **Restart** — `triggerRestart("update")` exits with code 42. Launcher respawns, loading the new binary.
6. **Cleanup** — On next startup, `CleanupOldBinary()` deletes `{exe}.old`.

### Launcher/Supervisor

The launcher (parent process, without `_MOOMBOX_CHILD`) spawns the application as a child with `_MOOMBOX_CHILD=1`. It watches the child's exit code:
- Exit code 42 — Restart requested. Launcher respawns immediately, picking up any new binary.
- Any other exit — Launcher exits with the same code.

The `CreateNoWindow` flag (0x08000000) on Windows prevents a console window flash during respawn. Restart triggers: config change requiring restart, update applied, setup wizard completion, `POST /api/restart`.

### Shutdown Sequence

When the main context is cancelled (Ctrl+C, SIGTERM, or restart trigger):

1. 10-second force-exit timer starts
2. Stop TwitchMonitor, DecapiMonitor, FeedMonitor
3. Stop DownloadWorker (waits for active downloads to save resume state)
4. Flush pending notifications
5. Stop CookieRefresh and AutoCookieService
6. Cleanup PotProvider (evict VMs)
7. Stop WebServer (graceful HTTP shutdown)
8. Unsubscribe log and DB event forwarders
9. Close Database (flush WAL)
10. Return restart flag to `main()`

Each service stop is wrapped in panic isolation via `stopService()`.

### Reference Repos

The `references/` directory (gitignored) contains clones of upstream projects tracked for awareness:
- **yt-dlp** — YouTube format/cipher/extraction, Twitch extractor, PO tokens, cookies
- **BgUtils** — BotGuard/PO token generation protocol
- **ejs** — yt-dlp external JS for cipher solving
- **chatterino7** — Twitch chat (IRC protocol, emotes, badges)
- **moonarchive** — Python stream archiver (segment download strategies)
- **moombox** — Original Python Moombox

Run `bash references/update-all.sh` to pull all upstream repos and see new commits with Moombox-relevant file changes. Use `--diff` flag for verbose file-level diffs.

### Notifications

Discord webhooks with async dispatch. The `NotificationManager` validates webhook URLs, formats Discord embeds with color-coded types (Info=blue, Success=green, Warning=yellow, Error=red, Download=teal, Muxing=purple, Cancelled=orange), and dispatches asynchronously via `sync.WaitGroup`. Supports event-based filtering per webhook target — see the event list below and the table in `docs/spec/operations.md`.

**Deep-dive:** [docs/spec/operations.md](docs/spec/operations.md)

---

### Notifications Detail

The notification system supports Discord webhooks with event-based filtering. Each `[[notifications]]` config entry has a webhook URL and an optional event filter list. If no filter is specified, all events are sent.

**Notification events:** `found`, `added`, `scheduled`, `rescheduled`, `downloading`, `quality_split`, `gap_split`, `muxing`, `finished`, `error`, `cancelled`, `auth`, `connectivity_pause`, `connectivity_resume`, `connectivity_split`, `trim_created`, `trim_deleted`, `trim_error`, `disk_warning`, `update_available` — see `docs/spec/operations.md` for the per-event table; new events must be registered in both UI filter registries.

**Discord embed format:** Title, description, colored sidebar (type-specific), fields (inline key-value pairs), optional URL link, optional thumbnail image. The embed color encodes the notification type: blue=info, green=success, yellow=warning, red=error, teal=download, purple=muxing, orange=cancelled.

Dispatch is asynchronous — `Manager.Send()` returns immediately and the HTTP POST to Discord happens in a background goroutine tracked by `sync.WaitGroup`. The `Wait()` method blocks until all pending notifications are delivered, which is called during shutdown to ensure no notifications are lost.

---

## Appendix

For volatile numbers that change frequently (line counts per package, current version, schema version, dependency versions), see [docs/spec/appendix-metrics.md](docs/spec/appendix-metrics.md). These are maintained separately so SPEC.md does not need updates for every line count change.
