# FAQ

## General

??? question "What does Moombox actually do?"
    It watches the YouTube and Twitch channels you list and records their live
    streams automatically, from the first second of the broadcast where the
    platform allows it, together with the live chat. It can also archive
    past videos, premieres and uploads (YouTube), past broadcasts, highlights
    and clips (Twitch), and any single video you paste in. Finished
    recordings are MP4 files on your disk, which you can play in the
    dashboard's player with the chat replayed beside them. See
    [Home](../index.md).

??? question "Which platforms does it run on?"
    Windows x64, Linux x64 and Linux arm64 (for example a Raspberry Pi 4 or 5
    on a 64-bit OS), plus a Docker image. macOS and 32-bit systems are not
    supported. On Linux a glibc distribution is needed for the BotGuard
    sidecar, so Alpine and other musl systems work only through the Docker
    image. A few extras are Windows-only: the one-click FFmpeg install and the
    DPAPI cookie fallback. See [Installation](../getting-started/installation.md).

??? question "Do I need anything besides the Moombox binary?"
    **FFmpeg**, including `ffprobe`. Moombox downloads video and audio
    separately and needs FFmpeg to combine them; without it, jobs stop at the
    muxing step. The Docker image already contains it. Everything else (the
    Node.js runtime for the BotGuard sidecar) is built into the binary and
    unpacked on first start.

??? question "Should I use the web dashboard or the terminal UI?"
    Either, or both at once: they show the same jobs and edit the same
    settings. The terminal UI starts when Moombox runs in an interactive
    terminal; the dashboard is always available at `http://localhost:774`
    (unless the port is taken). On a server, run Moombox without the terminal
    UI (`-no-tui`, or `MOOMBOX_NO_TUI=1`) and use the dashboard. See
    [Web dashboard](../guide/web-dashboard.md) and
    [Terminal UI](../guide/terminal-ui.md).

??? question "If I close the terminal window, does recording stop?"
    Yes. Quitting the terminal UI (++q++ ++q++, or ++ctrl+c++) or closing its
    window stops Moombox. In-progress downloads are saved at shutdown and
    continue when Moombox starts again, but anything broadcast while it is
    stopped is only recovered where the platform keeps it. To keep Moombox
    running without a terminal, run it headless, for example as a systemd
    service or in Docker. See [Installation](../getting-started/installation.md)
    and [Command line](../reference/cli.md).

## Recording

