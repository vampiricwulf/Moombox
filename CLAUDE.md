# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
npm install              # Install dependencies
npm run build            # Compile TypeScript (tsc → dist/)
npm run start            # Start with TUI + web dashboard (requires build)
npm run dev              # Run directly from TypeScript via tsx (development)
npm run test             # Run all tests once (vitest)
npm run test:watch       # Run tests in watch mode
npm run test:coverage    # Run tests with V8 coverage
node dist/index.js add <video_id_or_url>  # Manually add video to queue
```

**Flags:**
- `--no-tui` - Disable TUI, use web dashboard only

## Building Executable

```bash
npm run package          # Build self-extracting Moombox.exe (Windows only)
```

8-step SEA build process (`scripts/build-sfx.cjs`):
1. Downloads Node.js 20.18.0 (cached in `sea-build/node/`)
2. Bundles TypeScript with esbuild → CJS (`sea-build/moombox-app.cjs`)
3. Embeds static web assets + sql-wasm.wasm in `globalThis.__MOOMBOX_ASSETS__`
4. gzip-compresses bundle into self-extracting `launcher.cjs`
5. Generates SEA blob with V8 code cache
6. Injects blob into Node.js binary via `postject`
7. Result: `Moombox.exe` (~72MB) that extracts `assets/moombox-app.cjs` on first run

**esbuild plugins:**
- `yoga-shim` — Proxy shim (`scripts/yoga-shim.js`) defers yoga-layout Wasm init until `startTUI()` calls `loadYoga()`, avoiding top-level `await` in CJS
- `ink-reconciler-fix` — Strips `await import('./devtools.js')` (dead code in production)
- `xhr-worker-plugin` — Disables jsdom's sync XHR worker (not needed)
- `import-meta-shim.js` — Shims `import.meta.url` → `__filename` for CJS context

## Ports

- **Dashboard:** http://localhost:774
- **POT Provider:** http://localhost:774 (same server, `/get_pot` endpoint)

## Project Stack

| Layer | Technology | Notes |
|-------|-----------|-------|
| Runtime | Node.js ≥20, ESM (`"type": "module"`) | TypeScript ESNext/NodeNext, `.js` import extensions |
| Backend | Express 5, ws, lowdb 7 | Async routes via `asyncHandler()` wrapper |
| Frontend | Vanilla JS ES modules, Shoelace v2.16 | No build step, loaded from CDN |
| TUI | Ink 6 (React 19 for CLI) | Yoga layout, mouse support via VT sequences |
| Testing | Vitest 4, supertest | V8 coverage, excludes `bgutils/`, `cipher/`, `ejs/` |
| Build | tsc (dev), esbuild (SEA) | SEA = Node.js Single Executable Application |

## Architecture Overview

Moombox is a YouTube live stream archiver with a web dashboard + TUI. It monitors channels via RSS feeds, detects livestreams, and downloads them using native JavaScript implementations of YouTube's signature decryption and BotGuard (PO Token).

### Data Flow

```
RSS Feed Monitor → Job Database (lowdb) → Download Worker → YouTube Innertube API
                                                 ↓
                       Web Dashboard ← FFmpeg Muxer ← Segment Downloads (DASH/HLS/VOD)
                      (localhost:774)      ↑
                                     Chat Downloader (live chat polling)
