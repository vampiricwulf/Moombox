# CLAUDE.md

This file is working memory for Claude Code (claude.ai/code). It should contain everything needed to pick up development on any machine without re-analyzing the codebase. Keep it current.

## Release Process

When bumping a version, follow this order:

1. **Generate `RELEASE_NOTES.md`** — review commits since the last tag (`git log --oneline <prev-tag>..HEAD`), write concise user-facing release notes grouped by **Features**, **Improvements**, **Bug Fixes**, **Internal** (skip empty sections). No version heading — start directly with sections.
2. **Bump version** in `cmd/moombox/main.go` (`version = "x.y.z"`).
3. **Commit** both `RELEASE_NOTES.md` and the version bump together (e.g. `chore: bump version to x.y.z — short summary`).
4. **Tag** (`git tag vx.y.z`) and **push** (`git push && git push origin vx.y.z`).

CI reads `RELEASE_NOTES.md` from the repo — no API calls or CLI installs needed at build time.

## Working Style

When implementing features, fixes, or non-trivial changes, ask questions about design decisions and intent before diving in. Don't assume — clarify the "why" and preferred approach so the implementation matches what's actually wanted. This applies to things like naming, placement, scope, UX behavior, architectural choices, and aesthetic preferences (even for trivial changes). Ask questions one at a time with suggested answers rather than batching multiple questions together.

## What This Is

Moombox is a YouTube/Twitch live stream archiver written in Go. It monitors channels, detects live streams, downloads segments (DASH/HLS), records live chat, muxes with FFmpeg, and serves a web dashboard + TUI. It also serves as a lightweight yt-dlp alternative for YouTube/Twitch — keep track of upstream yt-dlp changes to their extraction/download logic. This is a standalone program — feature work, bug fixes, and improvements are the primary focus.

The Go rewrite from TypeScript is complete (all 12 phases, full file-by-file comparison done). `REWRITE_GUIDE.md` documents the original porting strategy for historical reference.

## Design Constraints

- **Windows-only** — Linux/Mac support only if explicitly requested. Always assume Windows.
- **No CGo** — Pure Go dependencies only. Simpler build chain over raw performance.
- **Resource efficiency** — Runs 24/7, minimize constant CPU/IO/Network/RAM usage.
- **Goja VMs are expensive** — BotGuard + cipher each hold multi-MB JS runtimes. Auto-evict when idle. Cipher solver capped at 3 cached VMs.
- **UX philosophy** — Intuitive for non-technical users, with advanced controls for power users. Sensible defaults, don't require expertise to get started.
- **Dual UI parity** — Web UI for accessibility/casual users, TUI for advanced users. Both are first-class citizens with full feature parity.

## Key Dependencies

| Library | Purpose |
|---------|---------|
| `go-chi/chi/v5` | HTTP router |
| `charmbracelet/bubbletea` + `bubbles` + `huh` + `lipgloss` | TUI framework (Charm ecosystem — see below) |
| `dop251/goja` | Pure-Go JS engine (BotGuard, cipher) |
| `modernc.org/sqlite` | SQLite driver (no CGo) |
| `nhooyr.io/websocket` | WebSocket |
| `BurntSushi/toml` | Config parsing |
| Shoelace v2.16 (CDN) | Web UI components |

### Charmbracelet / Charm ecosystem (TUI)

The TUI is built on **Charmbracelet's full Bubble Tea suite**. Before implementing or refactoring any TUI feature, check what's available in the Charm ecosystem — there may already be a well-tested component that fits.

| Package | What we use it for |
|---------|-------------------|
| `bubbletea` | Core TUI framework (Elm architecture, `Model`/`Update`/`View`) |
| `bubbles` | Pre-built components: `list`, `viewport`, `spinner`, `key`, `cursor`, `paginator` |
| `huh` | Form framework: multi-step wizards (setup wizard, add video advanced mode) |
| `lipgloss` | Styling: colors, borders, padding, layout composition |

