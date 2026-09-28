# Appendix: Project Metrics

> **Last verified:** 2026-09-25
>
> No package was added or removed since the 2026-09-19 pass — the counts moved inside packages that already existed. The four new production files are `internal/engine/reorder_budget.go` (the process-wide reorder budget), `internal/utils/resolution.go` (`CapDimension`/`SelectByCap`, ruling R1), and the `internal/web/openpath_{windows,other}.go` pair (the shared folder-open composer both UIs call).
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
- **Database schema version:** 20
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
| dop251/goja | v0.0.0-20260915 | JS engine |
| modernc.org/sqlite | v1.59.0 | SQLite driver |
| coder/websocket | v1.8.15 | WebSocket (was nhooyr.io/websocket — upstream moved) |
| BurntSushi/toml | v1.6.0 | Config parsing |

## Package Scale

Source lines exclude `_test.go` files; the test-file count is listed separately. Line counts are rounded to the nearest 10. `internal/docs` is excluded from the Package Scale rows and the internal/ Totals because it carries no production code — its only non-test file is `doc.go`, a 5-line package comment that exists so the spec-citation test beside it is reachable by `go test ./...`.

| Package | Source Lines | Src Files | Test Files | Description |
|---------|-------------|-----------|------------|-------------|
| tui/ | ~22,290 | 43 | 63 | Largest — 2-over-1 panel layout, overlays, chord system |
| worker/ | ~17,320 | 38 | 54 | Download orchestration, strategies, queue, quality monitor |
| cookies/ | ~15,870 | 35 | 80 | Cookie jar, refresh, auto-cookie (Firefox/Chromium), Job Object |
| engine/ | ~8,040 | 20 | 42 | Segment downloader (DASH/HLS/VOD), manifest, resume, eviction probe, reorder budget |
| web/routes/ | ~7,570 | 25 | 51 | REST handlers (jobs, config, stats, output, staging, cookies) |
| youtube/ | ~6,470 | 13 | 17 | YouTube service, player API, format selector, membership tab |
| twitch/ | ~6,410 | 14 | 34 | Twitch GQL API, auth, HLS, IRC chat, VOD chat, emotes |
| monitor/ | ~5,030 | 9 | 11 | Feed (RSS), DECAPI, Twitch monitors, archive scheduling |
| database/ | ~3,970 | 8 | 11 | SQLite/WAL, migrations, batch updates, pub/sub |
| web/ | ~3,950 | 9 | 16 | chi router, WebSocket, auth, middleware, embed, folder-open composer |
| cipher/ | ~3,110 | 13 | 11 | YouTube signature cipher: sidecar-routed + goja fallback |
| chat/ | ~3,030 | 3 | 16 | YouTube live chat downloader (polling + batching) |
| utils/ | ~2,590 | 26 | 24 | HTTP helpers, formatters, YouTube URL parsing, JSON, DACL, resolution cap |
| config/ | ~2,250 | 6 | 6 | TOML config, FlexDuration, channel terms, migrations |
| bgutils/ | ~2,050 | 6 | 5 | PO token: PotProvider, Challenge, BotGuard, WebPoMinter (goja fallback) |
| bgutils/sidecar/ | ~1,840 | 7 | 5 | Node subprocess manager: extract, JSON-RPC mux, Job Object pinning |
| goja/ | ~1,470 | 5 | 11 | JS runtime shims (minimal DOM, timers, encoding) |
| cookies/dpapi/ | ~1,100 | 6 | 7 | Windows DPAPI decryption for browser cookie stores |
| updater/ | ~870 | 3 | 3 | GitHub release checker, self-updater, Ed25519 |
| notifications/ | ~820 | 3 | 3 | Manager + Discord webhook |
| logger/ | ~760 | 1 | 2 | slog wrapper, file rotation, ring buffer, pub/sub |
| connectivity/ | ~480 | 3 | 3 | Reachability monitor; gates stream-end verdicts during outages |
| jobfilter/ | ~470 | 2 | 3 | Dashboard filter language (Go twin of `filter-parser.js`/`filter-engine.js`), used by the TUI's `/` filter box |
| ytdlpplugin/ | ~330 | 1 | 1 | yt-dlp PO-token plugin status/install — shared by the dashboard's Integrations card and the TUI's E Y overlay |
| constants/ | ~320 | 1 | 2 | Hardcoded values (client configs, UAs, URLs) |
| disk/ | ~130 | 3 | 2 | Disk space queries: kernel32 on Windows, statfs on Linux |
| httpx/ | ~110 | 1 | 1 | Shared keep-alive-tuned http.Client/Transport shapes |
| bgutils/embed/ | ~80 | 4 | 0 | go:embed boundary for the Node binaries + sidecar tarball |
| stats/ | ~70 | 1 | 1 | Figures shared by the Web Stats tab and the TUI's E T overlay — job aggregates, disk reading |
| webtest/ | ~70 | 1 | 1 | Shared goja harness for evaluating shipped Web UI JS (`settings.js`) from Go tests |

### Totals

- **cmd/:** ~8,200 lines across 22 source files (moombox entry/launcher/adapters + sign tool), plus 46 test files (~7,670 lines)
- **internal/ packages:** ~118,890 lines across 310 source files in 30 packages
- **Test code:** ~146,310 lines across 486 test files under `internal/`
- **Frontend:** ~21,340 lines across 27 files (~944 KB) — `app.js`, `boot-theme.js`, `favicon.svg`, `index.html`, `login.html`, `login.js`, `moombox.css`, plus 20 ES modules under `web/public/modules/` (`chat-timeline.js`, `files.js`, `filter-bar.js`, `filter-engine.js`, `filter-parser.js`, `imports.js`, `job-details.js`, `log-panel.js`, `logout.js`, `nico-geometry.js`, `nico-lanes.js`, `nico-scheduler.js`, `player.js`, `segments.js`, `settings.js`, `setup.js`, `stats.js`, `trimmer.js`, `update-indicator.js`, `utils.js`)

## Entry Points

- `cmd/moombox/main.go` — Application entry point and launcher/supervisor
- `cmd/sign/main.go` — CI signing tool (Ed25519)
