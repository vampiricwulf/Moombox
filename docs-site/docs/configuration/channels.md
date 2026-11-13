# Channels

The channels you monitor are stored in `config.toml` as an array of tables,
one `[[channels]]` block per channel. Each entry names the channel and can
override several global settings for that channel alone.

```toml
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
name = "Some VTuber"
terms = "(?i)(karaoke|歌枠)"
include_non_live_content = true
archive_window_days = 14

[[channels]]
id = "somestreamer"
name = "Some Streamer"
platform = "twitch"
quality_preference = "1080p60"
twitch_vod_backlog = true
```

You rarely need to edit this by hand. The dashboard (**Settings →
Channels**) and the terminal UI (**Settings → Channels**) add, edit, disable
and remove channels, and resolve URLs and @handles to IDs for you. The guide
page [Channels](../guide/channels.md) walks through both.

!!! info "When changes take effect"
    A channel added, edited, enabled, disabled or removed from the dashboard
    takes effect at once: the save runs a check of all three monitors. In the
    terminal UI the change takes effect when you save the Settings overlay.
    Settings that shape a download (output directory, quality) are copied
    onto each job when the job is created, so a change applies to **jobs
    created after it**, not to jobs that already exist. Hand edits to
    `config.toml` are read at the next start.

!!! note "Validation"
    Moombox refuses, when you save from the dashboard or the TUI, a channel
    whose ID is empty or already in the list (compared ignoring case), an
    override out of range, an unknown quality, or a Twitch-only option on a
    YouTube channel. When it loads a hand-edited `config.toml`, it instead
    corrects such values (each key below says to what) and logs a warning at
    startup.

## Identity

### `channels[].id`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| None (required) | YouTube: a channel ID (`UC` followed by 22 characters). Twitch: the channel's login name. | Immediately | Dashboard: Settings → Channels → **Add Channel** → **Channel ID** (**Channel Login** for Twitch). TUI: Settings → Channels → ++a++ → **Channel ID**. |

The channel Moombox polls. This is the only required key.

In the dashboard and the TUI you can type any of these, and Moombox stores
the resolved ID:

| You type | Stored as |
|---|---|
| `UCxxxxxxxxxxxxxxxxxxxxxx` | as typed |
| `@handle` | the handle's channel ID (looked up on YouTube) |
| `https://www.youtube.com/channel/UC…` | the `UC…` ID |
| `https://www.youtube.com/@handle`, `/c/Name`, `/user/Name` | the channel ID (looked up on YouTube) |
| `youtube.com/...` without `https://` | same as above |
| a Twitch login, e.g. `somestreamer` | as typed |
| `https://www.twitch.tv/somestreamer` (also its `/videos` or `/clips` page) | `somestreamer`, platform set to Twitch |

A YouTube lookup also fills in **Display Name** if you left it empty. A URL
that is not a channel (a watch link, a Twitch VOD link) is refused with "Not
a YouTube or Twitch channel URL".

Edge cases:

- The ID cannot be changed after the channel is added from the dashboard
  (the field is locked in **Edit Channel**). To change it, remove the
  channel and add it again.
- Plain text that is not a URL or an @handle is stored exactly as typed
  (trimmed). A typo there is only noticed when the monitors keep failing to
  reach the channel; see the health notes in
  [Channels](../guide/channels.md).
- In a hand-edited file, surrounding spaces are trimmed at load, and a
  second entry with the same ID (ignoring case) is dropped.
- Hand-edited files are not resolved: write the real `UC…` ID or Twitch
  login, never a URL or @handle.

```toml
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
```

### `channels[].name`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Empty | Any text | Immediately | Dashboard: Add/Edit Channel → **Display Name**. TUI: channel editor → **Display name**. |

A friendly name for the channel, shown in the channel list and in log lines.
When it is empty, the dashboard shows the ID instead.

```toml
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
name = "Some VTuber"
```

### `channels[].platform`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `youtube` (when absent) | `youtube` or `twitch` (lowercase) | Immediately | Dashboard: Add Channel → **Platform**. TUI: channel editor → **Platform** (++left++/++right++). |

Which site the channel is on. It decides which monitors poll it and which of
the platform-only options apply.

Edge cases:

