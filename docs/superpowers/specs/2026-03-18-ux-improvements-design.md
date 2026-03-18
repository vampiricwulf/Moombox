# UX Improvements Design — Full Sweep

**Date:** 2026-03-18
**Scope:** TUI + Web UI + Setup — strict parity, additive + light restructuring
**Approach:** Theme-based phases (4 phases, 18 improvements)

---

## Overview

A comprehensive UX improvement pass across both interfaces and the setup process. Organized into four theme-based phases, each independently shippable:

1. **Setup & Onboarding** — First-run wizard clarity, post-setup guidance
2. **Feedback & Indicators** — Real-time feedback, clearer action states
3. **Discoverability & Labels** — Feature findability, label clarity
4. **Power User Features** — Batch operations, convenience controls

All changes maintain strict parity between Web UI and TUI where both interfaces support the feature. Changes are additive with light restructuring where it clearly improves UX.

---

## Phase 1: Setup & Onboarding (6 items)

### 1.1 Mode Selection Guidance

**Problem:** Quick Setup and Advanced Setup are presented with no recommendation. New users don't know which to pick.

**Solution:**
- Add "(Recommended)" badge to Quick Setup card
- Add descriptive subtext: Quick = "Best for most users — takes ~2 minutes", Advanced = "Full control over every setting"
- **Web UI:** Badge element on the Quick Setup card, subtext in `<p>` tags
- **TUI:** "(recommended)" suffix on Quick Setup label in mode select screen

**Files:** `web/public/index.html` (setup mode cards), `internal/tui/setup_wizard.go` (mode select view)

### 1.2 Cookie Timeout Countdown

**Problem:** Auto-cookie browser login has a silent 60s timeout (client-side `setTimeout` in setup.js/settings.js). Users get a vague "No login detected" error with no warning.

**Solution:**
- Show visible countdown timer in the cookie dialog (both UIs)
- At 10s remaining, show amber warning text
- On timeout, offer "Try Again" and "Skip" buttons instead of a vague failure message
- 60s timeout value unchanged
- Track both YouTube and Twitch independently — each platform gets its own countdown when active

**Web UI implementation:** Add a `setInterval` (1s tick) alongside the existing `setTimeout`. Countdown text element in the auto-cookie dialog updates each tick. At ≤10s, apply `color: var(--sl-color-warning-600)`. On timeout, replace dialog content with "Try Again" / "Skip" buttons. Clear interval on success, cancel, or timeout.

**TUI implementation:** The TUI cookie flow currently waits indefinitely (no timeout). Add a `tea.Tick` command that fires every second, decrementing a `cookieCountdown int` field on `SetupWizardModel`. Spinner text updates: "Waiting for login... (45s remaining)". At ≤10s, render countdown in amber via `lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f"))`. On expiry (countdown reaches 0), cancel the cookie operation and show "Try Again" / "Skip" options. This adds a new timeout to the TUI that didn't previously exist — matching Web UI behavior.

**Files:** `web/public/modules/setup.js` + `web/public/modules/settings.js` (cookie dialog logic), `internal/tui/setup_wizard.go` (new `tea.Tick` timer, countdown state, cookie flow)

### 1.3 Cookie Success Confirmation (Per-Platform)

**Problem:** After successful cookie extraction, TUI shows a badge but no explicit confirmation. Web UI shows "Done" badge. Neither gives a clear per-platform success message.

**Solution:**
- On successful extraction, show explicit green confirmation per platform:
  - "YouTube cookies configured" or "Twitch cookies configured"
- Message displays for ~2s before auto-advancing
- Both platforms tracked independently — completing YouTube shows YouTube confirmation, then user can proceed to Twitch or skip
- **Web UI:** Brief inline success alert in the cookie dialog + existing "Done" badge
- **TUI:** Green feedback message per platform

**Files:** `web/public/modules/setup.js`, `web/public/modules/settings.js`, `internal/tui/setup_wizard.go`

### 1.4 FFmpeg Skip Option + Re-Accessible Installer

**Problem:** TUI FFmpeg overlay forces resolution with no skip. Web UI has "Quit Moombox" but no soft skip. Neither UI allows re-accessing the guided installer later.

**Solution — two parts:**

