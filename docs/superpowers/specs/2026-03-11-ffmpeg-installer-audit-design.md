# FFmpeg-Shared Installer Audit — Design Spec

**Date**: 2026-03-11
**Scope**: Full audit of the FFmpeg installer subsystem (security, correctness, error handling, UX, code quality)
**Approach**: Fix all issues + light cleanup (constants, dedup, comments)

## Files In Scope

| File | Lines | Role |
|------|-------|------|
| `internal/web/routes/ffmpeg.go` | 649 | API routes, install logic, PATH refresh, version check, pending tokens |
| `internal/web/routes/ffmpeg_elevation_windows.go` | 133 | Syscall UAC elevation, process waiting |
| `internal/tui/ffmpeg_check.go` | 630 | Bubble Tea overlay (5 modes: main, install, custom, review, manual) |
| `internal/tui/app_keys.go` | ~30 | Key dispatch for ffmpeg overlay actions |
| `internal/tui/app_update.go` | ~60 | Message handling for ffmpeg async results |
| `internal/tui/app.go` | ~20 | Callbacks, model wiring, startup flag |
| `web/public/modules/setup.js` | 808 (of which ~220 FFmpeg-related) | Web UI FFmpeg overlay, install flow, script review |
| `web/public/index.html` | ~68 lines FFmpeg markup | HTML markup for FFmpeg overlay |

## Findings & Fixes

### Security (3 issues)

**S1. PowerShell string injection in `generateInstallScript`** (`ffmpeg.go:429`)

The `resultPath` is embedded into a PowerShell double-quoted string via concatenation. In PS double-quoted strings, `$` starts variable interpolation and backticks are escape characters. While the current path (hex token in temp dir) is safe, this is fragile against future changes.

**Fix**: Use single-quoted string for the result path in the generated PS script. PS single-quoted strings treat all characters literally.

**S2. `generateToken` ignores `rand.Read` error** (`ffmpeg.go:437`)

```go
rand.Read(b) // error ignored
```

If `crypto/rand` fails, the token is all zeros — predictable and a collision risk. Note: as of Go 1.24+, `crypto/rand.Read` panics on failure rather than returning an error, making this primarily a code hygiene fix. Still the right practice for defensive coding.

**Fix**: Return error from `generateToken`, propagate through `PrepareInstall`.

**S3. `ConfirmInstall` lacks concurrent install protection** (`ffmpeg.go:490`)

`InstallFFmpeg` uses `installMu` to serialize, but `ConfirmInstall` does not. Two concurrent confirm requests could run two package managers simultaneously. Additionally, a `ConfirmInstall` and an `InstallFFmpeg` (already-elevated direct install) could race each other since they don't share the mutex.

**Fix**: Acquire `installMu` at the start of `ConfirmInstall`.

### Correctness (3 issues)

**C1. `expandWindowsEnv` misleading docstring** (`ffmpeg.go:638`)

Comment says it expands both `%VAR%` and `$VAR`, but the code only expands `%VAR%`. `os.ExpandEnv` is never called. Since REG_EXPAND_SZ values use `%VAR%` syntax, the behavior is correct — only the comment is wrong.

**Fix**: Rewrite comment to accurately describe `%VAR%`-only expansion.

**C2. `CheckFFmpegCached` blocks all concurrent callers** (`ffmpeg.go:46`)

Holds the mutex while running `checkFFmpeg()` (up to 10s timeout). During the 2s polling from `pollForRestart`, requests pile up and all block.

**Fix**: Replace the mutex + manual cache with `golang.org/x/sync/singleflight`. One concurrent probe runs; others wait for its result without each spawning a separate `ffmpeg -version` process. Keep the TTL cache layer on top so repeated calls within 10s return immediately. Note: this adds `golang.org/x/sync` as a dependency — this is a widely-used, stable module from the Go team. If avoiding new dependencies is preferred, an alternative is to release the mutex before calling `checkFFmpeg`, then re-acquire to store the result (with a double-check on cache validity).

**C3. `parseFFmpegVersion` uses `fmt.Sscanf` unnecessarily** (`ffmpeg.go:273`)

The regex already captured digit groups as strings. `fmt.Sscanf` with `%d` works but is unidiomatic when `strconv.Atoi` is available and provides proper error returns.

**Fix**: Replace `fmt.Sscanf` with `strconv.Atoi`.

### Error Handling (3 issues)

