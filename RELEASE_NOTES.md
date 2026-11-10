**Before you update:** this release moves the database to schema 27. Once it has opened your database, v2.9.7 and older can no longer open it. In the TUI, Cancel Job has moved from `A C` to `A X`; `A C` is now Import Chapters.

## Features

- **Twitch clips.** A pasted clip URL is archived as a job of its own, tagged CLIP in the dashboard and filterable with `type:clip`. A per-channel option, `twitch_archive_clips`, sweeps a channel's new clips hourly and queues them.
- **Twitch gap-fill from the archive VOD.** A live Twitch capture records which spans it lost: a late start, a restart, a long outage, or an ad break. Within 24 hours of the broadcast Moombox fetches those spans from the broadcast's archive VOD and keeps them as recovered parts beside the recording, which is never touched. Ad-break gaps are filled only on channels with `twitch_gap_fill_ad_breaks`. The job shows whether its gaps are pending, filled or kept as recorded.
- **Twitch video lists, collections and VOD backlog.** A pasted `/videos` or collection URL queues each VOD. A per-channel option, `twitch_vod_backlog`, archives a channel's past broadcasts hourly.
- **Chapters.** Archives now carry chapters: YouTube description chapters, Twitch VOD game chapters, and the categories seen during a live Twitch capture. The Web player shows a chapter strip under the video and a chapter list.
- **Import chapters from a timestamp comment.** Paste a viewer's timestamp comment into the Web player's chapter panel or the TUI (`A C`), preview it against the current chapters with an editable offset, and save. The original chapters are kept and can be restored (`A U`). Import is offered on complete Finished jobs.
- **Chat deletions and edits.** Deleted and replaced chat messages are marked in the replay, and every chat action Moombox does not render is kept in a `<name>.chat.raw.jsonl` sidecar.
- **Both UIs say why a job is waiting.** Upcoming rows show their wait line, Queued rows say what is holding the scheduler (offline, a YouTube rate limit, a full disk), and an Error row shows an automatic retry or mux that is still pending.
- **`prefer_hdr` setting**, and one format ranking shared by every YouTube path, matching yt-dlp's default order. A `yt:stretch` video is archived with its corrected aspect ratio.
- **`[twitch] client_id`** overrides the built-in Twitch Client-ID.

## Improvements

- **YouTube rate limits and captchas are waited out.** A rate-limited backlog VOD goes back to Queued for an hour instead of ending in Error, a broadcast keeps waiting, and backlog admission holds for the hour, across restarts. A job ends in Error only after 24 hours and at least three refusals. A captcha shows its own line and sends one notification, with another when it clears.
- **Live captures recover from more failures.** A segment YouTube keeps refusing now splits the capture into a new part instead of stalling it. A stream more than about five days old starts from the live playlist, since its beginning is already gone. Stream URLs are renewed before they expire. When the player's JavaScript challenges cannot be solved, a download falls back to formats that need none.
- **Reinitialize asks first.** Both UIs confirm a Reinitialize and name what it deletes: set-aside recordings, recorded parts, staged files and imported chapters, and any automatic retry or mux it cancels.
- **`/api/jobs/{id}/retry` continues the recording**, as Resume does, and never deletes staging. Starting over is `/reinitialize` alone.
- **Actions that cannot run are refused with a reason.** Mux, Resume and Reinitialize answer with a clear message (HTTP 409, a red line in the TUI) when the job's state does not allow them or another operation holds the job's files, instead of reporting success and doing nothing.
- **Format picker.** The picker starts on Auto with a [Best] badge on the rendition Auto would take, and tags damaged and segmented renditions. A rendition YouTube serves shorter than the video no longer wins selection.
- **A capture that started at the live edge says so** on the job, and each fall to the live edge is notified.
- **Twitch detection survives a dead login.** An expired Twitch token no longer stops every channel check: detection continues signed out and reports the lost login. A sub-only stream on an account without the subscription ends in Error instead of asking for new cookies.
- **Whole-file downloads retry harder**: ten retries on a 1 to 30 second ladder, and a server that ends a few bytes short of the stated size no longer fails the download. Encrypted HLS and DRM DASH are refused before anything is written; an all-DRM DASH manifest falls back to HLS when one is listed.
- **Brand accounts.** Members-only chat and the membership scan now run as the brand account the cookies belong to.
- **The Web player names an audio codec the browser cannot play** (E-AC-3) instead of playing silently.
- **TUI.** A channel whose last history scan failed shows a dim "BF: retry", and the Add Video tables open on the Best row.

