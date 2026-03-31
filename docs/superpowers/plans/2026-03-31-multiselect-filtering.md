# Web UI Multiselect & Filtering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix multiselect bugs, add channel filtering, extend filtering and batch actions to the Archived panel, reposition the batch action bar to viewport bottom, and polish mobile UX.

**Architecture:** Three files change — `index.html` (DOM structure), `app.js` (logic), `moombox.css` (styling). Selection state splits into per-panel sets. The batch bar moves outside tab panels and becomes `position: fixed` above the status bar. A new `getFilteredArchivedJobs()` mirrors the existing `getFilteredJobs()`. Channel filter is a third `sl-select` auto-populated from job data.

**Tech Stack:** Vanilla JS, Shoelace v2.16 web components, CSS

---

### Task 1: Split Selection State Into Per-Panel Sets

**Files:**
- Modify: `web/public/app.js:27-28` (constructor)
- Modify: `web/public/app.js:411-444` (setupJobContainer click handler)
- Modify: `web/public/app.js:1291` (renderJobItem selection check)
- Modify: `web/public/app.js:3342-3346` (_getSelectedJobs)
- Modify: `web/public/app.js:3348-3382` (updateBatchActionBar)

This task replaces the single `_selectedJobs` Set with two per-panel sets and adds a helper to get the active set.

- [ ] **Step 1: Add per-panel selection sets and helper**

In the constructor (`app.js:27-28`), replace:

```js
this._selectedJobs = new Set();
this._lastCheckedJobIds = { jobs: null, archived: null };
```

with:

```js
this._selectedTaskJobs = new Set();
this._selectedArchivedJobs = new Set();
this._lastCheckedJobIds = { jobs: null, archived: null };
```

Add a helper method to the class (after the constructor):

```js
/** Return the selection set for the currently active panel. */
_activeSelectionSet() {
  const panel = document.querySelector("sl-tab-panel[active]")?.getAttribute("name");
  return panel === "archived" ? this._selectedArchivedJobs : this._selectedTaskJobs;
}

/** Return the selection set for a given container key ("jobs" or "archived"). */
_selectionSetFor(containerKey) {
  return containerKey === "archived" ? this._selectedArchivedJobs : this._selectedTaskJobs;
}
```

- [ ] **Step 2: Update setupJobContainer click handler**

In the click handler (`app.js:411-444`), the closure references `this._selectedJobs`. Replace every occurrence inside `setupJobContainer` with `this._selectionSetFor(containerKey)`:

Replace the block at lines 418-442:

```js
const checkbox = e.target.closest(".job-checkbox");
if (checkbox) {
  e.stopPropagation();
  const jobId = checkbox.dataset.jobId;
  const selectionSet = this._selectionSetFor(containerKey);
  // Shift+Click: select range between last checked and current (within same container)
  const lastId = this._lastCheckedJobIds[containerKey];
  if (e.shiftKey && lastId && checkbox.checked) {
    const allCards = [...container.querySelectorAll(".video-item")];
    const lastIdx = allCards.findIndex(c => c.dataset.jobId === lastId);
    const curIdx = allCards.findIndex(c => c.dataset.jobId === jobId);
    if (lastIdx !== -1 && curIdx !== -1) {
      const [start, end] = lastIdx < curIdx ? [lastIdx, curIdx] : [curIdx, lastIdx];
      for (let i = start; i <= end; i++) {
        const id = allCards[i].dataset.jobId;
        selectionSet.add(id);
        allCards[i].classList.add("selected");
        const cb = allCards[i].querySelector(".job-checkbox");
        if (cb) cb.checked = true;
      }
    }
  } else if (checkbox.checked) {
    selectionSet.add(jobId);
  } else {
    selectionSet.delete(jobId);
  }
  this._lastCheckedJobIds[containerKey] = jobId;
  checkbox.closest(".video-item")?.classList.toggle("selected", checkbox.checked);
  this.updateBatchActionBar();
  return;
}
```

- [ ] **Step 3: Update renderJobItem selection check**

In `renderJobItem()` (`app.js:1291`), replace:

