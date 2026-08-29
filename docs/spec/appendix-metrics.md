# Appendix: Project Metrics

> **Last verified:** 2026-08-29
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
- **Current app version:** 2.8.5
- **Database schema version:** 19
- **Default port:** 774

## Test Baseline

- **Packages:** 31 in `go list ./...`. `go test -count=1 ./...` reports **27 ok / 0 fail**; the other four have no test files (`cmd/sign`, `internal/bgutils/embed`, `tools/sidecar-sig-probe`, `web`).
- **Browser detection table:** `knownBrowsers` (`internal/cookies/autocookies_detect.go`) has **10 entries** — four Gecko, six Chromium. The full table with type keys is in [data-and-storage.md](data-and-storage.md) § Cookies.

## Key Dependencies

| Library | Version | Purpose |
|---------|---------|---------|
| go-chi/chi/v5 | v5.3.1 | HTTP router |
| charm.land/bubbletea/v2 | v2.0.8 | TUI framework |
| charm.land/bubbles/v2 | v2.1.1 | TUI components |
| charm.land/huh/v2 | v2.0.3 | TUI forms |
| charm.land/lipgloss/v2 | v2.0.6 | TUI styling |
| charm.land/glamour/v2 | v2.0.1 | Markdown rendering (release notes) |
| dop251/goja | v0.0.0-20260806 | JS engine |
| modernc.org/sqlite | v1.56.0 | SQLite driver |
| coder/websocket | v1.8.15 | WebSocket (was nhooyr.io/websocket — upstream moved) |
| BurntSushi/toml | v1.6.0 | Config parsing |

## Package Scale

Source lines exclude `_test.go` files; the test-file count is listed separately.

| Package | Source Lines | Src Files | Test Files | Description |
|---------|-------------|-----------|------------|-------------|
| tui/ | ~18,300 | 38 | 23 | Largest — 2-over-1 panel layout, overlays, chord system |
| worker/ | ~14,100 | 37 | 34 | Download orchestration, strategies, queue, quality monitor |
| cookies/ | ~12,200 | 15 | 52 | Cookie jar, refresh, auto-cookie (Firefox/Chromium), Job Object |
| web/routes/ | ~6,800 | 24 | 30 | REST handlers (jobs, config, stats, output, staging, cookies) |
| engine/ | ~6,400 | 16 | 30 | Segment downloader (DASH/HLS/VOD), manifest, resume, eviction probe |
| youtube/ | ~5,200 | 13 | 13 | YouTube service, player API, format selector, membership tab |
| twitch/ | ~4,300 | 11 | 11 | Twitch GQL API, auth, HLS, IRC chat, VOD chat, emotes |
| monitor/ | ~4,200 | 9 | 10 | Feed (RSS), DECAPI, Twitch monitors, archive scheduling |
| database/ | ~3,800 | 8 | 9 | SQLite/WAL, migrations, batch updates, pub/sub |
| cipher/ | ~3,100 | 13 | 11 | YouTube signature cipher: sidecar-routed + goja fallback |
| web/ | ~2,900 | 7 | 6 | chi router, WebSocket, auth, middleware, embed |
| bgutils/ | ~2,100 | 6 | 5 | PO token: PotProvider, Challenge, BotGuard, WebPoMinter (goja fallback) |
| utils/ | ~1,950 | 17 | 16 | HTTP helpers, formatters, YouTube URL parsing, JSON, DACL |
| config/ | ~1,900 | 6 | 3 | TOML config, FlexDuration, channel terms, migrations |
| chat/ | ~1,850 | 3 | 5 | YouTube live chat downloader (polling + batching) |
| goja/ | ~1,500 | 5 | 11 | JS runtime shims (minimal DOM, timers, encoding) |
| bgutils/sidecar/ | ~1,300 | 5 | 3 | Node subprocess manager: extract, JSON-RPC mux, Job Object pinning |
| cookies/dpapi/ | ~860 | 6 | 6 | Windows DPAPI decryption for browser cookie stores |
| updater/ | ~830 | 3 | 3 | GitHub release checker, self-updater, Ed25519 |
| notifications/ | ~720 | 3 | 3 | Manager + Discord webhook |
| logger/ | ~620 | 1 | 2 | slog wrapper, file rotation, ring buffer, pub/sub |
| connectivity/ | ~470 | 3 | 3 | Reachability monitor; gates stream-end verdicts during outages |
| constants/ | ~350 | 1 | 2 | Hardcoded values (client configs, UAs, URLs) |
| disk/ | ~130 | 3 | 2 | Disk space queries: kernel32 on Windows, statfs on Linux |
| httpx/ | ~110 | 1 | 1 | Shared keep-alive-tuned http.Client/Transport shapes |
| bgutils/embed/ | ~80 | 4 | 0 | go:embed boundary for the Node binaries + sidecar tarball |

### Totals

- **cmd/:** ~6,420 lines across 20 source files (moombox entry/launcher/adapters + sign tool), plus 15 test files
- **internal/ packages:** ~96,200 lines across 258 source files in 26 packages
- **Test code:** ~86,100 lines across 294 test files under `internal/`
- **Frontend:** ~18,100 lines across 15 files (~717 KB) — `app.js`, `index.html`, `moombox.css`, `login.html`, `favicon.svg`, plus 10 ES modules under `web/public/modules/`

## Entry Points

- `cmd/moombox/main.go` — Application entry point and launcher/supervisor
- `cmd/sign/main.go` — CI signing tool (Ed25519)
