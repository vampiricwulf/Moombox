# Paths and logs

Two sections decide where Moombox keeps things on disk and how much it logs.
The `[paths]` section names the database, the log file, the output and
staging directories, and the FFmpeg program. The `[logs]` section sets the
log level and how the log file is rotated.

```toml
[paths]
database_path = "./moombox.db"
log_file_path = "./moombox.log"
output_directory = "./output"
staging_directory = "./staging"
# ffmpeg_path = ""                # empty or absent = ffmpeg from PATH

[logs]
log_level = "INFO"
log_max_file_size = 10485760      # 10 MB
log_max_files = 5
```

!!! info "Relative paths"
    A relative path is resolved against the **working directory of the
    Moombox process** — the folder you started it from — not against the
    folder `config.toml` is in. If you start Moombox from different places
    (a shortcut, a service, a terminal), use absolute paths so it always
    finds the same database and folders. Absolute paths (`/data/output`,
    `D:\Archive`, `\\nas\share\archive`) are fine everywhere.

    The Docker image seeds absolute paths under `/data`
    (`/data/moombox.db`, `/data/moombox.log`, `/data/output`,
    `/data/staging`); see [Docker](../getting-started/docker.md).

!!! info "When changes take effect"
    `database_path`, `log_file_path`, `log_max_file_size` and `log_max_files`
    need a **restart**: both UIs mark them as such, the dashboard offers to
    restart after you save, and the TUI shows a restart banner. `log_level` and `ffmpeg_path` apply at once.
    `output_directory` and `staging_directory` apply to jobs that start after
    the save (see each key for the details).

    If you edit `config.toml` by hand, Moombox reads it at the next start. An
    empty path or an out-of-range log setting in a hand-edited file is
    replaced with its default at load and a warning is logged.

