# Downloader Activity Indicator — Surface "verifying / waiting", not a frozen progress line

**Date:** 2026-06-26
**Status:** Approved design (pre-implementation)
**Owner decisions:** reuse the existing `progress` line (no new `JobStatus`, no new DB column); cover all four wait windows; no per-second refresh ticker in the minimal version.

## 1. Problem

When a live download stops pulling segments, the dashboard's progress line freezes on
the last segment counter (e.g. `(A: 1234 V: 1234)`) with a stale non-zero speed, and
nothing tells the user the downloader is still working. A user reported this as the
download being "hung" / "frozen" — when in fact the engine was running its normal
**end-of-stream verification**: it keeps probing the next segment and re-checks whether
YouTube still reports the stream "live", and only finalizes once the stream is confirmed
ended.

The freeze happens because `ProgressTracker` only updates `progress`/`speed`/`eta` when a
**segment event fires** (`AttachVideoDownloader`/`AttachAudioDownloader` set
`dl.OnProgress`, `internal/worker/progress.go:63,117`). During a wait window no segments
arrive, so `maybeUpdate` never runs and the last values sit on screen for the whole
window — which can be seconds (clean 403 stream-end) up to ~10 min (engine
`NoSegmentTimeout`) or ~30 min (orchestrator-level verification,
`streamEndVerifyInterval` × `maxConsecutiveLiveChecks` = 5 min × 6).

Partial precedent already exists: when YouTube reports the stream *still live* but
segments stopped, the orchestrator writes `progress: "Waiting for stream to end..."`
(`internal/worker/orchestrator_youtube.go:360`). But it is set **only** in that one
branch and is static, and it does **not** cover the engine-level retry window inside the
downloader — which is the window the user actually hit.

The same frozen-silence also occurs in three other engine wait windows that emit no
callbacks today:
- **Reconnecting / offline** — `waitForConnectivity` / `!IsOnline()` branches in
  `handleGoneError` and `handleHTTPError` (`downloader_dash.go:322,419,435,455`).
- **Rate-limited (429)** — `handleRateLimitError` exponential backoff up to
  `RetryDelayCap` (~60 s) (`downloader_dash.go:355`).
- **Searching for first segment** — the pre-first-byte 403 hunt in `handleGoneError`
  (`downloader_dash.go:342`), which `progress.go:66` notes "can hold this window open for
  minutes".

## 2. Goals & non-goals

**Goals**
- During any downloader wait window, the progress line shows a clear, reason-specific
  message instead of a frozen counter, so the job reads as *working*, not *hung*.
- Cover all four windows: end-of-stream verification, reconnecting/offline, rate-limited,
  searching-for-first-segment.
- Reuse the existing `progress` field; no new `JobStatus` and no new DB column. Both
  Web UI and TUI already render `progress`, so they need no per-UI change.
- Clear `speed`/`eta` while waiting so no stale numbers linger.
- The message auto-reverts to the normal segment counter the instant real segments
  resume (a flap), with no explicit teardown.
- Pure Go, resource-efficient, no new goroutines in the minimal version.

**Non-goals (deliberately deferred)**
- A guaranteed per-second elapsed tick. The elapsed updates whenever the engine reaches a
  retry/check point — frequent on most paths, but up to ~60 s apart at the deepest 429
  backoff. The *message itself* communicates "working" the whole time. A 1 s refresh
  ticker is a sound future add but is extra goroutine/lifecycle machinery, left out here.
- A distinct `JobStatus` (`Verifying`) — rejected for blast radius (lifecycle, badges,
  filters, `isTerminalStatus`, transitions, web/TUI parity) and because the job is
  genuinely still `Downloading` and may resume on a flap.
- A new persisted column / structured status object — the human-readable `progress`
  string carries it.

## 3. Design

### 3.1 Activity model (engine)

A small typed value in `internal/engine` describing what the downloader is *waiting on*:

```go
type DownloadActivity int

const (
    ActivityNone DownloadActivity = iota // actively downloading — normal counter shows
    ActivityVerifyingEnd                 // segments stopped; confirming the stream ended
    ActivityReconnecting                 // connectivity lost; waiting for the network
    ActivityRateLimited                  // 429 backoff
    ActivityFindingFirstSegment          // pre-first-byte hunt for the first valid segment
)
```

The engine stays UI-agnostic: it emits the *reason key*, not the user-facing string.
Wording lives in the worker layer (§3.3).

### 3.2 Callback + emit points (engine)

Add `OnActivity func(a DownloadActivity)` to `SegmentDownloader` (a field like the other
callbacks) and a guarded `emitActivity(a)` helper. The callback lives on the downloader,
so it serves both the DASH loop (YouTube live / manifestless) and the HLS loop (Twitch).