**E1. `RejectInstall` removes files that don't exist yet** (`ffmpeg.go:545-546`)

Calls `os.Remove` on `scriptPath` even though the script is only written to disk at `ConfirmInstall` time. `os.Remove` silently fails, so this is harmless but misleading.

**Fix**: Only remove `resultPath` (which also doesn't exist yet but is the one that could theoretically be created by a stale elevated process). Add a comment explaining the defensive cleanup.

**E2. Rate limiter no-op subrouter** (`ffmpeg.go:106`)

`rl = r.With()` creates a subrouter with no middleware when `RateLimit` is nil. This works but is unnecessary indirection.

**Fix**: Use `r` directly when `deps.RateLimit` is nil.

**E3. `ConfirmInstall` missing-result error message** (`ffmpeg.go:522`)

Current message: "install may have succeeded but result file not found — restart to check". Could be more actionable.

**Fix**: Append "try running `ffmpeg -version` in a terminal to verify".

### UX (3 issues)

**U1. TUI: No loading indicator for custom path check** (`ffmpeg_check.go:354-357`)

When user presses Enter on a custom path, the async check dispatches but no spinner appears. The `checkFFmpeg` function has a 10s timeout — user gets no visual feedback during this wait.

**Fix**: Add a `checking` state to `FFmpegCheckModel`. Set it before dispatching the async check, clear it when the result arrives. Show the spinner in the `ffmpegCustom` and `ffmpegManual` views while `checking` is true. Also block duplicate dispatches — ignore Enter while `checking` is already true.

**U2. Web UI: `window.close()` Quit button doesn't work for direct navigation** (`setup.js:151`)

`window.close()` only works if the page was opened by JavaScript. If the user typed the URL directly, the button does nothing silently.

**Fix**: After calling `window.close()`, set a short timeout that shows a "You can close this tab manually" fallback message if the window is still open.

**U3. Web UI: Success auto-dismiss too fast with warnings** (`setup.js:694-697, 792-795`)

When there's a version warning (e.g., "FFmpeg 3.x is outdated"), the overlay auto-dismisses after 3 seconds. This isn't enough time to read the warning. The same pattern exists in both `showInstallSuccess` (line 694) and `checkFFmpegPath` (line 792).

**Fix**: When a warning is present, replace the auto-dismiss timeout with a "Continue" button that the user must click. No-warning installs keep the 1.5s auto-dismiss. Apply to both `showInstallSuccess` and `checkFFmpegPath`, ideally extracting a shared success handler.

### Code Quality (3 issues)

**Q1. Method string constants** (repeated ~15 times across files)

"choco", "choco-install", "winget" are string literals scattered across `InstallFFmpeg`, `generateInstallScript`, `PrepareInstall`, and both UI frontends. Typos would fail silently.

**Fix**: Define exported constants in `ffmpeg.go`:
```go
const (
    MethodChoco        = "choco"
    MethodChocoInstall = "choco-install"
    MethodWinget       = "winget"
)
```

Use these in all switch statements and API method validation. Frontend strings stay as-is (they're the wire format).

**Q2. Duplicated post-install verification in API handlers** (`ffmpeg.go:198-210, 233-245`)

Both `POST /api/ffmpeg/install` and `POST /api/ffmpeg/install/confirm` have identical "InvalidateCache → checkFFmpeg → error or success JSON response" blocks.

**Fix**: Extract to a helper:
```go
func verifyInstallAndRespond(rw http.ResponseWriter, logger interface{ Info(string, ...any) }) {
    InvalidateFFmpegCache()
    valid, version, warning := checkFFmpeg("ffmpeg")
    if !valid {
        jsonError(rw, "FFmpeg installed but not found on PATH. You may need to restart.", http.StatusInternalServerError)
        return
    }
    logger.Info("FFmpeg installed successfully", "version", version)
    jsonResponse(rw, map[string]any{"success": true, "version": version, "warning": warning})
}
```

**Q3. Duplicated TUI custom path persistence** (`app_update.go:350-366`)

The `ffmpegCheckResultMsg` handler has identical "persist custom FFmpeg path to config" blocks for both `ffmpegCustom` and `ffmpegManual` modes.

**Fix**: Collapse into a single condition:
```go
if a.ffmpegCheck.mode == ffmpegCustom || a.ffmpegCheck.mode == ffmpegManual {
    // set result, persist path
}
```
