### Bug Fixes

- **Fixed TUI chat count not updating for Upcoming jobs** — the progress overlay was gated behind `hasActiveDownloads()` which excluded Upcoming status, so early chat message counts never appeared in the job details panel
