# Storage and files

This page describes everything Moombox keeps on disk: where finished
archives go and how they are named, what the staging directory holds and
when it is cleaned up, which files sit beside each archive, how disk space
is watched, and where the database and the logs live.

## Where things live

| What | Default location | Setting |
|---|---|---|
| Finished archives and their companion files | `./output` | [`paths.output_directory`](../configuration/paths-and-logs.md#pathsoutput_directory), or a channel's [`output_directory`](../configuration/channels.md#channelsoutput_directory) |
| Recordings in progress | `./staging` | [`paths.staging_directory`](../configuration/paths-and-logs.md#pathsstaging_directory) |
| Database (`moombox.db`, `moombox.db-wal`, `moombox.db-shm`) and `open-alerts.json` | `./moombox.db` | [`paths.database_path`](../configuration/paths-and-logs.md#pathsdatabase_path) |
| Log file, its rotated copies and the crash file | `./moombox.log` | [`paths.log_file_path`](../configuration/paths-and-logs.md#pathslog_file_path) |
| Settings | `config.toml` | See [Configuration](../configuration/index.md) |
| Cookies | `./cookies.txt` | [`cookies.cookie_file`](../configuration/cookies.md#cookiescookie_file) |
| Browser profile for automatic cookie login | `./browser-profile` | [`cookies.browser_profile_dir`](../configuration/cookies.md#cookiesbrowser_profile_dir) |

Relative paths are resolved against the folder Moombox was **started
from**. On Docker, everything above is seeded under the `/data` volume.

Two more places are outside these settings:

=== "Windows"

    - `%LOCALAPPDATA%\Moombox\sidecar` — the unpacked BotGuard helper
      (Node.js and its scripts). Moombox recreates it if you delete it.
    - The single-instance lock, `moombox.lock`, is also kept per user under
      `%LOCALAPPDATA%`.

=== "Linux"

    - `~/.cache/Moombox/sidecar` (or under `$XDG_CACHE_HOME`) — the unpacked
      BotGuard helper. Moombox recreates it if you delete it.
    - `~/.local/share/moombox/moombox.lock` — the single-instance lock, and
      `moombox-daemon.json` beside it, which tells `moombox add` which
      database the running Moombox has open.

=== "Docker"

    The image sets `HOME=/data`, so the helper cache lives in
    `/data/.cache` on the same volume as everything else.

## Output

### How archives are named

Each archive's path inside the output directory comes from
[`downloader.output_template`](../configuration/downloader.md#downloaderoutput_template).
The default,

```text
${channel}/${start_date} ${title} [${id}]
```

puts every channel in its own folder and gives files names like

```text
output/
└── Some Channel/
    ├── 20261113 Late Night Karaoke [dQw4w9WgXcQ].mp4
    ├── 20261113 Late Night Karaoke [dQw4w9WgXcQ].chat.json
    ├── 20261113 Late Night Karaoke [dQw4w9WgXcQ].chat.raw.jsonl
    ├── 20261113 Late Night Karaoke [dQw4w9WgXcQ].jpg
    └── 20261113 Late Night Karaoke [dQw4w9WgXcQ].description
```

The variables are `${title}`, `${id}`, `${channel}`, `${start_date}`
(`YYYYMMDD`) and `${start_time}` (`HHMM`), with dates in the host's local
time zone. Titles and channel names are filtered to ASCII letters and
digits, `_`, `-`, spaces and Chinese/Japanese characters (CJK ideographs,
kana, CJK punctuation and full-width forms), so most punctuation, emoji,
Korean Hangul and accented letters disappear from file names. The settings page has the full rules,
including how long names are shortened.

The output directory and the template's subfolders are created when the
first archive is written. Nothing appears in the output directory until a
recording is muxed: FFmpeg writes the archive straight from staging.

!!! note "Which output directory a job uses"
    A job found by a channel monitor uses the output directory (the
    channel's own, or the global one) that is set when its download starts.
    Changing `paths.output_directory` or a channel's folder therefore also
    reaches jobs still waiting in `Upcoming` or `Queued`. A job that has
    started or finished keeps the directory it started with.

### Files written beside each archive

`<name>` below is the template's result, for example
`Some Channel/20261113 Late Night Karaoke [dQw4w9WgXcQ]`.

| File | When it is written |
|---|---|
| `<name>.mp4` | Always: the archive. Moombox always writes MP4, with chapters embedded when the source has them (YouTube chapter markers or description timestamps, Twitch VOD moments or category changes). |
| `<name>.chat.json` | When [`downloader.download_chat`](../configuration/downloader.md#downloaderdownload_chat) is on and chat was captured. Used by the player's chat replay. |
| `<name>.chat.raw.jsonl` | YouTube chat only: every chat event that is not a message (banners, polls, deletions and the like), one JSON object per line. |
| `<name>.jpg` (or `.webp`, `.png`) | The thumbnail. For YouTube the best available size is fetched, 16:9 sizes first; failing that, the job's own thumbnail link is tried, and only a job with no thumbnail link at all gets the channel avatar. |
| `<name>.description` | The video's description as plain text, when it has one. |
| `<name> - part1.mp4`, `<name> - part2.mp4`, … | Instead of one `<name>.mp4`, when a live recording had to be split into parts (each part is muxed as soon as it ends). The chat, raw chat, thumbnail and description of a split recording are written once, under the plain `<name>` the parts share. A recording that ends with only one part is renamed to the plain `<name>.mp4`. |
| `<name>.chapters.json` | The chapter list of a split recording (its parts carry none of their own), or a chapter list you imported (see [Chapters](chapters.md)). |
| `<name>.restart-<timestamp>.mp4` | One of two things (see [Jobs](jobs.md)): a set-aside recording that was recovered into its own file, which overlaps the start of the archive; or the complete earlier archive of a job you reinitialized, which the reset keeps under this name and never replaces. |
| `trim/<video id> [<start>s-<end>s].mp4` | Trims, in a `trim` folder beside the archive (see [Player and chat replay](player.md)). |

Deleting a job does **not** remove any of these files; they become orphans
(see [Orphaned files](#orphaned-files)).

!!! note "`.moombox-tmp-` files while a mux runs"
    FFmpeg never writes a final name directly. While it writes an archive, a
    part, a recovered recording or a trim, the folder holds a temporary file
    named `<name>.moombox-tmp-<run>-<n>.mp4` (the run code and the number
    differ each time). Only when FFmpeg finishes successfully and Moombox
    has checked the result is it renamed to its final name, so a file under
    a final name is never half-written. If the mux fails or is cancelled,
    the temporary file is deleted. Leave these files alone while a job is
    `Muxing` or a trim is being saved; one left behind by a crash or a forced stop is deleted at the
    next start (see [At startup](#when-staging-is-cleaned-up)).

## Staging

Every job works in its own folder, `<staging directory>/<job id>/` — for
example `staging/dQw4w9WgXcQ/` or `staging/tw_123456789/`. While a job
downloads, that folder holds:

- the video and audio being downloaded, and a `.resume.json` beside each
  that records how far it got, so a restart or a Resume can continue rather
  than start over;
- `chat.json` and its resume data, and for YouTube `chat.raw.jsonl`;
- the thumbnail, and for Twitch the chapter material collected while live;
- `seg_1`, `seg_2`, … subfolders when the recording is split into parts;
- occasionally a **set-aside recording** (a file ending in
  `.restart-<timestamp>`): footage Moombox could not continue after a
  restart, kept rather than overwritten.

!!! warning "Do not edit staging by hand"
    The file names and layout are Moombox's own bookkeeping. Deleting or
    renaming files in a job's staging folder while the job is still in the
    list can make Resume start over or Mux produce a broken file. Use the
    [Orphaned tab](#orphaned-files) instead.

### When staging is cleaned up

After a successful mux
:   The job's staging folder is deleted — unless it holds something that
    exists nowhere else, in which case it is kept and the job details say
    why:

    | Kept because | For how long |
    |---|---|
    | The recording has an **Incomplete tail** (Resume can append the rest) | [`downloader.incomplete_staging_expiry_days`](../configuration/downloader.md#downloaderincomplete_staging_expiry_days), 7 days by default |
    | The **chat capture ended incomplete**, or the chat could not be copied beside the archive (staging is pruned down to the chat capture) | The same expiry |
    | It holds a **set-aside recording** that could not be muxed | Until it is recovered or the job is deleted |
    | It holds a **part** that could not be muxed (the Mux action finishes it) | Until it is muxed or the job is deleted |
    | It holds a **complete recording the finalize did not use** (it can be the longer copy) | Until the job is deleted |
    | A mux had to **leave damaged bytes out**, or could not verify its copy | Until the job is deleted |

    "Kept" means it is never listed as an orphan. Once the expiry runs out,
    the folder is listed in the Orphaned tab like any other; nothing is
    deleted automatically.

After `Error` or `Cancelled`
:   Staging is kept. It is what **Resume** continues and what **Mux** turns
    into an archive. **Reinitialize** deletes it.

At startup
:   Moombox deletes leftovers that are provably redundant and logs each one:
    temporary repair copies an interrupted mux left behind, the
    `.moombox-tmp-` files FFmpeg was writing beside an archive when Moombox
    crashed or was killed (only those no running Moombox is still writing),
    set-aside recordings whose recovered `.restart-<timestamp>.mp4` file is already in
    the output directory, and the whole staging folder of a `Finished` job
    whose archive files are all on disk and that no rule above keeps (for
    example, when Moombox stopped between finishing the mux and deleting the
    folder). Anything else is left for you to review.

!!! tip "Plan staging space for your longest stream"
    A recording occupies its full size in staging until it is muxed, and the
    mux then writes a second full-size copy into the output directory. If
    both are on the same disk, a long 4K stream briefly needs about twice
    its size free.

## Orphaned files

The dashboard's **Orphaned** tab (TUI: ++a++ ++o++, *Browse Orphaned Items*)
lists what no job needs any more. Click **Refresh** to rescan.

**Orphaned Files** lists:

- **staging** folders that belong to no job, to a `Finished` job whose
  staging is not kept, to a `Queued`, `Error`, `Cancelled` or `COOKIES?`
  job, or whose keep period has expired. The job's title and status are shown when
  the job still exists, and any set-aside recordings inside are named so you
  can see you are about to delete footage;
- **output** files that no job names, such as the files of a deleted job
  (only under the global output directory — a channel's own
  `output_directory` is not scanned);
- **trim** files that no trim record names.

Delete entries one by one with the trash icon, or **Delete All**. Both ask
first. Folders of active jobs (`Upcoming`, `Live`, `Downloading`, `Muxing`)
are never listed, and a path that a job has claimed since the list was read
is refused with a message asking you to refresh.

!!! warning
    The staging folder of an `Error`, `Cancelled` or `COOKIES?` job is listed
    too, and so is that of a `Queued` backlog video sent back for a retry
    (which resumes from it). It is the only copy of whatever that job
    recorded: deleting it means Resume and Mux have nothing left to work
    with.

**Orphaned History Entries** lists videos that Moombox remembers as already
processed but that no longer have a job — usually jobs you deleted. While an
entry remains, the monitors do not pick that video up again; delete the
entry to let them archive it again. History entries are never dropped for
their age: an entry stays until you delete it here.

## Disk space

Moombox measures the disk that holds the output directory at startup, every
6 minutes after that, and at once when you save a change to the output
directory, the staging directory or the thresholds. The dashboard shows a
disk gauge and the TUI shows usage in its status bar. When the staging
directory is on a different disk, that disk is measured too: the alerts and
the backlog hold below go by the fuller of the two, while the gauge and the
status bar keep showing the output disk.

| Usage | What happens |
|---|---|
| Below [`disk_warn_percent`](../configuration/disk-memory-connectivity.md#diskdisk_warn_percent) (90 %) | Normal. |
| At or above the warning threshold | A **Disk Space Warning** notification, repeated every 30 minutes while it lasts. |
| At or above [`disk_critical_percent`](../configuration/disk-memory-connectivity.md#diskdisk_critical_percent) (95 %) | A **Disk Space Critical** notification (also every 30 minutes). Backlog `Queued` jobs are no longer admitted (*Held: disk full*), and a chapter import will not re-mux an archive if that would take the disk past the threshold. |
| 2 points below the critical threshold | Backlog admission resumes. If usage is still at or above the warning threshold, a **Disk Space Warning** follows. |
| Below the warning threshold, and 2 points below the threshold that last alerted | **Disk Space Recovered**. |

Live recordings and downloads that are already running are **not**
stopped. If the disk actually fills up, a backlog VOD goes back to `Queued`
to be retried later (see [Jobs](jobs.md#backlog-retries)); other jobs
fail with an error you can Resume or Mux from once you have made room.

If the disk cannot be read at all (an unplugged drive, a network share that
went away), the gauge freezes at its last reading and, after two failed
checks in a row, a **Disk Monitoring Failed** notification is sent; a
**Disk Monitoring Recovered** follows when readings work again. An open disk
alert and the disk-full hold survive a restart.

## Database

The SQLite database holds the job list and everything about each job (its
parts, trims, watched state and playback position), the processed-video
history, each YouTube channel's feed history, and the dashboard's client
tokens. It runs in WAL mode, so while Moombox runs you will see
`moombox.db-wal` and `moombox.db-shm` beside it. `open-alerts.json` in the
same folder remembers open notifications and the backlog holds across
restarts.

- **Backing up:** stop Moombox, then copy `moombox.db` together with its
  `-wal` and `-shm` files if they are there. A copy taken while Moombox is
  writing can be inconsistent.
- **Moving:** stop Moombox, move all of those files, update
  [`paths.database_path`](../configuration/paths-and-logs.md#pathsdatabase_path),
  start Moombox. Pointing the setting at an empty location starts a **new,
  empty** database, and the monitors will then queue everything in the
  archive window again.
- The database does not contain your archives: deleting it loses the job
  list, not the files.

## Logs

All log lines go to [`paths.log_file_path`](../configuration/paths-and-logs.md#pathslog_file_path)
(`moombox.log`), each with its local time (`2026-11-13 21:30:00`), level
and message. When it reaches
[`logs.log_max_file_size`](../configuration/paths-and-logs.md#logslog_max_file_size)
(10 MB) it is rotated to `moombox.log.1`, and up to
[`logs.log_max_files`](../configuration/paths-and-logs.md#logslog_max_files)
(5) rotated copies are kept. How much is logged is
[`logs.log_level`](../configuration/paths-and-logs.md#logslog_level).

`moombox-crash.log` beside it catches the output of a crash that never
reaches the normal log. If a run crashed, the next start logs its beginning
and keeps it as `moombox-crash.log.1`.

You can also read logs without opening the file:

- the dashboard's **Logs** tab and the TUI's log panel show the most recent
  lines (about 200) and then follow new ones live;
- each job's own lines are shown in its details (*Job Logs*) and in the
  TUI's ++o++ ++l++ overlay. These per-job logs are kept in memory only and
  are lost at a restart — the log file has everything.

When reporting a problem, set the level to `DEBUG`, reproduce it, and attach
the relevant part of `moombox.log`. See
[Troubleshooting](../help/troubleshooting.md).
