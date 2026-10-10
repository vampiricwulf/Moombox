# SPEC.md — Moombox Technical Specification

Comprehensive AI-first reference for the Moombox project. Written for machine comprehension — explicit, unambiguous, no assumed context. Each section stands alone; an LLM reading just one section should understand that subsystem well enough to modify its code correctly. For deeper implementation details, follow the deep-dive pointer at the end of each section.

Module: `github.com/vampiricwulf/Moombox` — Go 1.27, single binary. Windows x64 + Linux x64 + Linux arm64.

---

## 1. Vision & Purpose

Moombox is a YouTube/Twitch live stream archiver built as a personal tool to product quality. It monitors channels for live streams, downloads video segments (DASH/HLS) and live chat in real time, muxes the results into final files with FFmpeg, and serves both a web dashboard and a terminal UI for managing everything. The target user is the owner first — someone technical enough to run a binary and configure a TOML file — and second, anyone wanting set-and-forget archival of YouTube and Twitch streams.

The full workflow is: monitors detect new streams via RSS feeds, DECAPI polling, and Twitch GQL queries. When a matching stream is found, a job enters the queue. A stream processor probes the video status via YouTube's Innertube API or Twitch's GQL API, waits for it to go live if it is upcoming, then hands off to a download orchestrator. The orchestrator runs segment downloaders (DASH sequential for YouTube, HLS playlist polling for Twitch) and a chat downloader concurrently. When the stream ends, segments are muxed into a final container via FFmpeg. The web dashboard and TUI display real-time progress via database pub/sub and WebSocket broadcasts.

Moombox is a standalone Go reimplementation. It is not a yt-dlp wrapper — it reimplements YouTube extraction, cipher decryption, BotGuard/PO token generation, format selection, and Twitch GQL/HLS/IRC from scratch. It tracks upstream changes in yt-dlp, BgUtils, and ejs for awareness, porting relevant logic when YouTube or Twitch change their protocols. The `references/` directory (gitignored) holds clones of these upstream repos for diffing.

What Moombox is not: it is not a general-purpose video downloader (it handles YouTube and Twitch only), not a hosted/multi-user service (single-operator deployment), and not designed for massive scale (it is a 24/7 appliance, not a batch processing system). macOS is not supported (deferred); Windows x64 and Linux x64/arm64 are the supported platforms.

Deployment is a single binary plus FFmpeg on PATH. First-run triggers a setup wizard (in both the web dashboard and the TUI) that walks through FFmpeg installation, cookie configuration, channel setup, and optional password. Self-updates check GitHub Releases daily, verify Ed25519 signatures and a signed release manifest, apply a three-step binary swap, and restart via exit code 42. The launcher/supervisor pattern ensures clean restarts without process chain buildup.

The application listens on port 774 by default. Configuration lives in `config.toml` searched in: current directory, `./config/`, `~/.config/moombox/`. The database is SQLite in WAL mode. Output files go to a configurable directory with per-channel subdirectories.

**CLI interface:** `moombox` (run the application), `moombox add <url_or_id>` (add a video/stream to the queue from the command line without starting the full app; it sends the "Job Added" notification itself and prints any delivery warning or error to stderr), `moombox --version` (show version), `moombox --headless` or `--no-tui` (web-only mode without TUI). The `MOOMBOX_NO_TUI=1` environment variable also disables TUI. TTY detection automatically falls back to headless mode when stdin/stdout are not terminals.

**Key dependencies:**

| Library | Purpose |
|---------|---------|
| `go-chi/chi/v5` | HTTP router with middleware chaining |
| `charm.land/bubbletea/v2` + `bubbles/v2` + `huh/v2` + `lipgloss/v2` | TUI framework (Charm ecosystem) |
| `dop251/goja` | Pure-Go JavaScript engine (cipher solving, BotGuard fallback) |
| `modernc.org/sqlite` | Pure-Go SQLite driver (no CGo) |
| `github.com/coder/websocket` | WebSocket (RFC 6455 compliant; the library formerly published as `nhooyr.io/websocket`) |
| `BurntSushi/toml` | TOML config parsing |
| `golang.org/x/crypto/scrypt` | Password hashing |
| `golang.org/x/sync/errgroup` | Concurrent download coordination |
| Shoelace v2.16 (CDN) | Web UI component library |
| Node.js v24 LTS (embedded) | Real V8 + JSDOM for the BotGuard sidecar. Pinned per-platform Node binaries (`node-windows-amd64.gz`, `node-linux-amd64.gz`, `node-linux-arm64.gz`) are `go:embed`'d and extracted on first launch — users do not need a Node install. |
| `bgutils-js` (npm, embedded) | LuanRT's BotGuard JS implementation (MIT). Bundled inside the sidecar payload. Used directly; the higher-level `bgutil-ytdlp-pot-provider` wrapper is GPL-3.0 and deliberately not depended on. |
| `jsdom` (npm, embedded) | DOM implementation for the sidecar's globalThis bootstrap. Bundled inside the sidecar payload. |

**Deep-dive:** [docs/spec/vision-and-purpose.md](docs/spec/vision-and-purpose.md)

---

## 2. Design Philosophy

### Priority Ordering

Moombox follows a strict priority hierarchy for all design decisions. When two concerns conflict, the higher-priority one wins:

1. **Correctness** — The archived output must be bit-perfect. A corrupt archive is worse than no archive. Download gaps are detected and logged. Muxing uses FFmpeg's container format handling rather than manual byte manipulation. Resume state is persisted so crashes do not lose hours of segments.

2. **Reliability** — The application must not crash, must not silently lose data, and must recover from transient failures automatically. Every goroutine has inline `defer/recover`. Network errors trigger exponential backoff with jitter. Stream-end detection uses a verification loop (up to 6 checks at 5-minute intervals) rather than trusting a single API response. Cookie auth loss triggers automatic refresh attempts.

