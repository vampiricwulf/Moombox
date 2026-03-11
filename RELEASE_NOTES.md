### Bug Fixes

- **Fixed XSS vulnerability in chat replay** — chat messages from external users could inject HTML via innerHTML; now uses safe DOM methods
- **Fixed race condition in stream download** — `streamEnded` flag changed from plain bool to atomic, preventing data races between download loop and defer
- **Fixed silent ParseInt error in YouTube chat** — `videoOffsetTimeMsec` parse failure was setting offset to 0 instead of skipping
- **Fixed WebSocket throttle map memory leak** — throttle timers for completed jobs were never cleaned up
- **Fixed FFmpeg two-pass audio sync** — pass 2 now re-encodes audio with AAC instead of `-c:a copy`, fixing A/V sync with re-encoded video
- **Fixed response body leak in HTTP retry** — response body is now closed before retrying failed requests
- **Fixed Twitch URL parsing with port** — `u.Host` included port number, breaking host comparison
- **Fixed FFprobe silent parse errors** — duration and file size parse failures now return errors instead of silently returning zero
- **Fixed notification Wait() deadlock risk** — added 30-second timeout to prevent blocking shutdown indefinitely
- **Fixed unchecked file I/O in chat header updates** — WriteAt/Truncate errors now logged in all 3 updateChatFileHeader functions
- **Fixed resume temp file cleanup** — orphaned `.tmp` files no longer accumulate on write/rename failure
- **Fixed trimmer drag state on blur** — timeline drag now cancels when window loses focus
- **Fixed trimmer minimum duration** — enforces minimum 1-second trim duration

### Improvements

- **Full codebase audit & refactor** — 99 fixes and 20 structural refactors across 137 files in 10 phases
- **Major file splits for maintainability:**
  - `orchestrator.go` 2,055 → 385 lines (split into 6 files)
  - `app.go` 2,486 → 536 lines (split into 7 files)
  - `settings.go` 2,143 → 514 lines (split into 7 files)
  - `downloader.go` 1,430 → 270 lines (split into 6 files)
  - `jobs.go` 2,525 → 1,192 lines (split into 7 files)
  - `muxer.go` 673 → 225 lines (split into 4 files)
  - `chat.go` 1,031 → 323 lines (split into 3 files)
  - Plus splits of stream_processor.go, strategies.go, main.go, and others
- **150+ new tests** across all packages
- **GQL rate limit/auth detection** — Twitch API now detects 429/401/403 specifically
- **Shared JSON header utilities** — deduplicated chat header manipulation into `internal/utils/json.go`
- **CSS prefers-reduced-motion support** — respects user motion preferences
- **Login page responsive fix** — no longer breaks on screens narrower than 380px
- **Incremental log rendering** — fast-path append avoids full DOM rebuild when unfiltered
