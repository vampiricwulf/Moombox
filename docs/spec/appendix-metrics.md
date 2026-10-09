# Appendix: Project Metrics

> **Last verified:** 2026-10-04
>
> The scale tables were regenerated on 2026-10-04 with the script below. Since the 2026-09-25 pass `internal/notifications` grew from 3 to 11 source files and gained the `notificationtest` helper package (shared fakes for its tests), and `internal/worker` and `internal/config` each gained a file; no package was removed.
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
- **Current app version:** 2.8.10
- **Database schema version:** 21
- **Default port:** 774

## Test Baseline

- **Packages:** 39 in `go list ./...`. `go test -count=1 ./...` reports **37 ok / 0 fail**; the other two have no test files (`tools/sidecar-sig-probe`, `web`).
- **Browser detection table:** `knownBrowsers` (`internal/cookies/autocookies_detect.go`) has **10 entries** — four Gecko, six Chromium. The full table with type keys is in [data-and-storage.md](data-and-storage.md) § Cookies.

## Key Dependencies

| Library | Version | Purpose |
|---------|---------|---------|
| go-chi/chi/v5 | v5.3.2 | HTTP router |
| charm.land/bubbletea/v2 | v2.0.10 | TUI framework |
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
| tui/ | ~22,710 | 43 | 77 | Largest — 2-over-1 panel layout, overlays, chord system |
| worker/ | ~18,070 | 39 | 64 | Download orchestration, strategies, queue, quality monitor |
| cookies/ | ~15,870 | 35 | 80 | Cookie jar, refresh, auto-cookie (Firefox/Chromium), Job Object |
| engine/ | ~8,170 | 20 | 43 | Segment downloader (DASH/HLS/VOD), manifest, resume, eviction probe, reorder budget |
| web/routes/ | ~7,790 | 25 | 54 | REST handlers (jobs, config, stats, output, staging, cookies) |
| youtube/ | ~6,710 | 13 | 19 | YouTube service, player API, format selector, membership tab |
| twitch/ | ~6,410 | 14 | 34 | Twitch GQL API, auth, HLS, IRC chat, VOD chat, emotes |
| monitor/ | ~5,090 | 9 | 11 | Feed (RSS), DECAPI, Twitch monitors, archive scheduling |
| database/ | ~4,110 | 8 | 12 | SQLite/WAL, migrations, synchronous writes, pub/sub |
| web/ | ~3,950 | 9 | 16 | chi router, WebSocket, auth, middleware, embed, folder-open composer |
| notifications/ | ~3,770 | 11 | 17 | Manager + Discord webhook, batching, edit-mode message ids |
| cipher/ | ~3,110 | 13 | 11 | YouTube signature cipher: sidecar-routed + goja fallback |
| chat/ | ~3,030 | 3 | 16 | YouTube live chat downloader (polling + batching) |
| utils/ | ~2,590 | 26 | 24 | HTTP helpers, formatters, YouTube URL parsing, JSON, DACL, resolution cap |
| config/ | ~2,490 | 7 | 8 | TOML config, FlexDuration, channel terms, migrations |
| bgutils/ | ~2,050 | 6 | 5 | PO token: PotProvider, Challenge, BotGuard, WebPoMinter (goja path) |
| bgutils/sidecar/ | ~1,880 | 7 | 6 | Node subprocess manager: extract, JSON-RPC mux, Job Object pinning |
| goja/ | ~1,470 | 5 | 11 | JS runtime shims (minimal DOM, timers, encoding) |
| cookies/dpapi/ | ~1,100 | 6 | 7 | Windows DPAPI decryption for browser cookie stores |
| updater/ | ~870 | 3 | 3 | GitHub release checker, self-updater, Ed25519 |
| logger/ | ~760 | 1 | 2 | slog wrapper, file rotation, ring buffer, pub/sub |
| connectivity/ | ~480 | 3 | 3 | Reachability monitor; gates stream-end verdicts during outages |
| jobfilter/ | ~470 | 2 | 3 | Dashboard filter language (Go twin of `filter-parser.js`/`filter-engine.js`), used by the TUI's `/` filter box |
| ytdlpplugin/ | ~330 | 1 | 1 | yt-dlp PO-token plugin status/install — shared by the dashboard's Integrations card and the TUI's E Y overlay |
| constants/ | ~320 | 1 | 2 | Hardcoded values (client configs, UAs, URLs) |
| notifications/notificationtest/ | ~140 | 1 | 1 | Shared fakes for the notification manager's tests |
| disk/ | ~130 | 3 | 2 | Disk space queries: kernel32 on Windows, statfs on Linux |
| httpx/ | ~110 | 1 | 1 | Shared keep-alive-tuned http.Client/Transport shapes |
| sqliteuri/ | ~30 | 1 | 1 | The `file:` URI every SQLite open goes through — the job database and the browsers' cookie databases |
| redact/ | ~280 | 3 | 2 | One redaction rule per kind of secret for error text, log lines and notifications — the GVS PO token in both its URL forms, a credential inside a URL |
| bgutils/embed/ | ~80 | 4 | 1 | go:embed boundary for the Node binaries + sidecar tarball |
| stats/ | ~70 | 1 | 1 | Figures shared by the Web Stats tab and the TUI's E T overlay — job aggregates, disk reading |
| webtest/ | ~70 | 1 | 1 | Shared goja harness for evaluating shipped Web UI JS (`settings.js`) from Go tests |

### Totals

- **cmd/:** ~8,670 lines across 25 source files (24 in `cmd/moombox` — entry/launcher/adapters/wiring — plus the sign tool), plus 51 test files (~8,480 lines)
- **internal/ packages:** ~124,500 lines across 325 source files in 33 packages
- **Test code:** ~160,780 lines across 541 test files under `internal/`
- **Frontend:** ~22,110 lines across 27 files (~970 KB) — `app.js`, `boot-theme.js`, `favicon.svg`, `index.html`, `login.html`, `login.js`, `moombox.css`, plus 20 ES modules under `web/public/modules/` (`chat-timeline.js`, `files.js`, `filter-bar.js`, `filter-engine.js`, `filter-parser.js`, `imports.js`, `job-details.js`, `log-panel.js`, `logout.js`, `nico-geometry.js`, `nico-lanes.js`, `nico-scheduler.js`, `player.js`, `segments.js`, `settings.js`, `setup.js`, `stats.js`, `trimmer.js`, `update-indicator.js`, `utils.js`)

## Entry Points

- `cmd/moombox/main.go` — Application entry point and launcher/supervisor
- `cmd/sign/main.go` — CI signing tool (Ed25519)
