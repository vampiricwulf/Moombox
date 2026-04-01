# Unified Filter System Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the separate search input and status dropdown with a single unified filter control featuring booru-style tag syntax, visual chips, and an optgroup dropdown.

**Architecture:** A chip-input control (plain `<input>` + `sl-tag` chips in a styled container) with an `sl-dropdown` showing grouped options (Statuses, Platforms, Channels). A token parser converts typed text and chips into a filter token array. A shared filter engine evaluates tokens against jobs. Both Tasks and Archived panels get independent instances.

**Tech Stack:** Vanilla JS, Shoelace v2.16 (`sl-tag`, `sl-dropdown`, `sl-menu`, `sl-menu-item`), CSS custom properties

---

### Task 1: Token Parser

**Files:**
- Create: `web/public/modules/filter-parser.js`

The parser converts a raw query string into an array of token objects. This is a pure function with no DOM dependencies.

- [ ] **Step 1: Create the parser module**

Create `web/public/modules/filter-parser.js`:

```js
/**
 * Parse a filter query string into token objects.
 *
 * Syntax:
 *   - Space-separated tokens are AND (intersection)
 *   - Pipe | within a token is OR (union)
 *   - Prefix - negates a token
 *   - Namespace prefix type:value for structured filters
 *   - Quotes for values with spaces: channel:"shachi too"
 *
 * Token types:
 *   { type: "text"|"status"|"channel"|"platform", value: string, negate: boolean }
 *   { type: "or", terms: [{ type, value, negate }, ...] }
 */

const NAMESPACES = new Set(["status", "channel", "platform"]);

/**
 * Parse a single term (no pipes) into a token object.
 * @param {string} raw - e.g. "status:active", "-jelly", "channel:\"shachi too\""
 * @returns {{ type: string, value: string, negate: boolean }}
 */
function parseTerm(raw) {
  let negate = false;
  let s = raw;
  if (s.startsWith("-")) {
    negate = true;
    s = s.slice(1);
  }
  const colonIdx = s.indexOf(":");
  if (colonIdx > 0) {
    const ns = s.slice(0, colonIdx).toLowerCase();
    if (NAMESPACES.has(ns)) {
      let value = s.slice(colonIdx + 1);
      // Strip surrounding quotes
      if ((value.startsWith('"') && value.endsWith('"')) ||
          (value.startsWith("'") && value.endsWith("'"))) {
        value = value.slice(1, -1);
      }
      return { type: ns, value, negate };
    }
  }
  return { type: "text", value: s, negate };
}

/**
 * Tokenize a raw query string, respecting quotes.
 * Returns array of raw string tokens split on unquoted spaces.
 */
function tokenize(query) {
  const tokens = [];
  let current = "";
  let inQuote = null;
  for (let i = 0; i < query.length; i++) {
    const ch = query[i];
    if (inQuote) {
      current += ch;
      if (ch === inQuote) inQuote = null;
    } else if (ch === '"' || ch === "'") {
      inQuote = ch;
      current += ch;
    } else if (ch === " ") {
      if (current) tokens.push(current);
      current = "";
    } else {
      current += ch;
    }
  }
  if (current) tokens.push(current);
  return tokens;
}

/**
 * Parse a full filter query string into an array of token objects.
 * @param {string} query
 * @returns {Array<{ type: string, value: string, negate: boolean } | { type: "or", terms: Array }>}
 */
export function parseFilterQuery(query) {
  if (!query || !query.trim()) return [];
  const rawTokens = tokenize(query.trim());
  return rawTokens.map(raw => {
    // Check for pipe (OR) — but not inside quotes
    if (raw.includes("|") && !raw.includes('"') && !raw.includes("'")) {
      const parts = raw.split("|").filter(Boolean);
      if (parts.length > 1) {
        return { type: "or", terms: parts.map(p => parseTerm(p)) };
      }
    }
    return parseTerm(raw);
  });
}

/**
 * Serialize a token back to query string form.
 * @param {{ type: string, value: string, negate: boolean } | { type: "or", terms: Array }} token
 * @returns {string}
 */
export function serializeToken(token) {
  if (token.type === "or") {
    return token.terms.map(t => serializeToken(t)).join("|");
  }
  const prefix = token.negate ? "-" : "";
  if (token.type === "text") {
    return `${prefix}${token.value}`;
  }
  const needsQuotes = token.value.includes(" ");
  const val = needsQuotes ? `"${token.value}"` : token.value;
  return `${prefix}${token.type}:${val}`;
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./...`
Expected: Build succeeds (new JS file is auto-embedded).

