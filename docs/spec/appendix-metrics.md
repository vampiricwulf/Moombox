# Appendix: Project Metrics

> **Last verified:** 2026-09-05
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

- **Go version:** 1.27 (`toolchain go1.27.1`)
- **Module path:** github.com/vampiricwulf/Moombox
- **Current app version:** 2.8.8
- **Database schema version:** 19
- **Default port:** 774

## Test Baseline

- **Packages:** 36 in `go list ./...`. `go test -count=1 ./...` reports **32 ok / 0 fail**; the other four have no test files (`cmd/sign`, `internal/bgutils/embed`, `tools/sidecar-sig-probe`, `web`).
- **Browser detection table:** `knownBrowsers` (`internal/cookies/autocookies_detect.go`) has **10 entries** — four Gecko, six Chromium. The full table with type keys is in [data-and-storage.md](data-and-storage.md) § Cookies.

## Key Dependencies

| Library | Version | Purpose |
|---------|---------|---------|
| go-chi/chi/v5 | v5.3.2 | HTTP router |
| charm.land/bubbletea/v2 | v2.0.9 | TUI framework |
| charm.land/bubbles/v2 | v2.2.1 | TUI components |
| charm.land/huh/v2 | v2.0.3 | TUI forms |
| charm.land/lipgloss/v2 | v2.0.6 | TUI styling |
| charm.land/glamour/v2 | v2.0.1 | Markdown rendering (release notes) |
| dop251/goja | v0.0.0-20260903 | JS engine |
| modernc.org/sqlite | v1.58.0 | SQLite driver |
| coder/websocket | v1.8.15 | WebSocket (was nhooyr.io/websocket — upstream moved) |
| BurntSushi/toml | v1.6.0 | Config parsing |

## Package Scale

Source lines exclude `_test.go` files; the test-file count is listed separately. Line counts are rounded to the nearest 10. `internal/docs` is excluded from the Package Scale rows and the internal/ Totals because it carries no production code — its only non-test file is `doc.go`, a 5-line package comment that exists so the spec-citation test beside it is reachable by `go test ./...`.

| Package | Source Lines | Src Files | Test Files | Description |
|---------|-------------|-----------|------------|-------------|
| tui/ | ~20,450 | 41 | 39 | Largest — 2-over-1 panel layout, overlays, chord system |
| cookies/ | ~15,060 | 35 | 71 | Cookie jar, refresh, auto-cookie (Firefox/Chromium), Job Object |
| worker/ | ~14,730 | 38 | 40 | Download orchestration, strategies, queue, quality monitor |
| web/routes/ | ~7,070 | 24 | 40 | REST handlers (jobs, config, stats, output, staging, cookies) |
| engine/ | ~6,510 | 17 | 31 | Segment downloader (DASH/HLS/VOD), manifest, resume, eviction probe |
| twitch/ | ~5,300 | 13 | 21 | Twitch GQL API, auth, HLS, IRC chat, VOD chat, emotes |
| youtube/ | ~5,150 | 13 | 13 | YouTube service, player API, format selector, membership tab |
| monitor/ | ~4,210 | 9 | 10 | Feed (RSS), DECAPI, Twitch monitors, archive scheduling |
| database/ | ~3,850 | 8 | 9 | SQLite/WAL, migrations, batch updates, pub/sub |
| cipher/ | ~3,080 | 13 | 11 | YouTube signature cipher: sidecar-routed + goja fallback |
| web/ | ~2,950 | 7 | 7 | chi router, WebSocket, auth, middleware, embed |
| chat/ | ~2,250 | 3 | 8 | YouTube live chat downloader (polling + batching) |
| bgutils/ | ~2,080 | 6 | 5 | PO token: PotProvider, Challenge, BotGuard, WebPoMinter (goja fallback) |
| config/ | ~2,010 | 6 | 3 | TOML config, FlexDuration, channel terms, migrations |
| utils/ | ~1,960 | 17 | 16 | HTTP helpers, formatters, YouTube URL parsing, JSON, DACL |
| goja/ | ~1,470 | 5 | 11 | JS runtime shims (minimal DOM, timers, encoding) |
| bgutils/sidecar/ | ~1,260 | 5 | 3 | Node subprocess manager: extract, JSON-RPC mux, Job Object pinning |
| updater/ | ~870 | 3 | 3 | GitHub release checker, self-updater, Ed25519 |
| cookies/dpapi/ | ~860 | 6 | 6 | Windows DPAPI decryption for browser cookie stores |
| notifications/ | ~730 | 3 | 3 | Manager + Discord webhook |
| logger/ | ~620 | 1 | 2 | slog wrapper, file rotation, ring buffer, pub/sub |
| connectivity/ | ~480 | 3 | 3 | Reachability monitor; gates stream-end verdicts during outages |
| constants/ | ~350 | 1 | 2 | Hardcoded values (client configs, UAs, URLs) |
| ytdlpplugin/ | ~320 | 1 | 1 | yt-dlp PO-token plugin status/install — shared by the dashboard's Integrations card and the TUI's R Y overlay |
| jobfilter/ | ~270 | 1 | 1 | Dashboard filter language (Go twin of `filter-parser.js`/`filter-engine.js`), used by the TUI's `/` filter box |
| disk/ | ~130 | 3 | 2 | Disk space queries: kernel32 on Windows, statfs on Linux |
| httpx/ | ~110 | 1 | 1 | Shared keep-alive-tuned http.Client/Transport shapes |
| bgutils/embed/ | ~80 | 4 | 0 | go:embed boundary for the Node binaries + sidecar tarball |
| stats/ | ~70 | 1 | 1 | Figures shared by the Web Stats tab and the TUI's R T overlay — job aggregates, disk reading |
| webtest/ | ~70 | 1 | 1 | Shared goja harness for evaluating shipped Web UI JS (`settings.js`) from Go tests |

### Totals

- **cmd/:** ~7,180 lines across 21 source files (moombox entry/launcher/adapters + sign tool), plus 24 test files
- **internal/ packages:** ~104,330 lines across 289 source files in 30 packages
- **Test code:** ~105,070 lines across 364 test files under `internal/`
- **Frontend:** ~20,110 lines across 25 files (~812 KB) — `app.js`, `index.html`, `moombox.css`, `login.html`, `favicon.svg`, plus 20 ES modules under `web/public/modules/` (`chat-timeline.js`, `files.js`, `filter-bar.js`, `filter-engine.js`, `filter-parser.js`, `imports.js`, `job-details.js`, `log-panel.js`, `logout.js`, `nico-geometry.js`, `nico-lanes.js`, `nico-scheduler.js`, `player.js`, `segments.js`, `settings.js`, `setup.js`, `stats.js`, `trimmer.js`, `update-indicator.js`, `utils.js`)

## Entry Points

- `cmd/moombox/main.go` — Application entry point and launcher/supervisor
- `cmd/sign/main.go` — CI signing tool (Ed25519)
