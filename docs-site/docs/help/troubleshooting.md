# Troubleshooting

Problems are grouped by what you notice. Messages in *italics* or code blocks
are the exact wording Moombox uses, so you can search for them in your log.
For the background on each area, follow the links into the guide.

## First, collect information

### Where the logs are

| Source | What it holds | How to reach it |
|---|---|---|
| `moombox.log` | Everything, with timestamps. Rotated to `moombox.log.1`, `.2`, … at 10 MB, 5 copies kept. | Beside the executable by default; see [`paths.log_file_path`](../configuration/paths-and-logs.md#pathslog_file_path). In Docker, `./data/moombox.log` on the host. |
| Dashboard **Logs** tab | The latest lines, then live. Filter buttons **All** / **Info** / **Warn** / **Error**, and a search box. | Web dashboard. |
| TUI log panel | The same live stream. `/` searches (++n++ / ++shift+n++ for next / previous match), ++c++ clears the view. | Terminal UI. |
| Job logs | One job's own lines. Kept in memory only, so they are gone after a restart. | The job's details in the dashboard; ++o++ ++l++ in the TUI. |
| `moombox-crash.log` | Output of a crash that never reached the normal log (a fatal runtime error). The next start logs its beginning and keeps it as `moombox-crash.log.1`. | Beside `moombox.log`. |
| Console / `docker compose logs` | Startup errors, launcher messages (crash restarts, update rollbacks), the entrypoint's messages in Docker. | The terminal you started Moombox from. |

### Turn on debug logging

- **For a while:** set **Log level** to `DEBUG` (dashboard Settings → **Logs**,
  or the TUI's Settings → Logs). It applies the moment you save. Set it back
  afterwards; `DEBUG` is verbose.
- **For one run, without touching the file:** start Moombox with
  `-log-level DEBUG`. The override is never saved.

Reproduce the problem, then take the matching part of `moombox.log`.

!!! tip "Before sharing a log"
    Moombox redacts media URL credentials from errors it logs, but a log still
    names your channels, video titles, paths and IP addresses. Read it before
    posting it publicly.

### Memory and CPU: profiling

If Moombox's memory keeps growing or it uses a lot of CPU, start it with the
environment variable `MOOMBOX_PPROF=1`. It then serves Go's standard
profiling endpoints at `http://localhost:6060/debug/pprof/` (this machine only,
no password). Without the variable nothing is started.

=== "Linux"

    ```bash
    MOOMBOX_PPROF=1 ./moombox
    # in another terminal:
    go tool pprof http://localhost:6060/debug/pprof/heap
    curl "http://localhost:6060/debug/pprof/goroutine?debug=2" > goroutines.txt
    ```

=== "Windows (PowerShell)"

    ```powershell
    $env:MOOMBOX_PPROF = "1"
    .\Moombox.exe
    # in another terminal:
    go tool pprof http://localhost:6060/debug/pprof/heap
    Invoke-WebRequest "http://localhost:6060/debug/pprof/goroutine?debug=2" -OutFile goroutines.txt
    ```

To see what grows, take two heap snapshots some minutes apart and compare
them:

```bash
curl -o heap-t0.pprof http://localhost:6060/debug/pprof/heap
# wait a while
curl -o heap-t1.pprof http://localhost:6060/debug/pprof/heap
go tool pprof -inuse_space -base heap-t0.pprof heap-t1.pprof
```

!!! note "Docker"
    Inside a container the profiling port listens on the container's own
    loopback, so it is not reachable through the published port, and the
    image ships no HTTP client to fetch it with. For a memory investigation,
    the simplest route is to run the same version of the binary outside
    Docker with a copy of your configuration.

At `DEBUG` level Moombox also logs a memory line every two minutes
(`[Memory] Sys: … Heap: …`, including the BotGuard sidecar's memory when it
runs). The memory caps themselves are on
[Disk, memory and connectivity](../configuration/disk-memory-connectivity.md).

## Moombox will not start

### "another moombox instance is already running"

Only one Moombox runs per user account. The message has one of these forms:

```text
another moombox instance is already running, or the last one is still shutting down (lock held on …)
another moombox instance is already running in this session
another moombox instance is already running (cross-session lock held)
If you believe this is in error (the previous instance crashed), wait a few seconds and try again.
```

- Look for a Moombox that is still running under the same user: another
  terminal, a minimised window, or a systemd service. The lock is per user,
  not per folder, so a second copy installed elsewhere counts too.
- A previous instance may still be shutting down; give it a minute.
- On Windows, *in this session* means another Moombox in your own logon
  session; *cross-session lock held* means one started by the same user in
  another session, for example a scheduled task set to run whether or not
  you are logged on.

More on the lock: [Command line](../reference/cli.md).

### "Startup error: load config: failed to parse config …"

`config.toml` is not valid TOML, or a value has the wrong type (text where a
number belongs, a duration Moombox cannot read such as `"1h30m"`). The message
names the line. Fix it in a text editor and start again. Moombox exits with
code 3 here, and the launcher does not retry.

Values that parse but are out of range do **not** stop startup; they are
replaced and logged. See
[Configuration → Values Moombox cannot use](../configuration/index.md#values-moombox-cannot-use).

### "port … (and nearby ports) already in use"

When the dashboard port is taken, Moombox logs *port in use, probing nearby
ports* and tries the next ten ports; the one it found is logged as *using
alternative port*, and the *Web dashboard available at …* line gives the
address. Only when all eleven are taken does it fail:

- **Without the TUI** (headless, Docker, systemd), Moombox prints
  `Error: port 774 (and nearby ports) already in use: …` and
  `Web dashboard is unavailable.`, and exits with code 3.
- **With the TUI**, it keeps running without a dashboard and logs *Web server
  failed, continuing in TUI-only mode*.

Free the port, or change [`network.port`](../configuration/network.md#networkport).
In Docker, the port inside the container must match the published port and
the health check.

!!! note "Linux: permission denied, not a busy port"
    On Linux an ordinary user usually cannot bind ports below 1024, and the
    default `774` and the ten ports Moombox tries after it are all below 1024.
    Moombox still reports this as *port 774 (and nearby ports) already in
    use*; the end of the message reads `bind: permission denied` instead of
    `address already in use`. Freeing the port will not help. Set
    [`network.port`](../configuration/network.md#networkport) to 1024 or
    above, or grant the binary the `CAP_NET_BIND_SERVICE` capability (for
    example `sudo setcap cap_net_bind_service=+ep ./moombox`, repeated after
    each update replaces the binary). The Docker image is not affected.

### "database schema vN is newer than this binary supports"

You started an older Moombox on a database a newer version has already
upgraded — usually after replacing the binary by hand, or after a rollback.
Run the newer version again. If an automatic rollback happened, the newer
binary is kept beside the executable with a `.failed` suffix and the
`.update-failed` marker file names it. The database error itself names
both files.

### Settings keep reverting, or the setup wizard runs again

The log line near the top says which file was used:

- *No configuration file found — running on defaults; a save will create one*
  means Moombox did not find your file. It was started from another working
  directory, or `-config` names a path with a typo. A save then creates a
  **second** file at the path shown. Start Moombox from the right directory or
  fix the path.
- Hand edits made **while Moombox was running** are overwritten by the next
  save. Stop Moombox, edit, start.

See [Configuration](../configuration/index.md).

### The window closes immediately (Windows)

Start `Moombox.exe` from a PowerShell or Command Prompt window instead of
double-clicking it, so the error stays visible. On an interactive console
Moombox waits with *Press Enter to exit...* after a startup error; the message
is also in `moombox.log` when it got far enough to open it.

## Moombox keeps restarting or crashed

Moombox runs as two processes: a small launcher, and the program it starts.
The launcher restarts the program when it crashes:

```text
moombox exited abnormally (code N) — restarting in 2s (attempt 2/5)
```

The wait doubles with each crash (1 s, 2 s, 4 s, 8 s, 16 s). After more than
five crashes in a row it gives up:

```text
moombox crashed 6 times in a row (last exit code N) — giving up; check the logs
```

A run that lasted at least a minute resets the count. A program that
exits abnormally within its first minute of a fresh start (not a respawn) is
not restarted at all: that is treated as a startup problem, and the launcher
exits with the same code so you see it. When a restarted
program comes up it sends a *Recovered From Crash* notification. If the crash
output never reached the log, the next start writes its beginning into
`moombox.log` and keeps the whole output as `moombox-crash.log.1`. Include
that file when you report the crash.

A startup error (exit code 3: a bad config, a port, a database) is never
retried; fix the cause.

### After an update, the new version would not start

If the first start after a self-update fails, the launcher restores the
previous binary, restarts it, and writes a marker file named after the
executable with `.update-failed` appended (for example
`Moombox.exe.update-failed`). It explains what happened; the failed release is
kept as `<executable>.failed` when possible. The restored version announces
it with a *Previous Update Failed* notification, and automatic update checks
skip the failed release. You can delete the marker once you have read it.

If the failure was a startup error rather than a broken release (a config it
cannot load, a port it cannot bind), nothing is rolled back; the console says
*Moombox could not start after a self-update … not a broken release. Fix the
cause and start Moombox again.*

Details: [Updating](../getting-started/updating.md).

## I cannot open the dashboard

### From another device on the network

[`network.network_access`](../configuration/network.md#networknetwork_access)
defaults to `localhost`: only the machine Moombox runs on can connect, and
Moombox listens on the loopback address only. Set it to `lan` (or `external`)
and **restart** Moombox: who is admitted changes at once, but listening on
the network starts only at startup. Then open `http://<the machine's IP>:774`.
Also check the host firewall.

### The dashboard is blank, unstyled, or its buttons do nothing

The dashboard loads its UI components (the Shoelace library) from
`cdn.jsdelivr.net`, and its security policy allows scripts only from
Moombox itself and that host. If the **browser** cannot reach
`cdn.jsdelivr.net` — an offline network, a corporate proxy or firewall, a
content or script blocker — the page opens without styling, with empty
areas, or with controls that do not respond. Allow `cdn.jsdelivr.net` on the
network and in any blocker on the device you browse from. Moombox's own
internet access makes no difference here; it is the browser that fetches the
library. The terminal UI does not need it. See
[Security](../reference/security.md).

### The browser shows "Forbidden"

| Response | Cause | Fix |
|---|---|---|
| `Forbidden` (plain text) | Your address is not allowed by `network_access` (for example a LAN device while access is `localhost`). | Raise `network_access` (and restart if leaving `localhost`). |
| `{"error":"Forbidden: unrecognized host — open the dashboard by IP address or localhost"}` | On `localhost` or `lan`, you used a host name (`http://myserver:774`). This protects against DNS-rebinding attacks. | Use the IP address or `localhost`, or a name that the HTTPS certificate covers. |
| `{"error":"Forbidden: unrecognized host — open the dashboard by IP address, localhost or network.public_url"}` | On `external`, a local or LAN browser used a host name Moombox does not recognise. | Use the IP address, or set [`network.public_url`](../configuration/network.md#networkpublic_url) to the name. |
| `{"error":"Forbidden: invalid origin"}` when you save or click an action (the page itself loads) | The address in the browser is not one Moombox accepts changes from. The log has a *CSRF: origin refused* line naming the origin and host it compared. Behind a reverse proxy that rewrites the `Host` header, the usual cause is a proxy missing from [`network.trusted_proxies`](../configuration/network.md#networktrusted_proxies). | Open the dashboard by its IP address or `localhost`, or set up the proxy settings on [Security and remote access](../reference/security.md). |
| `{"error":"Forbidden: loopback only"}` | The request is one only the machine Moombox runs on may make: the PO token endpoints the [yt-dlp plugin](../guide/ytdlp-plugin.md) uses, and opening a job's folder. | Run yt-dlp on the same machine, or open the folder there. |

!!! note "Docker and IPv6"
    With the shipped compose file, a client that reaches the container over a
    **global** IPv6 address gets `403 Forbidden` under `lan`, and browsers do
    not retry over IPv4. Open the dashboard by the host's IPv4 address, or
    publish the port as `0.0.0.0:774:774`. Details in
    [Docker](../getting-started/docker.md).

### It asks for a password I do not know

The password is asked only of clients that are neither on this machine nor on
a private network, and only with `external` access. From the machine itself
(`http://localhost:774`) you are never asked. To reset it, stop Moombox, put
a new plain password in [`network.password_hash`](../configuration/network.md#networkpassword_hash)
(8 to 128 characters), and start Moombox; it hashes it on startup. Deleting
the line removes the password.

### Certificate warning with HTTPS

With [`network.https_enabled`](../configuration/network.md#networkhttps_enabled)
and no certificate of your own, Moombox generates a self-signed certificate,
which browsers warn about. Accept it for this site, or point
[`tls_cert_path`](../configuration/network.md#networktls_cert_path) and
[`tls_key_path`](../configuration/network.md#networktls_key_path) at a real
certificate (a changed path needs a restart; new files written over the
configured paths, such as a renewed certificate, are picked up without one).
Plain `http://` requests to the HTTPS port are redirected to `https://`.

Remote access in general: [Security and remote access](../reference/security.md).

## FFmpeg

**"FFmpeg Not Found"** appears in the dashboard and the TUI when Moombox
cannot run FFmpeg. Without it nothing can be muxed: jobs download, then fail
at the muxing step with an error starting `ffmpeg start:` (typically
*executable file not found*).

- Install FFmpeg (a standard build also includes `ffprobe`, which Moombox uses
  too), and make sure it is on
  `PATH` for the user Moombox runs as, **or** set
  [`paths.ffmpeg_path`](../configuration/paths-and-logs.md#pathsffmpeg_path).
  A path checked from the dashboard or the TUI is applied at once, no restart
  needed.
- On Windows, the **Install FFmpeg** button can install it through winget or
  Chocolatey (with an administrator prompt). On Linux it shows the command for
  your package manager instead; run it yourself. If the install succeeds but
  the message says *FFmpeg installed but not found on PATH*, restart Moombox
  so it sees the new `PATH`.
- The Docker image already contains FFmpeg.

After fixing it, use **Mux** on the failed jobs: their downloaded data is
still staged. See [Installation](../getting-started/installation.md).

## Jobs and downloads

### A stream was not recorded at all

- **Is the channel monitored and enabled?** Check Settings → Channels. A
  disabled channel is paused.
- **Does the title filter exclude it?** [`terms`](../configuration/channels.md#channelsterms)
  is a case-insensitive regex on the title (on Twitch, also the category).
- **Is it members-only?** RSS and DECAPI never list members-only videos; only
  [`monitors.membership_discovery`](../configuration/monitors.md#monitorsmembership_discovery)
  finds them, and only with YouTube cookies.
- **Is it older than the archive window?** Past videos are only picked up
  within [`archive_window_days`](../configuration/monitors.md#monitorsarchive_window_days),
  and regular uploads only with
  [`include_non_live_content`](../configuration/channels.md#channelsinclude_non_live_content).
- **Did a *Channel Not Responding* notification arrive?** It means every
  monitor covering that channel failed repeatedly (*it may be renamed, banned,
  or misconfigured, and its streams are being missed*). Check the channel ID.
  If the alert says the feed keeps answering 404, that is common when YouTube
  is busy and usually clears by itself; check the ID only if it lasts.
  A *Channel Responding Again* follows when it recovers.
- **Was Moombox offline?** See [Internet outages](#internet-outages) below.

To poll at once, click the monitor countdown (*Feed … · DECAPI … · Twitch …*)
in the dashboard's status bar, or press ++r++ ++m++ in the TUI. Details: [Channels and monitoring](../guide/channels.md).

### A job sits in `Queued`

Backlog jobs wait for the per-channel
[archive slots](../configuration/monitors.md#monitorsarchive_slots). The job's
line says why it is waiting:

| Line | Meaning |
|---|---|
| *Waiting in queue...* | Nothing holds it; its channel's archive slots are in use. It is admitted when one frees up. |
| *Held: offline* | Moombox thinks the internet is down. Nothing is admitted until it is back. |
| *Held: disk full* | The output disk, or the staging disk when it is a different and fuller one, reached [`disk_critical_percent`](../configuration/disk-memory-connectivity.md#diskdisk_critical_percent). Admission resumes once usage is 2 points below it. |
| *Held: YouTube rate limit until 14:05* | YouTube rate-limited the session. YouTube backlog waits for an hour. |
| *Retry 2 of 3 at 14:05* | This backlog job failed (or found the disk full) and went back to `Queued`; it is admitted again at the time shown. |

Live and upcoming streams never wait in `Queued`. More in
[Jobs and their lifecycle](../guide/jobs.md).

### A job ended in `Error`

Open the job's details and read the error text; the job's log has the rest.
Staged data is kept, so you can:

- **Resume** (YouTube, when something is staged) to continue the recording;
- **Mux** to turn what was captured into an archive as it is;
- **Reinitialize** to discard the attempt and start again.

An error that begins *Age restricted: …* and ends *(cookies from an
age-verified account would allow this)* means YouTube refused the video to
your session because of its age restriction. No notification is sent for it,
and the job does not come back on its own. Load cookies from a signed-in
account that YouTube has age-verified (see
[Cookies and sign-in](../guide/cookies.md)), then **Reinitialize** the job.

Some Twitch errors recover on their own; the details then show *Retries
automatically if the broadcast is live again, while the channel is monitored*
or *Muxes automatically once Twitch confirms the broadcast over, while the
channel is monitored*. See [Jobs and their lifecycle](../guide/jobs.md).

### A job shows `COOKIES?`

The platform needs credentials for it: members-only, sign-in required,
subscriber-only, or your cookies expired. (Age-restricted videos end in
`Error` instead; see [A job ended in `Error`](#a-job-ended-in-error).) An *Authentication Required*
notification (*Cookies needed: …*) names the job. Fix the cookies (sign in
again, or import a fresh `cookies.txt`) and the job resumes on its own;
*Authentication Recovered* is sent when Moombox sees working cookies again.
A parked job is retried automatically at most 3 times; if its error then says
the cookies are not the cause, refreshing them again will not help, and
**Resume** or **Reinitialize** is the way to try once more.
If the account simply is not a member, refreshing the same account will not
help. See [Cookies and sign-in](../guide/cookies.md).

### "rate-limited by YouTube" or "YouTube Captcha Required"

YouTube throttles a session that makes too many requests. The job's error
reads like *… Your account has been rate-limited by YouTube for up to an
hour* (*The current session …* when you have no cookies). Moombox waits it out: backlog pauses for an hour, other jobs retry
every 30 minutes, and a job only gives up (*… (gave up after 24 hours of
refusals)*) after 24 hours and at least three refusals.

A captcha needs a person: open YouTube in a browser signed in with the same
account as your cookies (or, without cookies, from the same network) and solve
it. *YouTube Captcha Cleared* follows.

To make fewer requests, raise
[`monitors.probe_cooldown`](../configuration/monitors.md#monitorsprobe_cooldown),
lower [archive slots](../configuration/monitors.md#monitorsarchive_slots), and
keep [`segment_workers`](../configuration/downloader.md#downloadersegment_workers)
at its default. Details: [YouTube](../guide/youtube.md).

### "this IP is likely being blocked by YouTube"

*every Innertube client returned a player response for a different video —
this IP is likely being blocked by YouTube* means YouTube answered with a
different video. Moombox never records the substitute. It cannot work around
an IP block: it passes when YouTube lifts it, or from another address.

### Downloads return 403 with many segment workers

At startup, or when you save a new value, a `segment_workers` above 16 logs:

```text
downloader.segment_workers N is high — a large simultaneous fan-out to YouTube raises bot-detection risk; reduce it if downloads start returning 403
```

Lower [`downloader.segment_workers`](../configuration/downloader.md#downloadersegment_workers)
(default 12).

### BotGuard sidecar down

The TUI's status bar shows **SIDECAR DOWN** (or **POT**) in red, and a
*BotGuard Sidecar Down* notification arrives after a minute: *The BotGuard
sidecar has been unhealthy for over 60 s — PO-token minting and
signature-ciphered formats are unavailable until it returns*. Moombox keeps
restarting it and keeps recording on formats that need no token, so some
qualities may be missing. *BotGuard Sidecar Restored* follows when it is back.

If it never starts:

- On Linux it needs **glibc**; on Alpine and other musl systems it cannot run.
  Use a glibc distribution or the Docker image.
- It unpacks about 36 MB into your cache folder on first start
  (`%LOCALAPPDATA%\Moombox\sidecar` or `~/.cache/Moombox/sidecar`). A full or
  read-only cache folder stops it.
- If it is killed for memory, raise
  [`memory.sidecar_hard_limit_mb`](../configuration/disk-memory-connectivity.md#memorysidecar_hard_limit_mb)
  (restart required).
- Make sure [`bgutils.use_sidecar`](../configuration/updates-bgutils-twitch.md#bgutilsuse_sidecar)
  is on.

See [YouTube → PO tokens](../guide/youtube.md).

### A recording starts at the live edge, or has gaps

Recording from the first second needs PO tokens and solved player challenges.
When those are unavailable (sidecar down, a new YouTube player version),
Moombox records the live HLS playlist instead, which starts near the live
edge. The same happens for a broadcast that has been running longer than
YouTube keeps from its start; the job log says why. Check for a
Moombox update. For interruptions, quality changes and set-aside recordings
after a restart, see [YouTube](../guide/youtube.md). On Twitch, ad breaks and
expired segments are covered in [Twitch](../guide/twitch.md).

### Every Twitch request fails

If Twitch has rotated out the built-in Client-ID, set a current one in
[`twitch.client_id`](../configuration/updates-bgutils-twitch.md#twitchclient_id)
(restart required). Otherwise leave it empty. See [Twitch](../guide/twitch.md).

## Cookies

The cookie guide has its own table of messages:
[Cookies and sign-in](../guide/cookies.md). The ones met most often:

- **"failed to update cookie file — rotated session cookies were discarded
  and the file will go stale"** — Moombox could not rewrite `cookies.txt`.
  In Docker this is almost always a `cookies.txt` bind-mounted on its own or
  read-only; keep it inside the `/data` volume instead. Elsewhere, check the
  file's permissions.
- **YouTube works, then stops after you browse** — you kept using the browser
  session you exported. Export from a private window and close it.
- **"cookies.sqlite is locked by another process"** — close Firefox before
  importing from its profile.
- **Browser login on a server** — there is no browser to start; paste or
  upload a `cookies.txt` in Settings → **Cookies** instead.

## Disk space

| Notification | Meaning |
|---|---|
| *Disk Space Warning* | The output drive (or the staging drive, when it is a different and fuller one; the alert names which) passed [`disk_warn_percent`](../configuration/disk-memory-connectivity.md#diskdisk_warn_percent) (90 %). Repeated at most every 30 minutes. |
| *Disk Space Critical* | It passed [`disk_critical_percent`](../configuration/disk-memory-connectivity.md#diskdisk_critical_percent) (95 %). Backlog jobs stop being admitted (*Held: disk full*); live and running downloads are **not** paused. |
| *Disk Space Recovered* | Usage is back at least 2 points below the warning threshold. |
| *Disk Monitoring Failed* | Disk checks failed twice in a row (*volume offline or I/O error*); low-disk alerts are suspended until *Disk Monitoring Recovered*. Check that the output drive or network share is mounted. |

Free space by deleting old archives, clearing the **Orphaned** tab, or moving
[`paths.output_directory`](../configuration/paths-and-logs.md#pathsoutput_directory)
to a bigger disk. A recording is staged in full before it is muxed, and
while it is muxed the staged parts and the finished file exist side by side,
so leave room for about twice your largest recording. See
[Storage and files](../guide/storage.md).

## Internet outages

When Moombox loses the internet it logs *Internet connectivity lost*, pauses
channel monitoring, lets downloads wait, and admits no backlog (*Held:
offline*). The TUI shows **OFFLINE**. When it returns (*Internet connectivity
restored*) the monitors check again, and an *Outage Alert* notification
reports how long it lasted. Moombox declares itself offline only after two
failed checks in a row, and outages shorter than a minute send no alert.

!!! warning "Moombox thinks it is offline, but the internet works"
    Moombox checks reachability by opening TCP connections to
    [`connectivity.probe_targets`](../configuration/disk-memory-connectivity.md#connectivityprobe_targets)
    (`1.1.1.1:443`, `8.8.8.8:443`, `9.9.9.9:443`) every 5 seconds. If your
    network blocks all three (a strict firewall, some corporate or national
    networks), set targets you can reach, such as a host you trust on port
    443, and restart.

## Notifications do not arrive

- Only **Discord** webhooks are supported. The URL must be a Discord webhook
  URL or the `discord://ID/TOKEN` shorthand.
- Check that the target is enabled and that its **events** list includes the
  event; an empty list receives everything.
- Use **Send Test** in the webhook dialog (Settings → **Integrations**) to try
  the target.
- A notification that cannot be delivered during an internet outage is held
  and retried when the connection returns.

See [Notifications](../guide/notifications.md).

## High memory use

- Moombox's own soft cap is
  [`memory.go_soft_limit_mb`](../configuration/disk-memory-connectivity.md#memorygo_soft_limit_mb)
  (256 MB): above it the garbage collector works harder, but nothing is
  killed.
- The BotGuard sidecar settles around 150 MB and can spike to 400–500 MB.
- Each download may hold out-of-order segments in memory while one segment
  is retried, up to
  [`reorder_buffer_mb`](../configuration/downloader.md#downloaderreorder_buffer_mb)
  per download and
  [`reorder_budget_mb`](../configuration/downloader.md#downloaderreorder_budget_mb)
  in total (1024 / 4096 MB; 256 / 1024 MB on arm64). On a small machine lower
  these.
- If memory still grows, profile it as described in
  [Memory and CPU: profiling](#memory-and-cpu-profiling) and report it with
  the two heap snapshots.

## Reporting a bug

Open an issue on [GitHub](https://github.com/vampiricwulf/Moombox/issues)
with:

1. the Moombox version (`moombox -version`) and platform (Windows, Linux,
   Docker, x64 or arm64);
2. what you did, what you expected, what happened;
3. the relevant part of `moombox.log` at `DEBUG` level, and the job's log for
   a job problem;
4. `moombox-crash.log.1` for a crash, or a heap/goroutine profile for a
   memory or hang problem.
