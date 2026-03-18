### Bug Fixes

**Web UI (80+ fixes across 28 audit rounds)**
- XSS defense-in-depth: 22 hardening fixes across all template literals and user-provided values
- Double-click/double-submit protection on all forms, dialogs, and action buttons
- Race conditions fixed: keyboard shortcuts, WebSocket reconnects, async chains, in-flight fetches, config mutations
- Stale state handling: config on reconnect, job details dialog, logs, uptime, archived jobs
- Null guards on WebSocket payloads, API responses, and JSON parsing throughout
- Dialog state fixes: details dialog for archived/deleted jobs, auto-clear timeouts, loading indicators
- CSS overflow fixes: logs panel, header, trim handles, light mode styling
- Keyboard navigation: focus management, shadow DOM input filtering, quick-action visibility
- Player: nico animation leak fix, chat badge colors, stale log prevention, segment seek clamp
- Import/export: cancel state leak, non-file drop handling, numeric input validation
- Job details: segments row now appears immediately for Live/Downloading jobs (was missing until status change)
- Job verification: dialog stays open on server errors (only closes on 404, not 500/401)

**Setup Wizard (14 fixes)**
- Channel IDs trimmed, field index clamped after platform cycle
- Password validation UX, masked input, timeout race fixes
- JSON validation for channel terms
- Cross-platform cookie detection, range validation parity, input constraints
- Redirect on port/HTTPS change, submit timeout
- Missing quality options added to channel dialog
- Navigation and hang recovery improvements
- Step label overflow fix for small screens
- TUI now writes ActivePlatforms (matches Web UI config format)

**Server / Backend**
- Config save uses copy pattern to prevent inconsistent in-memory state
- CleanupJob guard on delete, setup re-run guard
- TUI: re-entrant RLock fix, stale details after filter
- OnSaveConfig moved back inside cfgMu lock (data race fix)
- Bare recover() calls now log panics to stderr (setup and update routes)
- Cookie refresh_interval: null from frontend now properly resets the field

**Settings**
- Password fields preserved during config reload when user is mid-entry
- Cookie auto-setup done button has proper double-click protection
