# Notifications

Moombox can post to Discord whenever something happens: a stream is found,
a download starts or finishes, a job fails, cookies stop working, the disk
fills up, an update is available. Notifications go to **Discord webhooks**;
no other service is supported.

This page explains what you receive and when. Every option of a target is
documented in the settings reference,
[Notifications](../configuration/notifications.md).

## Set up a webhook

1. In Discord, open the channel's settings → **Integrations** →
   **Webhooks** → **New Webhook**, then **Copy Webhook URL**.
2. Add it to Moombox:

    === "Web dashboard"

        Settings → **Integrations** → **Add Notification**. Paste the URL
        into **Webhook URL**, press **Send Test** to check it, then **Add**.

    === "Terminal UI"

        Settings (++grave++) → **Integrations** → ++a++. Type the URL into
        **Webhook URL**, press ++enter++, then save the settings (++esc++ →
        **Y**). Select the target and press ++t++ to send a test.

    === "config.toml"

        ```toml
        [[notifications]]
        url = "https://discord.com/api/webhooks/123456789012345678/AbCdEf..."
        ```

        Read at the next start.

A new target receives **every** event. Narrow it with an event filter if
that is too much (see [Choosing events](#choosing-events)).

The full URL and the short `discord://<id>/<token>` form both work, and a
forum thread can be targeted with `?thread_id=<id>` on the end. Edits in
either UI take effect immediately; no restart is needed.

### Testing a webhook

The test button (the paper-plane icon on a dashboard card, **Send Test** in
the *Add Webhook* dialog, ++t++ in the TUI) posts one green embed, *Test
Notification* — "Moombox notifications are configured correctly". It makes a
single attempt with no retries, so you see the real result straight away:

- *Test notification delivered* — the webhook works.
- An error — the reason Discord or the network gave, for example an invalid
  URL, a deleted webhook, or a rate limit.

The dialog's **Send Test** tests the URL before you save it, so a broken
paste is caught early. Saving an invalid URL is refused in both UIs.

## What a notification looks like

Each notification is a Discord embed:

- **Title** — what happened, for example *Stream Found*, *Download
  Finished*, *Job Failed*.
- **Colour** — blue for information, green for success, yellow for a
  warning, red for an error, teal for download activity, purple for muxing,
  orange for a cancel.
- **Author line** — the channel's name and avatar, linking to the channel.
- **Fields** — the details: channel, quality, file size, duration, error
  text and so on.
- **Thumbnail** — the video's thumbnail where there is one.
- **Footer** — `Moombox · youtube · <job id>` for a job, or just `Moombox`.
- **Timestamp** — when the event happened, even if Discord was slow to
  accept it.

The title links to the video on YouTube or Twitch. If you set
[`network.public_url`](../configuration/network.md#networkpublic_url) to the
address of your dashboard, the title instead opens that job in your
dashboard, and the YouTube/Twitch link moves to the channel line. System
notifications (disk, updates and so on) have no job to link to.

Text that comes from the video — titles, channel names, error messages — is
shown literally: Discord formatting in it is neutralised, so a title cannot
inject links or pings. Over-long text is shortened with `…` to fit Discord's
limits rather than failing to send.

## Events

Every notification has an **event name**, which is what filters and
mentions refer to. In the dashboard's chips and the TUI's editor they are
grouped as below; the label in brackets is what the dashboard shows.

### Job lifecycle

| Event | Typical title | When it is sent |
|---|---|---|
| `found` (Found) | *Stream Found* | A monitor found a new stream or video for a channel. The description says what it is ("Upcoming:", "Live:", "Premiering:", "Ended stream:", "Video:"); an upcoming stream with a known schedule shows **Starts At** with a countdown. For YouTube it is sent after Moombox's first look at the video, so it carries the real details. |
| `added` (Added) | *Job Added* | You added a video by hand (dashboard, terminal UI, API or `moombox add`). Pasting a Twitch video list sends none per video. |
| `scheduled` (Scheduled) | *YouTube Start Time Confirmed* | An upcoming YouTube stream's start time became known after it was found (a schedule known at discovery is shown on *Stream Found* instead). |
| `rescheduled` (Rescheduled) | *YouTube Schedule Changed* | An upcoming YouTube stream's start time moved. |
| `downloading` (Downloading) | *YouTube Download Starting*, *Twitch Download Starting* | Recording began or resumed. |
| `quality_split` (Quality Split) | *YouTube Quality Split*, *Twitch Quality Split* | The stream's quality changed mid-recording; the current part was closed and a new one started in the new quality. |
| `gap_split` (Gap Split) | *Twitch Stream Gap — New Part Started* | On Twitch, segments expired before they could be downloaded; the part was closed and recording continues at the live edge in a new part. |
| `live_edge` (Live Edge Start) | *Recording From the Live Edge* | A live YouTube recording could not start from the beginning of the broadcast and records from "now" instead (the stream was older than YouTube keeps, protected, or tokens or signatures could not be solved). Sent again if a later resume also has to fall back. |
| `muxing` (Muxing) | *Muxing Starting* | FFmpeg is joining the recording into the final file — also for a manual Mux. |
| `finished` (Finished) | *Download Finished* | The archive is done. Shows file, size, duration, resolution, parts, chat message count. Yellow instead of green, reading "Archived with its end missing" or "Archived with a gap", when the recording is knowingly incomplete. |
| `error` (Error) | *Job Failed* | A job failed. Says whether the download or the mux failed and whether staging was kept — that is, whether **Resume** (keeps staging) or **Retry** (starts over) is the right next step. |
| `cancelled` (Cancelled) | *Job Cancelled* | A job was cancelled. |
| `auth` (Auth) | see [Sign-in alerts](#sign-in-alerts) | A credential problem: cookies dead or missing, a job parked in `COOKIES?`, Twitch chat downgraded to anonymous. |
| `auth_recovered` (Auth Recovered) | *Authentication Recovered*, *Parked Jobs Re-evaluated* | Sign-in works again, and how many parked jobs resumed. |

!!! note "Failures that are not reported"
    No *Job Failed* is sent for a failure there is nothing to do about (for
    example an age-restricted video). A Twitch job that is being retried
    automatically reports its first failure and, if every retry fails, its
    last one, but not the failures in between (see [Twitch](twitch.md)).

### Connectivity

| Event | Typical title | When it is sent |
|---|---|---|
| `connectivity_resume` (Download Resumed) | *Twitch Download Resumed* | After an internet outage of at least 60 seconds, a live Twitch recording picked up the same broadcast again. Says when it paused and how long the outage lasted. |
| `connectivity_split` (Finalized During Outage) | *Twitch Download Finalizing — Connectivity Lost* | The broadcast ended or changed while the connection was down; what was captured is being finalized. |
| `connectivity_restored` (Outage Alert) | *Outage Alert* | Moombox's internet connection is back after an outage. See [Outage alerts](#outage-alerts). |

There is deliberately no "connection lost" notification: with the
connection down it could not be delivered.

### Trim

| Event | Typical title | When it is sent |
|---|---|---|
| `trim_created` (Trim Created) | *Trim Created* | A trimmed copy of a finished recording was made. |
| `trim_deleted` (Trim Deleted) | *Trim Deleted* | A trim was deleted. |
| `trim_error` (Trim Error) | *Trim Failed* | A trim failed — FFmpeg, the disk or the database failed, or an automatic post-download trim could not produce its file. |

### System

| Event | Typical title | When it is sent |
|---|---|---|
| `disk_warning` (Disk Warning) | *Disk Space Warning*, *Disk Monitoring Failed* | The output drive (or the staging drive, when it is a different and fuller one) reached the warning threshold, or free space could not be read. |
| `disk_critical` (Disk Critical) | *Disk Space Critical* | The output drive (or the staging drive, when it is a different and fuller one) reached the critical threshold. |
| `disk_ok` (Disk Recovered) | *Disk Space Recovered*, *Disk Monitoring Recovered* | The incident above is over. |
| `update_available` (Update Available) | *Update Available* | A new Moombox version was published. |
| `update_applied` (Update Applied) | *Update Applied* | Moombox restarted on a new version; says whether the web dashboard came back. |
| `update_failed` (Update Failed) | *Previous Update Failed* | A failed self-update was found at startup and needs your attention. |
| `crash_recovered` (Crash Recovered) | *Recovered From Crash* | Moombox crashed and was restarted automatically. Names the cause from the crash output where there is one, and the file that kept it. |
| `channel_unhealthy` (Channel Unhealthy) | *Channel Not Responding* | A channel failed 20 checks in a row on every monitor watching it — it may be renamed, banned or misconfigured, and its streams are being missed. When the cause is the YouTube feed answering 404, the alert says so: that is common when YouTube is busy and usually clears by itself. |
| `channel_healthy` (Channel Recovered) | *Channel Responding Again* | That channel answers again. |
| `sidecar_down` (Sidecar Down) | *BotGuard Sidecar Down* | The BotGuard sidecar has been unhealthy for 60 seconds and could not be restarted. Until it returns, YouTube downloads lose PO tokens and some signature solving. |
| `sidecar_restored` (Sidecar Restored) | *BotGuard Sidecar Restored* | The sidecar is healthy again. |
| `captcha_required` (Captcha Required) | *YouTube Captcha Required*, *YouTube Captcha Cleared* | YouTube answered with a captcha. Sent once per spell, naming the first video refused; solve it in a browser signed in to the same YouTube account (or on the same network if you use no cookies). The *Cleared* message follows when YouTube answers normally again. |

### Alerts and their all-clears

Several alerts have a matching **all-clear**: `disk_warning`/`disk_critical`
→ `disk_ok`, `channel_unhealthy` → `channel_healthy`, `sidecar_down` →
`sidecar_restored`, `auth` → `auth_recovered`, and the captcha alert →
*YouTube Captcha Cleared*.

- An all-clear is only sent for an alert that was actually sent.
- A filter that lists the alert also receives its all-clear, so you do not
  need to list both.
- Open alerts are remembered across a restart, so an alert raised before an
  update or a crash still gets its all-clear afterwards.

### Disk alerts

The disk alerts watch the drive that holds the
[output directory](../configuration/paths-and-logs.md#pathsoutput_directory).
Usage is checked at startup, about every six minutes, and right after you
change the thresholds or the output directory.

- *Disk Space Warning* fires at
  [`disk.disk_warn_percent`](../configuration/disk-memory-connectivity.md#diskdisk_warn_percent)
  (90 % by default), *Disk Space Critical* at
  [`disk.disk_critical_percent`](../configuration/disk-memory-connectivity.md#diskdisk_critical_percent)
  (95 %). Each shows the percentage used and the free space.
- The same level repeats at most every 30 minutes; a change of level is sent
  at once.
- *Disk Space Recovered* is sent once usage falls 2 points below the
  threshold.
- While the drive is at the critical level, backlog videos are held in
  `Queued` (see [Storage](storage.md)).

### Outage alerts

When Moombox's internet connection comes back after an outage, it sends one
*Outage Alert* with when the outage started and ended and how long it
lasted.

- Outages shorter than 60 seconds in total are not reported.
- The alert is sent once the link has been up for 2 minutes. If the link
  drops again within that time, it counts as the same outage: one alert, with
  the total time offline and a **Flaps** count.
- A link that never settles is reported at most every 30 minutes.

### Sign-in alerts

All of these use the `auth` event (or `auth_recovered`), so one filter entry
covers the family. The five *Cookie …* alerts are about a whole platform and
repeat at most every 30 minutes per platform while the problem lasts.

| Title | Meaning |
|---|---|
| *Cookie Re-Authentication Required* | A platform's sign-in stopped working and automatic cookie refresh is off. Replace the cookies. |
| *Cookie Auto-Refresh Failed* | The automatic browser refresh ran and failed. |
| *Cookie Auto-Refresh Skipped* | The refresh was skipped because a browser may still be using the profile. Close it (or remove the stale lock the message names). |
| *Cookie File Unreadable* | The cookie file could not be read, so nothing was written to it. Check the file or the drive it is on; Moombox retries. |
| *Cookie Auto-Refresh Ineffective* | The refresh ran but did not restore sign-in, and why is unknown. |
| *Authentication Required* | A job stopped in `COOKIES?` because it needs credentials. |
| *Twitch chat is anonymous for …* | A Twitch job that had credentials is recording chat anonymously; subscriber-only messages and badges will be missing, and the next capture will start anonymous. |
| *Authentication Recovered* | The platform's credentials work again ("N jobs resumed"). |
| *Parked Jobs Re-evaluated* | Parked jobs were resumed after the saved credentials were seen again. |

See [Cookies and sign-in](cookies.md) for how to fix each.

## Choosing events

By default a target receives every event. To narrow it:

=== "Web dashboard"

    On the target's card, click **Filter...**. The event chips appear, all
    lit. Click a chip to turn it off (or on); each click is saved at once.
    **Clear filter** returns to all events.

=== "Terminal UI"

    Select the target, ++enter++, move to an event row and press ++space++
    to tick or untick it. ++enter++ keeps the edit; save the settings to
    write it.

Unticking everything does **not** mean "nothing" — an empty filter means
all events. To silence a target, use its **Enabled** switch instead; the
target and its filter are kept.

Useful patterns:

- **Everything** in a quiet log channel, plus a second webhook filtered to
  `error`, `auth`, `disk_warning`, `sidecar_down`, `update_failed` and
  `crash_recovered` in a channel you watch.
- **Just results**: `finished`, `error`, `cancelled`.
- **No discovery spam**: leave out `found`, `scheduled` and `rescheduled` —
  a full re-scan of a channel's history can find many videos at once.

## Pinging someone

A target can ping a Discord role, a user, `@here` or `@everyone`. Set the
**Mention** field on the card (or in the TUI editor) to `<@&ROLE_ID>`,
`<@USER_ID>`, `@here` or `@everyone`.

By default the ping is added only for six events: `error`, `auth`,
`disk_critical`, `update_failed`, `crash_recovered` and `sidecar_down`.
Once a mention is set, the dashboard card shows a **Mentions:** row of chips
to change that; in the TUI, press ++m++ on an event row (the `@` column).
All-clears never ping unless you add them yourself.

See [`mention`](../configuration/notifications.md#notificationsmention) and
[`mention_events`](../configuration/notifications.md#notificationsmention_events)
for the details.

## One message per job

With **Delivery: One message per job** (`mode = "edit"`), a target keeps
each job to a single Discord message that is rewritten as the job moves on —
*Found*, *Downloading*, *Muxing*, *Finished* — with a **Status** field and a
**History** of the states with relative times. Failures and cancels also
post a separate message, and everything that is not part of a job's
progress (sign-in, disk, updates, trims) is always posted on its own.
Discord does not notify on edits, so in this mode only new posts can ping.
See [`mode`](../configuration/notifications.md#notificationsmode).

## Delivery details

How Moombox gets messages to Discord, and what to expect when Discord or
the network is slow:

- **Batching.** `found`, `added` and per-job *Authentication Required*
  notifications are held for 5 seconds and sent together, up to 10 embeds
  per Discord message. A single one still waits the 5 seconds. Everything
  else goes out at once; an error never waits behind a batch.
- **Order.** Each target delivers in order, one message at a time, so a
  job's notifications never arrive out of order.
- **Retries.** Each message gets up to 3 attempts, with a 15-second timeout
  per attempt. Discord's rate limits are respected (Moombox waits when
  Discord says to). A permanent refusal, such as a deleted webhook, is not
  retried.
- **Backlog limit.** Each target holds at most 256 waiting messages. If it
  fills (for example Discord is slow during a big channel re-scan), the
  oldest discovery message (`found`, `added`, `scheduled`, `rescheduled`)
  is dropped to make room for the new one. If no discovery message is
  waiting, the new message is dropped instead. The log says how many were
  dropped.
- **During an internet outage.** Notifications are held, not wasted on a
  dead link. When the connection returns they are sent in order, each marked
  "Delayed by a connectivity outage — raised …" with the original time. If
  more piled up than a target can hold, the oldest are dropped and a
  *Notifications Dropped During Outage* message says how many. The *Outage
  Alert* comes after the held messages.
- **At shutdown.** Moombox makes one attempt per waiting message and exits
  within about 15 seconds. A message that cannot be sent in that time, or
  one held by an ongoing outage, is lost.
- **Several targets.** Each target has its own queue, so a slow or broken
  webhook does not delay the others.

## Troubleshooting

Nothing arrives
:   Press the test button. If the test fails, the message says why. If it
    works, check that the target is not muted and that its event filter
    includes the events you expect. A target with an invalid URL in a
    hand-edited `config.toml` is skipped with a warning in the log at
    startup.

A filter entry never matches
:   A misspelled event name is warned about in the log at startup ("filters
    on unknown event — it will never match"). Use the chips or the TUI
    editor, which only offer valid names.

Every notification arrives twice
:   Two different webhooks for the same channel, perhaps. The same webhook
    listed twice — even in two spellings — is merged into one target.

The ping shows as text but nobody is notified
:   In one-message-per-job mode, only new posts notify — not edits. Also
    check that the event is in the target's mention list and the role can be
    mentioned.

Discovery notifications are missing after a re-scan
:   They may have been dropped because the target's backlog filled up; the
    log has a warning ("notification queue full — shedding notifications")
    with the count. Discovery messages are the first to go.
