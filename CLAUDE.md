# CLAUDE.md

Working memory for Claude Code (claude.ai/code). Contains everything needed to pick up development without re-analyzing the codebase. Keep it current.

## What This Is

Moombox is a YouTube/Twitch live stream archiver written in Go. It monitors channels, detects live streams, downloads segments (DASH/HLS), records live chat, muxes with FFmpeg, and serves a web dashboard + TUI. Also serves as a lightweight yt-dlp alternative for YouTube/Twitch — track upstream yt-dlp changes to their extraction/download logic. This is a standalone program — feature work, bug fixes, and improvements are the primary focus.

The Go rewrite from TypeScript is complete. `REWRITE_GUIDE.md` documents the porting strategy for historical reference.

### Design Constraints

- **Windows-only** — Linux/Mac only if explicitly requested. Always assume Windows.
- **No CGo** — pure Go dependencies only. Simpler build chain over raw performance.
- **Resource efficiency** — runs 24/7, minimize constant CPU/IO/Network/RAM.
- **Goja VMs are expensive** — BotGuard + cipher hold multi-MB JS runtimes. Auto-evict when idle. Cipher capped at 3 cached VMs.
- **Dual UI parity** — Web UI + TUI are both first-class with full feature parity.
- **UX philosophy** — intuitive for non-technical users, advanced controls for power users. Sensible defaults.

## Working Style

When implementing features, fixes, or non-trivial changes, ask questions about design decisions and intent before diving in. Don't assume — clarify the "why" and preferred approach so the implementation matches what's actually wanted. This applies to naming, placement, scope, UX behavior, architectural choices, and aesthetic preferences (even for trivial changes). Ask questions one at a time with suggested answers rather than batching.

## Key Dependencies

| Library | Purpose |
|---------|---------|
| `go-chi/chi/v5` | HTTP router |
| `charmbracelet/bubbletea` + `bubbles` + `huh` + `lipgloss` | TUI (Charm ecosystem) |
| `dop251/goja` | Pure-Go JS engine (BotGuard, cipher) |
| `modernc.org/sqlite` | SQLite driver (no CGo) |
| `nhooyr.io/websocket` | WebSocket |
| `BurntSushi/toml` | Config parsing |
| Shoelace v2.16 (CDN) | Web UI components |