```

### Initialization Order (src/index.ts)

Services must start in this exact order due to dependencies:

```
1. ConfigManager.load()        → TOML config (everything depends on this)
2. Logger.init()               → reads config for log level/path
3. Database.getInstance()      → reads moombox.json, builds in-memory indexes
4. FeedMonitor.start()         → RSS polling (depends on DB + config)
5. DownloadWorker.start()      → job queue polling (depends on DB + YouTube)
6. CookieRefreshService.start()→ 30-min cookie refresh cycle
7. AutoCookieService           → singleton created, no auto-start
8. WebServer.start()           → HTTP + WebSocket (subscribes to Logger + DB pub/sub)
9. TUI (if interactive)        → Ink render tree
```

**Circular dependency:** `ConfigManager.log()` uses try-catch fallback — tries Logger, falls back to console if Logger not yet initialized.

### Shutdown Order (src/index.ts)

Consumers stop first, infrastructure last. 10-second force-exit timer as safety net.

```
FeedMonitor → DownloadWorker → CookieRefreshService → AutoCookieService → PotProvider → WebServer → Logger.flush()
```

`DownloadOrchestrator.stop()` propagates to all active `SegmentDownloader` and `ChatDownloader` instances via `AbortController.abort()`.

### Key Directories

```
src/
├── core/                    # Application services (singletons)
│   ├── config.ts            # ConfigManager — TOML loader, validation, defaults
│   ├── logger.ts            # Logger — file rotation, pub/sub
│   ├── database.ts          # Database — lowdb, batch updates, pub/sub
│   ├── cookies.ts           # CookieJar — Netscape parser, SAPISIDHASH generation
│   ├── cookieRefresh.ts     # CookieRefreshService — 30-min refresh cycle
│   ├── autoCookies.ts       # AutoCookieService — browser-based cookie extraction
│   ├── browserDetect.ts     # Browser detection (Firefox > Edge > Chrome)
│   ├── cdpClient.ts         # Chrome DevTools Protocol WebSocket client
│   ├── globalDom.ts         # JSDOM setup for BotGuard (sets globalThis.window/document)
│   ├── http.ts              # fetchWithTimeout + createRetryFetch (p-retry wrapper)
│   ├── monitor.ts           # FeedMonitor — RSS polling, regex matching
│   ├── potProvider.ts       # PotProvider — BotGuard/PO Token with minter cache
│   ├── notifications.ts     # NotificationManager — webhook dispatch
│   └── worker/              # Download orchestration
│       ├── index.ts              # DownloadWorker singleton
│       ├── jobQueue.ts           # p-queue backed scheduler with priority
│       ├── streamProcessor.ts    # Status probing, live-wait loop, early chat
│       ├── downloadOrchestrator.ts # Strategy selection, cancellation, staging
│       ├── downloadStrategies.ts # downloadVod(), downloadDash(), downloadHls()
│       ├── muxFinalize.ts        # FFmpeg mux, ffprobe metadata, file copy
│       ├── progressTracking.ts   # Segment/chat progress events → DB updates
│       ├── formatUtils.ts        # Format selection utilities
│       ├── timeUtils.ts          # Time parsing for timestamp selection
│       ├── trimService.ts        # Post-download trim via FFmpeg
│       └── assetDownloader.ts    # Thumbnail + description saving
├── engine/                  # Download engine (no singleton dependencies)
│   ├── downloader.ts        # SegmentDownloader — DASH/HLS segment loop, parallel catch-up
│   ├── manifest.ts          # ManifestParser — DASH XML + HLS M3U8 parsing
│   ├── muxer.ts             # Muxer — FFmpeg wrapper via execa
│   ├── youtube/             # YouTube service modules
│   │   ├── index.ts              # YouTubeService facade (singleton)
│   │   ├── playerApi.ts          # Innertube /youtubei/v1/player multi-client strategy
│   │   ├── auth.ts               # YouTubeAuth — cookie loading, SAPISIDHASH headers
│   │   ├── watchPage.ts          # WatchPageParser — HTML scraping for ytcfg/playerResponse
│   │   ├── formatSelector.ts     # FormatSelector — codec priority, resolution filtering
│   │   └── poToken.ts            # PoTokenGenerator — BotGuard challenge → PO token
│   └── chat/                # Live chat system
│       ├── chatApi.ts            # ChatApi — Innertube live_chat/replay endpoints
│       └── chatDownloader.ts     # ChatDownloader — polling loop, memory bounding, resume
├── web/                     # Express server + dashboard
│   ├── server.ts            # WebServer — middleware, WebSocket, static serving
│   ├── routes/              # RESTful API endpoints
│   │   ├── jobRoutes.ts          # Job CRUD, formats, video streaming, trims
│   │   ├── configRoutes.ts       # Config, status, cookies, yt-dlp plugin
│   │   ├── importRoutes.ts       # ZIP archive import (zip bomb protection)
│   │   ├── trimRoutes.ts         # Video trimming (FFmpeg)
│   │   ├── potRoutes.ts          # POT provider (yt-dlp compatible, root-mounted)
│   │   ├── errorHandler.ts       # asyncHandler() + errorMiddleware
│   │   ├── rateLimiter.ts        # In-memory IP-based rate limiter
│   │   ├── validators.ts         # Pagination validation
│   │   └── index.ts              # Barrel exports
│   └── public/              # Static frontend (no build step)
│       ├── index.html            # Shoelace dark theme, sl-tab-group layout
│       ├── moombox.css           # Dark theme, CSS grid job table, nico overlay
│       ├── app.js                # MoomboxApp — WebSocket, job rendering, API calls
│       └── modules/
│           ├── player.js         # PlayerController — video + nico chat + sidebar
│           ├── settings.js       # SettingsController — config, channels, notifications
│           ├── imports.js        # ImportController — ZIP drag-and-drop upload
│           └── setup.js          # SetupController — 5-step first-run wizard
├── tui/                     # Terminal UI (Ink/React)
│   ├── index.tsx            # startTUI() — yoga init, mouse tracking, VT input
│   ├── stdinFilter.ts       # Mouse sequence interceptor (strips VT from Ink input)
│   ├── clipboard.ts         # Platform-aware clipboard (powershell/pbpaste/xclip)
│   ├── textWidth.ts         # CJK-aware string width/truncation/wrapping
│   ├── App.tsx              # Root component — 3-panel layout, keyboard/mouse routing
│   ├── components/
│   │   ├── TaskList.tsx          # Virtual list with archived divider, status icons
│   │   ├── TaskItem.tsx          # Single job row (React.memo, CJK-aware truncation)
│   │   ├── JobDetails.tsx        # Detail panel with scrollable field rows
│   │   ├── LogViewer.tsx         # Log panel with level coloring, auto-scroll
│   │   ├── AddVideoDialog.tsx    # 5-step wizard (URL → formats → timestamps → confirm)
│   │   ├── TrimDialog.tsx        # Create/delete trim dialog
│   │   ├── SetupWizard.tsx       # First-run config wizard
│   │   ├── StatusBar.tsx         # Bottom bar with shortcuts + cookie status
│   │   ├── HelpOverlay.tsx       # Keyboard help screen
│   │   └── ErrorBoundary.tsx     # React error boundary
│   └── hooks/
│       └── useMouse.ts           # SGR mouse event parser hook
├── types/                   # TypeScript interfaces
│   ├── jobs.ts              # Job, JobStatus, TrimRecord, NewJobData, DownloadProgress
│   ├── youtube.ts           # VideoInfo, Format, StreamStatus, DashStream, HlsPlaylist
│   ├── config.ts            # MoomboxConfig, ChannelConfig, DownloaderConfig
│   ├── chat.ts              # ChatMessage, ChatData, ChatStatus, SuperchatInfo
│   ├── errors.ts            # MoomboxError hierarchy (YouTube/Download/Network/Config/Muxing/Auth)
│   ├── schemas.ts           # Zod validation schemas for all API endpoints
│   └── sql.js.d.ts          # Type declarations for sql.js (dynamic import)
├── utils/                   # Shared utilities
│   ├── youtube.ts           # extractVideoId() — URL/ID parser
│   ├── ffprobe.ts           # extractVideoMetadata() via ffprobe
│   ├── ipValidation.ts      # isPrivateIP(), isLoopback()
│   ├── sanitize.ts          # sanitizeForFilename(), sanitizeForTemplate()
│   ├── textNormalization.ts # normalizeText(), fuzzyMatch() — diacritic-insensitive
│   ├── timeFormat.ts        # formatDuration(), formatTime(), formatBytes(), formatSpeed()
│   ├── SmoothValue.ts       # EMA smoother for download speed/ETA (alpha=0.7)
│   ├── PromiseQueue.ts      # Async operation serializer (currently unused — see Known Issues)
│   └── Singleton.ts         # Generic singleton base class with clearInstance() for testing
├── bgutils/                 # BotGuard/PO Token (native JS port, JSDOM-based)
│   ├── core/
│   │   ├── challengeFetcher.ts   # Fetch + descramble BotGuard challenge from Google API
│   │   ├── botGuardClient.ts     # Run BotGuard VM (new Function(interpreterJS))
│   │   ├── webPoClient.ts        # Exchange snapshot for integrity token → mint PO token
│   │   └── webPoMinter.ts        # Mint final PO token from integrity token
│   └── utils/                    # BGError, DeferredPromise, base64 utilities
├── cipher/                  # YouTube signature decryption (yt-cipher port)
│   └── src/
│       ├── handlers/             # decryptSignature(), getSts(), resolveUrl()
│       ├── solver.ts             # 3-tier cache: disk → preprocessedCache → solverCache
│       ├── playerCache.ts        # ~/.cache/yt-cipher/player_cache/ (14-day eviction)
│       ├── preprocessedCache.ts  # LRU in-memory (150 entries)
│       ├── solverCache.ts        # LRU in-memory (50 entries)
│       └── workerPool.ts         # Inline preprocessPlayer() (was worker pool)
├── ejs/                     # JS-in-JS solver for YouTube player functions
│   └── yt/solver/
│       ├── solvers.ts            # preprocessPlayer() — AST extract + regenerate
│       ├── n.ts                  # N-parameter function extractor
│       ├── sig.ts                # Signature cipher function extractor
│       ├── setup.ts              # Browser stub AST nodes (window, document, navigator)
│       └── lib.ts                # meriyah + astring re-exports
└── constants.ts             # User-Agents, API URLs, download/worker/feed constants
```

### Singleton Services

All major services use `getInstance()` pattern. Initialization order matters (see above).

| Service | File | Key Responsibility |
|---------|------|--------------------|
| `ConfigManager` | `core/config.ts` | TOML loader, defaults, validation, `resolveTemplate()` |
| `Logger` | `core/logger.ts` | File rotation, 200-entry pub/sub |
| `Database` | `core/database.ts` | lowdb JSON, O(1) indexes (`jobsMap`, `historySet`), batch writes (100ms), pub/sub |
| `YouTubeService` | `engine/youtube/index.ts` | Facade: auth, player API, format selection, PO token |
| `DownloadWorker` | `core/worker/index.ts` | Composes JobQueue + StreamProcessor + DownloadOrchestrator |
| `WebServer` | `web/server.ts` | Express 5 + WebSocket, CORS, CSP, IP gate, static serving |
| `PotProvider` | `core/potProvider.ts` | BotGuard minter cache (TTL from API), session cache (6hr), inflight dedup |
| `AutoCookieService` | `core/autoCookies.ts` | Browser launch (Firefox/Chromium), CDP/SQLite cookie extraction |
| `CookieRefreshService` | `core/cookieRefresh.ts` | 30-min cycle: fetch youtube.com, parse Set-Cookie headers |
| `NotificationManager` | `core/notifications.ts` | Webhook dispatch for job events |

### Database Pub/Sub

The Database class provides event subscriptions for real-time UI updates:
- `db.onJobUpdate(callback)` → fires after batch flush (100ms window); carries single updated `Job`
- `db.onJobsChange(callback)` → fires on add/delete; carries full `Job[]` array

Consumers: WebServer (WebSocket broadcast), DownloadOrchestrator (cancel detection), TUI App.

### Concurrency Architecture

| Package | Where Used | Configuration |
|---------|-----------|---------------|
| `p-queue` | `jobQueue.ts` | `concurrency` = `num_parallel_downloads` (default 2), rate: 10 starts/sec |
| `p-limit` | `downloader.ts` catch-up | `pLimit(6)` — max 6 parallel segment fetches |
| `p-limit` | `downloadStrategies.ts` VOD | `pLimit(2)` — parallel video + audio download |
| `p-limit` | `downloadStrategies.ts` DASH | `pLimit(10)` — parallel signature decryptions |
| `p-retry` | `core/http.ts` | 3 retries, exponential backoff on 5xx/429 |
| `AbortController` | `downloadOrchestrator.ts` | Per-job cancellation, propagates to all downloaders |

### Job Processing Pipeline

```
Job pulled from DB
  → StreamProcessor.process(job)
      → probe via ANDROID_VR (no cookies)
      → if Upcoming: wait polling loop (30s + jitter)
      → optionally starts ChatDownloader early
  → DownloadOrchestrator.execute(job, videoInfo, isVod, chatDl?)
      → strategy selection: VOD / DASH / HLS
      → SegmentDownloader(s) for video + audio
      → ChatDownloader for live chat
      → progressTracking wires events → DB updates
      → muxFinalize: FFmpeg mux + ffprobe metadata + file copy
      → optional TrimService for timestamp ranges