**Part A: Skip option**
- TUI: Add "Skip for now" keybinding (S) to FFmpeg check overlay
- Show warning: "Muxing will fail until FFmpeg is installed. You can install it later from Settings → Paths."
- Web UI: Add "Skip for now" alongside existing "Quit Moombox" button

**Part B: Re-accessible installer**
- Add "Install FFmpeg" button/action in Settings → Paths, next to the FFmpeg path input
- Clicking opens the same guided installer overlay (Choco/Winget/manual)
- **Web UI:** Button next to FFmpeg path input in settings. Reuses the existing `#ffmpeg-overlay` / `#ffmpeg-install-view` elements — show them as a dialog rather than full-page overlay when triggered from settings.
- **TUI:** Action keybinding (I for Install) when FFmpeg path field is focused in settings. Opens the `FFmpegCheckModel` overlay.

**Files:** `internal/tui/ffmpeg_check.go`, `internal/tui/settings.go` + `settings_view.go`, `web/public/index.html`, `web/public/modules/settings.js`, `web/public/modules/setup.js`

### 1.5 Restart Progress Feedback (Web UI)

**Problem:** After setup completes, Web UI shows static "Restarting Moombox..." for up to 2 minutes with no progress.

**Solution:**
- Replace the static message with phased progress display, mapping to the existing setup completion flow in `setup.js`:
  1. "Saving configuration..." — shown immediately when user clicks Finish, before the `POST /api/setup/complete` call
  2. "Restarting server..." — shown after the POST returns 200 (server accepted config, will restart)
  3. "Reconnecting..." — shown when polling begins (`GET /api/setup/status` every 2s)
  4. "Connected" — shown when poll returns `isFirstRun: false` (server has restarted with new config)
- Show elapsed seconds via a `setInterval` timer started at phase 1
- After 15s in phase 3, add hint: "This is taking longer than usual. The server may be installing plugins."
- If the POST itself fails (phase 1), show error with "Try Again" button — don't advance to polling
- If port/HTTPS changed, show redirect notice with new URL (existing behavior, just integrate with new progress display)

**TUI equivalent:** Not needed — TUI restart is a full process exit/relaunch (exit code 42), handled by the launcher.

**Files:** `web/public/modules/setup.js` (restart polling logic — wraps existing `waitForRestart()` flow)

### 1.6 Post-Setup Onboarding Nudge

**Problem:** After setup, users land on an empty job list with a generic "No jobs yet" message. No guidance about channel monitoring (the primary use case).

**Solution:**
- Enhanced empty state that detects first-run context
- Shows welcome message with two paths:
  1. "Add channels to auto-monitor streams" → links to Settings → Channels tab
  2. "Add a video URL to start archiving now" → opens Add Video dialog

**Web UI implementation:** After setup completes and the app reinitializes, set `sessionStorage.setItem("justCompletedSetup", "1")`. In `renderJobs()`, when jobs array is empty and `sessionStorage.getItem("justCompletedSetup")` is truthy, show the welcome variant of the empty state instead of the generic one. Clear the flag after rendering. This survives the page reload during restart but doesn't persist across browser sessions.

**TUI implementation:** The `App` struct already has `IsFirstRun bool`. After setup wizard closes (which sets `IsFirstRun` back to false via restart), we instead keep a transient `justCompletedSetup bool` flag set during the setup completion handler. `task_list.go` checks this flag to show "Setup complete!" header with action hints: "Press \` to open Settings and add channels, or A A to add a video." Flag cleared after first job arrives or user navigates away.

**Files:** `web/public/app.js` (renderJobs empty state), `web/public/modules/setup.js` (set sessionStorage flag), `internal/tui/task_list.go` (empty view), `internal/tui/app.go` (justCompletedSetup flag)

---

## Phase 2: Feedback & Indicators (5 items)

### 2.1 Log Auto-Scroll Indicator (TUI)

**Problem:** When user scrolls up in logs, auto-scroll silently disables. No visual cue. Re-enables only when tabbing away.

**Solution:**
- When `autoScroll` is false, render a dim indicator at the bottom of the log viewport: "↓ Auto-scroll paused (End to resume)"
- Press End to re-enable auto-scroll (no "G" shortcut — would conflict with chord system since log panel keys go through `helpViewportKeyMap()` which blocks letter keys)
- Tab away also re-enables (existing behavior preserved)
- Indicator styled as dim/gray text, rendered as the last line of the viewport content (not a separate element)

