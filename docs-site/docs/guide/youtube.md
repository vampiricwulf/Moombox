# YouTube

This page explains how Moombox archives YouTube: how it finds videos, how it
records a live stream from its very first second, what it does with
scheduled streams, premieres, finished broadcasts and members-only content,
how it picks a quality, and how it copes with the obstacles YouTube puts in
the way — PO tokens, player JavaScript challenges, rate limits, captchas and
blocked IP addresses.

You do not need any of this to get started: add a channel, and Moombox does
the rest. Read on when you want to know why a job is waiting, why a recording
started late, or what a warning in the log means.

## How YouTube videos become jobs

A YouTube job is created in one of these ways:

| Source | What it finds |
|---|---|
| **Feed monitor** | Each enabled YouTube channel's RSS feed, checked every [`monitors.feed_check_interval`](../configuration/monitors.md#monitorsfeed_check_interval). The main discovery path. |
| **DecAPI monitor** | The newest video of every enabled YouTube channel, asked of the third-party service DecAPI (`decapi.me`). A second, faster look that catches a stream the RSS feed has not listed yet. See [`monitors.decapi_check_interval`](../configuration/monitors.md#monitorsdecapi_check_interval). |
| **Members tab** | With YouTube login cookies and [`monitors.membership_discovery`](../configuration/monitors.md#monitorsmembership_discovery) on, each channel's members-only tab. RSS and DecAPI never list members-only videos, so this is the only way Moombox finds them on its own. |
| **Feed history backfill** | When you add a channel (or force it with **Re-scan Feed History**, TUI ++r++ ++b++), Moombox scans the channel's Videos, Live and (with membership discovery) Members tabs back to the archive window, because the RSS feed only ever shows the newest ~15 entries. The Shorts tab is never scanned, so older Shorts are never backfilled: only a new Short that appears in the RSS feed can be archived, and only with `include_non_live_content` on. |
| **By hand** | **Add Video** in the dashboard, ++a++ ++a++ in the terminal UI, or `moombox add` on the command line. |

By default a channel only records live and upcoming streams (premieres that
have not aired yet included). Uploads, finished premieres and broadcasts that
ended before Moombox saw them are only archived when the channel has
[`include_non_live_content`](../configuration/channels.md#channelsinclude_non_live_content)
turned on. A channel's title filter
([`terms`](../configuration/channels.md#channelsterms)) applies to everything
the monitors find for it.
See [Channels](channels.md) for the whole monitoring picture.

The dashboard's **Add Video** box accepts a bare 11-character video ID or any
of these URL forms: `youtube.com/watch?v=…`, `youtu.be/…`,
`youtube.com/live/…`, `youtube.com/shorts/…`, `youtube.com/embed/…` and
`youtube.com/v/…`.

!!! tip "Pick formats or a section by hand"
    In **Add Video**, tick **Advanced** after pasting a YouTube URL. Moombox
    looks the video up and offers a **Video Format** and an **Audio Format**
    list (each can be left on *Auto (best quality)*, or set to *None* for an
    audio-only or video-only file), plus **Start Time** and **End Time** to
    download only part of the video (`HH:MM:SS`, `MM:SS` or seconds; invalid
    input downloads the whole video). The Advanced panel only works for
    YouTube; for a Twitch URL it shows nothing. The choices you made are
    shown in the job's details.

## Scheduled streams and premieres

A stream that has been announced but has not started is an `Upcoming` job.
While it waits, Moombox:

- **Polls the stream's status** with a light request that needs no cookies.
  How often depends on how far away the scheduled start is:

    | Time until scheduled start | Poll interval |
    |---|---|
    | 1 hour or more | about every 10 minutes |
    | under 1 hour | about every 5 minutes |
    | under 5 minutes, or no scheduled start known | about every 30 seconds |

    Each interval gets a little random jitter so requests do not line up.
- **Makes a full look-up every 30 minutes**, picking up a changed title,
  thumbnail or description at no extra cost.
- **Watches for a new start time.** A rescheduled stream sends a
  *Rescheduled* notification.
- **Records the waiting-room chat.** With
  [`downloader.download_chat`](../configuration/downloader.md#downloaderdownload_chat)
  on, the chat is captured from before the stream starts.
- **Checks early when chat bursts.** 30 or more chat messages within 15
  seconds usually means the stream just went live, so Moombox polls at once
  instead of waiting for the next tick.
- **Switches to a signed-in poll** when the stream turns out to be
  members-only, login-only or age-restricted and you have YouTube cookies,
  since the cookie-less poll can no longer see it.

When the poll reports the stream live, Moombox confirms it with a full
look-up, moves the job to `Live` and starts recording.

A waiting job gives up only on a definitive answer: 10 such failed polls in a
row, or a look-up that says the video was made private or removed (`Error`,
*stream unavailable: …*). Network errors, rate limits, YouTube's bot wall and
other temporary refusals never count toward giving up, so a stream scheduled
days ahead survives outages.

**Premieres** are handled exactly like scheduled streams while they wait. The
*Found* notification's text starts with *Upcoming premiere: …* or
*Premiering: …* (instead of *Upcoming: …* or *Live: …*) so you can tell them
from ordinary streams.

## Recording a live stream

### From the very first second

YouTube keeps every segment of a live broadcast available for roughly 120
hours. Moombox uses that: a live capture always starts at the **first
segment of the broadcast**, not at the moment Moombox noticed the stream. A
stream found an hour late, or a Moombox that was started mid-broadcast, still
produces a complete recording.

When a capture starts far behind the live edge, Moombox catches up quickly:
once it is about 40 segments behind, it switches to downloading segments in
parallel with
[`downloader.segment_workers`](../configuration/downloader.md#downloadersegment_workers)
workers, writes them in order, and falls back to one-at-a-time downloading
when it is close to the live edge again. Out-of-order segments wait in a memory buffer bounded
by [`downloader.reorder_buffer_mb`](../configuration/downloader.md#downloaderreorder_buffer_mb)
and [`downloader.reorder_budget_mb`](../configuration/downloader.md#downloaderreorder_budget_mb).

A live broadcast never waits for a download slot.
[`downloader.num_parallel_downloads`](../configuration/downloader.md#downloadernum_parallel_downloads)
only limits VODs, so a busy backlog can never cost you a live stream.

While recording, the job's progress reads like `(V: 1234 A: 1234 C: 5678)`:
video segments, audio segments and chat messages so far.

### When the recording starts at the live edge instead

A few situations make a from-the-start capture impossible. Moombox then
records YouTube's live HLS playlist, which only reaches back a short way, and
the job says so in both UIs as **Recorded from the live edge: *reason*,
joined H:MM:SS in**, and a *Recording From the Live Edge* notification is sent
(the **Live Edge Start** event; see [Notifications](notifications.md)). The
reasons are:

| Reason shown | What happened |
|---|---|
| the stream had been live longer than YouTube keeps its segments (~120 h) | A marathon stream more than 118 hours old when the capture started. |
| the stream's start was already gone from YouTube's ~120 h window | YouTube had already dropped the opening. |
| every DASH format of a kind the job needs was DRM-protected | Only DRM-protected video or audio was on offer. |
| every DASH format of a kind the job needs was DRM-protected, and a post-live stream's HLS holds only its last ~2 h | The same, for a broadcast that had just ended: YouTube's HLS copy of a post-live stream covers only its last two hours or so. |
| no GVS token could be minted for the from-start formats (missing_pot) | The from-start formats need a PO token and none could be made (see [PO tokens](#po-tokens-and-the-botguard-sidecar)). |
| the player's JS challenges could not be solved and no split pair that needs none was left | See [Signature and n-parameter challenges](#signature-and-n-parameter-challenges). |

If the live HLS playlist is DRM-protected too, there is nothing Moombox can
record: see [Streams Moombox cannot record (DRM)](#streams-moombox-cannot-record-drm).

### Quality changes mid-stream

Every 30 seconds Moombox checks which qualities the stream currently offers.
If the best available quality changes (a streamer switches from 720p to
1080p, say), the current part is closed and muxed, and recording continues at
the new quality in a new part. The result is several files named
`… - part1.mp4`, `… - part2.mp4` and so on, with a *Quality Split*
notification. A change that comes less than 10 seconds into a part is
ignored. At the end, neighbouring parts whose streams turn out to be
identical are joined back into one file, and a job that ends with a single
part loses the `- part1` suffix.

A segment YouTube keeps refusing while the stream moves on is skipped the
same way: the part closes at the hole and the next part starts just after it.

### Interruptions and the end of a stream

- **Stalls.** When no new segment arrives, Moombox shows *Waiting for next
  segment…* and checks every 30 seconds whether the stream has ended. If
  nothing arrives for
  [`downloader.maximum_timeout`](../configuration/downloader.md#downloadermaximum_timeout)
  (10 minutes by default), the recording is finalized even if YouTube still
  calls it live.
- **Interrupted broadcasts.** When the streamer's connection drops but YouTube
  says the broadcast may resume, the job shows *Stream interrupted - waiting
  for resume… (12m 30s)* for up to
  [`downloader.interruption_timeout`](../configuration/downloader.md#downloaderinterruption_timeout).
  If it does not come back, the archive is finalized and marked **Incomplete
  tail**; **Resume** (or the monitor seeing the same broadcast live again)
  can still add the missing end. See [Jobs](jobs.md).
- **Confirming the end.** When the downloaders stop, Moombox asks YouTube
  whether the stream really ended (*Verifying stream ended…*). If YouTube
  still says it is live, Moombox keeps trying for about half an hour (checks
  every 5 minutes) before it accepts the end, so a brief CDN hiccup never
  cuts a recording short.
- **Losing the network.** A connectivity outage pauses the job rather than
  ending it (*Waiting for network connectivity…*); see
  [Network, disk and memory settings](../configuration/disk-memory-connectivity.md).

YouTube's download URLs expire after about six hours. Moombox fetches fresh
ones when YouTube refuses an expired link, and for some live formats renews
them ahead of time, so long streams keep recording.

### Restarts and set-aside recordings

Moombox saves a live capture's position regularly. If Moombox is restarted
(or crashes) during a stream, the job picks up where the staged files end and
carries on.

Sometimes a capture cannot be continued — its resume data is missing or does
not match what YouTube now serves. Moombox then **never overwrites** the
footage already on disk. It renames the staged recording to
`<name>.restart-<timestamp>`, logs a warning (*Staged recording set aside for
a fresh start*), and starts a fresh capture from the beginning of the
broadcast. Because the fresh capture starts at the first segment too, the two
overlap: the set-aside recording is never merged into the archive.

- When the job finishes, every set-aside recording is muxed into its own file
  beside the archive, named `<archive name>.restart-<timestamp>.mp4`.
- If that did not happen (the job ended in `Error` or was cancelled, or the
  mux failed), the job's details show **Set-aside Recordings (N)** with each
  recording's time and size, and a **Recover** button. In the terminal UI the
  same action is ++a++ ++s++ (*Recover Set-aside Recordings*, confirm-gated).
  Recover muxes each one into its own file and, if a chat capture was kept,
  places it beside the first. It is offered only when the job is not active.
- Staging that still holds a set-aside recording is protected from the
  automatic cleanup. If such a folder ever shows up on the dashboard's
  **Orphaned** tab, its row lists the set-aside recordings, and deleting it
  warns that captured footage is deleted too (see
  [Storage](storage.md#orphaned-files)).

### After the broadcast: post-live and VODs

If a broadcast has already ended when Moombox gets to it, YouTube serves it
in one of two shapes:

- **Post-live** — YouTube has not finished processing the recording yet.
  Moombox downloads the segments directly from the start.
- **VOD** — a finished broadcast, a premiere or an ordinary upload, served
  as a normal video. Moombox downloads the whole file in chunks, retrying
  each chunk on errors and fetching a fresh URL if the old one expires
  mid-download.

A VOD waits for a free slot under
[`downloader.num_parallel_downloads`](../configuration/downloader.md#downloadernum_parallel_downloads)
and shows *Waiting for a download slot…* meanwhile. If its look-up is more
than an hour old when the slot frees, it is looked up again first.

Older videos found by the archive window or the backfill are **backlog**:
they start as `Queued` and are let in a few at a time per channel
([`monitors.archive_slots`](../configuration/monitors.md#monitorsarchive_slots)).
See [Jobs](jobs.md).

A live job that was split into parts and is only restarted after the
broadcast has ended comes back as a VOD and downloads the complete recording
from the start; once that is done it becomes the archive, and the earlier
parts are kept beside it as `.restart-` files.

### The pre-roll ad wait

When Moombox downloads a VOD (a finished broadcast or an upload — never a
live or post-live capture) and YouTube's answer includes a pre-roll ad, which
happens for a signed-in account without YouTube Premium, every media URL in
it is refused until the ad could have been skipped. Moombox waits that out
before its first request, showing *Waiting out YouTube's pre-roll ad… (12s
left)*. It is a few seconds and needs nothing from you.

## Quality selection

What Moombox records is decided, in order, by:

1. **An intact rendition first.** A rendition YouTube encoded much shorter
   than the video (its track would stop partway) is used only if nothing else
   exists.
2. **Resolution**, measured on the frame's shorter edge, capped by
   [`downloader.max_video_resolution`](../configuration/downloader.md#downloadermax_video_resolution)
   (2160 by default). The largest size at or under the cap wins; if nothing is
   that small, the closest one above it.
3. **The channel's or job's quality preference**
   ([`channels[].quality_preference`](../configuration/channels.md#channelsquality_preference)),
   for example `1080p60` or `audio_only`, applied within the cap.
4. **Frame rate** — 60 fps over 30 fps at the same size when
   [`downloader.prefer_60fps`](../configuration/downloader.md#downloaderprefer_60fps)
   is on (the default).
5. **Dynamic range** — HDR over SDR with
   [`downloader.prefer_hdr`](../configuration/downloader.md#downloaderprefer_hdr)
   (the default); SDR first without it. Dolby Vision ranks last either way.
6. **Premium renditions** (YouTube's higher-bitrate "Premium" copies, only
   offered to a signed-in Premium account) over ordinary ones.
7. **Codec** — AV1, then VP9 HDR, VP9, HEVC, H.264, VP8.
8. **Bitrate.**

Audio is ranked on its own: the original-language track first, then
YouTube's audio-quality grade, the number of channels, the codec (Opus, then
AAC, then the surround codecs) and bitrate. A 5.1 surround track therefore
beats stereo when YouTube offers one; most browsers cannot play E-AC-3 or
AC-3, which the dashboard player tells you about (VLC or mpv play it).

The job's details list each recorded part with its resolution.

Some old uploads (mostly from before 2012) store 4:3 frames that YouTube
stretches to 16:9 on playback, marked by a `yt:stretch=16:9` keyword. Moombox
muxes such a video with the display aspect the keyword asks for, as yt-dlp
does, so the archive plays at the intended shape rather than squeezed. The
ratio is kept with the staged files, so a later **Mux** or a re-mux after a
restart applies it too. The video stream itself is not re-encoded.

## Live chat

With [`downloader.download_chat`](../configuration/downloader.md#downloaderdownload_chat)
on (the default), Moombox saves the chat of every YouTube job:

- **Live streams** — from the waiting room onward. Moombox switches from
  YouTube's filtered *Top chat* view to the unfiltered *Live chat* view when
  YouTube offers it.
- **Finished broadcasts** — the chat replay. After the video has finished
  downloading, the job may wait a while longer for the replay to finish
  paging; it gives back its download slot meanwhile.

The chat is written to `<name>.chat.json` beside the archive. Deleted,
retracted and replaced messages are kept with a marker rather than removed,
and every chat event that is not a message (banners, polls, tickers and
moderation actions) goes to `<name>.chat.raw.jsonl`. The chat capture
survives a restart. The dashboard player replays it next to the video; see
[Player and chat replay](player.md).

## Members-only, private and age-restricted videos

These need YouTube login cookies; see [Cookies](cookies.md).

| YouTube's refusal | Where the job ends | What to do |
|---|---|---|
| Members-only, request not signed in | `COOKIES?` — *Member-only: … the request was not signed in* | Provide fresh cookies. The job resumes on its own once YouTube sign-in works again. |
| Members-only, signed in but not a member | `COOKIES?` — *Member-only: … most likely this account is not a member of this channel* | Export cookies from the account that holds the membership. Refreshing the same cookies cannot help, so Moombox does not try; the job resumes when the cookies belong to a different account. |
| Login required | `COOKIES?` — *Login required: …* | Provide working cookies. |
| Private, while your cookies look dead | `COOKIES?` — *Private video: … they look dead* | Re-export cookies if this account was granted access. |
| Age restricted | `Error` — *Age restricted: … (cookies from an age-verified account would allow this)*. No error notification is sent, so the job's error text is the only sign. | Use cookies from an age-verified account, then Reinitialize the job. |
| Private or removed | `Error` | Nothing; the video is gone. |

A backlog VOD parked in `COOKIES?` goes back to `Queued` when the cookies are
fixed, so it is paced like the rest of the backlog. Members-only streams are
otherwise recorded exactly like public ones, chat included.

### Streams Moombox cannot record (DRM)

When every format a job needs is DRM-protected, Moombox first falls back to
YouTube's HLS playlist (see
[the live-edge reasons](#when-the-recording-starts-at-the-live-edge-instead)).
If that playlist is encrypted as well — or a VOD's HLS playlist is refused as
encrypted, or a live playlist turns encrypted mid-stream — the job ends in
`Error` with text starting *DRM-protected; Moombox cannot record it: …*
(*…; the HLS fallback is DRM-protected too: …* when the fallback was tried).
No cookie, retry, Reinitialize or quality change helps; Moombox does not
decrypt DRM.

## PO tokens and the BotGuard sidecar

YouTube increasingly asks the web player to prove it is a real browser. The
proof is a **PO token** ("proof of origin"), produced by Google's BotGuard
challenge, and YouTube requires it on the video-stream URLs of many formats.
A request without one is refused.

Moombox produces real PO tokens with a small helper process, the **BotGuard
sidecar**: a Node.js runtime bundled inside the Moombox binary, running
Google's BotGuard script in a simulated browser page. You do not install
anything; it is on by default
([`bgutils.use_sidecar`](../configuration/updates-bgutils-twitch.md#bgutilsuse_sidecar)).

- **First start.** The sidecar's files are unpacked from the Moombox binary
  into your cache folder the first time it runs — `%LOCALAPPDATA%\Moombox\sidecar`
  on Windows, `~/.cache/Moombox/sidecar` on Linux (or under
  `$XDG_CACHE_HOME`; `/data/.cache/Moombox/sidecar` in the Docker image).
  The log then says *BotGuard sidecar ready*. Startup has up to 60 seconds.
- **Network.** The sidecar goes through the same HTTPS proxy as Moombox (the
  one set by the `HTTPS_PROXY` / `NO_PROXY` environment variables), so its
  tokens come from the same address as the downloads.
- **Caching.** A token is reused for up to six hours for the same video
  binding, so the sidecar is asked rarely.
- **Signatures.** The sidecar also solves YouTube's player challenges (next
  section).
- **The token endpoint.** The same tokens can be handed to yt-dlp; see
  [yt-dlp plugin](ytdlp-plugin.md).

### Memory limits

The sidecar typically settles around 150 MB and can briefly reach 400–500 MB
while solving a BotGuard challenge. Two settings bound it:

- [`memory.sidecar_soft_limit_mb`](../configuration/disk-memory-connectivity.md#memorysidecar_soft_limit_mb)
  (default 200) — when Moombox's periodic check (about every two minutes)
  finds the sidecar above this, it asks it to free memory. The log says
  *sidecar soft limit hit; ran GC*.
- [`memory.sidecar_hard_limit_mb`](../configuration/disk-memory-connectivity.md#memorysidecar_hard_limit_mb)
  (default 512) — the sidecar's hard heap ceiling. Reaching it kills the
  sidecar, which Moombox then restarts. Keep it comfortably above the soft
  limit.

### When the sidecar is down

If the sidecar dies or fails to start, Moombox restarts it after 5 seconds,
then 15 seconds, then 1 minute, then every 5 minutes until it comes back.
While it is down:

- the terminal UI's status bar shows **SIDECAR DOWN** in red (**POT** on a
  narrow terminal), and the dashboard's status bar shows **PO tokens:
  sidecar down**;
- a *BotGuard Sidecar Down* notification is sent (event **Sidecar Down**,
  which pings your mention by default), and *BotGuard Sidecar Restored* when
  it is back (event **Sidecar Restored**, no ping);
- no new PO tokens can be minted, and new signatures cannot be solved.

Moombox does not stop recording. It works around the missing pieces: a VOD
switches to a copy that needs neither a token nor the player JavaScript,
looked up again without cookies; a live capture falls back to formats that
need neither, and if there are none, to the live HLS playlist (recorded from
the live edge). The job's log says which path it took and why. Some
qualities may be missing until the sidecar is back.

### Turning the sidecar off (the goja fallback)

Setting **Enable sidecar** off (dashboard Settings → **BotGuard**;
needs a restart) leaves only an in-process fallback. It cannot pass
BotGuard's timing check, so **it mints no PO tokens at all**, and it cannot
solve the signatures of current YouTube players (only the scrambling patterns
learned while the sidecar was running still work). Expect fewer formats,
recordings that start at the live edge more often, and a startup warning
(*BotGuard sidecar disabled in config; no PO tokens will be minted and
signature-ciphered formats are unavailable*). Turn it off only to diagnose a
problem with the sidecar itself.

## Signature and n-parameter challenges

YouTube scrambles many stream URLs: a **signature** that must be unscrambled
and an **`n` parameter** that must be answered by running code from YouTube's
player JavaScript. A URL with either one unanswered is refused.

Moombox downloads the player script, keeps it cached (in the system temp
folder under `yt-cipher`, revalidated with YouTube on each use), and solves
the challenges:

- **Signatures** are solved by the sidecar. The first signature of each
  length a player produces also teaches Moombox that player's scrambling
  pattern, which it keeps on disk and can reuse while the sidecar is down.
- **`n` challenges** are solved by the sidecar, with an in-process solver as
  a fallback.

A URL whose `n` no solver can answer is never sent. When the challenges
cannot be solved at all — the sidecar off or down for over a minute, a new
player version the solver does not understand yet, or the player script
cannot be downloaded — Moombox drops every format that needs them and uses
the ones that do not (log line *the player's JS challenges cannot be solved
— dropping every format that needs them*). For a live capture that means the
formats that need no player JavaScript, recorded from the start, or failing
those, the live HLS playlist. If this happens in the middle of a live
recording, the part so far is kept as its own file and recording continues in
a new part on the fallback path.

!!! tip
    A new YouTube player version can occasionally break challenge solving
    until Moombox is updated. Recordings keep going on the fallback formats
    meanwhile; check for an update (see [Updating](../getting-started/updating.md)).

## Rate limits and captchas

YouTube sometimes refuses a session with *This content isn't available, try
again later*, or answers every request with HTTP 429. That is a rate limit,
which YouTube says lasts up to an hour. Moombox waits it out instead of
failing jobs:

- **The job's error text** words it the way yt-dlp does, for example *… Your
  account has been rate-limited by YouTube for up to an hour* (signed in) or
  *… The current session has been rate-limited …* (not signed in).
- **Backlog pauses for an hour.** The first refusal stops the scheduler from
  admitting any YouTube backlog video for an hour, and a backlog VOD that was
  refused goes back to `Queued` for that hour without using up its retries.
  The hold survives a Moombox restart.
- **Other jobs wait in place.** A live stream, a newly published video or a
  video you added keeps its status and tries again every 30 minutes, showing
  *Rate-limited by YouTube; fetching again… (24m 0s left)*.
- **A stream about to go live** keeps polling and recording its chat; a
  go-live confirmation YouTube refused is retried 2, 5, 10 and 20 minutes
  after the first refusal, then every 30 minutes.
- **Giving up.** A job ends in `Error` (the message ends *(gave up after 24
  hours of refusals)*) only once its refusals have gone on for 24 hours **and**
  it has been refused at least 3 times. Refusals only count once the stream is
  live or over; while a stream is still upcoming they cost it nothing. Any
  answered request ends the spell.

**Captchas** are handled the same way, with the line *YouTube asks for a
captcha… (24m 0s left)*. A captcha only passes when someone solves it, so
Moombox sends one *YouTube Captcha Required* notification per spell (not one
per job) and *YouTube Captcha Cleared* when requests succeed again. Solve it
by opening YouTube in a browser signed in with the same account as your
cookies. These notifications use the *Captcha Required* event, which does not
ping your mention unless you add it; see [Notifications](notifications.md).

!!! tip "Fewer requests"
    If rate limits are frequent, set
    [`monitors.probe_cooldown`](../configuration/monitors.md#monitorsprobe_cooldown)
    to stop the monitors re-checking the same videos every cycle, and
    consider fewer [archive slots](../configuration/monitors.md#monitorsarchive_slots)
    so the backlog runs slower.

## The substitute video: blocked IP addresses

When YouTube blocks an IP address, it sometimes answers a request for one
video with the player data of a **different** video. Moombox checks every
answer against the video it asked for and never records a substitute.

When every client it tried was answered with a substitute, the look-up fails
with *every Innertube client returned a player response for a different video
— this IP is likely being blocked by YouTube*, and the log names the
substitute video. Then:

- a stream still waiting to go live keeps waiting — the refusal does not
  count toward giving up;
- a backlog VOD goes back to `Queued` and is retried up to three times
  (after 5, 10 and 20 minutes) before it ends in `Error`;
- any other job ends in `Error`;
- a running recording's 30-second quality check simply skips that tick
  instead of asking YouTube again and again.

A substitution also counts as "the network works", so it never makes Moombox
think it is offline. Moombox cannot get around an IP block; it passes when
YouTube lifts it, or when Moombox connects from a different address.

## Related settings

| Setting | What it changes |
|---|---|
| [`downloader.max_video_resolution`](../configuration/downloader.md#downloadermax_video_resolution) | Resolution cap |
| [`downloader.prefer_60fps`](../configuration/downloader.md#downloaderprefer_60fps), [`downloader.prefer_hdr`](../configuration/downloader.md#downloaderprefer_hdr) | Frame rate and HDR preference |
| [`channels[].quality_preference`](../configuration/channels.md#channelsquality_preference) | Per-channel quality |
| [`downloader.download_chat`](../configuration/downloader.md#downloaderdownload_chat) | Chat capture |
| [`downloader.segment_workers`](../configuration/downloader.md#downloadersegment_workers) | Catch-up speed |
| [`downloader.maximum_timeout`](../configuration/downloader.md#downloadermaximum_timeout), [`downloader.interruption_timeout`](../configuration/downloader.md#downloaderinterruption_timeout) | Stalls and interruptions |
| [`monitors.membership_discovery`](../configuration/monitors.md#monitorsmembership_discovery) | Members-only discovery |
| [`monitors.decapi_check_interval`](../configuration/monitors.md#monitorsdecapi_check_interval) | DecAPI polling |
| [`bgutils.use_sidecar`](../configuration/updates-bgutils-twitch.md#bgutilsuse_sidecar) | BotGuard sidecar on/off |
| [`memory.sidecar_soft_limit_mb`](../configuration/disk-memory-connectivity.md#memorysidecar_soft_limit_mb), [`memory.sidecar_hard_limit_mb`](../configuration/disk-memory-connectivity.md#memorysidecar_hard_limit_mb) | Sidecar memory |
