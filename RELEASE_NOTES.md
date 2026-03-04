### Features

- **Unified chord system** — all TUI actions now use consistent two-key chords with prefix categories: `A` (Action), `R` (Request), `O` (Open), `Q` (Quit)
- **Action menu** (`M` key) — command palette overlay with full keyboard and mouse navigation, contextual job selection, and confirmation flows
- **Open stream page** (`O S`) — new chord to open a job's YouTube/Twitch stream page in the browser
- **Description toggle** — press `F` in the Details panel to show/hide the job description section
- **TUI cookie controls** — `R C` to recheck cookie auth status, `R F` to force browser cookie refresh (previously not accessible from TUI)

### Improvements

- Status bar now shows uniform chord hints across all panels instead of panel-specific controls
- Help overlay restructured around chord groups (Action, Request, Open, Quick Keys, Navigation)
- Version check/update hints in Details panel updated to show new chord keys (`R V` / `R U`)
- Panel-sensitive `F` key: cycles job filter in Tasks, toggles description in Details, cycles log level in Logs

### Internal

- Replaced ad-hoc confirm/chord state (`deleteConfirmID`, `cancelConfirmID`, `lastUPress`, `lastRPress`) with unified `chordState` state machine
- Extracted reusable helpers: `streamURL()`, `canOpenFolder()`, `canOpenStream()`, `openTrimForJob()`
- Removed dead code from keys.go (`isQuit`, `keyHome`, `keyEnd`, `keyQ`)
- Removed `SetFocused`/`focusedPanel` from StatusBarModel and `SetActivePanel`/`isActiveSection` from HelpModel