## Bug Fixes

- **A private stream ends its wait.** A stream made private while Moombox waited for it was polled with a full extraction every cycle, 3,528 times over 31 hours in one case. It now ends the wait, and the waiting poll is one request.
- **A YouTube outage no longer ends a live capture.** An outage that refused every segment ended a still-live capture after about 35 minutes. It now waits while the stream is live.
- **A Cancel is never overwritten, and never loses a finished file.** Status changes decided on an earlier read are now written only if the status is still the same, so a Cancel that lands in between wins. A Cancel that arrives after the archive is written and checked ends Finished with its file.
- **Resume and Reinitialize no longer delete files under a running FFmpeg.** They wait for a Mux, chapter import or set-aside recovery on the same job to finish.
- **Chat status after a failure.** A job that ends in Error on any platform no longer leaves its chat showing as downloading.
- **Twitch gap-fill takes the right VOD.** It takes only an archive published within 10 minutes of the broadcast's start, checks its timeline against the recording, and keeps the recording as it is when they disagree.
- **Removing a Twitch channel no longer deletes VODs you pasted by hand.**
- **EU consent pages.** A signed-out session behind YouTube's consent redirect got an empty page: no player, no chat. Every YouTube page fetch now carries the consent cookie.
- **Unsolved `n` challenges fail clearly** instead of sending a URL YouTube refuses with a 403.
- **Post-live streams answered by the TV client** were treated as VODs and failed; so did Resume of a job with an incomplete tail in that window.
- **A refused first segment could leave hours of capture unmuxable.** The first segment carries the only init data; it is now retried rather than skipped.
- **Pre-roll ads.** A download waits out a pre-roll ad window before its first request instead of being refused in it.
- **A status probe refused by the bot wall is no longer stored as "not a stream".** Rows misfiled that way are reset once.
- **Stitched Twitch ads titled "Amazon"** are no longer recorded as content. A Twitch VOD that is still recording is followed to its end.
- **Age-gated videos** whose TV request failed are retried through the embed page.
- **Settings.** An edit made while a save is in flight is kept and sent next. An expired session reloads to the login page instead of showing "Disconnected". Delete All cannot be confirmed twice.
- **An update the updater refuses** (one is already being applied) shows a neutral notice instead of "Update failed".
- **TUI.** One chord window, cleared by Esc; mouse clicks follow the same overlay order as keys; a monitor with no channels shows a blank timer instead of "now"; late release-notes answers are dropped.
- **A connection loss that begins at startup raises its alert.**

## Internal

- **State-machine conventions.** Job verbs, wait lines, admission holds and pending automatic actions each have one Go predicate (`internal/jobverbs`, `internal/jobstate`) with a JavaScript mirror checked by a parity fixture. Closed vocabularies are typed, and per-job work on staging goes through one claim registry. `CLAUDE.md` records the six rules.
- **Twitch offline rows carry a park reason** (`twitch_offline`, `twitch_ended_offline`) instead of being recognised by their error text. Existing rows are converted at startup.
- **Database schema 24 to 27**, including the `twitch_gap_fills` table, a one-time reset of misfiled feed rows, and a capture's join point.
- **Spec trim.** `SPEC.md` and `docs/spec/` state current behaviour in short paragraphs, with test names in an appendix per file.
- **CI.** FFmpeg packages are cached instead of fetched from the Ubuntu mirror each run, and the Windows and Linux test jobs are green again.
- **Dependencies.** bubbletea 2.1.1, modernc.org/sqlite 1.61.1, goldmark 1.8.7.
- **Upstream review of 2026-11-05** (yt-dlp, ejs, BgUtils) is ported; `docs/spec/upstream-ports.md` records where it left off.