- [ ] **Step 3: Commit**

```bash
git add web/public/modules/filter-parser.js
git commit -m "feat: add filter token parser with booru-style syntax support"
```

---

### Task 2: Filter Engine

**Files:**
- Create: `web/public/modules/filter-engine.js`

The engine evaluates parsed tokens against job objects. Pure function, no DOM.

- [ ] **Step 1: Create the filter engine module**

Create `web/public/modules/filter-engine.js`:

```js
/**
 * Filter engine — evaluates parsed tokens against job objects.
 * All tokens AND (intersect). OR tokens are unions within.
 */

const STATUS_FILTER_MAP = {
  active: ["Downloading", "Live", "Upcoming", "Muxing"],
  errors: ["Error", "COOKIES?"],
  finished: ["Finished", "Cancelled"],
};

/**
 * Test whether a single term matches a job.
 * @param {{ type: string, value: string, negate: boolean }} term
 * @param {object} job
 * @returns {boolean}
 */
function matchTerm(term, job) {
  let result;
  switch (term.type) {
    case "text": {
      const val = term.value.toLowerCase();
      result = (job.title || "").toLowerCase().includes(val) ||
               (job.channelName || "").toLowerCase().includes(val);
      break;
    }
    case "status": {
      const allowed = STATUS_FILTER_MAP[term.value];
      result = allowed ? allowed.includes(job.status) : false;
      break;
    }
    case "channel":
      result = (job.channelName || "").toLowerCase() === term.value.toLowerCase();
      break;
    case "platform":
      result = (job.platform || "") === term.value;
      break;
    default:
      result = true;
  }
  return term.negate ? !result : result;
}

/**
 * Test whether a token (possibly an OR group) matches a job.
 * @param {object} token
 * @param {object} job
 * @returns {boolean}
 */
function matchToken(token, job) {
  if (token.type === "or") {
    return token.terms.some(t => matchTerm(t, job));
  }
  return matchTerm(token, job);
}

/**
 * Filter a list of jobs using parsed tokens. All tokens must match (AND).
 * @param {object[]} jobs
 * @param {Array} tokens - parsed token array from parseFilterQuery
 * @returns {object[]}
 */
export function applyFilterTokens(jobs, tokens) {
  if (!tokens || tokens.length === 0) return jobs;
  return jobs.filter(job => tokens.every(token => matchToken(token, job)));
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./...`

- [ ] **Step 3: Commit**

```bash
git add web/public/modules/filter-engine.js
git commit -m "feat: add filter engine for token-based job filtering"
```

---

### Task 3: Unified Filter Control — HTML & CSS

**Files:**
- Modify: `web/public/index.html:50-73` (Tasks filter bar)
- Modify: `web/public/index.html:242-261` (Archived filter bar)
- Modify: `web/public/moombox.css:2117-2201` (filter bar styles)

Replace both panels' filter bars with the unified control, and replace the Chosen-style CSS with the chip-input CSS.

- [ ] **Step 1: Replace Tasks panel filter bar HTML**

In `index.html`, replace the Tasks panel filter bar (lines 51-73) — everything inside the `<div class="panel-header tasks-filter-bar">` — with:

```html
                <div class="panel-header tasks-filter-bar">
                    <div id="tasks-filter" class="unified-filter">
                        <div class="unified-filter-chips"></div>
                        <input class="unified-filter-input" placeholder="Filter..." autocomplete="off" aria-label="Filter jobs">
                        <sl-icon class="unified-filter-clear" name="x-circle-fill" style="display:none" title="Clear all filters"></sl-icon>
                        <sl-dropdown class="unified-filter-dropdown" hoist>
                            <button slot="trigger" class="unified-filter-dropdown-trigger" aria-hidden="true" tabindex="-1"></button>
                            <div class="unified-filter-panel">
                                <sl-menu class="unified-filter-menu"></sl-menu>
                            </div>
                        </sl-dropdown>
                    </div>
                    <span id="tasks-filter-count" style="display:none"></span>
                    <sl-button variant="primary" id="add-video-btn">
                        <sl-icon slot="prefix" name="plus"></sl-icon>
                        Add video
                    </sl-button>
                </div>
```