3. **Resource Efficiency** — Moombox runs 24/7 unattended. All concurrency is signal-driven rather than polling-driven. The database has no background writer — `UpdateJobFields` runs synchronously under `db.mu` when a caller has something to write — so idle periods produce zero database I/O. The BotGuard sidecar runs as a single long-lived Node subprocess (one V8 heap, not per-request); the goja cipher VMs auto-evict when idle (10-VM LRU cap). WebSocket broadcasts rely on upstream rate-limiting (ProgressTracker's per-job gate caps progress writes at the configured progress interval — `downloader.progress_interval_ms`, 16 ms by default, so ~60 Hz/job) — no extra hub-level throttle. The TUI uses non-blocking channel sends with drop counters to prevent event loop blocking.

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

Never crash. Degrade gracefully. Always inform the user. Silent failures are bugs. Every error path either retries with backoff, reports to the user via status updates, or both. Errors are plain Go errors; the only classification is sentinel matching with `errors.Is` — `worker.ErrCookiesRequired`, `worker.ErrNotAMember`, `twitch.ErrTwitchAuthExpired` and `twitch.ErrSubscriberOnly` park a job at `COOKIES?` (something the user can act on), `worker.ErrNonActionable` marks a failure not worth a notification, and everything else lands in `Error` with the error text as the job's `error` column.

### TUI Design Rule

Always check Charm's ecosystem (`charm.land/bubbletea/v2`, `bubbles/v2`, `huh/v2`, `lipgloss/v2`) for existing components before building custom ones. Prefer extending their building blocks over rolling custom implementations.

**Deep-dive:** [docs/spec/design-philosophy.md](docs/spec/design-philosophy.md)

---

## 3. Architecture

### Process Model

Moombox uses a launcher/supervisor pattern controlled by the `_MOOMBOX_CHILD` environment variable:

- **Without `_MOOMBOX_CHILD`** — The process acts as a launcher. It spawns itself as a child process with `_MOOMBOX_CHILD=1`, waits for it to exit, and respawns if the exit code is 42 (restart requested). The child shares the launcher's console (the TUI needs it); `CREATE_NO_WINDOW` (0x08000000) is used only for the launcher's detached cleanup spawn. This keeps one stable parent process holding the console so the child's TUI can restore terminal state cleanly.

- **With `_MOOMBOX_CHILD=1`** — The process runs the full application stack. When a restart is needed (config change, update applied, setup wizard completion, or API request — from the web or the TUI), `triggerRestart(source)` sets an atomic flag, puts the web server into drain mode (503 for new requests), and five seconds later cancels the main context and quits the TUI if it is running. The `run()` function returns `true`, and `main()` calls `os.Exit(42)`.

- **Shutdown** — Context cancellation propagates to all services. A 15-second force-exit timer (`time.AfterFunc`) ensures the process terminates even if a service hangs. Shutdown order is: monitors, trim service, download worker, notifications flush, cookie services, PO token cleanup, web server, event subscribers, database close.

### Service Initialization Order

Services are initialized sequentially in `run()` inside `cmd/moombox/main.go`. The order matters because later services depend on earlier ones:

1. **Config** — `config.Load()` reads TOML, applies defaults, runs legacy migrations
2. **Logger** — slog wrapper with file rotation, ring buffer, pub/sub
3. **Updater** — GitHub release checker, cleans up `.old` binary from previous update
4. **Database** — SQLite WAL, 1 connection, migrations to the current schema version (`docs/spec/appendix-metrics.md`, where the volatile numbers live), synchronous `UpdateJobFields` writes, pub/sub
5. **CookieJar** — Netscape cookie file parsing, in-memory cookie store
6. **YouTube Service** — PlayerAPI + Auth + format selector, fetches homepage for visitor data and API key
7. **Twitch Service** — GQL API + Auth + EmoteResolver
8. **PotProvider + Sidecar** — BotGuard/PO token generation. Primary path via embedded Node.js + JSDOM + bgutils-js subprocess (real integrity tokens); goja-only fallback when sidecar disabled or unhealthy. Triple in-process cache (session, minter, inflight).
9. **CipherSolver** — YouTube signature/n-parameter decryption, 10-VM LRU, disk cache
10. **NotificationManager** — Discord webhook dispatch
11. **TrimService** — FFmpeg-based clip extraction from finished recordings; ahead of the worker, which runs every job's post-download trim through it
12. **DownloadWorker** — Job queue (100 lifecycle + N VOD download slots), backlog scheduler, stream processor, orchestrator
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
  database/        <- SQLite/WAL, synchronous writes, pub/sub, schema migrations
  cookies/         <- Netscape jar, refresh service, auto-cookie (Firefox/Chromium/DPAPI)
  youtube/         <- Service facade, PlayerAPI, Auth, watch page, format selector
  twitch/          <- Service facade, GQL API, Auth, HLS, IRC chat, VOD chat, emotes
  bgutils/         <- PotProvider, WebPoClient, Challenge, BotGuard, WebPoMinter
                  -- triple cache (session/minter/inflight) wrapping both paths
    sidecar/       <- Node + JSDOM + bgutils-js subprocess manager: extraction,
                  -- Job Object pinning, JSON-RPC mux (primary PO-token path)
    embed/         <- go:embed of node-windows-amd64.gz + node-linux-amd64.gz + node-linux-arm64.gz + sidecar.tar.gz + version.txt
  cipher/          <- Signature + n-param decryption: AST + regex, 10-VM LRU, disk cache
  engine/          <- SegmentDownloader (DASH/HLS/VOD), manifest parser, FFmpeg muxer
  chat/            <- YouTube live chat downloader (polling + batching + resume)
  worker/          <- DownloadWorker, DownloadOrchestrator, StreamProcessor, JobQueue,
                      TrimService, QualityMonitor, orphaned file scanner
  monitor/         <- FeedMonitor (RSS), DecapiMonitor, TwitchMonitor
  notifications/   <- Manager + Discord webhook sender
  web/             <- chi server, WebSocket hub, auth, middleware, rate limiter
  web/routes/      <- HTTP route handlers (jobs, auth, config, cookies, update, etc.)
  tui/             <- 2-over-1 panel layout, 15 overlays, chord system, Charm ecosystem
  goja/            <- JS runtime shims (minimal DOM, TextEncoder, timers)
  disk/            <- Disk space queries: kernel32 GetDiskFreeSpaceExW on Windows, statfs on Linux
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

**FeedMonitor** (YouTube RSS + members-only) — Polls YouTube's RSS feed endpoint (`/feeds/videos.xml?channel_id={id}`) for each monitored YouTube channel. Runs on a configurable interval (default 10 minutes) with jitter. When `membership_discovery` is enabled (default on) and YouTube auth cookies are present, it ALSO fetches each channel's authenticated `/membership` tab — RSS and DECAPI never list members-only content, so this is the only discovery source for members-only live/upcoming streams (and, with `include_non_live_content`, their VODs). Discovery is store-driven: every item either source lists is upserted into the persistent per-channel `feed_items` table, so nothing is ever "crowded out" of a transient per-cycle list. Each cycle the monitor walks the store's archive scope — everything published within `archive_window_days` (default 3, per-channel overridable) plus ALL upcoming/live items regardless of age and the still-unresolved ones (RSS items first seen inside the window, whose RSS date is only the announcement time) — serially probing candidates with `ProbeVideo()` — or the authenticated `ProbeVideoAuth()` for members-only items, so a members VOD isn't misfired as "upcoming" — to classify each (live/upcoming/VOD/regular), applying channel term filtering (include/exclude), and skipping anything already tracked (`HasActiveJob` dedup). Items that pass are archived via `OnVideoFound()`: broadcasts (live/upcoming) and VODs first seen this cycle — or by a recent cycle that has not jobbed them yet — become jobs immediately, while backlog VODs — older items already known to the store — are created as `Queued` and admitted at most `archive_slots` (default 3, per-channel overridable) at a time per channel by the worker's scheduler, so a backlog sweep never starves new or live content. The monitor uses a `MetadataFailureTracker` to stop retrying videos that consistently fail metadata probes.

**Catalog backfill** — When a YouTube channel is added (or a manual re-scan is forced via the TUI `R B` chord or `POST /api/backfill/rescan`), a dedicated backfill worker scans the channel's `videos`, `streams`, and (when membership discovery is active) `membership` tabs down to the archive-window depth, feeding everything found into the `feed_items` store — the full-catalog seed that RSS's ~15-entry feed can never provide. Scans run strictly serially across channels, paced at one tab page per second, resume from a persisted cursor after interruption, and report per-channel progress in both UIs. An every-cycle sweep re-queues channels that were never backfilled or whose archive window has since widened.

**DECAPIMonitor** — Polls DECAPI for the latest video from each monitored YouTube channel. This is a secondary detection mechanism that catches streams the RSS feed might miss (RSS updates can be delayed by minutes). Extracts video IDs from DECAPI responses, runs the same ProbeVideo + term filtering pipeline as FeedMonitor. Has its own rate limit tracking (respects DECAPI rate limit headers) and configurable check interval.

**TwitchMonitor** — Polls Twitch GQL for each monitored Twitch channel. Batches the `StreamMetadata` and `ComscoreStreamingQuery` persisted queries through `GetStreamInfoBatch` (`internal/twitch/api.go`) to check whether each channel is live. When a live stream is detected, extracts stream metadata (title, category, start time, thumbnail, profile image) and calls `OnStreamFound()`. Twitch jobs are created with `StatusLive` immediately (the monitor already confirmed live status), unlike YouTube jobs which start as `StatusUpcoming` and are probed by the StreamProcessor. A failure of a whole batch request (Twitch refusing the client, a transport error) is no one channel's fault and stays off every channel's health streak; the monitor Warns once when such a streak starts and logs its recovery.

All monitors share these patterns:
- **Signal-driven scheduling** — Use `time.Timer` (not `time.Ticker`) so the next check is scheduled after the current one completes, preventing overlap
- **CheckNow()** — Forces an immediate check cycle, used when channel configuration changes
- **OnSchedule callback** — Reports the next check time (epoch ms) for display in the status bar
- **Context-based cancellation** — `Start(ctx)` and `Stop()` for clean lifecycle management
- **No channels = idle** — Monitors with no configured channels of their type skip polling entirely

### Download Pipeline Detail

**StreamProcessor** handles the pre-download phase. For YouTube: probes video status using `ProbeVideoStatus()` (ANDROID_VR client, no cookies needed), classifies the result as live/upcoming/VOD/not-a-stream/members-only. For upcoming streams, enters a `waitForLive` loop: polls at a dynamic interval based on time until scheduled start (10 minutes if >1h away, 5 minutes if ≤1h, 30 seconds if ≤5min — `probeIntervalDistant`/`Near`/`Imminent`; a stream with no scheduled start polls every 5 minutes, since it can go live at any moment) plus random jitter (up to 30s), persists metadata from each probe (title, thumbnail, description, scheduled start time) with change detection so rescheduled streams and title changes are picked up automatically at zero extra network cost. Starts chat download during the wait phase (so chat messages from the "waiting room" are captured), and uses chat surge detection (30 messages within a 15-second window) to trigger early re-probing — a burst of chat messages often indicates the stream just went live. Sends a "Schedule Changed" notification if the scheduled start time shifts between probes. When authenticated, uses `TV_DOWNGRADED` client for members-only upcoming stream polling. The processor tracks consecutive probe errors and gives up after 10 failures. For Twitch: manual adds poll the channel via GQL every 15 seconds (plus 5s jitter) until the channel goes live or context is cancelled. Monitor-discovered Twitch streams skip this phase since the monitor already confirmed live status.

**DownloadOrchestrator** manages the full download lifecycle after the stream processor confirms it is ready. It selects a download strategy based on the stream type:
- **YouTube live DASH** — Sequential segment polling with head-probing
- **YouTube VOD** — Sequential whole-file download in 5MB Range-request chunks (`runDirectDownload`; streams the body instead when the server ignores Range)
- **YouTube VOD segmented** — Sequential segment download with known total
- **Twitch live HLS** — Playlist re-fetching with variant selection
- **Twitch VOD** — HLS segment download

The orchestrator starts the segment downloader and chat downloader concurrently via an `errgroup`. When the segment download completes, it runs a verification loop: up to 6 checks at 5-minute intervals (`streamEndVerifyInterval`) querying the YouTube API to confirm the stream actually ended. This guards against premature termination from transient 404s or temporary CDN outages. Only after verification confirms the stream is truly over does the orchestrator proceed to muxing.

Quality splits occur when the `QualityMonitor` (probing every 30 seconds) detects a resolution change mid-stream. The orchestrator creates a new segment file and continues downloading, resulting in multi-part recordings that the video player handles seamlessly. Segments shorter than 10 seconds are not split. A split job interrupted and restarted after the broadcast ended comes back as a VOD and downloads the whole recording from the start; once that download is complete it is the archive, and the earlier parts are kept beside it as `.restart-` siblings rather than finalized over it (`supersedePartsWithVod`, `internal/worker/vod_supersede.go`).

Twitch live recordings are additionally **gap-split**: Twitch has no DVR, so segments that leave the playlist window are unrecoverable. The engine appends while HLS sequence numbers stay continuous (a fast daemon restart or network blip resumes seamlessly with zero loss) and stops at any true discontinuity (`ErrGapDetected`); the orchestrator then muxes the current capture as a finished, internally-gapless part (`{name} - partN.mp4`) and continues at the live edge in a new part. The live IRC chat file rolls at every part boundary with offsets rebased to that part's start, and each part's chat is copied beside its video (`{name} - partN.chat.json`, recorded in `segments.chat_file`). Connectivity outages pause the job rather than finalizing it — one job per broadcast: when the connection returns and the same broadcast (stream_start_time identity) is still live, the same job resumes; the job finalizes only when the stream's end is confirmed or the broadcast changes — a recovery that cannot confirm either (no answer from the recheck, a variant refresh still failing after its retries) ends the job in Error with its staging kept rather than Finished. The same identity guards a restart: a job that already holds a capture (Downloading, a recorded position or part, or staged media — whatever its status) never attaches to a different broadcast; it ends with the "a new broadcast is live" error and its captured data stays muxable. A job that finalizes with exactly one part is renamed back to the plain template name. Both MPEG-TS and fMP4/CMAF delivery are supported: on fMP4 playlists the engine writes the `#EXT-X-MAP` init segment at the head of each part file (recognizing token-rotated init URIs by content hash), and a genuine mid-part init change (transcode restart) or an fMP4→TS reversion part-splits via `ErrInitSegmentChanged` — handled like a gap split, minus the lost-data notification.

SEGMENT muxes run on goroutines parented by the orchestrator's mux root rather than by the job's own context, so a finished part is still muxed into a usable file even if that job is cancelled (user quits during download) rather than abandoned as raw segments. The job's FINAL mux is deliberately not on that root — it runs on the job's own context, so a cancel reaches it and the row stays `Muxing` for the next start; the off-queue restart/Mux path and the Twitch outage finalize, which have no job context to run on, use the root. The root is not `context.Background()` either: shutdown cancels it once the worker's ten-second wait for in-flight jobs runs out, so a daemon exit kills FFmpeg instead of leaving it writing into a staging dir the restarted child re-muxes over. Either way those rows stay `Muxing` with their staging deliberately intact, and the next start re-muxes them from it.

**SegmentDownloader** has three modes:
- **DASH sequential** — Increments segment number, fetches `{base_url}/sq/{n}`, handles 404 with exponential backoff. Saves resume state every 50 sequential segments. Verification is time-based: once the gap since the last segment crosses 30s, calls the `CheckStreamStatus` callback (re-checked at most once per 30s) to verify whether the stream is still live. If the stream ended, exits cleanly; if still live, keeps waiting. A configurable `maximum_timeout` (default 600s, YouTube only) force-finalizes the recording if no segment arrives for that long even while YouTube still reports the stream live (its status can lag or stick); the clock resets whenever a segment lands, and offline time pauses it.
- **HLS polling** — Re-fetches the media playlist, identifies new segments by media sequence number (`#EXT-X-MEDIA-SEQUENCE` plus position, compared against the next sequence the file needs), downloads them in order. YouTube HLS honors the same `maximum_timeout` backstop — on both strategies it ends the downloader's loop without marking the stream ended, so the resume sidecar survives for the worker's re-verify and a still-live refresh resumes from it; Twitch HLS relies on its GQL end-detection instead. Saves resume state at the same interval as DASH.
- **VOD direct** — Knows the total size and downloads it sequentially in 5MB Range-request chunks, up to 3 attempts per chunk (`MaxChunkRetries`); there is no worker pool on this path. A chunk refused with 403 or 410 — the URL expired mid-transfer — re-extracts a fresh URL for the same file through `OnCredentialRefresh` instead of ending the job, and a failure while the connectivity monitor reports an outage is waited out rather than charged. Reports percentage progress throttled to 500ms intervals (`ProgressThrottle`) to avoid flooding the UI.

**Catch-up mode** activates when the downloader falls more than 10 segments behind the live head (`CatchupThreshold`), past a 30-segment (`stayBehindSegments`) buffer that avoids racing in-flight segments. It hands off to a rolling window of `segment_workers` parallel workers (default 12, configurable, no upper limit — distinct from `num_parallel_downloads`, which gates concurrent VOD jobs and never applies to a live broadcast) until caught up, then resumes sequential downloading. Workers claim sequences continuously and flush completed segments in strict ascending order as they arrive, rather than waiting on per-batch barriers. This prevents permanent drift during transient slowdowns. Both the catch-up path and the parallel VOD path hold out-of-order segments in a byte-bounded reorder buffer while the head-of-order segment works through its retry ladder, and two settings bound that memory: `reorder_buffer_mb` caps one download and `reorder_budget_mb` caps every live download between them (`0` = unbounded on either, and a per-job value above the budget is clamped to it at startup with a warning). The defaults are 1024 MB and 4096 MB, or 256 MB and 1024 MB on arm64 — the one place in the config where a default is platform-conditional, because the original 256 MB ceiling was chosen for an arm-class box. Both are re-applied on save from either UI, so neither needs a restart; the head-of-order segment is admitted regardless of either ceiling, since nothing frees them without a flush and nothing flushes without its head.

**Resume state** (`.resume.json` sidecar) stores `lastSeq`, `bytesWritten`, `timestamp`, and `baseUrl`. On startup, the downloader checks for a resume file, validates it, and resumes from the last checkpoint rather than starting over.

**JobQueue** implements dual-layer concurrency: 100 lifecycle slots (`maxLifecycle`) gate how many jobs can be in the DOWNLOAD half of the pipeline simultaneously — downloading plus muxing — while a configurable download semaphore (`maxDownloads`, default 10) gates how many VOD jobs can be actively downloading segments. The pool gates VODs ONLY: a broadcast is never made to wait for a slot — missing a slot on a VOD delays a file that already exists, while missing it on a live broadcast loses footage — so peak concurrent downloads is (live broadcasts) + `num_parallel_downloads`. This design means stream probing, waiting-for-live, and auth negotiation do not consume download slots — only active segment downloading does. The download slot is acquired when the orchestrator begins segment downloads and released when it finishes (before muxing). The LIFECYCLE slot is claimed at the download decision rather than at dequeue, so a job still probing or waiting for its stream to go live holds neither: an `Upcoming` job or a manually-added offline Twitch channel used to occupy one for its whole wait — hours to days — and at 100 such waiters a newly live stream was never started at all.

The worker also owns the **backlog Scheduler**: a single admission goroutine that is the only path out of `Queued`. Woken by backlog-job creation and job completion (with a heartbeat safety net), it admits per channel at most `archive_slots` minus that channel's in-flight backlog jobs, newest published first — writing `Upcoming` durably before enqueueing so a crash between the two steps self-heals on restart. That write is a compare-and-set on `Queued` (`UpdateJobFieldsIf`, `internal/database/database.go`), so an operator's Cancel that lands between the scheduler's read and its write stands instead of being turned back into a download. `ShouldProcess(Queued)` is false by design, so neither startup recovery nor the heartbeat poller ever touches a `Queued` row. It admits nothing while the connectivity monitor reports offline and sweeps again when connectivity returns, nor from the moment the output volume reaches `disk_critical_percent` until usage is 2 points below it — the disk alerts' own reading and recovery margin, taken fresh each sweep, so a held backlog resumes within a heartbeat of that much space being freed (`Scheduler.diskGateClosed`, `internal/worker/scheduler.go`) — and a backlog job whose pre-download fetch fails transiently goes back to `Queued` held for a backoff (at most three times before it ends in Error) rather than straight to Error — together they stop an outage draining a channel's whole backlog into Error (`requeueBacklogAfterTransientFailure`, `internal/worker/backlog_retry.go`). A backlog job whose download or mux fails because the disk is full takes the same way back to `Queued` (`requeueBacklogAfterDiskFull`, `internal/worker/backlog_retry.go`), so a full disk the operator then clears costs the backlog a wait, not its jobs.

Priority ordering: Live=1 (highest), everything else 0 — every retry path writes Upcoming or Downloading before it enqueues. Live streams are always processed before upcoming or retried jobs. The pending queue caps at 100 entries; jobs beyond that are dropped with a warning log. Duplicate detection uses both the pending set and the processing map — a job that is already pending or actively processing is not re-enqueued.

**processJob flow** (DownloadWorker.processJob, runs in a goroutine per job):
1. Fetch job from database, verify it is still in a processable state
2. Call StreamProcessor.Process() — probes, waits for live, handles auth, holding no slot
3. If result says "should download": acquire the download slot (VODs only; a broadcast passes through), then the lifecycle slot (blocks if all 100 are in use) — in that order, so a VOD queueing for the download pool holds no lifecycle slot a live broadcast needs. A queueing VOD keeps its pre-download status with a "Waiting for a download slot..." progress line, and once both slots are held an extraction older than an hour (or whose format URLs expire within one) is re-extracted (`refreshStaleVodInfo`, `internal/worker/vod_slot_wait.go`), since the googlevideo URLs it carries live ~6 h; a re-extraction that fails transiently while those URLs have yet to expire leaves them in use
4. Run DownloadOrchestrator.ExecuteWithChat()
5. Release the download slot — done inside the orchestrator before muxing, and for a VOD earlier still, before the chat wait (`resolveVodChatOutcome`); `processJob` itself never releases it
6. Update job status to Finished or Error
7. Send notification (download complete / error)
8. Release the lifecycle slot — a deferred `Complete` on every exit path, so a job that declined to download or errored out still gives back whatever it took

The worker also runs a 60-second heartbeat poll (`heartbeatInterval`) as a safety net to catch any jobs that were missed by signal-driven notification. Normal job discovery is signal-driven via the `notifyJob` channel — when `EnqueueJob()` is called, it sends a non-blocking signal to wake the worker's dispatch loop.

### Concurrency Patterns

| Component | Pattern | Parameters |
|-----------|---------|------------|
| Worker | Dual semaphore | 100 lifecycle slots (downloading + muxing) + 10 download slots (VODs only, configurable) |
| WebSocket | No hub throttle | Rate bounded upstream by ProgressTracker (one report per configured progress interval per job; ~60 Hz at the 16 ms default) |
| Database | Synchronous writes under `db.mu` | No background writer; the write rate is the callers' (ProgressTracker's per-job gate), zero idle I/O |
| TUI | Non-blocking sends | Drop counters for diagnostics |
| TUI logs | Batched flush | 250ms flush interval |
| BotGuard | Sidecar + triple cache | Sidecar minter (internal, ~6h TTL); session cache (6h TTL); minter cache (dynamic TTL, goja-fallback only); inflight dedup |
| Cipher | LRU with mutex | 10-VM cache (`solverCacheSize`), mutex-serialized compilation |
| Cookie refresh | Periodic | 30-minute interval with immediate-check capability |
| Update check | Periodic | 5s initial delay, then 24-hour interval |
| Disk check | Piggyback on memory ticker | Every 3rd tick of 2-minute memory diagnostic |

