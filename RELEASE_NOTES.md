### Bug Fixes

- **Fixed rescheduled stream start time not updating** — watch page's `liveBroadcastDetails.startTimestamp` (authoritative source YouTube updates on reschedule) was being ignored in favor of TV client's stale `liveStreamability.scheduledStartTime` epoch due to "fill if empty" merge semantics
