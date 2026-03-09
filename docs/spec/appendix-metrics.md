# Appendix: Project Metrics

> **Last verified:** 2026-03-09
>
> These metrics are volatile — they drift as development continues. Update this file periodically.

## Runtime

- **Go version:** 1.25.5
- **Module path:** github.com/vampiricwulf/Moombox
- **Current app version:** 2.3.20
- **Database schema version:** 6
- **Default port:** 774

## Key Dependencies

| Library | Version | Purpose |
|---------|---------|---------|
| go-chi/chi/v5 | v5.2.5 | HTTP router |
| charmbracelet/bubbletea | v1.3.10 | TUI framework |
| charmbracelet/bubbles | v1.0.0 | TUI components |
| charmbracelet/huh | v0.8.0 | TUI forms |
| charmbracelet/lipgloss | v1.1.0 | TUI styling |
| dop251/goja | v0.0.0-20260219 | JS engine |
| modernc.org/sqlite | v1.46.1 | SQLite driver |
| nhooyr.io/websocket | v1.8.17 | WebSocket |
| BurntSushi/toml | v1.6.0 | Config parsing |

## Package Scale

| Package | Approx Lines | Files | Description |
|---------|-------------|-------|-------------|
| tui/ | ~12,900 | 21 | Largest — 2-over-1 panel layout, overlays, chord system |
| worker/ | ~6,400 | 13 | Download orchestration, queue, quality monitor |
| web/ | ~6,400 | 15 | chi router, WebSocket, auth, middleware, routes |
| twitch/ | ~3,200 | 8 | Twitch GQL API, auth, HLS, IRC chat, emotes |
| engine/ | ~2,700 | 3 | Segment downloader (DASH/HLS/VOD), manifest, muxer |
| cookies/ | ~2,600 | 5 | Cookie jar, refresh, auto-cookie (Firefox/Chromium) |
| youtube/ | ~2,000 | 6 | YouTube service, player API, format selector |
| database/ | ~1,800 | 3 | SQLite/WAL, batch updates, pub/sub |
| cipher/ | ~1,500 | 7 | YouTube signature cipher: AST + regex, 3-VM LRU |
| monitor/ | ~1,500 | 4 | Feed (RSS), DECAPI, Twitch monitors |
| chat/ | ~1,400 | 3 | YouTube live chat downloader (polling + batching) |
| bgutils/ | ~1,400 | 7 | PO token: challenge, BotGuard VM (Goja), mint |
| utils/ | ~1,000 | 13 | HTTP helpers, formatters, YouTube URL parsing |
| config/ | ~800 | 4 | TOML config, FlexDuration, channel terms |
| goja/ | ~700 | 4 | JS runtime shims (minimal DOM, timers, encoding) |
| updater/ | ~450 | 3 | GitHub release checker, self-updater, Ed25519 |
| logger/ | ~450 | 1 | slog wrapper, file rotation, ring buffer, pub/sub |
| constants/ | ~400 | 1 | Hardcoded values (API keys, URLs, timeouts) |
| notifications/ | ~300 | 2 | Manager + Discord webhook |
| errors/ | ~200 | 1 | Typed error hierarchy, sentinel codes |
| disk/ | ~60 | 1 | Windows disk space queries (kernel32) |

### Totals

- **main.go:** ~2,074 lines
- **internal/ packages:** ~48,300 lines across 125 files (excluding tests and web assets)

## Entry Points

- `cmd/moombox/main.go` — Application entry point and launcher/supervisor
- `cmd/sign/main.go` — CI signing tool (Ed25519)
