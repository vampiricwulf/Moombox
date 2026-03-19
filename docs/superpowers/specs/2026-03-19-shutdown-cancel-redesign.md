# Shutdown vs Cancel Redesign

## Problem

Shutdown and user-cancel share the same code path, causing bugs (e.g., the chat resume race condition where context cancellation deletes the resume file before `Stop()` is called). The current single "Retry" action is too coarse — it always starts fresh, discarding hours of downloaded segments when the user may just want to continue.

## Core Semantics

### Shutdown (process restart/exit)

Not a cancellation. Active jobs preserve their current status (`Downloading`, `Live`, `Upcoming`) and auto-resume on next startup. No new status introduced. The existing behavior is correct — shutdown-interrupted jobs re-enter the processing pipeline via `ShouldProcess()` and resume from saved state (DB seq numbers, chat resume files).

**Muxing jobs on shutdown:** `ShouldProcess()` only accepts `Upcoming`, `Live`, and `Downloading` — a job interrupted during muxing is not re-enqueued on startup, creating a permanent dead state. Fix: on startup, scan for jobs in `Muxing` status and reset them to `Downloading`. The worker then re-enters the pipeline, re-probes, discovers the download is complete (segments already exist), and proceeds to mux again. This is safe because muxing is idempotent — partial mux output is overwritten.

### User Cancel

A deliberate user action. Sets status to `Cancelled` (terminal). The job stops and staging files are preserved. The user gets granular follow-up actions instead of a single "Retry":

| Action | Chord | Description |
|---|---|---|
| **Resume** | `A R` | Continue downloading from where we left off |
| **Reinitialize** | `A I` | Fresh start — wipes staging and all progress |
| **Mux** | `A M` | Force-mux whatever files exist in staging |
| **Delete** | `A D` | Remove job and associated files |

The current `Retry` action (`A R`) is replaced. `Retry` no longer exists as a named concept.

### Action Availability by Platform and Status

| Action | YouTube (Cancelled) | Twitch (Cancelled) | Error | COOKIES? |
|---|---|---|---|---|
| Resume | Yes (if staging exists) | No | Yes (YouTube, if staging exists) | Yes (YouTube, if staging exists) |
| Reinitialize | Yes | Yes | Yes | Yes |
| Mux | Yes (if segments exist) | Yes (if segments exist) | Yes (if segments exist) | No |
| Delete | Yes | Yes | Yes | Yes |

Resume is YouTube-only because Twitch HLS streams cannot be resumed after interruption. Resume requires staging files to exist — without them, only Reinitialize is available. For Error and COOKIES? jobs with partial downloads, Resume continues from saved state rather than discarding progress. This preserves the `A R` muscle memory from the old Retry chord while giving better semantics.

## Resume Flow (YouTube)

When the user triggers Resume on a cancelled, errored, or COOKIES? YouTube job:

1. Set status to `Downloading`. This is a pre-enqueue signal — the stream processor still re-probes before the orchestrator runs, same as the existing shutdown-resume path.
2. Clear the `error` field. Preserve `progress` string, `percent`, and `download_started_at` so the user sees where it picks up and the download timer reflects the original start.
3. Re-enqueue via `EnqueueJob()`.
4. Stream processor re-probes the stream with a full `GetVideoInfo` call. This fetches fresh signed URLs and checks current stream state (live, VOD, post-live, ended).
5. Orchestrator starts normally. `ExecuteWithChat` (YouTube orchestrator) must skip overwriting `download_started_at` when it is already set (currently at `orchestrator.go:86-90` it unconditionally sets it — add a guard). The Twitch orchestrator does not need this guard since Twitch has no Resume. The DASH/HLS/VOD downloaders detect existing segment files in staging and resume from the last sequence number (already works via DB-saved seq state and downloader resume files).
6. Chat resumes via the existing resume file mechanism (continuation token + dedup IDs).

If the stream ended while the job was cancelled, the re-probe detects it as VOD/post-live, and the download strategy adapts accordingly.

### Difference from Reinitialize

Reinitialize resets status to `Upcoming`, clears all progress/percent/error/seq counters, deletes staging, and starts the processing pipeline from scratch. Resume preserves staging files, progress, and sequence numbers — it re-enters the processing pipeline which naturally resumes from saved state.

## Reinitialize Flow

When the user triggers Reinitialize:

1. Set status to `Upcoming`.
2. Clear all non-input fields to ensure a truly fresh start:
   - Progress: `error`, `progress`, `percent`, `speed`, `eta`
   - Seq counters: `last_video_seq`, `last_audio_seq`, `total_video_seq`, `total_audio_seq`
   - Chat: `chat_status`, `total_chat_messages`
   - Timestamps: `download_started_at`, `stream_end_time`
   - Output: `output_file`, `filename`, `file_size`, `chat_file`, `chat_filename`, `description_file`, `thumbnail_file`
   - Media metadata: `video_width`, `video_height`, `video_fps`, `length_seconds`
   - Selection: `selected_video_itag`, `selected_audio_itag` (re-selected during download)
