# Appendix: Project Metrics

> **Last verified:** 2026-10-10
>
> The Package Scale table and the Totals were regenerated on 2026-10-10: the table with the script below, the Totals by the same count (`wc -l` per file) over the tracked files (`git ls-files`; `web/public/` less `vendor/`). Since the 2026-10-04 pass two packages were added — `internal/redact` and `internal/sqliteuri` — and none removed; `internal/worker` grew from 39 to 53 source files (among them the disk-full admission gate, channel removal, backlog retry, the VOD supersede and slot wait, the Twitch end-unconfirmed park and the output claims), `internal/updater` gained the signed release manifest, `internal/logger` (1 to 3), `internal/disk` (3 to 5), `internal/cookies` (35 to 37), `internal/tui` (43 to 45) and `internal/web/routes` (25 to 26) gained files, and `internal/utils` and `internal/twitch` each lost one. The Runtime, Test Baseline (the package count and the two packages without tests) and Key Dependencies sections were checked against the tree the same day.
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
- **Current app version:** 2.9.0
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
| tui/ | ~25,430 | 45 | 126 | Largest — 2-over-1 panel layout, overlays, chord system |
| worker/ | ~23,110 | 53 | 123 | Download orchestration, strategies, queue, quality monitor, disk-full gate, channel removal |
| cookies/ | ~16,370 | 37 | 91 | Cookie jar, refresh, auto-cookie (Firefox/Chromium), Job Object |
| web/routes/ | ~9,470 | 26 | 79 | REST handlers (jobs, config, stats, output, staging, cookies, import) |
| engine/ | ~8,670 | 20 | 53 | Segment downloader (DASH/HLS/VOD), manifest, resume, eviction probe, reorder budget |
| twitch/ | ~7,470 | 13 | 45 | Twitch GQL API, auth, HLS, IRC chat, VOD chat, emotes |
| youtube/ | ~6,480 | 13 | 19 | YouTube service, player API, format selector, membership tab |
| monitor/ | ~5,440 | 9 | 15 | Feed (RSS), DECAPI, Twitch monitors, archive scheduling, backfill |
| notifications/ | ~4,720 | 11 | 27 | Manager + Discord webhook, batching, edit-mode message ids |
| database/ | ~4,650 | 8 | 20 | SQLite/WAL, migrations, synchronous writes, pub/sub |
| web/ | ~4,610 | 9 | 24 | chi router, WebSocket, auth, middleware, embed, folder-open composer |
| chat/ | ~3,290 | 3 | 23 | YouTube live chat downloader (polling + batching) |
| cipher/ | ~3,190 | 13 | 12 | YouTube signature cipher: sidecar-routed + goja fallback |
| config/ | ~2,920 | 7 | 15 | TOML config, FlexDuration, channel terms, migrations |
| utils/ | ~2,910 | 25 | 28 | HTTP helpers, formatters, YouTube URL parsing, JSON, DACL, resolution cap, canonical paths |
| bgutils/ | ~2,180 | 6 | 11 | PO token: PotProvider, Challenge, BotGuard, WebPoMinter (goja path) |
| bgutils/sidecar/ | ~2,100 | 7 | 13 | Node subprocess manager: extract, JSON-RPC mux, Job Object pinning |
| goja/ | ~1,510 | 5 | 12 | JS runtime shims (minimal DOM, timers, encoding) |
| updater/ | ~1,370 | 4 | 7 | GitHub release checker, self-updater, Ed25519, signed release manifest |
| cookies/dpapi/ | ~1,110 | 6 | 8 | Windows DPAPI decryption for browser cookie stores |
| logger/ | ~970 | 3 | 3 | slog wrapper, file rotation, ring buffer, pub/sub |
| redact/ | ~540 | 4 | 3 | One redaction rule per kind of secret for error text, log lines and notifications — a media URL's credentials (a googlevideo URL's client IP, signatures and GVS PO token in both its URL forms; the playback session a Twitch weaver playlist's or edge segment's path spells), a credential inside a URL |
| jobfilter/ | ~500 | 2 | 4 | Dashboard filter language (Go twin of `filter-parser.js`/`filter-engine.js`), used by the TUI's `/` filter box |
| connectivity/ | ~500 | 3 | 3 | Reachability monitor; gates stream-end verdicts during outages |
| ytdlpplugin/ | ~350 | 1 | 1 | yt-dlp PO-token plugin status/install — shared by the dashboard's Integrations card and the TUI's E Y overlay |
| constants/ | ~310 | 1 | 2 | Hardcoded values (client configs, UAs, URLs) |
| disk/ | ~200 | 5 | 4 | Disk space queries: kernel32 on Windows, statfs on Linux |
| notifications/notificationtest/ | ~180 | 1 | 1 | Shared fakes for the notification manager's tests |
| httpx/ | ~110 | 1 | 1 | Shared keep-alive-tuned http.Client/Transport shapes |
| bgutils/embed/ | ~80 | 4 | 1 | go:embed boundary for the Node binaries + sidecar tarball |
| stats/ | ~80 | 1 | 1 | Figures shared by the Web Stats tab and the TUI's E T overlay — job aggregates, disk reading |
| webtest/ | ~70 | 1 | 1 | Shared goja harness for evaluating shipped Web UI JS (`settings.js`) from Go tests |
| sqliteuri/ | ~30 | 1 | 1 | The `file:` URI every SQLite open goes through — the job database and the browsers' cookie databases |

### Totals

- **cmd/:** ~10,390 lines across 27 source files (26 in `cmd/moombox` — entry/launcher/adapters/wiring — plus the sign tool), plus 85 test files (~12,680 lines)
- **internal/ packages:** ~140,890 lines across 348 source files in 33 packages
- **Test code:** ~204,490 lines across 779 test files under `internal/` (`internal/docs`'s two included)
- **Frontend:** ~23,440 lines across 27 files (~980 KB) — `app.js`, `boot-theme.js`, `favicon.svg`, `index.html`, `login.html`, `login.js`, `moombox.css`, plus 20 ES modules under `web/public/modules/` (`chat-timeline.js`, `files.js`, `filter-bar.js`, `filter-engine.js`, `filter-parser.js`, `imports.js`, `job-details.js`, `log-panel.js`, `logout.js`, `nico-geometry.js`, `nico-lanes.js`, `nico-scheduler.js`, `player.js`, `segments.js`, `settings.js`, `setup.js`, `stats.js`, `trimmer.js`, `update-indicator.js`, `utils.js`)

## Entry Points

- `cmd/moombox/main.go` — Application entry point and launcher/supervisor
- `cmd/sign/main.go` — CI signing tool (Ed25519)
