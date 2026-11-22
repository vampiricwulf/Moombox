**After you update (Windows).** Versions up to 2.9.9 restricted the folder holding `cookies.txt` and `config.toml` to the account Moombox ran as. With the default `./cookies.txt` that is the install folder, so other accounts, SYSTEM and Administrators lost access to the recordings, the database and the log. Moombox does not change the folder back. When it finds such a folder it logs *this folder does not inherit permissions from its parent* at startup, with the command that restores it: run `icacls "<your Moombox folder>" /inheritance:e` as the account Moombox runs as, then save any setting once and refresh or re-import your cookies, so both files are restricted again. Linux and Docker installs need nothing. There is no database or config migration in this release.

**After you update (cookies).** A `cookies.txt` written by an earlier cookie-file import still holds every youtube.com row of the file you pasted, including rows older versions never read. This version sends each unexpired one from its first request, until YouTube or the next browser pass replaces it. If YouTube sign-in misbehaves after the update, import a fresh cookie file.

## Features

- **The YouTube session is kept alive.** Moombox now keeps the session cookies YouTube renews on its own replies (player calls, chat polls, watch pages), keeps and sends every cookie `www.youtube.com` is sent, as yt-dlp does, and, with `cookies.auto_enabled` on and a managed browser profile, opens the sites in the headless browser at least once a day when nothing else has renewed the cookies, even with no job active. The cookie status in both UIs shows what renewed the session and when, and spells out a session that is flickering between signed in and signed out, with its next attempt.
- **Session cookies are no longer sent to the googlevideo media hosts**, unless a host refuses a request without them and serves it with them; that download then sends them, and logs a warning.
- **A job parked on its cookies gets three automatic retries** instead of an endless refresh loop, and both UIs show how many are used. Any refresh, import or setup that ends with the platform verified resumes the jobs parked on it that still have a retry left; one that has used all three stays parked until you resume it, or until a day after its last retry.
- **Windows is asked not to idle-sleep** while a capture or a mux is running. On by default; set `downloader.keep_awake = false` to turn it off. The screen may still turn off.
- **A user documentation site** is published on GitHub Pages, with a heading for every `config.toml` key.
- **Reinitialize keeps the archive a job already has.** The existing single-file archive is renamed aside as `<stem>.restart-<timestamp>.mp4`, with its chat, and listed under Set-aside Archives in both UIs.
- **A YouTube live capture stages its thumbnail as it starts**, for a finalize that can no longer fetch it.
- **The Job Failed notification says when an automatic mux or retry is pending.**
- **A mux waiting for a download slot says so on its row.**
- **Dashboard:** open tabs are told when cookie status or sidecar health changes; the details dialog re-reads its Job Log every second; error toasts stay until closed.
- **TUI:** Ctrl+A selects every task the list shows.
- **The chat replay draws a Twitch `/me` message as an action.**

## Improvements

- **The BotGuard sidecar uses far less memory.** Prepared players are stored flat, leave RAM when idle and reload from a disk store. A replaced BotGuard client is shut down once no mint uses it. The soft memory limit acts once per episode and recycles an idle sidecar that stays over it, with callers waiting the recycle out.
- **History is never pruned.**
- **The low-disk alerts and the backlog hold also watch a staging directory on another volume.**
- **The connectivity probe also tries YouTube and Twitch** when no probe target answers.
- **Gap-fill is more patient.** It retries a failed VOD segment with a pause, skips looks while offline, waits out a full volume, and a plan past its 24 h window gets one last look before the recording is kept as it is.
- **Turning on the Twitch VOD backlog lists the channel at once.**
- **A failed automatic update check is retried the same day.**
- **A Windows console close, logoff or system shutdown** cancels a running mux at once and saves inside the 5 seconds Windows allows.
- **A waiting monitor-detected job follows the output directory setting**; a started row, and a removed channel's row, keep theirs.
- **Web server hardening.** A stalled request body is cut off after 60 seconds. Revoking a client token ends its session and its open socket, and a logout closes the sockets opened on its session. `/minter_cache` needs a session from a non-local client. Another site's page can no longer fire the ffprobe or orphan-scan GETs. Unknown API paths answer the JSON error body.
- **YouTube:** `web_embedded` takes the web family's GVS token; Innertube is asked for condensed JSON; the randomized Chrome major window moved to 151–157; YouTube links are read by one parser in every add path and found inside other text.
- **In-memory job logs are kept for the newest 200 finished jobs only.**
- **A channel platform in `config.toml` is trimmed and case-folded**; an unknown one disables the channel with a message saying why.

