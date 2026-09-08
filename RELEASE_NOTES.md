## Features

- **TUI filter speaks the dashboard's language** (`/` on the Tasks panel): text, `status:`, `channel:` and `platform:` terms, `-` negation and `a|b` alternatives, matched by substring and by video ID exactly like the Web filter bar; `F` cycles the status token, `Esc` clears the whole filter, and the header and empty state echo the query. The parser lives in the new `internal/jobfilter` package and is pinned against its JavaScript twin.
- **TUI statistics overlay** (`R T`): the Web Stats tab's figures, rendered from one derivation source (`internal/stats`, shared with `/api/stats`) and refreshed every minute while open.
- **TUI/Web parity chords and buttons**: `R I` imports a Netscape cookie file through the same verify-and-roll-back path as the Web import; `A W` toggles Watched on Finished jobs (a dim `•` on the row); `A` on the Orphans section deletes every orphan in it; `c` clears the log view; `R Y` shows the yt-dlp plugin status (installed, plugin dir, port mismatch) and installs it; `S` inside the release-notes overlay skips the pending version. The Web dashboard gains a Re-scan Feed History button and a Copy stream URL row in job details.
- **Per-channel overrides in both channel editors**: `num_desc_lookbehind`, `output_directory`, `archive_window_days` and `archive_slots` are editable in the TUI and the Web dialog, and a TUI save keeps every field it does not show instead of dropping it.
- **Settings surfaces tell the truth about restarts**: `go_soft_limit_mb`, `trust_forwarded_proto` and `ffmpeg_path` apply on save in both UIs; `probe_targets`, `sidecar_hard_limit_mb` and `use_sidecar` are marked restart-required and the restart prompt names only what actually needs one; the Network section exposes `connectivity.probe_targets`, validated on save.
- **Issues bucket and logout**: the Web filter's "Issues" bucket is `Error | Cancelled | COOKIES?`; a logout icon appears in the status bar when a password is set; the TUI's cookie prompts say "Auth Required" and "Refresh Cookies from Browser".

## Improvements

- **Cookie verification is faster and cannot lose a working platform**: the YouTube and Twitch auth checks run concurrently in their own 12-second windows (setup and refresh budgets stay under their caps), and the import and refresh paths share one restore routine, so a paste or refresh that regresses a platform rolls that platform back and reports it.
- **Twitch chat parts survive restarts**: a resumed part keeps its recording base instead of the restart time (offsets no longer shift), and a part whose sidecar is gone is adopted from its file rather than overwritten by the first flush.
- **YouTube chat**: a replay run no longer adopts a live run's resume sidecar; legacy chat files whose messages carry no offsets are placed from the file's epoch in the player.
- **Player**: the resume dialog traps Tab within its actions and Space is inert behind it; dismissing an update in the dashboard clears the TUI's update badge.
- **TUI**: a "Terminal too small" screen below 60×20 instead of a garbled layout; help rows wrap so the overlay fits 80 columns; the files dialog sizes from one helper; the chord hint is bounded.
- **Web**: batch resume uses the same gate as the details button and unknown staging no longer blocks it; the TUI's import request percent-encodes titles like the Web UI; the config API validates `client_token_ttl_days`.
- **Connectivity**: probe dial goroutines recover from panics and still report.
- **Housekeeping**: `downloader.pot_provider_url` is retired (it was read nowhere); the three uncalled GET routes are documented; `fetch-node` trusts a stamp beside the blobs instead of `version.txt`.

## Bug Fixes

- **Every archived YouTube Super Chat was tier 1 blue.** The tier table held YouTube's body colors but was looked up with the header color, which never matches; Super Stickers were tier 0 with no color. One table now holds both colors of every tier, stickers resolve from their own fields, every record carries `kind` plus the raw `headerColor`/`bodyColor`, and a color the table does not know is archived as tier 0 with color `gray` and logged once per distinct pair with the raw values. Files written before this release keep their old tiers; re-downloading a replay recovers them.
- TUI channel editor dropped the fields it did not display on save; number fields did not bind their text input.
- A TUI settings save did not re-apply `go_soft_limit_mb`, `trust_forwarded_proto` and `ffmpeg_path`.
- The cookie import prompt took each key twice; the rollback note appeared even when nothing rolled back.
- The DACL-memo cookie tests raced the goroutine's bookkeeping and could hang the suite.

## Internal

- **Toolchain and dependencies**: `go 1.27` with `toolchain go1.27.1` as the floor; embedded Node v24.20.0 (Krypton LTS) for the sidecar; modernc sqlite v1.58.0 and libc 1.75.7, x/crypto v0.56.0, x/net v0.58.0, goja 2026-09-03, regexp2/v2 v2.7.2, goldmark v1.8.6, go-runewidth v0.0.29, chroma/v2 v2.27.0, ultraviolet 2026-09-03, xo/terminfo v1.0.0; every reachable indirect module at its latest, gated by the full suite, both Linux cross-builds and the live cipher and sidecar checks. setup-qemu-action v4; web tests on jsdom 30.
- **CI**: build, gofmt, vet, staticcheck (hard gate, pinned 2026.2.1) and the full test suite run on every push to main and every pull request on ubuntu and windows; ubuntu also cross-builds linux/arm64 and runs the node suite. This release is the first Linux execution of the bumped sqlite runtime.
- **Tests**: engine loop waits read from one injectable delays struct (the engine package runs in seconds instead of a minute); the notifications Wait timeout is injectable; a shared goja harness evaluates `settings.js` for the TUI and route tests; a jsdom harness pins `app.js` rendering; the spec citation test requires the declaring file.
- **Structure**: `internal/cookies` split into 34 files by responsibility; `app.js` split into five controllers (files, log panel, update indicator, filter bar, job details); the player overlay's geometry and scheduler live in their own modules; new packages `internal/jobfilter`, `internal/stats`, `internal/ytdlpplugin`, `internal/webtest`.
- **Docs and hygiene**: the spec deep-dives describe the shipped behaviour; implemented plan and design documents are removed (git history keeps them); the metrics appendix is regenerated; gopls modernizer sweeps; the dead sig-route logging path and the remaining staticcheck findings are gone.
