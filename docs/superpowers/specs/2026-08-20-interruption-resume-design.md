# Broadcast Interruption Resume — Design

**Problem (field case, 2026-08-20, job `BJUz0SzJP_c`):** a live broadcast's
ingestion died mid-stream (streamer disconnect). YouTube kept the watch page
`streamStatus=live` but removed all streaming data (`formats=0`). The
downloader saw only "no new segments, head reached, status still live,"
burned its 10-minute MaxTimeout, force-finalized a byte-complete recording
(`cur == head`, so no incomplete-tail flag — staging destroyed), and muxed.
Eleven seconds later the streamer restarted the same broadcast (same video
ID, retitled "LETS RUN IT BACK"). The monitor re-detected it every ~15s and
silently dropped each detection at the duplicate-job check. The continuation
was never captured.

**Goal:** capture continues exactly where it left off across a broadcast
interruption — same segment timeline, seamless playback — using the
existing restart/part machinery. No change to MaxTimeout's meaning for
genuinely-ended or stuck streams.

## Signals

Two observable "the broadcast may resume" signals, both already flowing:

- **Interruption signature:** player response with `formats == 0` while
  `streamStatus == live`. A genuinely ended stream keeps post-live formats;
  an interrupted one has its streaming data removed. The credential-refresh
  path already fetches player responses every ~20s during a stall — this is
  classification, not new traffic.
- **Chat-open:** the job's ChatDownloader is still receiving live
  continuations. Directional by design (owner decision): chat OPEN ⇒ resume
  is possible; chat CLOSED ⇒ **no information** (streamers can disable chat
  independently). Never used as evidence of ending. "Closed" means a
  definitive `IsComplete`/empty-continuation response — fetch errors and
  network failures do not count. Jobs without chat (disabled, none exists)
  simply lack the signal.

## Tier 1 — stall finalize-and-mux while resume is plausible

At the two MaxTimeout-backstop finalize sites, a new deferral joins the
behind-head guard: **do not finalize while `mayResume()` is true**, where

```
mayResume = NOT confirmed-ended
            AND (interruption signature observed recently OR chat open)
```

- **Confirmed-ended always wins.** A `CheckStreamStatus` ended verdict
  finalizes exactly as today, even with chat still open — chat lingers for
  minutes after every normal stream end, and stalling on it would delay
  every ordinary finalize. The stall exists only for the not-ended case
  (status live/stuck) that today falls through to the MaxTimeout backstop.
- While stalled, the downloader stays alive: staging, currentSeq, and the
  refresh loop are untouched, so the moment ingestion returns the next
  credential refresh gets real formats and the capture continues in place
  at the next sequence — the seamless case, no resume round-trip at all.
- Activity line: `Stream interrupted — waiting for resume… (12m)` (new
  DownloadActivity), so the stall never reads as a hang.
- **Bounds:** the stall ends when (a) the stream resumes, (b) the verdict
  becomes confirmed-ended, (c) chat definitively closes AND the
  interruption signature stops (falls through to Tier 2's finalize), or
  (d) a hard ceiling expires: new `downloader.interruption_timeout`
  (FlexDuration, default 2h, 0 = disabled → Tier 2 immediately). The
  ceiling exists for the abandoned-broadcast edge where neither YouTube's
  status nor chat ever terminates.

Mechanics: `DownloaderOptions.MayResume func() bool` (nil = feature off,
prior behavior byte-identical). The worker builds the closure: the
strategy's refresh path records "last player fetch saw formats=0 + live"
(timestamped atomic on the job context; stale after ~2× refresh cadence),
and the orchestrator exposes the chat handle's open-state
(`ChatDownloader.LiveContinuationOpen()` — new accessor, true from first
successful live poll until a definitive close). The engine only calls the
closure; all classification stays worker-side.

## Tier 2 — preserve resume data when finalizing anyway

When a finalize proceeds and the possibly-resuming evidence was observed
during the terminal stall window (ceiling expiry, or chat-closed
fall-through), the job finalizes **as incomplete**: set `incomplete_tail`,
preserving staging + resume sidecar through the mux exactly as the
existing incomplete-tail machinery does. The recording plays fine; the
badge says it may be missing its tail; the flag clears on a clean re-run
(all existing behavior). A finalize with no possibly-resuming evidence
(plain stuck-status timeout) is unchanged — complete, staging cleaned.

## Tier 3 — auto-resume on re-detection

In `createYouTubeJob`'s duplicate-job path (`monitor_callbacks.go`), replace
the silent drop for exactly this case: disposition is broadcast (live), the
existing job is **Finished with `incomplete_tail` set** and platform
youtube → invoke the worker's **ResumeJob** (the staging-preserving path —
never Retry/Reinitialize, per the retry-vs-resume rule), update the stored
title to the detection's, log at INFO, send a "Stream resumed — continuing
capture" notification. Guards:

- **Never Cancelled** — a human decision is not overridden.
- Error/COOKIES? keep their existing recovery paths.
- Per-job auto-resume cooldown (5 min) so a flapping detection cannot spin
  the resume machinery.
- Finished **without** the flag (staging gone) stays a silent drop — nothing
  to resume into; the operator can still Reinitialize manually.

Downstream, the existing machinery does the rest: restart discovery routes
into the correct part staging, DB-sequence seeding continues at
`last_*_seq + 1` (with the A/V realignment guard), a quality change across
the re-ingest becomes an ordinary part split, and resume-append + re-mux
produce one seamless recording.

## Tier 4 — lossless merge of same-format parts