??? question "Does Moombox catch a stream from the beginning if it notices it late?"
    On YouTube, normally yes: a live capture starts at the first segment of
    the broadcast, so a stream found an hour late still produces a complete
    recording. A broadcast that has been running for about five days or more
    is recorded from the live edge instead, because YouTube no longer serves
    its start; so is one whose player challenges cannot be solved (for
    example while the BotGuard sidecar is down). The job's log says why. On Twitch a live capture starts when the monitor sees the channel go
    live (every 15 seconds by default); to also keep the channel's past
    broadcasts, turn on
    [`twitch_vod_backlog`](../configuration/channels.md#channelstwitch_vod_backlog).
    See [YouTube](../guide/youtube.md) and [Twitch](../guide/twitch.md).

??? question "How many streams can it record at once?"
    Live streams are never limited: every channel that goes live starts
    recording at once.
    [`downloader.num_parallel_downloads`](../configuration/downloader.md#downloadernum_parallel_downloads)
    (default 10) caps only VOD downloads, and
    [`monitors.archive_slots`](../configuration/monitors.md#monitorsarchive_slots)
    (default 3) caps how many backlog videos one channel downloads at once.
    The real limits are your bandwidth, disk and memory.

??? question "Can I record only some streams of a channel, for example karaoke?"
    Yes. Give the channel a title filter,
    [`terms`](../configuration/channels.md#channelsterms), such as
    `karaoke|singing`. It is a regular expression matched against the title
    (on Twitch also the game category), always case-insensitive and ignoring
    accents. It cannot exclude words: lookahead is not supported, and a
    pattern that is not a valid regular expression is matched as plain text
    instead. See
    [Channels and monitoring](../guide/channels.md).

??? question "How do I archive a channel's older videos?"
    On YouTube, turn on **Also archive uploads & premieres**
    ([`include_non_live_content`](../configuration/channels.md#channelsinclude_non_live_content))
    and set the channel's archive window to how far back you want to go; on
    Twitch, turn on
    [`twitch_vod_backlog`](../configuration/channels.md#channelstwitch_vod_backlog).
    The older videos are queued and downloaded a few at a time per channel,
    and never delay a live stream. See
    [Channels and monitoring](../guide/channels.md).

??? question "Can I download a single video without adding its channel?"
    Yes: **Add video** on the dashboard's Tasks tab, ++a++ ++a++ in the
    terminal UI, or `moombox add <url>` from a shell (it adds the job to the
    running Moombox's database). Accepted: a YouTube video URL or ID; a Twitch
    VOD, clip, or channel (the job waits for its next live broadcast); a
    Twitch channel's video list or a collection, which queues one job per
    video. Twitch clip lists are not supported. See
    [Command line](../reference/cli.md).

??? question "How do I record members-only or age-restricted videos?"
    Give Moombox the cookies of an account that can watch them: sign in
    through the browser login, import a `cookies.txt`, or point it at a
    Firefox profile. Members-only streams are found only by
    [`monitors.membership_discovery`](../configuration/monitors.md#monitorsmembership_discovery)
    (on by default), which needs those cookies. A members-only or
    sign-in-only job that has no working cookies shows `COOKIES?` and
    resumes by itself once working cookies arrive.

    Age-restricted videos work differently: the job ends in `Error` with
    *Age restricted: … (cookies from an age-verified account would allow
    this)*, no *Job Failed* notification is sent, and nothing retries it.
    Add cookies from an age-verified account, then **Reinitialize** the job
    (see [Error](../guide/jobs.md#error)). See
    [Cookies and sign-in](../guide/cookies.md).

??? question "Is chat saved?"
    Yes, by default
    ([`downloader.download_chat`](../configuration/downloader.md#downloaderdownload_chat)).
    It is saved as `<name>.chat.json` beside the video, and the dashboard's
    player replays it in sync. See [Player and chat replay](../guide/player.md).

??? question "Which quality does it record?"
    The best available up to
    [`downloader.max_video_resolution`](../configuration/downloader.md#downloadermax_video_resolution)
    (2160 by default, measured on the shorter edge), preferring 60 fps and
    HDR at equal resolution. A channel can set its own preference with
    [`quality_preference`](../configuration/channels.md#channelsquality_preference).
    If nothing fits under the cap, the closest rendition above it is taken, so
    a recording is never empty.

??? question "What happens if Moombox or the computer restarts in the middle of a stream?"
    On restart, Moombox picks the job up again. On YouTube, what was recorded
    before the restart is kept; if the new capture cannot simply continue it,
    the earlier recording is set aside in staging and can be recovered into
    its own file: **Recover** under *Set-aside Recordings* in the job's
    details, or ++a++ ++s++ in the terminal UI. On Twitch, a capture that
    stopped before the broadcast's end was confirmed is muxed automatically
    once Twitch confirms the broadcast over, while the channel is monitored;
    the job's details say so. See [Jobs and their lifecycle](../guide/jobs.md).

## Files and storage

??? question "Where are my recordings?"
    In [`paths.output_directory`](../configuration/paths-and-logs.md#pathsoutput_directory)
    (`./output` by default, relative to the folder Moombox was started from),
    one folder per channel, named by
    [`downloader.output_template`](../configuration/downloader.md#downloaderoutput_template):
    `Channel/20261113 Title [videoid].mp4`. Downloads in progress live in the
    staging directory until they are muxed. See
    [Storage and files](../guide/storage.md).

??? question "How much disk space do I need?"
    A recording is staged in full before it is muxed into the final file,
    and both exist while it is muxed, so plan for about twice the size of
    your largest recording, plus your archive. Moombox warns at 90 % usage and, at 95 %, stops admitting
    backlog downloads until space is freed (live recordings continue). See
    [Disk, memory and connectivity](../configuration/disk-memory-connectivity.md).

??? question "A finished job disappeared from the Tasks list. Where did it go?"
    Finished jobs move to the **Archived** tab after
    [`monitors.hide_finished_age_days`](../configuration/monitors.md#monitorshide_finished_age_days)
    (30 days by default). The files are untouched.

??? question "Does deleting a job delete the video?"
    No. Deleting a job removes it from the list; the archive and the files
    beside it stay on disk and show up in the **Orphaned** tab, where you can
    delete them. To add a recording back to the list, pack it (with its chat
    file) into a `.zip` and upload it on the **Imports** tab. See [Storage and files](../guide/storage.md) and
    [Web dashboard](../guide/web-dashboard.md).

## Access and security

??? question "How do I open the dashboard from my phone or another computer?"
    Set [`network.network_access`](../configuration/network.md#networknetwork_access)
    to `lan` in Settings → **Network** and restart Moombox, then open
    `http://<the computer's IP address>:774`. Use the IP address rather than a
    host name. For access from outside your network, read
    [Security and remote access](../reference/security.md) first.

??? question "I forgot the dashboard password."
    You are never asked for it from the machine Moombox runs on or from your
    local network, only from outside. To reset it, stop Moombox, set
    `password_hash = "your new password"` under `[network]` in `config.toml`,
    and start Moombox; it hashes the plain password on startup. See
    [`network.password_hash`](../configuration/network.md#networkpassword_hash).

## Configuration and updates

??? question "Where is the configuration file, and can I edit it by hand?"
    Usually `config.toml` in the folder Moombox is started from. You can edit
    it, but only while Moombox is stopped: Moombox rewrites the whole file,
    without comments, whenever a setting is saved. See
    [Configuration](../configuration/index.md).

??? question "Do I have to restart after changing a setting?"
    Usually not. Most settings apply at once or at their next use. The few
    that are read at startup (the port, network access when leaving
    `localhost`, HTTPS, database and log settings, some cookie settings, the
    connectivity probe targets, the sidecar switch and its hard memory limit,
    the Twitch Client-ID) are marked **Restart** in the dashboard and
    **[restart required]** in the terminal UI, and both offer to restart for
    you. See [Configuration → When changes take effect](../configuration/index.md#when-changes-take-effect).

??? question "How do I update Moombox?"
    Moombox checks GitHub for new releases on startup and every 24 hours, and
    offers the update in both UIs; it verifies the release's signature, swaps
    the binary and restarts. If the new version fails to start, it rolls back
    automatically. In Docker, pull a new image instead
    (`docker compose pull && docker compose up -d`). See
    [Updating](../getting-started/updating.md).

??? question "Can I get notified when a stream starts or a recording fails?"
    Yes, through **Discord** webhooks (the only supported target). Add one in
    Settings → **Integrations**, choose which events it receives, and
    optionally ping a role or user for the important ones. See
    [Notifications](../guide/notifications.md).

??? question "Can yt-dlp use Moombox's PO tokens?"
    Yes. Moombox serves the same proof-of-origin tokens to yt-dlp through the
    bgutil PO token plugin, on the machine Moombox runs on. See
    [yt-dlp plugin](../guide/ytdlp-plugin.md).

??? question "Something is wrong. What should I send when I report it?"
    The Moombox version (`moombox -version`), your platform, what happened,
    and the relevant part of `moombox.log` with the log level set to `DEBUG`.
    See [Troubleshooting](troubleshooting.md).