## Bug Fixes

- **Moombox no longer changes the permissions of a folder that already existed.** Only the cookie and config files themselves are restricted. On Linux a folder Moombox creates for them is created for its owner (0700); on Windows a new folder inherits its parent's permissions, and the folder lockdown that stripped an install folder's inherited permissions is gone. See the note at the top for a folder an earlier version already restricted.
- A feed 404 is worded as YouTube being busy, not as a dead channel.
- A stalled console or pipe no longer freezes the goroutines that log, on stdout or on the logger's own stderr lines.
- A Mux of a Finished live Twitch job, and a boot re-mux of it, keep the pending gap-fill.
- A finalize never takes or overwrites a file it did not write. One that no job names is set aside; one that another job names is left alone and the finalize stops with an error naming that job, staging kept: rename one of the two, then Mux.
- Engine: an HLS resume state is saved during a long playlist pass; catch-up no longer re-runs at a segment it could not fetch or records a gap when it hands a segment back; an empty 2xx body in the live HLS loop is a failed fetch; the received part of a broken whole-file chunk is kept; a fetch that gets no HTTP answer while online is no longer retried for ever in the live DASH loop: it is handled like a 5xx, with the stream-status checks, a rebuild and the `maximum_timeout` finalize.
- Cipher: a cancelled caller no longer poisons or blocks others waiting on the same player, and a player download that breaks mid-body is retried.
- Twitch: the prefixed `RECONNECT` line is taken; a null VOD comments answer is not read as the end of chat; a failed emote provider is not cached as "no emotes" for a day; a chatter with no display name is named by their login.
- Cookies: a re-check after a cookie write waits for a pass in flight; a verified import or recovery clears a stale "Last cookie error"; a browser pass no longer undoes a renewal YouTube sent during it.
- Dashboard: a full WebSocket queue drops only a progress tick that a newer tick of the same job replaces; the `#job=` deep link survives a login; a rendition picked for another video is never sent; the history toasts count the rows actually removed; the Logs tab opens on the newest line; a failed stats query shows its error.
- TUI: the action menu judges the job again on the confirming Enter; Resume and Reinitialize run off the update loop; a refused Add Video keeps the dialog and its input; events queued before a catch-up snapshot are no longer applied over it; warnings are yellow and a recovered panic red instead of success green; the restart prompts warn about active downloads.
- Deleting orphaned history leaves the row of a video that has a job.
- Staging a job still holds is no longer treated as an orphan.
- A Resume or Reinitialize accepted while the job's previous run is still ending now waits for it, however long, instead of being dropped after 60 seconds.
- A mux restarted after a crash no longer puts a stale "Waiting for a download slot to mux" line back on a failed row.

## Internal

- 147 tests that could not fail, restated a literal or duplicated another were removed.
- CI: the Windows FFmpeg install is retried; the vulnerability scan covers the Windows build, the bundled ejs parsers and the Node pin; the Docker job starts the amd64 image once before the multi-arch build; release and Docker jobs have time limits.
- Go toolchain floor 1.27.4; embedded Node v24.22.3; jsdom 31.0.1; `golang.org/x/crypto` 0.59.0; lipgloss 2.1.0; `golang.org/x/sys` and `x/text` bumps.
- The one-release Twitch offline text fallbacks were deleted.
