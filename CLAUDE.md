# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

Moombox is a YouTube/Twitch live stream archiver written in Go. It monitors channels, detects live streams, downloads segments (DASH/HLS), records live chat, muxes with FFmpeg, and serves a web dashboard + TUI. This is a standalone program — feature work, bug fixes, and improvements are the primary focus.

The Go rewrite from TypeScript is complete (all 12 phases, full file-by-file comparison done). `REWRITE_GUIDE.md` documents the original porting strategy for historical reference.

## TypeScript Reference

The original TypeScript codebase at `D:\Git\Moombox\` serves as a **reference** — not an authoritative source. Consult it when:
- Investigating behavioral gaps or edge cases
- Looking at protocol/API details (YouTube InnerTube, Twitch GQL, BotGuard)
- Needing inspiration for new features

The TS repo also contains a `references/` folder with upstream sources we rely on:
- `moombox` — the original Python moombox (Containerfile, config examples)
- `moonarchive` — Python stream archiver (segment download strategies)
- `yt-dlp` — YouTube format/cipher/extraction logic
- `BgUtils` — BotGuard/PO token generation (challenge + VM)
- `chatterino7` — Twitch chat client (IRC, emotes, badges)
- `bgutil-ytdlp-pot-provider` — yt-dlp plugin for PO tokens
- `ejs` — template engine reference

Key TS → Go mappings (for cross-referencing):
- `src/bgutils/` → `internal/bgutils/`
- `src/engine/` → `internal/engine/` + `internal/chat/`
- `src/core/worker/` → `internal/worker/`
- `src/core/monitors/` + `src/core/monitorUtils.ts` → `internal/monitor/`
- `src/core/` → `internal/cookies/`, `internal/config/`, `internal/youtube/`
- `src/engine/twitch/` → `internal/twitch/`

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

No Makefile, linter config, or CI pipeline exists. Runtime requires FFmpeg on PATH.

## Architecture

Module: `github.com/vampiricwulf/Moombox` — Go 1.25, single binary, all code under `internal/`.

### Service initialization order (cmd/moombox/main.go, ~840 lines)

Config → Logger → Database (SQLite) → CookieJar → YouTube Service → Twitch Service → PotProvider (BotGuard) → CipherSolver → NotificationManager → DownloadWorker (orchestrator + stream processor + job queue) → TrimService → Monitors (Feed/DECAPI/Twitch) → CookieRefresh → AutoCookies → WebServer (chi + WebSocket) → TUI (BubbleTea)

All services are wired together in main.go via callback closures (OnVideoFound, OnStreamFound, OnSchedule) and struct-based dependency injection.

### Package dependency graph

```
cmd/moombox/main.go          ← orchestrates everything
├── internal/config           ← TOML config parsing
├── internal/logger           ← slog wrapper with file rotation, ring buffer, pub/sub
├── internal/database         ← SQLite with WAL, batch updates, pub/sub for job changes
├── internal/cookies          ← jar, refresh service, auto-cookie (CDP browser extraction)
├── internal/youtube          ← Service (PlayerAPI + Auth + watch page + format selector)
├── internal/twitch           ← Service (GQL API + auth + HLS + IRC chat + VOD chat + emotes)
├── internal/bgutils          ← PO token: challenge → BotGuard VM (Goja) → integrity token → mint
├── internal/cipher           ← YouTube signature cipher: extract transforms from player.js, execute via Goja
├── internal/engine           ← SegmentDownloader (DASH/HLS/VOD), manifest parser, FFmpeg muxer
├── internal/chat             ← YouTube live chat downloader (polling API with batching)
├── internal/worker           ← DownloadWorker, StreamProcessor, Orchestrator, JobQueue, TrimService
├── internal/monitor          ← FeedMonitor (RSS), DecapiMonitor, TwitchMonitor
├── internal/notifications    ← Manager + Discord webhook
├── internal/web              ← chi router, WebSocket hub, auth, middleware, embedded SPA
│   └── internal/web/routes   ← All REST API route handlers
├── internal/tui              ← BubbleTea 3-panel layout
├── internal/goja             ← JS runtime shims (minimal DOM, TextEncoder, timers)
├── internal/errors           ← Typed error hierarchy (MoomboxError, YouTubeError, etc.)
├── internal/constants        ← All hardcoded values (API keys, URLs, timeouts)
└── internal/utils            ← HTTP helpers, text/time formatters, YouTube URL parsing
```

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

`JobStatus` is `type JobStatus string`. Timestamps are ISO 8601 strings (not `time.Time`). Optional numeric fields use pointers (`*int`, `*int64`).

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
Used for two things only: BotGuard VM (PO token generation) and YouTube cipher decryption. Package `internal/goja/` provides minimal DOM shims. No CGo or V8 dependency.

### Download orchestration
- **StreamProcessor**: Probes video status, waits for live, handles auth upgrades
- **DownloadOrchestrator**: Manages full lifecycle — segment downloaders + chat + mux + trim
- **SegmentDownloader**: Handles DASH sequential loop, HLS playlist polling, VOD parallel, and catch-up with bounded concurrency (6 parallel)
- **Live stream verification loop**: After downloaders stop, probes YouTube API to confirm stream ended vs still live (up to 6 consecutive checks)

### Monitor → Worker wiring
Monitors fire `OnVideoFound`/`OnStreamFound` callbacks. These are closures set in main.go that create database jobs and enqueue them to the DownloadWorker's JobQueue.

## Web UI

Static assets live in `web/public/` and are embedded via `go:embed` in `web/embed.go`. Changes to CSS/JS/HTML require `go build` to take effect.

- `app.js` — main SPA logic (jobs, logs, status bar, WebSocket, settings dialogs)
- `modules/player.js` — video player with niconico-style chat overlay + sidebar chat
- `modules/utils.js` — shared formatting helpers
- `moombox.css` — all styles including mobile responsive rules
- `login.html` — standalone auth page
- `index.html` — SPA shell with Shoelace UI components

Mobile breakpoints: `992px` (tablet/layout), `768px` (phone/touch), `hover: none` (touch devices).
