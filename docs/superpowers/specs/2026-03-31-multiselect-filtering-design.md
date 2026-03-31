# Web UI Multiselect & Filtering Improvements

Comprehensive pass over the Web UI's multiselect and filtering features: fix bugs, add missing capabilities, extend to Archived panel, and polish the experience.

## 1. Bug Fixes

### Select All Respects Filters

The `batch-select-all` handler currently adds every job from `this.jobs` and `this.archivedJobs` regardless of active filters. Change it to select only the filtered/visible jobs for the active panel:

- **Tasks panel active:** use `getFilteredJobs()` results
- **Archived panel active:** use `getFilteredArchivedJobs()` results

### Selections Persist Across Filter Changes

Current behavior is kept: manually selected items stay selected even when filtered away. The batch count may show more than what's visible on screen — this is intentional since the user explicitly chose those items.

### Stale Selection Cleanup

Keep existing logic that removes IDs for deleted/gone jobs from the selection sets. No additional trimming on filter changes.

## 2. Batch Action Bar Repositioning

### Fixed to Viewport Bottom

Change the batch action bar from `position: sticky` (bottom of job list) to `position: fixed` at the viewport bottom, just above the status bar. Uses `left: 0; right: 0;` like the status bar.

### Single DOM Element, Panel-Aware

Keep one `#batch-action-bar` in the DOM, moved outside the tab panels to be a sibling of the status bar. Its visibility and content are driven by whichever tab is active — switching to a tab with no selections hides the bar.

### Separate Selection Sets

Split `_selectedJobs` into two sets:

- `_selectedTaskJobs` — tracks Tasks panel selections
- `_selectedArchivedJobs` — tracks Archived panel selections

`updateBatchActionBar()` reads from whichever set corresponds to the active tab. This prevents cross-panel confusion (e.g., selecting tasks then switching to Archived and accidentally operating on tasks).

### Z-Index Layering

Batch bar above content, below dialogs/overlays. Status bar remains at the very bottom.

## 3. Archived Panel Parity

### Filter Bar

Add the same `panel-header tasks-filter-bar` structure to the Archived panel:

- Text search input (searches title + channel name)
- Status filter dropdown (same options: Active/Errors/Finished)
- Channel filter dropdown (see Section 4)
- Filter count display ("X of Y")

### Separate Filter State

Each panel maintains independent filter state:

- `archivedSearchQuery`, `archivedStatusFilter`, `archivedChannelFilter`
- Switching tabs does not reset the other panel's filters

### `getFilteredArchivedJobs()`

New method mirroring `getFilteredJobs()`, applying the archived panel's three filters to `this.archivedJobs`.

### Selection

`_selectedArchivedJobs` gives the Archived panel its own checkboxes, shift-click range selection, and batch bar behavior. The existing `setupJobContainer()` event delegation for `archived-container` already handles click events.

### Empty State

Same "No matching jobs" treatment when filters produce zero results, matching the Tasks panel pattern.

## 4. Channel Filter

### New Dropdown

A new `sl-select` in each panel's filter bar, placed between text search and status filter. Placeholder: "All channels". Clearable, small size.

### Auto-Populated Options

On each render, collect unique `channelName` values from the panel's job list (active or archived), sort alphabetically, and populate the `sl-option` elements. Options update dynamically as jobs arrive/depart via WebSocket.

### Filter Logic

Added as a third filter stage in `getFilteredJobs()` / `getFilteredArchivedJobs()` — after text search and status filter, filter by exact channel name match.

### Interaction with Text Search

Filters stack. Text search matches title + channel name (fuzzy), channel dropdown matches exact channel (precise). If both are set, both must match. Some overlap is expected and intuitive.

### State

`tasksChannelFilter` and `archivedChannelFilter` strings, empty string when cleared.

## 5. Polish & Mobile

### Batch Bar Animation

CSS slide-up transition: `transform: translateY(100%)` to `translateY(0)` with 150ms ease-out. Smooth appearance rather than abrupt pop-in.

### Selection Highlight

Keep `var(--sl-color-primary-50)` background. Add a subtle left border accent (`3px solid var(--sl-color-primary-500)`) on `.video-item.selected` for stronger visual signal when scrolling.

### Mobile Multiselect

- Checkboxes already always visible on mobile (no change)
- Batch bar buttons: `flex-wrap` so they wrap naturally on narrow screens; horizontal scroll as fallback if wrapping gets too tall
- Batch bar buttons: minimum 44px tap target height
- Batch count and clear button pinned to edges, actions centered

### Escape Key

Extend to clear whichever panel's selection is active (Tasks or Archived), not just Tasks.

### Filter Bar Mobile

New channel dropdown follows existing flex-wrap pattern. On very narrow screens, controls stack vertically with `flex: 1 1 100%`.

## Files to Modify

- `web/public/index.html` — move batch bar outside tab panels, add archived filter bar + channel dropdowns
- `web/public/app.js` — split selection sets, add `getFilteredArchivedJobs()`, channel filter logic, panel-aware batch bar, Select All fix, archived filter event listeners
- `web/public/moombox.css` — batch bar `position: fixed`, slide-up animation, selection left-border accent, mobile batch bar wrapping
