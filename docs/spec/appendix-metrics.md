# Appendix: Project Metrics

> **Last verified:** 2026-11-09
>
> The Package Scale table and the Totals were regenerated on 2026-10-16, after Wave 30 was integrated: the table with the script below, the Totals by the same count (`wc -l` per file) over the tracked files (`git ls-files`; `web/public/` less `vendor/`). Since the 2026-10-10 pass two packages were added — `internal/treeguard`, the test-only guard every tested package's `TestMain` runs through, and `internal/childproc`, which starts every FFmpeg and ffprobe tied to the process that runs it — and none removed. `internal/engine` (20 to 23), `internal/worker` (to 56), `internal/notifications` (to 13) and `internal/connectivity` (3 to 4) gained source files this round, and `cmd/moombox` 6 (26 to 32: the stop signals, the crash output capture, the daemon record, the outage alerts). The Runtime, Test Baseline (the package count and the two packages without tests) and Key Dependencies sections were checked against the tree the same day. On 2026-10-30 `internal/httpx/httpxtest` was added, and on 2026-11-09 `internal/jobverbs` and `internal/jobstate`, each with its row and its share of the Totals and of the Test Baseline's package count. Every row and the Totals were regenerated again on 2026-11-09, and again on 2026-11-10 after the improvement sweep, whose file splits account for most of the new source files (`internal/worker` 79 to 83, three of them `worker_verbs.go`, `worker_failure.go` and `worker_staging_claims.go`; `internal/tui` 49 to 70; `cmd/moombox` 36 to 39).
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

- **Go version:** 1.27 (`toolchain go1.27.3`)
- **Module path:** github.com/vampiricwulf/Moombox
- **Database schema version:** 27
- **Default port:** 774

## Test Baseline

- **Packages:** 44 in `go list ./...`. `go test -count=1 ./...` reports **42 ok / 0 fail**; the other two have no test files (`tools/sidecar-sig-probe`, `web`).
- **Browser detection table:** `knownBrowsers` (`internal/cookies/autocookies_detect.go`) has **10 entries** — four Gecko, six Chromium. The full table with type keys is in [data-and-storage.md](data-and-storage.md) § Cookies.

## Key Dependencies

| Library | Version | Purpose |
|---------|---------|---------|
| go-chi/chi/v5 | v5.5.0 | HTTP router |
| charm.land/bubbletea/v2 | v2.1.0 | TUI framework |
| charm.land/bubbles/v2 | v2.2.1 | TUI components |
| charm.land/huh/v2 | v2.0.3 | TUI forms |
| charm.land/lipgloss/v2 | v2.0.6 | TUI styling |
| charm.land/glamour/v2 | v2.0.1 | Markdown rendering (release notes) |
| dop251/goja | v0.0.0-20260915 | JS engine |
| modernc.org/sqlite | v1.61.0 | SQLite driver |
| coder/websocket | v1.8.15 | WebSocket (was nhooyr.io/websocket — upstream moved) |
| BurntSushi/toml | v1.6.0 | Config parsing |

## Package Scale

Source lines exclude `_test.go` files; the test-file count is listed separately. Line counts are rounded to the nearest 10. `internal/docs` is excluded from the Package Scale rows and the internal/ Totals because it carries no production code — its only non-test file is `doc.go`, a 5-line package comment that exists so the spec-citation test beside it is reachable by `go test ./...`.

