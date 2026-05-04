# Appendix: Project Metrics

> **Last verified:** 2026-03-19 (post full audit & refactor)
>
> These metrics are volatile — they drift as development continues. Update this file periodically.

## Runtime

- **Go version:** 1.25.5
- **Module path:** github.com/vampiricwulf/Moombox
- **Current app version:** 2.6.10
- **Database schema version:** 13
- **Default port:** 774

## Key Dependencies

| Library | Version | Purpose |
|---------|---------|---------|
| go-chi/chi/v5 | v5.2.5 | HTTP router |
| charm.land/bubbletea/v2 | v2.0.2 | TUI framework |
| charm.land/bubbles/v2 | v2.0.0 | TUI components |
| charm.land/huh/v2 | v2.0.3 | TUI forms |
| charm.land/lipgloss/v2 | v2.0.2 | TUI styling |
| dop251/goja | v0.0.0-20260219 | JS engine |
| modernc.org/sqlite | v1.46.1 | SQLite driver |
| nhooyr.io/websocket | v1.8.17 | WebSocket |
| BurntSushi/toml | v1.6.0 | Config parsing |

## Package Scale

| Package | Approx Lines | Files | Test Files | Description |
|---------|-------------|-------|------------|-------------|
| tui/ | ~13,100 | 33 | 1 | Largest — 2-over-1 panel layout, overlays, chord system |
| worker/ | ~6,500 | 23 | 9 | Download orchestration, queue, quality monitor |
| web/ + routes/ | ~6,600 | 21 | 4 | chi router, WebSocket, auth, middleware, routes |
| twitch/ | ~3,200 | 10 | 3 | Twitch GQL API, auth, HLS, IRC chat, emotes |
| engine/ | ~2,850 | 12 | 3 | Segment downloader (DASH/HLS/VOD), manifest, muxer |
| cookies/ | ~2,700 | 9 | 1 | Cookie jar, refresh, auto-cookie (Firefox/Chromium) |
| youtube/ | ~1,950 | 8 | 2 | YouTube service, player API, format selector |
| database/ | ~1,850 | 7 | 1 | SQLite/WAL, batch updates, pub/sub |
| cipher/ | ~1,500 | 9 | 2 | YouTube signature cipher: AST + regex, 3-VM LRU |
| monitor/ | ~1,450 | 4 | 1 | Feed (RSS), DECAPI, Twitch monitors |
| bgutils/ | ~1,800 | 10 | 3 | PO token: PotProvider, WebPoClient, Challenge, BotGuard, WebPoMinter (sidecar primary, goja fallback) |
| bgutils/sidecar/ | ~700 | 5 | 1 | Node subprocess manager: extract, JSON-RPC mux, Job Object pinning |
| bgutils/embed/ | ~30 | 1 | 0 | go:embed boundary for node-windows-amd64.gz + node-linux-amd64.gz + node-linux-arm64.gz + sidecar.tar.gz + version.txt |
| chat/ | ~1,400 | 3 | 1 | YouTube live chat downloader (polling + batching) |
| utils/ | ~1,150 | 14 | 14 | HTTP helpers, formatters, YouTube URL parsing, JSON |
| config/ | ~850 | 4 | 1 | TOML config, FlexDuration, channel terms |
| goja/ | ~800 | 4 | 2 | JS runtime shims (minimal DOM, timers, encoding) |
| logger/ | ~470 | 1 | 1 | slog wrapper, file rotation, ring buffer, pub/sub |
| updater/ | ~450 | 3 | 2 | GitHub release checker, self-updater, Ed25519 |
| constants/ | ~400 | 1 | 0 | Hardcoded values (API keys, URLs, timeouts) |
| notifications/ | ~330 | 2 | 1 | Manager + Discord webhook |
| errors/ | ~230 | 1 | 1 | Typed error hierarchy, sentinel codes |
| disk/ | ~60 | 2 | 0 | Disk space queries: kernel32 on Windows, statfs on Linux |

### Totals

- **cmd/moombox/:** ~2,170 lines across 5 files (main + adapters + addvideo + helpers + launcher)
- **internal/ packages:** ~49,400 lines across 179 source files
- **Test code:** ~12,600 lines across 51 test files
- **Frontend:** ~10,250 lines across 13 files (~398KB)

## Entry Points

- `cmd/moombox/main.go` — Application entry point and launcher/supervisor
- `cmd/sign/main.go` — CI signing tool (Ed25519)