**Files:** `internal/tui/log_viewer.go` (View method, End key handling)

### 2.2 Log Auto-Scroll Indicator (Web UI)

**Problem:** Similar silent disable on scroll. Existing "Sync" button isn't clearly connected to auto-scroll behavior.

**Solution:**
- When auto-scroll is paused, show a floating pill/badge at the bottom of the log panel: "↓ Resume auto-scroll"
- Clicking the pill re-enables auto-scroll and scrolls to bottom
- Pill has subtle animation (fade-in) to draw attention
- Position: `position: sticky; bottom: 0` within the log viewer's scrollable container (not `position: fixed`, which would be relative to viewport). The log viewer container (`#logs-viewer`) needs `position: relative` if not already set.
- Hidden when auto-scroll is active

**Files:** `web/public/index.html` (log panel — add pill element), `web/public/app.js` (log scroll handling — toggle pill visibility), `web/public/moombox.css` (pill styling with sticky positioning)

### 2.3 Action Menu Disabled Explanations (TUI)

**Problem:** Unavailable actions in the action menu show "(none)" with no explanation of why they're disabled.

**Solution:**
- Add a `DisabledReason string` field to `ActionMenuItem` struct in `action_menu.go`
- Each action that uses `NeedsJob` + `JobFilter` gets a corresponding `DisabledReason`:
  - Retry: "no failed jobs"
  - Cancel: "no active jobs"
  - Trim: "no finished jobs with files"
  - Delete: "no deletable jobs"
- When rendering and no jobs match the filter, display: "Retry · no failed jobs" in gray/dim styling, non-selectable
- The `DisabledReason` is set in `buildMenuItems()` alongside the existing `JobFilter`

**Files:** `internal/tui/action_menu.go` (ActionMenuItem struct, render logic), `internal/tui/app.go` (`buildMenuItems()` — add DisabledReason to each filtered action)

### 2.4 Error Message Truncation Fix (Web UI)

**Problem:** Error messages on job cards are truncated to 50 chars. Full text is only in tooltip, which doesn't work on mobile/touch devices.

**Solution:**
- **Mobile (<768px):** Show full error text with word-wrap, no truncation
- **Desktop:** Keep truncation but make the error text clickable to toggle between truncated and full. Click to expand in-place, click again to collapse. Tooltip stays as hover backup.
- Add `cursor: pointer` and subtle underline to indicate clickability on desktop

**Files:** `web/public/app.js` (renderJobCard error text), `web/public/moombox.css` (expandable error styling + mobile override)

### 2.5 Settings Unsaved Changes Banner (Web UI)

**Problem:** Settings `_dirty` flag exists but visual indicator is limited to the Save button. Easy to navigate away and lose changes.

**Solution:**
- Sticky warning bar at top of settings content area (below tab header, above settings sections)
- Text: "You have unsaved changes" with inline Save and Discard buttons
- "Discard" calls the existing `loadConfig()` method (re-fetches from server) and sets `_dirty = false`
- Appears on first field change, hides on save or discard
- If user tries to switch to a different main tab (Tasks, Player, etc.) while dirty, show confirmation dialog: "You have unsaved settings changes. Save before leaving?"
- Does NOT block switching between settings sections (those are internal navigation)
- **TUI parity:** TUI already handles this — pressing Esc in settings with changes prompts discard confirmation

**Files:** `web/public/modules/settings.js` (dirty tracking, banner rendering, discard = loadConfig()), `web/public/index.html` (banner element), `web/public/moombox.css` (sticky banner styling), `web/public/app.js` (tab switch interception)

---

## Phase 3: Discoverability & Labels (5 items)

### 3.1 Chord System Hints (TUI)

**Problem:** New users don't know the chord system exists. Must discover by accident or read help.

**Solution:**
- On first launch (and until dismissed), show a persistent hint: "Press ? for help · M for menu · A for actions"
- Auto-dismisses after user presses any chord prefix (A, R, O, Q), M, or ?
- Tracked via `seenChordHint bool` flag in App state
- Not persisted to config — resets each session until user demonstrates familiarity
- Rendered in the status bar model (`status_bar.go`) as an additional dim text section, appended to the right side of the existing status bar content. The status bar already has flexible layout — this adds a trailing hint section.