| Package | Source Lines | Src Files | Test Files | Description |
|---------|-------------|-----------|------------|-------------|
| worker/ | ~37,890 | 83 | 221 | Download orchestration, strategies, queue, quality monitor, disk-full gate, channel removal |
| tui/ | ~33,810 | 70 | 173 | 2-over-1 panel layout, overlays, chord system |
| cookies/ | ~17,400 | 39 | 99 | Cookie jar, refresh, auto-cookie (Firefox/Chromium), Job Object |
| engine/ | ~12,720 | 29 | 83 | Segment downloader (DASH/HLS/VOD), manifest, resume, eviction probe, reorder budget |
| web/routes/ | ~11,840 | 31 | 106 | REST handlers (jobs, config, stats, output, staging, cookies, import) |
| youtube/ | ~10,570 | 21 | 45 | YouTube service, player API, format selector, membership tab |
| twitch/ | ~9,830 | 16 | 59 | Twitch GQL API, auth, HLS, IRC chat, VOD chat, emotes |
| monitor/ | ~6,670 | 13 | 29 | Feed (RSS), DECAPI, Twitch monitors, archive scheduling, backfill |
| notifications/ | ~5,810 | 13 | 31 | Manager + Discord webhook, batching, edit-mode message ids |
| database/ | ~5,510 | 10 | 28 | SQLite/WAL, migrations, synchronous writes, pub/sub |
| web/ | ~4,700 | 9 | 28 | chi router, WebSocket, auth, middleware, embed, folder-open composer |
| chat/ | ~4,280 | 4 | 29 | YouTube live chat downloader (polling + batching) |
| utils/ | ~4,190 | 28 | 32 | HTTP helpers, formatters, YouTube URL parsing, JSON, DACL, resolution cap, canonical paths |
| cipher/ | ~4,090 | 15 | 21 | YouTube signature cipher: sidecar-routed + goja fallback |
| config/ | ~3,380 | 9 | 27 | TOML config, FlexDuration, channel terms, migrations |
| bgutils/ | ~2,350 | 6 | 12 | PO token: PotProvider, Challenge, BotGuard, WebPoMinter (goja path) |
| bgutils/sidecar/ | ~2,320 | 7 | 16 | Node subprocess manager: extract, JSON-RPC mux, Job Object pinning |
| goja/ | ~1,510 | 5 | 13 | JS runtime shims (minimal DOM, timers, encoding) |
| updater/ | ~1,440 | 5 | 9 | GitHub release checker, self-updater, Ed25519, signed release manifest |
| cookies/dpapi/ | ~1,110 | 6 | 9 | Windows DPAPI decryption for browser cookie stores |
| logger/ | ~970 | 3 | 4 | slog wrapper, file rotation, ring buffer, pub/sub |
| connectivity/ | ~600 | 4 | 5 | Reachability monitor; gates stream-end verdicts during outages |
| redact/ | ~540 | 4 | 4 | One redaction rule per kind of secret for error text, log lines and notifications — a media URL's credentials (a googlevideo URL's client IP, signatures and GVS PO token in both its URL forms; the playback session a Twitch weaver playlist's or edge segment's path spells), a credential inside a URL |
| jobfilter/ | ~520 | 2 | 6 | Dashboard filter language (Go twin of `filter-parser.js`/`filter-engine.js`), used by the TUI's `/` filter box |
| constants/ | ~460 | 1 | 3 | Hardcoded values (client configs, UAs, URLs) |
| jobstate/ | ~400 | 6 | 5 | Derived job sub-states both UIs show — the pending automatic action (the one Twitch auto-retry predicate), the wait-line marker, a Queued row's admission hold by the scheduler's precedence; imports only `database`, mirrored by utils.js against a shared table |
| childproc/ | ~380 | 4 | 4 | FFmpeg and ffprobe started tied to the process that runs them (Pdeathsig on a locked thread on Linux, a kill-on-close Job Object of the child's own on Windows) |
| ytdlpplugin/ | ~350 | 1 | 2 | yt-dlp PO-token plugin status/install — shared by the dashboard's Integrations card and the TUI's E Y overlay |
| treeguard/ | ~200 | 1 | 1 | Test-only: every tested package's `TestMain` runs through `Run`, which fails a run that leaves files in the package directory |
| disk/ | ~200 | 5 | 5 | Disk space queries: kernel32 on Windows, statfs on Linux |
| httpx/ | ~190 | 1 | 3 | Shared keep-alive-tuned http.Client/Transport shapes |
| notifications/notificationtest/ | ~180 | 1 | 2 | Shared fakes for the notification manager's tests |
| httpx/httpxtest/ | ~130 | 1 | 2 | Test-only: an http.Client that serves a handler in the test's own process, so a request can run inside a `testing/synctest` bubble (the updater's stall tests, the engine's idle-deadline and ceiling tests) |
| jobverbs/ | ~120 | 1 | 1 | One status predicate per job verb (Cancel, Resume, Reinitialize, Mux, Delete, Watched, Trim, chapter import) — called by the REST routes, the TUI and the worker; imports only `database` |
| bgutils/embed/ | ~80 | 4 | 2 | go:embed boundary for the Node binaries + sidecar tarball |
| stats/ | ~80 | 1 | 2 | Figures shared by the Web Stats tab and the TUI's E T overlay — job aggregates, disk reading |
| webtest/ | ~70 | 1 | 2 | Shared goja harness for evaluating shipped Web UI JS (`settings.js`) from Go tests |
| sqliteuri/ | ~30 | 1 | 2 | The `file:` URI every SQLite open goes through — the job database and the browsers' cookie databases |

### Totals

- **cmd/:** ~13,620 lines across 40 source files (39 in `cmd/moombox` — entry/launcher/adapters/wiring — plus the sign tool), plus 132 test files (~21,330 lines)
- **internal/ packages:** ~186,880 lines across 461 source files in 38 packages
- **Test code:** ~291,670 lines across 1,128 test files under `internal/` (`internal/docs`'s three included)
- **Frontend:** ~27,250 lines across 29 files (~1,150 KB) — `app.js`, `boot-theme.js`, `favicon.svg`, `index.html`, `login.html`, `login.js`, `moombox.css`, plus 22 ES modules under `web/public/modules/` (`chapter-import.js`, `chat-timeline.js`, `cookie-setup-session.js`, `files.js`, `filter-bar.js`, `filter-engine.js`, `filter-parser.js`, `imports.js`, `job-details.js`, `log-panel.js`, `logout.js`, `nico-geometry.js`, `nico-lanes.js`, `nico-scheduler.js`, `player.js`, `segments.js`, `settings.js`, `setup.js`, `stats.js`, `trimmer.js`, `update-indicator.js`, `utils.js`)

## Entry Points

- `cmd/moombox/main.go` — Application entry point and launcher/supervisor
- `cmd/sign/main.go` — CI signing tool (Ed25519)
