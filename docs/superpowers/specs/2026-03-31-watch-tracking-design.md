# Watch Tracking — Design Spec

Track watched status and resume position for downloaded videos in Moombox's Web UI, providing a Plex-like experience for marking videos as watched and resuming playback where the user left off.

## Scope

- **Web UI only** — the TUI cannot play videos and will not display watch state
- **Own player only** — tracked through Moombox's HTML5 video player (single-file and multi-segment via SegmentPlayer), not embedded YouTube/Twitch iframes
- **Finished jobs only** — watch indicators and actions only apply to jobs with status `Finished`

## Data Model

### Schema Migration (v8)

Two new columns on the `jobs` table:

```sql
ALTER TABLE jobs ADD COLUMN watched INTEGER DEFAULT 0;
ALTER TABLE jobs ADD COLUMN resume_position REAL;
```

- `watched` — boolean (0/1), whether the job is marked as watched
- `resume_position` — seconds into the video where the user left off, NULL means no resume position

### Job Struct

```go
Watched        bool     `json:"watched"`
ResumePosition *float64 `json:"resumePosition,omitempty"`
```

### fieldToColumn Map

Add both fields to the `fieldToColumn` map in `database.go`:

```go
"watched":         "watched",
"resume_position": "resume_position",
```

### scanJob

The `scanJob` function (used by all job queries) must be updated to scan the two new columns into the Job struct.

### Database Methods

**`UpdateResumePosition(jobID string, seconds float64)`** — Lightweight method that updates only `resume_position` without bumping `updated_at` or triggering subscriber notifications. Called every 10 seconds during playback. Direct SQL update, no read-back, no broadcast.

**`UpdateJobFields`** — Used for watched toggle and resume reset (infrequent user actions). Triggers `updated_at` bump and subscriber notifications as normal.

**`BatchSetWatched(jobIDs []string, watched bool)`** — Sets `watched` and clears `resume_position` for multiple jobs in a single UPDATE. Triggers `OnJobsChange` (full list refresh) after completion.

## API Endpoints

### Resume Position (lightweight, high-frequency)

```
PUT /api/jobs/{id}/resume-position
Body: { "position": 1234.5 }
Response: 204 No Content
```

Calls `UpdateResumePosition()`. No subscriber notification, no WebSocket broadcast.

### Watched Toggle (single job)

```
POST /api/jobs/{id}/watched
Response: 200 + updated job JSON

DELETE /api/jobs/{id}/watched
Response: 200 + updated job JSON
```

Both set `resume_position=NULL` and go through `UpdateJobFields` which triggers WebSocket `job_update` broadcast.

- `POST` sets `watched=1`
- `DELETE` sets `watched=0`

### Batch Watched Toggle

```
POST /api/jobs/batch/watched
Body: { "jobIds": ["id1", "id2", ...] }
Response: 200

DELETE /api/jobs/batch/watched
Body: { "jobIds": ["id1", "id2", ...] }
Response: 200
```

Both clear `resume_position` for all specified jobs. Silently skip non-finished jobs. Trigger `BroadcastJobsUpdate` (full job list refresh) after completion.

## Web UI — Player Integration

### Resume Dialog

When the user opens the player for a job that has a non-null `resume_position`:

1. Video loads but does not autoplay
2. A centered overlay appears on the player area with two buttons:
   - **"Resume from X:XX:XX"** (primary, indigo) — seeks to the saved position and starts playing
   - **"Start from beginning"** (secondary, gray) — plays from 0:00
3. If `resume_position` is null, the video plays from the beginning with no dialog

### Periodic Save

While the video is playing, a `setInterval` timer fires every 10 seconds:

- Calls `PUT /api/jobs/{id}/resume-position` with `video.currentTime` (or total elapsed time for multi-segment)
- Also saves on `pause` event
- Also saves on `beforeunload` via `navigator.sendBeacon()` (fire-and-forget for tab close/crash). The resume-position endpoint must also accept POST (in addition to PUT) since `sendBeacon` can only send POST requests.
- Timer is cleared when the player is closed or the video ends

### Watched Detection

