/**
 * Filter Bar Controller — the unified filter bar for the Tasks and Archived tabs
 */
import { parseFilterQuery, serializeToken } from "./filter-parser.js";
import { applyFilterTokens } from "./filter-engine.js";

export class FilterBarController {
  constructor(app) {
    this.app = app;
    this.tasksFilterTokens = [];
    this.archivedFilterTokens = [];
    this._tasksChannels = [];
    this._archivedChannels = [];
  }

  /** Wire up both filter bars (Tasks and Archived tabs). */
  bind() {
    this._setupUnifiedFilter("tasks-filter", {
      getTokens: () => this.tasksFilterTokens,
      setTokens: (tokens) => { this.tasksFilterTokens = tokens; this.app.renderJobs(); },
      getChannels: () => this._tasksChannels,
    });

    this._setupUnifiedFilter("archived-filter", {
      getTokens: () => this.archivedFilterTokens,
      setTokens: (tokens) => { this.archivedFilterTokens = tokens; this.app.renderArchivedJobs(); },
      getChannels: () => this._archivedChannels,
    });
  }

  /** Current filter tokens for a container key ("jobs" or "archived"). */
  tokens(containerKey) {
    return containerKey === "archived" ? this.archivedFilterTokens : this.tasksFilterTokens;
  }

  /** Recompute the channel dropdown list for a container key, from its current job set. */
  refreshChannels(containerKey, jobs) {
    const list = [...new Set(jobs.map(j => j.channelName).filter(Boolean))].sort(
      (a, b) => a.localeCompare(b, undefined, { sensitivity: "base" })
    );
    if (containerKey === "archived") this._archivedChannels = list;
    else this._tasksChannels = list;
  }

  getFilteredJobs() {
    return applyFilterTokens(this.app.jobs, this.tasksFilterTokens);
  }

  getFilteredArchivedJobs() {
    return applyFilterTokens(this.app.archivedJobs, this.archivedFilterTokens);
  }

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
      { type: "status", value: "issues", label: "Issues" },
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

        html += `<sl-menu-item data-group-header disabled>${this.app.escapeHtml(group.header)}</sl-menu-item>`;
        for (const opt of filtered) {
          const isActive = activeTokens.some(t =>
            t.type === opt.type && t.value === opt.value && !t.negate
          );
          const isExcluded = activeTokens.some(t =>
            t.type === opt.type && t.value === opt.value && t.negate
          );
          const cls = (isActive || isExcluded) ? ' class="already-active"' : "";
          const val = this.app.escapeHtml(JSON.stringify({ type: opt.type, value: opt.value }));
          html += `<sl-menu-item value='${val}'${cls}>`;
          html += this.app.escapeHtml(opt.label);
          // A span around the icon, not role/tabindex on the sl-icon: sl-icon
          // re-asserts aria-hidden and strips any role on its host at first
          // render. The name says WHICH option, since every item carries one.
          html += `<span slot="suffix" class="filter-item-exclude" role="button" tabindex="0" title="Exclude" aria-label="Exclude ${this.app.escapeHtml(opt.label)}" data-exclude='${val}'><sl-icon name="dash-circle"></sl-icon></span>`;
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

    // Single keydown handler — do local input behaviour AND stop propagation
    // so global shortcuts never see keys while the filter is focused.
    // Previously there were two keydown handlers and the listener-ordering
    // dependency between them was implicit; one combined handler makes the
    // ordering explicit and impossible to break by future listener shuffling.
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        clearTimeout(filterTimeout);
        syncTokens();
      } else if (e.key === "Backspace" && !input.value) {
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
      } else if (e.key === "Escape") {
        dropdown.hide();
        input.blur();
      }
      // Always stop propagation so app-level shortcuts don't fire for
      // ordinary typing in this input.
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

    // Exclude control — click or Enter/Space: add a negated chip. One
    // `exclude` for both input paths (the rule the status bar's controls
    // set): a keyboard copy would drift from the click.
    const exclude = (target) => {
      const excludeIcon = target.closest(".filter-item-exclude");
      if (!excludeIcon) return false;
      try {
        const { type, value } = JSON.parse(excludeIcon.dataset.exclude);
        addChipToken({ type, value, negate: true });
      } catch {}
      input.focus();
      return true;
    };
    menu.addEventListener("click", (e) => {
      if (!e.target.closest(".filter-item-exclude")) return;
      e.stopPropagation(); // prevent sl-select from firing
      exclude(e.target);
    });
    // Capture phase, and stopPropagation, on purpose. sl-menu's own keydown
    // handler lives on the <slot> inside its shadow root — between the item
    // and this host in the event path — and on Enter/Space it clicks the
    // CURRENT item (the one with tabindex="0"), i.e. a select, then stops
    // propagation. A bubbling listener here would run after it or not at
    // all; only a capture listener on the host runs before it, and stopping
    // the event there is what keeps the select from firing as well.
    menu.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" && e.key !== " ") return;
      if (!e.target.closest?.(".filter-item-exclude")) return;
      e.preventDefault();
      e.stopPropagation();
      exclude(e.target);
    }, true);

    // Clear all — click or Enter/Space, one `clearAll` for both. The control
    // hides itself once there is nothing left to clear, so the keyboard path
    // hands focus to the input rather than letting it fall to <body>.
    const clearAll = () => {
      input.value = "";
      setTokens([]);
      renderChips();
      updateClearBtn();
    };
    clearBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      clearAll();
    });
    clearBtn.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" && e.key !== " ") return;
      e.preventDefault();
      clearAll();
      input.focus();
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
      const labels = { active: "Active", issues: "Issues", errors: "Issues", finished: "Finished" };
      return labels[token.value] || token.value;
    }
    if (token.type === "platform") {
      return token.value === "youtube" ? "YouTube" : token.value === "twitch" ? "Twitch" : token.value;
    }
    if (token.type === "channel") return token.value;
    return token.value;
  }
}
