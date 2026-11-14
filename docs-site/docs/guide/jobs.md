# Jobs and their lifecycle

Every video or stream Moombox records is a **job**. A job is created when a
channel monitor finds something to archive, when you add a video by hand
(**Add Video** in the dashboard, ++a++ ++a++ in the terminal UI, or
`moombox add` on the command line), or when you import an existing archive.
This page explains every status a job can be in, why a job is waiting, what
Moombox retries on its own, and every action you can take on a job.

## Statuses at a glance

| Status | What it means | What you can do |
|---|---|---|
| `Queued` | A backlog video waiting for its channel's turn. Only older VODs, a Twitch channel's VOD backlog, pasted Twitch video lists and swept clips wait here. | Cancel; Recover set-aside recordings |
| `Upcoming` | Waiting: for a scheduled stream to start, for a download slot, or to be processed. | Cancel |
| `Live` | The stream has been confirmed live; recording is starting. | Cancel |
| `Downloading` | Recording or downloading. | Cancel |
| `Muxing` | FFmpeg is combining the downloaded video, audio and chapters into the final `.mp4`. | Cancel |
| `Finished` | The archive is written. | Play, trim, mark watched, import chapters, delete; Resume if it has an *Incomplete tail*; Mux while a part its finalize could not mux is still in staging; Recover set-aside recordings |
| `Error` | The job failed. The error text says why. | Resume (YouTube), Reinitialize, Mux, Recover set-aside recordings, Delete |
| `Cancelled` | You cancelled it. Whatever was downloaded is kept in staging. | Resume (YouTube), Reinitialize, Mux, Recover set-aside recordings, Delete |
| `COOKIES?` | Parked: the platform refused the request and signing in (again) is the fix. | Fix your cookies; Resume (YouTube), Reinitialize, Recover set-aside recordings, Cancel, Delete |

`Finished`, `Error` and `Cancelled` are final: nothing moves a job out of
them except your actions and the few automatic recoveries described below.

## The normal path

```text
Queued ──► Upcoming ──► Live ──► Downloading ──► Muxing ──► Finished
(backlog     (waiting)  (live     (recording)    (FFmpeg)
 only)                  confirmed)

From any status: ──► Error, Cancelled or COOKIES?
```

- A **live or scheduled stream** starts in `Upcoming`, waits for the stream
  to go live, passes through `Live` and records in `Downloading`.
- A **VOD** (a past broadcast, an upload, a premiere, a clip) skips `Live`.
  It stays in `Upcoming` while it waits for a download slot, then goes to
  `Downloading`.
- **Backlog** videos start in `Queued` and are moved to `Upcoming` by the
  scheduler described next. Live streams, upcoming streams, newly published
  videos and anything you add by hand never wait in `Queued`.
- When the recording ends, the job goes to `Muxing` and then `Finished`.

### What happens at a restart

When Moombox starts, jobs that were `Upcoming`, `Live` or `Downloading` are
picked up again: a recording continues from its staged files where it can.
A job that was `Muxing` is muxed again from what is staged; if nothing is
staged (or it has an *Incomplete tail* still to fetch) it goes back to
`Downloading` instead. `Queued` jobs
wait for the scheduler as before. `Finished`, `Error`, `Cancelled` and
`COOKIES?` jobs are left alone.

## Queued: backlog admission

