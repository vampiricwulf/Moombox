# Twitch

Moombox records Twitch live streams as they air, downloads past broadcasts
(VODs), highlights, uploads and clips, and saves chat with third-party
emotes. This page explains how each of those works and what you see while
it happens. Setting up a channel is covered in
[Channels and monitoring](channels.md); the per-channel switches are in the
[channel settings](../configuration/channels.md).

!!! tip "No account needed"
    Public streams, VODs and clips need no Twitch login. Moombox works
    anonymously on Twitch unless you give it cookies, and that is normal.
    You only need cookies for subscriber-only content and to record chat
    with your own account (see [Signing in](#signing-in)).

## Recording live streams

### How a stream is detected

The Twitch monitor asks Twitch every
[`monitors.twitch_check_interval`](../configuration/monitors.md#monitorstwitch_check_interval)
seconds (15 by default) whether each enabled Twitch channel is live. When a
channel goes live and the stream matches the channel's
[terms](../configuration/channels.md#channelsterms) (the stream title **or**
the game category), Moombox creates a job and starts recording at once.
Twitch jobs skip `Upcoming` and start as `Live`, then move to `Downloading`.

Each broadcast is one job. If a streamer ends the stream and starts a new
one, that is a new broadcast and a new job.

Moombox does not look at the kind of stream. A **rerun**, a watch party, a
premiere or any other stream type Twitch reports is detected and recorded
exactly like a live broadcast. If you do not want reruns archived, set the
channel's [terms](../configuration/channels.md#channelsterms) to match only
the titles or categories of the streams you do want. Terms can only select,
not exclude, so this works when the streams you want share a recognisable
word.

### Adding a channel by hand

Paste a channel URL (`https://www.twitch.tv/somestreamer`) or just the login
into the dashboard's **Add a video** dialog or the TUI's `A A` (Add Video):

!!! tip "Use the full URL"
    A bare word is tried as a YouTube video ID first. A login that is exactly
    11 letters, digits, `_` or `-` is read as a YouTube video ID and becomes
    a YouTube job, so give such channels as the full
    `https://www.twitch.tv/<login>` URL. The same applies to
    [`moombox add`](../reference/cli.md).

- If the channel is live, recording starts.
- If it is offline, the job waits in `Upcoming` with the progress line
  *Waiting for stream...* and records the next broadcast. A login Twitch
  says does not exist is refused when you add it; one that disappears while
  waiting (renamed, banned, deleted) fails after 10 such answers in a row.
- While a hand-added job is waiting or recording, the monitor does not
  create a second job for the same broadcast.

### Quality

The quality is chosen from the stream's renditions by the channel's
[`quality_preference`](../configuration/channels.md#channelsquality_preference)
(default `best`), capped by
[`downloader.max_video_resolution`](../configuration/downloader.md#downloadermax_video_resolution)
and steered by
[`downloader.prefer_60fps`](../configuration/downloader.md#downloaderprefer_60fps):

- `best` takes the largest size within the cap.
- A size such as `720p60` takes that size, or the next lower one if the
  stream does not offer it. Sizes are measured on the short side, so a
  portrait stream's `720p` is its 720×1280 rendition.
- `audio_only` records only the audio rendition.
- At the chosen size, Moombox prefers AV1, then HEVC, then H.264 (Twitch
  "enhanced broadcasts" offer AV1 or HEVC sources on some channels), then
  the frame rate `prefer_60fps` asks for, then Twitch's *source* rendition.

Moombox checks the stream's renditions again every 30 seconds while it
records. If the quality it is recording disappears or changes, it closes the
current part and continues in the new quality — a **quality split**
(`- part2`, `- part3` … files, and a `quality_split` notification).

The rendition actually being recorded is shown on the job as its quality.

### Ads

Twitch inserts ("stitches") ads into the live stream. During an ad break
the real stream is not sent to the viewer at all, so there is nothing to
record. Moombox recognises the ad segments, skips them without saving them,
and logs *Twitch stitched-ad break started* / *ended*.

The result: the archive has a **jump** where the break was. The footage that
aired during the break can be put back afterwards from the broadcast's VOD —
see [Filling gaps from the VOD](#filling-gaps-from-the-vod).

Whether a capture is shown ads is Twitch's decision for the session that
asks. An anonymous capture is shown them.

### When the stream ends

When the stream ends, Moombox stops, muxes the recording into an MP4 and
marks the job `Finished`.

If the recording stops on an error instead, Moombox first asks Twitch
whether the broadcast is really over (two checks a few seconds apart). Only
a confirmed end is finalized. Otherwise the job goes to `Error` with its
footage kept (see [Unconfirmed end](#unconfirmed-end-automatic-mux)).

### Internet outages during a stream

If Moombox's connection drops while recording:

- Short drops are ridden out in place.
- After an outage of 60 seconds or more, when the connection returns and the
  **same** broadcast is still live, the same job resumes recording
  (notification *Twitch Download Resumed*, event `connectivity_resume`).
- If the broadcast ended during the outage, what was captured is
  finalized into the archive (*Twitch Download Finalizing — Connectivity
  Lost*, event `connectivity_split`).

Either way the stretch that was missed can be filled from the VOD.

### Segments that expired

If Moombox falls so far behind that Twitch has already dropped the segments
it needed (a long stall), it closes the current part and continues at the
live edge in a new part — a **gap split** (*Twitch Stream Gap — New Part
Started*, event `gap_split`).

### Restarts

If Moombox restarts during a stream, the job picks the broadcast back up
when Moombox starts again. If the broadcast ended while Moombox was down,
the job stops in `Error` ("stream ended while Moombox was offline — captured
data can be muxed via the Mux action") with its footage staged; use
**Mux** (`A M` in the TUI) to turn it into the archive.

## Automatic recovery

Two kinds of failed Twitch live jobs fix themselves while the channel is
still configured and enabled. The task list shows **⟳ Auto-retry pending**
or **⟳ Auto-mux pending** on the dashboard and `⟳ retry` / `⟳ mux` in the
TUI, and the job details say what will happen.

### Offline flap: automatic retry

Twitch sometimes reports a channel offline for a moment right after it went
live, or Moombox cannot reach Twitch when the job starts. A monitor-created
job that fails this way **before recording anything** goes to `Error` with
"twitch channel is offline" (or a network error), and:

- When the monitor finds the **same** broadcast live again, the job is reset
  and starts again — at most **2** times (3 attempts in all).
- The first failure sends *Job Failed*. A failure after an automatic retry
  sends none while another retry is still possible; the last one, when the
  retries run out, is reported again.
- A job you added by hand is not retried this way; use **Retry**.
- A manual **Reinitialize** resets the retry count.

*Retries automatically if the broadcast is live again, while the channel is
monitored.*

### Unconfirmed end: automatic mux

If a live capture stops with an error while nothing says the broadcast is
over, Moombox does **not** finalize it — that would delete the staged
footage and the resume data. The job goes to `Error` with its staging kept.
Once the Twitch monitor sees the channel offline, or live with a different
broadcast, Moombox muxes the staged footage exactly as the **Mux** action
would, once.

*Muxes automatically once Twitch confirms the broadcast over, while the
channel is monitored.*

You can still Mux, Resume or Retry such a job yourself at any time. If you
remove or disable the channel, neither automatic action happens any more;
use the job actions by hand. See
[Jobs and their lifecycle](jobs.md) for every action.

## Filling gaps from the VOD

A live capture can miss footage: a late start, a restart, an outage, a gap
split. After the archive is finished, Moombox looks for the broadcast's
archive VOD (Twitch's saved past broadcast) and fills what is missing from
it. This is always on for Twitch live captures.

- **What is filled:** the opening, if recording started more than 1 minute
  after the broadcast did; each hole between parts wider than 3 seconds;
  and the end, if the broadcast ended during an outage.
- **Ad breaks** are filled only on channels with
  [`twitch_gap_fill_ad_breaks`](../configuration/channels.md#channelstwitch_gap_fill_ad_breaks)
  on. With it on, the capture closes the current part at the end of every
  ad break so the missing footage can be slotted in between — expect more
  parts per stream. With it off, an ad break stays a jump inside its part.
- **The result** is saved as extra files beside the archive,
  `<name> - recovered1.mp4`, `- recovered2.mp4` …, in the right place in the
  job's parts. The recording itself is never changed, and a recovered part
  is never the job's main file.
- **Timing.** Moombox waits until Twitch has finished processing the VOD,
  checking again after 10 minutes, doubling up to every 3 hours, for up to
  24 hours. It also waits while the output drive is at the critical disk
  level.
- **When it gives up**, the archive is kept as recorded and the reason is
  shown: the VOD is gone or not found, it is much shorter than what was
  captured, or its timing drifts more than 5 seconds from the live
  recording. Segments Twitch has muted or will not serve are left as gaps.

The job details show the state on a finished live Twitch job:

| Gap-fill line | Meaning |
|---|---|
| *Gap-fill pending: N spans, until …* | Waiting for the VOD (with the last reason, if any). |
| *Filled N spans* | The missing stretches were added as recovered parts. |
| *Kept as recorded: …* | Nothing was filled, and why. |

!!! note
    A channel that does not keep VODs (or deletes them quickly) gets nothing
    filled. Subscriber-only VODs need cookies from an account with access.

A Mux of a failed or cancelled live capture also gets a gap-fill. After a
**Cancel**, only the opening and the holes are filled — not the rest of the
broadcast you chose to stop.

## Past broadcasts, highlights and uploads

### Pasting a VOD

Paste a VOD URL (`https://www.twitch.tv/videos/123456789`, or forms such as
`/<login>/v/<id>` and `player.twitch.tv/?video=v<id>`) or a bare VOD number
into **Add a video**, `A A` or `moombox add`. The VOD is downloaded with
several segments in parallel and muxed to MP4. Its chat replay is saved too
when [`downloader.download_chat`](../configuration/downloader.md#downloaderdownload_chat)
is on.

!!! tip "Prefer the full VOD URL"
    A bare word of exactly 11 characters is read as a YouTube video ID, so
    an 11-digit bare VOD number becomes a YouTube job. In the dashboard and
    the TUI you can also write the number as `v<digits>` (for example
    `v1234567890`); `moombox add` reads that 11-character form as a YouTube
    ID too. The full `https://www.twitch.tv/videos/<id>` URL works
    everywhere.

### Pasting a video list or collection

Paste a channel's videos page or a collection, and Moombox queues **every
video in it** as a `Queued` job:

| Paste | Queues |
|---|---|
| `twitch.tv/<login>/videos` | Every video, newest first |
| `…/videos?filter=archives` (or `highlights`, `uploads`, `past_premieres`) | Only that type |
| `…/videos?sort=views` | Ordered by views |
| `…/videos?filter=collections` | The videos of every collection the channel has |
| `twitch.tv/collections/<id>` | The videos of that collection |

- Up to 50 pages of the listing are read.
- Videos that already have a job are skipped. The dashboard reports
  *Queued N Twitch VODs, M already queued* (and, in yellow, how many could
  not be queued).
- The queued videos are admitted a few at a time by the channel's
  [archive slots](../configuration/channels.md#channelsarchive_slots) (the
  global [`monitors.archive_slots`](../configuration/monitors.md#monitorsarchive_slots)
  for a channel you have not configured).
- No *Job Added* notification is sent per video; each announces itself with
  *Twitch Download Starting* when it starts.
- Removing the channel later never deletes these pasted jobs.

A clip list (`/<login>/clips`) is refused: "Twitch clip lists are not
supported yet. Use a channel, VOD, clip, video list or collection URL."

### Automatic backlog

To archive a channel's past videos automatically, turn on
[`twitch_vod_backlog`](../configuration/channels.md#channelstwitch_vod_backlog)
for the channel. Once an hour Moombox lists its newest videos and queues
each one that has never had a job and whose title matches the channel's
[terms](../configuration/channels.md#channelsterms). It skips the VOD of a broadcast it already
recorded live — the live capture is that broadcast's archive, and gap-fill
covers what it missed — unless the live capture failed before recording
anything.

### Subscriber-only videos

| Situation | Result |
|---|---|
| No Twitch cookies, or cookies that no longer work | The job waits in `COOKIES?` until sign-in is fixed, then resumes. |
| Signed in, but the account has no access | `Error`: "your account does not have access to this subscriber-only content". |

## Clips

Paste a clip URL (`twitch.tv/<login>/clip/<slug>`, `clips.twitch.tv/<slug>`)
into **Add a video**, `A A` or `moombox add`. A clip is a job of its own:

- The best landscape rendition is chosen within your quality settings
  (portrait crops only when there is nothing else). With `audio_only`, only
  the audio is kept; a clip with no audio fails.
- The title is `<channel> — <clip title>`, and the clip's creation time is
  its date in the output template. The job ID is `tw_clip_<slug>`, which
  appears in the file name and tells clips apart from VODs.
- The description file links back to the VOD and time the clip came from,
  when Twitch still has it.
- Pasting the same clip twice is refused; a clip Twitch does not have is
  refused with *Twitch clip not found*.

To archive a channel's new clips automatically, turn on
[`twitch_archive_clips`](../configuration/channels.md#channelstwitch_archive_clips).
Once an hour Moombox lists the channel's recent clips and queues each new
one as a `Queued` job, paced by archive slots. A clip you pasted earlier is
never queued again.

## Chat

With [`downloader.download_chat`](../configuration/downloader.md#downloaderdownload_chat)
on (the default), Twitch chat is saved beside the archive as
`<name>.chat.json`, and the dashboard player replays it over the video (see
[Player and chat replay](player.md)).

**Live streams** — chat is read live from Twitch's IRC chat while the stream
records:

- Each part of a split recording gets its own chat file, timed against that
  part's first frame, so chat stays in sync in every part.
- Regular messages, Bits cheers, subs, resubs, gift subs, raids and other
  system notices are saved, with each user's badges and colour, and the
  emotes Twitch marked in each message.
- If the chat connection drops, Moombox reconnects, waiting longer after
  each failure (up to 30 seconds). After 10 failed reconnects in a row it
  keeps trying every 2 minutes for as long as the job runs. A reconnect that
  Twitch asks for, or one after a connection that went silent, is not
  counted as a failure. Twitch chat
  has no replay, so messages sent while disconnected are missing.
- A restart resumes the same chat file without duplicating messages.
- If a batch of messages cannot be written to its part's chat file (at a
  part boundary or at the end of a run, for example because the disk is
  full or the file is locked), the messages are not thrown away. They are
  saved next to that part's chat file in the job's staging folder as
  `chat.json.lostbatch.json`; a later spill for the same part is added to
  that file rather than replacing it. The job log shows *lost chat batch
  spilled to sidecar for recovery*, and the job reports "twitch chat: N
  messages could not be written to their part file, at a part boundary or
  the end of a run; see the part's chat.json.lostbatch.json". The file is
  kept with the rest of the chat capture when staging is cleaned up, so you
  can recover those messages from it by hand.

**VODs** — chat replay is downloaded from Twitch's comment history for the
video, page by page, and resumes where it stopped if interrupted.

**Emotes** — when the chat is done, Moombox adds the channel's
**7TV**, **BetterTTV** and **FrankerFaceZ** emote sets to the chat file, so
the player shows those emotes as images alongside Twitch's own. A provider
that does not know the channel is simply skipped; the sets are cached for
24 hours.

**Badges** — saved with every message. The player colours names by role
(broadcaster, moderator, subscriber, VIP).

### Chat and your account

Chat is recorded with your Twitch account when the cookie file holds both
the `auth-token` and `login` cookies, and anonymously otherwise. Anonymous
chat misses subscriber-only messages and badges.

If a job that had credentials ends up recording chat anonymously (Twitch
refused the login, or the `login` cookie is missing or unusable), Moombox
sends **Twitch chat is anonymous for …** (event `auth`) once per job. The
video download itself is unaffected, but the next capture will start
anonymous until the cookies are fixed. Re-export cookies from a browser
signed in to Twitch, or refresh them from the browser (`R F` in the TUI).
A running chat session reads the credentials again each time it reconnects,
so cookies you re-import reach it at its next reconnect.

## Signing in

Twitch sign-in is one cookie, `auth-token`, from `twitch.tv`, kept in the
same cookie file as YouTube's. See [Cookies and sign-in](cookies.md) for the
ways to provide it. With it, Moombox can:

- record subscriber-only streams and VODs your account can watch;
- record chat as your account (needs the `login` cookie as well).

Moombox only **checks** the Twitch token; it never renews it. When Twitch
stops accepting it (password change, "log out everywhere", expiry):

- Live detection keeps working — status checks are repeated without the
  token — but the platform is marked as needing a new login.
- Captures may stop in `COOKIES?` until the cookies are replaced, and
  resume on their own once sign-in works again.
- You are notified (event `auth`) once per incident, and again with
  *Authentication Recovered* when it works.

## Client-ID

Every request to Twitch's API and playlist servers sends a Client-ID. Moombox
has a built-in one. If Twitch ever rotates it out and every Twitch request
starts failing, you can set a newer one in
[`twitch.client_id`](../configuration/updates-bgutils-twitch.md#twitchclient_id)
(Settings → **Twitch** → **Client-ID override**; requires a restart). Leave it
empty otherwise.

There is no notification when this happens. When the whole Twitch status
check fails (a rotated Client-ID, or Twitch's API being down), no single
channel is blamed, so no *Channel Not Responding* alert fires. Look in the
log instead: Moombox writes one warning at the start of the outage,
*Twitch batch check failed; Twitch channels are not being checked until it
recovers*, and *Twitch batch checks recovered* when it ends. Further
failures in between are logged only at DEBUG level. While this lasts, no
Twitch channel is being checked for live streams.

## Twitch notifications at a glance

| Title | Event |
|---|---|
| *Stream Found* | `found` |
| *Twitch Download Starting* | `downloading` |
| *Twitch Quality Split* | `quality_split` |
| *Twitch Stream Gap — New Part Started* | `gap_split` |
| *Twitch Download Resumed* | `connectivity_resume` |
| *Twitch Download Finalizing — Connectivity Lost* | `connectivity_split` |
| *Muxing Starting* | `muxing` |
| *Download Finished* | `finished` |
| *Job Failed* | `error` |
| *Twitch chat is anonymous for …* | `auth` |

See [Notifications](notifications.md) for filtering and pinging.

## Tips and gotchas

- **Keep the channel configured** if you rely on automatic retry or
  automatic mux; both are driven by the channel's monitor.
- **Turn on `twitch_vod_backlog`** for channels whose streams you cannot
  afford to miss: if Moombox was down for a whole broadcast, the VOD is
  queued later, before Twitch deletes it (after 7 to 60 days depending on
  the channel).
- **Many parts per stream?** That is expected with
  `twitch_gap_fill_ad_breaks` on (one per ad break) and after quality
  changes. Each part has its own chat file.
- **A clean `Finished` with "Filled N spans"** means footage was recovered
  into `- recoveredN.mp4` files next to the archive; keep them with it.
