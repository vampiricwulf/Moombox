**Before you update:** on its first start this release clears the "incomplete" flag from finished YouTube live recordings that stopped exactly two segments short of the head YouTube reported, and removes their staging folders (see the first bug fix). It logs one line per recording. A recording whose archive file is not where the job says is left as it is.

## Features

- **The TUI works in a small terminal.** Below 60x20 the task list switches to a compact layout, down to 30x6, and the day-to-day overlays (action menu, Add Video, Trim, the imports, Job Log, Statistics, Settings, Help, Release Notes and more) draw without borders at that size. The setup wizard and the FFmpeg check still need 60x20 and say so.
- **Muxing shows its progress.** A Muxing job's line follows the FFmpeg copy ("Muxing NN%") in both UIs, including the last part of a split recording.
- **Twitch moderation in chat replays.** Live Twitch chat now stores timeouts, bans, deleted messages and full chat clears. The replay marks the affected messages the way YouTube deletions are marked; a full clear shows one sidebar line.
- **Global BTTV, FFZ and 7TV emotes** are archived with each Twitch chat, so they render in the replay. A channel emote wins over a global one of the same name.
- **A failing channel is flagged.** A channel that none of its monitors has reached for three checks in a row is flagged in the Settings channel list of both UIs until a check succeeds.
- **Both UIs start before the first cookie check.** The dashboard and the TUI come up at once and show "checking cookies" until the check answers.
- **Job details name a kept recording** when a finished job still holds a recording its archive was not made from.
- **TUI:** the mouse wheel scrolls every overlay that has something to scroll, and Shift+Up / Shift+Down reorder channels in Settings.
- **The Tasks tab says when live updates cannot connect**, instead of silently showing stale rows.

## Improvements

- **A faster dashboard.** A delete, a status change or a re-sort redraws one card instead of the whole Tasks list. A hidden tab stops drawing and catches up when shown. The Archived tab draws its newest 200 cards with Show more, survives a failed fetch, and takes Arrow and Enter like Tasks. Rows are real tab stops.
- **A lighter TUI.** An unchanged frame is not rebuilt, and the fast progress tick idles while an overlay covers the panels.
- **A steadier speed readout on slow VODs**: a whole-file chunk reports its bytes as they arrive.
- **Stale HTTP/2 connections are detected** by a health check on the shared transports, and connects are bounded. The self-update keeps the transport it had in v2.9.8.
- **The BotGuard sidecar recovers more cleanly.** A PO-token request waits up to 60 seconds for a restarting sidecar when its caller has the time. A sidecar that stops answering is probed once and restarted. Fetched bodies are read under a size cap.
- **Twitch chat downloads.** Adding emotes to a finished chat no longer loads the whole file into memory. VOD chat progress is logged about every 30 seconds, and Twitch chat lines reach the job's own log.
- **Startup.** The dashboard bind is judged by a real signal rather than a fixed 500 ms wait. A `config.toml` key that nothing reads is named in a warning. A `-config` path that cannot be read refuses to start instead of booting as a fresh install. After a restart, live rows are offered to the queue before downloading and upcoming ones.
- **Quieter logs.** A failing feed warns once per streak and says when it recovers. The error for a stream too long to archive from its start says so plainly.

## Bug Fixes

- **Finished live recordings were flagged incomplete and kept their staging.** Since early November YouTube reports a head two segments past the last one it serves once a broadcast is over. A live capture that stops exactly there is now complete when YouTube confirmed the stream over, nothing interrupted the capture, and audio and video stopped at the same segment. Recordings already flagged this way are cleared once, at the first start. VOD downloads are judged as before.
- A Cancel issued while a restart was re-muxing a job is no longer overwritten.
- Job Failed notifications name the mux stage for a split recording's failures.
- A fractional `incomplete_staging_expiry_days` is honoured instead of being rounded down.
- A long cookie path no longer cuts the cause off the cookie load error.
- A Twitch global emote provider being down no longer leaves later parts of the same chat without global emotes.

## Internal

- `local-gate.sh`, `new-worktree.sh` and `embed-standins.sh` for local verification; TUI tests can no longer open a real browser.
- `worker.go` split into verbs, failure handling and staging claims; boot code extracted into named functions.
- jsdom 31 in the sidecar and the dashboard tests; `golang.org/x/sync` 0.25.0; `modernc.org/sqlite` 1.62.0.