**Files:** `internal/tui/app.go` (hint state flag), `internal/tui/status_bar.go` (render hint in status bar), `internal/tui/app_keys.go` (dismiss on chord use)

### 3.2 "Include Non-Live Content" Label Clarity

**Problem:** Label "Include non-live content" is ambiguous. "Non-live" could mean anything.

**Solution:**
- Rename display label to "Also archive uploads & premieres"
- Add help text: "When enabled, Moombox will also capture uploaded videos and premieres from this channel, not just live streams."
- Config TOML field name `include_non_live_content` stays unchanged (backward compat)
- Apply to both channel add/edit dialogs

**Web UI files:** `web/public/index.html` (channel dialog), `web/public/modules/settings.js` (channel form)
**TUI files:** `internal/tui/settings.go` (channel editor fields), `internal/tui/setup_wizard.go` (channel addition in setup)

### 3.3 Platform-Specific Field Indicators

**Problem:** Channel editor hides/shows fields per platform silently. Users don't know why fields appear or disappear when switching platforms.

**Solution:**
- Append "(YouTube only)" to platform-specific field labels
- The primary value is helping users understand why a field disappears when they switch from YouTube to Twitch — the suffix provides that context while the field is still visible, before it hides
- Currently applicable: "Also archive uploads & premieres (YouTube only)"
- Fields still hide when the irrelevant platform is selected (existing behavior)
- Future-proofed for any new platform-specific fields

**Web UI files:** `web/public/index.html` (channel dialog labels)
**TUI files:** `internal/tui/settings.go` (channel field labels), `internal/tui/setup_wizard.go` (channel field labels)

### 3.4 Output Template Live Preview

**Problem:** Template field shows variable names (`${channel}/${start_date} ${title} [${id}]`) but no preview of what the output will look like.

**Solution:**
- Show a live preview below the template input using hardcoded sample data:
  - channel: "Miko Ch"
  - title: "Singing Stream"
  - id: "dQw4w9WgXcQ"
  - start_date: current date (YYYY-MM-DD)
  - start_time: "20-00-00"
- Preview text: "Example: Miko Ch/2026-03-18 Singing Stream [dQw4w9WgXcQ].mkv"
- Updates on each keystroke as user types
- Applied in both setup wizard and settings

**Web UI implementation:** Add a `<div class="template-preview">` below the template `<sl-input>`. Attach an `sl-input` event listener that performs `String.replace()` for each variable and updates the div text. Debounce at 100ms.

**TUI implementation:** The settings template field is rendered via the generic `renderFields()` in `settings_view.go`. To add a preview below this specific field, add a `previewFn func(value string) string` field to the `fieldDef` struct (in `settings.go`). When set, `renderFields()` calls it and appends the result as a dim line below the field. Only the output template field gets this callback. Same approach in `setup_wizard.go` for the setup template field — add preview rendering after the field in the advanced step view.

**Web UI files:** `web/public/index.html` (template input areas), `web/public/modules/settings.js`, `web/public/modules/setup.js`
**TUI files:** `internal/tui/settings.go` (settingsFieldDef + previewFn), `internal/tui/settings_view.go` (renderFields preview), `internal/tui/setup_wizard.go` (setup template preview)

### 3.5 Parallel Downloads Guidance

**Problem:** Parallel downloads number input has no context about implications.

**Solution:**
- Add help text: "Number of streams to download simultaneously. 2–4 recommended. Higher values use more CPU and network bandwidth."
- Applied to both setup wizard and settings
- **Web UI:** `help-text` attribute on `<sl-input>`
- **TUI:** `help` field on `setupFieldDef` / settings field

**Web UI files:** `web/public/index.html` (setup + settings parallel input)
**TUI files:** `internal/tui/setup_wizard.go` (parallel field), `internal/tui/settings.go` (parallel field)

---

## Phase 4: Power User Features (3 items)

### 4.1 Batch Job Operations (Web UI)

**Problem:** Each job action (cancel, delete, retry) requires individual clicks and confirmation. Managing many jobs is tedious.

