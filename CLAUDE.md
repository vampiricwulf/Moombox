# CLAUDE.md

Control prompts for Claude Code. For architecture, design, and implementation details, consult `SPEC.md` and `docs/spec/`.

## What This Is

Moombox is a YouTube/Twitch live stream archiver written in Go — single binary. Windows x64 + Linux x64 + Linux arm64 supported. Pragmatic parity: core download pipeline / web dashboard / TUI / sidecar work identically across platforms; Windows-specific features (UAC elevation, DPAPI cookie reading) degrade gracefully on Linux with clear UI messaging. Feature work, bug fixes, and improvements are the primary focus. See `SPEC.md` for full project specification.

## Working Style

When implementing features, fixes, or non-trivial changes, ask questions about design decisions and intent before diving in using the AskUserQuestion tool. Don't assume — clarify the "why" and preferred approach so the implementation matches what's actually wanted. This applies to naming, placement, scope, UX behavior, architectural choices, and aesthetic preferences (even for trivial changes). Ask questions one at a time with suggested answers rather than batching. Always use AskUserQuestion — never inline questions into regular text output.

## Build & Test

```bash
go build ./...                                      # Build all packages
go build -o moombox.exe ./cmd/moombox               # Build binary
go test ./...                                       # Run all tests
go test -v ./internal/engine/...                    # Single package
go test -v -run TestParseDash ./internal/engine/... # Single test
go vet ./...                                        # Static analysis
```

