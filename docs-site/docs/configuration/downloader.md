# Downloader

The `[downloader]` section controls how each recording is fetched and named:
the output file name, which video quality is picked, how many downloads and
segment fetches run at once, how much memory a download may use for
out-of-order data, whether chat is saved, and how long Moombox waits on a
stalled or interrupted YouTube livestream before it finalizes.

```toml
[downloader]
output_template = "${channel}/${start_date} ${title} [${id}]"
max_video_resolution = 2160
num_parallel_downloads = 10
segment_workers = 12
reorder_buffer_mb = 1024          # 256 on Linux arm64
reorder_budget_mb = 4096          # 1024 on Linux arm64
progress_interval_ms = 16
download_chat = true
prefer_60fps = true
prefer_hdr = true
maximum_timeout = 600
interruption_timeout = 120
incomplete_staging_expiry_days = 7
```

!!! info "When changes take effect"
    No key on this page needs a restart. Most of them are read when a job
    **starts a run** (a new job, or a Resume, Mux or retry of an existing
    one): a run that is already going keeps the values it started with.
    [`num_parallel_downloads`](#downloadernum_parallel_downloads) and
    [`reorder_budget_mb`](#downloaderreorder_budget_mb) apply as soon as you
    save, [`reorder_buffer_mb`](#downloaderreorder_buffer_mb) applies to the
    next buffer a download builds, and
    [`incomplete_staging_expiry_days`](#downloaderincomplete_staging_expiry_days)
    is read each time the orphaned-files list is built.

    If you edit `config.toml` by hand, Moombox reads it at the next start.
    An invalid value in a hand-edited file (a number below its minimum, an
    empty template) is replaced with the default at load and a warning is
    logged; the dashboard and the TUI refuse an invalid value when you save.
    Editing the file while Moombox runs is not picked up, and a later save
    from either UI writes the running settings back over your edit — stop
    Moombox first.

In the web dashboard these settings are under **Settings → Downloader**. In
the terminal UI, open Settings with ++grave++ and pick the **Downloader**
section. The guide pages [Jobs](../guide/jobs.md) and
[Storage and files](../guide/storage.md) explain the behaviour these keys
tune.

## File naming

### `downloader.output_template`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `${channel}/${start_date} ${title} [${id}]` | Non-empty text, at most 500 bytes | Next job run | Dashboard: Settings → Downloader → **Output Template** (with a live preview). TUI: Settings → Downloader → **Output template** (preview line under the row). Also in both first-run wizards. |

The path of each archive, relative to the output directory, **without** the
file extension. Moombox always writes an `.mp4` archive and adds the
extension itself, along with the names of the files that sit beside it
(`.chat.json`, the thumbnail, and so on — see
[Storage and files](../guide/storage.md)).

Five variables are replaced:

| Variable | Value |
|---|---|
| `${title}` | The video or stream title, cleaned (see below). |
| `${id}` | YouTube: the 11-character video ID. Twitch: Moombox's job ID — `tw_<stream id>` for a live capture, `tw_v<VOD id>` for a VOD, `tw_clip_<slug>` for a clip. |
| `${channel}` | The channel's display name, cleaned (see below). |
| `${start_date}` | The stream's start date as `YYYYMMDD`. Falls back to the time the job was created when there is no start time. |
| `${start_time}` | The same time as `HHMM` (24-hour). |

Anything else in the template is kept literally, including text that looks
like a variable but is not one of the five. A `/` or `\` in the template
creates subfolders — the default puts each channel in its own folder.

How values are cleaned and fitted:

- **Title and channel are filtered to a safe set of characters.** Kept:
  ASCII letters and digits, `_`, `-`, spaces, CJK ideographs (Chinese
  characters and Japanese kanji), hiragana, katakana, CJK punctuation and
  full-width forms. **Everything else is removed**, including punctuation
  such as `. , ' ! ? : ( ) [ ]`, emoji, Korean Hangul, and accented or
  other non-Latin letters (`é`, Cyrillic, Greek, and so on).
  A title like `Karaoke! (Part 2)` becomes `Karaoke Part 2`.
- The literal text of the template is **not** filtered, which is why the
  default's `[` `]` around the ID survive. Avoid characters Windows forbids in
  file names (`: * ? " < > |`) in your literal text.
- Dates and times are in the **host's local time zone** (on Docker, the
  container's `TZ`), not UTC.
- Long names are shortened so they fit file-system limits: the title is
  capped at 180 bytes and the channel at 200 bytes. A folder name may then
  be at most 255 bytes and the archive's own name at most 210 bytes (room is
  left for the longest suffix Moombox adds beside it, such as
  ` - part2.chat.json`). A component still too long has its title and
  channel shortened, but never below 30 bytes each. The ID and your literal
  text are not shortened at that step, so two archives stay distinguishable;
  only a template whose literal text alone is too long gets the component
  cut as a last resort.
- A path component that would be a reserved Windows device name (`CON`,
  `NUL`, `COM1`, and so on, with or without an extension) gets a leading
  underscore on every platform.

When to change it: to flatten the folder layout, add the time of day, or
put the date first for sorting. Keep `${id}` somewhere in the name — two
streams with the same title on the same day would otherwise collide.

Edge cases:

- An empty template, or one longer than 500 bytes in a hand-edited file, is
  replaced with the default at load.
- The template is resolved when a job's run starts. Files already written are
  never renamed when you change it; a job that is resumed or muxed again
  later resolves the template afresh.
- The directory the template's path is placed in is the job's output
  directory: [`paths.output_directory`](paths-and-logs.md#pathsoutput_directory)
  or a channel's own
  [`output_directory`](channels.md#channelsoutput_directory).

```toml
[downloader]
# 20261113 2130 Some Stream Title [dQw4w9WgXcQ].mp4, one folder per channel
output_template = "${channel}/${start_date} ${start_time} ${title} [${id}]"
```

## Quality

### `downloader.max_video_resolution`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `2160` | `0` (unbounded) or any whole number ≥ 1 | Next job run | Dashboard: Settings → Downloader → **Max Video Resolution** (presets: *Unbounded — always the largest*, 480p, 720p, 1080p, 1440p, 4K (2160), 8K (4320), *Custom…* which reveals **Custom Max Resolution**). TUI: Settings → Downloader → **Max resolution** (++left++ / ++right++ step through the same presets; any number can be typed). Also in both first-run wizards. |

The highest resolution Moombox records, measured on the frame's **shorter
edge**: `1080` means 1920×1080 for a landscape stream and 1080×1920 for a
portrait one, and `2160` means 4K either way.

Moombox takes the largest rendition at or below the cap. If a stream offers
nothing that small, it takes the closest rendition **above** the cap rather
than recording nothing. `0` removes the cap: the largest rendition on offer
is always taken. It applies to YouTube and Twitch alike, including Twitch
clips.

A channel's or a job's own
[quality preference](channels.md#channelsquality_preference) (for example
`720p60`) is applied within this cap: a job that asks for 720p under a 2160
cap gets 720p.

When to change it: lower it to save disk space and bandwidth (1080 is a
common choice), or set `0` if you want 8K sources kept at full size.

Edge cases: a negative value in a hand-edited file is reset to `2160`. A
value that is not one of the presets is kept as typed; the TUI's preview line
shows it as *Custom: N*.

```toml
[downloader]
max_video_resolution = 1080
```

### `downloader.prefer_60fps`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `true` | `true` / `false` | Next job run | Dashboard: Settings → Downloader → **Prefer 60fps**. TUI: Settings → Downloader → **Prefer 60fps**. |

When two renditions have the same resolution, record the 60 fps one rather
than the 30 fps one. Resolution always comes first: a 1080p30 rendition still
wins over 720p60. Applies to YouTube and Twitch.

When to change it: turn it off if you want smaller files, or if your player
struggles with high-frame-rate video. A quality preference that names a
frame rate (such as `1080p60`) asks for that rate whatever this says.

```toml
[downloader]
prefer_60fps = false
```

### `downloader.prefer_hdr`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `true` | `true` / `false` | Next job run (the manual format picker reads it on each request) | Dashboard: Settings → Downloader → **Prefer HDR**. TUI: Settings → Downloader → **Prefer HDR**. |

YouTube only. When an HDR and an SDR rendition share the same resolution and
frame rate, record the HDR one. `false` keeps SDR, for players and screens
that cannot show HDR (HDR played on an SDR screen without tone mapping looks
washed out). Dolby Vision renditions rank below both either way.

!!! note "Audio"
    Audio is chosen by its own ranking, independent of this switch. A 5.1
    E-AC-3 or AC-3 track ranks above stereo Opus or AAC, and most browsers
    cannot play those — the dashboard player says so when it meets one.
    VLC or mpv play the file.

```toml
[downloader]
prefer_hdr = false
```

## Power

### `downloader.keep_awake`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `true` | `true` / `false` | Immediately (no restart) | Dashboard: Settings → Downloader → **Keep the PC awake while recording (Windows only)**. TUI: Settings → Downloader → **Keep PC awake (Windows)**. |

Windows only. A recording makes no keyboard or mouse input, so a PC whose
power plan allows sleep can go to sleep in the middle of a capture. While
this is on, Moombox asks Windows not to sleep on its idle timer whenever at
least one job is `Live`, `Downloading` or `Muxing`, and lets it sleep again
as soon as none is. Jobs waiting in `Queued` or `Upcoming` do not hold the
PC awake, since they can wait for days.

The screen can still turn off, and sleeping by hand (Start → Sleep, closing
a laptop lid) is not blocked. On Linux and in Docker the setting has no
effect.

```toml
[downloader]
keep_awake = false
```

## Concurrency and memory

### `downloader.num_parallel_downloads`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `10` | Whole number ≥ 1 (no maximum) | Immediately | Dashboard: Settings → Downloader → **Parallel Downloads**. TUI: Settings → Downloader → **Parallel downloads**. |

How many **VOD** downloads (past broadcasts, uploads, premieres, clips)
may run at once across all channels. Live broadcasts never wait on this
limit — a stream that goes live always starts recording at once — so the
total number of downloads running can be the number of live streams plus
this value.

A VOD that is waiting for a slot keeps its status (usually `Upcoming`) and
shows the progress line *Waiting for a download slot... (3m)*. The **Mux**
action and recovering set-aside recordings also take a slot each.

When to change it: lower it on a slow connection, a small machine or a
spinning disk; raise it if you have bandwidth to spare and a large backlog.
Per-channel backlog pacing is a separate limit,
[`monitors.archive_slots`](monitors.md#monitorsarchive_slots).

Edge cases: a value below 1 in a hand-edited file is reset to `10`.

```toml
[downloader]
num_parallel_downloads = 3
```

### `downloader.segment_workers`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `12` | Whole number ≥ 1 (no maximum) | Next job run | Dashboard: Settings → Downloader → **Segment Workers**. TUI: Settings → Downloader → **Segment workers**. |

How many segments **one** download fetches at the same time. This is not the
number of downloads — that is
[`num_parallel_downloads`](#downloadernum_parallel_downloads). It matters
when a download has many segments available at once: catching up on a live
YouTube stream you joined late (or after a restart), and downloading a VOD
that is served as segments. More workers catch up faster.

When to change it: lower it if downloads start failing with HTTP 403 errors
or YouTube starts refusing requests; raise it carefully on a fast
connection.

!!! warning "Bot-detection risk"
    There is deliberately no upper limit, but a wide burst of simultaneous
    requests to YouTube is the kind of traffic that attracts bot detection.
    Above 16, Moombox logs a warning at startup and whenever a save changes
    the value: *downloader.segment_workers N is high — a large simultaneous
    fan-out to YouTube raises bot-detection risk; reduce it if downloads
    start returning 403*.

Edge cases: a value below 1 in a hand-edited file is reset to `12`.

```toml
[downloader]
segment_workers = 8
```

### `downloader.reorder_buffer_mb`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `1024` (`256` on Linux arm64) | `0` (unbounded) or any whole number of megabytes (no maximum) | The next reorder buffer a download builds (a running catch-up or VOD download keeps the limit it started with) | Dashboard: Settings → Downloader → **Reorder Buffer Per Job (MB)**. TUI: Settings → Downloader → **Reorder buffer per job**. |

When segments are fetched in parallel, later segments often arrive before an
earlier one that is still being retried. They are held in memory until the
gap is filled so the file is written in order. This is the most memory one
download may hold that way. When it is full, the download's other workers
wait for the slow segment instead of fetching further ahead — it slows down
rather than failing. The segment everything waits on is always let in, so a
download can go one segment over the limit.

The default is lower on 64-bit ARM Linux (for example a Raspberry Pi) because
those machines usually have little memory.

When to change it: lower it if Moombox uses too much memory during
parallel catch-up; raise it, or set `0`, on a machine with plenty of RAM.

Edge cases:

- A negative value in a hand-edited file is reset to the default.
- If this value is larger than
  [`reorder_budget_mb`](#downloaderreorder_budget_mb) (and neither is `0`),
  it is treated as equal to the budget, and a warning naming both keys is
  logged at startup and on each save that leaves them that way. The file is
  not rewritten.

```toml
[downloader]
reorder_buffer_mb = 512
```

### `downloader.reorder_budget_mb`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `4096` (`1024` on Linux arm64) | `0` (unbounded) or any whole number of megabytes (no maximum) | Immediately | Dashboard: Settings → Downloader → **Reorder Budget Total (MB)**. TUI: Settings → Downloader → **Reorder budget total**. |

The same out-of-order memory as
[`reorder_buffer_mb`](#downloaderreorder_buffer_mb), but **added up across
every download** running in the process. Without it, ten parallel VOD
downloads could each fill their own per-job buffer. A download waits when
either its own buffer or this shared budget is full.

When to change it: set it to a comfortable share of the machine's free
memory. On a dedicated archiving box with lots of RAM you can set `0`.

Edge cases: a negative value in a hand-edited file is reset to the default.

```toml
[downloader]
reorder_budget_mb = 2048
```

### `downloader.progress_interval_ms`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `16` | Whole number of milliseconds ≥ 1 (no maximum) | Next job run | `config.toml` only — neither the dashboard nor the TUI shows it. |

The minimum time between two progress updates of one job — the counters and
percentages you see moving in the dashboard and the TUI. The default of 16 ms
is about 60 updates a second per job.

Each update is a database write and a message to every connected dashboard,
so a lower value is smoother but costs more CPU and disk activity, and a
higher one is lighter. `8` (about 120 a second) suits a fast machine; `250`
or `1000` suits a slow one.

Edge cases: `0` does **not** mean "unlimited" here — anything below 1 is reset
to `16` at load. Because no UI exposes it, change it by stopping Moombox,
editing `config.toml` and starting it again.

```toml
[downloader]
progress_interval_ms = 100
```

## Chat

### `downloader.download_chat`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `true` | `true` / `false` | Next job run | Dashboard: Settings → Downloader → **Download Live Chat**. TUI: Settings → Downloader → **Download chat**. |

Save the chat alongside each recording: YouTube live chat and chat replay,
and Twitch chat for live captures and VODs. The chat is written as
`<name>.chat.json` beside the archive (and, for YouTube, a
`<name>.chat.raw.jsonl` of the chat events that are not messages). The
dashboard player replays it over the video — see
[Player and chat replay](../guide/player.md).

When to change it: turn it off if you never watch chat and want to save the
extra requests and disk space.

```toml
[downloader]
download_chat = false
```

## Stalls and interruptions (YouTube livestreams)

### `downloader.maximum_timeout`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `600` (seconds) | Whole number of seconds ≥ 30 (no maximum) | Next job run | Dashboard: Settings → Downloader → **YouTube Maximum Timeout (seconds)**. TUI: Settings → Downloader → **YouTube max timeout**. |

YouTube livestreams only. When a live recording stops receiving new segments,
Moombox keeps retrying — checking every 30 seconds whether the stream has
ended — for this long, and then finalizes the recording even if YouTube
still reports the stream as live. The clock restarts whenever a segment
arrives.

When to change it: raise it for streamers whose broadcasts often freeze for
several minutes and then continue; lower it to finalize faster after a
stream that YouTube fails to mark as ended.

!!! warning
    Very large values keep polling YouTube for a long time after a stream
    has really stopped, which can have negative consequences for your
    account.

Edge cases: a value below 30 in a hand-edited file is reset to `600`.

```toml
[downloader]
maximum_timeout = 900   # 15 minutes
```

### `downloader.interruption_timeout`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `120` (minutes) | Number of minutes ≥ 0, fractions allowed; `0` disables the wait (no maximum) | Next job run | Dashboard: Settings → Downloader → **Interruption Resume Timeout (minutes)**. TUI: Settings → Downloader → **Interruption resume timeout**. |

When a YouTube broadcast is interrupted — the streamer's connection drops
but YouTube says the broadcast may still resume — this is how long Moombox
may hold the recording open waiting for it to come back before finalizing
what it has. While it waits, the job shows
*Stream interrupted - waiting for resume... (12m 30s)*.

If the broadcast does not come back in time, the archive is finalized and
marked **Incomplete tail**. Its staging files and resume data are kept, so
the missing end can still be added later: by **Resume**, or automatically
when the monitor sees the same broadcast live again (see
[Jobs](../guide/jobs.md)). How long those staging files are protected is
[`incomplete_staging_expiry_days`](#downloaderincomplete_staging_expiry_days).

`0` turns off the wait only: finalize never holds the recording open, but an
interrupted recording is still marked and its staging kept, exactly as when
a wait gives up.

In `config.toml` you can also write a duration string such as `"90m"`,
`"2h"` or `"1d"`; it is converted to minutes.

Edge cases: a negative value in a hand-edited file is reset to `120`.

```toml
[downloader]
interruption_timeout = "3h"
```

### `downloader.incomplete_staging_expiry_days`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `7` (days) | Number of days ≥ 0, fractions allowed; `0` keeps staging forever (no maximum) | Immediately (read each time the orphaned-files list is built) | Dashboard: Settings → Downloader → **Incomplete Staging Expiry (days)**. TUI: Settings → Downloader → **Incomplete staging expiry**. |

How long a `Finished` job keeps its staging folder protected when it was
finalized with an **Incomplete tail** (so Resume can append the rest) or with
an **incomplete chat capture** (the copy kept in staging may be the only full
copy of the chat). Until it expires, that staging is never offered for
deletion.

After expiry the staging folder appears in the dashboard's **Orphaned** tab
(TUI: ++a++ ++o++, *Browse Orphaned Items*) like any other orphan. Nothing is
deleted automatically; you decide. Once it is gone, Resume and the automatic
resume can no longer append the tail — **Reinitialize** (which re-downloads
from the start) is the remaining option. The *Incomplete tail* badge itself
never expires.

Age is counted from the job's last update, so any activity on the job (an
automatic resume attempt, a metadata refresh) restarts the clock. This
window does **not** apply to staging that holds footage found nowhere else —
set-aside recordings, unmuxed parts, recordings a mux could not carry whole —
which stay protected with no age limit until they are recovered or the job
is deleted. See [Storage and files](../guide/storage.md).

When to change it: YouTube cannot resume a broadcast days later, so a week
is generous. Lower it to reclaim space sooner; set `0` to keep such staging
until you remove it yourself. In `config.toml` a duration string such as
`"2w"` or `"36h"` is also accepted and converted to days.

Edge cases: a negative value in a hand-edited file is reset to `7`.

```toml
[downloader]
incomplete_staging_expiry_days = 3
```
