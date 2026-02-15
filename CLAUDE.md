# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
npm install              # Install dependencies
npm run build            # Compile TypeScript
npm run start            # Start with TUI + web dashboard
npm run dev              # Run directly from TypeScript (development)
node dist/index.js add <video_id_or_url>  # Manually add video to queue
```

**Flags:**
- `--no-tui` - Disable TUI, use web dashboard only

## Building Executable

```bash
npm run package          # Build self-extracting Moombox.exe
```

The build process creates a self-extracting executable:
1. Downloads Node.js 20.x if not present (used as the SEA base)
2. Bundles TypeScript source with esbuild (no tsc needed)
3. Embeds static web assets (dashboard HTML/CSS/JS)
4. Compresses app bundle into SEA launcher
5. Creates Node.js SEA that extracts and loads the bundle via `createRequire()` in-process (no child process)

**Result:** `Moombox.exe` (~72MB) that extracts:
- `assets/moombox-app.cjs` - Application bundle

**Note:** Yoga-layout's top-level `await` is handled via a Proxy shim (`scripts/yoga-shim.js`) that defers initialization until `startTUI()` calls `loadYoga()`. Ink's reconciler devtools `await import()` is stripped by an esbuild plugin.

## Ports

- **Dashboard:** http://localhost:774
- **POT Provider:** http://localhost:774 (same server, `/get_pot` endpoint)

The POT provider is compatible with bgutil-ytdlp-pot-provider for yt-dlp.

## Architecture Overview

Moombox is a YouTube live stream archiver with a web dashboard. It monitors channels via RSS feeds, detects livestreams, and downloads them using native JavaScript implementations of YouTube's signature decryption and BotGuard (PO Token).

### Data Flow

```
RSS Feed Monitor (10min) → Job Database (lowdb) → Download Worker → YouTube API
                                                         ↓
                             Web Dashboard ← FFmpeg Muxer ← Segment Downloads
                            (localhost:774)
