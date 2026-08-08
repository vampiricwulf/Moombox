# Appendix: Project Metrics

> **Last verified:** 2026-08-07
>
> These metrics are volatile — they drift as development continues. Update this file periodically.
>
> Regenerate the scale tables with:
> ```bash
> # per-package source lines / file counts
> for d in $(find internal -type d); do
>   printf "%-32s %6s lines %3s src %3s test\n" "$d" \
>     "$(find "$d" -maxdepth 1 -name '*.go' ! -name '*_test.go' -exec cat {} + 2>/dev/null | wc -l)" \
>     "$(find "$d" -maxdepth 1 -name '*.go' ! -name '*_test.go' | wc -l)" \
>     "$(find "$d" -maxdepth 1 -name '*_test.go' | wc -l)"
> done | sort -k2 -rn
> ```

## Runtime

- **Go version:** 1.26
- **Module path:** github.com/vampiricwulf/Moombox
- **Current app version:** 2.7.7
- **Database schema version:** 17
- **Default port:** 774

## Key Dependencies

| Library | Version | Purpose |
|---------|---------|---------|
| go-chi/chi/v5 | v5.3.1 | HTTP router |
| charm.land/bubbletea/v2 | v2.0.8 | TUI framework |
| charm.land/bubbles/v2 | v2.1.1 | TUI components |
| charm.land/huh/v2 | v2.0.3 | TUI forms |
| charm.land/lipgloss/v2 | v2.0.5 | TUI styling |
| dop251/goja | v0.0.0-20260701 | JS engine |
| modernc.org/sqlite | v1.55.0 | SQLite driver |
| coder/websocket | v1.8.15 | WebSocket (was nhooyr.io/websocket — upstream moved) |
| BurntSushi/toml | v1.6.0 | Config parsing |

## Package Scale

Source lines exclude `_test.go` files; the test-file count is listed separately.

| Package | Source Lines | Src Files | Test Files | Description |
|---------|-------------|-----------|------------|-------------|
| tui/ | ~17,200 | 38 | 8 | Largest — 2-over-1 panel layout, overlays, chord system |
| worker/ | ~11,500 | 33 | 25 | Download orchestration, strategies, queue, quality monitor |
| web/routes/ | ~6,300 | 24 | 17 | REST handlers (jobs, config, stats, output, staging) |
| engine/ | ~5,200 | 16 | 24 | Segment downloader (DASH/HLS/VOD), manifest, resume, eviction probe |
| cookies/ | ~4,200 | 14 | 9 | Cookie jar, refresh, auto-cookie (Firefox/Chromium) |
| monitor/ | ~4,200 | 9 | 10 | Feed (RSS), DECAPI, Twitch monitors, archive scheduling |
| youtube/ | ~3,800 | 10 | 6 | YouTube service, player API, format selector |
| twitch/ | ~3,700 | 11 | 7 | Twitch GQL API, auth, HLS, IRC chat, emotes |
| database/ | ~3,700 | 8 | 7 | SQLite/WAL, migrations, batch updates, pub/sub |
| cipher/ | ~3,100 | 13 | 11 | YouTube signature cipher: sidecar-routed + goja fallback |
| web/ | ~2,700 | 7 | 6 | chi router, WebSocket, auth, middleware, embed |
| bgutils/ | ~1,900 | 6 | 5 | PO token: PotProvider, Challenge, BotGuard, WebPoMinter (goja fallback) |
| config/ | ~1,800 | 5 | 2 | TOML config, FlexDuration, channel terms, migrations |
| chat/ | ~1,700 | 3 | 3 | YouTube live chat downloader (polling + batching) |
| utils/ | ~1,600 | 16 | 15 | HTTP helpers, formatters, YouTube URL parsing, JSON |
| goja/ | ~1,500 | 5 | 11 | JS runtime shims (minimal DOM, timers, encoding) |
| bgutils/sidecar/ | ~1,200 | 5 | 2 | Node subprocess manager: extract, JSON-RPC mux, Job Object pinning |
| updater/ | ~830 | 3 | 3 | GitHub release checker, self-updater, Ed25519 |
| notifications/ | ~720 | 3 | 3 | Manager + Discord webhook |
| logger/ | ~620 | 1 | 2 | slog wrapper, file rotation, ring buffer, pub/sub |
| cookies/dpapi/ | ~510 | 6 | 3 | Windows DPAPI decryption for browser cookie stores |
| connectivity/ | ~470 | 3 | 3 | Reachability monitor; gates stream-end verdicts during outages |
| constants/ | ~310 | 1 | 1 | Hardcoded values (client configs, UAs, URLs) |
| disk/ | ~130 | 3 | 2 | Disk space queries: kernel32 on Windows, statfs on Linux |
| httpx/ | ~110 | 1 | 1 | Shared keep-alive-tuned http.Client/Transport shapes |
| bgutils/embed/ | ~80 | 4 | 0 | go:embed boundary for the Node binaries + sidecar tarball |

### Totals

- **cmd/:** ~5,270 lines across 20 files (moombox entry/launcher/adapters + sign tool)
- **internal/ packages:** ~79,100 lines across 248 source files
- **Test code:** ~45,100 lines across 186 test files
- **Frontend:** ~16,900 lines across 14 files (~651 KB) — `app.js`, `index.html`, `moombox.css`, `login.html`, plus 10 ES modules under `web/public/modules/`

## Entry Points

- `cmd/moombox/main.go` — Application entry point and launcher/supervisor
- `cmd/sign/main.go` — CI signing tool (Ed25519)