Note: The `sl-dropdown` has a hidden trigger button (zero-size, aria-hidden). The JS will call `dropdown.show()`/`dropdown.hide()` programmatically when the input is focused. This avoids the trigger needing to be the input itself (which caused issues before).

- [ ] **Step 2: Replace Archived panel filter bar HTML**

In `index.html`, replace the Archived panel filter bar (lines 243-261) with:

```html
                <div class="panel-header tasks-filter-bar">
                    <div id="archived-filter" class="unified-filter">
                        <div class="unified-filter-chips"></div>
                        <input class="unified-filter-input" placeholder="Filter..." autocomplete="off" aria-label="Filter archived jobs">
                        <sl-icon class="unified-filter-clear" name="x-circle-fill" style="display:none" title="Clear all filters"></sl-icon>
                        <sl-dropdown class="unified-filter-dropdown" hoist>
                            <button slot="trigger" class="unified-filter-dropdown-trigger" aria-hidden="true" tabindex="-1"></button>
                            <div class="unified-filter-panel">
                                <sl-menu class="unified-filter-menu"></sl-menu>
                            </div>
                        </sl-dropdown>
                    </div>
                    <span id="archived-filter-count" style="display:none"></span>
                </div>
```

- [ ] **Step 3: Replace filter bar CSS**

In `moombox.css`, replace the entire block from `/* Tasks search/filter bar */` through `.chosen-select-menu` (lines 2117-2196) with:

```css
/* Tasks search/filter bar */
.tasks-filter-bar {
    display: flex;
    gap: var(--sl-spacing-small);
    align-items: center;
    flex-wrap: wrap;
}

/* ===== Unified Filter Control ===== */
.unified-filter {
    flex: 1;
    min-width: 180px;
    display: flex;
    align-items: center;
    gap: var(--sl-spacing-2x-small);
    padding: 0 var(--sl-spacing-small);
    min-height: var(--sl-input-height-small);
    border: solid var(--sl-input-border-width) var(--sl-input-border-color);
    border-radius: var(--sl-input-border-radius-small);
    background: var(--sl-input-background-color);
    cursor: text;
    flex-wrap: wrap;
    position: relative;
}

.unified-filter:focus-within {
    border-color: var(--sl-input-border-color-focus);
    box-shadow: 0 0 0 var(--sl-focus-ring-width) var(--sl-input-focus-ring-color);
}

.unified-filter-chips {
    display: contents;
}

.unified-filter-chips sl-tag {
    cursor: default;
    max-width: 200px;
}

.unified-filter-chips sl-tag::part(base) {
    padding: 0 var(--sl-spacing-2x-small);
    height: 1.4rem;
}

.unified-filter-input {
    all: unset;
    flex: 1;
    min-width: 60px;
    font-size: var(--sl-input-font-size-small);
    font-family: var(--sl-input-font-family);
    color: var(--sl-input-color);
    padding: var(--sl-spacing-2x-small) 0;
}

.unified-filter-input::placeholder {
    color: var(--sl-input-placeholder-color);
}

.unified-filter-clear {
    font-size: 0.85rem;
    color: var(--sl-input-icon-color);
    cursor: pointer;
    flex-shrink: 0;
}

.unified-filter-clear:hover {
    color: var(--sl-color-neutral-700);
}

/* Hidden trigger — dropdown is opened programmatically */
.unified-filter-dropdown-trigger {
    all: unset;
    position: absolute;
    width: 0;
    height: 0;
    overflow: hidden;
}

.unified-filter-panel {
    min-width: 220px;
}

.unified-filter-menu {
    max-height: 280px;
    overflow: auto;
}

.unified-filter-menu sl-menu-item[data-group-header] {
    font-weight: var(--sl-font-weight-semibold);
    font-size: var(--sl-font-size-x-small);
    color: var(--sl-color-neutral-500);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    pointer-events: none;
    padding-top: var(--sl-spacing-x-small);
}

.unified-filter-menu sl-menu-item[data-group-header]:first-child {
    padding-top: 0;
}

.unified-filter-menu .filter-item-exclude {
    font-size: 0.7rem;
    color: var(--sl-color-neutral-400);
    cursor: pointer;
    margin-left: auto;
    padding: 2px 4px;
}

.unified-filter-menu .filter-item-exclude:hover {
    color: var(--sl-color-danger-600);
}

.unified-filter-menu sl-menu-item.already-active {
    opacity: 0.5;
}
```

