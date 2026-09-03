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

Runtime requires FFmpeg on PATH. CI (`.github/workflows/release.yml`) cross-compiles all 3 platform binaries (Windows x64, Linux x64, Linux arm64) from a single ubuntu-latest job on tag push, reads `RELEASE_NOTES.md` for GitHub release body.

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

# 2. Build the JS sidecar payload (~3.5 MB tarball):
cd bgutil-sidecar
npm ci --omit=dev                         # production deps only (jsdom + bgutils-js)
node build.mjs                            # tars node_modules + src/ to ../internal/bgutils/embed/
cd ..

# 3. Now build Moombox normally:
go build -o moombox.exe ./cmd/moombox
```

CI runs steps 1+2 automatically (see `.github/workflows/release.yml`). For local builds, run them once after fresh checkout; subsequent `go build` calls reuse the embedded blobs until `version.txt` drifts (Node version bump or sidecar JS change).

To skip the sidecar entirely (smaller binary, falls back to goja-only PO tokens), set `[bgutils] use_sidecar = false` in `config.toml`. The embed blobs are still required at build time though — they're either present or the binary doesn't compile.

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
    "progress": "V:1234 A:1234 C:5678",
})
```
Dynamically builds SET clauses. Auto-updates `updated_at`. Triggers `OnJobUpdate` subscribers. Returns `*Job`.

### Job status lifecycle
`Upcoming` → `Live` → `Downloading` → `Muxing` → `Finished`
Backlog VODs only: enter as `Queued` and are admitted to `Upcoming` by the worker's per-channel archive-slots scheduler (live/upcoming and newly published content never waits in `Queued`).
Error paths: any → `Error`, `Cancelled`, or `COOKIES?`

`JobStatus` is `type JobStatus string`. Timestamps are ISO 8601 strings. Optional numerics use pointers.

### TUI chord system
`buildMenuItems()` in `internal/tui/app_actions.go` = single source of truth for chords, action menu, hints, and help. `dispatchAction(chord, job)` = unified handler. Adding a chord: one entry in `buildMenuItems()` + one case in `dispatchAction()`.

Prefixes: **A** (Action), **R** (Request), **O** (Open), **Q** (Quit). Single keys: **F** (Filter), **M** (Menu), **`** (Settings), **?** (Help), **/** (Search logs — log panel only; `n`/`N` navigate matches, `Esc` clears). **O** chords include `O C` (Copy Stream URL to clipboard via OSC 52) and `O G` (Open GitHub Page). Confirm chords require a third keypress within 3s. **R** chords include `R N` (View Release Notes — shows pending-update notes when an update is available, otherwise fetches current version's notes from GitHub; from inside the overlay `U` applies the update) `R B` (Re-scan Feed History — forces a full-catalog backfill re-scan of every configured YouTube channel) and `R L` (Cookie Login — opens the setup wizard's cookie step alone so the browser login is reachable after first run; preselects the platform the status bar flags for re-login).

### Config migrations
`migrateOldFormat()` in `config/config.go` handles backward compat — migrates flat fields into current sections, converts legacy flags. Non-destructive (only applies when new section doesn't exist). Add migration logic for any renamed/relocated fields.

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