```js
const isSelected = this._selectedJobs.has(job.id);
```

with:

```js
const isSelected = this._selectedTaskJobs.has(job.id) || this._selectedArchivedJobs.has(job.id);
```

- [ ] **Step 4: Update _getSelectedJobs to be panel-aware**

Replace `_getSelectedJobs()` (`app.js:3342-3346`):

```js
/** Return selected jobs for the currently active panel. */
_getSelectedJobs() {
  const panel = document.querySelector("sl-tab-panel[active]")?.getAttribute("name");
  if (panel === "archived") {
    return this.archivedJobs.filter(j => this._selectedArchivedJobs.has(j.id));
  }
  return this.jobs.filter(j => this._selectedTaskJobs.has(j.id));
}
```

- [ ] **Step 5: Update updateBatchActionBar count**

In `updateBatchActionBar()` (`app.js:3352`), replace:

```js
const count = this._selectedJobs.size;
```

with:

```js
const count = this._activeSelectionSet().size;
```

- [ ] **Step 6: Update stale selection cleanup in renderJobs**

In `renderJobs()` (`app.js:1171-1175`), replace:

```js
// Remove stale selected IDs (jobs that no longer exist in either list)
const currentJobIds = new Set([...this.jobs, ...this.archivedJobs].map(j => j.id));
this._selectedJobs.forEach(id => {
  if (!currentJobIds.has(id)) this._selectedJobs.delete(id);
});
```

with:

```js
// Remove stale selected IDs (jobs that no longer exist)
const taskJobIds = new Set(this.jobs.map(j => j.id));
this._selectedTaskJobs.forEach(id => {
  if (!taskJobIds.has(id)) this._selectedTaskJobs.delete(id);
});
const archivedJobIds = new Set(this.archivedJobs.map(j => j.id));
this._selectedArchivedJobs.forEach(id => {
  if (!archivedJobIds.has(id)) this._selectedArchivedJobs.delete(id);
});
```

- [ ] **Step 7: Update selection re-apply in renderJobs**

In `renderJobs()` (`app.js:1177-1185`), replace:

```js
// Re-apply selection state and sync the batch bar
this._selectedJobs.forEach(id => {
  const card = container.querySelector(`[data-job-id="${CSS.escape(id)}"]`);
  if (card) {
    card.classList.add("selected");
    const cb = card.querySelector(".job-checkbox");
    if (cb) cb.checked = true;
  }
});
```

with:

```js
// Re-apply selection state and sync the batch bar
this._selectedTaskJobs.forEach(id => {
  const card = container.querySelector(`[data-job-id="${CSS.escape(id)}"]`);
  if (card) {
    card.classList.add("selected");
    const cb = card.querySelector(".job-checkbox");
    if (cb) cb.checked = true;
  }
});
```

- [ ] **Step 8: Update batch action completion clear**

In `batchAction()` (`app.js:3456`), replace:

```js
this._selectedJobs.clear();
```

with:

```js
this._activeSelectionSet().clear();
```

- [ ] **Step 9: Verify build**

Run: `go build ./...`
Expected: Build succeeds (embedded files updated on next `go build` after all HTML/JS/CSS changes).

- [ ] **Step 10: Commit**

```bash
git add web/public/app.js
git commit -m "refactor: split selection state into per-panel sets"
```

---

### Task 2: Fix Select All, Clear All, and Escape Key

**Files:**
- Modify: `web/public/app.js:504-516` (batch-select-all and batch-clear handlers)
- Modify: `web/public/app.js:2746-2754` (Escape key handler)

- [ ] **Step 1: Fix Select All to respect filters and active panel**

Replace the `batch-select-all` handler (`app.js:504-510`):

