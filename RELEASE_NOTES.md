### Bug Fixes

- Trims now properly persist across restarts — AddTrim/DeleteTrim notify UI subscribers so both TUI and Web UI stay in sync
- Trim dialog refreshes its trim list in real-time when trims are added or deleted
- Delete trim errors are now surfaced to the user instead of silently swallowed

### Improvements

- Trim progress bar uses the mux gradient (green → yellow) instead of flat cyan
