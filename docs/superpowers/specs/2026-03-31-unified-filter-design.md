# Unified Filter System

Single filter control per panel combining text search, status filtering, channel filtering, and platform filtering with booru-style tag syntax. Replaces the separate search input and status dropdown.

## Filter Syntax

Tokens are space-separated. All tokens intersect (AND) by default. Pipe `|` creates OR groups within a token.

| Token Type | Examples | Matching |
|---|---|---|
| Free text | `minecraft`, `jelly` | Substring match on title OR channel name (case-insensitive) |
| Status tag | `status:active`, `status:errors`, `status:finished` | Matches jobs in that status group via STATUS_FILTER_MAP |
| Channel tag | `channel:shachimu`, `channel:"shachi too"` | Exact channel name match (case-insensitive) |
| Platform tag | `platform:youtube`, `platform:twitch` | Exact platform match |
| Negation | `-jelly`, `-status:active`, `-channel:shachimu` | Excludes matching jobs (inverts the predicate) |
| OR group | `jelly\|shachi`, `status:active\|status:errors` | Union -- matches if ANY term in the group matches |

**Quoting**: values with spaces require quotes: `channel:"shachi too"`. Clicking a channel from the dropdown auto-quotes if needed.

**Combining example**: `status:active channel:"shachi too"|channel:shachimu minecraft`
= active status AND (shachi too OR shachimu) AND title/channel contains "minecraft"

**Negation example**: `-status:finished platform:youtube`
= NOT finished AND youtube platform

## Filter Input Control

Single unified control per panel replacing both the search input and status dropdown.

### Layout

A flex container styled to look like one input field (border, background match Shoelace input variables). Contains:

- **Chip area**: `sl-tag` elements for active structured tags, inline before the text input
- **Text input**: A plain `<input>` element (not `sl-input` -- needs to be inline without shadow DOM wrapper). Grows to fill remaining space. Placeholder "Filter..." when no chips and no text.
- **Clear button**: X icon at the right edge, visible when any chips or text exist.

Clicking anywhere in the container focuses the text input.

### Chips

When a structured tag is added (via dropdown click or typed `status:active` + Enter/space), it becomes a visual chip (`sl-tag`).

**Chip styling by type:**
- Status/channel/platform chips: default neutral color, text shows the display label (e.g. "Active", "Shachimu", "YouTube")
- Negated chips: danger/red variant to visually distinguish exclusions (e.g. red chip "-Finished")
- Free text: NOT chipped -- stays as plain text in the input. Only structured tags become chips.
- OR groups: single chip with "A | B" text (e.g. "Active | Errors")

**Chip actions:**
- Click X on a chip to remove it
- Backspace when text input is empty removes the last chip
- Each chip stores its raw token data (type, value, negate) so the full query can be reconstructed

### Dropdown

An optgroup-style dropdown that appears below the filter control.

**Opening**: appears when the text input is focused. Filters in real-time as the user types.

**Groups** (each with a header, hidden when all items are filtered out):
- **Statuses**: Active, Errors, Finished
- **Platforms**: YouTube, Twitch
- **Channels**: dynamically populated from current panel's jobs, sorted alphabetically

**Item layout**: each item has:
- Left side: label text
- Right side: small minus/exclude icon button

Clicking the label area adds the tag as a chip. Clicking the exclude icon adds the negated tag as a chip.

**Already-applied tags**: items that are already active as chips are visually dimmed or checkmarked. Clicking them again removes the chip (toggle behavior).

**Keyboard navigation**:
- Arrow Up/Down navigates items
- Enter selects the highlighted item
- Escape closes the dropdown
- Typing resumes filtering

**Closing**: dropdown closes when:
- An item is selected (tag added)
- User presses Escape
- Focus leaves the entire filter control
- Does NOT close on exclude-icon click (allows excluding multiple items quickly)

## Filter Engine

### Token Data Model

```js
// Single term
{ type: "text"|"status"|"channel"|"platform", value: string, negate: boolean }

// OR group
{ type: "or", terms: [{ type, value, negate }, ...] }
```

The filter control maintains an array of these token objects. Chips map 1:1 to structured tokens. Free text in the input is parsed into text tokens on each keystroke.

### Evaluation

Each token is a predicate. All tokens must pass (AND). Within an OR token, any term passing is sufficient.

- **text**: `(title OR channelName).toLowerCase().includes(value)` (substring)
- **status**: `STATUS_FILTER_MAP[value].includes(job.status)`
- **channel**: `channelName.toLowerCase() === value.toLowerCase()` (exact, case-insensitive)
- **platform**: `job.platform === value`
- **negate**: inverts the predicate result

Shared function: `_applyFilterTokens(jobs, tokens)` used by both `getFilteredJobs()` and `getFilteredArchivedJobs()`.

## Integration

### State Changes

Remove:
- `tasksSearchQuery`, `tasksStatusFilter`
- `archivedSearchQuery`, `archivedStatusFilter`
- `_setupChosenSelect`, `STATUS_OPTIONS`

Add:
- `tasksFilterTokens: []`, `archivedFilterTokens: []`
- `_tasksChannels: []`, `_archivedChannels: []` (populated in render methods)

### HTML Changes

Per panel, replace the entire filter bar contents (search input + status dropdown + filter count) with:
- Unified filter control div
- Filter count span (after the control)
- "Add video" button stays (Tasks panel only)

### JS Changes

- `_setupUnifiedFilter(containerId, opts)` -- wires up chip input, dropdown, and token parsing
- `_applyFilterTokens(jobs, tokens)` -- shared filter engine
- `getFilteredJobs()` / `getFilteredArchivedJobs()` -- call shared engine with panel's tokens
- `updateBatchActionBar()` Select All label -- check `tokens.length > 0`
- `renderJobs()` / `renderArchivedJobs()` -- update `_tasksChannels` / `_archivedChannels` arrays
- Keyboard shortcut "f" -- focuses the text input inside the unified control

### CSS Changes

- New styles for the chip container, inline input, dropdown panel with groups
- Remove old `.chosen-select-*` and `.tasks-filter-bar` specific styles
- Chip colors: neutral default, danger for negated
- Mobile: container wraps chips naturally, dropdown is full-width

## Files to Modify

- `web/public/index.html` -- replace filter bar markup in both panels
- `web/public/app.js` -- unified filter setup, token parsing, filter engine, remove old filter code
- `web/public/moombox.css` -- chip container, dropdown groups, chip styling, mobile