```js
document.getElementById("batch-select-all")?.addEventListener("click", () => {
  const panel = document.querySelector("sl-tab-panel[active]")?.getAttribute("name");
  const selectionSet = this._activeSelectionSet();
  let visibleJobs;
  if (panel === "archived") {
    visibleJobs = this.getFilteredArchivedJobs();
  } else {
    visibleJobs = this.getFilteredJobs();
  }
  visibleJobs.forEach(j => selectionSet.add(j.id));
  // Only update checkboxes in the active panel's container
  const containerId = panel === "archived" ? "archived-container" : "jobs-container";
  const container = document.getElementById(containerId);
  if (container) {
    container.querySelectorAll(".job-checkbox").forEach(cb => { cb.checked = true; });
    container.querySelectorAll(".video-item").forEach(el => el.classList.add("selected"));
  }
  this.updateBatchActionBar();
});
```

Note: `getFilteredArchivedJobs()` does not exist yet — it will be created in Task 4. This handler will work correctly once that method exists. Until then, build will still succeed since JS is not statically checked.

- [ ] **Step 2: Fix Clear All to be panel-aware**

Replace the `batch-clear` handler (`app.js:511-516`):

```js
document.getElementById("batch-clear")?.addEventListener("click", () => {
  const panel = document.querySelector("sl-tab-panel[active]")?.getAttribute("name");
  this._activeSelectionSet().clear();
  const containerId = panel === "archived" ? "archived-container" : "jobs-container";
  const container = document.getElementById(containerId);
  if (container) {
    container.querySelectorAll(".job-checkbox").forEach(cb => { cb.checked = false; });
    container.querySelectorAll(".video-item.selected").forEach(el => el.classList.remove("selected"));
  }
  this.updateBatchActionBar();
});
```

- [ ] **Step 3: Fix Escape key to be panel-aware**

Replace the Escape handler (`app.js:2746-2754`):

```js
case "Escape":
  if (this._activeSelectionSet().size > 0) {
    const panel = document.querySelector("sl-tab-panel[active]")?.getAttribute("name");
    this._activeSelectionSet().clear();
    const containerId = panel === "archived" ? "archived-container" : "jobs-container";
    const container = document.getElementById(containerId);
    if (container) {
      container.querySelectorAll(".job-checkbox").forEach(cb => { cb.checked = false; });
      container.querySelectorAll(".video-item.selected").forEach(el => el.classList.remove("selected"));
    }
    this.updateBatchActionBar();
    return;
  }
  break;
```

- [ ] **Step 4: Commit**

```bash
git add web/public/app.js
git commit -m "fix: Select All respects filters, Clear/Escape are panel-aware"
```

---

### Task 3: Move Batch Action Bar to Fixed Viewport Bottom

**Files:**
- Modify: `web/public/index.html:223-248` (move batch bar outside tab panels)
- Modify: `web/public/moombox.css:2349-2365` (batch bar positioning)
- Modify: `web/public/app.js:261` (update batch bar on tab switch)

- [ ] **Step 1: Move batch bar in HTML**

In `index.html`, remove the entire `#batch-action-bar` div from its current location (lines 223-248, inside the Tasks tab panel) and place it just before the status bar (before line 1665):

```html
        <!-- Batch Action Bar (fixed, panel-aware) -->
        <div id="batch-action-bar" style="display: none;">
            <span id="batch-count"></span>
            <sl-button id="batch-select-all" variant="text" size="small">Select All</sl-button>
            <div class="batch-actions">
                <sl-button id="batch-cancel" variant="warning" size="small">
                    <sl-icon slot="prefix" name="x-circle"></sl-icon> Cancel
                </sl-button>
                <sl-button id="batch-resume" variant="success" size="small">
                    <sl-icon slot="prefix" name="play-fill"></sl-icon> Resume
                </sl-button>
                <sl-button id="batch-reinit" variant="primary" size="small">
                    <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon> Reinitialize
                </sl-button>
                <sl-button id="batch-delete" variant="danger" size="small">
                    <sl-icon slot="prefix" name="trash"></sl-icon> Delete
                </sl-button>
                <sl-divider vertical></sl-divider>
                <sl-button id="batch-watched" variant="success" size="small">
                    <sl-icon slot="prefix" name="eye"></sl-icon> Mark Watched
                </sl-button>
                <sl-button id="batch-unwatched" variant="neutral" size="small">
                    <sl-icon slot="prefix" name="eye-slash"></sl-icon> Mark Unwatched
                </sl-button>
            </div>
            <sl-icon-button id="batch-clear" name="x-lg" label="Clear selection"></sl-icon-button>
        </div>

        <!-- Status Bar -->
```