A scheduler admits `Queued` jobs, newest first, a few per channel at a time:
each channel may have at most
[`monitors.archive_slots`](../configuration/monitors.md#monitorsarchive_slots)
backlog downloads in progress (a channel can override it with its own
[`archive_slots`](../configuration/channels.md#channelsarchive_slots)).
Admitted VODs then also share the global VOD limit,
[`downloader.num_parallel_downloads`](../configuration/downloader.md#downloadernum_parallel_downloads).

Admission can also be **held**. A `Queued` row's progress line says why, in
both UIs. The first hold that applies is shown, in this order:

| Line | Why | Ends when |
|---|---|---|
| `Held: offline` | Moombox's connectivity check says the internet is unreachable. Nothing is admitted. | Connectivity returns. |
| `Held: disk full` | The output disk, or the staging disk when it is a different and fuller one, reached [`disk_critical_percent`](../configuration/disk-memory-connectivity.md#diskdisk_critical_percent). Nothing is admitted. | Usage falls **2 points** below the critical threshold. |
| `Held: YouTube rate limit until 14:05` | YouTube rate-limited the session. No **YouTube** backlog is admitted (Twitch backlog still is). | The hour runs out. |
| `Retry 2 of 3 at 14:05` | This video's last attempt failed in a way worth retrying, and it was sent back to `Queued`. | The backoff runs out. |
| `Waiting in queue...` | Nothing holds it; it waits for a free archive slot. | A slot frees up. |

The disk-full hold and the rate-limit hold survive a restart. Live
recordings and downloads already running are **not** paused by any of these
holds — only new backlog admissions are.

!!! note "A disabled channel"
    A disabled channel gets no slots, so its `Queued` backlog stays paused
    until you enable it again.

### Backlog retries

A backlog VOD that fails for a reason that may pass goes back to `Queued`
instead of ending in `Error`:

- **A temporary failure before the download** (network error, timeout, a
  server error) or **a full disk** during the download or mux: it is
  retried after 5, then 10, then 20 minutes (`Retry 1 of 3`, …). After the
  third retry in a row fails, the job ends in `Error` with *(gave up after 3
  retries)* added to the error. A video that is really gone (removed,
  private, members-only) is not retried. A disk-full retry keeps its
  staging and resumes from the last checkpoint.
- **A YouTube rate limit**: the job goes back to `Queued` for an hour, and
  this does not count against its three retries. Only a rate limit that has
  lasted **24 hours with at least 3 refusals** ends the job in `Error`, with
  *(gave up after 24 hours of refusals)*.

Any action you take on the job (Resume, Reinitialize, Cancel) starts a fresh
retry budget.

## Upcoming, Live and Downloading: progress and wait lines

While a job is working, its progress line shows counters, for example
`V: 1234/1300 A: 1234/1300 C: 5678` — video segments, audio segments and
chat messages so far. When a job is **waiting** rather than moving data, the
line has the shape `<what>... (<time>)`. The TUI shows such a row with a dim
`⋯ wait` tag instead of a frozen percentage, and the dashboard shows the
wait line on an `Upcoming` row in place of its usual *Last check: …* time.

| Wait line | Meaning |
|---|---|
| `Waiting for a download slot... (3m)` | A VOD waiting for one of the `num_parallel_downloads` slots. |
| `Waiting for network connectivity... (1m 20s)` | Moombox is offline; the job continues when connectivity returns. |
| `Rate-limited by YouTube; fetching again... (24m 0s left)` | YouTube refused the request with a rate limit. A job that is not backlog waits in place and tries again about every 30 minutes. |
| `YouTube asks for a captcha... (24m 0s left)` | YouTube wants a captcha solved. Solve it in a browser signed in with the same account; Moombox sends one notification per spell. |
| `Waiting out YouTube's pre-roll ad... (12s left)` | YouTube makes the stream URLs usable only after an ad's length; Moombox waits it out. |
| `Waiting for first segment... (…)` / `Waiting for next segment... (…)` | The stream has not produced new data yet. |
| `Connection lost - reconnecting... (…)` | A network hiccup during recording. |
| `Rate-limited - backing off... (…)` | Segment requests are being throttled. |
| `Segment fetch failing - retrying... (…)` | A segment keeps failing, or the request for the next one gets no answer from YouTube; it is retried. |
| `Verifying stream ended... (…)` | No new data; Moombox is checking whether the stream really ended. See [`downloader.maximum_timeout`](../configuration/downloader.md#downloadermaximum_timeout). |
| `Stream interrupted - waiting for resume... (…)` | The YouTube broadcast was interrupted and may come back. See [`downloader.interruption_timeout`](../configuration/downloader.md#downloaderinterruption_timeout). |

An `Upcoming` row that is a VOD (or an admitted backlog video) rather than a
stream to wait for shows **Awaiting processing** when no wait line is up.

A YouTube rate limit while a scheduled stream is still upcoming costs it
nothing. Once the stream is live or over, a spell of rate-limit refusals that
lasts 24 hours with at least 3 refusals ends the job in `Error`.

### Chat capture status

When a job captures chat, its details show a **Chat** row: a coloured badge
on the dashboard, a coloured value in the TUI, with the message count beside
it once there is one (for example `finished (5678 messages)`).

| Chat status | Meaning |
|---|---|
| `pending` | The chat capture is set up but has not received anything yet. |
| `downloading` | Messages are being received. |
| `unavailable` | The video has no chat, or the capture ended without any messages. |
| `finished` | The capture completed. |
| `incomplete` | Shown in the warning colour. The capture **ended without completing** — a Twitch VOD's chat stopped paging, a Twitch live chat connection ran out of reconnects, you cancelled the job, or the download ended in `Error` — so messages may be missing. The archive that was written is intact. The chat's resume data is kept in staging, which keeps that staging folder around for a while (see [When staging is cleaned up](storage.md#when-staging-is-cleaned-up)). |

## Finished

The archive and its companion files are in the output directory (see
[Storage and files](storage.md)), and the staging folder is removed unless
something in it still needs keeping.

### Incomplete tail

A YouTube livestream that was interrupted and did not come back within
[`downloader.interruption_timeout`](../configuration/downloader.md#downloaderinterruption_timeout)
is finalized with what was recorded and flagged **Incomplete tail**: a
warning icon on the dashboard row (*Recording is missing its tail — Resume
to fetch it*) and a badge in the job details. Its staging and resume data
are kept, so:

- **Resume** appends the missing end to the same recording, and
- if a monitor sees the same broadcast live again, Moombox resumes it
  automatically (at most once every 5 minutes per job), as long as the
  staging is still on disk.

The staging is protected for
[`downloader.incomplete_staging_expiry_days`](../configuration/downloader.md#downloaderincomplete_staging_expiry_days)
(7 days by default). The badge itself never goes away.

### Finished jobs leave the Tasks list

Finished jobs older than
[`monitors.hide_finished_age_days`](../configuration/monitors.md#monitorshide_finished_age_days)
(30 days by default) move from the dashboard's **Tasks** tab to the
**Archived** tab. Nothing is deleted.

## Error, Cancelled and COOKIES?

### Error

The job failed; the error text in the details says why. Staged data is
kept, so you can usually choose between:

- **Resume** (YouTube only, when something is staged) — continue the same
  recording;
- **Mux** — turn whatever was captured into an archive as it is;
- **Reinitialize** — throw the attempt away and start from the beginning.

An **age-restricted** YouTube video ends here, not in `COOKIES?`, with
*Age restricted: … (cookies from an age-verified account would allow this)*.
No *Job Failed* notification is sent for it and nothing resumes it on its
own, so the error text is the only sign: add cookies from an age-verified
account (see [Cookies and sign-in](cookies.md)), then **Reinitialize** the
job. An ordinary private or removed video also ends in `Error`.

Some Twitch errors carry a reason that lets Moombox act on its own — see
[Automatic recovery on Twitch](#automatic-recovery-on-twitch).

### Cancelled

Cancelling stops the job and writes `Cancelled`. Nothing is deleted:
whatever was recorded stays in staging, so Resume, Mux and Reinitialize work
exactly as for `Error`. A Cancel that arrives after the archive has already
been written is refused with *Not cancelled — the job is already finishing:
its archive is written*; the job ends `Finished`.

### COOKIES?

The platform refused the job and credentials are the fix. The error text
says which case it is:

- *Member-only: …* — a members-only video, requested while signed out (or
  with dead cookies), or with an account that is not a member;
- *Login required: …* — YouTube wants a signed-in session;
- *Private video: … they look dead …* — a private video, while YouTube
  answered your loaded cookies as signed out (if this account was granted
  access, fresh cookies may restore it);
- a Twitch stream that is subscriber-only.

Age-restricted videos are **not** parked here; they end in
[`Error`](#error). The full list of YouTube refusals and where each one
ends is in
[YouTube](youtube.md#members-only-private-and-age-restricted-videos).

The job is parked rather than failed, and Moombox brings
it back on its own once the credentials change:

- **Signed out or dead cookies:** as soon as Moombox sees working cookies
  again (a cookie refresh, an import, a login through **Cookie Login**), it
  resumes every job parked this way.
- **Signed in, but the account lacks the membership:** refreshing the same
  account cannot help, so these jobs are resumed only when the cookies
  belong to a **different** account than the one that was refused.

A live or manually added job resumes to `Upcoming`; a backlog video returns
to `Queued` and is re-admitted at the normal pace. See
[Cookies and sign-in](cookies.md) for fixing credentials. A Twitch
subscriber-only refusal for a session that is already signed in ends in
`Error` instead, because there is no other account to wait for.

### Automatic recovery on Twitch

Two kinds of Twitch `Error` rows are handled by the Twitch monitor, as long
as that channel is still monitored. Both UIs show the pending action in the
job details:

*Retries automatically if the broadcast is live again, while the channel is monitored*
:   A live job that found the channel offline right after the monitor saw it
    go live (a brief flap), or could not reach Twitch at all, **before
    recording anything**. When the monitor finds the same broadcast live
    again, the job is reset and starts again — at most **2** times. A job
    you added by hand is not retried this way. The first failure sends a
    *Job Failed* notification; a failed automatic retry sends none while
    another retry is still possible, and the final failure does.

*Muxes automatically once Twitch confirms the broadcast over, while the channel is monitored*
:   A live capture that stopped while nothing said the broadcast was over.
    Its staging is kept, and once the monitor sees the channel offline (or
    live with a different broadcast), Moombox muxes it exactly as the Mux
    action would — once.

A Twitch capture interrupted by a Moombox restart, whose broadcast ended
while Moombox was down, ends in `Error` with its footage staged; nothing
automatic follows — use **Mux** to turn it into the archive.

## Job actions

Each action is offered only where it applies. The dashboard shows them as
row icons (Cancel, Resume or Reinitialize, Delete), in a row's **⋮** menu on
narrow screens, and as buttons in the **Job Details** dialog (click a row).
The terminal UI uses chords — see
[Keyboard shortcuts](../reference/shortcuts.md) and
[Terminal UI](terminal-ui.md).

| Action | Dashboard | TUI | Offered for |
|---|---|---|---|
| Cancel | **Cancel** (confirm) | ++a++ ++x++ (confirm ++x++) | `Queued`, `Upcoming`, `Live`, `Downloading`, `Muxing`, `COOKIES?` |
| Resume | **Resume** | ++a++ ++r++ | YouTube jobs in `Error`, `Cancelled`, `COOKIES?`, or `Finished` with an *Incomplete tail* — and only when staging holds something |
| Reinitialize | **Reinitialize** (confirm, lists what it deletes) | ++a++ ++i++ (confirm ++i++) | `Error`, `Cancelled`, `COOKIES?` |
| Mux | **Mux** | ++a++ ++m++ (confirm ++m++) | `Error` or `Cancelled` with staged media; `Finished` with a part left unmuxed in staging |
| Recover set-aside recordings | **Recover** in the details' *Set-aside recordings* row | ++a++ ++s++ (confirm ++s++) | Any job that is not active and whose staging holds a set-aside recording |
| Delete | **Delete** (confirm) | ++a++ ++d++ (confirm ++d++) | `Finished`, `Error`, `Cancelled`, `COOKIES?` |
| Mark watched / unwatched | **Mark Watched** / **Mark Unwatched** | ++a++ ++w++ (toggles) | `Finished` |
| Trim | **Create Trim** | ++a++ ++t++ | `Finished` with an archive |
| Import chapters | **Import chapters** in the Player | ++a++ ++c++ | `Finished` without an *Incomplete tail* |
| Restore original chapters | **Restore original** in the import dialog | ++a++ ++u++ (confirm ++u++) | The same, once chapters have been imported |
| Play | **Play** | — | `Finished` with an archive |
| Open folder | **Open Folder** | ++o++ ++f++ | Finished jobs with a file, and active jobs |
| Open stream page | **Open URL** | ++o++ ++s++ | Jobs with a stream URL |
| Copy stream URL | — | ++o++ ++c++ | Jobs with a stream URL |
| Job log | *Job Logs* section of the details | ++o++ ++l++ | Any job |

In the TUI, a confirm-gated chord needs its last key pressed again within 3
seconds (for example ++a++ ++x++ ++x++).

### Cancel

Stops the job and marks it `Cancelled`; staged data is kept. Cancelling a
`Queued` or `Upcoming` job simply stops it from ever starting. Cancelling a
`COOKIES?` job stops Moombox from resuming it automatically.

### Resume

Continues the **same** recording from its staging and resume data, keeping
everything already captured. It sets the job back to `Downloading`. Resume
is **YouTube only**; it is refused with *Resume is only supported for
YouTube jobs — use Reinitialize instead* for Twitch, and with *No staging
files found — use Reinitialize instead* when nothing is staged. A Resume
clicked while the job's previous run is still shutting down waits for it
(up to a minute) instead of racing it.

On the dashboard row the restart icon is **Resume** where the job can
continue, and **Reinitialize** only where it cannot. Whichever applies is
also in the Job Details dialog and, on narrow screens, the row's ⋮ menu.

### Reinitialize

Starts the job over from scratch: the row goes back to `Upcoming` and the
video is downloaded again from the beginning. Before it does, it
**deletes**:

- the job's staging folder (including set-aside recordings),
- recorded parts of a split recording that are already in the output
  directory, with their chat files,
- an imported chapter list,

and it cancels a pending automatic mux or retry. The confirm in both UIs
asks the server first and names what will go, for example *This deletes 2
recorded parts (3.1GB) and 41 staged files (812.0MB).* — or says it could
not tell. Reinitialize also gives a Twitch job a fresh automatic-retry
budget. This cannot be undone.

!!! warning
    If a job has a partial recording you might want, **Mux** it (or
    **Recover** its set-aside recordings) before reinitializing.

### Mux

Muxes whatever the job's staging holds into an archive, as it is: for a
failed or cancelled job, that is the footage captured so far. The job goes
to `Muxing` and then `Finished` (or back to `Error` if the mux fails). For a
`Finished` job whose recording was split into parts, Mux is offered only
while a part its finalize could not mux is still in staging.

### Recover set-aside recordings

When Moombox restarts a recording and cannot continue the file it was
writing (for example, after an update or a crash at an awkward moment), it
never truncates it: the old file is kept in staging as a **set-aside
recording** and a fresh one is started. Normally these are muxed
automatically into their own files when the job finishes. One still in
staging is listed in the job details under *Set-aside recordings*.

**Recover** muxes each one into its own file beside the archive, named
`<archive name>.restart-<timestamp>.mp4`, and copies a chat capture kept in
staging beside the first one. These files overlap the archive's opening, so
they are never merged into it. The job's status does not change; progress
and failures appear in the job's log. Staging is cleaned up afterwards only
when nothing else unmuxed is left in it. Recover and Mux cannot run on the
same job at the same time (see [When two actions collide](#when-two-actions-collide)).

### Delete

Removes the job from the list. It does **not** delete any files: the archive, its chat and thumbnail, and any
staging stay on disk and appear in the dashboard's **Orphaned** tab (TUI:
++a++ ++o++), where you can delete them. The video also stays in Moombox's
processed history, so the monitors do not pick it up again — remove its
entry under **Orphaned History Entries** on the same tab if you want it
archived again.

### Watched

**Mark Watched** shows a watched marker on the row (a dim `•` in the TUI).
**Mark Unwatched** clears it. Either one also clears the saved playback
position the Player resumes from.

### Trim and chapters

**Create Trim** cuts a section of a finished archive into its own file — see
[Player and chat replay](player.md). Chapter import and restore are covered
in [Chapters](chapters.md).

### When two actions collide

Some actions cannot run on a job while another one is working it. A refused
action changes nothing; the dashboard shows the reason as an error toast and
the TUI on its feedback line:

| Message | Why | What to do |
|---|---|---|
| *another operation is already working this job's staging directory* | A Mux, a Recover, a chapter import or Moombox's startup cleanup is already using this job's staging folder. Mux also answers this while the job's last run is still shutting down. | Wait for the other operation to finish (watch the job's log), then try again. |
| *reinitialize already pending* (or *resume already pending*) | A different action is already waiting for the job's previous run to exit — for example, you clicked Resume while a Reinitialize waits. | Let the pending action run; act again afterwards if you still need to. |
| *Job cannot be resumed in current state (Finished)* (the action and status vary) | The job moved on between what your screen showed and the click — a stale row, another operator, or a monitor acting on it. | Look at the job's current status and choose again. |
| *Job is active; set-aside recovery would race the download* | Recover was asked for a job that is `Upcoming`, `Live`, `Downloading` or `Muxing`. | Wait until the job is no longer active. |

Pressing the **same** action again while it is already waiting or running
is not an error: it is accepted quietly, since that action is already on its
way.

### Batch actions

In the dashboard, tick the checkboxes on several rows (shift+click selects a
range) and use the bar that appears: **Cancel**, **Resume**,
**Reinitialize**, **Delete**, **Mark Watched**, **Mark Unwatched**. Each
asks once for the whole selection and skips rows that no longer qualify. In
the TUI, press ++space++ on rows in the Tasks panel to select them; the
batch-capable chords (++a++ ++x++, ++a++ ++r++, ++a++ ++i++, ++a++ ++d++,
++a++ ++w++) then act on the selection.

## Tips

- **A job is stuck in `Upcoming`.** Read its line: a hold, a wait line, or
  *Last check* (a scheduled stream that has not started). ++r++ ++m++ in the
  TUI (*Check Monitors Now*) runs every monitor at once.
- **Something failed overnight.** Open the job's details and its *Job Logs*
  (TUI: ++o++ ++l++). Per-job logs are kept in memory only — the most
  recent 100 to 200 lines per job — so after a restart, or for older
  lines, read `moombox.log` instead (see
  [Storage and files](storage.md#logs)).
- **Before deleting staging by hand**, check the job's status: the staging
  of an `Error` or `Cancelled` job is what Resume and Mux work from, and it
  is listed in the Orphaned tab like any other folder.
- **You get notified** of jobs found, finished, failed and parked through
  [Notifications](notifications.md).