Runtime requires FFmpeg on PATH. CI: `.github/workflows/ci.yml` runs gofmt / `go mod tidy -diff` / vet / the `modernc.org/libc` pin check / the Dockerfile Go-version check / staticcheck / build / `go test -count=1 ./...` on ubuntu + windows for every push to main and every PR (ubuntu also runs `go test -race` on `internal/logger`, `internal/database` and `internal/web`, cross-builds linux/arm64, runs `bgutil-sidecar`'s `npm test`, and runs `node --test ./*.test.mjs` in `web/tests` after its own `npm ci`, so the jsdom suites run too); `.github/workflows/release.yml` runs on tag push: it calls `ci.yml` as a required `test` job on the tagged commit, then cross-compiles all 3 platform binaries (Windows x64, Linux x64, Linux arm64) from a single ubuntu-latest job, reads `RELEASE_NOTES.md` for the GitHub release body, and calls `.github/workflows/docker-publish.yml` for the image. Running release.yml by hand ("Run workflow") is a dry run — every job, nothing published — and is the only way to exercise that pipeline before a tag.

### Profiling (pprof)

For memory/CPU/goroutine investigations, run the binary with `MOOMBOX_PPROF=1` in the environment. The child process binds the standard `net/http/pprof` handlers on `localhost:6060` (loopback-only, no auth). Disabled by default — adds zero overhead when the env var is unset.

```powershell
$env:MOOMBOX_PPROF = "1"
.\moombox.exe
# In another terminal:
go tool pprof http://localhost:6060/debug/pprof/heap     # live heap
go tool pprof http://localhost:6060/debug/pprof/allocs   # cumulative allocs since start
go tool pprof http://localhost:6060/debug/pprof/profile  # 30s CPU profile
curl http://localhost:6060/debug/pprof/goroutine?debug=2 # goroutine dump (text)
```

Diff two snapshots to isolate growth from steady-state heap:
```powershell
Invoke-WebRequest http://localhost:6060/debug/pprof/heap -OutFile heap-t0.pprof
# wait N minutes
Invoke-WebRequest http://localhost:6060/debug/pprof/heap -OutFile heap-t1.pprof
go tool pprof -inuse_space -base heap-t0.pprof heap-t1.pprof
# (pprof) top 30 -cum
```

### BotGuard sidecar embed prerequisites

`go build ./cmd/moombox` requires two embed blobs to be present in `internal/bgutils/embed/` before compilation. Without them the `go:embed` directives in `internal/bgutils/embed/embed.go` fail.

```bash
# 1. Fetch + gzip the pinned Node.js binaries for all 3 platforms (~150 MB total):
go run ./tools/fetch-node                 # idempotent; skips on version match

# 2. Build the JS sidecar payload (~4 MB tarball):
cd bgutil-sidecar
npm ci --omit=dev                         # production deps only (jsdom + bgutils-js)
node build.mjs                            # tars node_modules + src/ to ../internal/bgutils/embed/
cd ..

# 3. Now build Moombox normally:
go build -o moombox.exe ./cmd/moombox
```

CI runs steps 1+2 automatically (see `.github/workflows/release.yml`). For local builds, run them once after fresh checkout; subsequent `go build` calls reuse the embedded blobs. Re-run step 1 when the Node pin in `version.txt` moves, and step 2 whenever anything under `bgutil-sidecar/` changes — `version.txt` records the Node pin only. A stale `sidecar.tar.gz` is caught by `internal/bgutils/embed/embed_test.go`, which fails when the tarball's `src/`, manifests or ejs pin differ from the tree.

To run without the sidecar, set `[bgutils] use_sidecar = false` in `config.toml`: only the goja path remains, which BotGuard's timing check rejects, so no PO tokens are minted and current players' signatures cannot be solved. The embed blobs are still required at build time though — they're either present or the binary doesn't compile.

### Windows resource embedding

Exe icon and version info via `.syso` files from `cmd/moombox/winres/`. CI generates at build time — none committed. Local builds with icon: `go install github.com/tc-hib/go-winres@latest && cd cmd/moombox && go-winres make`.

## Critical Patterns

These patterns MUST be followed exactly. Deviating will break things.

### Logger interface
```go
logger interface {
    Debug(msg string, args ...any)
    Info(msg string, args ...any)
    Warn(msg string, args ...any)
    Error(msg string, args ...any)
}
```
Anonymous interface repeated in every struct — intentional for loose coupling. **Do not extract to a named interface.**

### Database partial updates
```go
db.UpdateJobFields(jobID, map[string]any{
    "status":   database.StatusDownloading,
    "progress": "(V: 1234/1300 A: 1234/1300 C: 5678)",
})
```
Dynamically builds SET clauses. Auto-updates `updated_at`. Triggers `OnJobUpdate` subscribers. Returns `*Job`.

A status transition decided on a status read earlier must not be written unconditionally — it overwrites whatever landed in between (an operator's Cancel). Use `db.UpdateJobFieldsIf(jobID, expected, fields)` (applies only while the status is still `expected`) or `db.UpdateJobFieldsUnless(jobID, unwanted, fields)` (only while it is not `unwanted`); both return whether the write applied, and notify subscribers only when it did.

### Job status lifecycle
`Upcoming` → `Live` → `Downloading` → `Muxing` → `Finished`
Backlog VODs only: enter as `Queued` and are admitted to `Upcoming` by the worker's per-channel archive-slots scheduler, which admits none while offline or while the output volume is at or past `disk_critical_percent` (live/upcoming and newly published content never waits in `Queued`); a backlog VOD parked in `COOKIES?` returns to `Queued` (when its feed row still exists), not `Upcoming`, so a cookie repair re-admits it through the same pacing.
Error paths: any → `Error`, `Cancelled`, or `COOKIES?`. Two automatic ways out of `Error`, both the Twitch monitor's and never both for one row: a Twitch job that failed with `TwitchOfflineErrMsg` before any segment was downloaded is re-initialised to `Upcoming` when the monitor finds the same broadcast live, within `MaxTwitchAutoRetries` (`isRecoverableTwitchError` → `AutoReinitializeJob`); and a live Twitch capture that stopped with its broadcast's end unconfirmed (`park_reason` `twitch_end_unconfirmed`, which `isRecoverableTwitchError` refuses) is muxed once the Twitch monitor confirms the broadcast over, exactly as the Mux action would, once.

`JobStatus` is `type JobStatus string`. Timestamps are ISO 8601 strings. Optional numerics use pointers.

### TUI chord system
`buildMenuItems()` in `internal/tui/app_actions.go` = single source of truth for chords, action menu, hints, and help. `dispatchAction(chord, job)` = unified handler. Adding a chord: one entry in `buildMenuItems()` + one case in `dispatchAction()`. Adding a PREFIX touches six closed lists as well: the prefix `case` in `app_keys.go`, `chordFeedback`'s label switch and `feedbackColor`'s chord-yellow prefixes (`app_actions.go`, `app_layout.go`), `categoryHelpTitles`/`categoryOrder` in `help.go`, the `named`/`bare` hint lists in `status_bar.go`, and the `Category` comment in `action_menu.go`.

Prefixes: **A** (Action), **R** (Request), **O** (Open), **E** (Extras), **Q** (Quit). Single keys: **F** (Filter), **M** (Menu), **`** (Settings), **?** (Help), **/** (Filter: on the Tasks panel the dashboard's filter language — text, status:/channel:/platform:, -negation, a|b; on the log panel a text search with n/N), **c** (Clear log view — log panel only), **PgUp/PgDn** (page scroll — Tasks, Details, Logs), **Home/End** (Tasks: jump to first/last row), **Ctrl+C** (quit immediately — checked ahead of every overlay, so nothing can swallow it). **A** chords include `A W` (Toggle Watched — batch-capable, Finished jobs; the row shows a dim `•`) and `A S` (Recover Set-aside Recordings — confirm-gated; muxes the recordings the engine set aside on a mid-stream restart into their own files beside the archive, carrying a kept chat capture beside the first, then reclaims staging only when nothing unmuxed is left in it; offered only for a job that is not active and whose staging still holds one — its own verb, never a widening of `A M`/`/mux`, and the two share a per-job staging claim so neither can run while the other does). **O** chords include `O C` (Copy Stream URL to clipboard — OSC 52 always, on every platform; a `clip.exe` child in addition on a local Windows console, i.e. not Windows Terminal and not SSH, run off the update goroutine, and only its success upgrades the line to "Copied") and `O G` (Open GitHub Page). **E** chords include `E Y` (yt-dlp Plugin — installed / plugin dir / port mismatch from the same `routes.YtdlpPluginStatus` the dashboard reads, `I` installs), `E L` (Cookie Login — opens the setup wizard's cookie step alone so the browser login is reachable after first run; preselects the platform the status bar flags for re-login), `E I` (Import Cookie File — a path prompt, with `~` expansion, that imports a Netscape cookies.txt through the same verify-and-roll-back path as the Web import; the path is the only thing ever shown or logged, never the file) and `E T` (Statistics — the Web Stats tab's figures, refreshed every minute). Confirm chords require a third keypress within 3s. **R** chords include `R N` (View Release Notes — shows pending-update notes when an update is available, otherwise fetches current version's notes from GitHub; from inside a pending update's notes `U` applies it and `S` skips it — the current version's notes offer neither) and `R B` (Re-scan Feed History — forces a full-catalog backfill re-scan of every configured YouTube channel).

### Config migrations
`migrateOldFormat()` in `internal/config/config.go` handles backward compat — migrates flat fields into current sections, converts legacy flags. Non-destructive (only applies when new section doesn't exist). Add migration logic for any renamed/relocated fields.

### API route prefix
All REST endpoints use `/api/` (no version). Route registration and frontend fetch calls must stay in sync.

### Panic recovery
All goroutines MUST have inline `defer func() { if r := recover(); ... }()`. HTTP: `RecoveryMiddleware`. DB callbacks: `safeCallJobUpdate`/`safeCallJobsChange`.

### Web UI embedding
Static assets in `web/public/`, embedded via `go:embed` in `web/embed.go`. Changes require `go build`.

## References

The local `references/` folder (gitignored) contains upstream repos:
- **`yt-dlp`** — YouTube format/cipher/extraction, Twitch extractor, PO tokens, cookies
- **`BgUtils`** — BotGuard/PO token generation; consumed as `bgutils-js` npm dep in the sidecar (`bgutil-sidecar/package.json`), with `internal/bgutils/` as the goja fallback
- **`ejs`** — yt-dlp external JS for cipher solving; vendored into `bgutil-sidecar/vendor/ejs/` (pinned via `VERSION`), with `internal/cipher/` as the goja fallback
- **`chatterino7`** — Twitch chat (IRC, emotes, badges)
- `bgutil-ytdlp-pot-provider` — yt-dlp PO token plugin
- `moonarchive` — Python stream archiver (segment strategies)
- `moombox` — original Python moombox

Run `bash references/update-all.sh` to pull upstream and see relevant changes. Use `--diff` for verbose diffs.

## Release Process

1. **Generate `RELEASE_NOTES.md`** — `git log --oneline <prev-tag>..HEAD`, group by Features/Improvements/Bug Fixes/Internal (skip empty). No heading.
2. **Bump version** in `cmd/moombox/main.go` (`version = "x.y.z"`).
3. **Commit** both together: `chore: bump version to x.y.z — short summary`.
4. **Tag** (`git tag vx.y.z`) and **push** (`git push && git push origin vx.y.z`).

CI reads `RELEASE_NOTES.md` from the repo.