- [ ] **Step 4: Update mobile CSS**

In the `@media (max-width: 768px)` block, replace the old filter bar rules:

```css
    .tasks-filter-bar sl-input,
    .tasks-filter-bar sl-select,
    .tasks-filter-bar .chosen-select,
    .tasks-filter-bar .chosen-select-trigger {
        min-width: 0 !important;
    }
```

with:

```css
    .unified-filter {
        min-width: 0 !important;
    }
```

- [ ] **Step 5: Verify build**

Run: `go build ./...`

- [ ] **Step 6: Commit**

```bash
git add web/public/index.html web/public/moombox.css
git commit -m "feat: unified filter control HTML and CSS"
```

---

### Task 4: Unified Filter Control — JavaScript Setup

**Files:**
- Modify: `web/public/app.js` — imports, constructor, setupEventListeners, new `_setupUnifiedFilter` method

This wires up the chip input, dropdown, and connects to the parser/engine. The biggest task.

- [ ] **Step 1: Add imports**

At the top of `app.js`, after the existing imports (line 10), add:

```js
import { parseFilterQuery, serializeToken } from "./modules/filter-parser.js";
import { applyFilterTokens } from "./modules/filter-engine.js";
```

- [ ] **Step 2: Replace filter state in constructor**

In the constructor (lines 44-47), replace:

```js
this.tasksSearchQuery = "";
this.tasksStatusFilter = "";
this.archivedSearchQuery = "";
this.archivedStatusFilter = "";
```

with:

```js
this.tasksFilterTokens = [];
this.archivedFilterTokens = [];
this._tasksChannels = [];
this._archivedChannels = [];
```

- [ ] **Step 3: Replace filter event listeners in setupEventListeners**

Replace the entire tasks search/filter and archived search/filter blocks (lines 376-420 approximately — from `// Tasks search/filter` through the archived `_setupChosenSelect` call) with:

```js
    // Unified filter controls
    this._setupUnifiedFilter("tasks-filter", {
      getTokens: () => this.tasksFilterTokens,
      setTokens: (tokens) => { this.tasksFilterTokens = tokens; this.renderJobs(); },
      getChannels: () => this._tasksChannels,
    });

    this._setupUnifiedFilter("archived-filter", {
      getTokens: () => this.archivedFilterTokens,
      setTokens: (tokens) => { this.archivedFilterTokens = tokens; this.renderArchivedJobs(); },
      getChannels: () => this._archivedChannels,
    });
```

- [ ] **Step 4: Add `_setupUnifiedFilter` method**

Add this method to the MoomboxApp class (replace the old `_setupChosenSelect` method):