- [ ] **Step 2: Update batch bar CSS**

Replace the batch selection CSS (`moombox.css:2349-2365`):

```css
/* ===== Batch Selection ===== */
#batch-action-bar {
    position: fixed;
    bottom: 28px; /* sits above the 28px status bar */
    left: 0;
    right: 0;
    background: var(--sl-color-neutral-100);
    border-top: 1px solid var(--sl-color-neutral-300);
    padding: var(--sl-spacing-small) var(--sl-spacing-medium);
    display: flex;
    align-items: center;
    gap: var(--sl-spacing-medium);
    z-index: 99; /* below status bar (100), above content */
    transform: translateY(100%);
    transition: transform 0.15s ease-out;
}

#batch-action-bar.visible {
    transform: translateY(0);
}

.batch-actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sl-spacing-x-small);
}
```

- [ ] **Step 3: Update updateBatchActionBar to use CSS class for animation**

In `updateBatchActionBar()` (`app.js:3353-3357`), replace the show/hide logic:

```js
const count = this._activeSelectionSet().size;
if (count === 0) {
  bar.classList.remove("visible");
  // Reset display after transition
  bar.addEventListener("transitionend", () => {
    if (!bar.classList.contains("visible")) bar.style.display = "none";
  }, { once: true });
  return;
}
bar.style.display = "";
// Force reflow before adding class so the transition plays
requestAnimationFrame(() => bar.classList.add("visible"));
```

- [ ] **Step 4: Update batch bar on tab switch**

In the `sl-tab-show` handler (`app.js:261`), add at the end of the handler (before the closing `});` at line 285):

```js
// Refresh batch action bar for the newly active panel
this.updateBatchActionBar();
```

- [ ] **Step 5: Add body padding for batch bar when visible**

Add CSS rule to increase body bottom padding when the batch bar is visible. Since CSS can't conditionally pad based on sibling visibility, handle this in JS. In `updateBatchActionBar()`, after the show logic:

```js
document.body.style.paddingBottom = count > 0 ? "68px" : "28px"; // 28px status + ~40px batch bar
```

And in the hide path (count === 0), inside the transitionend listener:

```js
bar.addEventListener("transitionend", () => {
  if (!bar.classList.contains("visible")) {
    bar.style.display = "none";
    document.body.style.paddingBottom = "28px";
  }
}, { once: true });
```

- [ ] **Step 6: Commit**

```bash
git add web/public/index.html web/public/app.js web/public/moombox.css
git commit -m "feat: reposition batch bar to fixed viewport bottom with slide animation"
```

---

### Task 4: Add Archived Panel Filter Bar and getFilteredArchivedJobs

**Files:**
- Modify: `web/public/index.html:261-280` (archived panel HTML)
- Modify: `web/public/app.js:38-39` (constructor — add archived filter state)
- Modify: `web/public/app.js:355-374` (setupEventListeners — add archived filter listeners)
- Modify: `web/public/app.js:1204-1229` (renderArchivedJobs — use filtered results)
- Modify: `web/public/app.js:2820-2846` (add getFilteredArchivedJobs method)

- [ ] **Step 1: Add archived filter state to constructor**

After `this.tasksStatusFilter = "";` (`app.js:39`), add:

```js
this.archivedSearchQuery = "";
this.archivedStatusFilter = "";
```

- [ ] **Step 2: Add archived filter bar HTML**

In `index.html`, replace the archived panel content (lines 261-280):

