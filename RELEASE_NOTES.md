### Bug Fixes
- Fix WebSocket origin validation using proper host-only patterns instead of InsecureSkipVerify
- Fix operator precedence bug in cookie file parsing (#HttpOnly_ lines)
- Fix data race in monitor NextCheckAt access (feed, DECAPI, Twitch)
- Fix auth session cleanup goroutine leak on Stop()
- Fix database migration defer rows.Close() scoping (deferred to function end, not block)
- Fix emote resolver concurrent duplicate fetches (added inflight dedup)
- Fix XSS vector in web UI job detail buttons (unescaped job ID in innerHTML)
- Fix TUI panic on empty channel fields in settings editor
- Fix sendInitialState silently ignoring WebSocket write errors

### Improvements
- Add panic recovery to all spawned goroutines (job processing, chat downloads, BotGuard VM, notifications)
- Add CSRF protection for mutating API requests from external origins
- Add rate limiting to FFmpeg muxing/remuxing endpoints
- Add context cancellation support to updater HTTP requests
- Add HTTP response body draining on non-200 for TCP connection reuse
- Add logger job log pruning to prevent unbounded memory growth
- Add WebSocket throttle timestamp cleanup to prevent map growth
- Add atomic counters for TUI dropped message tracking
- Migrate downloader bytesWritten to atomic.Int64 (lock-free)
- Deduplicate TUI job detail segment row rendering (~120 lines)
- Use O(1) index map for TUI job updates instead of linear scan
- Return consistent JSON error responses from file-serving routes
- Remove redundant database re-fetch in video serve route
- Marshal YouTube API request body once before retry loop
