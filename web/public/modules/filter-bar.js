/**
 * Filter Bar Controller — the unified filter bar for the Tasks and Archived tabs
 */
import { openToken, parseFilterQuery, serializeToken } from "./filter-parser.js";
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

    // The tokens parsed from the input box, as opposed to its chips. Every
    // sync re-parses them from the box's text, so a token the box no longer
    // holds never outlives it. They are remembered by identity because type
    // cannot tell them apart: a text-only OR group (alpha|beta) is not of
    // type "text", and telling chips by `type !== "text"` kept it as an
    // invisible filter after the box was cleared and AND-ed it with its own
    // edit; the token still being typed and a half-typed `status:` stay in
    // the box although they are structured.
    const typed = new WeakSet();

    /** Whether a token is a chip — anything not parsed from the box. */
    const isChip = (t) => !typed.has(t);

    /**
     * Whether a token parsed from the box may become a chip: a structured
     * term with a value, or an OR group with a structured term and no
     * valueless one. `status:` with nothing after the colon yet is half
     * typed, never a chip.
     */
    const chippable = (t) => {
      const halfTyped = (term) => term.type !== "text" && term.value === "";
      if (t.type === "text") return false;
      if (t.type === "or") return t.terms.some(term => term.type !== "text") && !t.terms.some(halfTyped);
      return !halfTyped(t);
    };

    /** Render chips from current structured tokens (not the box's own). */
    const renderChips = () => {
      const tokens = getTokens();
      chipsEl.innerHTML = "";
      for (const token of tokens) {
        if (!isChip(token)) continue; // the box's tokens stay in the box
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
     * The whole box applies at once, as the TUI's / box does, but on the
     * debounce (`commit` false) the token still being typed stays in the box
     * as typed: a pause after `status:` committed an empty chip, cleared the
     * box, and the `live` typed next became a text term. Enter commits it.
     *
     * The token being typed is the caret's, not the box's last: typing
     * `status:` in front of a word already in the box made `status:karaoke`,
     * closed by the space that was there before, and the pause chipped it.
     * Everything from the caret's token on stays in the box as typed, and a
     * rewrite of the box keeps the caret where it was in that text: put at
     * the end, it sent the rest of a word typed mid-box onto the last one.
     */
    const syncTokens = ({ commit = false } = {}) => {
      const chipTokens = getTokens().filter(isChip);
      const inputText = input.value.trim();
      if (!inputText) {
        setTokens(chipTokens);
        updateClearBtn();
        renderDropdownItems();
        return;
      }
      const value = input.value;
      const caret = input.selectionStart;
      const selEnd = input.selectionEnd;
      const openStart = commit ? value.length : caret - openToken(value.slice(0, caret)).length;
      const open = value.slice(openStart);
      const settledText = value.slice(0, openStart);
      const newChips = [];
      const remainingText = [];
      for (const t of parseFilterQuery(settledText)) {
        if (chippable(t)) {
          newChips.push(t);
        } else {
          remainingText.push(t);
        }
      }
      const openTokens = parseFilterQuery(open);
      for (const t of [...remainingText, ...openTokens]) typed.add(t);
      // One token per canonical form. AND-ing a token with itself changes
      // nothing, but a chip typed again beside its existing chip rendered
      // twice.
      const seen = new Set();
      const allTokens = [...chipTokens, ...newChips, ...remainingText, ...openTokens].filter(t => {
        const key = serializeToken(t);
        if (seen.has(key)) return false;
        seen.add(key);
        return true;
      });
      setTokens(allTokens);
      // Update input to show only remaining free text, and the open text
      // exactly as typed. On the debounce the settled text ended in a space,
      // so the box keeps one: the next word typed must not join the last.
      if (newChips.length > 0) {
        const rest = remainingText.map(t => serializeToken(t));
        if (open) rest.push(open);
        else if (!commit && rest.length > 0) rest.push("");
        input.value = rest.join(" ");
        // The caret and any selection lie in the open text, which ends the
        // box unchanged: they keep their distance from its end. Enter leaves
        // the caret at the end, where the value write put it.
        if (!commit) {
          const end = input.value.length;
          input.setSelectionRange(end - (value.length - caret), end - (value.length - selEnd));
        }
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
      const tokens = getTokens().filter(isChip);
      const textTokens = getTokens().filter(t => !isChip(t));
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
        syncTokens({ commit: true });
      } else if (e.key === "Backspace" && !input.value) {
        // Remove last chip
        const tokens = getTokens();
        const chipTokens = tokens.filter(isChip);
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
      // Own properties only — the value is user text, and status:constructor
      // would otherwise read Object's constructor into the chip.
      return Object.hasOwn(labels, token.value) ? labels[token.value] : token.value;
    }
    if (token.type === "platform") {
      return token.value === "youtube" ? "YouTube" : token.value === "twitch" ? "Twitch" : token.value;
    }
    if (token.type === "channel") return token.value;
    return token.value;
  }
}