Emit points in the DASH loop (`downloader_dash.go`):

| Site | Condition | Activity |
|---|---|---|
| `handleGoneError` | `!hasStartedDownloading`, hunting (`<= goneRetryBeforeFirstSegment`) | `FindingFirstSegment` |
| `handleGoneError` | `hasStartedDownloading` AND sustained gones past `goneRetryDuringDownload` (the status-check path) | `VerifyingEnd` |
| `handleGoneError` / `handleHTTPError` | `IsOnline()==false` / `waitForConnectivity` | `Reconnecting` |
| `handleRateLimitError` | 429 backoff sleep | `RateLimited` |
| `handleHTTPError` | at/past live edge, backoff + status checks, `NoSegmentTimeout` wait | `VerifyingEnd` |
| `runDashLoop` | successful segment write | `None` (alongside the existing `OnProgress`) |

The HLS loop (`runHlsLoop`, used by Twitch live) receives the same activities at its
structurally-identical wait points; exact line sites enumerated during implementation by
reading `internal/engine/downloader_hls.go`. The direct-VOD path
(`runDirectDownload`) is out of scope — a VOD is a bounded fetch, not a live wait.

### 3.3 Rendering + lifecycle (ProgressTracker)

`ProgressTracker` gains two fields under its existing mutex: `activity DownloadActivity`
and `activityStart time.Time`. `AttachVideoDownloader`/`AttachAudioDownloader` set
`dl.OnActivity = func(a) { pt.setActivity(a) }`.

`setActivity(a)`:
- `a == ActivityNone` → clear `activity` (so the next `OnProgress` resumes the normal
  counter + live speed/eta). No write needed; the segment path will overwrite.
- `a != current activity` → record `activity = a`, `activityStart = now`.
- Write `progress = format(a, now - activityStart)`, and set `speed = ""`, `eta = ""`.
  DB writes throttled to ~1 s (reuse the existing `progressPersistInterval` cadence) so a
  burst of retries doesn't spam `UpdateJobFields`.

`OnProgress` (existing) additionally clears `activity` to `ActivityNone` before the normal
`maybeUpdate`, so a real segment immediately restores the counter and live speed/eta.

`format()` mapping:

| Activity | Message |
|---|---|
| `VerifyingEnd` | `Verifying stream ended… (1m 20s)` |
| `Reconnecting` | `Connection lost — reconnecting… (30s)` |
| `RateLimited` | `Rate-limited — backing off… (15s)` |
| `FindingFirstSegment` | `Waiting for first segment… (40s)` |

Elapsed formatted with the existing duration helper style (`Xm Ys` / `Ys`).

### 3.4 Orchestrator unification

`runLiveStreamDownload` (`orchestrator_youtube.go`) cancels the engine downloaders before
its own verification sleeps, so no engine activity fires during the orchestrator's
5-minute checks. The orchestrator therefore writes the same `VerifyingEnd` message itself:
replace the lone `"Waiting for stream to end..."` write (line 360) with the unified
wording, and set it on the other verify-sleep branches (the `err != nil` retry at line
337 and the refresh-retry at line 383) which currently leave the stale value. Also blank
`speed`/`eta` there. The Twitch orchestrator verify path
(`orchestrator_twitch.go`) gets the same treatment for parity.

### 3.5 Two-downloader interaction

Video and audio downloaders share one `ProgressTracker`. At true stream-end they enter
the same wait together, so the activity is consistent. In the rare transient case where
one is downloading while the other waits, last-writer-wins on the `progress` field may
briefly flip between the counter and the activity message. This is cosmetic and
self-corrects; not worth added coordination in the minimal version.

## 4. Testing (TDD)

- **ProgressTracker rendering** (`progress_test.go`): for each activity, `setActivity`
  writes the expected message and blanks `speed`/`eta`; a following `OnProgress` event
  reverts `progress` to the segment counter and restores speed. Elapsed text format.
- **Engine emit points** (`downloader_dash` test): drive the gone/HTTP/rate-limit/offline
  handlers and assert `OnActivity` fires the correct `DownloadActivity` (table-driven,
  capturing the callback).
- Existing engine/worker suites must stay green (no behavior change to the download
  loops themselves — only added callback emissions).

## 5. Risks & caveats

- **Tick smoothness** varies by path (see Non-goals). Accepted.
- **DB write rate**: throttled to ~1 s; activities fire at most a few times/second on the
  fastest path, so no material increase.
- **HLS/Twitch emit points** require reading `downloader_hls.go` during implementation;
  if a wait window has no clean seam, it falls back to today's behavior for that one path
  (documented, not silently dropped).
- No `JobStatus` change ⇒ filters, badges, notifications, and resume logic are untouched.
