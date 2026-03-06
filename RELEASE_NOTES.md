### Features

- **Unified TUI chord system** — `buildMenuItems()` is now the single source of truth for all keyboard chords, the action menu, chord feedback hints, and help text. Adding a new chord requires changes in only two places (menu items + dispatch handler) instead of five. Chord routing, feedback, and help all auto-derive from the menu definition.

### Bug Fixes

- Fix "A K" (Manage Client Tokens) keyboard shortcut doing nothing — chord was defined in the menu and execution handler but missing from keyboard routing
- Fix "A I" (Import Archive) not appearing in chord feedback hints when pressing the "A" prefix
- Fix chord confirm step (A C, A D) entering confirm mode even when no valid job is selected
- Fix confirm step not re-validating job status — if a job's status changed during the 3-second confirm window (e.g. download finishes), the action could execute on an invalid target

### Improvements

- Cancel/delete chords (A C, A D) now check the selected job's status before entering confirm mode, giving immediate feedback instead of silently doing nothing after confirmation
- WebSocket broadcasts skip JSON serialization when no clients are connected
- Database existence checks use `SELECT 1 ... LIMIT 1` instead of `COUNT(*)` for early termination
- History pruning consolidated into a single query
- Format selector caches codec scores during comparison instead of re-extracting per candidate

### Internal

- Replace `time.After` with `time.NewTimer` + explicit `.Stop()` across 10 call sites (bgutils, engine, orchestrator, stream processor, utils) to avoid leaked timers in select statements
- Replace `fmt.Sprintf("%d")` with `strconv.Itoa`/`strconv.FormatInt` across hot paths (cookies, muxer, progress, rate limiter, auth, webpo minter)
- Replace string concatenation with `strings.Builder` in chat file writers (YouTube chat, Twitch chat, Twitch VOD chat)
- Replace `os.ReadFile`/`os.WriteFile` with streaming `io.Copy` for file operations (orchestrator chat/thumbnail copy, TUI import)
- Replace `sort.Slice` with `slices.SortFunc` (jobs routes, task list)
- Hoist compiled regexps and string replacers to package level (cipher extractor, worker filename sanitizer, config template resolver)
- Pre-allocate slices and maps with capacity hints across database, engine, twitch, worker, and task list packages
- Replace `sync.Mutex` with `atomic.Bool` for logger stdout suppression toggle
- Hoist lipgloss styles to package level in status bar and task list (avoid re-allocation per render)
- Cache TUI HTTP client for local API calls instead of re-creating per request
- Remove redundant JSON re-marshal on Android VR retry path (player API)
