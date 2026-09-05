# Improvement chain — design (2026-09-04)

Umbrella design for the post-v2.8.7 improvement chain. Source: the read-only sweep in
`.superpowers/sdd/2026-09-04-improvement-sweep/findings.md` (gitignored) plus three extension-point
surveys. Every item there was verified at `da8e88b` before it entered this design.

The chain is eleven arcs. Arcs A–F and K are designed here in full. Arcs G–J are scoped here only;
each gets its own brainstorm, spec, and plan when its turn comes (the ratchet rule: hidden
complexity upgrades the path, never downgrades it).

## 1. Owner rulings (2026-09-04)

| # | Question | Ruling |
|---|---|---|
| Q1 | Scope | Everything, packages 1–10, plus Arc K (all-module upgrade at the end) |
| Q2 | go.mod | `go 1.27` + `toolchain go1.27.1` (floor, not pin) |
| Q3 | Embedded Node | v24.20.0 (Krypton LTS) in fetch-node, Dockerfile, release workflow |
| Q4 | Cancelled bucket | Issues = Error, Cancelled, COOKIES?; Finished = Finished only; both UIs |
| Q5 | `COOKIES?` label | TUI shows "Auth Required" like the Web |
| Q6 | Logout | Status-bar icon beside the theme toggle, visible only when auth is on and the session is authenticated |
| Q7 | Uncalled routes | Keep all; document the three GETs in the README API table |
| Q8 | `pot_provider_url` | Remove |
| Q9 | TUI cookie import | New chord `R I` Import Cookie File (path input) |
| Q10 | CI | ubuntu + windows matrix, full suite (no `-short`) |
| Q11 | Module bumps | Direct five + tidy now (Arc A); everything (`go get -u ./...`) last (Arc K) |
| Q12 | Hot-reload | Make the cheap three live; mark the two sidecar knobs restart-required |
| Q13 | Old worktree | Remove `player-review-plan` worktree + local + remote branch |
| — | Design defaults | Approved as presented: prune implemented plan docs, install FFmpeg on CI runners, expose the four per-channel fields and `probe_targets` in both UIs, `•` watched glyph |

## 2. Execution model

- One arc at a time, in order **A, D, B, C, E, F, G, H, I, J, K**. Each arc is a branch cut from
  `main` into `.worktrees/<arc-slug>`; the SDD loop is implementer → controller verification →
  reviewer → fix rounds → Fable arc-close review → merge `--no-ff` → delete worktree AND branch →
  post-merge gates. One implementer per working tree; reviewers and planners may run in parallel.
- Gates per arc: `go build ./...`, `go vet ./...`, `gofmt -l` empty, `staticcheck ./...` clean,
  `GOOS=linux GOARCH=amd64` and `GOARCH=arm64` builds, ONE `go test -count=1 ./...` (28/28), and
  `node --test web/tests/*.test.mjs` when JS changed. Live gates (`MOOMBOX_LIVE_CIPHER_TEST`,
  `MOOMBOX_LIVE_BG_TEST`) run where an arc touches goja, the sidecar, or the embedded Node.
