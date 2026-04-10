# Probe Metadata Refresh During Upcoming Phase

**Date:** 2026-04-09
**Scope:** YouTube only (Twitch unchanged)

## Problem

When a YouTube stream is in the Upcoming state, Moombox polls with lightweight `ProbeVideoStatus` calls to detect when it goes live. These probes return full metadata (title, thumbnail, description, scheduled start time, etc.) but the polling loop **discards everything except `ScheduledStartTime`** — and even that is only persisted once (when the job field is empty).

This means:
- If YouTube reschedules a stream, the new time is silently ignored
- Title/thumbnail changes during the Upcoming phase are never picked up
- Metadata only refreshes on the Live/VOD transition via an expensive `GetVideoInfo` call

## Solution

Use the metadata already returned by `ProbeVideoStatus` (zero additional network cost) and persist changes on every probe cycle where values actually differ.

## Design

### 1. Unified `updateJobMetadata` function

**Replace** the two existing functions:
- `updateJobMetadata(job, info)` — fills blanks only, sends "Start Time Confirmed" notification
- `updateJobMetadataOnLive(job, info)` — always overwrites, has start-time fallback

**With** a single function:

```go
func (sp *StreamProcessor) updateJobMetadata(job *database.Job, info *youtube.VideoInfo, overwrite bool)
```

**Behavior by mode:**

| Field | `overwrite=false` (initial) | `overwrite=true` (probe/live) |
|---|---|---|
| title | Set if non-empty, not "Unknown Title" | Same guards + only if differs from `job.Title` |
| channel_name | Set if non-empty, not "Unknown Channel" | Same guards + only if differs from `job.ChannelName` |
| thumbnail_url | Set if non-empty | Same guard + only if differs from `job.ThumbnailURL` |
| description | Set if non-empty | Same guard + only if differs from `job.Description` |
| stream_start_time | Set if job field empty | Overwrite if non-empty + differs from `job.StreamStartTime` |
| stream_end_time | Set if job field empty | Overwrite if non-empty + differs from `job.StreamEndTime` |
| length_seconds | Set if > 0 | Same guard + only if differs from `job.LengthSeconds` |

Note: in `overwrite=false` mode, fields like `description` that currently write unconditionally (no `job.Description == ""` check) retain that existing behavior for backward compat. In `overwrite=true` mode, **all** fields including `description` use change detection — no DB write unless the value actually differs.

When `overwrite=true`, the change-detection comparison against current job fields means **zero unnecessary DB writes** — if nothing changed, the updates map stays empty and `UpdateJobFields` is not called.

**Notifications:**
- "Start Time Confirmed" — fires when `stream_start_time` goes from empty to a value (regardless of mode)
- "Schedule Changed" (new) — fires when `stream_start_time` changes to a different non-empty value during `overwrite=true`

**Local job object sync:** Done inline for all fields after the DB write, consistently.

**Live start-time fallback:** The "use current UTC time if no start time available" logic stays in the caller at the live-transition site, applied before calling `updateJobMetadata`. Keeps the function clean.

### 2. Updated `waitForLive` loop

**Remove:** Lines 147-155 (dedicated `stream_start_time` handling block). The unified function handles this.

**Add after each successful probe:**
```go
// Update local scheduledStartTime for interval calculation
if probeInfo.ScheduledStartTime != "" {
    scheduledStartTime = probeInfo.ScheduledStartTime
}
// Persist any metadata changes (change-detected, zero-cost if nothing changed)
sp.updateJobMetadata(job, probeInfo, true)
```

**Flow change:**
```
Before: probe -> extract ScheduledStartTime -> persist once if empty -> check status
After:  probe -> update local scheduledStartTime -> updateJobMetadata(overwrite=true) -> check status
```

### 3. Call sites

| Site | File | Mode | Notes |
|---|---|---|---|
| `Process()` initial fetch | `stream_processor.go` | `overwrite=false` | Fill blanks from first `GetVideoInfo` |
| `waitForLive` probe loop | `stream_processor_youtube.go` | `overwrite=true` | Per-probe refresh from `ProbeVideoStatus` |
| `waitForLive` -> Live transition | `stream_processor_youtube.go` | `overwrite=true` | Full refresh from `GetVideoInfo` |
| `waitForLive` -> VOD transition | `stream_processor_youtube.go` | `overwrite=true` | Full refresh from `GetVideoInfo` |
| `waitForLive` -> unclear auth status | `stream_processor_youtube.go` | `overwrite=true` | Full refresh from `GetVideoInfo` |

### 4. What's NOT changing

- **Probe intervals** — same adaptive 1/5/10 min schedule with 0-30s jitter
- **Chat surge detection** — still triggers early probes
- **Twitch polling** — no changes to `waitForTwitchLive`
- **No new constants or config fields**
- **`ProbeVideoStatus` / `GetVideoInfo` APIs** — unchanged
- **Members-only transition logic** — unchanged

### 5. "Schedule Changed" notification

When `overwrite=true` and `stream_start_time` changes from one non-empty value to a different non-empty value:

```go
sp.notifier.Send("Schedule Changed",
    fmt.Sprintf("Rescheduled: %s", job.Title),
    notifications.TypeInfo,
    []notifications.Field{
        {Name: "Channel", Value: job.ChannelName, Inline: true},
        {Name: "Old Time", Value: oldStartTime, Inline: true},
        {Name: "New Time", Value: newStartTime, Inline: true},
    },
    notifications.SendOptions{
        URL:       job.URL,
        Thumbnail: job.ThumbnailURL,
        Event:     "rescheduled",
    },
)
```

### 6. Notification filtering for "rescheduled" event

The `rescheduled` event must be filterable per-webhook in both Web UI and TUI, just like all other notification events. The notification system automatically filters events based on the `Events` list in `NotificationConfig` — we just need to register the new event ID in the UI event group lists.

**Add `rescheduled` to the "Job Lifecycle" group in:**
- `internal/tui/settings.go` — `notifEventGroups` slice (after `"scheduled"`)
- `web/public/modules/settings.js` — `NOTIFICATION_EVENT_GROUPS` array (after `scheduled`)

The notification manager (`notifications/manager.go`) already handles filtering via `SendOptions.Event` — no changes needed there.

## Files touched

- `internal/worker/stream_processor.go` — replace `updateJobMetadata` + `updateJobMetadataOnLive` with unified function
- `internal/worker/stream_processor_youtube.go` — update `waitForLive` loop, update live/VOD transition calls
- `internal/tui/settings.go` — add `"rescheduled"` to `notifEventGroups`
- `web/public/modules/settings.js` — add `rescheduled` to `NOTIFICATION_EVENT_GROUPS`

## Testing

- Existing tests for `stream_processor` continue to pass
- New test: probe with changed metadata triggers DB update
- New test: probe with unchanged metadata skips DB update
- New test: schedule change triggers notification
- New test: initial fill mode doesn't overwrite existing values