```js
  /**
   * Set up a unified filter control with chip input and optgroup dropdown.
   * @param {string} containerId - ID of the .unified-filter container
   * @param {object} opts
   * @param {() => Array} opts.getTokens - returns current token array
   * @param {(tokens: Array) => void} opts.setTokens - apply new tokens and re-render
   * @param {() => string[]} opts.getChannels - returns current channel list for dropdown
   */
  _setupUnifiedFilter(containerId, { getTokens, setTokens, getChannels }) {
    const container = document.getElementById(containerId);
    if (!container) return;
    const chipsEl = container.querySelector(".unified-filter-chips");
    const input = container.querySelector(".unified-filter-input");
    const clearBtn = container.querySelector(".unified-filter-clear");
    const dropdown = container.querySelector(".unified-filter-dropdown");
    const menu = container.querySelector(".unified-filter-menu");
    if (!chipsEl || !input || !dropdown || !menu) return;

    const STATUS_OPTIONS = [
      { type: "status", value: "active", label: "Active" },
      { type: "status", value: "errors", label: "Errors" },
      { type: "status", value: "finished", label: "Finished" },
    ];
    const PLATFORM_OPTIONS = [
      { type: "platform", value: "youtube", label: "YouTube" },
      { type: "platform", value: "twitch", label: "Twitch" },
    ];

    /** Render chips from current structured tokens (not free-text). */
    const renderChips = () => {
      const tokens = getTokens();
      chipsEl.innerHTML = "";
      for (const token of tokens) {
        if (token.type === "text") continue; // text stays in input, not chipped
        const tag = document.createElement("sl-tag");
        tag.size = "small";
        tag.removable = true;
        if (token.type === "or") {
          tag.textContent = token.terms.map(t => {
            const prefix = t.negate ? "-" : "";
            return prefix + this._filterTokenLabel(t);
          }).join(" | ");
          tag.variant = token.terms.some(t => t.negate) ? "danger" : "neutral";
        } else {
          const prefix = token.negate ? "-" : "";
          tag.textContent = prefix + this._filterTokenLabel(token);
          tag.variant = token.negate ? "danger" : "neutral";
        }
        tag.addEventListener("sl-remove", () => {
          const updated = getTokens().filter(t => t !== token);
          setTokens(updated);
          renderChips();
          updateClearBtn();
          renderDropdownItems();
        });
        chipsEl.appendChild(tag);
      }
    };

    /** Get the free-text portion from current tokens. */
    const getFreeText = () => {
      return getTokens().filter(t => t.type === "text").map(t => serializeToken(t)).join(" ");
    };

    /**
     * Sync tokens from chips + current input text.
     * Parses the input text — structured tokens (status:, channel:, platform:)
     * become chips and are removed from the input. Free text stays in the input.
     */
    const syncTokens = () => {
      const chipTokens = getTokens().filter(t => t.type !== "text");
      const inputText = input.value.trim();
      if (!inputText) {
        setTokens(chipTokens);
        updateClearBtn();
        renderDropdownItems();
        return;
      }
      const parsed = parseFilterQuery(inputText);
      const newChips = [];
      const remainingText = [];
      for (const t of parsed) {
        if (t.type === "text") {
          remainingText.push(t);
        } else if (t.type === "or") {
          // OR groups with any structured term become chips; pure text ORs stay
          const hasStructured = t.terms.some(term => term.type !== "text");
          if (hasStructured) {
            newChips.push(t);
          } else {
            remainingText.push(t);
          }
        } else {
          newChips.push(t);
        }
      }
      const allTokens = [...chipTokens, ...newChips, ...remainingText];
      setTokens(allTokens);
      // Update input to show only remaining free text
      if (newChips.length > 0) {
        input.value = remainingText.map(t => serializeToken(t)).join(" ");
        renderChips();
      }
      updateClearBtn();
      renderDropdownItems();
    };

    const updateClearBtn = () => {
      const hasContent = getTokens().length > 0 || input.value.trim();
      clearBtn.style.display = hasContent ? "" : "none";
    };

    /** Add a structured token as a chip. */
    const addChipToken = (token) => {
      const tokens = getTokens().filter(t => t.type !== "text");
      const textTokens = getTokens().filter(t => t.type === "text");
      // Check if already exists
      const exists = tokens.some(t =>
        t.type === token.type && t.value === token.value && t.negate === token.negate
      );
      if (exists) {
        // Toggle off — remove it
        const updated = tokens.filter(t =>
          !(t.type === token.type && t.value === token.value && t.negate === token.negate)
        );
        setTokens([...updated, ...textTokens]);
      } else {
        // Also remove any opposite negate version
        const cleaned = tokens.filter(t =>
          !(t.type === token.type && t.value === token.value)
        );
        setTokens([...cleaned, token, ...textTokens]);
      }
      renderChips();
      updateClearBtn();
      renderDropdownItems();
    };

    /** Render the optgroup dropdown items. */
    const renderDropdownItems = () => {
      const query = input.value.trim().toLowerCase();
      const activeTokens = getTokens();
      let html = "";

      const groups = [
        { header: "Statuses", items: STATUS_OPTIONS },
        { header: "Platforms", items: PLATFORM_OPTIONS },
        { header: "Channels", items: getChannels().map(ch => ({ type: "channel", value: ch, label: ch })) },
      ];

      for (const group of groups) {
        const filtered = query
          ? group.items.filter(o => o.label.toLowerCase().includes(query))
          : group.items;
        if (filtered.length === 0) continue;

        html += `<sl-menu-item data-group-header disabled>${this.escapeHtml(group.header)}</sl-menu-item>`;
        for (const opt of filtered) {
          const isActive = activeTokens.some(t =>
            t.type === opt.type && t.value === opt.value && !t.negate
          );
          const isExcluded = activeTokens.some(t =>
            t.type === opt.type && t.value === opt.value && t.negate
          );
          const cls = (isActive || isExcluded) ? ' class="already-active"' : "";
          const val = this.escapeHtml(JSON.stringify({ type: opt.type, value: opt.value }));
          html += `<sl-menu-item value='${val}'${cls}>`;
          html += this.escapeHtml(opt.label);
          html += `<sl-icon slot="suffix" class="filter-item-exclude" name="dash-circle" data-exclude='${val}' title="Exclude"></sl-icon>`;
          html += `</sl-menu-item>`;
        }
      }

      if (!html) {
        html = `<sl-menu-item disabled>No matches</sl-menu-item>`;
      }
      menu.innerHTML = html;
    };

    // --- Event Wiring ---

    // Clicking container focuses input
    container.addEventListener("click", (e) => {
      if (e.target.closest("sl-tag") || e.target.closest(".unified-filter-clear")) return;
      input.focus();
    });

    // Input focus opens dropdown
    input.addEventListener("focus", () => {
      renderDropdownItems();
      dropdown.show();
    });

    // Input typing: debounced filter update + dropdown filtering
    let filterTimeout = null;
    input.addEventListener("input", () => {
      clearTimeout(filterTimeout);
      renderDropdownItems();
      filterTimeout = setTimeout(() => syncTokens(), 200);
    });

    // Enter key: if input has structured tags, chip them immediately
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        clearTimeout(filterTimeout);
        syncTokens();
      }
      if (e.key === "Backspace" && !input.value) {
        // Remove last chip
        const tokens = getTokens();
        const chipTokens = tokens.filter(t => t.type !== "text");
        if (chipTokens.length > 0) {
          const last = chipTokens[chipTokens.length - 1];
          const updated = tokens.filter(t => t !== last);
          setTokens(updated);
          renderChips();
          updateClearBtn();
          renderDropdownItems();
        }
      }
      if (e.key === "Escape") {
        dropdown.hide();
        input.blur();
      }
    });

    // Stop keyboard shortcut propagation while typing in the filter
    input.addEventListener("keydown", (e) => {
      e.stopPropagation();
    });

    // Dropdown item clicked — add as chip
    dropdown.addEventListener("sl-select", (e) => {
      const raw = e.detail.item.value;
      if (!raw) return;
      try {
        const { type, value } = JSON.parse(raw);
        addChipToken({ type, value, negate: false });
      } catch {}
      input.focus();
    });

    // Exclude icon clicked — add negated chip
    menu.addEventListener("click", (e) => {
      const excludeIcon = e.target.closest(".filter-item-exclude");
      if (!excludeIcon) return;
      e.stopPropagation(); // prevent sl-select from firing
      try {
        const { type, value } = JSON.parse(excludeIcon.dataset.exclude);
        addChipToken({ type, value, negate: true });
      } catch {}
      input.focus();
    });

    // Clear all
    clearBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      input.value = "";
      setTokens([]);
      renderChips();
      updateClearBtn();
    });

    // Close dropdown when focus leaves filter entirely
    container.addEventListener("focusout", (e) => {
      // Check if new focus target is still within the container or dropdown
      setTimeout(() => {
        if (!container.contains(document.activeElement) &&
            !dropdown.contains(document.activeElement)) {
          dropdown.hide();
        }
      }, 100);
    });

    // Initial render
    renderChips();
    updateClearBtn();
  }

  /** Get display label for a filter token. */
  _filterTokenLabel(token) {
    if (token.type === "text") return token.value;
    if (token.type === "status") {
      const labels = { active: "Active", errors: "Errors", finished: "Finished" };
      return labels[token.value] || token.value;
    }
    if (token.type === "platform") {
      return token.value === "youtube" ? "YouTube" : token.value === "twitch" ? "Twitch" : token.value;
    }
    if (token.type === "channel") return token.value;
    return token.value;
  }
```