**Design rule**: When designing TUI features, always check [Charm's repos](https://github.com/charmbracelet) first for existing components or patterns. Prefer using or extending their building blocks over rolling custom implementations. This applies to lists, text inputs, forms, file pickers, tables, progress bars, overlays, and any UI primitive.

## TypeScript Reference

The original TypeScript codebase is on the `abandoned-nodejs` branch. The local `references/` folder (gitignored) contains upstream source repos for cross-referencing protocol details:
- `yt-dlp` — YouTube format/cipher/extraction logic, Twitch extractor, PO token system, downloaders, cookies
- `BgUtils` — BotGuard/PO token generation (challenge, attestation, integrity tokens)
- `bgutil-ytdlp-pot-provider` — yt-dlp plugin for PO tokens (provider protocol)
- `ejs` — yt-dlp external JS for cipher/signature solving across runtimes
- `moonarchive` — Python stream archiver (DASH/HLS segment download strategies)
- `moombox` — the original Python moombox
- `chatterino7` — Twitch chat client (IRC, emotes, badges)

The most important references are **yt-dlp**, **BgUtils**, **ejs**, and **chatterino7** — these directly affect YouTube/Twitch extraction, authentication, cipher solving, and Twitch chat (IRC, emotes, badges) that Moombox reimplements in Go.

### Updating references

Run `bash references/update-all.sh` to pull all upstream repos and see what changed. The script records each repo's HEAD before pulling, then shows new commits and highlights Moombox-relevant file changes (YouTube/Twitch extractors, PO token system, cipher, downloaders, BotGuard core, etc.). Use `--diff` for verbose file-level diffs. Review the output for changes worth porting to Moombox.

## Build & Test Commands

```bash
# Build all packages
go build ./...

# Build the binary (single entry point)
go build -o moombox.exe ./cmd/moombox

# Build with version info (production)
go build -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse --short HEAD)" -o moombox.exe ./cmd/moombox

# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run a single package's tests
go test -v ./internal/engine/...

# Run a single test by name
go test -v -run TestParseDash ./internal/engine/...

# Static analysis
go vet ./...
```

Runtime requires FFmpeg on PATH. CI: `.github/workflows/release.yml` builds Windows exe on tag push, reads `RELEASE_NOTES.md` for the GitHub release body.

### Windows resource embedding

The exe icon and version info are embedded via `.syso` files generated at build time. Source files live in `cmd/moombox/winres/` (icon + `winres.json` config). CI generates the `.syso` files with the correct version from the git tag before building — no `.syso` files are committed. For local builds with an icon: `go install github.com/tc-hib/go-winres@latest && cd cmd/moombox && go-winres make`.

## Architecture

Module: `github.com/vampiricwulf/Moombox` — Go 1.25, single binary, all code under `internal/`.

### Process model (cmd/moombox/main.go, ~2,068 lines)

Moombox uses a **launcher/supervisor pattern**. The binary checks `_MOOMBOX_CHILD` env var on startup:
- **Without it** → launcher mode: spawns itself as a child, waits, respawns on exit code 42
- **With it** → application mode: runs the full service stack

All restarts (config change, update applied, setup wizard, API) set `restartRequested` and exit with code 42. The launcher respawns, picking up any new binary on disk (for updates). This avoids process chain buildup and keeps terminal state clean for BubbleTea.

A single `triggerRestart(source)` closure in `run()` handles all restart triggers. Shutdown uses a 10-second force-exit timer if graceful cleanup stalls. Non-blocking channel sends to the TUI use drop counters to avoid blocking the main event loop.

### Service initialization order (inside `run()`)

Config → Logger → Updater → Database (SQLite) → CookieJar → YouTube Service → Twitch Service → PotProvider (BotGuard) → CipherSolver → NotificationManager → DownloadWorker (orchestrator + stream processor + job queue) → TrimService → Monitors (Feed/DECAPI/Twitch) → CookieRefresh → AutoCookies → WebServer (chi + WebSocket) → TUI (BubbleTea)

All services are wired together in main.go via callback closures (OnVideoFound, OnStreamFound, OnSchedule) and struct-based dependency injection.

### Package dependency graph

```
cmd/moombox/main.go               ← launcher + orchestrator (~2,068 lines)
cmd/sign/main.go                  ← CI signing tool (Ed25519 key gen + binary signing)
├── internal/config        ~1,065  ← TOML config parsing, FlexDuration, channel terms (5 files)
├── internal/updater         ~626  ← GitHub release checker + self-updater + Ed25519 signing (5 files)
├── internal/logger          ~564  ← slog wrapper with file rotation, ring buffer (200), pub/sub
├── internal/database      ~2,030  ← SQLite with WAL, batch updates (100ms coalesce), pub/sub (4 files)
├── internal/cookies       ~2,749  ← jar, refresh, auto-cookie with Firefox/Chromium (6 files)
├── internal/youtube       ~2,076  ← Service, PlayerAPI, Auth, watch page, format selector (7 files)
├── internal/twitch        ~3,385  ← Service, GQL API, auth, HLS, IRC chat, VOD chat, emotes (9 files)
├── internal/bgutils       ~2,441  ← PO token: challenge → BotGuard VM (Goja) → integrity token → mint (8 files)
├── internal/cipher        ~2,001  ← YouTube signature cipher: AST + regex fallback, 3-VM LRU (9 files)
├── internal/engine        ~2,937  ← SegmentDownloader (DASH/HLS/VOD), manifest parser, FFmpeg muxer (4 files)
├── internal/chat          ~1,845  ← YouTube live chat downloader (polling API with batching) (4 files)
├── internal/worker        ~7,437  ← Worker, Orchestrator, StreamProcessor, Queue, Trim, Quality (16 files)
├── internal/monitor       ~1,687  ← FeedMonitor (RSS), DecapiMonitor, TwitchMonitor (5 files)
├── internal/notifications   ~527  ← Manager + Discord webhook (3 files)
├── internal/web           ~7,233  ← chi router, WebSocket hub, auth, middleware, 40+ endpoints (18 files)
│   └── internal/web/routes        ← All REST API route handlers
├── internal/tui          ~12,893  ← 3-panel layout, 8 overlay dialogs, chord system (21 files)
├── internal/goja          ~1,321  ← JS runtime shims (minimal DOM, TextEncoder, timers) (6 files)
├── internal/disk             ~57  ← Windows-only disk space queries (kernel32 GetDiskFreeSpaceExW)
├── internal/errors          ~755  ← Typed error hierarchy, 100+ sentinel codes
├── internal/constants       ~392  ← All hardcoded values (API keys, URLs, timeouts)
└── internal/utils         ~2,000  ← HTTP helpers, text/time formatters, YouTube URL parsing (22 files)
```

Total codebase: **~56,000 lines** of Go across 158 files (excluding tests, web assets, and main.go).

### Key data flow

```
Monitors (RSS/DECAPI/Twitch) → Job Database → StreamProcessor (probe/wait) →
DownloadOrchestrator → SegmentDownloader (DASH/HLS) + ChatDownloader →
FFmpeg Muxer → Output file + metadata
                  ↑
WebServer + TUI ← Database pub/sub ← WebSocket broadcasts
```

## Critical Patterns

### Logger interface (used by every package)
```go
logger interface {
    Debug(msg string, args ...any)
    Info(msg string, args ...any)
    Warn(msg string, args ...any)
    Error(msg string, args ...any)
}
```
This anonymous interface is repeated in every struct that needs logging. Do not change to a named interface — the pattern is intentional for loose coupling.

### Database partial updates
```go
db.UpdateJobFields(jobID, map[string]any{
    "status":   database.StatusDownloading,
    "progress": "V:1234 A:1234 C:5678",
})
```
`UpdateJobFields` dynamically builds SQL SET clauses. Always updates `updated_at` automatically. Triggers `OnJobUpdate` subscriber callbacks.

### Job status lifecycle
`Upcoming` → `Live` → `Downloading` → `Muxing` → `Finished`
Error paths: any state → `Error`, `Cancelled`, or `COOKIES?`

`JobStatus` is `type JobStatus string`. Timestamps are ISO 8601 strings (not `time.Time`). Optional numeric fields use pointers (`*int`, `*int64`). Database schema is at **v6** — v5 added a `segments` table for quality-split multi-segment recordings, v6 added `client_tokens` for persistent login tokens.

### Dependency injection via deps structs
```go
type DownloadWorkerDeps struct {
    CipherSolver  *cipher.Solver
    PotProvider   *bgutils.PotProvider
    TwitchService *twitch.Service
    Notifier      *notifications.Manager
}
```
Route handlers similarly use `FormatRoutesDeps`, `StatusRouteDeps`, etc.

### Goja (pure-Go JavaScript engine)
Used for two things only: BotGuard VM (PO token generation) and YouTube cipher decryption. Package `internal/goja/` provides minimal DOM shims. No CGo or V8 dependency. BotGuard uses a **triple-tier cache**: session tokens (6h TTL) → minter instances (dynamic TTL) → inflight request dedup. Cold start uses fallback tokens while the VM initializes. Cipher solver uses **full player.js AST approach** with legacy regex fallback — 3-VM LRU cache keyed by player.js URL.

### Download orchestration
- **StreamProcessor**: Probes video status, waits for live, handles auth upgrades. For Twitch, polls offline channels until live when manually added (one-time monitor)
- **DownloadOrchestrator**: Manages full lifecycle — segment downloaders + chat + mux + trim. Supports quality-split multi-segment recordings
- **SegmentDownloader**: Handles DASH sequential loop, HLS playlist polling, VOD parallel, and catch-up with bounded concurrency (6 parallel). Returns `ErrQualityLost` sentinel when selected quality disappears mid-stream
- **QualityMonitor**: Probes stream quality every 30s during live downloads. On resolution change, signals orchestrator to cleanly mux the current segment and start a new one at the new quality
- **Background mux pattern**: Completed segments are muxed in goroutines with `context.Background()` (detached from job context) so already-downloaded data is always muxed even during cancellation. Coordinated via `sync.WaitGroup`
- **Live stream verification loop**: After downloaders stop, probes YouTube API to confirm stream ended vs still live (up to 6 consecutive checks)

### Worker concurrency model
The DownloadWorker uses **dual-layer concurrency**: 100 lifecycle goroutine slots (for probing, waiting, auth upgrades) and a separate configurable download semaphore (default 2) for actual segment downloading. This prevents slow probes from blocking downloads and vice versa. The JobQueue uses a `sync.Map` for O(1) lookups alongside a mutex-protected slice for ordered iteration.

### WebSocket throttling
WebSocket broadcasts to web clients use **100ms leading/trailing edge throttling**. The first update fires immediately (leading edge), then subsequent updates within 100ms are coalesced and the latest state is sent after the window expires (trailing edge). This prevents flooding clients during rapid progress updates while maintaining responsiveness.

### Database batch coalescing
`OnJobUpdate` subscriber callbacks are debounced via a **100ms signal-driven coalescing window**. When a job update occurs, it starts a 100ms timer — additional updates reset the timer. Only after 100ms of quiet does the batch callback fire. This dramatically reduces UI update frequency during heavy download activity.

### YouTube multi-client auth strategy
The YouTube service uses a **multi-client fallback chain** across Innertube API clients: WEB, ANDROID, IOS, TV_EMBEDDED, MWEB, WEB_CREATOR. Each client has different capabilities and auth requirements. Format selection priority: resolution > FPS > codec > bitrate > auth level. Auth levels range from AndroidVR(0) to WebCreator(5).

### Twitch emote caching
Twitch emotes (BTTV, FFZ, 7TV) use an **LRU cache** capped at 200 channels. Global emotes are cached separately. Emote data is fetched lazily on first chat connection to a channel and evicted LRU when the cache is full.

### Auto-cookies browser extraction
Cookie extraction supports **Firefox** (direct SQLite read from `cookies.sqlite`) and **Chromium** browsers (Chrome, Edge, Brave — via Chrome DevTools Protocol). Browser processes spawned for cookie extraction use **Windows Job Objects** (`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`) to guarantee cleanup even on crashes. Browser detection uses Windows Registry lookups.

### Updater binary swap + Ed25519 signing
Self-update uses a **three-step rename dance**: download new binary to `.new`, rename current to `.old`, rename `.new` to current. Separate HTTP timeouts: 10s for API calls, 5min for binary downloads. On restart, the launcher picks up the new binary automatically.

**Signature verification**: CI signs release binaries with Ed25519 (`cmd/sign` tool, private key in `SIGNING_KEY` GitHub Actions secret). The updater downloads both `.exe` and `.exe.sig`, verifies the signature against the public key embedded in `internal/updater/signing.go` before proceeding with the rename dance. Verification is skipped for local dev builds (no `SignatureURL`). On startup, `CleanupOldBinary()` sweeps stale `.old`, `.new`, and `.new.sig` files from interrupted updates.

### Web server middleware stack
7-layer middleware: RecoveryMiddleware → RequestLogger → CSRFMiddleware → AuthMiddleware → IPAccessControl → RateLimiter → CSP/security headers. Auth uses scrypt password hashing with 24-hour sessions and persistent client tokens (database v6).

### Monitor → Worker wiring
Monitors fire `OnVideoFound`/`OnStreamFound` callbacks. These are closures set in main.go that create database jobs and enqueue them to the DownloadWorker's JobQueue.

### CSRF protection
`CSRFMiddleware` in `web/middleware.go` validates Origin/Referer headers on mutating requests. The TUI bypasses this via a shared secret: the web server generates a 16-byte random hex token at startup (`X-Internal-Token` header), passed to the TUI which injects it on every API call via a custom `http.RoundTripper`. Browsers cannot set custom headers cross-origin without CORS preflight (which the server doesn't grant).

### FFmpeg install with UAC elevation
The FFmpeg install process uses a two-phase flow for non-elevated processes. `PrepareInstall` checks `isElevated()` — if already elevated, runs `InstallFFmpeg` directly. If not, it generates a PowerShell script, stores it in `pendingInstalls` (keyed by random token, 5-minute TTL), and returns the script for user review. `ConfirmInstall` writes the script to a temp file, launches it via `ShellExecuteExW` with `runas` verb (UAC prompt), waits on the process handle, then reads a JSON result file. `RejectInstall` cleans up the pending entry. The elevation code in `ffmpeg_elevation_windows.go` uses the same `syscall.NewLazyDLL` pattern as `internal/disk/`.

### Config migrations
`migrateOldFormat()` in `config/config.go` handles backward compatibility with older config layouts. It migrates flat top-level fields into their current sections (`[network]`, `[paths]`, `[logs]`, `[monitors]`, `[cookies]`), converts legacy boolean flags (e.g. `allow_lan`/`allow_external` → `network_access`), and merges the old `[auto_cookies]` and `[tasklist]` sections into their new homes. Migrations are non-destructive — they only apply when the new section doesn't already exist. When restructuring the config schema, add migration logic here for any renamed or relocated fields so existing user configs keep working.

### API route prefix
All REST endpoints use `/api/` prefix (no version number). Route registration and frontend fetch calls must stay in sync across Go route files and `web/public/` JS files.

### Panic recovery
All spawned goroutines use inline `defer func() { if r := recover(); r != nil { ... } }()`. No shared helper — pattern is consistently inline. HTTP handlers are covered by `RecoveryMiddleware` in `web/server.go`. Database subscriber callbacks use `safeCallJobUpdate`/`safeCallJobsChange` wrappers.

### TUI structure (21 files, ~7,000 lines)
3-panel layout: task list (left) + job details (right) + log viewer (bottom). Tab switches focus — focused panel expands. 8 overlay dialogs: add video (quick/advanced/import modes), settings, help, trim, orphans, client tokens, action menu, confirm. `ProgressStore` manages per-job progress state with 100ms update batching. `StyleCache` pre-computes lipgloss styles on resize to avoid per-render allocation.

### TUI chord system
`buildMenuItems()` in `app.go` is the **single source of truth** for all keyboard chords, the action menu (M key), chord feedback hints, and help text. `dispatchAction(chord, job)` is the unified execution handler. Adding a new chord requires changes in only two places: the item in `buildMenuItems()` and a case in `dispatchAction()`.

Chord prefixes: **A** (Action), **R** (Request), **O** (Open), **Q** (Quit). Single-key shortcuts: **F** (Filter), **M** (Menu), **`** (Settings), **?** (Help). Each `ActionMenuItem` has `Chord`, `Label`, `HintLabel`, `Category`, `NeedsJob`, `NeedsConfirm`, and `JobFilter` fields. The chord routing (`processSecondKey`), feedback hints (`chordFeedback`), and help text (`HelpModel.SetMenuItems`) all derive from this list.

Confirm chords (e.g. A C, A D, R P) require a third keypress matching the second key within 3 seconds. Job-specific chords validate the selected job against the item's `JobFilter` before executing.

## Web UI

Static assets live in `web/public/` and are embedded via `go:embed` in `web/embed.go`. Changes to CSS/JS/HTML require `go build` to take effect.

- `app.js` — main SPA logic (jobs, logs, status bar, WebSocket, settings dialogs)
- `modules/player.js` — video player with niconico-style chat overlay + sidebar chat, multi-segment playback with cross-segment seeking and trim
- `modules/setup.js` — first-run wizard + FFmpeg overlay (install flow, UAC elevation, manual path)
- `modules/settings.js` — settings dialog (config, channels, cookies, integrations)
- `modules/trimmer.js` — trim clip creation UI
- `modules/stats.js` — statistics dashboard
- `modules/imports.js` — zip archive import UI
- `modules/utils.js` — shared formatting helpers
- `moombox.css` — all styles including mobile responsive rules
- `login.html` — standalone auth page
- `index.html` — SPA shell with Shoelace UI components

Mobile breakpoints: `992px` (tablet/layout), `768px` (phone/touch), `hover: none` (touch devices).

## Maintaining This File

When asked to update CLAUDE.md, don't just add context from the current session — check git history for all changes since the last substantive update:
1. `git log --oneline --all -- CLAUDE.md` to find when it was last updated
2. `git log --oneline <last-update>..HEAD` to see all commits since then
3. Read RELEASE_NOTES.md at each version tag for architectural changes
4. Look for: new packages, line count drift in main.go, new critical patterns, database schema changes, new worker components, web UI capabilities
5. Keep updates concise — this documents architecture and patterns, not a changelog
