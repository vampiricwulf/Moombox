### Improvements

- Adopt charmbracelet ecosystem components across the TUI: `bubbles/spinner` for loading states, `bubbles/table` for format selection, `bubbles/list` for task list, files dialog, and action menu
- Animated loading spinners in all dialogs (add video, import, trim, FFmpeg install, setup wizard, orphaned files)
- Format selection now uses a proper table with column headers, row highlighting, and built-in scrolling
- Progress tick rate increased to ~60fps during active downloads for smoother updates (adaptive: drops to 500ms when idle)
- Split import dialog into its own component for cleaner separation from add-video flow
- Shared text input utilities extracted to reduce code duplication across dialogs

### Bug Fixes

- Fix viewport arrow key navigation in job details panel (KeyMap was zeroed out, disabling all keys)
- Fix log viewer letter keys (j/k/d/u/f/b) conflicting with app chord bindings
- Remove misleading `MouseWheelEnabled` on log viewer (mouse scroll handled explicitly)
- Fix cipher solver for YouTube's restructured player.js
- Exempt POT provider endpoints from CSRF middleware
