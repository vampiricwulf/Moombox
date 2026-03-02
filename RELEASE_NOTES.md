### Bug Fixes

- Fixed auto-cookie refresh racing with itself or browser setup, causing "another process is already running" errors. Concurrent refresh attempts are now skipped, and setup blocks while a refresh is in progress
- Fixed orphaned browser child processes during cookie refresh on Windows — all kill paths now use process tree kill (`taskkill /F /T`) so no children survive to hold profile locks
- Fixed `runWithTimeout` not reaping processes after timeout kill, leaking goroutine and process resources
- Fixed TUI log panel showing duplicate entries when filtering
