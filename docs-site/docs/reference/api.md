# API

Everything the web dashboard does goes through a small HTTP API on the
dashboard's own port, plus one WebSocket for live updates. You can call the
same API from scripts: add a video, list jobs, trigger a monitor check, read
the status. This page is an overview for that use; the dashboard itself is the
reference client.

!!! warning "Unversioned and internal"
    All endpoints live under `/api/` with no version number. They exist for the
    dashboard and change with it. A script that relies on them should be
    re-checked after updates.

## Base URL

`http://<host>:<port>` — `https://` when
[`network.https_enabled`](../configuration/network.md#networkhttps_enabled) is
on. The port is [`network.port`](../configuration/network.md#networkport)
(default `774`), or the nearby port Moombox fell back to when that one was
busy (the log line `Web dashboard available at …` shows the real one).

On `localhost` and `lan`, use an IP address or `localhost` as the host, not a
DNS name: a name is refused with `403` (see
[Security](security.md#open-it-by-ip-address-or-localhost)).

## Authentication

There are no API keys. A client is let in on the same terms as a browser:

1. **The access check.** The client's address must pass
   [`network.network_access`](../configuration/network.md#networknetwork_access).
2. **Login, only for remote clients on `external`/`public` with a password.**
   Clients on this machine or a private network never log in. Everyone else
   logs in and sends the cookies back.
3. **An `Origin` header on every change.** Any `POST`, `PUT` or `DELETE` must
   carry an `Origin` (or `Referer`) that names the dashboard itself —
   scheme, host and port as you are calling it. Without one the answer is
   `403 {"error":"Forbidden: missing origin"}`.

From this machine:

```bash
# Read: no Origin needed
curl -s http://127.0.0.1:774/api/jobs

# Change: send the dashboard's own address as Origin
curl -s -X POST \
  -H "Origin: http://127.0.0.1:774" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://www.youtube.com/watch?v=dQw4w9WgXcQ"}' \
  http://127.0.0.1:774/api/jobs
```

Remotely, with a password:

```bash
BASE=https://moombox.example.com
# Log in once; the cookie jar keeps both cookies
curl -s -c jar.txt -b jar.txt -X POST \
  -H "Origin: $BASE" -H "Content-Type: application/json" \
  -d '{"password":"your password"}' "$BASE/api/auth/login"

# Later calls send the jar
curl -s -b jar.txt -c jar.txt "$BASE/api/status"
```

The login answers `{"success":true}` and sets two cookies:
`moombox_session` (24 hours, renewed while used, lost when Moombox restarts)
and `moombox_client`, a remembered-client token that lasts
[`network.client_token_ttl_days`](../configuration/network.md#networkclient_token_ttl_days)
and quietly starts a new session when the old one is gone. Keep and reuse the
same cookie jar: each fresh login without the old `moombox_client` cookie adds
another entry to **Connected Clients**, while one that sends it replaces it.
Logins are limited to 5 a minute per client.

Without valid cookies, a remote client gets
`401 {"error":"Authentication required"}` from any `/api/` path.

## Responses and errors

- Bodies are JSON; field names are mostly camelCase, except configuration,
  which uses the `config.toml` key names.
- Errors are `{"error":"<message>"}`. Settings validation failures are
  `400 {"error":"Validation failed","details":{"<key>":"<message>"}}`.
- `409 Conflict` means the job moved on since you looked (it finished, or
  another action holds it); the message says why. Re-read and try again.
- `429 Too Many Requests` carries a `Retry-After` header and
  `{"error":"Too many requests, please try again later","retryAfter":<seconds>}`.
- Request bodies are limited to 1 MB (the import upload to 500 MB).

## Rate limits

Counted per client address (per `/64` for global IPv6; behind a reverse proxy
per real client only when the proxy is in
[`trusted_proxies`](../configuration/network.md#networktrusted_proxies)). In
the tables below, **Shared** marks the endpoints that share one budget of
**20 per minute**.

| Budget | Endpoints |
|---|---|
| 5 per minute | `POST /api/auth/login` |
| 3 per minute | `POST /api/auth/set-password` and `POST /api/auth/remove-password` together |
| 20 per minute (Shared) | Marked below; also a `POST /api/config/channels` or `PUT /api/config` that adds a channel given as a URL or `@handle` (which must be resolved) |
| 5 per minute | `POST /api/import` |
| 10 per minute | `POST /get_pot` |

`POST /api/monitors/check-now`, `POST /api/backfill/rescan` and
`POST /api/update/check` are not rate limited but each accepts one call per 30
seconds (across all clients); a call inside that window answers
`200 {"success":false,"debounced":true,"retryAfterMs":…}`.

## Endpoints

`{id}` is a job ID unless noted. **Local** marks endpoints that only answer a
direct loopback connection (see
[Security](security.md#actions-limited-to-this-machine)).

### Authentication and clients

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/auth/status` | Open to everyone. `{authRequired, authenticated, hasPassword, passwordlessExternal}`. |
| `POST` | `/api/auth/login` | Body `{password}`. Sets the session and client-token cookies. |
| `POST` | `/api/auth/logout` | Ends the session, revokes this browser's client token, clears both cookies. |
| `POST` | `/api/auth/set-password` | Body `{currentPassword, newPassword}`. Set or change the password (8–128 characters). Needs a logged-in session or a local/private address; the first password needs a session or this machine. |
| `POST` | `/api/auth/remove-password` | Body `{currentPassword}`. Removes the password and resets access to `localhost`. |
| `GET` | `/api/client-tokens` | List remembered clients: `id`, `tokenPrefix`, `label`, `createdAt`, `lastUsedAt`, shortened `lastIp`. |
| `DELETE` | `/api/client-tokens/{id}` | Revoke one remembered client (`{id}` is the token's ID). |

### Jobs

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/jobs` | Active (not archived) jobs. Optional `?offset=` and `?limit=` (1–1000) switch to `{data, pagination}`. |
| `GET` | `/api/jobs/archived` | Archived jobs (`Finished` jobs older than [`monitors.hide_finished_age_days`](../configuration/monitors.md#monitorshide_finished_age_days)); same pagination. |
| `GET` | `/api/jobs/{id}` | One job, with the staging facts the details dialog uses. |
| `POST` | `/api/jobs` | Add a job (Shared). Body at minimum `{url}`: a YouTube video or stream, or a Twitch channel, VOD, clip, video list or collection (a list queues one VOD job per video). Optional fields include `outputDirectory`, `selectedVideoItag`/`selectedAudioItag`, `startTime`/`endTime` and `quality_preference` (Twitch). |
| `POST` | `/api/jobs/{id}/cancel` | Cancel an active job. |
| `POST` | `/api/jobs/{id}/resume` | Resume a YouTube job from its staging files (from `Error`, `Cancelled`, `COOKIES?`, or `Finished` with an incomplete tail). |
| `POST` | `/api/jobs/{id}/retry` | Same as `resume`. |
| `GET` | `/api/jobs/{id}/reinitialize/preview` | What a reinitialize would delete (set-aside recordings, recorded parts, staged files, imported chapters) and which pending automatic action it would cancel. |
| `POST` | `/api/jobs/{id}/reinitialize` | Start the job over, deleting its staging files (from `Error`, `Cancelled`, `COOKIES?`). |
| `POST` | `/api/jobs/{id}/mux` | Mux what is already in staging without downloading again. |
| `POST` | `/api/jobs/{id}/recover-asides` | Mux the recordings set aside by a mid-stream restart into their own files. |
| `POST` | `/api/jobs/{id}/open-folder` | Local. Open the job's folder in Explorer (Windows) or the desktop file manager (Linux). |
| `DELETE` | `/api/jobs/{id}` | Delete the job's row (an active job is cancelled first). Files stay on disk and show up as orphaned. |
| `GET` | `/api/jobs/{id}/logs` | The job's own log lines. |
| `GET` | `/api/formats/{videoId}` | Shared. Available formats for an 11-character YouTube video ID. Refused when started by another site's page. |

### Playback and media

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/jobs/{id}/video` | Stream the archive (supports range requests for seeking). |
| `GET` | `/api/jobs/{id}/thumbnail` | The locally stored thumbnail. |
| `GET` | `/api/jobs/{id}/segments` | Parts of a multi-part recording. |
| `GET` | `/api/jobs/{id}/segments/{index}/video` | Stream one part. |
| `GET` | `/api/jobs/{id}/segments/{index}/chat` | One part's chat file. |
| `GET` | `/api/jobs/{id}/chat` | The job's chat file. |
| `GET` | `/api/jobs/{id}/audio-info` | Codec and channel count of the first audio track (for the player's unplayable-audio notice). |
| `GET` | `/api/jobs/{id}/watch-state` | Watched flag, resume position and chat offset. |
| `PUT` | `/api/jobs/{id}/resume-position` | Save the playback position. |
| `POST` | `/api/jobs/{id}/resume-position` | Same, for browsers that can only `POST` on page close. |
| `POST` | `/api/jobs/{id}/watched` | Mark watched (clears the resume position). |
| `DELETE` | `/api/jobs/{id}/watched` | Mark unwatched. |
| `POST` | `/api/jobs/batch/watched` | Body `{jobIds:[…]}`. Mark several watched. |
| `DELETE` | `/api/jobs/batch/watched` | Body `{jobIds:[…]}`. Mark several unwatched. |
| `PUT` | `/api/jobs/{id}/chat-offset` | Body `{chatOffset}`. Save the chat timing offset. |
| `DELETE` | `/api/jobs/{id}/chat-offset` | Reset the chat offset to 0. |

### Chapters and trims

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/jobs/{id}/chapters` | The job's chapters, in seconds on the player's timeline. |
| `POST` | `/api/jobs/{id}/chapters/preview` | Parse a pasted timestamp comment against the archive without saving. |
| `POST` | `/api/jobs/{id}/chapters` | Save a pasted timestamp comment as the job's chapters. |
| `POST` | `/api/jobs/{id}/chapters/restore` | Bring back the chapters the first import replaced. |
| `GET` | `/api/jobs/{id}/trims` | Trim clips made from the job. |
| `POST` | `/api/jobs/{id}/trims` | Shared. Start a trim clip. |
| `DELETE` | `/api/jobs/{id}/trims/{trimId}` | Delete a trim clip. |

### Status, monitors and logs

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/status` | Version, uptime, memory, disk, cookie status per platform, next monitor checks, per-channel health, BotGuard sidecar health, pending update, admission holds. |
| `GET` | `/api/stats` | The figures on the Stats tab (`disk`, `storage`, `activity`), plus `twitchHints`, which only the API shows: `hits` and `misses` counters, since Moombox started, of Twitch jobs that reused the stream details the Twitch monitor had just fetched (a hit) or had to ask Twitch again (a miss). Hits that stay at zero while the monitor keeps finding Twitch broadcasts mean every Twitch job is making that extra request. |
| `GET` | `/api/logs` | Recent log lines. |
| `POST` | `/api/monitors/check-now` | Poll all monitors now (once per 30 s). |
| `POST` | `/api/backfill/rescan` | Re-scan the feed history of every YouTube channel (once per 30 s). |
| `POST` | `/api/restart` | Restart Moombox. |

### Configuration and channels

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/config` | The current configuration (without the password hash). |
| `PUT` | `/api/config` | Change settings. Send only the keys you change, nested by section as in `config.toml`, e.g. `{"monitors":{"archive_slots":2}}`. Refuses `external` access without a password, and the hand-edit-only `public`. |
| `POST` | `/api/config/channels` | Add a channel, or update one with the same `id`. Shared when the channel is given as a URL or `@handle`. |
| `PUT` | `/api/config/channels/reorder` | Body `{ids:[…]}` naming every channel once, in the new order. |
| `GET` | `/api/config/channels/{id}/removal` | What removing the channel would do to its jobs (`{id}` is the channel ID). |
| `DELETE` | `/api/config/channels/{id}` | Remove a channel. |
| `POST` | `/api/resolve-channel` | Shared. Turn a channel URL or `@handle` into a channel ID and name. |
| `POST` | `/api/notifications/test` | Body `{url}`. Send one test notification to a webhook URL, saved or not. |

### Cookies

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/cookies/recheck` | Re-check the cookies now. |
| `POST` | `/api/cookies/auto-refresh` | Shared. Refresh cookies from the headless browser or the browser profile. |
| `POST` | `/api/cookies/import` | Shared. Import a Netscape `cookies.txt` (plain-text body or multipart part `cookies`, up to 512 KiB). |
| `POST` | `/api/cookies/auto-setup/start` | Shared, Local. Open the login browser (body `{platform}`: `youtube` or `twitch`). |
| `POST` | `/api/cookies/auto-setup/finish` | Shared, Local. Take the cookies from the login browser. |
| `POST` | `/api/cookies/auto-setup/cancel` | Shared, Local. Close the login browser. |
| `POST` | `/api/cookies/auto-setup/abandon` | Shared, Local. Release the setup when the page closes, without closing the browser. |
| `POST` | `/api/auto-cookies/validate-browser-path` | Shared. Check a browser executable path. |
| `GET` | `/api/cookies/auto-status` | State of the automatic cookie service. |

### Files and history

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/files/orphaned` | Output files and staging folders no job uses. |
| `DELETE` | `/api/files/orphaned` | Body `{paths:[…]}`. Delete them. |
| `GET` | `/api/history/orphaned` | History rows with no job, which stop a video being discovered again. |
| `DELETE` | `/api/history/orphaned` | Body `{videoIds:[…]}`. Remove them so the videos can be found again. |
| `POST` | `/api/import` | Upload a zip archive to import as a job (5 per minute). |

### Updates, FFmpeg, setup and plugin

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/update/status` | The pending update, if any. |
| `POST` | `/api/update/check` | Check GitHub for a newer release (once per 30 s). |
| `POST` | `/api/update/apply` | Local, and the page's address must be `localhost`, `127.0.0.1` or `::1`. Download, verify and install the update, then restart. |
| `POST` | `/api/update/verify` | Verify the running binary's signature and its release's signed manifest. |
| `GET` | `/api/update/release-notes` | Release notes; optional `?version=x.y.z`. Refused when started by another site's page. |
| `POST` | `/api/update/dismiss` | Body `{tagName}`. Skip the pending release. |
| `GET` | `/api/ffmpeg/check` | Is FFmpeg available, and which version. Refused when started by another site's page. |
| `POST` | `/api/ffmpeg/check` | Shared. Body `{path}`. Check an FFmpeg path and save it if it works. |
| `GET` | `/api/ffmpeg/install-suggestion` | The package-manager command for this Linux distribution (empty on Windows). |
| `GET` | `/api/ffmpeg/install-options` | Where FFmpeg can be downloaded from. |
| `POST` | `/api/ffmpeg/install` | Shared. Start downloading FFmpeg. |
| `POST` | `/api/ffmpeg/install/confirm` | Shared. Confirm the install location. |
| `POST` | `/api/ffmpeg/install/reject` | Shared. Cancel the install. |
| `GET` | `/api/setup/status` | Whether first-run setup is done, plus FFmpeg state. Refused when started by another site's page. |
| `POST` | `/api/setup/complete` | Local, once. Save the first-run wizard's settings and restart. |
| `GET` | `/api/ytdlp-plugin/status` | Is the yt-dlp plugin installed, and for which port. |
| `POST` | `/api/ytdlp-plugin/install` | Install or update the yt-dlp plugin. |

### PO-token endpoints for yt-dlp

These sit outside `/api/` because the yt-dlp plugin expects these paths. See
[yt-dlp plugin](../guide/ytdlp-plugin.md).

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/ping` | Open to everyone. Lets the plugin find Moombox. |
| `POST` | `/get_pot` | Local, 10 per minute. Mint a PO token. |
| `POST` | `/invalidate_caches` | Local. Drop all PO-token caches. |
| `POST` | `/invalidate_it` | Local. Invalidate the identity token. |
| `GET` | `/minter_cache` | Local. List cached minter keys. |
| `GET` | `/pot_stats` | Local. PO-token counters. |

## WebSocket

The dashboard keeps one WebSocket open for live updates. Connect to the
dashboard's address with `ws://` (or `wss://` over HTTPS); the dashboard uses
the root path. The connection passes the same access check, host rule and
login cookies as any request, and an `Origin` header, when sent, must name the
dashboard.

- Every message is JSON: `{"type":"<type>","payload":…}`.
- **Send `{"type":"ping"}` at least every 90 seconds** (the dashboard sends one
  every 15 s), or the server closes the connection. The server answers
  `pong`, and also pings every 30 seconds itself.
- The server accepts messages of at most 4 KiB; `ping` is the only one it
  uses.
- A client that falls behind loses its oldest queued messages and then gets a
  fresh `initial_state` instead (at most once a second).

| Type | Payload | Sent when |
|---|---|---|
| `initial_state` | All jobs, recent log lines, next monitor checks, connectivity, the hide-finished age, backfill progress, running trims | Once, right after connecting |
| `job_update` | One full job | A job changes in any way other than progress (status, error, a new job, …) |
| `job_progress` | `{id, status, progress, percent, speed, eta, …}` | Each progress report of a running download; merge it onto the job you hold |
| `jobs_update` | All jobs | A job is added or deleted |
| `job_deleted` | `{id}` | A job row is removed |
| `config_update` | `{hideFinishedAgeDays}` | A setting that changes what the dashboard shows |
| `log` | One log line (with its number in `seq`) | A log line is written |
| `check_timers` | `{feed, decapi, twitch}` | The monitors' next check times change |
| `backfill_status` | `{channel, tab, pages, state}` | Feed-history backfill progress |
| `trim_status` | `{id, jobId, startTime, endTime, progress, state, …}` | A trim starts, progresses, finishes or fails |
| `admission_holds` | `{offline, diskHeld, rateLimitedUntil, jobs}` | Why queued jobs are waiting changes |
| `connectivity` | `{online}` | Internet connectivity changes |
| `disk_status` | `{free, total, usedPct, warnLevel}` | A disk-space reading |
| `update_available` | Release info | A newer release is found |
| `update_cleared` | `{tagName}` | The pending release was skipped or withdrawn |
| `pong` | — | Answer to `ping` |

## Related

- [Security](security.md) — what each access mode allows and how to expose
  Moombox safely.
- [Network settings](../configuration/network.md) — port, HTTPS, password and
  proxy settings.