Both UIs apply the same two rules to every path field when you change it:
a path with a `..` segment is refused, and a path wrapped in quotes (as
Explorer's *Copy as path* produces) is refused rather than silently
unquoted — remove the quotes. In the web dashboard these settings are under
**Settings → Paths** and **Settings → Logs**; in the terminal UI, open
Settings with ++grave++ and pick the **Paths** or **Logs** section.

For what Moombox actually writes into these places, see
[Storage and files](../guide/storage.md).

## Paths

### `paths.database_path`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `./moombox.db` | Any non-empty file path | Restart | Dashboard: Settings → Paths → **Database Path**. TUI: Settings → Paths → **Database path**. |

The SQLite database that holds every job, its parts and trims, the list of
videos already processed, the per-channel feed history and the dashboard's
client tokens. It runs in WAL mode, so two companion files sit beside it
while Moombox runs: `moombox.db-wal` and `moombox.db-shm`.

The same directory also holds `open-alerts.json`: the notifications Moombox
has sent and not yet closed (for example an open disk alert), and the two
backlog holds that must survive a restart (the disk-full hold and the YouTube
rate-limit hold).

When to change it: to put the database on a different disk, or beside the
rest of your data so one backup covers everything.

!!! danger "Move the file, don't just change the path"
    If the new path does not exist, Moombox creates a **new, empty**
    database there on the next start. With no history, the monitors treat
    every video in the archive window as new and queue it again. To move the
    database: stop Moombox, move `moombox.db` **together with** its `-wal`
    and `-shm` files (and `open-alerts.json`), change the setting, then
    start Moombox.

Edge cases:

- An empty value in a hand-edited file is reset to `./moombox.db`.
- `moombox add` (see [Command line](../reference/cli.md)) writes to the
  database the running Moombox has open. With no Moombox running, it
  resolves a relative `database_path` against its own working directory and
  refuses if no database is there.
- To back up the database safely, stop Moombox first, then copy
  `moombox.db` together with its `-wal` and `-shm` files if they are there.

```toml
[paths]
database_path = "D:/Moombox/moombox.db"
```

### `paths.log_file_path`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `./moombox.log` | Any non-empty file path | Restart | Dashboard: Settings → Paths → **Log File Path**. TUI: Settings → Paths → **Log file path**. |

The file every log line is written to. Its folder is created if needed.
Rotated copies sit beside it as `moombox.log.1`, `moombox.log.2`, … (see
[`logs.log_max_files`](#logslog_max_files)).

Beside it Moombox also keeps a crash file, named after the log:
`moombox-crash.log` for the default. It catches the output of a fatal crash,
which would otherwise only reach the console. Each start creates it empty;
if the previous run left crash output in it, the next start logs the
beginning of it and keeps it as `moombox-crash.log.1`.

When to change it: to keep logs on another disk, or under `/var/log` or
another log folder your tools watch.

Edge cases: an empty value in a hand-edited file is reset to
`./moombox.log`.

```toml
[paths]
log_file_path = "/var/log/moombox/moombox.log"
```

### `paths.output_directory`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `./output` | Any non-empty directory path | Jobs that start downloading after the save | Dashboard: Settings → Paths → **Output Directory**. TUI: Settings → Paths → **Output directory**. Also in both first-run wizards. |

Where finished archives go. Each archive's path inside it comes from
[`downloader.output_template`](downloader.md#downloaderoutput_template) —
by default one folder per channel. Nothing is written here until a
recording is muxed, so you never see half-finished files in it.

The disk-space gauge and the low-disk alerts watch the volume this
directory is on; a save that changes it takes a new reading at once.

When to change it: point it at the disk or network share with room for
your archive.

Edge cases:

- A channel can have its own
  [`output_directory`](channels.md#channelsoutput_directory), which wins
  over this setting for that channel's jobs.
- A change reaches every job a channel monitor found that has not started
  downloading yet: an `Upcoming` or `Queued` job takes the directory set at
  the moment its download starts. A job that has started, or finished, keeps
  the directory it started with, and so does one you Resume or one that is
  retried. A job you Reinitialize follows the current setting again.
- A waiting job whose channel has since been removed keeps the directory it
  was created with.
- Moombox creates the directory (and the template's subfolders) when it
  writes the first archive. The first-run wizards create it up front and
  refuse a path they cannot create.
- The **Orphaned** tab looks for orphaned archives under this directory
  only; files under a channel's own `output_directory` are not scanned.
- An empty value in a hand-edited file is reset to `./output`.

```toml
[paths]
output_directory = "/mnt/archive/streams"
```

### `paths.staging_directory`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `./staging` | Any non-empty directory path | Jobs started after the save | Dashboard: Settings → Paths → **Staging Directory**. TUI: Settings → Paths → **Staging directory**. Also in both first-run wizards. |

The scratch space for recordings in progress. Each job gets a folder named
after its job ID; segments, chat and resume data are written there and
muxed into the output directory when the recording ends. After a
successful mux the folder is deleted, unless it still holds something worth
keeping. See [Storage and files](../guide/storage.md#staging) for what lives
there and when it is cleaned up.

When to change it: put it on a fast local disk with enough free space for
your largest recordings in progress. On a slow network share, keep staging
local and only the output directory remote.

!!! warning "Changing it while something is staged"
    The new directory is used by jobs that **start** after the save. Mux,
    Resume, Recover Set-aside Recordings and the orphaned-files scan always
    look in the **current** directory, so anything already staged under the
    old one is no longer found by them. Change it when nothing is staged, or
    stop Moombox and move the job folders across to the new location.

Edge cases: an empty value in a hand-edited file is reset to `./staging`.

```toml
[paths]
staging_directory = "/fast-ssd/moombox-staging"
```

### `paths.ffmpeg_path`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| empty (use `ffmpeg` from `PATH`) | Empty, or the path to an FFmpeg executable | Immediately (new muxes, probes and trims) | Dashboard: Settings → Paths → **FFmpeg Path**, with an **Install** button. TUI: Settings → Paths → **FFmpeg path**. |

The FFmpeg program Moombox runs to mux recordings, probe files, merge parts
and cut trims. Leave it empty to use the `ffmpeg` found on the system
`PATH`. Version 4.0 or newer is recommended; an older version is reported
with a warning when it is checked.

If FFmpeg cannot be found when the dashboard loads, it shows an FFmpeg
setup screen instead of the job list. The **Install** button in Settings
opens the same screen:

=== "Windows"

    It can install FFmpeg with Chocolatey (**Install via Chocolatey**) or
    winget (**Install via Winget**) when they are present; without
    Chocolatey it offers **Install Chocolatey + FFmpeg** instead. If Moombox
    is not running elevated, it shows you the install
    script for review and asks Windows for elevation before running it. You
    can also enter the path to an `ffmpeg.exe` you already have.

=== "Linux and Docker"

    Automatic installation is not offered. The screen suggests the install
    command for your distribution (for example `sudo apt install ffmpeg`,
    `sudo dnf install ffmpeg` or `sudo pacman -S ffmpeg`), and
    you can enter a custom path. The Docker image already includes FFmpeg.

A path entered on that screen is checked (Moombox runs it with `-version`)
before it is saved, and takes effect at once.

When to change it: when FFmpeg is not on the `PATH` of the account Moombox
runs as, or you want a specific build.

Edge cases:

- Changing the path in the dashboard requires the program to be named
  `ffmpeg` or `ffmpeg.exe`.
- A mux or trim already running keeps the FFmpeg it started with.
- This key is omitted from `config.toml` while it is empty.

```toml
[paths]
ffmpeg_path = "C:/Tools/ffmpeg/bin/ffmpeg.exe"
```

## Logs

### `logs.log_level`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR` (any letter case) | Immediately | Dashboard: Settings → Logs → **Log Level**. TUI: Settings → Logs → **Log level** (cycles through the four). |

The least severe messages that are logged. `DEBUG` logs everything,
including request-level detail; `INFO` logs normal activity (jobs found,
started, finished); `WARN` only problems Moombox works around; `ERROR` only
failures. The level applies to the log file, the dashboard's **Logs** tab,
the TUI's log panel and each job's own log alike.

When to change it: switch to `DEBUG` while investigating a problem or
before filing a bug report, then back to `INFO` — `DEBUG` is much larger
and makes the log rotate quickly.

!!! tip "One-off debug run"
    Starting Moombox with `-log-level DEBUG` raises the level for that run
    only, without saving it. Saving a different `log_level` from either UI
    ends the override; saving other settings does not. See
    [Command line](../reference/cli.md).

Edge cases: the value is stored in capitals. Anything other than the four
levels in a hand-edited file is reset to `INFO`.

```toml
[logs]
log_level = "DEBUG"
```

### `logs.log_max_file_size`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `10485760` (10 MB) | 1024 – 1073741824 bytes (1 KB – 1 GB) | Restart | Dashboard: Settings → Logs → **Max Log File Size (bytes)**. TUI: Settings → Logs → **Max log file size**. |

The size, in bytes, at which the log file is rotated: the current file is
renamed to `.1`, older copies move up one number, and a new file is started.

When to change it: raise it if you want more history per file (useful at
`DEBUG`), lower it on a small disk.

Edge cases:

- Values from 1024 to 4095 are accepted but treated as 4096, the smallest
  size the logger rotates at; a note is printed to the console at startup.
- A value outside the range in a hand-edited file is reset to 10 MB.
- If a rotation cannot rename the files — on Windows, another program
  holding `moombox.log.1` open (an editor, a log viewer, antivirus) is the
  usual cause — the live file keeps growing past this size rather than
  losing any line. Moombox retries no sooner than a minute later, and
  repeats the complaint about once an hour while it lasts. These
  complaints go to the console (or the TUI log panel), not to the log file
  itself.

```toml
[logs]
log_max_file_size = 52428800   # 50 MB
```

### `logs.log_max_files`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `5` | 1 – 100 | Restart | Dashboard: Settings → Logs → **Max Log Files**. TUI: Settings → Logs → **Max log files**. |

How many rotated copies to keep (`moombox.log.1` up to `moombox.log.5` by
default) besides the live `moombox.log`. Older ones are deleted at each
rotation, and lowering the number removes the extra copies at the next
rotation. The most disk the logs can use is roughly
`(log_max_files + 1) × log_max_file_size`.

Edge cases: a value outside 1–100 in a hand-edited file is reset to `5`.

```toml
[logs]
log_max_files = 10
```