- Ledger: `.superpowers/sdd/2026-09-04-improvement-chain/progress.md` (rulings with "why / cost if
  wrong"), per-arc reports and reviews beside it. Plans live in `docs/superpowers/plans/` and are
  deleted in the arc's last commit once implemented (the delete-implemented-plans rule).
- The owner controls push, tag, version bump, and release. Nothing in the chain pushes.
- Tests: TDD per task; every new assertion has a named mutant that fails it. Frontend logic goes in
  `web/public/modules/*` with node tests where it is pure; DOM behaviour uses the jsdom harness in
  `web/tests/`.

## 3. Arc A — toolchain, dependencies, housekeeping

**Goal.** Every build (local, CI, Docker) uses a patched toolchain; direct deps current; stale
artefacts gone.

**Design.**
1. `go.mod`: `go 1.27`, `toolchain go1.27.1`. The toolchain line is a floor: an older local Go
   auto-downloads 1.27.1; CI's `setup-go` (`go-version-file: go.mod`) resolves the same. Dockerfile
   build stage `golang:1.26-bookworm` → `golang:1.27-bookworm`.
2. Direct module bumps, one commit each so a regression bisects: `modernc.org/sqlite` 1.58.0,
   `golang.org/x/crypto` 0.56.0, `github.com/mattn/go-runewidth` 0.0.29, `github.com/yuin/goldmark`
   1.8.6, then `github.com/dop251/goja` 20260903 LAST with the cipher + sidecar live gates (the goja
   BotGuard live test is a known long-standing failure and is not a gate). `go mod tidy` after each;
   indirects move only where a direct bump requires it.
3. Embedded Node: `tools/fetch-node/main.go` `nodeVersion = "v24.20.0"` with the three SHA-256
   values from `https://nodejs.org/dist/v24.20.0/SHASUMS256.txt`:
   - `node-v24.20.0-win-x64.zip` `6cac9ffbca8f6a47091e4b5c772e0606049c3871cb67d900c0cedde630e545ba`
   - `node-v24.20.0-linux-x64.tar.xz` `2f2c0da162318f0de47665410c7c8c2ed3d36c8f3105de4bbc61176c70a7cbf2`
   - `node-v24.20.0-linux-arm64.tar.xz` `5f4ddab610c1ab2016b3c227cebdbf6d9495161487e4739c7b90090595f465f7`

   The "v22 LTS" comments in that file (`:15`, `:43-47`, `:268`) are rewritten and `Last bumped`
   updated.
   Dockerfile sidecar stage `node:22-bookworm-slim` → `node:24-bookworm-slim`; `release.yml`
   `node-version: '24'`. Regenerate the embed blobs locally (`go run ./tools/fetch-node`, sidecar
   `npm ci --omit=dev && node build.mjs`); `internal/bgutils/embed/version.txt` changes and is
   committed. Gates: the sidecar integration tests (`internal/bgutils/sidecar`, ~35 s) and
   `MOOMBOX_LIVE_BG_TEST`.
4. `docker-publish.yml`: `docker/setup-qemu-action@v3` → `@v4`.
5. `web/tests/package.json`: jsdom `^25.0.1` → `^30.0.1`, lockfile regenerated, node suite green
   (92/92 today).
6. Housekeeping: `git worktree remove --force .claude/worktrees/player-review-plan`, `git worktree
   prune`, `git branch -D player-review-plan`, `git push origin --delete player-review-plan` (the
   only push in the chain — it deletes, it does not publish; owner-ruled Q13). Prune the implemented
   plan docs in `docs/superpowers/plans/` and their design specs in `docs/superpowers/specs/`
   (everything dated on or before 2026-09-03 except `2026-08-29-cookie-remediation-field-test-plan.md`,
   which is a live field-test procedure). Before deleting each one, confirm it is implemented: its
   merge commit exists on `main` (`git log --grep` on the arc name) or its EXECUTION STATUS says so;
   anything unconfirmed stays and is listed in the ledger. One commit; `git log` keeps them.
7. Local tooling (not committed): reinstall `staticcheck` for Go 1.27 so the local gate matches CI.

**Docs.** Every line that names the old versions: `SPEC.md:5,38,404,800` (Go 1.26, Node v22),
`docs/spec/operations.md:9,98,99,197`, `docs/spec/platform-services.md:687,736`,
`docs/spec/appendix-metrics.md` (Go version, test baseline), the `release.yml:83` comment,
`bgutil-sidecar/README.md` if it names Node 22. `CLAUDE.md` names no version and stays.

**Tests.** Existing suites (fetch-node's manifest tests, the sidecar integration tests, the node
suite). No new tests: this arc changes pins, not logic.

## 4. Arc D — CI test workflow and the notifications timeout

**Goal.** Tests run on every push and pull request on both supported desktop platforms.

**Design.**
1. `.github/workflows/ci.yml`, triggers `push: branches: [main]` and `pull_request`. Matrix
   `os: [ubuntu-latest, windows-latest]`. Steps: checkout; restore the embed-blob cache with the
   same key as `release.yml`; setup-go from go.mod; on cache miss setup-node 24, build the sidecar
   payload, `go run ./tools/fetch-node`; install FFmpeg (`apt-get install -y ffmpeg` /
   `choco install ffmpeg -y`) so `muxer_concatcopy_test.go` and `probe_params_test.go` stop
   skipping; `gofmt -l` must print nothing; `go vet ./...`; `go install
   honnef.co/go/tools/cmd/staticcheck@latest && staticcheck ./...`; `go build ./...`;
   `go test -count=1 ./...`. ubuntu only: `GOOS=linux GOARCH=arm64 go build ./cmd/moombox`,
   `npm ci` in `web/tests`, `node --test web/tests/*.test.mjs`. `permissions: contents: read`.
   Concurrency group per ref with cancel-in-progress.
2. `internal/notifications/manager.go`: `Manager.waitTimeout time.Duration`, set to 30 s in
   `NewManager`; `Wait()` uses it and logs the actual value. `TestManagerWaitTimesOut` sets 50 ms and
   drops its `testing.Short` skip. Package time 31.6 s → ~1 s.

**Docs.** `docs/spec/operations.md` §CI gains the workflow; `CLAUDE.md` Build & Test mentions it in
one line.

## 5. Arc B — bug fixes and settings gaps

**Goal.** No data loss from the TUI channel editor; the settings surface tells the truth.

**Design.**
1. **Channel editor preserve** (`internal/tui/settings_channels.go`): `valuesToChannel(vals,
   existing *config.ChannelConfig)` copies `existing` when non-nil and overwrites only the edited
   fields. `Terms`: if the operator did not change the terms field, the existing `ChannelTerms`
   (multi-key included) is kept verbatim; if changed, `Simple` is set. `saveCurrentChannel` passes
   the current channel; the setup wizard passes nil. Test: a channel with all four extra fields and
   a `{stream, vod}` terms map survives an edit of the name; mutant: drop the copy.
2. **Four per-channel fields in both editors.** Web channel dialog (`settings.js`) and TUI channel
   form gain `num_desc_lookbehind` (int, blank = default), `output_directory` (path, blank = global),
   `archive_window_days` and `archive_slots` (ints, blank = default). Blank clears the pointer /
   string. Validation mirrors `config.validate`.
3. **TUI import headers** (`internal/tui/app_commands.go:246-249`): `url.PathEscape` on both
   values; test proves a title with `%`, `+` and CJK round-trips through `decodeImportHeader`.
4. **Web resume gate**: one `canResume(job)` in `app.js` (status ∈ RESUME set or Finished+incompleteTail,
   platform youtube, hasStaging) used at the details button, the batch-bar visibility, and
   `batchAction("resume")`. jsdom test: a Twitch Error job and a YouTube Error job without staging
   are excluded from the batch count.
5. **Recover** in the dial goroutines at `internal/connectivity/probe.go:38` (log at Debug).
6. **Validation**: `validateConfigUpdates` gains `network.client_token_ttl_days` (1..3650) and a
   `connectivity.probe_targets` block (each entry passes `net.SplitHostPort`); `applyConfigUpdates`
   gains the `connectivity` section. Both UIs expose `probe_targets` in the Network section as a
   list control like `trusted_proxies`; it is restart-required (the monitor takes its targets at
   construction).
7. **Hot-reload**: `ConfigRoutesCallbacks` gains `OnMemoryLimitChange`, `OnTrustForwardedProtoChange`,
   `OnFfmpegPathChange`; `cmd/moombox/routes_wiring.go` re-applies `debug.SetMemoryLimit` and
   `web.SetTrustForwardedProto`, and `TrimService` gains `SetFfmpegPath` (atomic value, read at
   the start of each trim) called from the new callback. `memory.sidecar_hard_limit_mb`, `bgutils.use_sidecar`, `connectivity.probe_targets`
   join `RESTART_REQUIRED_FIELDS` and `restartRequiredKeys`; `TestRestartRequiredListsAgree` keeps
   them equal. Tests: a PUT that changes each live field observes the new value without restart.

**Docs.** `docs/spec/data-and-storage.md` config table (live vs restart), `user-interfaces.md`
settings rows and channel editor fields, `config.example.toml` comments for the four fields.

## 6. Arc C — dead code, dead config, logout, wording

**Design.**
1. Remove `discordWebhookPath`, `redactDiscordWebhookURL`, `challengeLabel`,
   `PlayerAPI.decryptSig`, `decryptSigLegacy`; fold `challengeLabel`'s dormant-minter note into the
   mint-path comment it references. `browse.go:254` becomes a conversion. `staticcheck` clean is a
   gate from here on.
2. Remove `Downloader.PotProviderURL` (`types.go`), its apply line (`config_routes.go`), the
   `config.example.toml` line, the `data-and-storage.md` row, and the `REWRITE_GUIDE.md` line. Test:
   a TOML file containing `pot_provider_url` still loads (unknown keys are ignored; the next save
   drops it).
3. README API table: rows for `GET /api/logs`, `GET /api/jobs/{id}/segments`,
   `GET /api/jobs/{id}/trims`, wording copied from `user-interfaces.md`.
4. **Logout**: `#btn-logout` (`sl-icon-button`, `box-arrow-right`) as the last child of
   `#status-bar-right`, hidden by default. `checkSecurityBanner()` stores `this.authStatus`; the
   button shows when `authRequired && authenticated`. Click → `POST /api/auth/logout` →
   `window.location.reload()` (the 401 monkey-patch exempts `/api/auth/*`, so no double reload).
   Route test already covers cookie clearing; jsdom test covers visibility + the POST.
5. **Wording**: `StatusLabel(status)` beside `StatusColor` in `internal/tui/styles.go`, used at
   `action_menu.go:117` (column 11 → 13, title budget `contentW-19`) and `job_details.go:278`
   (the gradient and border colour keep the raw status). Web `filter-engine.js`:
   `issues: ["Error","Cancelled","COOKIES?"]`, `finished: ["Finished"]`, `errors` kept as an alias
   resolving to `issues`; `STATUS_OPTIONS` and `_filterTokenLabel` say "Issues"; node tests updated
   and an alias test added. TUI `passesFilter` unchanged (already Issues). Hints: "no jobs to
   resume" / "no jobs to reinitialize". `R F` label → "Refresh Cookies from Browser".

**Docs.** `user-interfaces.md` chord table + filter rows + status label; `SPEC.md` if it names the
buckets.

## 7. Arc E — engine test speed

**Design.** `SegmentDownloader` gains an unexported `delays` struct: `singleGoneRetry` (500 ms),
`interruptionStallRetry` (5 s), `hlsStuckRetry` (2 s), `connectivityPoll` (5 s), `atEdgeBackoffUnit`
(1 s), `hlsReloadFloor` (from `hlsReloadDelay`). Defaults are assigned from the existing constants in
`NewSegmentDownloader`; the ~11 call sites in `downloader_dash.go`, `downloader_hls.go`,
`downloader_fetch.go` read `d.delays.X`; `waitForConnectivity` takes the poll interval as a
parameter (4 callers). The eight slow tests set millisecond delays and rescale their own
`time.After` bounds (`downloader_interruption_test.go:96,384` switch to the `OnActivity` hook they
already install). A test pins every default equal to its constant, so production timing is
unchanged. Target: `internal/engine` under 10 s.

## 8. Arc F — small parity features

**Web.**
1. "Re-scan feed history" button in `#settings-channels` beside "Add channel"; handler copies
   `checkMonitorsNow` (POST `/api/backfill/rescan`, debounced toast with `retryAfterMs`).
2. "Copy stream URL" `data-copy` button in job details next to "Copy video ID"; `streamUrl(job)` in
   `modules/utils.js` is the JS twin of `internal/tui/app_actions.go:498-516` (job.url, else
   platform-derived); node test with the four cases.

**TUI.**
3. `R I` Import Cookie File: a step overlay (path input with `~` expansion and an existence check)
   → `App.OnImportCookieFile(path) (cookies.ImportResult, error)` wired to `ImportCookies`; result
   per platform (`imported / unchanged / rolled-back / rejected`) rendered inside the overlay, the
   overlay closes on Esc. Never logs cookie content. Test drives the dialog with a fake callback.
4. `A W` Toggle Watched: `SupportsBatch`, `JobFilter` Finished; `App.OnSetWatched(ids, watched)` →
   `db.BatchSetWatched`. Row glyph: a dim `•` between the platform tag and the title, counted in
   `titleWidth`. Test: glyph appears/disappears and the row does not wrap.
5. Orphans dialog: `A`/`a` deletes every entry in the current section after the two-press confirm,
   looping the existing per-item callbacks; per-item failures are collected and shown in the dialog.
6. Release-notes overlay: `S`/`s` skips the pending version via `App.OnDismissUpdate(tag)`. The Web
   handler body moves to `routes.DismissUpdate(store, shared, tag) error` and both callers use it.
7. Log panel: `c` clears the view (lines, filtered, search) when `PanelLogs` has focus; help
   `quickKeys` lists it.
8. `R Y` yt-dlp Plugin: async overlay showing installed / plugin dir / port mismatch from a new
   exported `routes.YtdlpPluginStatus(cfg)` (extracted from the GET handler); `I` runs the existing
   `OnInstallYtdlp` and refreshes.

**Docs.** `CLAUDE.md` chord line, `README.md` keyboard controls, `user-interfaces.md` chord table and
Web actions, `SPEC.md` parity table if present.

## 9. Arcs G–J — scoped only

- **G** TUI stats view (the Web's Stats tab: disk, throughput, history). Own brainstorm: layout,
  which of the Web's charts translate to text, refresh cadence.
- **H** TUI structured filter (the Web's `status:`/`channel:`/`platform:` language with negation and
  OR, plus videoId in the Web's text match). Own brainstorm: reuse of `filter-parser.js` semantics
  in Go, interaction with the `F` cycle and `/` search.
- **I** Structure: split `internal/cookies/autocookies.go` (3973) and `refresh.go` (3638) by
  responsibility; extract `NicoScheduler` + `nico-geometry.js` from `player.js`; thin `app.js`.
  Own design: boundaries, no behaviour change, tests pin before/after.
- **J** Carried residuals: CP1 (a replay run must not adopt a LIVE early-chat sidecar), T-F10,
  T-F12 remainder, overlay focus trap, capacity knob, rollback-commit block duplication, the
  `loadCookieJar` seam, `authVerifyTimeout` split. Each with its own design; the player ledger's
  rulings are the design of record for anything touching chat timing.

## 10. Arc K — all-module upgrade

After A–J are merged and gated: `go get -u ./...` (all 23+ indirects), `go mod tidy`, then the full
gate set plus the cipher and sidecar live gates and both Linux cross-builds. A rendering regression
in the TUI (charm internals) or a regex regression (regexp2) is bisected by reverting the module
group (charm / modernc / x/* / goja chain) rather than the whole bump. Nothing pushes.

## 11. Out of scope

Wiring an external PO-token provider (Q8 chose removal); `-short` CI (Q10); Node 26; any release,
tag, or push; changes to progress-update cadence or the DB layer (standing owner rulings).

## 12. Success criteria

- A TUI channel edit round-trips every `ChannelConfig` field.
- `go version moombox.exe` on the owner's machine reports a toolchain ≥ 1.27.1 and `govulncheck`
  reports zero reachable stdlib issues.
- CI runs on push/PR on both runners and is green at the chain's end.
- `staticcheck ./...` clean; no config field, route, or UI control is dead.
- Both UIs share bucket names, status labels, and action wording.
- Engine, notifications, and total suite wall time are reported before/after in the ledger.