```

### Major Refactor (v1.0.0 → v1.1.0)

**Commit:** `a01f6da` - February 2026
**Impact:** 25 files changed, 4,033 insertions, 3,729 deletions

The codebase underwent a comprehensive modularization refactor to improve maintainability and separation of concerns:

**Backend Modularization:**
- **`src/core/worker/downloadOrchestrator.ts`** (1,061 lines removed)
  - Extracted into focused modules:
    - `downloadStrategies.ts` (423 lines) - Live, VOD, and parallel download strategies
    - `muxFinalize.ts` (330 lines) - FFmpeg muxing and finalization logic
    - `progressTracking.ts` (320 lines) - Download progress calculation
    - `formatUtils.ts` (29 lines) - Format selection utilities

**Web Routes Modularization:**
- **`src/web/server.ts`** (989 lines removed)
  - Extracted into RESTful route modules:
    - `routes/jobRoutes.ts` (340 lines) - Job CRUD, formats API, video streaming
    - `routes/configRoutes.ts` (243 lines) - Configuration management
    - `routes/importRoutes.ts` (255 lines) - Video import/upload handling
    - `routes/potRoutes.ts` (130 lines) - POT provider endpoints
    - `routes/errorHandler.ts` (39 lines) - Async error handling wrapper
    - `routes/index.ts` (5 lines) - Route registration

**Frontend Modularization:**
- **`src/web/public/app.js`** (1,757 lines removed)
  - Extracted into ES6 modules:
    - `modules/player.js` (533 lines) - Video player with synced chat
    - `modules/settings.js` (758 lines) - Settings UI and auto-cookie setup
    - `modules/imports.js` (166 lines) - Import tab for .zip uploads
    - `modules/setup.js` (205 lines) - First-time setup wizard

**Benefits:**
- ✅ Improved code organization and readability
- ✅ Better separation of concerns (SRP compliance)
- ✅ Easier testing and debugging (isolated modules)
- ✅ Reduced cognitive load (smaller files)
- ✅ Modular route registration pattern (`register*Routes()`)
- ✅ Frontend ES6 modules with clean exports

**Pattern Established:**
- Backend routes: `register*Routes(router, ctx)` pattern
- Frontend modules: Controller classes that take `app` reference
- Error handling: `asyncHandler()` wrapper for Express 5 compatibility

### Key Directories

- `src/core/` - Application services (config, database, logging, worker, cookies)
- `src/core/worker/` - Download orchestration modules:
  - `downloadOrchestrator.ts` - Main orchestrator (coordinates strategies)
  - `downloadStrategies.ts` - Live, VOD, parallel download implementations
  - `muxFinalize.ts` - FFmpeg muxing and file finalization
  - `progressTracking.ts` - Download progress calculation
  - `formatUtils.ts` - Format selection utilities
  - `timeUtils.ts` - Time parsing for timestamp selection
- `src/engine/` - Download engine (YouTube API, manifest parsing, muxing)
- `src/engine/youtube/` - Modularized YouTube service (auth, player API, format selection)
- `src/engine/chat/` - Live chat downloader (ChatDownloader, ChatApi)
- `src/web/` - Express server and web dashboard
- `src/web/routes/` - Modular Express routes (RESTful API endpoints):
  - `jobRoutes.ts` - Job CRUD, formats API, video streaming
  - `configRoutes.ts` - Configuration management
  - `importRoutes.ts` - Video import/upload handling
  - `potRoutes.ts` - POT provider endpoints (yt-dlp compatible)
  - `errorHandler.ts` - Async error handling wrapper
- `src/web/public/` - Static HTML/CSS/JS for dashboard UI
- `src/web/public/modules/` - Frontend ES6 modules:
  - `player.js` - Video player with synchronized chat
  - `settings.js` - Settings UI and auto-cookie setup
  - `imports.js` - Import tab for .zip uploads
  - `setup.js` - First-time setup wizard
- `src/tui/` - Terminal UI built with Ink (React for CLI)
- `src/tui/components/` - TUI React components (TaskList, LogViewer, JobDetails, AddVideoDialog)
- `src/tui/hooks/` - Custom hooks (useMouse for mouse support)
- `src/types/` - Centralized TypeScript interfaces
- `src/utils/` - Shared utilities (extractVideoId for URL parsing)
- `src/bgutils/` - BotGuard/PO Token generation (native JS port)
- `src/cipher/` - YouTube signature decryption (yt-cipher port)
- `src/ejs/` - JavaScript-in-JavaScript solver for signature functions

### Singleton Services

All major services use `getInstance()` pattern:
- `ConfigManager` - TOML config loader
- `Logger` - File + web logging with pub/sub
- `Database` - lowdb wrapper for `moombox.json` with pub/sub for real-time updates
- `YouTubeService` - Video info, formats, manifests
- `DownloadWorker` - Job processing orchestration
- `WebServer` - Express + WebSocket server
- `PotProvider` - BotGuard/PO Token generation
- `AutoCookieService` - Automatic cookie acquisition via browser

### Database Pub/Sub

The Database class provides event subscriptions for real-time UI updates:
- `db.onJobUpdate(callback)` - Called when any job is updated (progress, status)
- `db.onJobsChange(callback)` - Called when jobs are added or deleted

### Download Features

- **Manual format selection:** Per-job format selection via "Advanced Options" in the Add Video dialog. Users can select specific video/audio itags or choose "None" for video-only/audio-only downloads (itag -1). The `FormatSelector.selectWithOptions()` method handles manual selection with automatic fallback.
- **Timestamp selection:** Segment-level download range via start/end time. For DASH streams, `ManifestParser.calculateSegmentRange()` maps timestamps to segment indices. The `SegmentDownloader` respects `endSeq` to stop at the right segment. FFmpeg re-encodes trimmed segments with `libx264`/`aac` for exact duration matching (fixes keyframe overshoot). Time input supports `HH:MM:SS`, `MM:SS`, or raw seconds.
- **Post-mux metadata extraction:** After muxing, `ffprobe` extracts actual file properties (duration, resolution, size) and updates the job metadata. This ensures `lengthSeconds` reflects the trimmed output, not YouTube's original duration.
- **Parallel segment downloads:** When catching up on a live stream, downloads 6 segments in parallel
- **Head sequence tracking:** Monitors the live stream head to detect when falling behind
- **Automatic catch-up:** Switches to parallel mode when >10 segments behind
- **Resume support:** Saves download state periodically for crash recovery
- **Early chat archiving:** When a stream is in the `Upcoming` state, `StreamProcessor.waitForLive()` starts a `ChatDownloader` during the probe phase to capture pre-stream chat messages. If chat is initially closed (continuation not available), it retries on each probe iteration. The pre-started `ChatDownloader` is passed via `StreamProcessResult.chatDownloader` to `DownloadOrchestrator.execute()` so it continues seamlessly into the live download phase without creating a duplicate downloader.
- **Graceful shutdown:** `DownloadWorker.stop()` propagates to `StreamProcessor.stop()` and `DownloadOrchestrator.stop()`. The orchestrator tracks all active `SegmentDownloader` and `ChatDownloader` instances and stops them on shutdown. The stream processor also stops any early chat downloaders it started during the upcoming phase.

### Job Status Flow

`Upcoming` → `Live` → `Downloading` → `Muxing` → `Finished`

Special states: `Error`, `Cancelled`, `COOKIES?` (member content needs cookie refresh)

## Terminal UI (TUI)

When running in an interactive terminal, Moombox displays a full-screen TUI built with Ink:
- **Top panel (75%):** Task list + job details side by side
- **Bottom panel (25%):** Live log viewer
- **Tab:** Switch focus between panels (focused panel expands to 75%)
- **Mouse support:** Click to select tasks, scroll wheel to navigate
- **Archived jobs:** Finished jobs older than `hide_finished_age_days` appear under a collapsible `Archived (N)` divider at the bottom of the task list

### TUI Keyboard Controls

| Key | Action |
|-----|--------|
| Tab | Switch focus between Tasks/Details/Logs |
| ↑/↓ | Navigate tasks or scroll logs |
| Enter | Expand/collapse archived jobs (on divider row) |
| A | Open Add Video dialog (Tab to navigate, ←/→ to toggle advanced) |
| C | Cancel selected job |
| R | Retry failed job |
| D | Delete job (press twice to confirm) |
| F | Cycle status filter (All/Active/Errors/Finished) |
| O | Open output folder (finished jobs) |
| W | Open web dashboard in browser |
| ? | Toggle help overlay |
| Q | Quit |

### TUI Add Video Dialog

Press **A** to open the Add Video dialog:
- Press **Tab** to navigate between URL input and advanced options checkbox
- Press **←/→ arrows** or **Space** to toggle advanced options when focused on checkbox
- **[✓] Advanced** - Shows 6-step wizard (format + timestamp selection)
- **[ ] Quick add** - Immediate submission with auto settings (best quality)

**Advanced Options Wizard (6 steps):**
  1. **Enter URL/ID** - Paste YouTube URL or video ID
  2. **Select Video Format** - Numbered list with best-format badges (`[a]` auto, `[n]` none for audio-only)
  3. **Select Audio Format** - Choose audio quality (`[a]` auto, `[n]` none for video-only)
  4. **Start Time** - Optional timestamp in `HH:MM:SS`, `MM:SS`, or seconds (blank = beginning)
  5. **End Time** - Optional timestamp (blank = end of video)
  6. **Confirmation** - Review selections before submitting

**Navigation:**
- **Esc** - Go back one step or cancel
- **Tab** - Switch focus between URL input and checkbox (step 0 only)
- **←/→ / Space** - Toggle checkbox when focused on it
- **↑/↓** - Scroll format lists (when >10 formats)
- **Ctrl+V / Right-click** - Paste from clipboard
- **Numbers (1-9)** - Quick select format by number

**Validation:**
- ❌ Both formats cannot be "None" (would download nothing)
- ❌ End time must be after start time
- Inline error messages guide user to fix issues

## Web Dashboard

The dashboard runs at `http://localhost:774` and provides:
- Real-time job list with status updates (via WebSocket)
- **Add videos** by URL or ID with optional **Advanced Options**:
  - Manual format selection (video/audio dropdowns with best-format badges)
  - Timestamp selection (start/end time for partial downloads)
  - Validation prevents invalid combinations (e.g., video-only + audio-only)