3. Delete the staging directory and its contents (fresh start).
4. Re-enqueue via `EnqueueJob()`.

This is an enhanced version of the current Retry action. Current Retry only clears `status`, `error`, `progress`, and `percent`. Reinitialize additionally clears seq counters, chat fields, `download_started_at`, and deletes the staging directory — ensuring a truly fresh start with no leftover state.

Available from: `Cancelled`, `Error`, `COOKIES?`.

### Twitch Reinitialize

Re-probes the channel. If the stream is still live, starts a fresh download (new staging directory, new files). If the channel is offline, the job enters Error state with a message indicating the stream is no longer available.

## Mux Flow

When the user triggers Mux on a cancelled job:

### Pre-check (determines button/chord availability)

Scan the job's staging directory for video/audio files matching downloader output patterns:
- DASH: `video_stream`, `audio_stream`
- HLS: `video.ts`
- VOD: `video.mp4`, `audio.m4a`

If no segment files are found (staging cleaned up or never created), the Mux action is not available (button hidden, chord disabled with reason).

### Constructing a DownloadResult from staging

The existing `muxAndFinalize()` requires a `*DownloadResult` with `VideoPath`, `AudioPath`, and format metadata. For the Mux flow, there is no download pipeline — only files on disk. A new `muxFromStaging()` wrapper is needed:

1. Scan the staging directory for video/audio files using the known filename patterns above.
2. Construct a minimal `DownloadResult` with the discovered file paths. Leave format metadata (codec, resolution) empty — `muxAndFinalize` already runs `ffprobe` on the output file after muxing to populate metadata fields, so pre-populating is unnecessary.
3. Pass this synthetic `DownloadResult` to `muxAndFinalize()`.

This wrapper lives in the worker package alongside the existing orchestrator code.

### Worker pipeline integration

The Mux action does not go through the normal `processJob` → `streamProc.Process` → `Execute` pipeline. Instead:

1. The API/TUI handler calls a new `MuxJob(jobID)` method on `DownloadWorker`.
2. `MuxJob` validates the job status and staging files.
3. Sets status to `Muxing`.
4. Spawns a goroutine (tracked by `wg`) that:
   a. Builds a `JobContext` via `buildJobContext()`.
   b. Calls `muxFromStaging()` on the orchestrator.
   c. On success: job → `Finished`, staging cleaned up.
   d. On failure: job → `Error` with the FFmpeg error message.

This bypasses `ShouldProcess` and the queue entirely — muxing is CPU-bound and should not compete with download slots. The `wg` tracking ensures graceful shutdown waits for in-flight mux operations. Concurrent mux operations are allowed without limiting — mux is a stream copy (not re-encoding), so it is I/O-bound and fast.

### Execution

1. Set status to `Muxing`.
2. `muxFromStaging()` discovers files, constructs `DownloadResult`, calls `muxAndFinalize()`.
3. On success: status → `Finished`, staging cleaned up normally.
4. On failure: status → `Error` with the FFmpeg error message. The user sees what went wrong and can manually mux if needed.

## Twitch Platform Differences

### Shutdown

Twitch HLS streams cannot be resumed after process restart. On startup, the stream processor re-probes the channel. If the stream is still live, it starts a fresh download. The old staging files are overwritten since the downloader uses fixed filenames (`video_stream`). If the channel is offline, the job transitions to Error.

### Cancel

Resume is not available for Twitch (HLS segments are not addressable by sequence number after reconnection). Available actions: Delete, Reinitialize, Mux.

## TUI Changes

### Chord Assignments

| Chord | Action | Replaces |
|---|---|---|
| `A R` | Resume | Retry (removed) |
| `A I` | Reinitialize | (new) |
| `A M` | Mux | (new) |
| `A Z` | Import | `A I` (moved to free the chord) |

### Menu Item Definitions (buildMenuItems)

**Resume (`A R`):**
- Label: "Resume Job"
- HintLabel: "Resume"
- Category: "Action"
- NeedsJob: true
- JobFilter: `status in {Cancelled, Error, COOKIES?}` AND platform is `youtube` AND staging directory has files (checked via filesystem)
- DisabledReason: "no resumable jobs"

**Reinitialize (`A I`):**
- Label: "Reinitialize Job"
- HintLabel: "Reinit"
- Category: "Action"
- NeedsJob: true
- JobFilter: `status in {Error, Cancelled, COOKIES?}`
- DisabledReason: "no retriable jobs"

**Mux (`A M`):**
- Label: "Mux Job"
- HintLabel: "Mux"
- Category: "Action"
- NeedsJob: true
- NeedsConfirm: true
- JobFilter: `status in {Cancelled, Error}` AND segment files detected in staging (checked via filesystem)
- DisabledReason: "no muxable jobs"

**Import (`A Z`):**
- Same definition as current `A I`, only the chord changes.

The help screen (`?` key) auto-derives from `buildMenuItems()`, so "Retry" will disappear and "Resume", "Reinitialize", and "Mux" will appear automatically with their correct chords and labels.

