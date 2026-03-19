### Bug Fixes

- Fix Resume/Mux buttons disappearing in Web UI details dialog during WebSocket updates
- Fix batch operations ignoring archived jobs in Web UI
- Fix TUI batch cancel not filtering by cancellable status (could cancel finished/errored jobs)
- Fix FFmpeg "i" shortcut in TUI settings typing into the path field before opening installer
- Fix "Cancelled 0 jobs" feedback when no jobs in selection match cancellable status

### Improvements

- Extract `EffectiveStagingDir()` config helper — eliminates 6+ repeated "./staging" fallbacks
- Consolidate template preview logic — settings delegates to shared `templatePreview()` function
- Extract shared job container click handler in Web UI — deduplicates checkbox/expand/action logic
- Per-container shift-click tracking — prevents cross-container range selection bugs
- Extract `wasCancelledOrShutdown()` in chat downloader — deduplicates cancellation detection
- Close chat downloader `done` channel safely under lock
- Consistent struct field alignment across TUI panel declarations