### Panic Recovery

Every goroutine has inline `defer func() { if r := recover(); ... }()` as the first statement. The HTTP server uses `RecoveryMiddleware` that catches panics, logs the stack that panicked (on the panic line itself: one line, innermost frame first, at most 32 frames, function and file:line only — no argument values), and returns a 500 Internal Server Error response; `outermostRecovery`, the server's outermost handler, does the same for a panic in what runs ahead of it (chi's `RequestID`, `DrainMiddleware`, the WebSocket upgrade's own gates), which `net/http` would otherwise report only to the discarded `ErrorLog`. Database subscriber callbacks use `safeCallJobUpdate`/`safeCallJobsChange` wrappers that isolate individual subscriber panics so one bad callback does not prevent other subscribers from receiving updates. Monitor callbacks (`OnVideoFound`, `OnStreamFound`) have explicit `defer recover()` blocks wrapping the job creation logic. The shutdown sequence uses `stopService()` which wraps each individual service stop in panic isolation — if one service panics during shutdown, the remaining services still get their stop calls.

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

### Error Classification

There is no typed error hierarchy and no errors package under `internal/`. Errors are plain Go errors built with `fmt.Errorf("...: %w", err)`, and the only classification is sentinel matching with `errors.Is`:

```go
worker.ErrCookiesRequired   // player-API "login required" / "member-only": park at COOKIES?
worker.ErrNotAMember        // members-only refused to a SIGNED-IN session: park at COOKIES?, but no auto cookie refresh
twitch.ErrTwitchAuthExpired // Twitch token dead: park at COOKIES?
twitch.ErrSubscriberOnly    // sub-only VOD/stream: park at COOKIES?
worker.ErrNonActionable     // age-restricted, probe budget exhausted: Error, notification suppressed
```

`DownloadWorker.setJobError` picks `COOKIES?` when `cookiesStatusError(err)` matches one of the first four and `Error` otherwise; the error text becomes the job's `error` column. Cancellation is not an error class at all: `handleCancellation` asks the queue whether the user cancelled (`WasCancelled`) and writes `Cancelled`, or leaves the status untouched on a shutdown so the job resumes on restart. A failure that a Cancel flagged first, or that finds the row already `Cancelled` — the Cancel landed as the run failed — leaves it so and ends the run through `handleCancellation` (`setJobError` asks `JobQueue.settle`, then writes with `UpdateJobFieldsUnless`); a Cancel that reaches the run after its failure is recorded is the canceller's to report. A run that panics settles the same way, ahead of the `queue.Complete` that would drop a Cancel's flag (`recordRunPanic`, `internal/worker/worker.go`): one a Cancel flagged first ends cancelled and sends the Job Cancelled left to it, and one that had recorded its outcome before the panic — a `COOKIES?` park, a requeue to `Queued` — keeps it. There is no "expected vs internal" flag — what the user can act on is expressed by the status the job lands in.

**Deep-dive:** [docs/spec/architecture.md](docs/spec/architecture.md)

---

## 4. Platform Services

### YouTube

