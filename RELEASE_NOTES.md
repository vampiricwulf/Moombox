### Improvements

- Web UI log viewer now preserves scroll position — auto-scroll pauses when you scroll up to read older entries, resumes when you scroll back to bottom
- Web UI now receives full startup logs on connect instead of only logs generated after the page loaded

### Bug Fixes

- Fixed auto-cookie refresh racing with itself or browser setup, causing "another process is already running" errors. Concurrent refresh attempts are now skipped, and setup blocks while a refresh is in progress
- Fixed orphaned browser child processes during cookie refresh on Windows — all kill paths now use process tree kill (`taskkill /F /T`) so no children survive to hold profile locks
- Fixed `runWithTimeout` not reaping processes after timeout kill, leaking goroutine and process resources
- Fixed TUI log panel showing duplicate entries when filtering
- Removed redundant DEBUG+ log filter level from both TUI and Web UI (identical to All since DEBUG is the lowest level)
