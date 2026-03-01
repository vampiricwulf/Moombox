### Features

- **Disk Space Monitoring** — Output drive free space is checked every ~6 minutes. Both the Web UI and TUI status bars show a color-coded disk indicator (green/yellow/red). Discord notifications fire when configurable thresholds are crossed (default 90% warn, 95% critical) with a 30-minute cooldown. New `[disk]` config section with `disk_warn_percent` and `disk_critical_percent` fields.
- **Stats Dashboard** — New "Stats" tab in the Web UI with a disk usage bar, storage breakdown by platform and status, and activity metrics (streams archived, total recording time, chat messages, active downloads). Auto-refreshes every 60 seconds.
- **TUI Status Bar Metrics** — Disk percentage/free space and active download count now display in the TUI status bar alongside cookie indicators.

### Bug Fixes

- **Idle heap reduced ~30 MB** — BotGuard VMs are evicted when not in use, cipher solver cache trimmed to 3 entries.
- **Keyboard navigation** now includes the Files tab (previously skipped when cycling with number keys).