YouTube integration reimplements yt-dlp's extraction logic in Go. The core is a multi-client Innertube strategy that fetches video info from multiple YouTube API clients and merges their format pools.

**Client fallback chain for authenticated fetches** (`GetVideoInfoAuthenticated`, `internal/youtube/player_api_strategy.go`):
1. Fetch watch page (WEB client) — extracts `ytcfg` (visitor data, API key, player URL), inline player response
2. **WEB_EMBEDDED_PLAYER** — queried first, as a format-pool and DASH contributor only; it never drives playability classification
3. **TV_DOWNGRADED** (TVHTML5, clientID 7) — primary authenticated client, sends cookies; the playability authority
4. **WEB** (clientID 1) — for DASH manifest URL (TV client sometimes lacks it)
5. **WEB_CREATOR** (clientID 62) — fallback for members-only content
6. **VISIONOS**, then **ANDROID_VR** (clientID 28) — the cookieless chain, run when WEB_CREATOR is inadequate too; no cipher needed

Each client's formats are tagged with an auth level (`internal/youtube/types.go`, lowest first): `AuthLevelTVPublic` (0), `AuthLevelTVAuth` (1), `AuthLevelWatchPagePublic` (2), `AuthLevelWatchPageAuth` (3), `AuthLevelWebSafari` (4), `AuthLevelWeb` (5), `AuthLevelWebEmbedded` (6), `AuthLevelWebCreator` (7), `AuthLevelVisionOS` (8), `AuthLevelAndroidVR` (9). The order is yt-dlp's client priority — tv, then the web family, then the cookieless last resorts — not a measure of cookie-freeness; see the table in [docs/spec/platform-services.md](docs/spec/platform-services.md). Formats from different clients are pooled and deduplicated by itag. Format selection priority: resolution cap (`max_video_resolution` compares the SHORTER frame dimension, resolves to the largest size at or below the cap or the closest above it, and treats `0` as unbounded — `internal/utils/resolution.go`) > long edge within that size > FPS (if prefer60fps enabled) > video codec score (av01=6 > vp9.2=5 > vp9/vp09=4 > h265/hevc=3 > h264/avc=2 > vp8=1) > bitrate > auth level (lower = the higher-priority client in the order above). Audio codec priority: opus=4 > mp4a.40.2 / mp4a.40.5=2 (AAC-LC and HE-AAC tie, so bitrate decides) > mp4a=1.

**Probe vs. full fetch:** Two distinct code paths serve different needs. `ProbeVideoStatus()` uses ANDROID_VR (lightweight, no cookies, no cipher, no watch page fetch) to quickly classify a video as live/upcoming/VOD/offline/members-only. It is used by monitors for pre-filtering and by the stream processor for polling. `GetVideoInfoAuthenticated()` runs the full multi-client chain: fetches the watch page, extracts ytcfg, tries multiple Innertube clients, decrypts signatures and n-parameters, and returns the merged format pool. It is used when actual download URLs are needed.

**Watch page parsing:** The watch page HTML is fetched with the WEB user agent and cookies. It yields: `ytcfg` (visitor data, API key, player.js URL, client versions), inline player response (can contain formats directly), and initial chat continuation tokens (for live chat download).

**Cipher decryption:** YouTube obfuscates streaming URLs with a signature cipher and an n-parameter throttle. Without decryption, URLs return 403 or are throttled to unusable speeds. The cipher solver downloads `player.js` from YouTube's CDN, extracts the transformation function chain via AST parsing of the JavaScript (identifying the function by structural patterns in the obfuscated code). If AST parsing fails, it falls back to regex pattern matching against known obfuscation patterns. The extracted JavaScript is compiled into Goja VMs and cached. Two-tier cache: memory (10-VM LRU keyed by player URL) for instant reuse, and disk (24-hour offline TTL, `playerCacheTTL`, with conditional-GET revalidation on every fetch) to avoid re-downloading player.js. The solver also extracts `signatureTimestamp` (STS) from player.js — this value must be sent in Innertube API requests or the returned formats will have invalid URLs.

**N-parameter decryption:** Separate from signature cipher but using the same extraction infrastructure. The `n` parameter in YouTube URLs controls throttling — the obfuscated value triggers aggressive rate limiting. The n-parameter function is extracted from player.js, compiled to a Goja VM, and used to transform the parameter. Same caching as signature cipher.

**Stream status classification:** The player response is parsed into one of: `StreamNotAStream` (regular video, not live), `StreamLive` (currently broadcasting), `StreamUpcoming` (scheduled, not yet live), `StreamPostLive` (the broadcast ended and YouTube is still processing the recording), `StreamVOD` (a finished stream served as an ordinary video). Those five are the whole of `StreamStatus` in `internal/youtube/types.go`. Classification uses `playabilityStatus.status`, `videoDetails.isLiveContent`, and `videoDetails.isLive` from the player response. Members-only streams are detected via `playabilityStatus.reason` containing membership-related text.

### Twitch

Twitch integration uses the GQL API with persisted query hashes (SHA256). No REST API — everything goes through `https://gql.twitch.tv/gql`.

**GQL operations:** `StreamMetadata` and `ComscoreStreamingQuery` (live stream info, sent as one batched request), `VideoMetadata` (VOD metadata), `VideoCommentsByOffsetOrCursor` (VOD chat) — those four persisted hashes are `TwitchGQLHashes` in `internal/constants/constants.go`. The two playback-access-token calls (`StreamPlaybackAccessToken`, `VideoPlaybackAccessToken`) send GraphQL text rather than a persisted hash. Each persisted operation is identified by its SHA256 hash, not the query text — Twitch's GQL endpoint routes requests by hash for caching. The `Client-ID` header is required on all GQL requests. Auth token (from cookies, specifically the `auth-token` cookie) is sent in the `Authorization: OAuth {token}` header when available.

**HLS variant selection:** The flow is: get stream/VOD access token via GQL, build Usher URL with the token, fetch the master playlist, parse `#EXT-X-STREAM-INF` lines into variant structs (resolution, frame rate, bandwidth, codecs, group ID). Selection reads the job's `quality_preference` (e.g., "1080p60", "best", "720p", "audio_only"; written at creation and never overwritten, an empty one selecting as "best") — the variant picked is recorded in `twitch_quality` — inside the size `max_video_resolution` resolves to. Sizes are the frame's SHORTER edge, the cap's own measure, so a portrait stream's "720p" is its 720x1280 rendition. A height preference picks at the size it names, descending to the next lower size when that one is missing, and an fps suffix keeps that size's renditions at its rate when there are any. The renditions of that size — or, for "best" or a preference nothing matches, of the size the cap chose — are ranked by codec (AV1 > HEVC > H.264), then the frame rate `prefer_60fps` asks for (an fps suffix asks for its own instead), then source, then bandwidth. Any other preference is a substring match on the variant name, and "audio_only" takes the audio-only rendition. See [docs/spec/platform-services.md](docs/spec/platform-services.md) § Variant Selection Algorithm.

