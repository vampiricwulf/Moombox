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
- Cookies: startup-dead-auth fires recovery once; network-error first
  check does not.
