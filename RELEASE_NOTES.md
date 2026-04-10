### Features

- Probe metadata refresh during Upcoming phase — lightweight probes now persist title, thumbnail, description, and scheduled start time changes at zero additional network cost
- "Schedule Changed" notification when a stream's scheduled start time is rescheduled, filterable per-webhook in both Web UI and TUI settings
- Unified metadata update function with change detection — only writes to DB when values actually differ

### Bug Fixes

- Fix TUI not showing metadata updates (title, thumbnail, start time) for Upcoming jobs until status changed
- Fix scheduled start time silently ignored when YouTube reschedules a stream
