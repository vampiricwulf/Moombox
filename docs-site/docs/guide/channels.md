# Channels

A **channel** is a YouTube or Twitch channel Moombox watches. For every
enabled channel, Moombox polls the platform, creates a job when a stream is
scheduled or goes live, and records it without you doing anything. This page
covers adding and managing channels, how Moombox finds streams, and what the
per-channel options do.

Every option mentioned here has a full entry, with defaults and limits, in
the settings reference: [Channels settings](../configuration/channels.md) for
per-channel keys and [Monitors settings](../configuration/monitors.md) for
the global ones.

## Adding a channel

=== "Web dashboard"

    1. Open **Settings → Channels** and click **Add Channel**.
    2. Pick the **Platform** (YouTube or Twitch).
    3. In **Channel ID** (called **Channel Login** for Twitch) type the
       channel's ID, @handle, or paste its URL.
    4. Optionally fill in the other fields (see
       [Channel options](#channel-options) below).
    5. Click **Add Channel**.

    If you paste a URL or an @handle, Moombox looks the channel up first,
    stores its real ID, fills in **Display Name** if you left it empty, and
    switches the platform to match the link. If the channel is already in
    your list (even typed differently), the dialog switches to editing it and
    says "Already configured — editing it instead", so you never overwrite
    an existing channel's settings by accident.

=== "Terminal UI"

    1. Press `` ` `` (backtick) to open **Settings**, then ++tab++ (or
       ++shift+right++) until the **Channels** section is selected.
    2. Press ++a++ to add a channel.
    3. Move between fields with ++up++ / ++down++. Type into text fields;
       change toggles and choices with ++left++ / ++right++.
    4. Press ++enter++ to put the channel in the list. A URL or @handle is
       looked up first; a channel that is already in the list is refused
       with "Channel … already added".
    5. Close Settings with ++esc++ and answer ++y++ to save. Nothing is
       saved until you do; ++n++ discards every change made in the overlay.

    Typing an @handle or a `youtube.com` link switches the platform to
    YouTube, and a `twitch.tv` link to Twitch.

=== "First-run wizard"

    Both setup wizards (dashboard and terminal UI) have a **Monitor
    Channels** step that adds channels the same way. You can skip it and add
    channels later.

=== "config.toml"

    Add a `[[channels]]` block per channel. A hand-written file is not
    looked up for you, so use the real ID: the `UC…` channel ID for YouTube,
    the login name for Twitch. Moombox reads the file at the next start.

    ```toml
    [[channels]]
    id = "UCxxxxxxxxxxxxxxxxxxxxxx"
    name = "Some VTuber"

    [[channels]]
    id = "somestreamer"
    platform = "twitch"
    ```

### What you can type as the channel

| Input | Example | Result |
|---|---|---|
| YouTube channel ID | `UCxxxxxxxxxxxxxxxxxxxxxx` | used as is |
| YouTube @handle | `@SomeVTuber` | looked up, stored as the `UC…` ID |
| YouTube channel URL | `https://www.youtube.com/@SomeVTuber`, `/channel/UC…`, `/c/Name`, `/user/Name` | stored as the `UC…` ID |
| Twitch login | `somestreamer` | used as is (pick Twitch as the platform) |
| Twitch channel URL | `https://www.twitch.tv/somestreamer` (or its `/videos`, `/clips` page) | stored as `somestreamer`, platform Twitch |

A link that is not a channel (a `watch?v=` link, a Twitch VOD link) is
refused with "Not a YouTube or Twitch channel URL". To record a single video
instead, see [Adding one video or stream by hand](#adding-one-video-or-stream-by-hand).

!!! warning "Plain text is not checked"
    Anything that is not a URL or an @handle is stored exactly as you typed
    it. A typo in a channel ID or Twitch login is accepted, and you only find
    out when the monitors keep failing to reach the channel (see
    [Channel health](#channel-health)). Pasting the channel's URL avoids
    this.

## Channel options

The Add/Edit Channel dialog (and the TUI channel editor) holds these fields.
Fields marked YouTube or Twitch only appear for that platform.

| Field (dashboard) | Field (TUI) | What it does | Reference |
|---|---|---|---|
| Platform | Platform | YouTube or Twitch. Locked after the channel is added (dashboard). | [`platform`](../configuration/channels.md#channelsplatform) |
| Channel ID / Channel Login | Channel ID | The channel to poll. Locked after the channel is added (dashboard). | [`id`](../configuration/channels.md#channelsid) |
| Display Name | Display name | Friendly name shown in the list. | [`name`](../configuration/channels.md#channelsname) |
| Filter Pattern (regex) | Filter regex | Only archive titles that match. | [`terms`](../configuration/channels.md#channelsterms) |
| Output directory | Output directory | Where this channel's files go; blank = global. | [`output_directory`](../configuration/channels.md#channelsoutput_directory) |
| Archive window (days) | Archive window (days) | How far back to archive; blank = global. | [`archive_window_days`](../configuration/channels.md#channelsarchive_window_days) |
| Archive slots | Archive slots | Backlog downloads at once; blank = global. | [`archive_slots`](../configuration/channels.md#channelsarchive_slots) |
| Also archive uploads & premieres (YouTube only) | Archive uploads & premieres (YouTube only) | Archive finished content too, not just live streams. | [`include_non_live_content`](../configuration/channels.md#channelsinclude_non_live_content) |
| Fill ad breaks from the archive VOD (Twitch only) | Fill ad breaks from the VOD (Twitch only) | Recover skipped ad breaks after a live capture. | [`twitch_gap_fill_ad_breaks`](../configuration/channels.md#channelstwitch_gap_fill_ad_breaks) |
| Queue past VODs, highlights & uploads (Twitch only) | Queue past VODs (Twitch only) | Hourly: queue the channel's past videos. | [`twitch_vod_backlog`](../configuration/channels.md#channelstwitch_vod_backlog) |
| Also archive new clips (Twitch only) | Archive new clips (Twitch only) | Hourly: queue the channel's new clips. | [`twitch_archive_clips`](../configuration/channels.md#channelstwitch_archive_clips) |
| Quality Preference | Quality preference | Quality to record; default Best (Source). | [`quality_preference`](../configuration/channels.md#channelsquality_preference) |
| Enabled | Enabled | Pause the channel without removing it. | [`enabled`](../configuration/channels.md#channelsenabled) |

Output directory and quality are copied onto each job when the job is
created. Changing them affects new jobs only.

## Managing channels

### In the dashboard

Each channel in **Settings → Channels** is a card showing the platform tag,
the display name and ID, the filter pattern, and a badge for each option that
is on: **Include VODs**, **Past VODs**, **Fill ad breaks**, **Archive
clips**, **Quality: …**. On the card:

- the **switch** enables or disables monitoring at once (a disabled card is
  dimmed);
- the **pencil** opens **Edit Channel**;
- the **trash can** removes the channel (see [Removing a channel](#removing-a-channel));
- the **grip** on the left lets you drag cards into a different order. The
  order is saved immediately ("Channel order updated").

Changes made here are saved immediately; they do not go through the
**Save** button of the other Settings sections.

### In the terminal UI

In **Settings → Channels**: ++up++ / ++down++ select a channel, ++enter++
edits it, ++a++ adds one, ++d++ removes one. Changes are kept in the overlay
until you save it. If you edit a channel in the TUI while someone changes the
same channel in the dashboard, the TUI's save writes only the fields you
changed, so the other edit survives.

!!! tip "Disabling instead of removing"
    Disabling keeps everything: the channel's feed history, its place in an
    unfinished history scan, and its queued backlog, which simply waits.
    Removing deletes the channel's feed history (see below). If you only want
    a break from a channel, disable it.

## Removing a channel

Removing a channel always asks what to do with its jobs.

=== "Web dashboard"

    Click the trash can. The **Remove Channel** dialog counts the channel's
    jobs and offers:

    - **Remove channel, keep its N jobs** (the default), or
    - **Remove channel and delete its N pending jobs**, shown only when
      there are pending jobs, or
    - **Cancel**.

=== "Terminal UI"

    Press ++d++ on the channel. The prompt offers ++enter++ (remove, keep its
    jobs), ++p++ (remove and delete its pending jobs, only when there are
    any) and ++esc++ (cancel). The removal, and the deletion, happen when you
    save the Settings overlay.

What each choice does:

- **Pending jobs** are the channel's `Queued`, `Upcoming` and `COOKIES?`
  jobs. Deleting them also clears their history entries, so the videos can
  be archived again if you re-add the channel.
- A parked job whose staging folder still holds recorded footage is
  **always kept**, whichever you choose, and the prompt names it. You can
  still resume or mux it later.
- Downloads in progress, and `Finished`, `Error` and `Cancelled` jobs, are
  never touched.
- Either way, the channel's **feed history** (the list of videos Moombox
  had seen, and its backfill progress) is deleted. Re-adding the channel
  later scans its history again from the start, without duplicating jobs you
  kept.

Kept jobs go on working without the channel: an `Upcoming` stream is still
recorded when it goes live, and `Queued` backlog is still admitted (at the
global [archive slots](../configuration/monitors.md#monitorsarchive_slots)
rate). For a removed Twitch channel, the automatic retry of a capture that
found the channel offline, and the automatic mux of a capture whose end was
unconfirmed, stop; **Retry** and **Mux** still work by hand.

!!! note
    YouTube jobs created by an older version, or videos you added by hand,
    may not be counted as the channel's, and so are never deleted by the
    removal.

## How Moombox finds streams

Moombox runs several monitors side by side. Each one skips disabled channels,
pauses while the machine is offline, and idles when there are no channels of
its platform.

### YouTube

RSS feed monitor
:   Every [`feed_check_interval`](../configuration/monitors.md#monitorsfeed_check_interval)
    (10 minutes by default), Moombox reads each channel's public RSS feed.
    The feed lists only the channel's ~15 newest videos, so Moombox keeps
    every video it has ever seen in a per-channel list and, each cycle,
    re-checks the ones that matter: everything published within the
    [archive window](../configuration/monitors.md#monitorsarchive_window_days),
    plus every upcoming or live video whatever its age.

DECAPI monitor
:   Every 15 seconds or so (one second per channel, never faster than 15 s;
    see [`decapi_check_interval`](../configuration/monitors.md#monitorsdecapi_check_interval)),
    Moombox asks DecAPI for each channel's newest video. This catches a
    stream within seconds of it being announced, where RSS can lag by
    minutes.

Members-only discovery
:   RSS and DecAPI never list members-only videos. With
    [`membership_discovery`](../configuration/monitors.md#monitorsmembership_discovery)
    on (the default) and YouTube login cookies set up, the feed monitor also
    reads each channel's Members tab every cycle. For channels where you are
    not a member, YouTube says so and Moombox stops checking that tab for 6
    hours. See [Cookies](cookies.md).

Feed-history backfill
:   When you add a YouTube channel, Moombox scans its **Videos** and
    **Live** tabs (and **Members** tab, when members-only discovery is
    active) down to the archive window, one page per second, one channel at
    a time. This fills in what the short RSS feed cannot show. An
    interrupted scan resumes where it stopped; one that fails is retried on
    a later cycle. A scan runs again automatically when you widen the
    channel's archive window, or turn on members-only discovery with cookies
    in place.

For every candidate video, Moombox looks up its current state on YouTube and
checks the channel's [filter](#filtering-by-title). Then:

| What it is | What Moombox does |
|---|---|
| Upcoming stream or premiere | Creates an `Upcoming` job straight away. The job waits for the start, then records. |
| Live now | Creates a job that starts recording at once. |
| Finished stream, upload or finished premiere, published within the archive window | Only with [**Also archive uploads & premieres**](../configuration/channels.md#channelsinclude_non_live_content) on. A video first seen in this or a recent cycle starts at once; an older one is queued as backlog. |
| Already has an active job, or is already in the processing history | Skipped. |
| Members-only and you are not signed in or not a member | Skipped; re-checked later. |

A stream that YouTube shows as ended only on weak signals is double-checked
before Moombox treats it as over; if that cannot be confirmed, the video is
checked again after 1 hour, 6 hours, then daily, so a stream that turns out
live is still found.

### Twitch

Live check
:   Every [`twitch_check_interval`](../configuration/monitors.md#monitorstwitch_check_interval)
    (15 seconds by default), Moombox asks Twitch which of your Twitch
    channels are live, up to 30 per request. When one is live and its title
    or game category matches the channel's filter, Moombox creates a job
    that starts recording at once. If you already added that channel's
    stream by hand and the job is waiting for it or recording it, the
    monitor leaves it to your job instead of recording twice.

VOD backlog (opt-in)
:   With [**Queue past VODs, highlights & uploads**](../configuration/channels.md#channelstwitch_vod_backlog)
    on, Moombox lists the channel's newest 100 videos every hour and queues
    the ones it has not archived as backlog, skipping the archives of
    broadcasts it already captured live.

Clip sweep (opt-in)
:   With [**Also archive new clips**](../configuration/channels.md#channelstwitch_archive_clips)
    on, Moombox lists the channel's new clips every hour and queues each one.

More on Twitch captures, gap-fill and clips: [Twitch](twitch.md).

### Checking now

The dashboard header shows when each monitor runs next ("Feed … · DECAPI …
· Twitch …", shortened to "F · D · T" on narrow screens). Click it to check
all monitors now. In the terminal UI the chord is ++r++ ++m++ (**Check
Monitors Now**). The dashboard accepts one forced check every 30 seconds; a
click inside that window says "Just checked — try again in Ns".

**Re-scan Feed History** (the button under the channel list, or ++r++ ++b++
in the TUI) forces a full history scan of every YouTube channel, ignoring
which ones were already scanned. Use it after repairing YouTube cookies, to
pick up members-only videos a scan missed while you were signed out. The
dashboard button, too, accepts one press every 30 seconds.

## Filtering by title

Set **Filter Pattern (regex)** to archive only some of a channel's streams.
Leave it empty to archive everything.

```text
(?i)(karaoke|singing|歌枠)     karaoke or singing streams
minecraft.*hardcore           "Minecraft" followed later by "hardcore"
```

- YouTube matches the **title**. Twitch matches the **title or the game
  category** (for example `Just Chatting`).
- Matching ignores upper/lower case and accents.
- Patterns are RE2 regular expressions, which have no lookahead, so you
  cannot write "everything except X".
- A pattern that is not valid regex is matched as plain text instead.
- The filter also applies to the Twitch VOD backlog, but not to the Twitch
  clip sweep or to anything you add by hand.

Full rules, including the multi-pattern table form:
[`channels[].terms`](../configuration/channels.md#channelsterms).

## Past content, backlog and archive slots

By default Moombox records only what is upcoming or live. To build an archive
of what a channel has **already** published:

- **YouTube:** turn on **Also archive uploads & premieres** and set the
  **Archive window (days)** to how far back you want to go. The history
  backfill finds the older videos.
- **Twitch:** turn on **Queue past VODs, highlights & uploads** (and **Also
  archive new clips** if you want clips).

Older videos found this way are created as `Queued` **backlog** jobs. A
scheduler admits them a few at a time per channel, newest first: at most
[archive slots](../configuration/monitors.md#monitorsarchive_slots) (3 by
default, overridable per channel) can be in progress for one channel at
once. Live and upcoming streams and newly published videos never wait in
`Queued`, so a backlog of hundreds of VODs never delays a live stream.

The scheduler admits nothing:

- while Moombox is offline;
- while the output disk is at or above its critical threshold, until usage
  is 2 points below it;
- for an hour after YouTube rate-limits the session (YouTube backlog only);
- for a disabled channel.

!!! example "Archiving a channel's whole catalogue gently"
    Set the channel's **Archive window (days)** to `3650`, turn on **Also
    archive uploads & premieres**, and set its **Archive slots** to `1`. The
    backfill lists the catalogue, and Moombox downloads it one video at a
    time while still recording every new stream immediately.

## Adding one video or stream by hand

You do not need to add a channel to archive a single video. Use **Add video**
on the dashboard's Tasks tab, the ++a++ ++a++ chord (**Add Video**) in the
terminal UI, or `moombox add <url>` on the command line. These accept a
YouTube video URL or ID, a Twitch VOD, clip, channel (recorded when it next
goes live) or a Twitch channel's video list. A video added by hand ignores
channel filters and is never paced by archive slots, except the videos of a
pasted Twitch list, which are queued as backlog.

Details: [Jobs](jobs.md) and [Command line](../reference/cli.md).

## Channel health

Moombox counts each channel's failed checks. When a channel fails **20 checks
in a row** (about 5 minutes at the Twitch default, longer for YouTube), it
logs a warning ("channel failing monitor checks — verify it still exists")
and sends a **Channel Not Responding** notification (event
`channel_unhealthy`). The usual causes are a renamed or deleted channel, a
ban, or a typo in the ID. When the channel answers again, Moombox sends
**Channel Responding Again** (`channel_healthy`).

- For YouTube, the alert is held back while the other YouTube monitor
  (RSS or DECAPI) still reaches the channel, since its streams are still
  being seen.
- For Twitch, a failure of a whole request (Twitch refusing the client, a
  network error) does not count against any one channel.
- Alerts that were open when Moombox stopped are remembered, so the
  recovery message still arrives after a restart.

To receive these alerts, add a notification target with those events; see
[Notifications](notifications.md). The per-channel counters are also in the
`channelHealth` field of `GET /api/status` ([API](../reference/api.md)).

The history backfill shows its own progress:

- Dashboard: a badge on the channel card, **Backfill: videos p3** while a
  scan runs, **Backfill failed — will retry** when one ended early.
- Terminal UI: the status bar shows **Backfill <name>: <tab> p<N>** (or
  **BF:<tab> p<N>** when narrow) during a scan and a dim **BF: retry** when
  a failed scan is waiting to be retried.

## Troubleshooting

A channel never gets jobs
:   Check that it is enabled, that the ID is right (paste the channel URL to
    be sure), and that the filter pattern matches the titles you expect.
    The log shows the monitors' decisions; raise the
    [log level](../configuration/paths-and-logs.md#logslog_level) to `DEBUG`
    to see each skipped video and why.

Old videos are not archived
:   YouTube needs **Also archive uploads & premieres** and a wide enough
    archive window; Twitch needs **Queue past VODs**. Videos already in the
    processing history are not queued again; clear them from the
    **Orphaned** tab's history list to retry them ([Storage](storage.md)).

Members-only streams are missed
:   Members-only discovery needs YouTube login cookies for an account that
    is a member. See [Cookies](cookies.md) and [YouTube](youtube.md).

A backlog is stuck in `Queued`
:   The channel may be disabled, Moombox may be offline, the disk may be
    near full, or YouTube may have rate-limited the session (admission
    resumes after an hour). See [Jobs](jobs.md).