When a resume lands on the *part* path rather than a same-staging append
(sidecar unusable, or quality bounced and returned), the job finalizes as
multiple part mp4s today. When adjacent parts share identical stream
parameters, merge them losslessly so the output is still one seamless file
— closing the last gap in "continue exactly where it left off." This also
benefits ordinary gap-split jobs, which usually never changed format at
all.

- **Mechanism:** FFmpeg concat demuxer with `-c copy` directly on the part
  mp4s — the same join `concatIntermediates` (muxer_trim.go) already
  performs, minus the trim pipeline's lossy libx264 re-encode that today
  manufactures the uniformity this tier instead verifies. No transcode:
  bit-identical media, I/O-bound speed.
- **Mergeability:** contiguous runs only, decided from the Segment rows'
  `Quality`/`VideoWidth`/`VideoHeight`/`VideoFps` and confirmed by ffprobe
  on the actual files (exact video codec string + audio codec, sample
  rate, channel count — the DB does not record audio parameters). A
  1080p → 720p → 1080p job merges each run, never across the differing
  middle.
- **Placement:** `finalizeMultiSegmentJob`, after parts are muxed: group
  contiguous same-format runs, concat-copy each run to a single file,
  collapse the run's Segment rows into one (summed duration/size, span
  timestamps), and concatenate the parts' chat JSON files (messages are
  timestamped, so ordering is mechanical).
- **Timeline gaps** between merged parts become playback jumps — identical
  to how a mid-part gap already plays.
- **Platform gate:** `finalizeMultiSegmentJob` only calls into Tier 4 when
  `jobCtx.Job.Platform == "youtube"` — Twitch is gated out entirely at the
  call site, not merely left unmergeable by a chat-schema mismatch. Twitch
  gap-split parts carry deliberate gapless-part semantics that a merge
  would destroy when chat is off; when every part's chat happens to be
  empty, a schema-blind merge would "succeed" and replace the per-part
  `twitch.TwitchChatData` files with a YouTube-shaped `chat.ChatData` husk;
  and when chat is present, a schema-mismatch abort (see below) would still
  cost a full throwaway video concat first. The gate avoids all three by
  never attempting Tier 4 for Twitch, full stop.
- **Failure-safe:** any probe disagreement, concat error, or DB-replace
  error leaves the parts exactly as today; the merge is an opportunistic
  improvement, never a gate on finalize. A chat-merge failure aborts that
  RUN specifically (its own identity — video included, not just chat: the
  run's parts, rows, and per-part chat files are all left exactly as they
  were), while every other run in the same finalize still merges
  independently. Before the platform gate existed, this was the only path
  by which a Twitch gap-split run avoided merging: `mergeChatFiles`
  unmarshals `chat.ChatData` (`message` a `[]MessagePart`) while Twitch's
  chat is `twitch.TwitchChatData` (`message` a string), so the unmarshal
  always errored. That schema mismatch is now moot for Twitch — the
  platform gate means Tier 4 is never reached — but the abort-the-run
  behavior itself remains load-bearing for any future non-YouTube chat
  shape that reaches this path some other way.

## Rider — steady-state dead-auth recovery

Found during this investigation: `OnRecoveryNeeded` fires only on a
*witnessed* authenticated→not transition. Auth already dead at process
start (today: `youtube=false` on every half-hourly check, all day) never
fires recovery or notification. Fix: the first completed check after
startup that finds a platform unauthenticated (with `err == nil`, so
network failures don't count) triggers the same recovery path once; the
existing 30-minute notification cooldown applies unchanged.

## Non-goals

- No change to MaxTimeout semantics when no resume evidence exists.
- No chat-closed ⇒ ended inference, ever.
- No auto-resurrection of Cancelled jobs.
- Twitch: out of scope (different interruption model; monitor recovery
  already handles stream flap).
- Tier 4 is YouTube-only, gated entirely: `finalizeMultiSegmentJob` only
  calls `mergeSameFormatParts` when `jobCtx.Job.Platform == "youtube"`.
  This is a platform gate, not merely a consequence of the chat-schema
  mismatch (`mergeChatFiles` only understands `chat.ChatData`, YouTube's
  shape, while Twitch's per-part chat is `twitch.TwitchChatData`) — Twitch
  gap-split parts carry deliberate gapless-part semantics a merge would
  destroy even with chat off, and an all-empty-chat run would otherwise
  "succeed" through the schema check and clobber Twitch's chat shape with a
  YouTube-shaped husk. Teaching the merge path a second chat schema (or a
  shared intermediate shape) and lifting the platform gate so Twitch runs
  can eventually merge losslessly, chat included, is explicit follow-up
  work, not part of this design.

## Testing

- Engine: MayResume deferral at both backstop sites (defer while true,
  finalize when false, ceiling expiry); nil-callback byte-compat.
- Chat: `LiveContinuationOpen` truth table — open on live polls, closed on
  `IsComplete`/empty continuation, unchanged on fetch errors; never opens
  for replay/disabled chat.
- Worker: interruption-signature recording on the refresh path (set, stale
  expiry); Tier 2 flag decision matrix (evidence × verdict).
- Monitor callbacks: duplicate-path matrix — Finished+flag+live →
  ResumeJob once per cooldown; Cancelled / flagless / non-broadcast →
  dropped as today.
- Merge: run-grouping matrix (identical run merges; format change splits
  runs; single part no-ops), ffprobe-disagreement/concat/db-replace
  failures leave parts untouched, Segment-row collapse arithmetic, chat
  concatenation ordering. A chat-merge failure leaves its own run in full
  identity (media untouched too) while an unrelated run in the same batch
  still merges. FFmpeg-dependent cases behind the same live-tool gating the
  muxer tests already use.
- Cookies: startup-dead-auth fires recovery once; network-error first
  check does not.
