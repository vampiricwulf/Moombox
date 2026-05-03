# Linux Build Support — Design

**Date:** 2026-05-02
**Status:** Approved, ready for implementation plan
**Affected systems:** build/CI, launcher, updater, disk/cookie/ffmpeg packages, web UI auto-cookies setup, BotGuard sidecar embed

## Goal

Compile, ship, and run Moombox on Linux (x64 + arm64) alongside the existing Windows x64 target. Achieve "pragmatic parity" where core features work identically on both platforms; Windows-specific features that have no clean Linux equivalent (UAC elevation, Chromium DPAPI cookie reading) degrade gracefully with clear UI messaging.

This is a Linux **port** at v1, not a Linux feature freeze. macOS, .deb/.rpm/Docker/AppImage packaging, and Linux-specific feature additions (libsecret cookie reading, `pkexec` FFmpeg auto-install) are deferred to follow-on work.

Bundled in this same spec because the touchpoints overlap: an opportunistic UX improvement to the auto-cookies browser-selection UI (browser dropdown with custom path support), and a fix for a long-standing bug where the Windows launcher's `schtasks`-based deferred cleanup of `.exe~` orphans never actually fires.

## Non-goals

- macOS support (Keychain replaces DPAPI, code-signing/notarization friction, .app bundles)
- `.deb`/`.rpm`/AppImage/Flatpak distribution formats
- Docker image (can be added later if there's demand; users can write their own 4-line Dockerfile)
- `pkexec`-based automatic FFmpeg installation on Linux (too brittle across desktop environments)
- libsecret-backed Chromium cookie reading on Linux (Firefox covers the common path; manual cookies.txt is documented fallback for Chromium)
- Switching the existing per-binary signature scheme to `checksums.txt` + single signature (worth doing on its own merits, but uncoupled from this work)
- systemd unit files (users write their own; we don't lock in a path)

## Scope decisions

| Decision | Choice | Rationale |
|---|---|---|
| Target platforms | Linux x64 + arm64 | x64 covers servers/NAS/desktops; arm64 unlocks free-tier ARM cloud (Oracle Ampere, AWS Graviton) more than Pis (RAM-constrained for Moombox's 300-400 MB footprint) |
| Feature parity level | Pragmatic | Core features work; gaps degrade with clear messages, not silently |
| Distribution format | Raw binaries only (no tarballs) | Mirrors Windows model. Linux users `wget && chmod +x && ./moombox`. The `chmod` is one-time; auto-updater preserves +x via existing `0o755` write mode |
| BotGuard Node sidecar | Embed for both platforms | Hermetic install matches "no runtime deps beyond FFmpeg" promise. Linux x64 + arm64 each get their own pinned Node binary embedded |
| Code-signing | Per-binary Ed25519 (existing scheme) | 6 release assets total: 3 binaries + 3 sigs. Auto-updater is the only consumer of `.sig` files; tarballs would need extra extraction logic in the updater |

## Release assets per tag

```
Moombox.exe              + Moombox.exe.sig
moombox-linux-amd64      + moombox-linux-amd64.sig
moombox-linux-arm64      + moombox-linux-arm64.sig
```

6 assets total. Windows users on existing 2.6.2 update with zero impact: the asset names `Moombox.exe` / `Moombox.exe.sig` stay byte-identical, so the existing exact-match asset lookup in `updater.go:138-141` continues to find them. New Linux assets are silently skipped by old clients.

## Design

### 1. Build matrix and CI

Split `.github/workflows/release.yml` into two parallel jobs:

**`windows`** — runs on `windows-latest`. Reuses every existing step (sidecar build, fetch-node Windows blob, go-winres resource generation, build, sign, release upload). Asset names stay `Moombox.exe` / `Moombox.exe.sig`.

**`linux`** — runs on `ubuntu-latest`. Builds both arches via Go cross-compilation:

```yaml
- name: Build Linux executables
  env:
    CGO_ENABLED: 0
  run: |
    GOOS=linux GOARCH=amd64 go build -ldflags "..." -o moombox-linux-amd64 ./cmd/moombox
    GOOS=linux GOARCH=arm64 go build -ldflags "..." -o moombox-linux-arm64 ./cmd/moombox

- name: Sign Linux binaries
  env:
    SIGNING_KEY: ${{ secrets.SIGNING_KEY }}
  run: |
    go run ./cmd/sign moombox-linux-amd64
    go run ./cmd/sign moombox-linux-arm64
```

Both jobs upload to the same GitHub release.

Add a separate **`linux-test`** workflow on PR/push (not just tag): runs `go build ./...` and `go test ./...` on `ubuntu-latest`. Catches Linux regressions before they reach a release tag.

The existing Windows go-winres / `.syso` generation step skips automatically on the Linux job because the `.syso` files use Go's filename build constraints (`rsrc_windows_amd64.syso`).

### 2. `tools/fetch-node` — multi-platform Node binaries

Today `tools/fetch-node/main.go` fetches a single Windows x64 Node binary, gzips it, writes to `internal/bgutils/embed/node.exe.gz`, and writes `version.txt`.

Extend to fetch all three platforms in one run:

```go
type nodeTarget struct {
    goos, goarch string
    archiveExt   string  // "zip" for windows, "tar.xz" for linux
    binaryName   string  // "node.exe" or "node"
    embedName    string  // "node-windows-amd64.gz" / "node-linux-amd64.gz" / "node-linux-arm64.gz"
    expectedSHA  string  // pinned per-platform SHA-256
}
```

Each target fetched independently, SHA-verified, gzipped. `version.txt` becomes a manifest of all three (`node@v22.22.2 windows-amd64@<sha> linux-amd64@<sha> linux-arm64@<sha>`) so cache-invalidation triggers on any platform's SHA change.

The Linux Node releases use `.tar.xz` archives. Use `archive/tar` + `github.com/ulikunitz/xz` (a pure-Go xz reader; the only third-party dep that handles xz). Or shell out to `tar` via exec.Command — but that fails on a Windows dev box. The pure-Go xz reader is the right choice.

`internal/bgutils/embed/embed.go` switches from a single hardcoded embed:

```go
//go:embed node.exe.gz
var EmbeddedNode []byte
```

…to per-platform embeds behind build tags, with one shared exported variable:

```go
// embed_windows.go
//go:build windows && amd64
//go:embed node-windows-amd64.gz
var EmbeddedNode []byte

// embed_linux_amd64.go
//go:build linux && amd64
//go:embed node-linux-amd64.gz
var EmbeddedNode []byte

// embed_linux_arm64.go
//go:build linux && arm64
//go:embed node-linux-arm64.gz
var EmbeddedNode []byte
```

Sidecar runtime extraction (which already gunzips `EmbeddedNode` on first launch) needs no changes — it sees the same `[]byte` regardless of build target.

The bgutil-sidecar tarball (`internal/bgutils/embed/sidecar.tar.gz`) is platform-agnostic JavaScript and stays a single embed.

### 3. Per-package Linux fallbacks

Several packages have Windows-only files with no `_other.go` companion. They need either a Linux implementation or a stub for the build to succeed.

| Package | Current state | Linux plan |
|---|---|---|
| `internal/disk/disk_windows.go` | `GetDiskFreeSpaceExW` syscall | New `internal/disk/disk_unix.go` (`//go:build !windows`) using `syscall.Statfs(path, &stat)`; `Free = stat.Bavail * uint64(stat.Bsize)`, `Total = stat.Blocks * uint64(stat.Bsize)`. Pure stdlib. |
| `internal/web/routes/ffmpeg_elevation_windows.go` | `ShellExecuteEx` UAC + `OpenProcessToken` | New `ffmpeg_elevation_other.go` (`//go:build !windows`) with stubs: `isElevated()` returns `os.Geteuid() == 0`; `runElevated()` returns `errors.New("elevation not supported on this platform")`; `waitForProcess()` returns nil. The `runtime.GOOS != "windows"` guards in `ffmpeg.go` already prevent install endpoints from firing on non-Windows, so these stubs only exist to make the package compile. |
| `cmd/moombox/launcher.go` | Windows-coupled (`syscall.SysProcAttr.CreationFlags = createNoWindow`, `.exe~` rename, `schtasks` deferred cleanup) | Split into `launcher_windows.go` and `launcher_unix.go`. Shared `launchAndSupervise()` core extracted into `launcher.go` with platform-specific helpers. Details in §6. |
| `cmd/moombox/main.go` | `createNoWindow = 0x08000000` package-level const | Move definition to `launcher_windows.go` since only Windows code uses it. The constant is currently referenced only from `launcher.go` (`deferDeleteOldLauncher`'s schtasks/ping invocations). File-manager spawning in `monitor_callbacks.go`/`tui_wiring.go` already branches on `runtime.GOOS` to use `explorer`/`open`/`xdg-open` and doesn't apply `createNoWindow`. |
| `internal/cookies/autocookies.go` | Already cross-platform (`isWindows()` check around `taskkill`) | No change. |
| `internal/cookies/dpapi/*` | Already has `_other.go` stubs | No change. |
| `internal/utils/winsec_windows.go` | Already has `_other.go` stub | No change. |
| `internal/bgutils/sidecar/job_windows.go` | Already has `_other.go` stub | No change. |

### 4. Single-instance lock on Linux

Replace the existing `single_instance_other.go` no-op stub with `single_instance_unix.go`:

```go
//go:build !windows

package main

import (
    "fmt"
    "os"
    "path/filepath"
    "syscall"
)

var lockFile *os.File

func acquireSingleInstanceLock() error {
    lockDir := filepath.Join(os.Getenv("HOME"), ".local", "share", "moombox")
    if err := os.MkdirAll(lockDir, 0o700); err != nil {
        return fmt.Errorf("create lock dir: %w", err)
    }
    lockPath := filepath.Join(lockDir, "moombox.lock")
    f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
    if err != nil {
        return fmt.Errorf("open lock file: %w", err)
    }
    if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
        f.Close()
        return fmt.Errorf("another moombox instance is already running (lock held on %s)", lockPath)
    }
    // Write our PID for human debugging
    f.Truncate(0)
    f.Seek(0, 0)
    fmt.Fprintf(f, "%d\n", os.Getpid())
    lockFile = f  // hold for process lifetime
    return nil
}

func releaseSingleInstanceLock() {
    if lockFile != nil {
        syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
        lockFile.Close()
        lockFile = nil
    }
}
```

`flock` releases automatically on process death (kernel-managed), so a crashed Moombox can't leave a stale lock — same guarantee as the Windows mutex.

If `$HOME` is unset (rare; happens in some service contexts), the lock falls back to `/tmp/moombox.lock` with a warning logged.

### 5. FFmpeg setup wizard on Linux

The existing FFmpeg install endpoints (`ffmpeg.go`) already refuse on non-Windows with "automatic installation is only supported on Windows". Keep that. Linux-side change is purely UI:

**Backend:**
- New endpoint `GET /api/ffmpeg/install-suggestion` returns the package-manager command for the user's distro.
- Detection reads `/etc/os-release`'s `ID=` and `ID_LIKE=` keys at first call (cached for process lifetime — distro doesn't change at runtime).
- Mapping table:
  | Distro family | Command |
  |---|---|
  | debian, ubuntu | `sudo apt install ffmpeg` |
  | fedora, rhel, centos | `sudo dnf install ffmpeg` |
  | arch, manjaro | `sudo pacman -S ffmpeg` |
  | alpine | `sudo apk add ffmpeg` |
  | opensuse, suse | `sudo zypper install ffmpeg` |
  | other | (return empty; UI shows generic link to https://ffmpeg.org/download.html) |

**Frontend:**
- The setup wizard's FFmpeg step on Linux replaces the Chocolatey/winget buttons with a copy-pasteable `<sl-input readonly>` showing the suggested command, plus a "Copy" button.
- Below: a "I've installed FFmpeg, recheck" button that calls the existing `GET /api/ffmpeg/check`.

**TUI:** matching change in the setup screen — display the suggestion text and a recheck action.

### 6. Launcher (cross-platform restructure)

Current `launcher.go` is Windows-coupled. Split into three files:

**`launcher.go`** (shared, no build tags):
- `launchAndSupervise()` — entry point, contains the spawn/wait/restart loop
- Calls platform helpers `cleanupOrphans(exePath)` and `handleUpdateRestart(exePath)` defined per-platform

**`launcher_windows.go`** (`//go:build windows`):
- `cleanupOrphans(exePath)`: existing line-50 logic — `os.Remove(exePath + "~")`. Plus the new `~` sweep proposed in §7B (defense in depth, harmless if file doesn't exist).
- `handleUpdateRestart(exePath)`: the existing `.old → ~` rename block. Necessary because Windows can't delete a locked binary.
- `deferDeleteOldLauncher(exePath)`: **revert from broken schtasks back to the proven ping/timeout approach** — see §7A.
- `createNoWindow` constant lives here.
- `setSysProcAttr(cmd)`: applies `CreationFlags: createNoWindow` to a `*exec.Cmd` — used by the launcher and any other Windows-only background spawn.

**`launcher_unix.go`** (`//go:build !windows`):
- `cleanupOrphans(exePath)`: empty — no orphans on Linux.
- `handleUpdateRestart(exePath)`: empty — child's `CleanupOldBinary` removes `.old` at startup, no rename needed because Linux can `os.Remove` a locked-but-renamed file.
- `deferDeleteOldLauncher(exePath)`: empty.
- `setSysProcAttr(cmd)`: empty.

Build-tag verification: `go build` on Linux must not pull in any `syscall.SysProcAttr` Windows fields. The `createNoWindow` constant and any `syscall.SysProcAttr{CreationFlags: ...}` literal moves to `launcher_windows.go`.

### 7. Updater cleanup improvements

#### A. Restore proven `~` cleanup (Windows)

The current `deferDeleteOldLauncher` uses `schtasks` to schedule a one-shot delete task ~10s after launcher exit. In practice this never fires reliably:

1. Task name `MoomboxCleanup_Moombox.exe~` contains `~`, may be rejected by schtasks
2. `cleanup.Start()` only checks fork success, never schtasks's exit code — failure is invisible
3. `/st HH:MM:SS` with no `/sd` (start date) wraps incorrectly when `now+10s` crosses midnight
4. Nested quoting in `/tr "cmd /c del \"...\" & schtasks /delete \"...\""` is brittle for paths with spaces
5. Task may run as a context (SYSTEM vs interactive user) that lacks delete permission on user-private files

**Replace with a single `cmd /C` invocation using `timeout.exe`** (built into Windows since Vista, always present):

```go
func deferDeleteOldLauncher(exePath string) {
    oldPath := exePath + "~"
    if _, err := os.Stat(oldPath); err != nil {
        return
    }
    // Pass as a single command string to cmd /C so & is unambiguously
    // cmd's operator. 11-second wait is enough for the launcher to
    // exit and release its file lock.
    delayedDel := fmt.Sprintf(
        `timeout /t 11 /nobreak >nul & del /f /q "%s" >nul 2>nul`, oldPath)
    cleanup := exec.Command("cmd", "/C", delayedDel)
    cleanup.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
    cleanup.Start()  // detached, fire-and-forget
}
```

Why this works where schtasks didn't:
- `timeout.exe` is in-box, no scheduler involved
- Single command string eliminates Go arg-parsing ambiguity around `&`
- `createNoWindow` keeps the cmd window invisible
- Inherits launcher's user context, so it has the same delete permissions
- 11s gives the launcher time to exit cleanly before `del` fires

#### B. Add `~` sweep to `CleanupOldBinary` (Windows only, defense in depth)

`updater.go:331` currently sweeps `.old`, `.new`, `.new.sig`, `.sig`. Add a Windows-only `~` sweep so any orphan that escaped both the launcher's startup `os.Remove` AND the deferred timeout/del still gets caught at the next child startup:

```go
func (u *Updater) CleanupOldBinary() {
    suffixes := []string{".old", ".new", ".new.sig", ".sig"}
    if runtime.GOOS == "windows" {
        suffixes = append(suffixes, "~")
    }
    for _, suffix := range suffixes {
        // ... existing logic unchanged
    }
}
```

The `runtime.GOOS == "windows"` guard prevents accidentally targeting a Linux user's `moombox~` editor backup file (extremely unlikely to coexist with a binary, but cheap to be safe).

#### C. Auto-updater asset matching (platform-aware)

Replace the hardcoded `Moombox.exe` / `Moombox.exe.sig` lookup at `updater.go:138-141` with a platform table:

```go
type assetNames struct{ binary, sig string }

var releaseAssetMap = map[string]assetNames{
    "windows/amd64": {binary: "Moombox.exe", sig: "Moombox.exe.sig"},
    "linux/amd64":   {binary: "moombox-linux-amd64", sig: "moombox-linux-amd64.sig"},
    "linux/arm64":   {binary: "moombox-linux-arm64", sig: "moombox-linux-arm64.sig"},
}

func currentPlatformAssets() (assetNames, bool) {
    key := runtime.GOOS + "/" + runtime.GOARCH
    a, ok := releaseAssetMap[key]
    return a, ok
}
```

The asset-iteration loop becomes:

```go
assets, ok := currentPlatformAssets()
if !ok {
    return nil, fmt.Errorf("auto-update unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
}
for _, asset := range release.Assets {
    switch {
    case strings.EqualFold(asset.Name, assets.binary):
        downloadURL = asset.BrowserDownloadURL
    case strings.EqualFold(asset.Name, assets.sig):
        signatureURL = asset.BrowserDownloadURL
    }
}
```

`VerifyCurrentSignature` (`updater.go:293`) gets the same platform-aware lookup. The Windows-side asset name stays `Moombox.exe` so existing 2.6.2 clients update normally.

`os.OpenFile(dest, ..., 0o755)` at `updater.go:365` already sets the executable bit; no change needed for Linux.

### 8. Browser-selection dropdown (Windows + Linux)

Currently `DetectBrowser()` returns the single best-match browser. The auto-cookies setup UI shows whichever was detected; the user has no override.

**Backend changes (`internal/cookies/autocookies_detect.go`):**

- New function `DetectBrowsers()` returns `[]DetectedBrowser` — same iteration as `DetectBrowser()` but doesn't stop at first match. Order: Firefox-family first, then Chromium-family, with the system default browser promoted to position 0.
- New config fields in `[cookies]` section:
  - `browser_path string` — when empty, auto-pick via `DetectBrowser()`. When set, use this exact executable path.
  - `browser_type string` — overrides type detection. Required when `browser_path` is set, because the path alone doesn't tell us "Firefox vs Chrome" if it's a custom binary.
- New validation function `ValidateBrowserPath(path, browserType string) error`:
  - File exists and is regular file
  - File is executable (`os.Stat` mode bits include `0o111`)
  - `--version` runs successfully within 10s timeout
  - `browser_type` is in the known list (firefox, waterfox, librewolf, zen, chrome, brave, edge, vivaldi, thorium, opera)

**API changes (`internal/web/routes/cookies.go` or wherever `GET /api/auto-cookies/status` lives):**

- Extend `AutoCookieStatus` JSON with:
  ```go
  type AutoCookieStatus struct {
      // ... existing fields
      AvailableBrowsers []DetectedBrowser `json:"availableBrowsers"`
      // ConfiguredBrowserPath echoes config value so UI can show it
      ConfiguredBrowserPath string `json:"configuredBrowserPath,omitempty"`
      ConfiguredBrowserType string `json:"configuredBrowserType,omitempty"`
  }
  ```
- New endpoint `POST /api/auto-cookies/validate-browser-path`:
  - Body: `{path, type}`
  - Returns 200 with `{valid: true, version: "..."}` or 400 with `{error: "..."}`
  - Used by frontend before saving config to give immediate feedback

**Frontend changes (`web/public/modules/setup.js` + `settings.js`):**

Replace the current auto-detected-browser display with a `<sl-select>`:

```html
<sl-select label="Browser for cookie extraction" name="browser_path">
  <sl-option value="">Auto-detect (recommended)</sl-option>
  <!-- For each detected browser: -->
  <sl-option value="/usr/bin/firefox" data-type="firefox">
    Firefox <small>/usr/bin/firefox</small>
  </sl-option>
  <!-- ... -->
  <sl-option value="__custom__">Custom path…</sl-option>
</sl-select>
<!-- When __custom__ selected, reveal: -->
<sl-input id="custom-browser-path" placeholder="/path/to/browser/binary"></sl-input>
<sl-select id="custom-browser-type" label="Browser type">
  <sl-option value="firefox">Firefox-family</sl-option>
  <sl-option value="chrome">Chromium-family</sl-option>
</sl-select>
```

On save: PUT `/api/config` with `cookies.browser_path` (and `browser_type` if custom). The backend validates and rejects with a clear error if invalid.

Default selection logic: use configured `browser_path` if set; otherwise use the first detected browser (Firefox-family preferred, matching `DetectBrowser` ordering); show "Auto-detect" as the explicit zero-state.

**TUI parity (`internal/tui/setup_*.go`):**

Settings/setup screen gets a matching `huh.NewSelect[string]()`. Same option list as web UI. Custom path entry uses a text input that follows the select.

### Wiring (post-implementation note)

The configured browser flows from config → service via a callback bridge:

1. User saves `cookies.browser_path` and `cookies.browser_type` via PUT `/api/config` (web) or settings save (TUI).
2. The handler validates with `cookies.ValidateBrowserPathQuick` and persists to `cfg.Cookies` under the configStore lock.
3. At service init (`cmd/moombox/services.go`), `AutoCookieService.ConfiguredBrowserOverride` is set to a closure over `s.configStore.Read(...)` that returns the current values.
4. `AutoCookieService.resolvedBrowser()` consults the override before falling back to `DetectBrowser()`. Both `StartSetup` and `RefreshCookies` use `resolvedBrowser()` instead of `DetectBrowser()` directly.
5. Because `ConfiguredBrowserOverride` reads via `configStore.Read` on every call (no captured snapshot), config changes take effect on the next refresh tick without requiring a service restart.

This wiring was not in the original spec but emerged during round-2 review (which discovered the override mechanism was needed but missing) and round-4 review (which discovered the PUT /api/config handler was silently dropping the new fields).

### 9. Config defaults on Linux

Confirm the existing config-load lookup (`internal/config/config.go`) works on Linux:

1. `./config.toml` (cwd)
2. `./config/config.toml`
3. `$XDG_CONFIG_HOME/moombox/config.toml` (or `~/.config/moombox/config.toml` if XDG unset)

No code changes expected — existing logic is platform-agnostic. Verify with a Linux smoke test in CI.

Default output directory: `./downloads/` relative to binary, same on both platforms. Linux users running under systemd can override to `/var/lib/moombox/downloads`.

### 10. Documentation

**New `BUILDING.md`** (separate from README):

Sections:
- **Prerequisites** — Go 1.25+, Node 22 LTS (for sidecar build only), FFmpeg in PATH (for testing)
- **One-time setup** — `go run ./tools/fetch-node` (fetches all 3 Node binaries), `cd bgutil-sidecar && npm ci --omit=dev && node build.mjs && cd ..` (builds sidecar tarball)
- **Build commands**:
  - Windows: `go build -o Moombox.exe ./cmd/moombox`
  - Linux x64: `GOOS=linux GOARCH=amd64 go build -o moombox-linux-amd64 ./cmd/moombox`
  - Linux arm64: `GOOS=linux GOARCH=arm64 go build -o moombox-linux-arm64 ./cmd/moombox`
  - Cross-compile from Windows: same env vars work
- **Resource embedding (Windows only)** — go-winres step (icon, manifest, version info)
- **Signing for releases** — `cmd/sign` usage, key generation, GitHub Actions secret setup
- **CI overview** — links to `.github/workflows/release.yml` and `linux-test.yml`

**`README.md` updates:**

- Requirements section: add Linux (x64/arm64) alongside Windows
- Quick Start: add Linux install steps
  ```bash
  wget https://github.com/.../releases/latest/download/moombox-linux-amd64
  chmod +x moombox-linux-amd64
  ./moombox-linux-amd64
  ```
- Move detailed build steps from "Building from Source" to BUILDING.md, leave a 2-line pointer in README

### 11. Release notes & in-app rendering

Two improvements bundled because they touch the same release/update flow:

#### A. Add Linux download links to GitHub release body

The release body currently puts a single Windows download link at the top:
```
[**`Download Moombox.exe for Windows (x64)`**](.../Moombox.exe)

---

<RELEASE_NOTES.md content>
```

Update `.github/workflows/release.yml`'s "Build release body" step to emit all three platform download links:
```bash
WIN_LINK="[**\`Download Moombox.exe for Windows (x64)\`**](https://github.com/${REPO}/releases/download/${TAG_NAME}/Moombox.exe)"
LIN_AMD_LINK="[**\`Download moombox-linux-amd64 for Linux (x64)\`**](https://github.com/${REPO}/releases/download/${TAG_NAME}/moombox-linux-amd64)"
LIN_ARM_LINK="[**\`Download moombox-linux-arm64 for Linux (arm64)\`**](https://github.com/${REPO}/releases/download/${TAG_NAME}/moombox-linux-arm64)"

{
  echo "$WIN_LINK"
  echo "$LIN_AMD_LINK"
  echo "$LIN_ARM_LINK"
  echo ""
  echo "---"
  echo ""
  cat RELEASE_NOTES.md
} > release_body.md
```

The body construction step belongs in whichever job runs `softprops/action-gh-release` to create the release. With the parallel windows/linux job split, the Windows job creates the release with the body, and the Linux job uploads its assets to the existing release (action-gh-release v2 handles incremental asset uploads against the same tag).

#### B. Strip download links + render markdown in app

**Problem 1**: The Web UI's update dialog (`app.js:819`) sets `notes.textContent = releaseNotes`, displaying the full GitHub body including the now-redundant download links at the top.

**Problem 2**: The body is markdown but rendered as plain text — `### Features`, `**bold**`, `[link](url)` all show as literal syntax characters.

**Server-side changes (`internal/updater/updater.go`):**

- Strip the download-link section from `release.Body` before exposing as `ReleaseNotes`. Split on `\n---\n`; keep only what comes after:
  ```go
  body := release.Body
  if i := strings.Index(body, "\n---\n"); i >= 0 {
      body = strings.TrimSpace(body[i+len("\n---\n"):])
  }
  ```
- Add a new `ReleaseNotesHtml` field to `ReleaseInfo`. Render the stripped markdown to sanitized HTML:
  ```go
  type ReleaseInfo struct {
      // ... existing fields
      ReleaseNotes     string `json:"releaseNotes"`     // stripped raw markdown (for TUI)
      ReleaseNotesHtml string `json:"releaseNotesHtml"` // sanitized HTML (for web UI)
  }
  ```
- Use `github.com/yuin/goldmark` for markdown→HTML conversion and `github.com/microcosm-cc/bluemonday` (UGC policy) for HTML sanitization. Both are stable, well-maintained, no CGo. Combined add ~600 KB to binary.

**Web UI changes (`web/public/app.js:819` and `index.html:1680`):**

- Swap `notes.textContent = ...` for `notes.innerHTML = data.releaseNotesHtml || ""`. The HTML is sanitized server-side via bluemonday, so direct innerHTML assignment is safe.
- Add a small CSS section in `moombox.css` for `#update-release-notes h1, h2, h3, ul, li, code, a` so the rendered markdown actually looks styled. Use existing Shoelace tokens (`--sl-color-primary-600`, `--sl-spacing-small`) so it matches the rest of the UI.

**TUI changes (new overlay component):**

The TUI today has no display surface for release notes — only a `⬆ Update!` indicator next to the version (`internal/tui/job_details.go:670`) and a feedback toast `Update available: <tag> — press R U to install` (`internal/tui/app_update.go:212`). `R U` triggers `ApplyUpdate` directly with no notes preview, so users update blind. We need to add the surface, not just the renderer.

- New file `internal/tui/release_notes_overlay.go` — modal overlay component modeled after `internal/tui/help.go`'s pattern: embedded `bubbles/viewport` for scrolling (with `helpViewportKeyMap()` to disable letter-key bindings that conflict with chords), lipgloss-bordered frame with title bar and footer.
- New chord `R N` (Request → Release Notes) added to `buildMenuItems()` and `dispatchAction()` in `internal/tui/app_actions.go`. Conditional on `updateAvailable != nil`.
- Inside the overlay: `U` applies the update directly (delegates to the same `OnApplyUpdate` callback used by `R U`), `Esc`/`Q` closes without applying, arrow/PgUp/PgDn scrolls the rendered notes.
- Body content rendered via `github.com/charmbracelet/glamour`:
  ```go
  r, _ := glamour.NewTermRenderer(
      glamour.WithAutoStyle(),       // auto-detects light/dark terminal
      glamour.WithWordWrap(width),   // size-aware word wrap
  )
  rendered, _ := r.Render(rawMarkdown)
  ```
- Glamour is part of the charmbracelet ecosystem already in use (Bubble Tea + Bubbles + Huh + Lip Gloss), so it's a natural fit. Adds ~200 KB.

This brings the TUI to feature parity with the web UI's existing update dialog: both surfaces now show the rendered notes before the user commits to applying an update.

**API stability:** the new `releaseNotesHtml` field is additive. The existing `releaseNotes` field stays (now contains stripped raw markdown — the previous "raw GitHub body with download links" was rarely useful anyway). Existing 2.6.2 clients hitting the new releases endpoint receive both fields; their UI ignores `releaseNotesHtml` (unknown field) and continues using `releaseNotes` as before. They'll see the stripped version (no download links) which is a strict improvement.

## Component summary

| Component | Files touched | Change shape |
|---|---|---|
| CI workflow | `.github/workflows/release.yml`, new `linux-test.yml` | Split jobs, new linux build/sign step, separate test workflow |
| Node sidecar embed | `tools/fetch-node/main.go`, `internal/bgutils/embed/embed*.go` | Multi-platform fetch, build-tagged embeds |
| Disk space | `internal/disk/disk_unix.go` (new) | Statfs-based implementation |
| FFmpeg elevation | `internal/web/routes/ffmpeg_elevation_other.go` (new) | Stub returning errors |
| Launcher | `cmd/moombox/launcher.go`, `launcher_windows.go`, `launcher_unix.go` | Restructure, fix schtasks bug, simplify Linux path |
| Single-instance | `cmd/moombox/single_instance_unix.go` (replaces `_other.go`) | flock-based |
| Updater asset matching | `internal/updater/updater.go` | Platform-aware lookup table |
| Updater cleanup sweep | `internal/updater/updater.go` | Add `~` for Windows |
| FFmpeg distro suggestion | `internal/web/routes/ffmpeg.go`, `web/public/modules/setup.js`, TUI setup screen | New endpoint + UI |
| Browser dropdown | `internal/cookies/autocookies_detect.go`, `internal/cookies/autocookies.go`, `internal/config/config.go`, web routes, frontend modules, TUI settings | New `DetectBrowsers()`, config fields, validation endpoint, UI components |
| Release body Linux links | `.github/workflows/release.yml` | Add Linux x64 + arm64 download links to body |
| Release notes rendering | `internal/updater/updater.go`, `internal/web/routes/update.go`, `internal/web/routes/jobs.go`, `web/public/app.js`, `web/public/moombox.css`, new `internal/tui/release_notes_overlay.go`, `internal/tui/app.go`, `internal/tui/app_actions.go`, `internal/tui/app_keys.go`, `internal/tui/app_layout.go`, `README.md` (chord doc) | Strip download links, server-side goldmark+bluemonday → HTML for web UI, new TUI overlay (bubbles/viewport + glamour) opened via R N chord with U-to-apply / Esc-to-close |
| Documentation | New `BUILDING.md`, updated `README.md` | Linux-specific install + build steps |

## Compatibility

- **Existing Windows 2.6.2 users**: zero impact at update time. `Moombox.exe` / `Moombox.exe.sig` asset names stay byte-identical, so the existing exact-match asset lookup finds them. New Linux assets are silently ignored by the old client. Auto-update from 2.6.2 → next-version proceeds normally.
- **Browser dropdown**: new `cookies.browser_path` and `browser_type` fields are optional (empty defaults to current auto-detect behavior). No migration needed for existing configs.
- **Updater asset matching**: the new platform-aware lookup table includes Windows entry mapping `windows/amd64 → Moombox.exe`. So even after the user updates to the new version, Windows updates continue working.
- **Release notes API**: `releaseNotesHtml` is a new additive field. Existing 2.6.2 clients ignore unknown fields and continue using `releaseNotes`, which now contains the stripped raw markdown — strict UX improvement (no more redundant download-link clutter for them either).

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Linux Node binary embed bloats binaries | Each platform only embeds its own Node (~30 MB compressed). Linux x64 binary doesn't carry Linux arm64's Node, etc. |
| `flock` lockfile under `$HOME` fails in service contexts | Fall back to `/tmp/moombox.lock` with warning log |
| Custom browser path validation runs `--version` (could be malicious binary) | Validation runs with a 10s timeout. Browser is launched per user choice, same trust model as today's auto-detect (which also runs whatever's in PATH). |
| Restored ping/timeout deferred cleanup spawns visible cmd window on some systems | `createNoWindow` flag suppresses it. Behavior matches the pre-schtasks era which worked for users for a long time. |
| Linux distro detection fails for unusual distros | Fall back to generic ffmpeg.org link. No blocker — user can install however they want. |
| arm64 cross-compile produces broken binary on edge cases | `linux-test` workflow runs `go build ./...` for both arches on every PR. Detects compile failures pre-tag. |
| goldmark/bluemonday/glamour deps add ~600 KB to binary | One-time cost; no runtime overhead. Markdown rendering is sub-millisecond per release. Acceptable for the polish gain. |
| Markdown XSS via crafted RELEASE_NOTES.md | Bluemonday UGC policy strips scripts, event handlers, and dangerous protocols. Source is our own RELEASE_NOTES.md, but defense-in-depth applies. |

## Implementation order suggestion

The implementation plan (separate doc, written next) will sequence these. Rough order:

1. Per-package Linux fallbacks (disk, ffmpeg_elevation) — gets the build to compile on Linux
2. Launcher restructure — fixes the launcher bug AND enables Linux launcher
3. Single-instance flock — Linux-only
4. `tools/fetch-node` multi-platform — needed before any Linux build can succeed end-to-end
5. Embed package build-tag split — paired with fetch-node change
6. Updater asset matching + cleanup sweep — small, low-risk
7. CI workflow split + linux-test workflow — ties everything together
8. Browser dropdown UX (independent, can be parallelized)
9. FFmpeg distro suggestion (independent)
10. Release notes rendering — workflow change for Linux download links + server-side markdown render + web UI/TUI display update (independent)
11. Documentation
