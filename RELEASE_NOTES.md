## Features

- **Niconico-style chat overlay rebuilt** (`web/public/modules/player.js`, `nico-lanes.js`): a media-time two-edge lane allocator, so wide messages never overtake narrow ones and lanes freeze while the video is paused; messages enter from the right edge one second before their timestamp like niconico and slide in at any tick rate; a message that finds no lane waits up to two seconds instead of being dropped, and drops are shown as a transient "+N not shown" pill instead of vanishing silently; playback rate and background tabs are honoured; rows and font follow the player's size and the overlay covers the video's picture, not the letterbox bars; the overlay defaults off under `prefers-reduced-motion`.
- **Chat before and after the video** (`chat-timeline.js`): the sidebar shows a "Waiting room" divider and count for messages sent before the stream started and a "Recording ended" divider for messages after it; post-end chat is readable and reachable through Sync instead of stuck as dim "future" rows; the overlay never animates the backlog or the tail.
- **Per-part Twitch chat in the player** (`GET /api/jobs/{id}/segments/{index}/chat`): multi-part Twitch recordings now replay every part's chat, merged onto the player timeline.
- **Third-party Twitch emotes render** in the player: the Content-Security-Policy admits the BTTV, 7TV and FrankerFaceZ image CDNs.
- **jsdom test harness for the web player** (`web/tests/`, `npm ci` inside `web/tests` to run it; the suite skips cleanly without it): 22 DOM tests cover the selection race, the overlay engine, the sidebar regions, keyboard gating and the resume/offset flows.

## Improvements

- **Chat timing is correct for every job type**: chat offsets are signed on both platforms (pre-stream messages keep negative times instead of piling onto 0:00), YouTube replay recovers pre-stream times from the timestamp text, a YouTube chat file keeps one epoch across restarts and adoption, VOD classification refreshes the job's start time to the actual start, and the player applies a platform-aware bias (Twitch offsets are already video-relative; multi-part YouTube no longer replays early by the detection lag).
- Media and chat routes use a revalidating cache policy instead of `immutable`, so a retried, resumed or merged file is never served stale; a conditional chat request answers 304 without reading the file; a 206 range response is never gzip-encoded.
- Watch-state writes for unknown job ids answer 404 instead of a silent 204.
- Player UI: the segment indicator renders below the video as keyboard-reachable buttons; the job list refreshes while a video is loaded; shortcuts work immediately after choosing a video and no longer collide with the tab strip or focused controls; the resume overlay ignores Escape meant for dialogs and restores focus; Space does nothing behind the resume scrim; the chat-offset reset button tracks the loaded offset; inline playback on iOS; touch-safe sidebar scroll lock; accessible labels.
- Cookie import verifies each platform after a paste and rolls back a platform whose working rows a dead paste would have replaced, matching the refresh paths; the result says per platform what happened.
- The updater names a GitHub HTML error page served with HTTP 200 instead of reporting it as a signature failure.
- `config.example.toml` documents `cookies.acquisition`.

## Bug Fixes

- Overlay: a message sent at or before 0:00 never appeared (the first-spawn seed was unreachable); lane clocks ran on wall time so messages overlapped after a pause; a hidden tab accumulated messages and released them in a burst; every forward seek, tab switch, job switch or overlay toggle counted the gap as drops.
- Sidebar: the "Waiting room" divider was illegible on a not-yet-reached row; a segmented job with unknown part durations could show a false "Recording ended" divider mid-video.
- Player: a slow response for a previously selected job could overwrite the current selection's video; ArrowRight seeked to 0 when segment durations were unknown; unhandled `play()` rejections logged console noise.
- Chat: a resumed chat run could compute offsets against a newer start time than the file's header; the segments list was cached immutably.

## Internal

- Dependencies: chi 5.3.2, go-runewidth 0.0.28, modernc sqlite 1.57.0, bubbles 2.2.1, bubbletea 2.0.9, esbuild 0.28.2 (sidecar build), docker actions setup-buildx v4, login v4, metadata v6, build-push v7.
- Docs: player, chat file, cookie import and route documentation updated to the shipped behaviour; stale PATCH-vs-PUT route comments corrected.
