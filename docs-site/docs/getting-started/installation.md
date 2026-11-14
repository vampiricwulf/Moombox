# Installation

Moombox is a single executable. There is no installer: you download one file,
put it in a folder of its own, make sure FFmpeg is available, and run it. The
web dashboard, the BotGuard helper (a small embedded Node.js runtime used to
mint YouTube proof-of-origin tokens and solve player signatures) and everything
else ship inside that one binary.

If you would rather run it in a container, skip to [Docker](docker.md).

## Supported platforms

| Platform | Release asset | Notes |
|---|---|---|
| Windows x64 | `Moombox.exe` | Windows-only extras: one-click FFmpeg install through Chocolatey or winget, reading cookies from Chromium-family browsers through DPAPI. |
| Linux x64 | `moombox-linux-amd64` | glibc-based distributions (Debian, Ubuntu, Fedora, Arch, …). |
| Linux arm64 | `moombox-linux-arm64` | 64-bit ARM, for example a Raspberry Pi 4/5 running a 64-bit OS. 32-bit ARM is not supported. |

Every release on the [GitHub releases page](https://github.com/vampiricwulf/Moombox/releases/latest)
carries eight assets: the three binaries, a `.sig` signature file for each,
and a signed release manifest (`moombox-manifest.json` and
`moombox-manifest.json.sig`). You only need the binary for your platform; the
other files are what the built-in updater uses to check a download before it
installs it (see [Updating](updating.md)).

!!! note "Linux: glibc is required for the BotGuard helper"
    The Moombox binary itself is statically linked, but the Node.js runtime it
    unpacks for the BotGuard helper is the official nodejs.org Linux build,
    which needs glibc. On a musl-based system such as Alpine the helper cannot
    start, so no proof-of-origin tokens are minted and current YouTube players
    cannot be deciphered. Use a glibc distribution, or the Docker image (which
    is Debian-based).

## Requirements

- **FFmpeg.** Moombox downloads video and audio separately and uses FFmpeg to
  combine (mux) them into the finished file, to trim, and to read chapters and
  audio details (through `ffprobe`, which it looks for next to `ffmpeg`).
  Without FFmpeg, recordings stop at the muxing step. Install a full build that
  includes `ffprobe`. Version 4.0 or newer is recommended: when Moombox checks
  an older FFmpeg it warns "FFmpeg 3.x is outdated — version 4.0+ is
  recommended for reliable muxing". Builds whose version it cannot read (git
  snapshots and some custom builds) are accepted without a warning.
- **Disk space** for the staging directory (segments being downloaded) and the
  output directory (finished archives). A live stream is fully staged before
  it is muxed, so plan for roughly twice the size of your largest recording
  while it is being finalised.
- **A desktop browser** (optional) if you want Moombox to sign in to YouTube
  or Twitch for you by opening a browser window. On a headless server you can
  paste or upload a cookie file instead; see [Cookies and sign-in](../guide/cookies.md).

## Install

