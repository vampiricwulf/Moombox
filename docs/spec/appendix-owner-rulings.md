# Appendix: Owner Rulings Cited in Code

## Scope

Code comments, tests and the other spec documents cite owner rulings by short ids such as `O-M` or `R3`. The ids on this page were defined in chain design specs that are no longer in the tree: once a chain was merged its design spec was removed, and git history is the archive. Each entry gives the ruling in one or two sentences, so a reader who meets the id in a comment can tell what was decided. Where another spec document describes the behaviour in full, the entry names it.

To read a removed design spec, ask git for it at the last release that held it:

```
git show d9bbbc83:docs/superpowers/specs/2026-09-17-sweep2-fix-chain-design.md
```

`d9bbbc83` is v2.9.8. All eight removed specs are at that commit under `docs/superpowers/specs/`, and their file names are given with each section below.

Several ids are reused between chains (`R1`, `R3`, `Q5`, `Q6`). A citation in code normally carries the date or the chain's name; when it does not, the subject tells which ruling is meant.

Ids of the form `WEB-6`, `CORE-2`, `MON-4`, `TOOL-18` or `T2-18` are not rulings. They name rows of the sweep reports, which were never tracked, and a comment that cites one states its reason itself. Rulings from later chains (`R-D2`, `R-I1`, `PR-M1`, `CI-4` and the like) are explained where the behaviour is documented, in the other spec documents and in CLAUDE.md.

## Improvement chain, 2026-09-04

`2026-09-04-improvement-chain-design.md`. The owner answered thirteen questions, Q1 to Q13. Four are cited:

- **Q4** — the Issues filter bucket is Error, Cancelled and COOKIES?; the Finished bucket is Finished only. Both UIs.
- **Q5** — the TUI shows a `COOKIES?` job as "Auth Required", as the Web UI does.
- **Q6** — logout is a status-bar icon beside the theme toggle, shown only when auth is on and the session is authenticated.
- **Q8** — the `pot_provider_url` setting was removed rather than wired to an external PO-token provider.

## Sweep fix chain, 2026-09-15

`2026-09-15-sweep-fix-chain-design.md`.

- **R1 (2026-09-15)** — the attestation-challenge plumbing that nothing used was deleted: the `challenge` parameters of the player API fetchers and the uncalled `GeneratePlayerPoToken`.
- **R2 (2026-09-15)** — `-movflags faststart` stays on the mux.
- **R3 (2026-09-15)** — the TUI hides Delete (`A D`) for an active job; the chord carries the Web's delete statuses (Finished, Error, Cancelled, COOKIES?).

## Post-chain follow-ups, 2026-09-15

`2026-09-15-post-chain-followups-design.md`.

- **O1** — a Twitch IRC `RECONNECT` no longer ends chat capture: the session reconnects without spending the reconnect budget.
- **O2** — `chat_status` comes from the chat downloader's outcome, not from the message count; a stalled VOD chat reads `incomplete`, not finished.
- **O3** — two web origin fixes. **O3a**: on an external or public install the same-host origin rule compares against the configured public host or the certificate's names, not a header the caller controls. **O3b**: the WebSocket origin check reads `X-Forwarded-Host` only from a trusted proxy and matches the port as the CSRF check does. See security.md.
- **O4** — the HLS live loop finalizes cleanly on a confirmed "ended" from any of its three end-verdict sites.
- **O5** — `/` and `/index.html` serve the cache-busted index copy with `no-cache` and an ETag, as the SPA fallback does.

## Sweep 2 fix chain, 2026-09-17

`2026-09-17-sweep2-fix-chain-design.md`, § 0. The ids run O-A to O-AA; the ones cited in the tree:

- **O-A** — a VOD job waits for chat paging to finish before finalizing, after releasing its download slot; live jobs keep the two-minute cut. See architecture.md.
- **O-B** — on restart, a job found in Muxing muxes what is staged and never re-downloads. A non-empty staged media file is not truncated unless the caller asked to discard it.
- **O-C** — a Twitch broadcast is confirmed ended only on two stream-info samples about 5 s apart; an unknown verdict is re-checked once and otherwise ends in Error with staging kept.
- **O-D** — a cookie import claims the refresh slot; while a refresh pass runs it answers 409 "refresh in progress". There is still no lock over `cookies.txt`. See data-and-storage.md.
- **O-E** — `DownloadWorker.Stop` cancels in-flight background and final muxes; the restarted process re-muxes partial part files.
- **O-F** — a lifecycle slot is taken only when the job should download; the wait phase holds none. See architecture.md.
- **O-G** — the output file is synced immediately before each resume-position save.
- **O-H** — the 30 s quality monitor probes auth-walled streams with one TV-with-cookies call, and runs the full extraction cascade only when that returns nothing selectable. See architecture.md.
- **O-I** — waiting-room polls reuse the first ANDROID_VR result and skip the cookieless clients when TV says the stream is upcoming and playable. See platform-services.md.
- **O-J** — disabling a channel pauses its queued backlog: it resolves to 0 archive slots. See architecture.md.
- **O-K** — on POSIX the 0700 chmod of the cookie file's parent applies only to a dedicated directory, never to a shared output, database or log directory. See data-and-storage.md.
- **O-L** — the cookie import and finish handlers set `Content-Length` before writing their answer. See data-and-storage.md.
- **O-M** — Ctrl+C always quits the TUI: `handleKey` checks it ahead of every overlay.
- **O-N** — the TLS scheme-sniffing accept loop retries temporary accept errors with a capped backoff; a web server that dies late does not take the process down.
- **O-O** — progress ticks travel as the slim `job_progress` WebSocket frame, merged client-side; `job_update` carries everything else; the cadence is unchanged. See user-interfaces.md.
- **O-Q** — two timing constants became test seams (the browser launch-window kill budget, the engine's fast delays); production values are unchanged.
- **O-R** — probe-only WEB-family player calls skip the PLAYER PO token; download calls keep it. See platform-services.md.
- **O-S** — `twitch.tv/membership` is not requested in the IRC capability request. See platform-services.md.
- **O-V** — rejected: Esc on the FFmpeg-not-found overlay keeps quitting, because muxing needs FFmpeg.
- **O-W** — `O C` adds a `clip.exe` fallback on a local Windows console and words the OSC 52 result as "sent", not "copied". See user-interfaces.md.
- **O-X** — the task list pages with PgUp, PgDn, Home and End.
- **O-Y** — an explicit `-config` path is authoritative: no fall-through search. See data-and-storage.md.
- **O-Z** — the citation test in `internal/docs` covers SPEC.md and CLAUDE.md as well as the deep-dive documents.
- **O-AA** — an archive import whose chat metadata carries an invalid video id falls back to a generated `imp_` id, and each extracted path is checked against the output directory first.

## Post-sweep-2 follow-ups, 2026-09-24

`2026-09-24-post-sweep2-followups-design.md`, and `2026-09-24-followup-residuals-design.md` for the four residual items that followed (it defines no ids).

- **R1** — `max_video_resolution` compares the shorter frame dimension, so `2160` means 4K in either orientation. **R1 format**: the key stays an integer, `0` means unbounded, and the Settings picker only maps presets to numbers. See platform-services.md.
- **R2** — among renditions of the chosen size Twitch ranks AV1, then HEVC, then H.264 (later amended by D-Y2). See platform-services.md.
- **R3 (2026-09-24)** — caps whose values were chosen for arm's memory, the reorder budgets among them, apply their low defaults on arm64 only. See data-and-storage.md.
- **D-1** — both UIs list a job's set-aside recordings and kept chat sidecars and offer Recover, which muxes each aside into its own output beside the archive. See user-interfaces.md (`A S`, `POST /api/jobs/{id}/recover-asides`).
- **W R-3** — both folder-open paths quote the directory on Windows, so a path containing `=` opens. See user-interfaces.md.
- **C L17** — the schema-downgrade refusal names the rollback artifacts after the running binary.

## TUI 120 fps and chat cards, 2026-09-25

`2026-09-25-tui-120fps-and-chat-cards-design.md`.

- **F1** — the TUI renders at 120 fps, and the engine's progress gate is the setting `downloader.progress_interval_ms` (16 ms by default). See data-and-storage.md.
- **K1 to K4** — the player's sidebar chat draws YouTube Super Chats as two-part cards (K1), memberships as green cards (K2) and Twitch sub, gift, raid and cheer messages as notices with Twitch's cheer colours (K3). Sidebar only: the video overlay keeps plain scrolling text (K4). See user-interfaces.md.

## Discord webhooks, 2026-09-27

`2026-09-27-discord-webhooks-design.md`. `N1`, `N2a`, `N2b` and `N3` in comments are that chain's arcs (delivery core, producers, config and batching, lifecycle messages), not rulings. The owner's rulings:

- **Q1/Q2** — edit-in-place is opt-in per target, and message ids are stored per job per target so a restart keeps editing the same message.
- **Q3** — found and added events within 5 s per target are batched into one message of up to 10 embeds.
- **Q5 (2026-09-27)** — each target has `mention` and `mention_events`; the default events are `error`, `auth`, `disk_critical`, `update_failed`, `crash_recovered` and `sidecar_down`.
- **Q6 (2026-09-27)** — with `network.public_url` set, a job embed's title links to the dashboard job.

See operations.md § Notifications (Discord Webhooks).