**Solution:**
- Add checkbox to each job card (left side, before thumbnail)
- Checkboxes visible on hover (desktop) or always visible (mobile)
- When 1+ jobs selected, show floating action bar at bottom of the jobs panel:
  - "X selected" count
  - Action buttons: Cancel, Delete, Retry — each only shown if valid for at least one selected job
  - "Clear selection" (×) button
- Single confirmation dialog for batch operations: "Cancel 5 jobs?" / "Delete 3 jobs?"
- Selection state:
  - Clears on successful batch action
  - Clears on Esc
  - Persists across re-renders (tracked by job ID `Set`)
  - "Select All" / "Select None" in the action bar
- Job cards show selected state (subtle highlight/border)
- Keyboard: Shift+Click to select range (Web UI convention)

**Batch API strategy:** Use existing per-job endpoints (`POST /api/jobs/{id}/cancel`, `DELETE /api/jobs/{id}`, `POST /api/jobs/{id}/retry`) called via `Promise.allSettled()`. No new batch API endpoint needed. On completion, show toast: "Deleted 3 of 5 jobs" (handles partial failures). Failed individual operations reported in toast detail.

**Files:** `web/public/app.js` (selection state via `Set`, renderJobs checkbox, action bar, batch API calls via Promise.allSettled), `web/public/index.html` (action bar element), `web/public/moombox.css` (checkbox, action bar, selection highlight styling)

### 4.2 Batch Job Operations (TUI)

**Problem:** Jobs are operated on individually via the chord system. No way to act on multiple jobs at once.

**Solution:**
- Multi-select mode via Space key on task list:
  - Space toggles selection checkmark on current job
  - Visual marker: `[✓]` prefix on selected jobs
  - Status bar shows "X selected" when any jobs are selected
- When jobs are selected, chord actions apply to all selected:
  - A C (Cancel) → cancels all selected active jobs
  - A D (Delete) → deletes all selected jobs
  - A R (Retry) → retries all selected failed/cancelled jobs
- Confirm chords still apply — single confirmation for the batch
- Esc clears all selections
- Selection persists across list navigation (arrow keys)
- Selection state stored as `map[string]bool` keyed by job ID

**Space key handling:** The task list (`task_list.go`) uses `bubbles/list` with a custom delegate, but Space is not bound by default in `bubbles/list` for standard usage, and the task list's delegate returns nil for all messages — so Space won't conflict. The Space handler goes in `task_list.go`'s `Update()`, not in `app_keys.go`, to keep selection logic co-located with the task list model.

**Files:** `internal/tui/task_list.go` (selection state map, Space handler, render markers), `internal/tui/app_keys.go` (chord dispatch checks selection for batch), `internal/tui/app.go` (batch action callbacks)

### 4.3 Player Chat Offset Reset (Web UI only)

**Problem:** Chat offset input exists but there's no quick reset to zero. User must manually clear and type 0.

**Solution:**
- Add small "×" reset button next to the chat offset input
- Only visible when offset value ≠ 0
- Clicking resets offset to 0, clears the input, and re-syncs chat display
- Styled as a small icon button (Shoelace `sl-icon-button`)

**TUI:** Not applicable — TUI has no player.

**Files:** `web/public/index.html` (reset button element), `web/public/modules/player.js` (reset handler, visibility toggle)

---

## Cross-Cutting Concerns

### Testing Strategy
- Each phase should be manually tested in both UIs before shipping
- No new automated tests needed for cosmetic/label changes (phases 1, 3)
- Batch operations (phase 4) should have unit tests for selection state management and batch action filtering

### Backward Compatibility
- No config field renames — all changes are display-label only
- No new API endpoints — batch operations use existing per-job endpoints via `Promise.allSettled` (Web UI) / sequential calls (TUI)
- No database schema changes
- WebSocket message format unchanged

### Performance
- Batch operations: `Promise.allSettled` for concurrent per-job API calls (Web UI), sequential loop with error collection (TUI)
- Template preview: debounce keystroke handler at 100ms to avoid excessive re-renders
- Selection state: O(1) lookups via `Set` (Web UI) / `map[string]bool` (TUI)

### Accessibility
- Batch checkboxes: proper `aria-label` and keyboard focus
- Action bar: ARIA live region for screen reader announcements
- Countdown timer: `aria-live="polite"` for screen reader updates
- All new buttons: proper labels and focus indicators