=== "Windows"

    1. Download `Moombox.exe` from the
       [latest release](https://github.com/vampiricwulf/Moombox/releases/latest).
    2. Create a folder for it, for example `C:\Moombox` or
       `D:\Archive\Moombox`, and move `Moombox.exe` there.
    3. Double-click `Moombox.exe`. A console window opens with the terminal UI,
       and your default browser opens the web dashboard at
       `http://localhost:774`.

    !!! tip "Pick a folder you own"
        By default Moombox keeps its configuration, database, logs, staging
        and finished downloads in the folder it is started from (see
        [Where Moombox keeps its files](#where-moombox-keeps-its-files)). The
        self-updater also writes the new binary next to the old one. Avoid
        `C:\Program Files`, which needs administrator rights to write to.

=== "Linux x64"

    ```bash
    mkdir -p ~/moombox && cd ~/moombox
    wget https://github.com/vampiricwulf/Moombox/releases/latest/download/moombox-linux-amd64
    chmod +x moombox-linux-amd64
    ./moombox-linux-amd64
    ```

=== "Linux arm64"

    ```bash
    mkdir -p ~/moombox && cd ~/moombox
    wget https://github.com/vampiricwulf/Moombox/releases/latest/download/moombox-linux-arm64
    chmod +x moombox-linux-arm64
    ./moombox-linux-arm64
    ```

You can rename the Linux binary to plain `moombox` if you like; the updater
replaces whatever file is running. Keep it in a directory your user can write
to, for the same reason as on Windows.

On the first start Moombox runs a setup wizard, in the terminal and in the
browser at the same time. Continue with [First run](first-run.md).

## Install FFmpeg

=== "Windows"

    You can let Moombox install it: if FFmpeg is missing, both the dashboard
    and the terminal UI show an **FFmpeg Not Found** screen with an
    **Install FFmpeg** button. It offers **Install via Chocolatey** (or
    **Install Chocolatey + FFmpeg** when Chocolatey is not installed) and,
    when winget is present, **Install via Winget**. When Moombox is not
    running as administrator, it shows you the
    exact script it will run with elevated rights and asks you to
    **Trust & Continue** (a UAC prompt follows) or
    **Distrust & Manually Install**.

    To install it yourself, download a build such as
    `ffmpeg-release-full-shared.7z` from
    [gyan.dev](https://www.gyan.dev/ffmpeg/builds/), unpack it, and either add
    its `bin` folder to your `PATH` or give Moombox the full path to
    `ffmpeg.exe` (see below).

=== "Debian / Ubuntu"

    ```bash
    sudo apt install ffmpeg
    ```

=== "Fedora / RHEL"

    ```bash
    sudo dnf install ffmpeg
    ```

=== "Arch"

    ```bash
    sudo pacman -S ffmpeg
    ```

=== "openSUSE"

    ```bash
    sudo zypper install ffmpeg
    ```

On Linux, Moombox does not install FFmpeg for you. The dashboard's
**FFmpeg Not Found** screen shows the install command for your distribution (read from
`/etc/os-release`) with a **Copy command** button and an
**I've installed FFmpeg, recheck** button.

### Using an FFmpeg that is not on PATH

If FFmpeg is installed somewhere that is not on your `PATH`, point Moombox at
it with the [`paths.ffmpeg_path`](../configuration/paths-and-logs.md#pathsffmpeg_path)
setting. You can set it in any of these places:

- the **FFmpeg Path** field on the Paths step of the Advanced setup wizard;
- the **Custom FFmpeg path** field on the **FFmpeg Not Found** screen, which
  runs the file you name, and saves the path only if it really is a working
  FFmpeg;
- Settings → Paths in the dashboard or the terminal UI.

The file must be named `ffmpeg` (or `ffmpeg.exe`); any other name is refused.
On the **FFmpeg Not Found** screen a pair of surrounding quotes, such as
Windows Explorer's "Copy as path" adds, is removed for you; in the wizard and
in Settings a quoted path is refused, so type it without quotes. `ffprobe` is
expected in the same folder.

## Where Moombox keeps its files

With the default settings every path is relative, and relative paths are
resolved against the **working directory Moombox was started from** — not
against the folder the configuration file is in. Double-clicking `Moombox.exe`
starts it in its own folder, so on Windows everything ends up beside the
executable.

| File or folder | Default | What it is |
|---|---|---|
| `config.toml` | `./config.toml` | All settings. See [Configuration](../configuration/index.md). |
| `moombox.db` | `./moombox.db` | The job database (SQLite, with `-wal`/`-shm` files beside it while running). |
| `open-alerts.json` | beside the database | Alerts that were sent and not yet closed, and backlog holds, kept across restarts. |
| `moombox.log` | `./moombox.log` | The log file, rotated by size. |
| `output/` | `./output` | Finished archives. |
| `staging/` | `./staging` | Segments of downloads in progress. |
| `cookies.txt` | `./cookies.txt` | The Netscape-format cookie file, once you sign in. |
| `browser-profile/` | `./browser-profile` | Browser profile used for automatic sign-in. |
| `moombox.crt`, `moombox.key` | `./` | Self-signed certificate, generated only when HTTPS is enabled. |

Two locations are outside that folder:

- The BotGuard helper's runtime is unpacked once into your user cache folder: `%LOCALAPPDATA%\Moombox\sidecar` on Windows,
  `~/.cache/Moombox/sidecar` on Linux (or under `$XDG_CACHE_HOME`).
- The single-instance lock lives in `%LOCALAPPDATA%\moombox` on Windows and
  `~/.local/share/moombox` on Linux. See
  [One instance per user](../reference/cli.md#one-instance-per-user).

You can move any of the configurable paths; see
[Paths and logs](../configuration/paths-and-logs.md). Which `config.toml` is
used is decided by the search described in
[Command line → Configuration file](../reference/cli.md#configuration-file).

## Running in the background on Linux

Moombox does not ship a service unit, but it runs well under systemd. Run it
headless (no terminal UI), give it a fixed working directory, and let it bind
the dashboard's default port:

```ini title="/etc/systemd/system/moombox.service"
[Unit]
Description=Moombox stream archiver
Wants=network-online.target
After=network-online.target

[Service]
User=moombox
WorkingDirectory=/srv/moombox
ExecStart=/srv/moombox/moombox -no-tui
# Lets an ordinary user bind the default port 774 (ports below 1024 are
# privileged). Drop this line if you set network.port to 1024 or above.
AmbientCapabilities=CAP_NET_BIND_SERVICE
Restart=on-failure
# Moombox needs up to about 15 seconds to save in-progress downloads.
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
```

Things to know:

- Moombox already supervises itself: restarts requested from the dashboard,
  after a settings change or after an update happen inside the process systemd
  started, so systemd sees one long-running service.
- The default dashboard port, 774, is below 1024. On most Linux systems an
  ordinary user may not bind such ports, so without
  `AmbientCapabilities=CAP_NET_BIND_SERVICE` Moombox fails on 774 and on the
  ten ports after it, and the log reports "port 774 (and nearby ports) already
  in use" even though nothing else is using them. Either keep that line or
  set [`network.port`](../configuration/network.md#networkport) to 1024 or
  above before you start the service.
- A startup failure (a configuration it cannot read, a port it cannot bind)
  exits with code 3. `Restart=on-failure` will retry it; fix the cause shown
  in `journalctl -u moombox`.
- The self-updater replaces the binary in place, so `/srv/moombox` must be
  writable by the service user. If you would rather update through your own
  tooling, turn [`updates.auto_check_updates`](../configuration/updates-bgutils-twitch.md#updatesauto_check_updates)
  off.
- `systemctl stop` sends SIGTERM, which starts a graceful shutdown. See
  [Signals and shutdown](../reference/cli.md#signals-and-shutdown).

The first-run wizard on a server: see
[First run → Setting up a headless server](first-run.md#setting-up-a-headless-server).

!!! note "Windows: running without a console"
    When Moombox has no console (for example a Task Scheduler task set to
    "Run whether user is logged on or not"), it detects that and runs
    headless, with only the web dashboard. The single-instance lock still
    prevents a second copy from starting for the same Windows user, even
    from another logon session.

## Building from source

You need Go 1.27 or later, Node.js 24 (only to build the BotGuard helper
bundle) and, at runtime, FFmpeg. From a checkout:

```bash
cd bgutil-sidecar
npm ci --omit=dev
node build.mjs
cd ..
go run ./tools/fetch-node          # downloads ~150 MB of pinned Node.js builds
go build -o moombox ./cmd/moombox  # or -o Moombox.exe on Windows
```

The two preparation steps fill the files the binary embeds; without them the
build fails. `BUILDING.md` in the repository covers cross-compiling and
Windows resource embedding. A locally built binary reports the version
written in the source and the commit it was built from (`moombox --version`).