```

### Job Status Flow

`Upcoming` → `Live` → `Downloading` → `Muxing` → `Finished`

Special states: `Error`, `Cancelled`, `COOKIES?` (member content needs cookie refresh)

### YouTube Multi-Client Strategy (playerApi.ts)

Authenticated path fetches formats from multiple clients, deduplicates by itag (prefers lowest authLevel):

| Priority | Client | Purpose |
|----------|--------|---------|
| 0 | ANDROID_VR | No cookies/cipher/PO token needed |
| 1 | WATCH_PAGE | Embedded in HTML response |
| 2 | TV_PUBLIC (TV_DOWNGRADED) | No cookies |
| 3 | TV_AUTH (TV_DOWNGRADED) | With cookies |
| 4 | WEB | Provides DASH manifest URLs |
| 5 | WEB_CREATOR | Fallback for members-only |

### Signature Decryption Flow

```
YouTube player JS URL → cipher/playerCache (disk, 14-day TTL)
  → ejs/preprocessPlayer() (meriyah parse → AST extract n/sig functions → astring regenerate)
    → cipher/solverCache (LRU in-memory, 50 entries)
      → decryptSignature({ n_param?, encrypted_signature? })
```

### PO Token Flow

```
PotProvider.generatePoToken(contentBinding)
  → BG.Challenge.create() → Google API → descramble challenge
    → new Function(interpreterJS)() → BotGuard VM in globalThis
      → bgClient.snapshot() → webPoSignalOutput[0] = minter factory
        → POST /GenerateIT → integrity token
          → WebPoMinter.mint(contentBinding) → base64url PO token
