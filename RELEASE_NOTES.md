### Features

- **Linux support.** Moombox now runs on Linux x64 and Linux arm64 alongside the existing Windows x64 build. Pragmatic feature parity: core download pipeline, web dashboard, TUI, BotGuard sidecar, and YouTube/Twitch extraction all work identically; Windows-only features (UAC elevation, Chromium DPAPI) degrade gracefully with clear UI messaging. Linux x64 covers servers/NAS/desktops; arm64 covers free-tier ARM cloud (Oracle Ampere, AWS Graviton) and capable ARM single-board computers.
- **Browser-selection dropdown for auto-cookies.** Settings → Cookies now lists every detected browser (Firefox, Waterfox, LibreWolf, Zen, Chrome, Brave, Edge, Vivaldi, Thorium, Opera) plus a "Custom path…" option for non-standard installs. Backend validates paths via `--version` smoke test before save. TUI gets matching `browser_path` / `browser_type` fields under Cookies settings.
- **Rendered release notes in the update dialog.** The web dashboard's update-available dialog now renders Markdown (headings, lists, code, links) instead of showing raw `### Features` syntax. Server-side rendered via goldmark + bluemonday for sanitization. TUI gets a new release-notes overlay accessed via the `R N` chord — scrollable with arrow keys / pgup-pgdn, press `U` to apply update or `Esc`/`Q` to close.
- **Distro-aware FFmpeg install suggestion on Linux.** The setup wizard's FFmpeg step on Linux now reads `/etc/os-release` and shows the matching package-manager command (apt/dnf/pacman/apk/zypper) including derivative distros like Mint and Pop!_OS. TUI shows the same suggestion in its FFmpeg-not-found view.

### Bug Fixes

- **Windows `.exe~` orphans now actually clean up.** The previous schtasks-based deferred cleanup was silently failing in practice (task names containing `~` were rejected, time-format wraparound bugs, no exit-code check). Reverted to the proven `timeout /t 11 /nobreak & del` approach that worked historically. `~` orphans from prior failed cleanups also get swept by `CleanupOldBinary` at next launch as belt-and-suspenders.

### Internal

- **Per-platform Node.js binaries embedded** at build time via `tools/fetch-node` (now multi-platform: Windows x64, Linux x64, Linux arm64) with build-tagged embed files. Each platform's binary only carries its own ~30-43 MB Node blob.
- **Platform-aware updater asset matching** — auto-update lookup table maps GOOS/GOARCH to release asset names. Windows entry preserves `Moombox.exe` so existing 2.6.2 clients update transparently.
- **Launcher restructured** into `launcher_windows.go` / `launcher_unix.go` with shared core. Linux launcher needs no orphan cleanup (Linux can delete a running binary directly).
- **Linux single-instance lock** via `flock` on `~/.local/share/moombox/moombox.lock` with `/tmp` fallback. Same lifetime guarantees as the Windows named-mutex.
- **Connectivity monitor split** — Windows keeps `InternetGetConnectedState`; Linux uses TCP dial to `1.1.1.1:443` with 2s timeout (a real reachability check vs the Windows heuristic).
- **CI workflow split** into parallel `windows` + `linux` jobs. New `linux-test.yml` runs `go build ./...` and `go test ./...` for both Linux arches on every PR/push to catch regressions before tag.
- **6 release assets per tag** — `Moombox.exe`, `moombox-linux-amd64`, `moombox-linux-arm64`, plus `.sig` for each. Release body lists all three download links.
- **New `BUILDING.md`** with cross-platform build instructions; README updated with Linux quickstart (`wget && chmod +x && ./moombox-linux-{amd64,arm64}`).
