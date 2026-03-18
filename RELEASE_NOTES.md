### Features

- Batch job operations: select multiple jobs with Space (TUI) or checkboxes (Web UI), then retry/cancel/delete in bulk with confirmation
- Newcomer chord hints in TUI status bar — dismisses on first chord use
- Contextual onboarding nudge after setup completion
- Per-platform cookie success confirmation in both UIs
- Cookie timeout countdown with retry/skip in setup wizard
- FFmpeg skip option during setup, re-accessible from Settings → Paths
- Recommended badge and descriptive subtext on setup mode selection

### Improvements

- Auto-scroll paused indicator in TUI log viewer
- Auto-scroll resume pill in Web UI log viewer
- Chat offset reset button in player
- Unsaved changes banner with discard/save in Web UI settings
- Live output template preview in settings and setup wizard
- Context-specific reasons for disabled actions in TUI menu
- Error messages expandable on desktop, full-width on mobile
- Phased progress and elapsed time during setup restart
- Parallel downloads setting now shows guidance text
- 'Include non-live' renamed to 'Archive uploads & premieres (YouTube only)'

### Bug Fixes

- Fix cookie countdown race: countdown no longer cancels in-flight extraction
- Fix View() state mutation in task list (Bubble Tea pure-render violation)
- Fix batch dispatch overriding explicit menu job selection
- Fix autoscroll pill destroyed by log viewer DOM rebuild
- Remove dead App.justCompletedSetup field
- 5 additional fixes from UX spec cross-reference audit
