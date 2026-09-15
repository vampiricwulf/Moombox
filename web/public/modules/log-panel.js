/**
 * Log Panel Controller — the log viewer: level filter, search, auto-scroll, clear
 */
export class LogPanelController {
  constructor(app) {
    this.app = app;
    this.logs = [];
    this.logFilter = "all";
    this._logAutoScroll = true;
    this._logSearchQuery = "";

    // Lines waiting for the next animation frame, and the rAF handle that will
    // flush them. See _flushPendingLines.
    this._pendingLines = [];
    this._pendingFrame = null;
  }

  /** Wire up the log filter buttons, search input, scroll tracking, and clear button. */
  bind() {
    // Clear logs
    document
      .getElementById("clear-logs-btn")
      .addEventListener("click", () => this.clearLogs());

    // Log level filter buttons
    document.querySelectorAll(".log-filter").forEach((btn) => {
      btn.addEventListener("click", () => {
        this.logFilter = btn.dataset.level;
        document.querySelectorAll(".log-filter").forEach((b) => b.classList.remove("active"));
        btn.classList.add("active");
        this._logAutoScroll = true;
        this.renderLogs();
      });
    });

    // Log scroll tracking — pause auto-scroll when user scrolls up
    const logsViewer = document.getElementById("logs-viewer");
    if (logsViewer) {
      logsViewer.addEventListener("scroll", () => {
        if (this._logRebuildingDOM) return;
        this._logAutoScroll = logsViewer.scrollTop + logsViewer.clientHeight >= logsViewer.scrollHeight - 30;
        const pill = document.getElementById("log-autoscroll-pill");
        if (pill) {
          pill.style.display = this._logAutoScroll ? "none" : "";
        }
      });
    }

    // Resume auto-scroll pill click handler
    document.getElementById("log-autoscroll-pill")?.addEventListener("click", () => {
      const viewer = document.getElementById("logs-viewer");
      if (viewer) {
        viewer.scrollTop = viewer.scrollHeight;
        this._logAutoScroll = true;
      }
      document.getElementById("log-autoscroll-pill").style.display = "none";
    });

    // Log search
    let logSearchTimeout = null;
    const logSearchInput = document.getElementById("log-search");
    if (logSearchInput) {
      logSearchInput.addEventListener("sl-input", () => {
        clearTimeout(logSearchTimeout);
        logSearchTimeout = setTimeout(() => {
          this._logSearchQuery = logSearchInput.value.trim();
          this.renderLogs();
        }, 200);
      });
    }
  }

  addLog(log) {
    this.logs.push(log);
    if (this.logs.length > 500) {
      this.logs = this.logs.slice(-500);
    }

    // Fast path: no filter/search active — queue the line and append it with
    // the next animation frame. One frame's worth of lines becomes ONE
    // fragment append, ONE count write and ONE scroll write; the per-line
    // version read viewer.scrollHeight once per line, and each of those reads
    // forces a synchronous layout, so a 100-line burst cost 100 reflows.
    if (this.logFilter === "all" && !this._logSearchQuery) {
      this._pendingLines.push(log);
      if (this._pendingFrame === null) {
        this._pendingFrame = requestAnimationFrame(() => this._flushPendingLines());
      }
      return;
    }

    // Debounce full renderLogs for filtered/search cases
    if (this._logRenderTimer) clearTimeout(this._logRenderTimer);
    this._logRenderTimer = setTimeout(() => this.renderLogs(), 100);
  }

  /** Append every queued line in one DOM write. Runs from a rAF. */
  _flushPendingLines() {
    this._pendingFrame = null;
    const pending = this._pendingLines;
    if (pending.length === 0) return;
    this._pendingLines = [];

    // The filter or the search box may have changed between the queue and this
    // frame. renderLogs rebuilds from this.logs, which already holds these
    // lines, so the queue is redundant rather than lost.
    if (this.logFilter !== "all" || this._logSearchQuery) {
      this.renderLogs();
      return;
    }

    const viewer = document.getElementById("logs-viewer");
    const countEl = document.getElementById("log-count");
    if (!viewer) return;

    // Suppress scroll-tracking during DOM mutation so that the appendChild +
    // scrollTop assignment don't disable auto-scroll
    this._logRebuildingDOM = true;

    // Trim to the same 500-line window this.logs holds. A single frame can
    // queue more than 500 lines itself (a burst); when it does, the existing
    // DOM is entirely older than this.logs' tail, so drop it all and keep
    // only the newest 500 of the pending batch instead of just trimming the
    // old DOM children.
    let toAppend = pending;
    if (toAppend.length >= 500) {
      while (viewer.firstChild) viewer.removeChild(viewer.firstChild);
      toAppend = toAppend.slice(-500);
    } else {
      let excess = viewer.childElementCount + toAppend.length - 500;
      while (excess > 0 && viewer.firstChild) {
        viewer.removeChild(viewer.firstChild);
        excess--;
      }
    }

    const frag = document.createDocumentFragment();
    for (const line of toAppend) frag.appendChild(this._createLogLine(line));
    viewer.appendChild(frag);

    if (countEl) countEl.textContent = `${this.logs.length} log entries`;
    if (this._logAutoScroll) {
      viewer.scrollTop = viewer.scrollHeight;
    }
    // Reset after next frame so any deferred scroll events are still suppressed
    requestAnimationFrame(() => { this._logRebuildingDOM = false; });
  }