```html
            <!-- Archived Panel -->
            <sl-tab-panel name="archived">
                <div class="panel-header tasks-filter-bar">
                    <sl-input id="archived-search" placeholder="Filter by title or channel..." clearable size="small" style="flex:1;min-width:180px">
                        <sl-icon slot="prefix" name="search"></sl-icon>
                    </sl-input>
                    <sl-select id="archived-status-filter" placeholder="All statuses" clearable size="small" style="min-width:140px">
                        <sl-option value="active">Active</sl-option>
                        <sl-option value="errors">Errors</sl-option>
                        <sl-option value="finished">Finished</sl-option>
                    </sl-select>
                    <span id="archived-filter-count" style="display:none"></span>
                </div>

                <div id="archived-table" class="archived-table">
                    <div id="archived-table-header">
                        <div>Thumbnail</div>
                        <div>Video</div>
                        <div>Status</div>
                        <div>Progress</div>
                        <div></div>
                    </div>
                    <div id="archived-container">
                        <!-- Archived jobs will be rendered here -->
                    </div>
                </div>

                <div id="archived-empty-state" style="display: none">
                    <sl-icon name="archive"></sl-icon>
                    <p>No archived jobs</p>
                    <p class="empty-state-subtext">Finished jobs older than the configured age will appear here automatically</p>
                </div>
            </sl-tab-panel>
```

- [ ] **Step 3: Add archived filter event listeners**

After the tasks filter listeners (`app.js:374`), add:

```js
// Archived search/filter
let archivedSearchTimeout = null;
const archivedSearch = document.getElementById("archived-search");
if (archivedSearch) {
  archivedSearch.addEventListener("sl-input", () => {
    clearTimeout(archivedSearchTimeout);
    archivedSearchTimeout = setTimeout(() => {
      this.archivedSearchQuery = archivedSearch.value.trim();
      this.renderArchivedJobs();
    }, 200);
  });
}

const archivedStatusFilter = document.getElementById("archived-status-filter");
if (archivedStatusFilter) {
  archivedStatusFilter.addEventListener("sl-change", () => {
    this.archivedStatusFilter = archivedStatusFilter.value || "";
    this.renderArchivedJobs();
  });
}
```

- [ ] **Step 4: Add getFilteredArchivedJobs method**

After `getFilteredJobs()` (`app.js:2846`), add:

```js
getFilteredArchivedJobs() {
  let jobs = this.archivedJobs;

  // Text filter
  if (this.archivedSearchQuery) {
    const query = this.archivedSearchQuery.toLowerCase();
    jobs = jobs.filter((j) =>
      (j.title || "").toLowerCase().includes(query) ||
      (j.channelName || "").toLowerCase().includes(query)
    );
  }

  // Status filter
  if (this.archivedStatusFilter) {
    const statusMap = {
      active: ["Downloading", "Live", "Upcoming", "Muxing"],
      errors: ["Error", "COOKIES?"],
      finished: ["Finished", "Cancelled"],
    };
    const allowed = statusMap[this.archivedStatusFilter];
    if (allowed) {
      jobs = jobs.filter((j) => allowed.includes(j.status));
    }
  }

  return jobs;
}
```

- [ ] **Step 5: Update renderArchivedJobs to use filtering**

Replace `renderArchivedJobs()` (`app.js:1204-1229`):

