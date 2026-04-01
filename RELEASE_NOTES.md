### Features

- Unified filter system with booru-style tag syntax — single filter input replaces separate search and status dropdown
  - Visual chips for structured filters (status, channel, platform), free text for title/channel search
  - Optgroup dropdown with Statuses, Platforms, and Channels sections (channels auto-populated from jobs)
  - Negation with `-` prefix (e.g. `-status:finished`, `-jelly`)
  - OR groups with `|` pipe (e.g. `jelly|shachi`, `status:active|status:errors`)
  - Quoted values for names with spaces: `channel:"shachi too"`
  - Exclude button per dropdown item for quick negation
  - Backspace removes last chip, Escape closes dropdown, "f" focuses filter
  - Both Tasks and Archived panels have independent filter instances

### Improvements

- Per-panel multiselect — Tasks and Archived track selections independently
- Batch action bar fixed to viewport bottom with slide-up animation
- Select All respects active filters, shows count when filtered
- Selection left-border accent, mobile batch bar polish (flex-wrap, 44px touch targets)
- ARIA labels on filter controls, job checkboxes, and batch action bar

### Bug Fixes

- Fix Select All selecting all jobs regardless of active filters
- Fix layout shift when selecting/deselecting jobs
