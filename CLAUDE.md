# CLAUDE.md

Control prompts for Claude Code. For architecture, design, and implementation details, consult `SPEC.md` and `docs/spec/`.

## What This Is

Moombox is a YouTube/Twitch live stream archiver written in Go — single binary, Windows-only. Feature work, bug fixes, and improvements are the primary focus. See `SPEC.md` for full project specification.

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

Runtime requires FFmpeg on PATH. CI (`.github/workflows/release.yml`) builds Windows exe on tag push, reads `RELEASE_NOTES.md` for GitHub release body.

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
Dynamically builds SET clauses. Auto-updates `updated_at`. Triggers `OnJobUpdate` subscribers.

### Job status lifecycle
`Upcoming` → `Live` → `Downloading` → `Muxing` → `Finished`
Error paths: any → `Error`, `Cancelled`, or `COOKIES?`

`JobStatus` is `type JobStatus string`. Timestamps are ISO 8601 strings. Optional numerics use pointers.

### TUI chord system
`buildMenuItems()` in `app.go` = single source of truth for chords, action menu, hints, and help. `dispatchAction(chord, job)` = unified handler. Adding a chord: one entry in `buildMenuItems()` + one case in `dispatchAction()`.

Prefixes: **A** (Action), **R** (Request), **O** (Open), **Q** (Quit). Single keys: **F** (Filter), **M** (Menu), **`** (Settings), **?** (Help), **/** (Search logs — log panel only; `n`/`N` navigate matches, `Esc` clears). **O** chords include `O C` (Copy Stream URL to clipboard via OSC 52). Confirm chords require a third keypress within 3s.

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
- **`BgUtils`** — BotGuard/PO token generation
- **`ejs`** — yt-dlp external JS for cipher solving
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