- [ ] **Step 5: Remove old `_setupChosenSelect` method**

Delete the entire `_setupChosenSelect` method (approximately lines 3007-3081).

- [ ] **Step 6: Verify build**

Run: `go build ./...`

- [ ] **Step 7: Commit**

```bash
git add web/public/app.js
git commit -m "feat: unified filter control JS setup with chip input and dropdown"
```

---

### Task 5: Wire Filter Engine into Render Methods

**Files:**
- Modify: `web/public/app.js` — `getFilteredJobs`, `getFilteredArchivedJobs`, `renderJobs`, `renderArchivedJobs`, `updateBatchActionBar`

Connect the new token-based filter to the existing render pipeline.

- [ ] **Step 1: Replace getFilteredJobs and getFilteredArchivedJobs**

Replace both methods:

```js
getFilteredJobs() {
  return applyFilterTokens(this.jobs, this.tasksFilterTokens);
}

getFilteredArchivedJobs() {
  return applyFilterTokens(this.archivedJobs, this.archivedFilterTokens);
}
```

- [ ] **Step 2: Remove STATUS_FILTER_MAP from app.js top-level**

Delete the `STATUS_FILTER_MAP` constant at the top of `app.js` (around line 18). It now lives in `filter-engine.js`.

- [ ] **Step 3: Update isFiltered checks in renderJobs**