**IRC chat:** Connects to `wss://irc-ws.chat.twitch.tv:443` via WebSocket. PASS and NICK are a PAIR rendered from one decision per session: authenticated is `PASS oauth:{token}` **with** `NICK {login}` (the account's own name, from the `login` cookie), anonymous is `PASS SCHMOOPIIE` with `NICK justinfan{random}`. A token beside the `justinfan` nickname is the hybrid Twitch refuses, so anything short of a complete, sendable pair falls all the way back to anonymous. A credentialed session Twitch never welcomes falls back to anonymous once per credential pair — for the rest of the job unless the cookie file's Twitch pair changes or Twitch auth recovers, when every live chat session is told to reconnect with the current credentials (`DownloadWorker.ReauthenticateTwitchChats`) — and notifies once per pair. Joins with `JOIN #{channel}`; requests capabilities (`ircCapRequest`: `CAP REQ :twitch.tv/tags twitch.tv/commands` — `twitch.tv/membership` is deliberately not requested, owner decision O-S, because it only adds JOIN/PART bursts) for rich message metadata. Parses IRC messages into structured chat events, handling PRIVMSG (chat messages with badges, emotes, color) and USERNOTICE (subscriptions, raids, gifts); `parseLine` turns no other command into an event — CLEARCHAT, CLEARMSG and ROOMSTATE are not handled — while the read loop still answers PING, honours RECONNECT and classifies a login-failure NOTICE. Maintains PING/PONG keepalive. See [docs/spec/platform-services.md](docs/spec/platform-services.md) § IRC Chat (Live).

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

YouTube requires Proof of Origin (PO) tokens for certain requests, particularly for premium-quality formats and live streams. Moombox runs BotGuard via an **embedded Node.js sidecar** that produces real integrity tokens. The in-process goja path runs only when the sidecar is turned off (`[bgutils] use_sidecar = false`), and BotGuard's timing check rejects it, so it mints no PO token; while a configured sidecar is down a mint fails at once. Either way, without the sidecar PO-token-gated formats are unavailable.

**Architecture (sidecar, or goja when the sidecar is turned off):**

1. **Sidecar path (preferred)** — A bundled Node.js v24 binary plus `bgutils-js` + JSDOM are extracted from `go:embed`'d blobs to `%LOCALAPPDATA%/Moombox/sidecar/` on first launch (~38 MB embed: ~34 MB gzipped node.exe + ~4 MB tarball of production node_modules + src/server.js). Moombox spawns the subprocess pinned to a Windows Job Object (so the child dies with the parent), pipes JSON-RPC requests over stdin/stdout, and consumes real PO tokens. First mint hits Google's WAA endpoint in ~460 ms; subsequent mints with the same binding hit the sidecar's internal minter cache in ~500 µs.

2. **Goja path** — Only when the sidecar is disabled (`[bgutils] use_sidecar = false` in config, so no sidecar is attached) does `PotProvider` run the legacy in-process flow. A configured sidecar that fails to start or dies mid-flight does not fall through to it: a mint fails at once (`errSidecarDown`) until the supervisor brings the sidecar back.
   1. Fetch challenge (POST to `jnn-pa.googleapis.com` or YouTube fallback)
   2. Load BotGuard interpreter JavaScript
   3. Execute the interpreter in a Goja VM with full DOM shims (real-class hierarchy: `EventTarget`, `Node`, `Element`, `Document`, `Window`, `CSSStyleDeclaration`, `URL`, `AbortController`, `DOMTokenList`)
   4. Take a snapshot, POST to `GenerateIT`
   5. Mint per-binding tokens via the returned minter callback. When GenerateIT returns no integrity token — the usual outcome in goja, which fails BotGuard's timing check — the mint fails: the `websafeFallbackToken` it returns instead is not used as a PO token (YouTube does not accept it for player requests; `webpo_client.go`, "Path B removed"). So while the sidecar is off or down no PO token is minted, and PO-token-gated formats are unavailable.

**Why the sidecar exists:** The goja interpreter runs ~100× faster than V8's JIT, and BotGuard uses snapshot wall-time as a "this isn't a real browser" signal. The hand-rolled real-class DOM shimming (test.50–test.55) raised goja's API fidelity to browser parity but couldn't bridge the timing gap. Real Node + V8 + JSDOM passes the timing fingerprint.

**Triple cache (in-process, applies to both paths):** Session cache (6-hour TTL, keyed by content binding) caches the final PO token. Minter cache (single minter per process, dynamic TTL from BotGuard response) caches the goja-side minter — effectively unused under sidecar mode since the sidecar has its own internal minter cache. Inflight dedup (concurrent requests for the same key share a channel) prevents thundering herd. The session cache is the authoritative "is this token still fresh" surface; the sidecar's internal caches handle BotGuard VM reuse.

### Cipher Solver

Dual extraction approach for YouTube's obfuscated `player.js`:

1. **AST parsing** (primary) — Parses the JavaScript, finds the signature transformation function chain and n-parameter function by structural patterns
2. **Regex fallback** — Pattern-matches known obfuscation patterns when AST parsing fails

Results are compiled into Goja VMs. Memory cache: 10-VM LRU keyed by player.js URL. Disk cache: raw extracted JavaScript with a 24-hour TTL (`playerCacheTTL`) that only matters when revalidation cannot reach YouTube — every fetch revalidates with a conditional GET, so a rotated player is picked up regardless. The compile mutex serializes compilation to prevent thundering herd when multiple goroutines need the same player.

### YouTube Live Chat

Polls YouTube's `live_chat/get_live_chat` endpoint using continuation tokens. The lifecycle: initial continuation from watch page HTML, then follow continuation chains. Supports "Top Chat" and "All Chat" (upgrades automatically when available). Messages are deduplicated via a 5000-ID sliding window. Written to disk incrementally (JSON format) with a flush interval of 1 second. Resume sidecar (`.resume.json`) stores continuation token and message count for crash recovery.

### Caching Summary

| Resource | Cache Type | TTL/Size | Key |
|----------|-----------|----------|-----|
| PO token sessions | In-memory map | 6 hours | Content binding |
| PO token minters | In-memory map | Dynamic (from VM) | Request key |
| Cipher VMs | In-memory LRU | 10 VMs max (`solverCacheSize`) | Player.js URL |
| Cipher JS | Disk files | 24 hours (`playerCacheTTL`), revalidated by conditional GET | Player.js URL hash |
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
| `app.js` | Main SPA module. Job list rendering (`renderJobs`/`renderJobItem`/`renderArchivedJobs`), WebSocket connection management, status bar, theme switching. The Files tab, log viewer, version indicator/update dialog, unified filter bar and job details dialog are delegated to controller modules below — each constructed with `this` in the same pattern as `settings.js`/`stats.js` — with a one-line delegating method kept on `MoomboxApp` for any cross-module call site. |
| `modules/files.js` | `FilesController` — the Files tab: orphaned output files and orphaned feed-history entries, their refresh/delete-all actions |
| `modules/log-panel.js` | `LogPanelController` — the log viewer: level filter buttons, debounced text search with match highlighting, auto-scroll with the resume pill, clear |
| `modules/update-indicator.js` | `UpdateController` — the header version indicator and the update-available dialog (apply/skip) |
| `modules/filter-bar.js` | `FilterBarController` — the unified filter bar shared by the Tasks and Archived tabs: token parsing/rendering, chip removal, channel/platform pickers |
| `modules/job-details.js` | `JobDetailsController` — the job details dialog: render, live updates, action buttons, per-job logs |
| `modules/player.js` | Video player with per-job chat replay: niconico-style media-time scrolling overlay, chat sidebar with pre-show/post-end dividers, chat search, per-job chat offset, resume/watched tracking, per-part Twitch chat merge, multi-segment seeking. The sidebar also renders Super Chat / Super Sticker tier cards, membership cards, Twitch sub/resub/gift/raid notice blocks and the cheer chip — sidebar only; the overlay keeps plain scrolling text |
| `modules/segments.js` | `SegmentPlayer` — multi-segment playback helper shared by the player and the trimmer |
| `modules/chat-timeline.js` | Pure chat/video timeline math: offset normalization, chat-to-video bias, pre-show/post-end partitioning, per-part chat merge |
| `modules/nico-lanes.js` | `LaneAllocator` — niconico lane-collision math for the overlay's right-to-left scrolling |
| `modules/nico-geometry.js` | Pure niconico overlay geometry: the centred-fit `letterboxStage`, `rowsFor` row count, and `sameStage`/`nextGeometry` change detection |
| `modules/nico-scheduler.js` | `NicoScheduler` — the niconico overlay's cursor/anchor/pending-list/drop-count state machine and the `NICO_*` tuning constants |
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

**Frontend test harness:** `web/tests/` runs pure-JS suites (chat-timeline, filter-engine, filter-parser, logout, nico-geometry, nico-lanes, nico-scheduler, utils) plus two jsdom-backed suites (`player.test.mjs`, `app.test.mjs`) on Node's built-in test runner (`node --test web/tests/*.test.mjs`); jsdom is an optional dev dependency installed separately inside `web/tests/`. See `web/tests/README.md`.

### TUI

The TUI uses Charmbracelet's full suite: bubbletea for the Elm architecture, bubbles for pre-built components, huh for form/dialog wizards, and lipgloss for styling.

**Layout:** Two-over-one split — two panels side-by-side on top, full-width logs on bottom. The focused panel's row expands vertically (top focused = 70% height, logs focused = 75% height). Width split depends on focus: tasks focused = 45%/55%, details focused = 35%/65%, logs focused = 50%/50%:
- **TaskList** (top left) — Job list with status icons, channel names, titles. Scrollable, filterable — both UIs share one filter language (`internal/jobfilter` is the Go twin of `filter-parser.js`/`filter-engine.js`): the TUI's `/` box takes free text plus `status:`/`channel:`/`platform:` tokens, `-` negation and `a|b` OR groups, and its `F` key cycles the query's `status:` token.
- **JobDetails** (top right) — Selected job's metadata, progress, segment counts, file info.
- **Logs** (bottom, full width) — Real-time log output, 250ms batched flush, scrollable viewport.

**Overlays (14):** Action Menu, Help, Add Video, Import, Cookie Import, Trim, Orphaned Files & History, Client Tokens, yt-dlp Plugin, Statistics, Settings, Setup Wizard, FFmpeg Check, Release Notes.

### Chord System

The TUI uses a chord-based keybinding system with a single source of truth:

- `buildMenuItems()` in `internal/tui/app_actions.go` defines all chords, their display text, action menu entries, hint bar text, and help text in one place.
- `dispatchAction(chord, job)` is the unified handler that executes the action for any chord.

**Chord prefixes:** A=Action (AC=Cancel, AD=Delete, AR=Resume, AI=Reinitialize, AA=Add), R=Request (RC=Recheck Cookies, RF=Refresh Cookies from Browser), O=Open (OF=Folder, OS=Stream Page, OW=Web UI), E=Extras (EY=yt-dlp Plugin, EL=Cookie Login, EI=Import Cookie File, ET=Statistics), Q=Quit (QQ=Quit confirm). **Single keys:** F=Filter, M=Action Menu, `=Settings, ?=Help. **Confirm chords** require a third keypress within 3 seconds (e.g., "Q" then "Q" within 3s to quit).

### WebSocket Protocol

The WebSocket connects on any path (upgrade handler intercepts before static file serving). Message format: `{"type": string, "payload": any}`.

**Server-to-client message types:**
- `job_update` — A job changed in a way a progress tick does not (status transition, error, chat status, mux output, new job, trim edit)
- `job_progress` — Progress-only tick of an active download, one per configured progress interval per job (~60 Hz at the 16 ms default) (payload: `{id, status, progress, percent, speed, eta, lastVideoSeq, lastAudioSeq, totalVideoSeq, totalAudioSeq, totalChatMessages, updatedAt}`, merged client-side)
- `jobs_update` — Full job list refresh (payload: array of all visible jobs)
- `job_deleted` — A job row was removed (payload: `{id}`)
- `config_update` — A config setting that affects client-side rendering changed (payload: partial config; currently `{hideFinishedAgeDays}`)
- `log` — Log line (payload: string; beside it, `seq` — the line's number in the logger's ring, which the dashboard compares with `initial_state`'s `logSeq` to skip a line the snapshot already holds)
- `check_timers` — Monitor schedule update (payload: `{nextFeedCheck, nextDecapiCheck, nextTwitchCheck}`)
- `initial_state` — Sent on connect (payload: `{jobs, logs, logSeq, nextFeedCheck, nextDecapiCheck, nextTwitchCheck, connectivity, hideFinishedAgeDays, backfill, runningTrims}`; `logSeq` numbers the newest line of `logs`)
- `update_available` — New version found (payload: release info)
- `update_cleared` — The pending release was withdrawn: skipped, or a check found nothing newer, so it was pulled (payload: `{tagName}`; a dashboard drops its badge only when the tag names the release it shows)
- `disk_status` — Disk space update (payload: `{free, total, usedPct, warnLevel}`)
- `connectivity` — Network reachability changed (payload: `{online}`)
- `backfill_status` — Per-channel backfill scan progress (payload: `{channel, tab, pages, state}`)
- `trim_status` — A trim the trim service runs, from either UI or a finished job's post-download trim (payload: `{id, jobId, startTime, endTime, progress, state, trim?, error?}`; `state`: running — as it starts, then with FFmpeg's percentage at most every 250 ms — then finished with the stored `trim` record or failed with an `error` written for the user). Running trims are also seeded via `initial_state`, so a reload mid-trim still shows its progress bar
- `pong` — Reply to the client's `ping` (payload: none)

That list is the whole wire protocol: the hub's own `Broadcast` helpers in `internal/web/websocket.go` (`job_update`, `job_progress`, `jobs_update`, `job_deleted`, `check_timers`, `connectivity`, `log`), the `cmd/moombox` callers (`update_available`, `update_cleared` via `announceUpdateCleared`, `disk_status`, `backfill_status`, `config_update`, and `trim_status` via `trimStatusFrames`), the `initial_state` snapshot the hub marshals on connect, and the `pong` reply.

The `hideFinishedAgeDays` field in `initial_state` and `config_update` drives the Web UI's client-side archive re-evaluation: on every `job_update`/`jobs_update` and on a 60-second idle sweep, the Web UI moves Finished jobs that have aged past the threshold from the active panel into the Archived panel. The TUI's `isJobArchived` reclassification (`internal/tui/task_list.go`) does the same, and the active panel stays in sync with wall-clock time without a page refresh. Every Go classifier — that TUI bucket, the REST `/api/jobs` split, the `job_update` broadcast gate and the `cmd/moombox` list filter — runs the one predicate in `internal/jobfilter/archive.go` (`ArchiveCutoff`, `IsArchived`), which scales the fractional day threshold exactly and treats the boundary as exclusive: a Finished job whose `updated_at` sits exactly on the cutoff stays active, as does one whose timestamp is missing or unparseable.

**Broadcast rate:** No hub-level throttle. The high-frequency caller (`OnJobChange` driven by `ProgressTracker.maybeUpdate`) is already capped to one report per job per configured progress interval — `downloader.progress_interval_ms`, whose 16 ms default is `progressUpdateInterval` in `internal/worker/progress.go`, so ~60 Hz unless an operator says otherwise — and now broadcasts the slim `job_progress` frame; `job_update` carries the state transitions, and the other callers are event-driven, not loops. A previous per-job throttle in the hub was removed because it raced against the (unthrottled) `BroadcastJobDeleted` and could resurrect deleted rows on the trailing edge.

**Connection management:** 30-second ping interval, 10-second write timeout, 4 KiB client read limit (the only client message is `{"type":"ping"}`), 16-frame per-client backpressure queue.

### API Route Catalog

All API routes use the `/api/` prefix (no version). Non-API routes exist for POT provider compatibility and health checks (`/ping`, `/minter_cache`, `/get_pot`, `/invalidate_caches`, `/invalidate_it` — all but `/ping` loopback-only and CSRF-exempt).

The complete catalog — every route, its body, its answers and its rate limit — is [REST API Routes — Complete Catalog](docs/spec/user-interfaces.md#rest-api-routes--complete-catalog) in `docs/spec/user-interfaces.md`. It is the one copy: `internal/web/routes/route_table_parity_test.go` fails when it and the router's registrations drift apart in either direction, which a second hand-kept list here could not promise (the one that stood here had fallen behind by more than a dozen routes, and said `DELETE /api/jobs/{id}` deletes files, which it never has).

### TUI Backend Communication

The TUI communicates with the web server via HTTP to `localhost:{port}`. A custom `http.RoundTripper` injects the `X-Internal-Token` header (16-byte random hex, generated at server startup) on every request. This bypasses CSRF validation and authenticates the TUI as a same-process client.

For real-time updates, the TUI does NOT use WebSocket. Instead, it subscribes directly to database pub/sub callbacks since it runs in the same process. This is more efficient than serializing to JSON and deserializing — the TUI receives typed Go structs directly. Updates are forwarded via buffered channels with non-blocking sends:

```
Database.OnJobChange()     -> jobUpdateCh (cap 100)      -> tea.Cmd -> TUI model
Database.OnJobAdded()      -> jobAddedCh (cap 100)       -> tea.Cmd -> TUI model
Database.OnJobDeleted()    -> jobDeletedCh (cap 100)     -> tea.Cmd -> TUI model
Database.OnTrimsChanged()  -> jobTrimsChangedCh (cap 50) -> tea.Cmd -> TUI model
Database.OnJobsChange()    -> jobsUpdateCh (cap 10)      -> tea.Cmd -> TUI model (bulk writes only)
Logger.SubscribeLines()    -> logCh (cap 200)            -> tea.Cmd -> TUI model (250ms batch; lines the backfill holds skipped)
CookieRefresh.OnAuthChange -> cookieStatusCh (cap 5)     -> tea.Cmd -> TUI model
Monitor.OnSchedule         -> checkTimersCh (cap 10)     -> tea.Cmd -> TUI model
```

When a channel is full, the send is dropped and a drop counter is incremented. On TUI exit, drop counts are logged to help diagnose missed updates. This non-blocking design prevents slow TUI rendering from back-pressuring database writes or monitor callbacks.

### Shared UI Patterns

Both the web UI and TUI implement the same user-facing features:
- **Status bar** — Shows version, uptime, cookie status (per-platform icons), disk usage (warning/critical thresholds), connection count, monitor check timers, theme toggle, and a logout icon when a password protects the UI and the session is authenticated
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

SQLite in WAL mode, single connection (`SetMaxOpenConns(1)`), 5-second busy timeout, foreign keys enabled. DSN: `file:{path}?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)`, with `%`, `?` and `#` in `{path}` percent-escaped so SQLite's URI parser takes the path literally (an install whose database an earlier release kept where the unescaped path resolved keeps using that file, with a Warn naming both paths, until it is moved into place; see `docs/spec/data-and-storage.md`) — modernc/sqlite ONLY honors `_pragma=name(value)` parameters; the mattn-style `_journal_mode=`/`_busy_timeout=`/`_foreign_keys=` forms are silently ignored (that exact mistake once disabled WAL and FK enforcement; the v14 migration cleans up its fallout).