### Staging checks in the TUI

The TUI runs in-process and can check the filesystem directly. The `JobFilter` functions receive a `*database.Job` — the staging path is computed as `filepath.Join(stagingBase, job.ID)`. A helper function `hasStagingFiles(jobID)` and `hasSegmentFiles(jobID)` will be added to the worker package (or passed as closures to the TUI) to encapsulate the filesystem checks.

### Callback changes

The TUI `App` struct currently has `OnRetryJob func(jobID string)`. This is replaced by:
- `OnResumeJob func(jobID string)` — calls `worker.ResumeJob()`
- `OnReinitializeJob func(jobID string)` — calls `worker.ReinitializeJob()` (same logic as old `OnRetryJob` + staging cleanup + seq counter clearing)
- `OnMuxJob func(jobID string)` — calls `worker.MuxJob()`

The wiring in `cmd/moombox/main.go` updates accordingly.

### dispatchAction Changes

- Remove the `"A R"` Retry case.
- Add `"A R"` Resume case: calls `OnResumeJob`.
- Add `"A I"` Reinitialize case: calls `OnReinitializeJob`.
- Add `"A M"` Mux case: calls `OnMuxJob`.
- Update `"A I"` Import to `"A Z"`.

### Batch operations

Batch Resume/Reinitialize/Mux follow the same pattern as existing batch Cancel/Delete:
- Iterate over selected jobs, apply the action to each that passes the JobFilter.
- Jobs that don't qualify are silently skipped (e.g., batch Resume skips Twitch jobs and jobs without staging).
- The status bar shows a count of how many jobs were affected (e.g., "Resumed 3 of 5 selected jobs").

## Web UI Changes

### API Endpoints

**New endpoints:**

- `POST /api/jobs/{id}/resume` — Resume a YouTube job with partial downloads. Validates: status in `{Cancelled, Error, COOKIES?}`, platform is `youtube`, staging directory exists. Sets status to `Downloading`, clears error, preserves progress, re-enqueues.
- `POST /api/jobs/{id}/reinitialize` — Fresh restart. Validates: status in `{Error, Cancelled, COOKIES?}`. Clears all progress fields and seq counters, deletes staging, sets status to `Upcoming`, re-enqueues.
- `POST /api/jobs/{id}/mux` — Force mux on a stopped job. Validates: status in `{Cancelled, Error}`, staging has segment files. Sets status to `Muxing`, dispatches mux via `worker.MuxJob()`.

**Deprecated endpoint:**

- `POST /api/jobs/{id}/retry` — Maps to `/reinitialize` logic for backward compatibility. Remove after one release cycle.

### Frontend (app.js)

**Status sets:**

```javascript
const RESUME_STATUSES = new Set(["Cancelled", "Error", "COOKIES?"]);  // + staging + youtube check
const REINIT_STATUSES = new Set(["Error", "Cancelled", "COOKIES?"]);
const MUX_STATUSES = new Set(["Cancelled", "Error"]);                  // + segment file check
const CANCEL_STATUSES = new Set(["Downloading", "Live", "Upcoming", "Muxing", "COOKIES?"]);
const DELETE_STATUSES = new Set(["Finished", "Error", "Cancelled", "COOKIES?"]);
```

**Button changes in job details panel:**

- Remove "Retry" button.
- Add "Resume" button (visible when status is in RESUME_STATUSES, platform is `youtube`, and `has_staging` is true).
- Add "Reinitialize" button (visible when status is in REINIT_STATUSES).
- Add "Mux" button (visible when status is in MUX_STATUSES and `has_segments` is true).

### Staging File Detection (API response)

To support the staging/segment checks in the Web UI without filesystem access from the frontend:

Add two computed fields to the job API response (not stored in DB — computed on read):
- `has_staging: bool` — staging directory for this job exists and is non-empty.
- `has_segments: bool` — staging directory contains recognized segment files (video_stream, audio_stream, video.ts, video.mp4, audio.m4a).

These are computed in the job serialization layer by checking `filepath.Join(stagingBase, job.ID)`. The filesystem check is cheap (single `ReadDir` call) and only performed for jobs in relevant statuses.

## Migration Path

1. The `/retry` endpoint continues to work during a transition period, mapped to Reinitialize logic. Remove after one release cycle.
2. The `A R` chord changes from Retry to Resume. For Error/COOKIES? jobs with staging, `A R` now resumes instead of restarting — this is strictly better behavior. For Error/COOKIES? jobs without staging (or Twitch jobs), `A R` is disabled and `A I` (Reinitialize) is the correct action. The disabled reason message guides the user.
3. WebSocket event names update: `job_retried` → `job_resumed` / `job_reinitialized` / `job_mux_started`. During the transition period, the deprecated `/retry` endpoint emits `job_reinitialized` (not the old name). All three events are emitted from their respective API endpoint handlers (not the worker) and carry the same payload as the existing `job_retried`: `{ type: "<event>", job: <full job object> }`. The frontend handles them by refreshing the job in the active/archived lists — same as current `job_retried` handling.