- Cancel, retry, delete jobs
- View job details with embedded video player
- **Archived tab** for viewing finished jobs older than `hide_finished_age_days` (fetched on demand via REST)
- **Player tab** for replaying archived videos with synchronized chat (Niconico-style flying overlay + sidebar chat, independently togglable)
- **Imports tab** for uploading .zip archives containing video + optional chat JSON. Files are extracted to the `imports/` subdirectory of the configured output directory, and a Finished job is created for immediate playback in the Player tab.
- Live log viewer
- POT provider endpoint for yt-dlp compatibility

### API Endpoints

- `GET /api/jobs` - List all jobs (excludes archived)
- `GET /api/jobs/archived` - List archived jobs (finished older than `hide_finished_age_days`)
- `GET /api/jobs/:id` - Get job details
- `GET /api/jobs/:id/video` - Stream video file (supports Range requests for seeking)
- `GET /api/formats/:videoId` - Get available video/audio formats for a video (for Advanced Options)
- `POST /api/jobs` - Add new job `{ videoId: string, selectedVideoItag?: number, selectedAudioItag?: number, startTime?: number, endTime?: number }`
- `POST /api/jobs/:id/cancel` - Cancel job
- `POST /api/jobs/:id/retry` - Retry failed job
- `DELETE /api/jobs/:id` - Delete job
- `POST /api/import` - Import zip archive (Content-Type: application/octet-stream, optional headers: X-Import-Title, X-Import-Channel)
- `GET /api/logs` - Get recent logs
- `GET /api/status` - Server status
- `GET /api/cookies/auto-status` - Auto-cookie service status
- `POST /api/cookies/auto-setup/start` - Launch browser for login
- `POST /api/cookies/auto-setup/finish` - Extract cookies after login
- `POST /api/cookies/auto-setup/cancel` - Cancel in-progress setup

### POT Provider Endpoints (yt-dlp compatible)