The video is considered "watched to the end" when either condition is met:

- Current time is within **30 seconds** of the end
- Current time is at or past **95%** of total duration

This applies whether the user watches normally or seeks to the end.

When the threshold is reached:

1. Call `POST /api/jobs/{id}/watched` (sets `watched=1`, clears `resume_position`)
2. Stop the periodic save timer (no more resume saves needed)

For videos where `lengthSeconds` is null/0, use the video element's reported `duration` instead.

### Multi-Segment Playback

Resume position is stored as **total elapsed seconds across all segments**. The SegmentPlayer:

- On resume: calculates which segment contains the target time by summing segment durations, seeks to the correct segment at the correct local offset
- On periodic save: reports total elapsed time (sum of completed segment durations + current segment position)
- Watched threshold: checked against total duration of all segments combined

## Web UI — Job List

### Thumbnail Eyeball Icon

An eyeball icon appears at the **bottom-right of the thumbnail**, with a semi-transparent dark background pill for contrast. Only rendered for finished jobs.

**Three visual states:**

1. **No icon** — unwatched, no resume position (`watched=false, resumePosition=null`)
2. **Outlined eye** (amber/yellow stroke, no fill) — has resume position, partially watched (`resumePosition != null, watched=false`)
3. **Filled eye** (green fill) — marked as watched (`watched=true`)

When a job is watched AND has a resume position (rewatching), the **filled eye** (green) takes precedence since the watched status is the dominant state.

### Batch Actions

Two new buttons added to the existing batch action bar (visible when checkboxes are selected):

- **"Mark Watched"** (green, eye icon) — calls `POST /api/jobs/batch/watched`
- **"Mark Unwatched"** (gray, eye-off icon) — calls `DELETE /api/jobs/batch/watched`

These appear after the existing Cancel/Resume/Reinitialize/Delete buttons, separated by a visual divider.

## Web UI — Job Details

### Watch Status Pill

A compact pill/chip displayed in the job details panel (alongside other metadata, not as a dedicated row). Only shown for finished jobs.

**Four display states:**

1. *(not shown)* — unwatched, no resume position
2. **"Paused at 1:23:45"** (amber background) — unwatched, has resume position
3. **"Watched"** (green background) — watched, no resume position
4. **"Watched · Paused at 1:23:45"** (green background) — watched, has resume position (rewatching in progress)

Each pill includes the corresponding eye icon (outlined for amber, filled for green).

### Individual Action Buttons

In the job details for finished jobs:

- **"Mark as Watched"** button — visible only when `watched=false`. Sets `watched=1`, clears `resume_position`.
- **"Mark as Unwatched"** button — visible only when `watched=true` OR `resumePosition != null`. Sets `watched=0`, clears `resume_position`.

## State Transition Matrix

| Action | `watched` | `resume_position` |
|--------|-----------|-------------------|
| Periodic save during playback | unchanged | → currentTime |
| Reaches end (30s / 95% threshold) | → true | → NULL |
| Seeks to end | → true | → NULL |
| Manually marks watched | → true | → NULL |
| Manually marks unwatched | → false | → NULL |
| Rewatch periodic save | unchanged (true) | → currentTime |
| Rewatch reaches end | unchanged (true) | → NULL |
| Job deleted | row deleted | row deleted |
| Job reinitialised | unchanged | unchanged |

## Edge Cases

- **Tab closed mid-playback:** `navigator.sendBeacon()` fires a final position save on `beforeunload`. Fire-and-forget — no response expected.
- **Multiple tabs watching same video:** Last-write-wins. No conflict resolution needed.
- **Video has no duration info:** Use the video element's reported `duration` property. If neither `lengthSeconds` nor the player reports a duration, skip the 95% threshold and rely on the 30-second-from-end check only.
- **Non-finished jobs:** Watch state fields exist in the database but are ignored in the UI. Eyeball icons, pills, and action buttons only render for `Finished` status. Batch operations silently skip non-finished jobs.
- **Job archival:** Watch state persists — archived jobs retain their watched/resume state.

## TUI

No changes. The TUI does not display watched status or resume position in job details, as it cannot play videos.