**Schema version:** v21 at the time of writing — the authoritative number is `schemaVersion` in `internal/database/migrations.go`, mirrored in `docs/spec/appendix-metrics.md` (v5 added the `segments` table, v6 `client_tokens`, v15 per-part `segments.chat_file`, v16 the feed-history store, v17–v20 the `incomplete_tail`, `park_reason`, `park_identity` and `notification_msgs` job columns, v21 a version bump with nothing to apply; see `docs/spec/data-and-storage.md` for the full migration table). Migrations run automatically on startup via `db.migrate()`.

**Write path:** `UpdateJobFields()` is synchronous. It takes `db.mu`, executes one `UPDATE jobs SET … WHERE id=?`, re-reads the row through the prepared `stmtGetJob` in the same critical section, releases the lock, and only then notifies subscribers (so a subscriber may call back into the database without deadlocking). Because notification follows the unlock, two writers' rows can reach subscribers in the opposite order to their writes; each read-back therefore carries `Job.Version`, drawn under the lock (AddJob's read-back too), and the subscribers that keep a copy — the WebSocket hub (`broadcastJobFrame`, `internal/web/websocket.go`) and the TUI (`staleJobUpdate`, `internal/tui/app_update.go`) — drop a row older than one they already hold, except the row that first introduces a job. A progress tick carries only the progress columns and the status, so the two are held apart: a tick is dropped behind any newer write, but a whole row only behind a newer whole row — the next tick routinely overtakes a rename or a `twitch_quality` write, and dropping the write behind it lost the write on every tab and in the TUI until a resync. The hub then sends the newer tick again after the older row, and the TUI keeps the newer tick's values in its progress store, so both end on the newest value of every column. There is no batching goroutine, update channel or coalescing window; the write rate during a download is bounded upstream by `ProgressTracker`'s per-job gate (`downloader.progress_interval_ms`), not by the database, and nothing in the package runs when nothing is being written.

**`UpdateJobFields` pattern:**
```go
db.UpdateJobFields(jobID, map[string]any{
    "status":   database.StatusDownloading,
    "progress": "(V: 1234/1300 A: 1234/1300 C: 5678)",
})
```
Dynamically builds `SET` clauses from the map using `fieldToColumn` (a 51-entry whitelist over `jobs` columns; `notification_msgs` is deliberately absent — `UpdateNotificationMsgs` is its only writer). Auto-updates `updated_at`. Triggers `OnJobUpdate` and `OnJobChange` subscribers after write. Returns the updated `*Job`.

**Pub/sub:** six subscriber kinds, each registered through an `On…` method that returns an unsubscribe function. `OnJobUpdate(func(*Job))` and `OnJobChange(func(*JobChange))` both fire synchronously after every `UpdateJobFields` write (the latter also carries the list of columns written). `OnJobAdded`, `OnJobDeleted` and `OnTrimsChanged` are the lifecycle events of `AddJob`, `DeleteJob` and `AddTrim`/`DeleteTrim`. `OnJobsChange(func([]*Job))` is the full-list refresh and is dispatched only by the two bulk writers, `BatchSetWatched` and `DeleteJobsAndHistoryForChannel`, on a fresh goroutine. Multiple subscribers are supported — the WebSocket hub, TUI, and notification manager all subscribe independently. Callback invocation uses `safeCallJobUpdate`/`safeCallJobsChange` (and their siblings for the other kinds) with panic recovery so one misbehaving subscriber does not affect others. The subscriber list is protected by a separate `subMu` RWMutex to avoid contention with the main database mutex.

**Per-job log buffers:** The database maintains in-memory log buffers per job (max 200 lines, trimmed to 100 when exceeded). `RouteLogToJobs(line)` scans each log line for the job IDs currently ROUTED and appends matching lines to the corresponding buffer. `TrackJobForLogs(jobID)` registers a job ID for log routing, `UntrackJobForLogs(jobID)` deregisters it while keeping its buffer, and `SyncJobLogTracking(jobs)` applies both across a list so only non-terminal jobs are ever scanned — a job that leaves a terminal state (retry, resume, auto-retry) is registered again on the status write, so the per-line cost tracks the number of LIVE jobs rather than the size of the table. `ClearJobLogs(jobID)` removes a deleted job's buffer and routing — the channel prune (`DeleteJobsAndHistoryForChannel`) does so for each row it deletes, before it returns. These per-job logs are served via `GET /api/jobs/{id}/logs` to the dashboard's job details dialog, and read through `db.GetJobLogs` by the TUI's `O L` Job Log overlay (`internal/tui/job_log.go`).

**Per-job routing runs inside the log call:** `RouteLogToJobs` is the logger's line router (`Logger.SetLineRouter`, wired by `wireLogForwarding`), called on the goroutine that logged the line before the log call returns — not by a subscriber later — so a line logged just before a job's terminal status write is routed before that write stops the routing.

**Job table columns (59):** id, video_id, url, title, channel_name, platform, status, progress, percent, eta, speed, error, created_at, updated_at, last_video_seq, last_audio_seq, total_video_seq, total_audio_seq, is_vod, manually_added, allow_non_stream, stream_start_time, stream_end_time, length_seconds, download_started_at, thumbnail_url, description, output_file, filename, output_directory, video_width, video_height, video_fps, file_size, chat_status, total_chat_messages, chat_filename, chat_file, thumbnail_file, description_file, twitch_quality, twitch_category, channel_avatar_url, selected_video_itag, selected_audio_itag, start_time, end_time, last_recheck_at, quality_preference, watched, resume_position, chat_offset, auto_retry_count, channel_id, queue_priority, incomplete_tail, park_reason, park_identity, notification_msgs.