  /** Create a single log line DOM element. */
  _createLogLine(log, searchQuery) {
    const div = document.createElement("div");
    const levelMatch = log.match(/\b(DEBUG|INFO|WARN(?:ING)?|ERROR)\b/i);
    const level = levelMatch ? levelMatch[1].toUpperCase() : "INFO";
    let levelClass = "log-info";
    if (level === "ERROR") levelClass = "log-error";
    else if (level === "WARN" || level === "WARNING") levelClass = "log-warn";
    else if (level === "DEBUG") levelClass = "log-debug";
    div.className = `log-line ${levelClass}`;

    if (searchQuery) {
      const regex = new RegExp(searchQuery.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "gi");
      let lastIndex = 0;
      let match;
      while ((match = regex.exec(log)) !== null) {
        if (match.index > lastIndex) {
          div.appendChild(document.createTextNode(log.slice(lastIndex, match.index)));
        }
        const mark = document.createElement("mark");
        mark.textContent = match[0];
        div.appendChild(mark);
        lastIndex = regex.lastIndex;
      }
      if (lastIndex < log.length) {
        div.appendChild(document.createTextNode(log.slice(lastIndex)));
      }
      if (lastIndex === 0) {
        div.textContent = log;
      }
    } else {
      div.textContent = log;
    }
    return div;
  }

  getFilteredLogs() {
    if (this.logFilter === "all") return this.logs;

    // Filter hierarchy: ERROR < WARN < INFO < DEBUG
    const levelPriority = { ERROR: 0, WARN: 1, WARNING: 1, INFO: 2, DEBUG: 3 };
    const threshold = levelPriority[this.logFilter] ?? 3;

    return this.logs.filter((log) => {
      const match = log.match(/\b(DEBUG|INFO|WARN(?:ING)?|ERROR)\b/i);
      if (!match) return true; // Show untagged lines always
      const level = match[1].toUpperCase();
      return (levelPriority[level] ?? 2) <= threshold;
    });
  }

  renderLogs() {
    // A queued fast-path batch is superseded by a rebuild from this.logs, which
    // already contains those lines. Dropping it here is what makes renderLogs
    // safe to call from clearLogs, the filter buttons and the initial_state
    // handler while a frame is pending.
    if (this._pendingFrame !== null) {
      cancelAnimationFrame(this._pendingFrame);
      this._pendingFrame = null;
    }
    this._pendingLines = [];

    const viewer = document.getElementById("logs-viewer");
    const countEl = document.getElementById("log-count");

    let filtered = this.getFilteredLogs();
    const searchQuery = this._logSearchQuery || "";

    // Filter by search query
    if (searchQuery) {
      const needle = searchQuery.toLowerCase();
      filtered = filtered.filter((log) => log.toLowerCase().includes(needle));
    }

    const frag = document.createDocumentFragment();
    for (const log of filtered) {
      frag.appendChild(this._createLogLine(log, searchQuery));
    }

    this._logRebuildingDOM = true;
    viewer.replaceChildren(frag);

    const suffix = this.logFilter !== "all" ? ` (${this.logFilter}+)` : "";
    const searchSuffix = searchQuery ? `, matching "${searchQuery}"` : "";
    countEl.textContent = `${filtered.length} log entries${suffix}${searchSuffix}`;

    // Auto-scroll to bottom (only when not paused by user scrolling up)
    if (this._logAutoScroll) {
      viewer.scrollTop = viewer.scrollHeight;
    }
    // Reset after next frame so scroll events from DOM rebuild are suppressed
    requestAnimationFrame(() => { this._logRebuildingDOM = false; });
  }

  clearLogs() {
    this.logs = [];
    this._logAutoScroll = true;
    this._logSearchQuery = "";
    const logSearchInput = document.getElementById("log-search");
    if (logSearchInput) logSearchInput.value = "";
    if (this._logRenderTimer) {
      clearTimeout(this._logRenderTimer);
      this._logRenderTimer = null;
    }
    this.renderLogs();
  }
}