```

## Web Server Details

### Middleware Stack (order matters)

1. CORS (validates Origin against network_access config)
2. IP Gate (rejects disallowed remote addresses)
3. Compression (gzip level 6, threshold 1024 bytes)
4. `express.json()` body parser
5. Security headers (X-Frame-Options, CSP, COEP, etc.)
6. Static file serving (SEA: in-memory `__MOOMBOX_ASSETS__`; dev: filesystem)
7. CSRF protection on mutating routes (validates Origin/Referer)

### API Routes (mounted at `/api/v1` and `/api`)

| Method | Path | Rate Limit | Notes |
|--------|------|-----------|-------|
| GET | `/jobs` | - | Supports `offset`/`limit` pagination |
| GET | `/jobs/archived` | - | Finished + older than threshold |
| GET | `/jobs/:id` | - | Finished jobs get immutable cache headers |
| GET | `/jobs/:id/video` | - | Range-request-aware streaming, path traversal guard |
| GET | `/jobs/:id/chat` | - | Full chat JSON |
| GET | `/jobs/:id/logs` | - | Per-job log lines |
| GET | `/jobs/:id/trims` | - | List trims |
| GET | `/formats/:videoId` | - | Video/audio formats for Advanced Options |
| POST | `/jobs` | 20/min | Zod-validated, duplicate check |
| POST | `/jobs/:id/cancel` | - | Active statuses only |
| POST | `/jobs/:id/retry` | - | Error/Cancelled/COOKIES? only |
| POST | `/jobs/:id/open-folder` | - | Loopback only |
| POST | `/jobs/:id/trims` | - | Creates FFmpeg trim, abort on client disconnect |
| DELETE | `/jobs/:id` | - | Terminal statuses only |
| DELETE | `/jobs/:id/trims/:trimId` | - | Delete trim file + record |
| POST | `/import` | 5/min | Raw body, zip bomb protection |
| GET | `/config` | - | Full config |
| PUT | `/config` | - | Allowlisted keys only |
| POST | `/config/channels` | - | Add/update channel |
| DELETE | `/config/channels/:id` | - | Remove channel |
| GET | `/status` | - | Uptime + cookie status |
| GET | `/logs` | - | Last 200 log entries |
| GET/POST | `/setup/*` | - | First-run wizard |
| GET/POST | `/cookies/*` | - | Auto-cookie management |
| GET/POST | `/ytdlp-plugin/*` | - | yt-dlp plugin install |

### POT Provider Endpoints (root-mounted, not under /api)

| Method | Path | Auth | Rate Limit |
|--------|------|------|-----------|
| POST | `/get_pot` | Loopback | 10/min |
| POST | `/invalidate_caches` | Loopback | - |
| POST | `/invalidate_it` | Loopback | - |
| GET | `/ping` | None | - |
| GET | `/minter_cache` | None | - |

### WebSocket Protocol

| Message | Direction | Trigger | Payload |
|---------|-----------|---------|---------|
| `initial_state` | → client | On connect | `{ jobs: Job[], logs: string[] }` |
| `jobs_update` | → client | Job added/deleted | Full `Job[]` array |
| `job_update` | → client | Progress change | Single `Job` (throttled: 10/sec per job) |
| `log` | → client | New log line | Log string |
| `ping` | client → | Heartbeat | - |
| `pong` | → client | Response | - |

Throttling: trailing-edge at 100ms per job. On client disconnect, per-client timers are cleaned up.

## TUI Architecture

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

### Backend Connectivity Pattern

TUI components access backend two ways:
- **Direct singleton access** (most operations): `Database.getInstance()` for pub/sub and CRUD, `ConfigManager` for config, `Logger` for log stream. This provides real-time updates without HTTP overhead.
- **Via REST API** (format fetching, trim operations): `fetch('/api/formats/...')`, `fetch('/api/jobs/:id/trims')`. These use route handlers where backend orchestration logic lives.

### Mouse Support

1. VT mouse tracking escape sequences written to stdout on start
2. `stdinFilter.ts` monkeypatches `process.stdin.push/emit` to intercept mouse sequences before Ink sees them
3. Stripped sequences emitted to `mouseDataBus` EventEmitter
4. `useMouse` hook parses SGR format into click/scroll events
5. Windows: C# helper compiled at runtime for `ENABLE_VIRTUAL_TERMINAL_INPUT` console mode

### TUI Keyboard Controls

| Key | Action |
|-----|--------|
| Tab | Cycle focus: Tasks → Details → Logs |
| ↑/↓ | Navigate tasks / scroll panels |
| Enter | Toggle archived section (on divider) |
| A | Add Video dialog (Tab toggles advanced mode) |
| C | Cancel selected job |
| R | Retry failed job |
| D | Delete job (double-press to confirm) |
| F | Cycle filter: All/Active/Errors/Finished |
| T | Open Trim dialog (finished jobs) |
| O | Open output folder |
| W | Open web dashboard |
| ? | Help overlay |
| Q | Quit |

## Frontend Architecture

### Shoelace Components Used

`sl-tab-group`, `sl-dialog`, `sl-input`, `sl-select`, `sl-checkbox`, `sl-switch`, `sl-button` (with `loading`), `sl-badge`, `sl-progress-bar`, `sl-spinner`, `sl-alert` (toast), `sl-icon`, `sl-icon-button`, `sl-tag`, `sl-menu`, `sl-details`, `sl-divider`

### Module Communication

All modules receive `app` (MoomboxApp instance) in constructor. They use:
- `app.config` — shared config state
- `app.loadConfig()` / `app.loadStatus()` — trigger refreshes
- `app.showToast(message, variant)` — notifications
- `app.setInputValue()` / `app.getInputValue()` / `app.getInputNumber()` — form I/O
- `app.escapeHtml()` — XSS protection for all template strings

Inline `onclick` attributes in HTML templates reference global `window.app` directly (e.g., `onclick="app.settings.editChannel('...')"`).

### Player Features

- Niconico-style flying chat overlay (Web Animations API, 15 lanes, 8s duration, lane collision avoidance)
- Sidebar chat panel (forward-walk index optimization, programmatic scroll sync at 70%)
- Binary search for message window during nico spawning
- Toggle preferences persisted in `localStorage`

## Type System

### Job Interface (core data model)

Key fields beyond the obvious:
- `selectedVideoItag` / `selectedAudioItag` — manual format selection (`-1` = none)
- `startTime` / `endTime` — timestamp range in seconds
- `gaps` — `Array<{from, to, stream}>` — segments lost during parallel catch-up
- `trims` — `TrimRecord[]` — derivative trimmed versions
- `chatStatus` — `"pending" | "downloading" | "finished" | "error" | "unavailable"`
- `percent` — 0-100 for progress bar rendering
- `isVod` / `manuallyAdded` / `allowNonStream` — behavioral flags

### Error Hierarchy

```
MoomboxError (code, expected, context, cause)
  ├── YouTubeError           — code: "YOUTUBE_ERROR"
  ├── VideoPlayabilityError  — code: "PLAYABILITY_{STATUS}" (expected=true)
  ├── DownloadError          — code: "DOWNLOAD_ERROR", httpStatus
  ├── NetworkError           — code: "HTTP_{status}", url
  ├── ConfigError            — code: "CONFIG_ERROR"
  ├── MuxingError            — code: "MUXING_ERROR", exitCode
  └── AuthenticationError    — code: "AUTH_ERROR"
```

Helper: `wrapError(unknown, defaultMsg)` — wraps any value into `MoomboxError` (no double-wrapping).

### Zod Schemas (src/types/schemas.ts)

Runtime validation for all API endpoints:
- `addJobSchema` — video ID, optional itags/timestamps, cross-field validation
- `createTrimSchema` — startTime/endTime with duration ≥ 1s
- `updateConfigSchema` — allowlisted config keys with range validation
- `addChannelSchema` — channel ID format, optional terms
- `getPotSchema` — rejects deprecated `data_sync_id`/`visitor_data` fields

## Key Patterns & Conventions

### Code Patterns

- **Route registration:** `register*Routes(router, ctx)` — each module exports a registration function
- **Route handlers:** Always wrapped in `asyncHandler()` from `errorHandler.ts` — no try-catch in routes
- **Express 5 params:** `asyncHandler` loses type narrowing, so `req.params.id` becomes `string | string[]` — use `as string` cast
- **Frontend modules:** Controller classes in `modules/`, each takes `app` reference in constructor
- **Atomic file writes:** Write to `.tmp` file, then `fs.move()` with overwrite (used by SegmentDownloader, ChatDownloader, Database)
- **Batch DB updates:** 100ms coalescing window in `Database.updateJob()` — prevents disk thrashing from rapid progress updates
- **Event-driven progress:** SegmentDownloader and ChatDownloader extend EventEmitter, emit `start`/`progress`/`finish`/`gap`/`error`

### External Process Management

| Process | Package | Notes |
|---------|---------|-------|
| FFmpeg (mux/trim) | `execa` | `cancelSignal` for abort (execa v9+), 10-min timeout |
| ffprobe (metadata) | `execa` | 30-second timeout |
| Browser (auto-cookies) | `spawn` | Long-lived process, intentionally NOT execa |
| File explorer | `execa` | Fire-and-forget, `detached: true, cleanup: false` |
| Clipboard | `execa` | Platform-specific (powershell/pbpaste/xclip) |
| taskkill/kill | `execa` | Browser process cleanup |

### Cookie / Auth Flow

```
config.toml [cookie_file] → CookieJar.load() → parse Netscape format
  → CookieJar.generateAuthorizationHeader() → SAPISIDHASH
    → YouTubeAuth.generateApiHeaders() → Cookie + Authorization + X-Goog-* headers
```

AutoCookieService: `spawn(browser)` → user logs in → CDP or SQLite extraction → write `cookies.txt`
CookieRefreshService: GET youtube.com → parse Set-Cookie headers → update cookie file

### Format Selection (FormatSelector)

Video codec priority: `vp9.2` > `vp9` > `av01` > `avc1` > `h264`
Audio codec priority: `opus` > `mp4a.40.5` > `mp4a.40.2` > `mp4a`

Auto-selection: filter by `max_video_resolution` (max of width/height for portrait) → highest resolution → fps preference → codec score → lower bitrate → lower authLevel.

## Configuration

Config file: `config.toml` (searches cwd → `./config/` → `~/.config/moombox/`)

All settings have defaults in `ConfigManager.DEFAULTS`. Missing fields auto-populated on load. File permissions set to `0o600`.

### All Config Fields

| Setting | Default | Validation |
|---------|---------|------------|
| `port` | `774` | Integer 1–65535 |
| `network_access` | `"localhost"` | `"localhost"`, `"lan"`, `"external"` |
| `log_level` | `"INFO"` | `"DEBUG"`, `"INFO"`, `"WARN"`, `"ERROR"` |
| `log_file_path` | `"./moombox.log"` | - |
| `log_max_file_size` | `10485760` (10MB) | ≥ 1 |
| `log_max_files` | `5` | ≥ 1 |
| `database_path` | `"./moombox.json"` | - |
| `max_feed_items` | `15` | ≥ 1 |
| `feed_check_interval` | `10` (minutes) | ≥ 1 (number or ms-string) |
| `downloader.output_directory` | `"./output"` | - |
| `downloader.output_template` | `"${channel}/${start_date} ${title} [${id}]"` | - |
| `downloader.staging_directory` | `"./staging"` | - |
| `downloader.num_parallel_downloads` | `2` | ≥ 1 |
| `downloader.max_video_resolution` | `1080` | ≥ 1 |
| `downloader.cookie_file` | `"./cookies.txt"` | - |
| `downloader.download_chat` | `true` | - |
| `downloader.prefer_60fps` | `true` | - |
| `downloader.segment_retry_delay_cap` | `60` (seconds) | - |
| `downloader.segment_live_check_retries` | `16` | - |
| `downloader.ffmpeg_path` | undefined | Optional |
| `downloader.po_token` | undefined | Optional manual override |
| `downloader.visitor_data` | undefined | Optional manual override |
| `downloader.pot_provider_url` | undefined | Optional external provider |
| `tasklist.hide_finished_age_days` | `30` | Number or ms-string |
| `auto_cookies.enabled` | `false` | - |
| `auto_cookies.browser_profile_dir` | `"./browser-profile"` | - |

Template variables: `${title}`, `${id}`, `${channel}`, `${start_date}`, `${start_time}`

## Dependencies (23 production, 17 dev)

### Production Dependencies

| Package | Version | Used For |
|---------|---------|----------|
| `adm-zip` | 0.5.16 | ZIP extraction for imports |
| `astring` | 1.9.0 | AST-to-JS code generation (cipher solver) |
| `compression` | 1.8.1 | Express gzip middleware |
| `envalid` | 8.1.1 | Environment variable validation |
| `execa` | 9.6.1 | FFmpeg, ffprobe, browser cleanup, clipboard |
| `express` | 5.2.1 | HTTP server + REST API |
| `fast-xml-parser` | 5.3.6 | DASH manifest XML parsing |
| `fs-extra` | 11.3.3 | Extended fs (ensureDir, move, pathExists) |
| `ink` | 6.6.0 | React-for-CLI TUI framework |
| `jsdom` | 24.1.3 | DOM for BotGuard VM execution |
| `lowdb` | 7.0.1 | JSON file database (moombox.json) |
| `lru-cache` | 11.2.4 | Cipher solver caches (STS, preprocessed, solver) |
| `meriyah` | 7.0.0 | Fast JS parser for cipher AST extraction |
| `ms` | 2.1.3 | Human-readable time strings (replaces all hardcoded math) |
| `p-limit` | 7.3.0 | Parallel segment/decrypt concurrency caps |
| `p-queue` | 9.1.0 | Priority job queue with concurrency control |
| `p-retry` | 7.1.1 | HTTP retry with exponential backoff |

| `react` | 19.2.4 | React runtime for Ink TUI |
| `rss-parser` | 3.13.0 | YouTube channel RSS feed parsing |
| `sql.js` | 1.14.0 | Firefox cookies.sqlite via WebAssembly (dynamic import) |
| `toml` | 3.0.0 | Config file parsing |
| `ws` | 8.19.0 | WebSocket server + CDP client |
| `zod` | 4.3.6 | Runtime schema validation for API requests |

### Dev Dependencies

| Package | Purpose |
|---------|---------|
| `@types/*` | TypeScript type definitions (adm-zip, compression, express, fs-extra, jsdom, ms, node, react, supertest, ws) |
| `esbuild` | SEA bundling (TypeScript → CJS) |
| `postject` | SEA blob injection into Node.js binary |
| `supertest` | HTTP assertion testing |
| `typescript` | 5.9.3 — compiler |
| `vitest` | 4.0.18 — test runner + `@vitest/coverage-v8` + `@vitest/ui` |

### Notable: `tsx` not in devDependencies

`npm run dev` uses `npx tsx` which downloads on demand. Should be added as devDependency for reproducibility.

## Known Code Quality Issues

Reference these during code reviews and improvements:

### High Priority

| Issue | Location | Details |
|-------|----------|---------|
| Duplicate `DOWNLOAD_CHUNK_SIZE` | `constants.ts` (5MB) vs `constants/limits.ts` (1MB) | `LIMITS.DOWNLOAD_CHUNK_SIZE` is dead — only `constants.ts` value is imported |
| Duplicate `createRateLimiter` | `potRoutes.ts` vs `rateLimiter.ts` | potRoutes has its own inline copy instead of importing from `rateLimiter.ts` |
| Duplicate loopback check | `potRoutes.ts` | Hardcodes IP strings instead of importing `isLoopback()` from `ipValidation.ts` |
| Dead code: `PromiseQueue` | `src/utils/PromiseQueue.ts` | Fully implemented utility that is never imported. ConfigManager/Database/Logger use inline `.then()` chains instead |

### Medium Priority

| Issue | Location | Details |
|-------|----------|---------|
| `var` scoping workaround | `jobRoutes.ts:38,76` | `var` used to escape try-catch scope — should be `let` before the try block |
| Inline filename sanitizer | `importRoutes.ts:200-201` | Duplicates `sanitizeForFilename()` from `src/utils/sanitize.ts` |
| Missing Zod validation | `configRoutes.ts:62,149` | `req.body` accepted without schema validation before passing to ConfigManager |
| `console.log` in server | `server.ts:655` | Dashboard URL logged via `console.log` instead of `Logger` |
| `catch (e: any)` pattern | downloader.ts, chatApi.ts, muxFinalize.ts | Should use `catch (e: unknown)` with type narrowing |

### Low Priority

| Issue | Location | Details |
|-------|----------|---------|
| `any` typed formats | downloadOrchestrator.ts, downloadStrategies.ts | `selectedVideoFormat: any` should use `Format` interface |
| Duplicated `sleep()` | 5+ files | Same `new Promise(r => setTimeout(r, N))` pattern; should extract to `src/utils/` |
| Event listener cleanup | progressTracking.ts | `start`/`finish`/`error`/`gap` handlers not removed (only `progress` is) |
| Commented debug lines | cipher/playerCache.ts | 4 commented-out `console.log` lines |
| `(globalThis as any)` | server.ts, autoCookies.ts | SEA assets access — could use `declare global` augmentation |

## Constants Reference (src/constants.ts)

### Download Constants
- `DOWNLOAD_CHUNK_SIZE`: 5MB per HTTP chunk
- `MAX_DOWNLOAD_RETRIES`: 3
- `DOWNLOAD_TIMEOUT_MS`: 30s
- `PROGRESS_UPDATE_INTERVAL_MS`: 3s

### Worker Constants
- `WORKER_CHECK_INTERVAL_MS`: 5s (job queue poll)
- `STREAM_RECHECK_INTERVAL_MS`: 30s (stream status probe)
- `PROBE_JITTER_MAX_MS`: 30s (anti-thundering herd)
- `MAX_CONSECUTIVE_PROBE_ERRORS`: 10
- `STREAM_SEGMENT_TIMEOUT_MS`: 10m (no new segment = stream ended)
- `STREAM_END_VERIFY_INTERVAL_MS`: 5m

### Segment Downloader (engine/downloader.ts)
- `PARALLEL_DOWNLOADS` (catch-up mode): 6
- `CATCHUP_THRESHOLD`: 10 segments behind → parallel mode
- `MAX_SEGMENT_RETRIES`: 10 per segment in catch-up
- Head probe: every 5 seconds via `X-Head-Seqnum` response header
- Resume state saved every 10 (catch-up) or 50 (sequential) segments

### Chat Downloader (engine/chat/chatDownloader.ts)
- `FLUSH_THRESHOLD`: 50,000 messages → flush to disk, keep last 5,000 in memory
- Consecutive error tolerance: 20 (live), 5 (VOD)
- Stale continuation: up to 30 retries with exponential backoff (10s → 5min cap)

### YouTube Clients
- `TV_DOWNGRADED_CLIENT` — primary auth client (clientId: 7)
- `WEB_CLIENT` — watch page + DASH manifests (clientId: 1)
- `WEB_CREATOR_CLIENT` — members-only fallback (clientId: 62)
- `ANDROID_VR_CLIENT` — no auth needed, probe-only (clientId: 28)
- `BOTGUARD_REQUEST_KEY`: `"O43z0dpjhgX20SCx4KAo"`

## Database

`moombox.json` structure:
```typescript
{
  jobs: Job[],                          // Download jobs
  history: string[],                    // Processed video IDs (pruned at 10,000)
  lastVideos?: Record<string, string>   // channelId → most recent videoId
}
```

Performance: `jobsMap: Map<string, Job>` for O(1) lookup, `historySet: Set<string>` for O(1) membership. Corrupt DB auto-backed up to `moombox.json.corrupt.<timestamp>`. File permissions `0o600`.
