# Monitors

The `[monitors]` section controls how Moombox watches your channels: how
often each monitor polls, how far back it archives, how many backlog
downloads a channel may run at once, and when finished jobs drop out of the
main job list.

```toml
[monitors]
archive_window_days = 3
archive_slots = 3
feed_check_interval = 10
hide_finished_age_days = 30
probe_cooldown = 0
membership_discovery = true
# decapi_check_interval and twitch_check_interval are absent by default
```

Moombox runs three independent monitors. The guide page
[Channels](../guide/channels.md) explains what each one does; in short:

Feed monitor (YouTube)
:   Reads each YouTube channel's RSS feed and, when you are signed in, its
    members-only tab. It keeps every video it has seen in a per-channel list
    and re-checks the videos inside the archive window each cycle. Paced by
    [`monitors.feed_check_interval`](#monitorsfeed_check_interval).

DECAPI monitor (YouTube)
:   Asks [DecAPI](https://decapi.me) for each YouTube channel's newest video.
    It reacts faster than RSS, which can lag by minutes. Paced by
    [`monitors.decapi_check_interval`](#monitorsdecapi_check_interval).

Twitch monitor
:   Asks Twitch whether each Twitch channel is live. Paced by
    [`monitors.twitch_check_interval`](#monitorstwitch_check_interval).

Every interval gets ±10% random jitter, so several Moombox installs never
poll in lockstep. All three monitors skip their cycle while Moombox considers
itself offline, and a monitor with no enabled channels of its platform does
not poll at all.

!!! info "When changes take effect"
    Every setting on this page applies without a restart once you save it in
    the dashboard or the terminal UI. The monitors read these values at the
    start of each cycle; a save from the TUI, or a dashboard save that
    changes one of the three poll intervals, also runs a check of all three
    monitors at once so a new interval does not wait out the old timer. If
    you edit `config.toml` by hand instead,
    Moombox reads it at the next start. A value outside its allowed range in a
    hand-edited file is replaced at load (with the fallback each key lists
    below) and a warning is logged at startup. The dashboard and the TUI
    refuse an out-of-range value when you save it.

Several of these keys can also be given per channel, in the
[`[[channels]]` entries](channels.md): the channel's value wins over the
global one here.

## Archive scope

### `monitors.archive_window_days`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `3` | Whole number, 1–3650 | Immediately (next monitor cycle) | Dashboard: Settings → Monitors → **Archive Window (days)**. TUI: Settings → Monitors → **Archive window (days)**. Also in both first-run wizards (advanced setup). |

How many days back the YouTube monitors look for content to archive. It sets
the depth of a channel's history that Moombox keeps track of and re-checks
each cycle:

- **Upcoming and live streams are always covered**, however old the video
  page is. A stream scheduled months ago that goes live today is still
  recorded.
- **Past content** (finished streams, uploads, finished premieres) is
  archived only when its publish date falls inside the window *and* the
  channel has
  [`include_non_live_content`](channels.md#channelsinclude_non_live_content)
  turned on.
- The feed-history **backfill** that runs when you add a YouTube channel
  scans the channel's Videos and Live tabs (and Members tab when signed in)
  down to this depth. See [Channels](../guide/channels.md).

When to change it:

- **Raise it** to archive a channel's older catalogue. Widening the window
  automatically re-scans each affected channel's history to the new depth
  (one page per second, one channel at a time), and the older VODs it finds
  are queued as backlog, paced by
  [`monitors.archive_slots`](#monitorsarchive_slots).
- **Lower it** if you only care about live streams and want less work per
  cycle. Every cycle re-reads every video inside the window, so a very wide
  window on many channels costs more time and requests.

Edge cases:

- This key is YouTube-only. Twitch has no equivalent; the
  [Twitch VOD backlog](channels.md#channelstwitch_vod_backlog) lists the
  channel's newest 100 videos instead.
- An out-of-range value in `config.toml` is reset to `3` at load.
- A channel's own
  [`archive_window_days`](channels.md#channelsarchive_window_days) overrides
  this value for that channel.

```toml
[monitors]
archive_window_days = 30   # archive a month of past content
```

### `monitors.archive_slots`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `3` | Whole number, 1–100 | Immediately (next admission sweep) | Dashboard: Settings → Monitors → **Archive Slots**. TUI: Settings → Monitors → **Archive slots**. Also in both first-run wizards (advanced setup). |

How many **backlog** downloads one channel may have in progress at once.
Backlog rows wait in the `Queued` status, and a scheduler admits them to
`Upcoming` (and on into the download queue), newest first, while the channel
has a free slot. Backlog rows are:

- older YouTube VODs found by the archive window or the backfill;
- videos queued by the [Twitch VOD backlog](channels.md#channelstwitch_vod_backlog);
- clips queued by the [Twitch clip sweep](channels.md#channelstwitch_archive_clips);
- the videos of a Twitch channel video list or collection you paste into
  **Add video**.

Live streams, upcoming streams, newly published videos and anything you add
by hand never wait in `Queued`, so a large backlog never delays new content.

When to change it:

- **Lower it** (for example to `1`) to drain a big backlog gently and keep
  bandwidth free.
- **Raise it** to finish a backlog faster. Admitted VODs still share the
  global download limit in
  [`downloader.num_parallel_downloads`](downloader.md#downloadernum_parallel_downloads).

Edge cases:

- Admission also pauses, for every channel, while Moombox is offline, while
  the output disk is at or above its critical threshold (until usage is 2
  points below it), and for an hour after YouTube rate-limits the session
  (YouTube backlog only). See [Jobs](../guide/jobs.md).
- A **disabled** channel gets no slots: its `Queued` backlog stays paused
  until you enable it again. Downloads already running are not stopped.
- The `Queued` rows of a channel you **removed** but whose jobs you kept are
  still admitted, using this global value.
- An out-of-range value in `config.toml` is reset to `3` at load.
- A channel's own [`archive_slots`](channels.md#channelsarchive_slots)
  overrides this value for that channel.

```toml
[monitors]
archive_slots = 1   # one backlog download per channel at a time
```

## Poll intervals

### `monitors.feed_check_interval`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `10` (minutes) | 1–1440 minutes; fractions allowed | Immediately | Dashboard: Settings → Monitors → **Feed Check Interval (minutes)**. TUI: Settings → Monitors → **Feed check interval**. Also in both first-run wizards (advanced setup). |

Minutes between the YouTube feed monitor's cycles. Each cycle reads every
enabled YouTube channel's RSS feed (half a second apart), fetches its
members-only tab when
[`monitors.membership_discovery`](#monitorsmembership_discovery) is on and
you are signed in, and re-checks the videos inside the archive window.

When to change it:

- The DECAPI monitor already checks each channel's newest video every 15
  seconds or so, so the feed monitor is mainly the catch-all for everything
  DECAPI cannot see (members-only videos, several videos published at once,
  older items). The default suits most installs.
- **Lower it** (for example `2.5`) if you rely on members-only discovery,
  which only the feed monitor does, and want members-only streams noticed
  sooner.
- **Raise it** for a large channel list on a slow connection.

Edge cases:

- In `config.toml` you can write a plain number of minutes or a duration
  string: `"90s"`, `"30m"`, `"2h"`.
- A value outside 1–1440 minutes in `config.toml` is reset to `10` at load.
- A YouTube cycle can legitimately take longer than the interval on a big
  channel list; the next cycle then starts right after.

```toml
[monitors]
feed_check_interval = "5m"
```

### `monitors.decapi_check_interval`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Absent (automatic) | 15–3600 seconds, whole number; or absent | Immediately | Dashboard: Settings → Monitors → **DECAPI Check Interval (seconds)** (leave empty for automatic). TUI: Settings → Monitors → **DECAPI check interval** (empty for dynamic). |

Seconds between the DECAPI monitor's cycles. Each cycle asks DecAPI for the
newest video of every enabled YouTube channel, one request per channel, one
second apart.

When the key is absent, Moombox picks the interval itself from DecAPI's rate
limit: one second per YouTube channel at DecAPI's default 60 requests a
minute (or whatever limit DecAPI's responses report), and never less than 15
seconds. With up to 15 YouTube channels that is 15 seconds; with 40 channels
it is about 40 seconds.

When to change it:

- Leave it empty unless you have a reason. The automatic value is the
  fastest DecAPI allows.
- **Set a longer value** to reduce traffic to DecAPI, for example on a
  metered connection. YouTube detection then leans more on the feed monitor.

Edge cases:

- A value outside 15–3600 in `config.toml` is removed at load, which puts the
  monitor back on the automatic interval.
- A value shorter than your channel count needs does not break DecAPI's
  limit: the per-request pacing and DecAPI's own rate-limit headers still
  apply, so cycles simply run back to back.
- This monitor only covers YouTube. It never sees members-only videos.

```toml
[monitors]
decapi_check_interval = 60
```

### `monitors.twitch_check_interval`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Absent (15 seconds) | 5–3600 seconds, whole number; or absent | Immediately | Dashboard: Settings → Monitors → **Twitch Check Interval (seconds)**. TUI: Settings → Monitors → **Twitch check interval** (empty = default 15). |

Seconds between the Twitch monitor's live checks. Each cycle asks Twitch
about every enabled Twitch channel, up to 30 channels per request, with half
a second between requests.

When to change it:

- Twitch keeps no rewind buffer for a live stream, so every second between
  the stream starting and Moombox noticing is footage the live capture
  misses (Moombox can fill an opening from the broadcast's archive VOD
  afterwards; see [Twitch](../guide/twitch.md)). Keep the interval short if
  you want the live capture to start as early as possible.
- **Raise it** if Twitch starts refusing requests or you watch many
  channels.

Edge cases:

- A value outside 5–3600 in `config.toml` is removed at load, which puts the
  monitor back on 15 seconds.
- The hourly Twitch VOD backlog and clip sweep do not follow this interval.

```toml
[monitors]
twitch_check_interval = 30
```

### `monitors.probe_cooldown`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `0` (off) | 0 or more seconds; fractions allowed; no maximum | Next monitor cycle | Dashboard: Settings → Monitors → **Probe Cooldown (seconds)**. TUI: Settings → Monitors → **Probe cooldown**. |

The minimum time, in seconds, between two metadata look-ups ("probes") of
the **same** YouTube video by the feed and DECAPI monitors. Each cycle
re-lists the same unfinished videos, and with the default `0` every cycle
probes every one of them again; the poll interval is then the only limit.

When to change it:

- Set it (for example `300`) if your log shows YouTube rate-limiting the
  session, or you watch many channels with long-scheduled upcoming streams,
  to cut the number of requests per cycle.
- A video that has no job yet is then re-checked at most once per cooldown,
  so a large value can delay noticing a status change of such a video (an
  ended stream restarted on the same video, a schedule that changed). A
  video seen for the first time is always probed at once.

Edge cases:

- A negative value in `config.toml` is reset to `0` at load.
- In `config.toml` you can write a number of seconds or a duration string
  (`"5m"`).
- Each monitor keeps its own cooldown list in memory; a restart clears it.

```toml
[monitors]
probe_cooldown = 300   # re-probe a given video at most every 5 minutes
```

## Members-only content

### `monitors.membership_discovery`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `true` | `true` or `false` | Next monitor cycle | Dashboard: Settings → Monitors → **Membership Stream Discovery** switch. TUI: Settings → Monitors → **Membership discovery**. |

Each feed cycle, also read every YouTube channel's members-only tab with your
YouTube login cookies. YouTube's RSS feed and DecAPI never list members-only
videos, so this is the **only** way Moombox finds members-only live and
upcoming streams on its own, and (for channels with
[`include_non_live_content`](channels.md#channelsinclude_non_live_content))
members-only VODs and premieres.

What to know:

- It does nothing without YouTube login cookies. See
  [Cookies](../guide/cookies.md).
- For a channel you are not a member of, YouTube says so and Moombox stops
  fetching that channel's members tab for 6 hours, so non-member channels
  cost little. Repairing your YouTube login, or switching accounts, clears
  that memory at once.
- When it is on and you are signed in, the history backfill also scans the
  Members tab. Turning it on later (or signing in later) re-scans channels
  that were backfilled without it. If your cookies were broken while a
  backfill ran, use **Re-scan Feed History** (dashboard Settings → Channels,
  or TUI chord ++r++ ++b++) after repairing them to pick up the members-only
  backlog.
- A failed members-tab fetch is logged but never marks the channel's feed as
  failing.

When to turn it off: if you have YouTube cookies only for age-restricted
content and have no memberships, turning it off saves one page load per
channel per feed cycle (up to the 6-hour non-member memo).

Edge cases: if the key is absent from `config.toml`, it counts as `true`.
Only an explicit `false` turns it off.

```toml
[monitors]
membership_discovery = false
```

## Job list

### `monitors.hide_finished_age_days`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `30` (days) | 0–365 days; fractions allowed | Immediately | Dashboard: Settings → Monitors → **Hide Finished Jobs After (days)**. TUI: Settings → Monitors → **Hide finished after**. Also in both first-run wizards (advanced setup). |

How long a `Finished` job stays in the main job list. Once a finished job's
last update is older than this, it moves out of the dashboard's **Tasks** tab
into the **Archived** tab, and in the terminal UI below the collapsed archive
divider at the bottom of the task list (select the divider and press
++enter++ to expand it).

Nothing is deleted: this only changes which list a job appears in. Jobs in
any other status (`Error`, `Cancelled`, `COOKIES?`, and all active ones)
always stay in the main list.

When to change it:

- **Lower it** (for example `1` or `0.5`) to keep the main list focused on
  what is happening now.
- `0` moves every finished job to the archive as soon as it finishes.
- **Raise it** if you browse recent archives from the Tasks tab.

Edge cases:

- A value outside 0–365 in `config.toml` is reset to `30` at load.
- In `config.toml` you can write days, or a duration string (`"12h"`,
  `"2w"`).
- An old config with a `[tasklist] hide_finished_age_days` key (or a
  top-level one) is migrated here automatically when `[monitors]` does not
  exist yet.

```toml
[monitors]
hide_finished_age_days = 7
```