**TUI design rule**: Always check [Charm's repos](https://github.com/charmbracelet) for existing components before building custom ones. Prefer extending their building blocks over rolling custom implementations — lists, inputs, forms, tables, progress bars, and any UI primitive.

## Build & Test

```bash
go build ./...                                      # Build all packages
go build -o moombox.exe ./cmd/moombox               # Build binary
go test ./...                                       # Run all tests
go test -v ./internal/engine/...                    # Single package
go test -v -run TestParseDash ./internal/engine/... # Single test
go vet ./...                                        # Static analysis
```

Runtime requires FFmpeg on PATH. CI (`.github/workflows/release.yml`) builds Windows exe on tag push, reads `RELEASE_NOTES.md` for GitHub release body.

### Windows resource embedding

Exe icon and version info via `.syso` files from `cmd/moombox/winres/`. CI generates at build time — none committed. Local builds with icon: `go install github.com/tc-hib/go-winres@latest && cd cmd/moombox && go-winres make`.

## Architecture

Module: `github.com/vampiricwulf/Moombox` — Go 1.25, single binary, all code under `internal/`.

### Process model

**Launcher/supervisor pattern** via `_MOOMBOX_CHILD` env var:
- **Without it** → launcher: spawns itself as child, respawns on exit code 42
- **With it** → application: runs full service stack

All restarts (config change, update, setup wizard, API) exit with code 42 via `triggerRestart(source)`. The launcher respawns, picking up any new binary (for updates). Shutdown uses a 10-second force-exit timer.

### Service initialization order

Config → Logger → Updater → Database → CookieJar → YouTube → Twitch → PotProvider → CipherSolver → NotificationManager → DownloadWorker → TrimService → Monitors → CookieRefresh → AutoCookies → WebServer → TUI

Services wired via callback closures (`OnVideoFound`, `OnStreamFound`, `OnSchedule`) and struct-based dependency injection in `main.go`.

### Package dependency graph

```
cmd/moombox/main.go                 ← launcher + orchestrator (~2,074 lines)
cmd/sign/main.go                    ← CI signing tool (Ed25519)
├── internal/config          ~819   ← TOML config, FlexDuration, channel terms (4 files)
├── internal/updater         ~447   ← GitHub release checker + self-updater + Ed25519 (3 files)
├── internal/logger          ~447   ← slog wrapper, file rotation, ring buffer, pub/sub (1 file)
├── internal/database      ~1,819   ← SQLite/WAL, batch updates (100ms coalesce), pub/sub (3 files)
├── internal/cookies       ~2,620   ← jar, refresh, auto-cookie with Firefox/Chromium (5 files)
├── internal/youtube       ~1,972   ← Service, PlayerAPI, Auth, watch page, format selector (6 files)
├── internal/twitch        ~3,179   ← Service, GQL API, auth, HLS, IRC chat, VOD chat, emotes (8 files)
├── internal/bgutils       ~1,355   ← PO token: challenge → BotGuard VM (Goja) → mint (7 files)
├── internal/cipher        ~1,475   ← YouTube signature cipher: AST + regex fallback, 3-VM LRU (7 files)
├── internal/engine        ~2,725   ← SegmentDownloader (DASH/HLS/VOD), manifest, FFmpeg muxer (3 files)
├── internal/chat          ~1,434   ← YouTube live chat downloader (polling + batching) (3 files)
├── internal/worker        ~6,443   ← Worker, Orchestrator, StreamProcessor, Queue, Trim, Quality (13 files)
├── internal/monitor       ~1,455   ← FeedMonitor (RSS), DecapiMonitor, TwitchMonitor (4 files)
├── internal/notifications   ~315   ← Manager + Discord webhook (2 files)
├── internal/web           ~6,437   ← chi router, WebSocket hub, auth, middleware, routes (15 files)
├── internal/tui          ~12,893   ← 3-panel layout, 8 overlays, chord system (21 files)
├── internal/goja            ~715   ← JS runtime shims (minimal DOM, TextEncoder, timers) (4 files)
├── internal/disk             ~57   ← Windows disk space queries (kernel32) (1 file)
├── internal/errors          ~231   ← Typed error hierarchy, sentinel codes (1 file)
├── internal/constants       ~392   ← Hardcoded values (API keys, URLs, timeouts) (1 file)
└── internal/utils         ~1,037   ← HTTP helpers, formatters, YouTube URL parsing (13 files)
```

Total: **~48,300 lines** of Go across 125 files (excluding tests, web assets, main.go).

### Key data flow

```
Monitors (RSS/DECAPI/Twitch) → Database jobs → StreamProcessor (probe/wait) →
DownloadOrchestrator → SegmentDownloader (DASH/HLS) + ChatDownloader →
FFmpeg Muxer → Output file + metadata
                  ↑
WebServer + TUI ← Database pub/sub ← WebSocket broadcasts
```

## Critical Patterns

### Logger interface
```go
logger interface {
    Debug(msg string, args ...any)
    Info(msg string, args ...any)
    Warn(msg string, args ...any)
    Error(msg string, args ...any)
}
```
Anonymous interface repeated in every struct — intentional for loose coupling. Do not extract to a named interface.

### Database partial updates
```go
db.UpdateJobFields(jobID, map[string]any{
    "status":   database.StatusDownloading,
    "progress": "V:1234 A:1234 C:5678",
})
```
Dynamically builds SET clauses. Auto-updates `updated_at`. Triggers `OnJobUpdate` subscribers.

### Job status lifecycle
`Upcoming` → `Live` → `Downloading` → `Muxing` → `Finished`
Error paths: any → `Error`, `Cancelled`, or `COOKIES?`

`JobStatus` is `type JobStatus string`. Timestamps are ISO 8601 strings. Optional numerics use pointers. Database schema at **v6** (v5: `segments` table, v6: `client_tokens`).

### Download orchestration
- **StreamProcessor**: Probes status, waits for live, handles auth upgrades. Twitch polls offline channels until live for manual adds
- **DownloadOrchestrator**: Full lifecycle — downloaders + chat + mux + trim. Supports quality-split multi-segment recordings
- **SegmentDownloader**: DASH sequential, HLS playlist polling, VOD parallel, catch-up (6 parallel). `ErrQualityLost` when quality disappears mid-stream
- **QualityMonitor**: Probes every 30s, signals orchestrator to split segments on resolution change
- **Background mux**: `context.Background()` goroutines ensure muxing completes even during cancellation
- **Verification loop**: After download, probes YouTube API to confirm stream ended (up to 6 checks)

### Concurrency model
- **Worker**: 100 lifecycle goroutine slots + configurable download semaphore (default 2)
- **WebSocket**: 100ms leading/trailing edge throttle for broadcasts
- **Database**: 100ms signal-driven coalescing for subscriber callbacks
- **TUI**: Non-blocking channel sends with drop counters
- **BotGuard**: Triple cache — session(6h) → minter(dynamic TTL) → inflight dedup
- **Cipher**: 3-VM LRU keyed by player.js URL, full AST + regex fallback

### YouTube multi-client auth
Innertube fallback chain: WEB, ANDROID, IOS, TV_EMBEDDED, MWEB, WEB_CREATOR. Format priority: resolution > FPS > codec > bitrate > auth level (AndroidVR(0) to WebCreator(5)).

### Security
- **Middleware**: Recovery → RequestLogger → CSRF → Auth → IPAccessControl → RateLimiter → CSP headers
- **CSRF**: Origin/Referer validation. TUI bypasses via `X-Internal-Token` (16-byte random hex, custom `RoundTripper`)
- **Auth**: scrypt password hashing, 24-hour sessions, persistent client tokens
- **Updater**: Ed25519 signature verification before binary swap. Three-step rename dance (`.new` → current → `.old`)

### Panic recovery
Inline `defer func() { if r := recover(); ... }()` in all goroutines. HTTP: `RecoveryMiddleware`. DB callbacks: `safeCallJobUpdate`/`safeCallJobsChange`.

### TUI chord system
`buildMenuItems()` in `app.go` = single source of truth for chords, action menu, hints, and help. `dispatchAction(chord, job)` = unified handler. Adding a chord: one entry in `buildMenuItems()` + one case in `dispatchAction()`.

Prefixes: **A** (Action), **R** (Request), **O** (Open), **Q** (Quit). Single keys: **F** (Filter), **M** (Menu), **`** (Settings), **?** (Help). Confirm chords require a third keypress within 3s.

### Config migrations
`migrateOldFormat()` in `config/config.go` handles backward compat — migrates flat fields into current sections, converts legacy flags. Non-destructive (only applies when new section doesn't exist). Add migration logic for any renamed/relocated fields.

### API route prefix
All REST endpoints use `/api/` (no version). Route registration and frontend fetch calls must stay in sync.

## Web UI

Static assets in `web/public/`, embedded via `go:embed` in `web/embed.go`. Changes require `go build`.

| File | Purpose |
|------|---------|
| `app.js` | Main SPA (jobs, logs, status bar, WebSocket, settings) |
| `modules/player.js` | Video player, niconico-style chat overlay, multi-segment seeking |
| `modules/setup.js` | First-run wizard + FFmpeg install flow |
| `modules/settings.js` | Settings dialog (config, channels, cookies, integrations) |
| `modules/trimmer.js` | Trim clip creation |
| `modules/stats.js` | Statistics dashboard |
| `modules/imports.js` | Zip archive import |
| `modules/utils.js` | Shared formatting helpers |
| `moombox.css` | All styles including mobile responsive |
| `login.html` / `index.html` | Auth page / SPA shell (Shoelace) |

Mobile breakpoints: `992px` (tablet), `768px` (phone), `hover: none` (touch).

## References

The original TypeScript codebase is on `abandoned-nodejs`. The local `references/` folder (gitignored) contains upstream repos:
- **`yt-dlp`** — YouTube format/cipher/extraction, Twitch extractor, PO tokens, cookies
- **`BgUtils`** — BotGuard/PO token generation
- **`ejs`** — yt-dlp external JS for cipher solving
- **`chatterino7`** — Twitch chat (IRC, emotes, badges)
- `bgutil-ytdlp-pot-provider` — yt-dlp PO token plugin
- `moonarchive` — Python stream archiver (segment strategies)
- `moombox` — original Python moombox

Run `bash references/update-all.sh` to pull upstream and see relevant changes. Use `--diff` for verbose diffs.

## Release Process

1. **Generate `RELEASE_NOTES.md`** — `git log --oneline <prev-tag>..HEAD`, group by Features/Improvements/Bug Fixes/Internal (skip empty). No heading.
2. **Bump version** in `cmd/moombox/main.go` (`version = "x.y.z"`).
3. **Commit** both together: `chore: bump version to x.y.z — short summary`.
4. **Tag** (`git tag vx.y.z`) and **push** (`git push && git push origin vx.y.z`).

CI reads `RELEASE_NOTES.md` from the repo.

## Maintaining This File

When updating, check git history for all changes since the last substantive update:
1. `git log --oneline --all -- CLAUDE.md` → when last updated
2. `git log --oneline <last-update>..HEAD` → commits since then
3. Read RELEASE_NOTES.md at each tag for architectural changes
4. Look for: new packages, line count drift, new patterns, schema changes, new capabilities
5. Keep concise — architecture and patterns, not a changelog