**Additional tables:** `history` (video IDs a job was created for — the monitors' re-add guard and the VOD archive gate), `segments` (multi-segment recordings, schema v5), `client_tokens` (persistent auth tokens, schema v6), `trims` (clip extractions from finished recordings), `feed_items` (persistent per-channel discovery store, schema v16), `channel_state` (per-channel backfill/RSS bookkeeping, schema v16).

### Config

TOML format parsed by `BurntSushi/toml`. An explicit `-config` path is authoritative — it is the only file considered, and a path that does not yet exist falls back to defaults rather than to another file. Without the flag the search order is `./config.toml`, `./config/config.toml`, `~/.config/moombox/config.toml`; if none exists, defaults are used.

**Sections:** `[network]` (port, access level, TLS, password, trusted proxies), `[paths]` (database, log, output, staging, ffmpeg), `[logs]` (level, rotation), `[monitors]` (intervals, archive window/slots, hide threshold, probe cooldown, membership discovery), `[downloader]` (template, resolution, parallelism, chat, retry), `[cookies]` (file, auto, browser profile, platforms, refresh interval), `[disk]` (warn/critical percent), `[updates]` (auto-check), `[[channels]]` (array of monitored channels), `[[notifications]]` (array of webhook configs).

**FlexDuration:** Custom type that accepts either a bare number (interpreted in the field's documented unit — minutes for `feed_check_interval`, seconds for `probe_cooldown`) or a duration string such as `"30s"` or `"7d"`, converted to that unit. Used for `feed_check_interval`, `hide_finished_age_days`, `probe_cooldown`, `interruption_timeout`, `incomplete_staging_expiry_days`, `refresh_interval`.

**Non-destructive migrations:** `migrateOldFormat()` handles backward compatibility — migrates flat fields into current sections, converts legacy flags (e.g., `allow_lan`/`allow_external` to `network_access`). Only applies when the new section does not already exist.

**Runtime hot-reload:** Log level, parallel download count, `network.trusted_proxies`, `network.trust_forwarded_proto`, `network.public_url`, `memory.go_soft_limit_mb`, the downloader reorder ceilings (`reorder_buffer_mb` / `reorder_budget_mb`), `paths.ffmpeg_path` (the trim service rebuilds its muxer), `monitors.hide_finished_age_days` (both UIs re-filter at once), the archive window and slots (read per sweep), the disk thresholds and output directory (a disk reading is taken at once), and the notification targets can be changed at runtime via the API or TUI without restart (the client-IP middleware re-reads the store per request, so `trusted_proxies` is deliberately absent from both restart-required lists). Channel changes trigger monitor re-evaluation via `kickMonitors()`, which calls `CheckNow()` on all three monitors to wake them from idle sleep. A changed monitor check interval (`feed_check_interval`, `decapi_check_interval`, `twitch_check_interval`) kicks them too — from the dashboard through `OnMonitorIntervalChange`, from the TUI on every save — because each monitor reads its interval only when it arms the next cycle, and the timer already armed would otherwise keep the old delay.

**Channel configuration:** Each `[[channels]]` entry has: `id` (YouTube channel ID or Twitch login — stored trimmed and unique, compared case-insensitively: every writer — `POST /api/config/channels`, `PUT /api/config`, `/api/setup/complete` and both TUI channel editors — runs it through `utils.NormalizeChannelID`, which resolves a channel URL or a bare `@handle` to the ID and refuses one that names no channel, and `Validate` refuses a padded or duplicate ID), `name` (display name), `platform` ("youtube" or "twitch", defaults to "youtube"), `enabled` (optional, defaults to true), `terms` (title/description match terms for filtering), `output_directory` (per-channel override), `include_non_live_content` (archive regular videos too), `archive_window_days`/`archive_slots` (per-channel overrides), `quality_preference` (quality cap for YouTube and Twitch alike: "best", "2160p60" through "160p", or "audio_only").

**Channel terms:** The `terms` field is one pattern (`terms = "(?i)karaoke|singing"`) or a table of named patterns (`terms = { stream = "karaoke", vod = "/singing.*stream/" }`); a video passes when ANY pattern matches, and an empty `terms` passes everything. Patterns match the title — on Twitch, the title or the game category — never the description: the store-driven YouTube passes match what a `feed_items` row holds, and DECAPI and the browse sources carry no description (`MatchesTerms`, `internal/monitor/utils.go`). Matching is case-insensitive and diacritic-insensitive; a pattern is an RE2 regex, optionally written `/pattern/flags`, and one that does not compile (a lookahead, for instance — RE2 has none) is matched as plain text instead. `num_desc_lookbehind` is retired: nothing reads it, and it is kept in the config only so existing files load unchanged.

### Cookies

Netscape-format cookie file (`cookies.txt`). The `CookieJar` parses it into TWO per-platform in-memory maps, not one — YouTube rows never reach a Twitch request and vice versa. Essential YouTube cookies include SAPISID (or `__Secure-3PAPISID`), SID, HSID, `__Secure-1PSID`, LOGIN_INFO; essential Twitch cookies (`essentialTwitchCookies`): `auth-token`, `twilight-user`, `login`, `name` — `login` is load-bearing, not decorative, since it is the IRC NICK above. Expiry is captured per entry and reported per platform but never filtered — the only operator-visible surface for it is the `Cookies loaded` line at startup.

**Cookie refresh service (`RefreshService`).** Always runs — on its own 30-minute timer, and on demand from either UI (`R C` / `POST /api/cookies/recheck`, plus the re-check that follows EVERY gesture which may have rewritten `cookies.txt`: both browser-refresh buttons, both setup-wizard finishes, the automatic recovery whatever its verdict, the worker's job-triggered refresh, the startup browser-profile seed, and the `auto_enabled` periodic timer) — and is never gated on any config flag. No monitor triggers it; the monitors' only channel into the service is `ObserveLiveness`, whose recovery arm has been armed since 2026-09-03. It validates YouTube by POSTing the Innertube `guide` endpoint (`youtubeGuideURL`, `internal/cookies/refresh.go`) and Twitch by GETting `id.twitch.tv/oauth2/validate`, reports changes through `OnAuthChange`, and fires `OnRecoveryNeeded` when auth is lost. **It rotates YouTube cookies only.** Google's `Set-Cookie` responses are admitted back into `cookies.txt` under `admitSetCookie`'s rules; there is no Twitch refresh anywhere in the process, and none appears to be possible in-process — reading yt-dlp's Twitch extractor and chatterino7 turned up no client that renews an `auth-token`, only ones that read it and detect its expiry, so a browser sign-in is the only thing observed to issue a new one. The Twitch side is a check with no rotation — plus a MARK. Anything that finds Twitch credentials refused where they are actually used calls `NoteTwitchAuthLoss(reason)`, which writes the Twitch triple under the same mutex, fires `OnRecoveryNeeded("twitch")` through the ordinary dedupe, and sticks against a `oauth2/validate` 200 until the credential pair's fingerprint changes (`CookieJar.TwitchIdentity`). Today's callers are the IRC chat handshake's four downgrade routes and `StreamProcessor.noteAnonymousPlayback` (`internal/worker/stream_processor_twitch.go`), which marks on `Service.GetHLSMasterPlaylist`'s verdict — whether the playback access token was issued to a signed-in session — through the same seam; it is the one detector a job with chat capture off still gets. A credential change also fires `OnCredentialsChanged("twitch")`, which tells every live chat session to re-read its credentials and reconnect in place rather than waiting for the next job; `OnAuthRecovered("twitch")` does the same on its own edge, for a transient refusal that heals with the fingerprint unchanged and so never reaches `OnCredentialsChanged` at all.

**Auto-cookie service (`AutoCookieService`).** Acquires credentials four ways: an interactive browser login, a headless browser refresh (Firefox reads `cookies.sqlite` **together with its `-wal` sidecar**; Chromium is driven over CDP, with an opt-in Windows DPAPI read of the user's real profile as a fallback, off by default), browser-free by importing a mounted browser profile, or from an operator-supplied Netscape file posted to `POST /api/cookies/import`. It manages a dedicated profile directory and refuses one that points inside a real installed browser's profile tree — for LAUNCHING. `cookies.acquisition` (`auto` | `profile`) selects how a refresh acquires credentials; `"profile"` never launches, reads the configured profile directory read-only, and is the explicit opt-in that lets that read proceed against a real browser's profile. The launch guard itself is never lifted, in any mode.

**What `cookies.auto_enabled` does.** It owns the headless-browser refresh TIMER, the one automatic browser recovery attempt, and the `SetExpectedPlatforms` seeding — and nothing else. It is not a master switch: `RefreshService` never consults it, `R C` / `POST /api/cookies/recheck` never consults it, and `R F` / the dashboard header's shift+click / the Settings page's "Refresh cookies from browser profile" button only let it decide WHICH rung of the refresh ladder runs — the flag never causes a nil-error decline, though a pass with the flag off AND no browser profile directory still fails with `ErrNoBrowserFound`. Flipping it off at runtime does not stop the already-running timer, which is why both UIs label it restart-required.

**What `cookies.acquisition` does.** It picks the PATH a refresh takes; `auto_enabled` picks whether a browser may run at all. The two compose and neither replaces the other. `"auto"` is the default and is the behaviour that shipped before the setting existed: a resolvable browser launches, a host with none imports. `"profile"` forces the browser-free import even on a desktop with a browser installed, which is the only route to reading a real signed-in profile on Windows; under it the flag's timer and its one automatic recovery attempt import instead of launching, and the timer's import stays behind `automaticImportGuard` like every automatic import. Two values by ruling — the audit's `"browser"` behaved exactly like `"auto"` and was dropped. It is read live (`AutoCookieService.AcquisitionMode`) and is not restart-required. `StartSetup` never consults it — the interactive login is acquisition, and gating it would leave a fresh install in `"profile"` mode unable to create the profile it is told to read.

**Docker works, with one manual step.** Leave `auto_enabled` off (the image ships no browser), mount a Firefox profile, and press `R F` / shift+click / the Settings button after each host-side profile refresh. The very first import is automatic — but only when there is no `cookies.txt` to lose. When the profile itself goes stale, `POST /api/cookies/import` takes a pasted or uploaded Netscape file from any authenticated client, merges it into `cookies.txt`, reloads the jar and answers with a live verdict — no browser, no shell, no volume access. It has no GET, deliberately and permanently.

**The two-tier cookie liveness pilot is ON.** `livenessRecoveryArmed` is `true` (armed 2026-09-03 by owner ruling), so a tier-2 verdict that is signed out and past the per-platform back-off logs a Warn and triggers recovery through `OnRecoveryNeeded`, exactly as a tier-1 loss does. The back-off re-alarms at 30 minutes, doubles per alarm to a 24-hour cap, and resets on a conclusive signed-in verdict; a tier-1 fire's dedupe stamp stops tier 2 firing twice for one loss. It is a source constant — the way back is a rebuild.

**Deep-dive:** [docs/spec/data-and-storage.md](docs/spec/data-and-storage.md) § Cookies for the jar, the refresh service and every acquisition path; [docs/spec/operations.md](docs/spec/operations.md) § Browser Cookie Acquisition for the platform differences (the reap — a Job Object on Windows, a process group on Linux, nothing on darwin — `AbandonSetup`, the drain) and [docs/spec/operations.md](docs/spec/operations.md) § Credential Notifications for what an operator is actually told.

### File Output

**Staging directory:** Active downloads write segments to `staging/{jobID}/`. Contains video segments, audio segments, chat JSON, and resume state files. At each start the worker deletes only the leftovers that are provably redundant — a recovered set-aside recording whose sibling is on disk, and a `Finished` job's staging when its row records a download of its own (never an import's), its archive is on disk, no recovered set-aside recording it could not verify is left in it, and the post-mux cleanup's own rules would have deleted it — logging each with its path and reason; everything else waits for the orphan sweep's confirm-before-delete (`reclaimBootLeftovers`, `internal/worker/boot_leftovers.go`).

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
2. **IPGateMiddleware** — Enforces `network_access` level (localhost/lan/external/public); ahead of CSRF so a refused peer cannot fill the log
3. **HostGateMiddleware** — On localhost/lan, refuses a `Host` the origin policy would not admit (the DNS-rebinding read path)
4. **CORSMiddleware** — Validates Origin based on `network_access` config
5. **SecurityHeaders** — X-Frame-Options: DENY, X-Content-Type-Options: nosniff, Referrer-Policy: no-referrer, Permissions-Policy, CSP
6. **CSRFMiddleware** — Origin/Referer validation on mutating requests
7. **MaxBodySize** — Default 1MB body limit (import endpoint overrides to 500MB)
8. **CompressionMiddleware** — Gzip response compression
9. **AuthMiddleware** — Session/token validation for external connections (registered separately on the router)

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
- `lan` — Loopback + private IP ranges (10.x, 172.16-31.x, 192.168.x, fc00::/7, link-local), plus — on this mode only — Tailscale's 100.64.0.0/10
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
script-src 'self' https://cdn.jsdelivr.net;
style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net;
font-src 'self' https://cdn.jsdelivr.net;
img-src 'self' data: https://i.ytimg.com https://yt3.ggpht.com https://*.jtvnw.net https://*.ttvnw.net https://cdn.betterttv.net https://cdn.7tv.app https://cdn.frankerfacez.com https://cdn.jsdelivr.net https://fonts.gstatic.com;
connect-src 'self' ws: wss: https://cdn.jsdelivr.net data:;
frame-src https://www.youtube-nocookie.com https://player.twitch.tv;
object-src 'none';
base-uri 'self';
form-action 'self'
```

### TLS

Optional. When enabled (`https_enabled = true`), uses configured cert/key paths. If paths are not provided, auto-generates a self-signed certificate.

### Update Signing

Binary releases are signed with Ed25519. The public key is embedded in the binary at compile time. The private key is stored as a GitHub Actions secret and never leaves CI. During update: download the release's signed manifest (`moombox-manifest.json`: version, tag, and each platform's asset name and SHA-256) and verify its signature; refuse unless it names the release being applied, a version newer than the running one, and this platform; then download the new binary and its `.sig` file, verify the Ed25519 signature of the binary against the embedded public key, and its SHA-256 against the manifest. A per-binary signature alone would accept a validly signed older binary or another platform's. If verification fails, the update is rejected and the downloaded file is deleted; a release with no manifest (or an unsigned one) is refused for auto-update. From `FirstManifestVersion` (2.8.11) on, every release publishes the signed manifest, so that refusal is a failure with no manual install advised (a binary installed by hand would fail Verify Signature for the same missing assets); only a release before it must be installed manually. The three-step binary swap: write new binary as `{exe}.new`, rename current `{exe}` to `{exe}.old`, rename `{exe}.new` to `{exe}`. The `.old` file is cleaned up on next startup by `CleanupOldBinary()`. This three-step approach is atomic on Windows (rename is atomic within the same volume) and allows rollback if something goes wrong.

The `cmd/sign/main.go` tool is used in CI to sign the binaries and, with `-manifest`, to write and sign the release manifest. It reads the Ed25519 private key from an environment variable, signs the binary, writes the `.sig` file, and verifies it with `VerifySignature` against the embedded public key — a `SIGNING_KEY` that is not that key's private half fails the release instead of publishing signatures every install would reject. The `POST /api/update/verify` endpoint allows users to verify the signature of the currently running binary at any time.

**Deep-dive:** [docs/spec/security.md](docs/spec/security.md)

---

## 8. Operations

### Build

```bash
go build -o moombox.exe ./cmd/moombox    # Build binary
go test ./...                              # Run all tests
go vet ./...                               # Static analysis
```

Go 1.27 required (go.mod carries `toolchain go1.27.1` as the floor: an older local Go and CI auto-download it; the Docker image pins its own 1.27 patch). Runtime requires FFmpeg on PATH. Windows resource embedding (exe icon, version info) via `go-winres`: `go install github.com/tc-hib/go-winres@latest && cd cmd/moombox && go-winres make`. This generates `.syso` files in `cmd/moombox/winres/` — CI generates these at build time, none are committed to the repo.

### CI/CD

GitHub Actions (`.github/workflows/release.yml`) triggers on tag push, and runs the test workflow on the tagged commit before anything is published (a manual run is a dry run that publishes nothing). Builds Windows exe, generates `.syso` for icon/version, signs with Ed25519 (private key in GitHub secret), writes and signs the release manifest, uploads binaries + signatures + manifest to GitHub Release. Release body is read from `RELEASE_NOTES.md` in the repo.

### Release Process

1. Generate `RELEASE_NOTES.md` — `git log --oneline <prev-tag>..HEAD`, group by Features/Improvements/Bug Fixes/Internal (skip empty sections). No heading.
2. Bump `version` in `cmd/moombox/main.go` (line: `version = "x.y.z"`).
3. Commit both: `chore: bump version to x.y.z — short summary`.
4. Tag: `git tag vx.y.z`.
5. Push: `git push && git push origin vx.y.z`.

### Self-Update Flow

1. **Check** — Query GitHub Releases API for latest release. Compare semver against `version` constant. Skip if current is equal or newer.
2. **Download** — Fetch the signed release manifest, then the platform's binary asset (`Moombox.exe` / `moombox-linux-*`) and its `.sig` signature asset.
3. **Verify** — Ed25519 signature verification of the manifest and the binary against the embedded public key, and the binary's SHA-256 against the manifest's entry for this platform. Reject if any check fails, or if the release has no manifest.
4. **Swap** — Three-step rename: write downloaded binary as `{exe}.new`, rename current `{exe}` to `{exe}.old`, rename `{exe}.new` to `{exe}`.
5. **Restart** — `triggerRestart("update")` exits with code 42. Launcher respawns, loading the new binary.
6. **Cleanup** — On next startup, `CleanupOldBinary()` deletes `{exe}.old`.

### Launcher/Supervisor

The launcher (parent process, without `_MOOMBOX_CHILD`) spawns the application as a child with `_MOOMBOX_CHILD=1`. It watches the child's exit code:
- Exit code 42 — Restart requested. Launcher respawns immediately, picking up any new binary.
- Any other exit — Launcher exits with the same code.

The respawned child shares the launcher's console; the `CREATE_NO_WINDOW` flag (0x08000000) is used only for the detached cleanup spawn that deletes the superseded launcher binary. Restart triggers: config change requiring restart, update applied, setup wizard completion, `POST /api/restart`.

### Shutdown Sequence

When the main context is cancelled (Ctrl+C, SIGTERM, or restart trigger):

1. 15-second force-exit timer starts (it must outlast the worker's 12-second stop budget — see `forceExitAfter` in `cmd/moombox/shutdown.go`)
2. Notifications switch to single-attempt delivery
3. Stop TwitchMonitor, DecapiMonitor, FeedMonitor, then the TrimService (cancels the trims it runs — a finished job's post-download trim among them — and waits up to 2 s for each to remove its partial file)
4. Stop DownloadWorker (waits up to 10 s for active downloads to save resume state, then cancels in-flight muxes)
5. Flush pending notifications
6. Stop CookieRefresh and AutoCookieService
7. Cleanup PotProvider (evict VMs)
8. Stop the BotGuard sidecar (when running)
9. Stop WebServer (graceful HTTP shutdown)
10. Unsubscribe log and DB event forwarders
11. Close Database (flush WAL)
12. Return restart flag to `main()`

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

Discord webhooks with queued dispatch. The `notifications.Manager` validates webhook URLs, formats Discord embeds with color-coded types (Info=blue, Success=green, Warning=yellow, Error=red, Download=teal, Muxing=purple, Cancelled=orange), and hands each one to a per-target FIFO queue drained by a single goroutine. Supports event-based filtering per webhook target — see the event list below and the table in `docs/spec/operations.md`.

**Deep-dive:** [docs/spec/operations.md](docs/spec/operations.md)

---

### Notifications Detail

The notification system supports Discord webhooks with event-based filtering. Each `[[notifications]]` config entry has a webhook URL and an optional event filter list. If no filter is specified, all events are sent. An entry also carries an `enabled` mute (absent means enabled; `false` keeps the target and its filter but delivers nothing) and an optional `mention` — a role, user, `@everyone` or `@here` ping — whose `mention_events` list picks which events carry it: absent means the default six (`error`, `auth`, `disk_critical`, `update_failed`, `crash_recovered`, `sidecar_down`), an explicit `[]` means never.

Two install-wide behaviours sit beside the per-entry keys. `network.public_url`, when set, makes a job embed's title a deep link to that job on the dashboard (`{public_url}/#job=<id>`) and moves the platform page to the embed's author line. It is not for embeds alone: the web server also trusts it as the dashboard's own address — on `localhost`/`lan` an Origin may name its port, and on `external`/`public` a local browser may address the dashboard by its host (`originPortServed`, `externalHostRefused`, `internal/web/middleware.go`). And the three high-volume families — `found`, `added`, and the per-job `auth` alert — coalesce into one message per target per 5-second window (up to ten embeds each), so a backfill re-scan is a handful of messages rather than one per catalogue row; everything else, alerts included, is delivered immediately.

**Notification events:** `found`, `added`, `scheduled`, `rescheduled`, `downloading`, `quality_split`, `gap_split`, `muxing`, `finished`, `error`, `cancelled`, `auth`, `auth_recovered`, `connectivity_resume`, `connectivity_split`, `connectivity_restored`, `trim_created`, `trim_deleted`, `trim_error`, `disk_warning`, `disk_critical`, `update_available`, `update_applied`, `update_failed`, `crash_recovered`, `channel_unhealthy`, `channel_healthy`, `sidecar_down`, `sidecar_restored`, `disk_ok` — see `docs/spec/operations.md` for the per-event table; new events must be registered in both UI filter registries. `connectivity_pause` is retired and is kept working as a legacy filter entry by an event alias. The alerts with a close (disk, sidecar, channel health, platform auth) persist which of them are open in `open-alerts.json` beside the database (`cmd/moombox/open_alerts.go`), loaded before any alerter starts, so an alert open across a restart still gets its close from the first healthy observation after it.

**Discord embed format:** Title, description, colored sidebar (type-specific), fields (inline key-value pairs), optional URL link, an author line (channel name + avatar + channel page), an optional thumbnail and full-width image, and a footer (`Moombox · {platform} · {job id}`). When a target is configured for one, the MESSAGE — not the embed — also carries a mention in `content` with a matching `allowed_mentions`; embeds never mention on their own. Every string is clamped to Discord's limits on a rune boundary before it is sent.

Dispatch is queued, not fire-and-forget: `Manager.Send()` returns immediately after appending to each matching target's bounded FIFO, and one goroutine per target delivers in order. `BeginShutdown()` switches to single-attempt delivery and `Wait()` drains the queues, both called during shutdown; the process's 15-second force-exit is what actually bounds the drain.

---

## Appendix

For volatile numbers that change frequently (line counts per package, current version, schema version, dependency versions), see [docs/spec/appendix-metrics.md](docs/spec/appendix-metrics.md). These are maintained separately so SPEC.md does not need updates for every line count change.