- `POST /get_pot` - Generate PO token `{ content_binding?: string }`
- `POST /invalidate_caches` - Clear all caches
- `POST /invalidate_it` - Invalidate integrity tokens
- `GET /ping` - Health check
- `GET /minter_cache` - Debug endpoint to view cached minters

To use with yt-dlp, configure the extractor args:
```bash
yt-dlp --extractor-args "youtube:player-client=web;po_token=web.gvs+http://127.0.0.1:774/get_pot" <URL>
```

### WebSocket Messages

- `initial_state` - Jobs and logs on connect
- `jobs_update` - Full jobs array (on add/delete)
- `job_update` - Single job update (real-time progress, throttled to 10/sec)
- `log` - New log entry

## Key Patterns

### Code Organization (Post-Refactor)

**Route Registration Pattern:**
```typescript
// src/web/routes/index.ts
export function registerRoutes(router: Router, ctx: RouteContext) {
  registerJobRoutes(router, ctx);
  registerConfigRoutes(router, ctx);
  registerImportRoutes(router, ctx);
  registerPotRoutes(router, ctx);
}
```

Each route module exports a `register*Routes(router, ctx)` function that:
- Takes an Express router and shared context
- Registers related endpoints
- Uses `asyncHandler()` wrapper for Express 5 async compatibility

**Frontend Module Pattern:**
```javascript
// src/web/public/modules/*.js
export class ModuleController {
  constructor(app) {
    this.app = app;  // Reference to main app for shared state
    this.init();
  }

  init() { /* Setup event listeners, render UI */ }
}
```

Frontend modules are ES6 classes that:
- Take `app` reference in constructor
- Initialize in `init()` method
- Export a single default class
- Imported dynamically by main `app.js`

**Error Handling:**
- Routes use `asyncHandler()` wrapper from `src/web/routes/errorHandler.ts`
- No try-catch needed in route handlers (wrapper handles it)
- Express 5 params lose type narrowing → use `as string` cast for `req.params.id`

### Output Template Variables
Used in `output_template` config:
- `${title}`, `${id}`, `${channel}`, `${start_date}`, `${start_time}`

### Feed Monitoring
- `terms` config uses regex patterns matched against title AND description
- `num_desc_lookbehind` compares descriptions with N older items to isolate unique content
- `include_non_live_content` - When true, downloads regular videos too

### Error Handling
Custom error classes in `src/types/errors.ts`:
- `YouTubeError`, `VideoPlayabilityError`, `DownloadError`, `NetworkError`, `AuthenticationError`
- Each has a machine-readable `code` field (e.g., `PLAYABILITY_MEMBERS_ONLY`)

### YouTube Authentication
- Cookies loaded from Netscape format file (`cookie_file` config)
- SAPISIDHASH generated for API auth
- PO Token generated via native BotGuard (JSDOM-based, no external server)
- `CookieRefreshService` keeps sessions alive

## Configuration

Config file: `config.toml` (searches cwd, ./config/, ~/.config/moombox/)

All settings have sensible defaults defined in `ConfigManager.DEFAULTS` (`src/core/config.ts`). Missing fields are automatically populated with defaults when loading.

### Default Values

| Setting | Default |
|---------|---------|
| `log_level` | `"INFO"` |
| `log_file_path` | `"./moombox.log"` |
| `log_max_file_size` | `10485760` (10MB) |
| `log_max_files` | `5` |
| `database_path` | `"./moombox.json"` |
| `max_feed_items` | `15` |
| `downloader.output_directory` | `"./output"` |
| `downloader.output_template` | `"${channel}/${start_date} ${title} [${id}]"` |
| `downloader.staging_directory` | `"./staging"` |
| `downloader.num_parallel_downloads` | `2` |
| `downloader.max_video_resolution` | `1080` (based on max of width/height) |
| `downloader.cookie_file` | `"./cookies.txt"` |
| `tasklist.hide_finished_age_days` | `30` |
| `auto_cookies.enabled` | `false` |
| `auto_cookies.browser_profile_dir` | `"./browser-profile"` |

### Example Config

```toml
log_level = "DEBUG"
[downloader]
output_directory = "./output"
output_template = "${channel}/${start_date} ${title} [${id}]"
cookie_file = "./cookies.txt"
max_video_resolution = 1080  # Limits max(width, height), handles portrait videos

[[channels]]
id = "UCxxxxxxxx"
terms = { stream = "(?i)live" }
include_non_live_content = false  # Set true to download regular videos
```

## Database

`moombox.json` structure:
```typescript
{
  jobs: Job[],        // Download jobs with status, progress, metadata
  history: string[]   // Video IDs already processed by monitor
}
```

## Constants

Shared values are in `src/constants.ts`:
- User-Agent strings for different YouTube clients
- API URLs and keys
- Download chunk sizes and retry settings
- YouTube client configurations (TV_DOWNGRADED, WEB_CREATOR, etc.)
