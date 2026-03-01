### Bug Fixes
- Fix YouTube chat returning 0 messages by switching from "Top Chat" (filtered) to "All Chat" (unfiltered) on first API response
- Re-trigger All Chat switch after stale continuation recovery to avoid silently reverting to Top Chat
- Log chat API errors instead of silently swallowing them (surfaces auth failures, member-gated 403s, etc.)

### Internal
- Fix TUI CSRF bypass via shared secret token