In `renderJobs()`, replace:

```js
const isFiltered = this.tasksSearchQuery || this.tasksStatusFilter;
```

with:

```js
const isFiltered = this.tasksFilterTokens.length > 0;
```

- [ ] **Step 4: Update isFiltered checks in renderArchivedJobs**

In `renderArchivedJobs()`, replace:

```js
const isFiltered = this.archivedSearchQuery || this.archivedStatusFilter;
```

with:

```js
const isFiltered = this.archivedFilterTokens.length > 0;
```

- [ ] **Step 5: Update channel list population in renderJobs**

In `renderJobs()`, after the skeleton removal, add:

```js
this._tasksChannels = [...new Set(this.jobs.map(j => j.channelName).filter(Boolean))].sort(
  (a, b) => a.localeCompare(b, undefined, { sensitivity: "base" })
);
```

- [ ] **Step 6: Update channel list population in renderArchivedJobs**

In `renderArchivedJobs()`, early in the method (after getting container refs), add:

```js
this._archivedChannels = [...new Set(this.archivedJobs.map(j => j.channelName).filter(Boolean))].sort(
  (a, b) => a.localeCompare(b, undefined, { sensitivity: "base" })
);
```

- [ ] **Step 7: Update Select All label in updateBatchActionBar**

Replace the `isFiltered` check in `updateBatchActionBar()`:

```js
const isFiltered = panel === "archived"
  ? (this.archivedSearchQuery || this.archivedStatusFilter)
  : (this.tasksSearchQuery || this.tasksStatusFilter);
```

with:

```js
const isFiltered = panel === "archived"
  ? this.archivedFilterTokens.length > 0
  : this.tasksFilterTokens.length > 0;
```

- [ ] **Step 8: Update keyboard "f" shortcut**

In the keyboard shortcuts handler, the "f" case focuses the search input. Update to focus the unified filter input:

```js
case "f": {
  const panel = activePanel?.getAttribute("name");
  const filterId = panel === "archived" ? "archived-filter" : "tasks-filter";
  const filterInput = document.querySelector(`#${filterId} .unified-filter-input`);
  if (filterInput) { filterInput.focus(); e.preventDefault(); }
  break;
}
```

- [ ] **Step 9: Verify build**

Run: `go build ./...`

- [ ] **Step 10: Commit**

```bash
git add web/public/app.js
git commit -m "feat: wire unified filter engine into render pipeline"
```

---

### Task 6: Cleanup and Final Integration

**Files:**
- Modify: `web/public/app.js` — remove dead code
- Modify: `web/public/moombox.css` — remove dead CSS

- [ ] **Step 1: Remove dead filter-related code from app.js**

Search for and remove any remaining references to:
- `tasksSearchQuery`, `tasksStatusFilter`, `archivedSearchQuery`, `archivedStatusFilter`
- `_setupChosenSelect` (method and calls)
- `STATUS_OPTIONS` (local variable in setupEventListeners, if still present)
- Old search debounce listeners (`tasksSearchTimeout`, `archivedSearchTimeout`)
- Old `#tasks-search`, `#archived-search`, `#tasks-status-dropdown`, `#archived-status-dropdown` getElementById calls

- [ ] **Step 2: Remove dead CSS**

Remove any remaining `.chosen-select-*` rules from `moombox.css` if they weren't already replaced in Task 3.

- [ ] **Step 3: Verify no orphaned references**

Search the three files for: `tasksSearchQuery`, `tasksStatusFilter`, `archivedSearchQuery`, `archivedStatusFilter`, `chosen-select`, `tasks-search`, `archived-search`, `tasks-status-dropdown`, `archived-status-dropdown`. All should return zero results.

- [ ] **Step 4: Verify build and tests**

Run: `go build ./... && go test ./...`

- [ ] **Step 5: Commit**

```bash
git add web/public/app.js web/public/moombox.css
git commit -m "chore: remove dead filter code and CSS from previous implementation"
```
