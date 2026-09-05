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
    const overflowed = this.logs.length > 500;
    if (overflowed) {
      this.logs = this.logs.slice(-500);
    }

    // Fast path: if no filter/search active, append/trim a single DOM
    // node instead of rebuilding all 500 lines
    if (this.logFilter === "all" && !this._logSearchQuery) {
      const viewer = document.getElementById("logs-viewer");
      const countEl = document.getElementById("log-count");
      if (viewer) {
        // Suppress scroll-tracking during DOM mutation so that the
        // appendChild + scrollTop assignment don't disable auto-scroll
        this._logRebuildingDOM = true;
        // Remove oldest DOM child if we overflowed
        if (overflowed && viewer.firstChild) {
          viewer.removeChild(viewer.firstChild);
        }
        const div = this._createLogLine(log);
        viewer.appendChild(div);
        if (countEl) countEl.textContent = `${this.logs.length} log entries`;
        if (this._logAutoScroll) {
          viewer.scrollTop = viewer.scrollHeight;
        }
        // Reset after next frame so any deferred scroll events are still suppressed
        requestAnimationFrame(() => { this._logRebuildingDOM = false; });
        return;
      }
    }

    // Debounce full renderLogs for filtered/search cases
    if (this._logRenderTimer) clearTimeout(this._logRenderTimer);
    this._logRenderTimer = setTimeout(() => this.renderLogs(), 100);
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
