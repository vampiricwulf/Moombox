### Features

- Add channel filter dropdown to Tasks and Archived panels — auto-populated from job data, stacks with existing search and status filters
- Add full filtering and multiselect to Archived panel — search, status filter, channel filter, batch actions, filter count display
- Reposition batch action bar to fixed viewport bottom with slide-up animation — always visible regardless of scroll position

### Improvements

- Select All now respects active filters — only selects visible/filtered jobs, shows count when filtered
- Per-panel selection state — Tasks and Archived panels track selections independently
- Escape key and Clear All are panel-aware — only affect the active panel's selections
- Selection left-border accent for stronger visual feedback when scrolling
- Mobile batch bar polish — flex-wrap, 44px touch targets, compact layout
- Skip redundant channel dropdown rebuild when channel list hasn't changed
- Add ARIA labels to filter controls, job checkboxes, and batch action bar
- Batch action bar announces selection changes to screen readers via live region
- Extract STATUS_FILTER_MAP constant — single source of truth for filter status groups

### Bug Fixes

- Fix Select All selecting all jobs regardless of active filters
- Fix layout shift when selecting/deselecting jobs (transparent border-left on base state)
- Fix batch bar animation race condition on rapid selection toggling