```js
renderArchivedJobs() {
  const container = document.getElementById("archived-container");
  const emptyState = document.getElementById("archived-empty-state");
  const table = document.getElementById("archived-table");
  const filterCount = document.getElementById("archived-filter-count");

  if (this.archivedJobs.length === 0) {
    container.innerHTML = "";
    table.style.display = "none";
    emptyState.style.display = "flex";
    const icon = emptyState.querySelector("sl-icon");
    if (icon) icon.name = "archive";
    const msg = emptyState.querySelector("p");
    if (msg) msg.textContent = "No archived jobs";
    const subtext = emptyState.querySelector(".empty-state-subtext");
    if (subtext) subtext.textContent = "Finished jobs older than the configured age will appear here automatically";
    if (filterCount) filterCount.style.display = "none";
    return;
  }

  const filtered = this.getFilteredArchivedJobs();
  const isFiltered = this.archivedSearchQuery || this.archivedStatusFilter;

  // Update filter count
  if (filterCount) {
    if (isFiltered) {
      filterCount.textContent = `${filtered.length} of ${this.archivedJobs.length}`;
      filterCount.style.display = "";
    } else {
      filterCount.style.display = "none";
    }
  }

  if (filtered.length === 0 && isFiltered) {
    container.innerHTML = "";
    table.style.display = "none";
    emptyState.style.display = "flex";
    const icon = emptyState.querySelector("sl-icon");
    if (icon) icon.name = "search";
    const msg = emptyState.querySelector("p");
    if (msg) msg.textContent = "No matching archived jobs";
    const subtext = emptyState.querySelector(".empty-state-subtext");
    if (subtext) subtext.textContent = "Search matches titles and channel names";
    if (filterCount) filterCount.style.display = "";
    return;
  }

  table.style.display = "";
  emptyState.style.display = "none";

  // Sort archived jobs by updatedAt descending (most recent first)
  const sorted = [...filtered].sort(
    (a, b) => new Date(b.updatedAt) - new Date(a.updatedAt)
  );

  container.innerHTML = sorted
    .map((job) => this.renderJobItem(job))
    .join("");

  // Re-apply archived selection state
  this._selectedArchivedJobs.forEach(id => {
    const card = container.querySelector(`[data-job-id="${CSS.escape(id)}"]`);
    if (card) {
      card.classList.add("selected");
      const cb = card.querySelector(".job-checkbox");
      if (cb) cb.checked = true;
    }
  });
  this.updateBatchActionBar();
}
```

- [ ] **Step 6: Commit**

```bash
git add web/public/index.html web/public/app.js
git commit -m "feat: add filtering and filter count to Archived panel"
```

---

### Task 5: Add Channel Filter Dropdown

**Files:**
- Modify: `web/public/index.html:52-59` (tasks filter bar — add channel select)
- Modify: `web/public/index.html` (archived filter bar — add channel select)
- Modify: `web/public/app.js:38-39` (constructor — add channel filter state)
- Modify: `web/public/app.js:355-374` (setupEventListeners — add channel filter listeners)
- Modify: `web/public/app.js:2820-2846` (getFilteredJobs — add channel stage)
- Modify: `web/public/app.js` (getFilteredArchivedJobs — add channel stage)
- Modify: `web/public/app.js:1051` (renderJobs — populate channel dropdown)
- Modify: `web/public/app.js` (renderArchivedJobs — populate channel dropdown)

- [ ] **Step 1: Add channel filter state to constructor**

After `this.archivedStatusFilter = "";` (added in Task 4), add:

```js
this.tasksChannelFilter = "";
this.archivedChannelFilter = "";
```

- [ ] **Step 2: Add channel dropdowns to HTML**

In the Tasks filter bar (`index.html:55-59`), add the channel select between the search input and status filter. Replace lines 55-59:

```html
                    <sl-select id="tasks-channel-filter" placeholder="All channels" clearable size="small" style="min-width:140px">
                    </sl-select>
                    <sl-select id="tasks-status-filter" placeholder="All statuses" clearable size="small" style="min-width:140px">
                        <sl-option value="active">Active</sl-option>
                        <sl-option value="errors">Errors</sl-option>
                        <sl-option value="finished">Finished</sl-option>
                    </sl-select>
```

In the Archived filter bar (added in Task 4), add the channel select between the search input and status filter:

```html
                    <sl-select id="archived-channel-filter" placeholder="All channels" clearable size="small" style="min-width:140px">
                    </sl-select>
                    <sl-select id="archived-status-filter" placeholder="All statuses" clearable size="small" style="min-width:140px">
                        <sl-option value="active">Active</sl-option>
                        <sl-option value="errors">Errors</sl-option>
                        <sl-option value="finished">Finished</sl-option>
                    </sl-select>
```

- [ ] **Step 3: Add channel filter event listeners**

After the tasks status filter listener (`app.js:374`), add:

```js
const tasksChannelFilter = document.getElementById("tasks-channel-filter");
if (tasksChannelFilter) {
  tasksChannelFilter.addEventListener("sl-change", () => {
    this.tasksChannelFilter = tasksChannelFilter.value || "";
    this.renderJobs();
  });
}
```

After the archived status filter listener (added in Task 4), add:

```js
const archivedChannelFilter = document.getElementById("archived-channel-filter");
if (archivedChannelFilter) {
  archivedChannelFilter.addEventListener("sl-change", () => {
    this.archivedChannelFilter = archivedChannelFilter.value || "";
    this.renderArchivedJobs();
  });
}
```

- [ ] **Step 4: Add channel filter stage to getFilteredJobs**

After the status filter block in `getFilteredJobs()` (`app.js:2843`), add:

```js
// Channel filter
if (this.tasksChannelFilter) {
  jobs = jobs.filter((j) => (j.channelName || "") === this.tasksChannelFilter);
}
```

- [ ] **Step 5: Add channel filter stage to getFilteredArchivedJobs**

After the status filter block in `getFilteredArchivedJobs()` (added in Task 4), add:

```js
// Channel filter
if (this.archivedChannelFilter) {
  jobs = jobs.filter((j) => (j.channelName || "") === this.archivedChannelFilter);
}
```

- [ ] **Step 6: Add helper to populate channel dropdown options**

Add this method to the class:

```js
/** Populate a channel filter dropdown with unique channel names from a job list. */
_populateChannelDropdown(selectId, jobs) {
  const select = document.getElementById(selectId);
  if (!select) return;
  const channels = [...new Set(jobs.map(j => j.channelName).filter(Boolean))].sort(
    (a, b) => a.localeCompare(b, undefined, { sensitivity: "base" })
  );
  const currentValue = select.value;
  select.innerHTML = channels
    .map(ch => `<sl-option value="${this.escapeHtml(ch)}">${this.escapeHtml(ch)}</sl-option>`)
    .join("");
  // Restore selection if the channel still exists
  if (currentValue && channels.includes(currentValue)) {
    select.value = currentValue;
  }
}
```

- [ ] **Step 7: Call channel dropdown population in renderJobs**

At the start of `renderJobs()` (after the skeleton removal at `app.js:1053`), add:

```js
this._populateChannelDropdown("tasks-channel-filter", this.jobs);
```

- [ ] **Step 8: Call channel dropdown population in renderArchivedJobs**

At the start of `renderArchivedJobs()` (after getting the container/emptyState/table refs), add:

```js
this._populateChannelDropdown("archived-channel-filter", this.archivedJobs);
```

- [ ] **Step 9: Update filter count and isFiltered checks**

In `renderJobs()`, update the `isFiltered` check (`app.js:1121`):

```js
const isFiltered = this.tasksSearchQuery || this.tasksStatusFilter || this.tasksChannelFilter;
```

In `renderArchivedJobs()`, update the `isFiltered` check:

```js
const isFiltered = this.archivedSearchQuery || this.archivedStatusFilter || this.archivedChannelFilter;
```

- [ ] **Step 10: Update keyboard "f" shortcut to also work on Archived**

In the keyboard shortcuts (`app.js:2785-2791`), replace:

```js
case "f": {
  if (isTasksActive) {
    const searchInput = document.getElementById("tasks-search");
    if (searchInput) { searchInput.focus(); e.preventDefault(); }
  }
  break;
}
```

with:

```js
case "f": {
  const panel = activePanel?.getAttribute("name");
  if (panel === "tasks") {
    const searchInput = document.getElementById("tasks-search");
    if (searchInput) { searchInput.focus(); e.preventDefault(); }
  } else if (panel === "archived") {
    const searchInput = document.getElementById("archived-search");
    if (searchInput) { searchInput.focus(); e.preventDefault(); }
  }
  break;
}
```

- [ ] **Step 11: Commit**

```bash
git add web/public/index.html web/public/app.js
git commit -m "feat: add channel filter dropdown to Tasks and Archived panels"
```

---

### Task 6: Selection Highlight and Mobile Polish

**Files:**
- Modify: `web/public/moombox.css:2367-2387` (selection styling)
- Modify: `web/public/moombox.css` (mobile media query section)

- [ ] **Step 1: Add left border accent to selected items**

Replace the selection highlight CSS (`moombox.css:2379-2381`):