- Pasting a `twitch.tv` URL or a YouTube URL/@handle sets it for you.
- The dashboard locks the platform once the channel exists.
- Write it in lowercase. The YouTube monitors treat every value other than
  exactly `twitch` as YouTube.

```toml
[[channels]]
id = "somestreamer"
platform = "twitch"
```

### `channels[].enabled`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `true` (when absent) | `true` or `false` | Immediately | Dashboard: the switch on the channel's card in Settings → Channels, or **Enabled** in Edit Channel. TUI: channel editor → **Enabled**. |

Pauses a channel without removing it. A disabled channel:

- is skipped by all monitors (no new jobs are created for it);
- has any feed-history backfill paused, keeping its place so enabling it
  again resumes where it stopped;
- gets no archive slots, so its `Queued` backlog waits.

Jobs already created keep going: an `Upcoming` stream is still recorded when
it starts, and running downloads are not stopped. The dashboard dims a
disabled channel's card.

```toml
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
enabled = false
```

## Filtering

### `channels[].terms`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Empty (match everything) | A regular expression string, or a table of named regular expressions | Immediately | Dashboard: Add/Edit Channel → **Filter Pattern (regex)**. TUI: channel editor → **Filter regex**. |

Only archive content whose title matches. Leave it empty to archive
everything the channel streams.

What is matched:

- **YouTube:** the video title (descriptions are not checked).
- **Twitch:** the stream title **or** the game category; either matching is
  enough. The same pattern also filters the
  [Twitch VOD backlog](#channelstwitch_vod_backlog) by video title.
- The [Twitch clip sweep](#channelstwitch_archive_clips) is **not** filtered.
- Videos and streams you add by hand are never filtered.

How patterns work:

- Patterns are RE2 regular expressions (Go's syntax). RE2 has no lookahead
  or back-references, so a pattern cannot say "but not this word".
- Matching is **always case-insensitive** and **ignores accents**: `cafe`
  matches "Café". A leading `(?i)` is accepted but not needed.
- The JavaScript form `/pattern/flags` is also accepted. The `g`, `y`, `u`
  and `d` flags are ignored; `i`, `m` and `s` work.
- A pattern that is not a valid regular expression is matched as plain text
  (a case- and accent-insensitive substring) instead of failing.

The table form, inherited from the original moombox, holds several named
patterns; a title matching **any** of them passes:

```toml
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
terms = { stream = "(?i)(karaoke|singing|歌枠)", collab = "(?i)collab" }
```

The dashboard and the TUI edit only one pattern. The dashboard shows and
edits the table's `stream` entry and keeps the others; a table without a
`stream` entry shows as an empty field there. The TUI shows any table as an
empty field. In both, typing a pattern into an empty field replaces the
whole table with that one pattern, and leaving the field untouched keeps the
table as it is.

```toml
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
terms = "(?i)(karaoke|歌枠|minecraft.*hardcore)"
```

### `channels[].num_desc_lookbehind`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Absent | Ignored | Never | Not shown in either UI |

**Retired. Do not set it.** Older versions used it to match terms against
video descriptions. Terms now match titles only, so nothing reads this key.
Moombox keeps it in your file only so an old config loads and saves
unchanged; you can delete the line.

```toml
# Obsolete — safe to remove:
# num_desc_lookbehind = 5
```

## Archiving scope

### `channels[].include_non_live_content`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `false` | `true` or `false` | Next monitor cycle | Dashboard: Add/Edit Channel → **Also archive uploads & premieres (YouTube only)** (badge **Include VODs** on the card). TUI: channel editor → **Archive uploads & premieres (YouTube only)**. |

YouTube only. By default Moombox records only live and upcoming streams
(including premieres that have not aired yet). Turn this on to also archive
content that is already over:

- regular uploads;
- finished premieres;
- recordings of streams that ended before Moombox caught them live;
- with [`membership_discovery`](monitors.md#monitorsmembership_discovery) and
  YouTube cookies, the same kinds of members-only content.

Only content published inside the channel's
[archive window](#channelsarchive_window_days) qualifies. Newly published
videos start right away; older ones found by the window or the feed-history
backfill are queued as backlog and paced by
[archive slots](#channelsarchive_slots).

Edge cases:

- Hidden for Twitch channels in both UIs and ignored there. For Twitch past
  broadcasts use [`twitch_vod_backlog`](#channelstwitch_vod_backlog).
- A video Moombox has archived (or tried to) before is in its processing
  history and is not queued again. Clear its entry under **Orphaned** to let
  it be picked up again; see [Storage](../guide/storage.md).
- Until Moombox has read the channel's feed or finished its backfill at
  least once, past content is held back, so a channel it cannot see properly
  is never mistaken for an empty one.

```toml
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
include_non_live_content = true
```

### `channels[].archive_window_days`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Absent (use [`monitors.archive_window_days`](monitors.md#monitorsarchive_window_days)) | Whole number, 1–3650; or absent | Next monitor cycle | Dashboard: Add/Edit Channel → **Archive window (days)** (blank = global). TUI: channel editor → **Archive window (days)** (blank = global). |

This channel's own archive window, in days, overriding the global one. The
global page explains the window in full; in short, it is how far back
Moombox archives past content (with
[`include_non_live_content`](#channelsinclude_non_live_content)) and how deep
the feed-history backfill scans. Upcoming and live streams are covered
whatever it says.

Use it to archive one channel's whole catalogue (say `3650`) while keeping a
short window for the rest.

Edge cases:

- Widening it re-scans this channel's history to the new depth
  automatically.
- In a hand-edited file, a value outside 1–3650 (including `0`) is removed
  at load, so the channel falls back to the global window.
- YouTube only; ignored for Twitch channels.

```toml
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
include_non_live_content = true
archive_window_days = 3650
```

### `channels[].archive_slots`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Absent (use [`monitors.archive_slots`](monitors.md#monitorsarchive_slots)) | Whole number, 1–100; or absent | Immediately (next admission sweep) | Dashboard: Add/Edit Channel → **Archive slots** (blank = global). TUI: channel editor → **Archive slots** (blank = global). |

How many of this channel's `Queued` backlog jobs may be in progress at once,
overriding the global value. Applies to YouTube backlog VODs and to Twitch
backlog VODs, clips and pasted listings alike. Live, upcoming and newly
published content never waits for a slot.

Edge cases:

- A disabled channel gets 0 slots whatever this says.
- In a hand-edited file, a value outside 1–100 is removed at load, so the
  channel falls back to the global value.

```toml
[[channels]]
id = "somestreamer"
platform = "twitch"
twitch_vod_backlog = true
archive_slots = 1
```

## Output

### `channels[].output_directory`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Empty (use [`paths.output_directory`](paths-and-logs.md#pathsoutput_directory)) | A folder path | Next job created | Dashboard: Add/Edit Channel → **Output directory** (blank = global). TUI: channel editor → **Output directory** (blank = global). |

Where this channel's finished files go, instead of the global output
directory. The
[`downloader.output_template`](downloader.md#downloaderoutput_template) still
applies beneath it, so with the default template files land in
`<this folder>/<channel>/<date> <title> [<id>].mp4`.

Edge cases:

- The folder is copied onto each job when the job is created. Changing it
  does not move existing files or redirect jobs that already exist.
- A relative path is resolved against Moombox's working directory.
- The disk-space alerts and the backlog's disk gate measure the **global**
  output directory's volume only, not a per-channel folder on another drive.
- In Docker, use a path inside a mounted volume, or the files stay inside the
  container.

```toml
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
output_directory = "D:/Archives/SomeVTuber"
```

### `channels[].quality_preference`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Absent (`best`) | `best`, `2160p60`, `2160p`, `1440p60`, `1440p`, `1080p60`, `1080p`, `900p60`, `900p`, `720p60`, `720p`, `480p`, `360p`, `160p`, `audio_only` | Next job created | Dashboard: Add/Edit Channel → **Quality Preference** (badge **Quality: …** on the card). TUI: channel editor → **Quality preference**. |

The quality to record for this channel, on both YouTube and Twitch.

- `best` (the default; stored as an absent key) records the best quality
  available, within the global
  [`downloader.max_video_resolution`](downloader.md#downloadermax_video_resolution)
  cap.
- A height such as `1080p` or `1080p60` asks for that size. If the stream
  has no rendition at that height, Moombox takes the next lower one, and if
  there is none lower, the best available. A `60` suffix prefers 60 fps at
  that height.
- `audio_only` records only the audio. A YouTube stream served only as
  HLS has no audio-only variant, so Moombox records its lowest-bandwidth
  variant instead and logs a warning; a Twitch stream without an audio-only
  variant falls back to the lowest listed one.

Edge cases:

- The preference is stored on each job when it is created; changing it does
  not affect existing jobs.
- In a hand-edited file an unknown value is removed at load (the channel
  then records `best`).

```toml
[[channels]]
id = "somestreamer"
platform = "twitch"
quality_preference = "720p60"
```

## Twitch-only options

These three switches only exist for Twitch channels. The dashboard and the
TUI show them only when the platform is Twitch. Saving one on a YouTube
channel is refused, and in a hand-edited file it is turned off at load.

### `channels[].twitch_gap_fill_ad_breaks`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `false` | `true` or `false` | Next live capture | Dashboard: Add/Edit Channel → **Fill ad breaks from the archive VOD (Twitch only)** (badge **Fill ad breaks**). TUI: channel editor → **Fill ad breaks from the VOD (Twitch only)**. |

After a live capture finishes, Moombox always fills footage the capture lost
(a late start, a restart, a network outage) from the broadcast's archive VOD,
saved as extra `- recoveredN.mp4` parts beside the archive. With this on, it
also fills the **ad breaks** Twitch stitched into the live stream.

To do that, the capture splits into a new part at every ad break, so the
footage that aired during the break can be slotted in between. Expect more
parts per stream.

Edge cases:

- It needs the broadcast's archive VOD to exist and stay available; a
  channel that does not keep VODs gets nothing filled.
- See [Twitch](../guide/twitch.md) for how gap-fill works and how its result
  shows on the job.

```toml
[[channels]]
id = "somestreamer"
platform = "twitch"
twitch_gap_fill_ad_breaks = true
```

### `channels[].twitch_vod_backlog`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `false` | `true` or `false` | Next hourly backlog run | Dashboard: Add/Edit Channel → **Queue past VODs, highlights & uploads (Twitch only)** (badge **Past VODs**). TUI: channel editor → **Queue past VODs (Twitch only)**. |

Archive the channel's past videos, not just its live streams. Once an hour
(the first run is two minutes after Moombox starts) Moombox lists the
channel's newest 100 videos of every type (past broadcasts, highlights and
uploads) and queues each one it has not archived before as a `Queued`
backlog job, paced by [archive slots](#channelsarchive_slots).

Skipped:

- videos whose title does not match the channel's [terms](#channelsterms);
- videos already in the processing history;
- the archive VOD of a broadcast Moombox already captured live (or is still
  capturing, or will retry). The live capture is that broadcast's archive;
  gap-fill recovers what it missed. If the live capture failed before
  recording anything, the VOD is queued.

When to use it: Twitch deletes past broadcasts after 7 to 60 days depending
on the channel, so this catches broadcasts Moombox missed (it was not
running, the stream did not match at the time) before they expire.

Edge cases:

- Removing the channel with "delete its pending jobs" deletes these queued
  rows too.
- Disabled channels are not listed.

```toml
[[channels]]
id = "somestreamer"
platform = "twitch"
twitch_vod_backlog = true
```

### `channels[].twitch_archive_clips`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `false` | `true` or `false` | Next Twitch live check (a channel not yet listed since Moombox started is listed then; after that, hourly) | Dashboard: Add/Edit Channel → **Also archive new clips (Twitch only)** (badge **Archive clips**). TUI: channel editor → **Archive new clips (Twitch only)**. |

Archive the channel's new clips. Once an hour, as part of the Twitch live
check, Moombox lists the channel's clips and queues each one it has no job for yet as a `Queued` backlog job,
paced by [archive slots](#channelsarchive_slots).

- The first listing after Moombox starts reaches back one week. Later ones
  reach back a day; after a longer gap in listings (an outage, the machine
  asleep) they reach back a week, then a month, never further.
- Clips are not filtered by the channel's [terms](#channelsterms).
- A clip you paste into **Add video** always works, whether this is on or
  not.

```toml
[[channels]]
id = "somestreamer"
platform = "twitch"
twitch_archive_clips = true
```