```css
.video-item.selected {
    background: var(--sl-color-primary-50);
    border-left: 3px solid var(--sl-color-primary-500);
}
```

- [ ] **Step 2: Add mobile batch bar styles**

Find the `@media (max-width: 768px)` section containing the `.video-item .job-checkbox` rule (`moombox.css:2383-2387`) and replace it with:

```css
@media (max-width: 768px) {
    .video-item .job-checkbox {
        opacity: 1;
    }

    #batch-action-bar {
        flex-wrap: wrap;
        padding: var(--sl-spacing-x-small) var(--sl-spacing-small);
        gap: var(--sl-spacing-x-small);
    }

    .batch-actions {
        flex-wrap: wrap;
        gap: var(--sl-spacing-2x-small);
    }

    .batch-actions sl-button::part(base) {
        min-height: 44px;
    }

    #batch-count {
        font-size: 0.8rem;
    }
}
```

- [ ] **Step 3: Commit**

```bash
git add web/public/moombox.css
git commit -m "feat: selection left-border accent and mobile batch bar polish"
```

---

### Task 7: Update Batch Bar on Tab Switch and Final Integration

**Files:**
- Modify: `web/public/app.js:261-285` (sl-tab-show handler)
- Modify: `web/public/app.js` (renderArchivedJobs stale cleanup)

- [ ] **Step 1: Add stale archived selection cleanup to renderArchivedJobs**

In `renderArchivedJobs()` (the version from Task 4), before the selection re-apply block, add:

```js
// Remove stale archived selected IDs
const archivedJobIds = new Set(this.archivedJobs.map(j => j.id));
this._selectedArchivedJobs.forEach(id => {
  if (!archivedJobIds.has(id)) this._selectedArchivedJobs.delete(id);
});
```

Note: This is only needed in `renderArchivedJobs()` because the Task 1 cleanup in `renderJobs()` already handles `_selectedArchivedJobs` when job lists refresh. But `renderArchivedJobs` runs independently on tab switch, so we duplicate the cleanup here for the archived set.

Actually — Task 1 Step 6 already handles `_selectedArchivedJobs` cleanup inside `renderJobs()`. We should remove that from `renderJobs()` and put each panel's cleanup in its own render function. So in `renderJobs()`, keep only the `_selectedTaskJobs` cleanup:

```js
// Remove stale selected IDs (jobs that no longer exist)
const taskJobIds = new Set(this.jobs.map(j => j.id));
this._selectedTaskJobs.forEach(id => {
  if (!taskJobIds.has(id)) this._selectedTaskJobs.delete(id);
});
```

(The `_selectedArchivedJobs` cleanup should be removed from `renderJobs()` since it belongs in `renderArchivedJobs()`.)

- [ ] **Step 2: Verify the sl-tab-show handler updates batch bar**

Confirm that the line added in Task 3 Step 4 (`this.updateBatchActionBar();`) is present at the end of the `sl-tab-show` handler. This ensures that switching between Tasks and Archived shows/hides the batch bar based on that panel's selection set.

- [ ] **Step 3: Test the full flow manually**

Open the Moombox web UI and verify:

1. **Tasks panel:** Select some jobs → batch bar slides up from bottom of viewport
2. **Filter by status:** Selected items persist, Select All only selects visible
3. **Channel filter:** Dropdown populated, filters correctly, stacks with other filters
4. **Switch to Archived tab:** Batch bar hides (no archived selections yet)
5. **Archived panel:** Search, status filter, channel filter all work
6. **Archived selection:** Select items → batch bar appears, actions work
7. **Switch back to Tasks:** Original selections still there, batch bar shows
8. **Escape key:** Clears selections on whichever panel is active
9. **Mobile:** Batch bar wraps, buttons have 44px touch targets
10. **Selection highlight:** Blue background + left blue border accent

- [ ] **Step 4: Build and verify**

Run: `go build -o moombox.exe ./cmd/moombox`
Expected: Build succeeds with all embedded assets updated.

- [ ] **Step 5: Commit**

```bash
git add web/public/app.js
git commit -m "fix: archived stale selection cleanup and tab-switch batch bar sync"
```
