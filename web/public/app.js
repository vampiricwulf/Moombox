/**
 * Moombox Dashboard Application
 */
import { SetupController } from "./modules/setup.js";
import { ImportController } from "./modules/imports.js";
import { PlayerController } from "./modules/player.js";
import { SettingsController } from "./modules/settings.js";
import { formatTimestamp as _formatTimestamp, formatBytes as _formatBytes, formatDurationSeconds as _formatDurationSeconds, formatRelativeTime as _formatRelativeTime } from "./modules/utils.js";

class MoomboxApp {
  constructor() {
    this.ws = null;
    this.jobs = [];
    this.archivedJobs = [];
    this.logs = [];
    this.config = null;
    this.selectedJobId = null;
    this.reconnectAttempts = 0;
    this.autoCookieReloginRequired = null;
    this.nextFeedCheck = 0;
    this.nextDecapiCheck = 0;
    this.nextTwitchCheck = 0;
    this._countdownInterval = null;
    this.logFilter = "all";
    this._logSearchQuery = "";
    this.tasksSearchQuery = "";
    this.tasksStatusFilter = "";
    this.focusedJobIndex = -1;
    this.theme = localStorage.getItem("moombox-theme") || (window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");

    // Module controllers
    this.setup = new SetupController(this);
    this.imports = new ImportController(this);
    this.player = new PlayerController(this);
    this.settings = new SettingsController(this);

    this.init();
  }

  async init() {
    // Check if this is a first-run (no config)
    const isFirstRun = await this.checkFirstRun();

    if (isFirstRun) {
      this.setup.show();
    } else {
      this.setTheme(this.theme);
      this.setupEventListeners();
      this.setupKeyboardShortcuts();
      this.settings.setupListeners();
      this.connectWebSocket();
      this.loadConfig();
      this.loadStatus();
      this._countdownInterval = setInterval(() => this.updateCheckCountdown(), 1000);
    }
  }

  async checkFirstRun() {
    try {
      const response = await fetch("/api/setup/status");
      if (response.ok) {
        const data = await response.json();
        return data.isFirstRun;
      }
    } catch (e) {
      console.error("Failed to check first-run status:", e);
    }
    return false;
  }

  setupEventListeners() {
    // Add video button
    document.getElementById("add-video-btn").addEventListener("click", () => {
      document.getElementById("add-dialog").show();
      document.getElementById("video-url-input").value = "";
      this.resetAdvancedOptions();
      setTimeout(() => document.getElementById("video-url-input").focus(), 100);
    });

    // Add video submit
    document
      .getElementById("add-submit-btn")
      .addEventListener("click", () => this.addVideo());

    // Enter key in input
    document
      .getElementById("video-url-input")
      .addEventListener("keypress", (e) => {
        if (e.key === "Enter") this.addVideo();
      });

    // Advanced options toggle
    document
      .getElementById("advanced-options-toggle")
      .addEventListener("sl-change", (e) => {
        const panel = document.getElementById("advanced-options-panel");
        const checked = e.target.checked;
        panel.style.display = checked ? "block" : "none";
        if (checked) {
          this.fetchFormatsForAdvanced();
        }
      });

    // Debounced format fetch when URL changes while advanced is checked
    let formatFetchTimeout = null;
    document
      .getElementById("video-url-input")
      .addEventListener("sl-input", () => {
        if (document.getElementById("advanced-options-toggle").checked) {
          clearTimeout(formatFetchTimeout);
          formatFetchTimeout = setTimeout(() => this.fetchFormatsForAdvanced(), 500);
        }
      });

    // Details dialog buttons
    document
      .getElementById("details-open-url-btn")
      .addEventListener("click", () => this.openJobUrl());
    document
      .getElementById("details-open-folder-btn")
      .addEventListener("click", () => this.openJobFolder());
    document
      .getElementById("details-play-btn")
      .addEventListener("click", () => this.openInPlayer());
    document
      .getElementById("details-trim-btn")
      .addEventListener("click", () => {
        const job = this.jobs.find(j => j.id === this.selectedJobId);
        if (job) this.openTrimDialog(job);
      });
    document
      .getElementById("details-cancel-btn")
      .addEventListener("click", () => this.cancelJob());
    document
      .getElementById("details-retry-btn")
      .addEventListener("click", () => this.retryJob());
    document
      .getElementById("details-delete-btn")
      .addEventListener("click", () => this.deleteJob());

    // Copy buttons in details dialog (event delegation via data-copy attribute)
    document.getElementById("details-dialog").addEventListener("click", (e) => {
      const copyBtn = e.target.closest("[data-copy]");
      if (copyBtn) {
        navigator.clipboard.writeText(copyBtn.dataset.copy);
      }
    });

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
        this.renderLogs();
      });
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

    // Tab activation handlers
    const tabGroup = document.querySelector("sl-tab-group");
    if (tabGroup) {
      tabGroup.addEventListener("sl-tab-show", (e) => {
        if (e.detail.name === "archived") {
          this.fetchArchivedJobs();
        } else if (e.detail.name === "player") {
          if (!this.player.playerInitialized) {
            this.player.initPlayer();
          }
          this.player.loadPlayerJobList();
        } else if (e.detail.name === "imports") {
          if (!this.imports.importInitialized) {
            this.imports.initImports();
          }
        } else if (e.detail.name === "files") {
          this.fetchOrphanedFiles();
        }
      });
    }

    // Refresh cookies button
    const refreshBtn = document.getElementById("btn-refresh-cookies");
    if (refreshBtn) {
      refreshBtn.addEventListener("click", () => this.recheckCookies());
    }

    // Files tab buttons
    const filesRefreshBtn = document.getElementById("files-refresh-btn");
    if (filesRefreshBtn) {
      filesRefreshBtn.addEventListener("click", () => this.fetchOrphanedFiles());
    }
    const filesDeleteAllBtn = document.getElementById("files-delete-all-btn");
    if (filesDeleteAllBtn) {
      filesDeleteAllBtn.addEventListener("click", () => this.deleteAllOrphanedFiles());
    }

    // Status warnings — delegated click
    const warningsEl = document.getElementById("status-warnings");
    if (warningsEl) {
      warningsEl.addEventListener("click", (e) => {
        const warning = e.target.closest(".status-warning");
        if (!warning) return;
        const action = warning.dataset.action;
        if (action === "yt-relogin") this.settings.startAutoCookieSetup("youtube");
        else if (action === "tw-relogin") this.settings.startAutoCookieSetup("twitch");
      });
    }

    // Tasks search/filter
    let tasksSearchTimeout = null;
    const tasksSearch = document.getElementById("tasks-search");
    if (tasksSearch) {
      tasksSearch.addEventListener("sl-input", () => {
        clearTimeout(tasksSearchTimeout);
        tasksSearchTimeout = setTimeout(() => {
          this.tasksSearchQuery = tasksSearch.value.trim();
          this.renderJobs();
        }, 200);
      });
    }

    const tasksStatusFilter = document.getElementById("tasks-status-filter");
    if (tasksStatusFilter) {
      tasksStatusFilter.addEventListener("sl-change", () => {
        this.tasksStatusFilter = tasksStatusFilter.value || "";
        this.renderJobs();
      });
    }

    // Theme toggle
    const themeToggle = document.getElementById("theme-toggle");
    if (themeToggle) {
      themeToggle.addEventListener("click", () => {
        this.setTheme(this.theme === "dark" ? "light" : "dark");
      });
    }

    // Event delegation for job items (prevents memory leaks from per-item listeners)
    const jobsContainer = document.getElementById("jobs-container");
    if (jobsContainer) {
      jobsContainer.addEventListener("click", (e) => {
        // Quick action buttons
        const quickBtn = e.target.closest("[data-quick-action]");
        if (quickBtn) {
          e.stopPropagation();
          const action = quickBtn.dataset.quickAction;
          const jobId = quickBtn.dataset.jobId;
          this.quickAction(action, jobId);
          return;
        }
        const videoItem = e.target.closest(".video-item");
        if (videoItem) {
          const jobId = videoItem.dataset.jobId;
          const job = this.jobs.find((j) => j.id === jobId);
          if (job) this.showJobDetails(job);
        }
      });
    }

    // Event delegation for archived jobs
    const archivedContainer = document.getElementById("archived-container");
    if (archivedContainer) {
      archivedContainer.addEventListener("click", (e) => {
        const quickBtn = e.target.closest("[data-quick-action]");
        if (quickBtn) {
          e.stopPropagation();
          const action = quickBtn.dataset.quickAction;
          const jobId = quickBtn.dataset.jobId;
          this.quickAction(action, jobId);
          return;
        }
        const videoItem = e.target.closest(".video-item");
        if (videoItem) {
          const jobId = videoItem.dataset.jobId;
          const job = this.archivedJobs.find((j) => j.id === jobId);
          if (job) this.showJobDetails(job);
        }
      });
    }
  }

  async loadConfig() {
    try {
      const response = await fetch("/api/config");
      if (response.ok) {
        this.config = await response.json();
        this.settings.populateConfigForm();
        this.settings.renderChannelsList();
        this.settings.renderNotificationsList();
        this.updateStatusBar();
      }
    } catch (e) {
      console.error("Failed to load config:", e);
    }
  }

  async loadStatus() {
    try {
      const response = await fetch("/api/status");
      if (response.ok) {
        const status = await response.json();
        this.autoCookieReloginRequired = status.autoCookieReloginRequired || null;
        this.cookieStatus = status.cookieStatus;
        this.twitchAuthStatus = status.twitchAuthStatus;
        this.updateStatusBar();
      }
    } catch (e) {
      console.error("Failed to load status:", e);
    }
  }

  async recheckCookies() {
    const btn = document.getElementById("btn-refresh-cookies");
    if (btn) btn.classList.add("checking");

    try {
      const response = await fetch("/api/cookies/recheck", { method: "POST" });
      if (response.ok) {
        const data = await response.json();
        this.autoCookieReloginRequired = data.autoCookieReloginRequired || null;
        this.cookieStatus = data.cookieStatus;
        this.twitchAuthStatus = data.twitchAuthStatus;
        this.updateStatusBar();
        this.showToast(
          data.success
            ? "Cookies refreshed successfully"
            : "Cookie check completed",
          data.success ? "success" : "primary",
        );
      } else {
        this.showToast("Failed to recheck cookies", "danger");
      }
    } catch (e) {
      this.showToast("Failed to recheck cookies: " + e.message, "danger");
    } finally {
      if (btn) btn.classList.remove("checking");
    }
  }

  updateStatusBar() {
    const autoCookiesEnabled = this.config?.auto_cookies?.enabled === true;

    // 1. Warnings (clickable, to the left)
    const warningsEl = document.getElementById("status-warnings");
    if (warningsEl) {
      const warnings = [];
      if (autoCookiesEnabled && this.autoCookieReloginRequired?.youtube)
        warnings.push('<span class="status-warning" data-action="yt-relogin" title="Click to re-login">YT: Re-login</span>');
      if (autoCookiesEnabled && this.autoCookieReloginRequired?.twitch)
        warnings.push('<span class="status-warning" data-action="tw-relogin" title="Click to re-login">TW: Re-login</span>');
      warningsEl.innerHTML = warnings.join("");
    }

    // 2. YT indicator (color only)
    const ytEl = document.getElementById("yt-indicator");
    if (ytEl) {
      if (this.autoCookieReloginRequired?.youtube && autoCookiesEnabled) {
        ytEl.className = "indicator-error"; ytEl.title = "YouTube: Re-login required";
      } else if (!this.cookieStatus?.found) {
        ytEl.className = "indicator-warn"; ytEl.title = "YouTube: No cookies";
      } else if (this.cookieStatus?.authenticated) {
        ytEl.className = "indicator-ok"; ytEl.title = "YouTube: Authenticated";
      } else {
        ytEl.className = "indicator-error"; ytEl.title = "YouTube: Not verified";
      }
    }

    // 3. TW indicator (color only)
    const twEl = document.getElementById("tw-indicator");
    if (twEl) {
      if (this.autoCookieReloginRequired?.twitch && autoCookiesEnabled) {
        twEl.className = "indicator-error"; twEl.title = "Twitch: Re-login required";
      } else if (this.twitchAuthStatus?.authenticated) {
        twEl.className = "indicator-ok"; twEl.title = "Twitch: Authenticated";
      } else {
        twEl.className = "indicator-off"; twEl.title = "Twitch: Anonymous";
      }
    }
  }

  // ===== WebSocket Management =====

  connectWebSocket() {
    // Clear countdown interval to prevent duplicates on reconnect
    if (this._countdownInterval) {
      clearInterval(this._countdownInterval);
      this._countdownInterval = null;
    }

    // Close any existing connection to prevent zombie sockets
    if (this.ws) {
      this.ws.onclose = null; // Prevent triggering another reconnect
      this.ws.close();
    }

    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const wsUrl = `${protocol}//${window.location.host}`;

    this.ws = new WebSocket(wsUrl);

    this.ws.onopen = () => {
      console.log("WebSocket connected");
      this.reconnectAttempts = 0;
      this.updateConnectionStatus(true);
      // Refresh status (cookie + Twitch auth) on reconnect
      this.loadStatus();
      // Restart countdown interval (cleared above to prevent duplicates)
      if (!this._countdownInterval) {
        this._countdownInterval = setInterval(() => this.updateCheckCountdown(), 1000);
      }
    };

    this.ws.onmessage = (event) => {
      try {
        const message = JSON.parse(event.data);
        this.handleMessage(message);
      } catch (e) {
        console.error("Failed to parse WebSocket message:", e);
      }
    };

    this.ws.onclose = () => {
      console.log("WebSocket disconnected");
      this.updateConnectionStatus(false);
      this.scheduleReconnect();
    };

    this.ws.onerror = (error) => {
      console.error("WebSocket error:", error);
    };
  }

  scheduleReconnect() {
    this.reconnectAttempts++;
    const delay = Math.min(
      1000 * Math.pow(1.5, this.reconnectAttempts - 1),
      30000,
    );
    console.log(
      `Reconnecting in ${Math.round(delay)}ms (attempt ${this.reconnectAttempts})`,
    );
    setTimeout(() => this.connectWebSocket(), delay);
  }

  updateConnectionStatus(connected) {
    const statusEl = document.getElementById("connection-status");
    if (connected) {
      statusEl.className = "connected";
      statusEl.innerHTML = '<sl-icon name="wifi"></sl-icon><span class="status-label"> Connected</span>';
    } else {
      statusEl.className = "disconnected";
      statusEl.innerHTML = '<sl-icon name="wifi-off"></sl-icon><span class="status-label"> Disconnected</span>';
    }
  }

  handleMessage(message) {
    switch (message.type) {
      case "initial_state":
        this.jobs = message.payload.jobs || [];
        this.logs = message.payload.logs || [];
        this.nextFeedCheck = message.payload.nextFeedCheck || 0;
        this.nextDecapiCheck = message.payload.nextDecapiCheck || 0;
        this.nextTwitchCheck = message.payload.nextTwitchCheck || 0;
        // Hide loading skeletons
        const skeleton = document.getElementById("jobs-skeleton");
        if (skeleton) skeleton.style.display = "none";
        this.renderJobs();
        this.renderLogs();
        this.updateCheckCountdown();
        break;

      case "jobs_update":
        this.jobs = message.payload || [];
        this.renderJobs();
        // Update details dialog if open (but don't reload logs)
        if (this.selectedJobId) {
          const job = this.jobs.find((j) => j.id === this.selectedJobId);
          if (job) this.updateJobDetails(job);
        }
        break;

      case "job_update": {
        // Single job update - update in place without full re-render
        const updatedJob = message.payload;
        const jobIndex = this.jobs.findIndex((j) => j.id === updatedJob.id);
        if (jobIndex !== -1) {
          this.jobs[jobIndex] = updatedJob;
          this.updateJobCard(updatedJob);
          // Update details dialog if this job is selected
          if (this.selectedJobId === updatedJob.id) {
            this.updateJobDetails(updatedJob);
          }
        } else {
          // Job not in array yet — add it and do a full re-render
          this.jobs.push(updatedJob);
          this.renderJobs();
        }
        break;
      }

      case "log":
        this.addLog(message.payload);
        break;

      case "check_timers":
        this.nextFeedCheck = message.payload.nextFeedCheck || 0;
        this.nextDecapiCheck = message.payload.nextDecapiCheck || 0;
        this.nextTwitchCheck = message.payload.nextTwitchCheck || 0;
        this.updateCheckCountdown();
        break;

      case "pong":
        // Heartbeat response
        break;
    }
  }

  formatCountdown(epochMs, compact) {
    if (!epochMs) return "--";
    const remaining = Math.max(0, Math.floor((epochMs - Date.now()) / 1000));
    if (remaining <= 0) return "now";
    const minutes = Math.floor(remaining / 60);
    const seconds = remaining % 60;
    if (compact) {
      return minutes > 0 ? `${minutes}:${String(seconds).padStart(2, "0")}` : `${seconds}s`;
    }
    if (minutes > 0) return `${minutes}m ${seconds}s`;
    return `${seconds}s`;
  }

  updateCheckCountdown() {
    const el = document.getElementById("check-countdown");
    if (!el) return;
    const compact = window.innerWidth <= 992;
    const feed = this.formatCountdown(this.nextFeedCheck, compact);
    const decapi = this.formatCountdown(this.nextDecapiCheck, compact);
    const twitch = this.formatCountdown(this.nextTwitchCheck, compact);
    if (compact) {
      el.textContent = `F ${feed} \u00b7 D ${decapi} \u00b7 T ${twitch}`;
    } else {
      el.textContent = `Next: Feed ${feed} \u00b7 DECAPI ${decapi} \u00b7 Twitch ${twitch}`;
    }
  }

  // ===== Job Rendering =====

  renderJobs() {
    const container = document.getElementById("jobs-container");
    const emptyState = document.getElementById("empty-state");
    const filterCount = document.getElementById("tasks-filter-count");

    if (this.jobs.length === 0) {
      container.innerHTML = "";
      emptyState.style.display = "flex";
      if (filterCount) { filterCount.style.display = "none"; }
      return;
    }

    const filtered = this.getFilteredJobs();
    const isFiltered = this.tasksSearchQuery || this.tasksStatusFilter;

    // Update filter count
    if (filterCount) {
      if (isFiltered) {
        filterCount.textContent = `${filtered.length} of ${this.jobs.length}`;
        filterCount.style.display = "";
      } else {
        filterCount.style.display = "none";
      }
    }

    if (filtered.length === 0 && isFiltered) {
      container.innerHTML = "";
      emptyState.style.display = "flex";
      emptyState.querySelector("sl-icon").name = "search";
      emptyState.querySelector("p").textContent = "No matching jobs";
      const subtext = emptyState.querySelector(".empty-state-subtext");
      if (subtext) subtext.textContent = "Try adjusting your search or filter";
      const cta = emptyState.querySelector(".empty-state-cta");
      if (cta) cta.style.display = "none";
      return;
    }

    if (filtered.length === 0) {
      container.innerHTML = "";
      emptyState.style.display = "flex";
      // Reset empty state to default
      emptyState.querySelector("sl-icon").name = "inbox";
      emptyState.querySelector("p").textContent = "No jobs yet";
      const subtext = emptyState.querySelector(".empty-state-subtext");
      if (subtext) subtext.textContent = "Add a YouTube or Twitch URL to start archiving";
      const cta = emptyState.querySelector(".empty-state-cta");
      if (cta) cta.style.display = "";
      return;
    }

    emptyState.style.display = "none";

    const sortedJobs = this._sortJobs(filtered);

    container.innerHTML = sortedJobs
      .map((job) => this.renderJobItem(job))
      .join("");

    // Reset focused index
    this.focusedJobIndex = -1;
  }

  async fetchArchivedJobs() {
    try {
      const response = await fetch("/api/jobs/archived");
      if (response.ok) {
        this.archivedJobs = await response.json();
        this.renderArchivedJobs();
      }
    } catch (e) {
      console.error("Failed to fetch archived jobs:", e);
    }
  }

  renderArchivedJobs() {
    const container = document.getElementById("archived-container");
    const emptyState = document.getElementById("archived-empty-state");
    const table = document.getElementById("archived-table");

    if (this.archivedJobs.length === 0) {
      container.innerHTML = "";
      table.style.display = "none";
      emptyState.style.display = "flex";
      return;
    }

    table.style.display = "";
    emptyState.style.display = "none";

    container.innerHTML = this.archivedJobs
      .map((job) => this.renderJobItem(job))
      .join("");

    // Event delegation is set up in setupEventListeners() - no per-item listeners needed
  }

  renderJobItem(job) {
    const statusClass = job.status.toLowerCase().replace("?", "");
    const isTwitch = job.platform === "twitch";
    const safeVideoId = this.escapeHtml(job.videoId || job.id);
    const ytThumb = `https://i.ytimg.com/vi/${safeVideoId}/mqdefault.jpg`;
    const twitchAvatarFallback = isTwitch && job.channelAvatarUrl ? job.channelAvatarUrl : "";
    const thumbnailUrl = job.thumbnailUrl || (isTwitch ? twitchAvatarFallback : ytThumb);
    const fallbackThumb = isTwitch ? twitchAvatarFallback : ytThumb;
    const isAvatarThumb = isTwitch && (!job.thumbnailUrl || thumbnailUrl === twitchAvatarFallback);
    const progress = this.formatProgress(job);
    const percent = job.percent || 0;
    const platformBadge = isTwitch
      ? '<sl-tag size="small" variant="primary" style="margin-right:4px;font-size:0.7em">TW</sl-tag>'
      : '';

    // Quick action button based on status
    const canCancel = ["Downloading", "Live", "Upcoming", "Muxing", "COOKIES?"].includes(job.status);
    const canRetry = ["Error", "Cancelled", "COOKIES?"].includes(job.status);
    const canDelete = ["Finished", "Error", "Cancelled", "COOKIES?"].includes(job.status);

    let actionsHtml = "";
    if (canCancel) actionsHtml += `<sl-icon-button name="x-circle" label="Cancel" data-quick-action="cancel" data-job-id="${this.escapeHtml(job.id)}"></sl-icon-button>`;
    if (canRetry) actionsHtml += `<sl-icon-button name="arrow-clockwise" label="Retry" data-quick-action="retry" data-job-id="${this.escapeHtml(job.id)}"></sl-icon-button>`;
    if (canDelete) actionsHtml += `<sl-icon-button name="trash" label="Delete" data-quick-action="delete" data-job-id="${this.escapeHtml(job.id)}"></sl-icon-button>`;

    return `
      <div class="video-item" data-job-id="${this.escapeHtml(job.id)}" data-status="${statusClass}">
        <div class="thumb">
          <img src="${this.escapeHtml(thumbnailUrl || fallbackThumb)}" alt="" loading="lazy" referrerpolicy="no-referrer"
               class="${isAvatarThumb ? "thumb-avatar" : ""}"
               onerror="this.onerror=null;${fallbackThumb ? `this.src='${this.escapeHtml(fallbackThumb)}';this.classList.add('thumb-avatar')` : "this.style.display='none'"}">
        </div>
        <div class="stream-info">
          <div class="stream-title" title="${this.escapeHtml(job.title)}">${platformBadge}${this.escapeHtml(job.title)}</div>
          <div class="stream-author">${this.escapeHtml(job.channelName)}</div>
        </div>
        <div>
          <sl-badge class="status ${statusClass}" variant="primary">${this.escapeHtml(job.status)}</sl-badge>
        </div>
        <div class="job-progress">
          <div class="job-progress-text" title="${this.escapeHtml(progress)}">${this.escapeHtml(progress)}</div>
          ${percent > 0 ? `<sl-progress-bar class="job-progress-bar" value="${percent}"></sl-progress-bar>` : ""}
        </div>
        <div class="job-quick-actions">${actionsHtml}</div>
      </div>
    `;
  }

  // Update a single job card in the list without full re-render
  updateJobCard(job) {
    const card = document.querySelector(`.video-item[data-job-id="${job.id}"]`);
    if (!card) return;

    // Update status badge and data-status
    const statusClass = job.status.toLowerCase().replace("?", "");
    card.dataset.status = statusClass;
    const statusBadge = card.querySelector(".status");
    if (statusBadge) {
      statusBadge.className = `status ${statusClass}`;
      statusBadge.setAttribute("variant", "primary");
      statusBadge.textContent = job.status;
    }

    // Update progress text
    const progressText = card.querySelector(".job-progress-text");
    if (progressText) {
      const progress = this.formatProgress(job);
      progressText.textContent = progress;
      progressText.title = progress;
    }

    // Update progress bar
    const progressContainer = card.querySelector(".job-progress");
    const progressBar = card.querySelector(".job-progress-bar");
    const percent = job.percent || 0;

    if (percent > 0) {
      if (progressBar) {
        progressBar.value = percent;
      } else if (progressContainer) {
        const newBar = document.createElement("sl-progress-bar");
        newBar.className = "job-progress-bar";
        newBar.value = percent;
        progressContainer.appendChild(newBar);
      }
    } else if (progressBar) {
      progressBar.remove();
    }

    // Update quick actions
    const actionsContainer = card.querySelector(".job-quick-actions");
    if (actionsContainer) {
      const canCancel = ["Downloading", "Live", "Upcoming", "Muxing", "COOKIES?"].includes(job.status);
      const canRetry = ["Error", "Cancelled", "COOKIES?"].includes(job.status);
      const canDelete = ["Finished", "Error", "Cancelled", "COOKIES?"].includes(job.status);

      let actionsHtml = "";
      if (canCancel) actionsHtml += `<sl-icon-button name="x-circle" label="Cancel" data-quick-action="cancel" data-job-id="${job.id}"></sl-icon-button>`;
      if (canRetry) actionsHtml += `<sl-icon-button name="arrow-clockwise" label="Retry" data-quick-action="retry" data-job-id="${job.id}"></sl-icon-button>`;
      if (canDelete) actionsHtml += `<sl-icon-button name="trash" label="Delete" data-quick-action="delete" data-job-id="${job.id}"></sl-icon-button>`;
      actionsContainer.innerHTML = actionsHtml;
    }
  }

  formatProgress(job) {
    if (job.status === "Upcoming") {
      if (job.lastRecheckAt) {
        return `Last check: ${this.formatRelativeTime(job.lastRecheckAt)}`;
      }
      return "Waiting for stream...";
    }

    if (job.status === "Live" || job.status === "Downloading") {
      if (job.progress) return job.progress;
      if (job.lastVideoSeq !== undefined) {
        return `V: ${job.lastVideoSeq || 0} / A: ${job.lastAudioSeq || 0}`;
      }
    }

    if (job.status === "Muxing") {
      return job.progress || "Muxing...";
    }

    if (job.status === "Finished") {
      return job.filename ? "Complete" : "Finished";
    }

    if (job.status === "Error") {
      return job.error ? job.error.substring(0, 50) : "Error";
    }

    if (job.status === "COOKIES?") {
      return "Needs cookie refresh";
    }

    return job.progress || "-";
  }

  // ===== Job Details =====

  showJobDetails(job) {
    this.selectedJobId = job.id;
    this.renderJobDetails(job);
    this.loadJobLogs(job.id);
    document.getElementById("details-dialog").show();
  }

  // Update job details without rebuilding logs section
  updateJobDetails(job) {
    const content = document.getElementById("job-details-content");
    if (!content) return;

    // Update status badge
    const statusBadge = content.querySelector(".status");
    if (statusBadge) {
      const statusClass = job.status.toLowerCase().replace("?", "");
      statusBadge.className = `status ${statusClass}`;
      statusBadge.textContent = job.status;
    }

    // Update progress text
    const progressRow = content.querySelector('[data-field="progress"]');
    if (progressRow) {
      progressRow.textContent = this.formatProgress(job);
    }

    // Update speed if present
    const speedRow = document.getElementById("speed-row");
    const speedValue = content.querySelector('[data-field="speed"]');
    if (speedRow && speedValue) {
      speedValue.textContent = job.speed || "";
      speedRow.style.display = job.speed ? "" : "none";
    }

    // Update updated time
    const updatedRow = content.querySelector('[data-field="updated"]');
    if (updatedRow) {
      updatedRow.textContent = this.formatRelativeTime(job.updatedAt);
    }

    // Update error display
    const errorDiv = content.querySelector(".details-error");
    if (job.error && !errorDiv) {
      const logsSection = content.querySelector(".details-section:last-child");
      if (logsSection) {
        const errorHtml = `<div class="details-error"><strong>Error:</strong> ${this.escapeHtml(job.error)}</div>`;
        logsSection.insertAdjacentHTML("beforebegin", errorHtml);
      }
    } else if (!job.error && errorDiv) {
      errorDiv.remove();
    } else if (job.error && errorDiv) {
      errorDiv.innerHTML = `<strong>Error:</strong> ${this.escapeHtml(job.error)}`;
    }

    // Update button visibility
    this.updateDetailsButtons(job);
  }

  updateDetailsButtons(job) {
    const canCancel = [
      "Downloading",
      "Live",
      "Upcoming",
      "Muxing",
      "COOKIES?",
    ].includes(job.status);
    const canRetry = ["Error", "Cancelled", "COOKIES?"].includes(job.status);
    const canDelete = ["Finished", "Error", "Cancelled", "COOKIES?"].includes(
      job.status,
    );
    const hasFile = job.status === "Finished" && job.filename;

    document.getElementById("details-cancel-btn").style.display = canCancel
      ? ""
      : "none";
    document.getElementById("details-retry-btn").style.display = canRetry
      ? ""
      : "none";
    document.getElementById("details-delete-btn").style.display = canDelete
      ? ""
      : "none";
    document.getElementById("details-trim-btn").style.display = hasFile
      ? ""
      : "none";
    const isLocalhost = ["localhost", "127.0.0.1", "::1"].includes(
      window.location.hostname,
    );
    document.getElementById("details-open-folder-btn").style.display =
      hasFile && isLocalhost ? "" : "none";
    document.getElementById("details-play-btn").style.display = hasFile
      ? ""
      : "none";
  }

  renderJobDetails(job) {
    const content = document.getElementById("job-details-content");
    const statusClass = job.status.toLowerCase().replace("?", "");

    // Show segment counts for Downloading/Muxing/Finished status
    const showSegments =
      ["Downloading", "Muxing", "Finished"].includes(job.status) &&
      (job.lastVideoSeq || job.lastAudioSeq);
    let segmentInfo = "";
    if (showSegments) {
      const vCurrent = job.lastVideoSeq || 0;
      const aCurrent = job.lastAudioSeq || 0;
      const vTotal = job.totalVideoSeq;
      const aTotal = job.totalAudioSeq;

      // Format: "current/total" or just "current" if no total
      const vDisplay = vTotal ? `${vCurrent}/${vTotal}` : vCurrent;
      const aDisplay = aTotal ? `${aCurrent}/${aTotal}` : aCurrent;

      // Twitch has single muxed HLS stream (no separate audio)
      const isTwitchSegments = job.platform === "twitch";
      segmentInfo = `<div class="details-row" id="segments-row">
          <span class="details-label">Segments:</span>
          <span class="details-value" data-field="segments">${isTwitchSegments ? vDisplay : `V: ${vDisplay} | A: ${aDisplay}`}</span>
        </div>`;
    }

    const isTwitch = job.platform === "twitch";
    // Extract Twitch login from URL or channelName for embed
    const twitchLogin = isTwitch
      ? (job.url ? job.url.replace(/.*twitch\.tv\//, "").split("/")[0] : job.channelName || "").toLowerCase()
      : "";
    const twitchVodId = isTwitch && job.videoId.startsWith("tw_v") ? job.videoId.slice(4) : "";

    // Build embed HTML
    let embedHtml;
    if (isTwitch && twitchVodId) {
      embedHtml = `<iframe class="details-embed" src="https://player.twitch.tv/?video=${this.escapeHtml(twitchVodId)}&parent=${location.hostname}&autoplay=false&muted=true" allowfullscreen></iframe>`;
    } else if (isTwitch && twitchLogin) {
      embedHtml = `<iframe class="details-embed" src="https://player.twitch.tv/?channel=${this.escapeHtml(twitchLogin)}&parent=${location.hostname}&autoplay=false&muted=true" allowfullscreen></iframe>`;
    } else {
      embedHtml = `<iframe class="details-embed" src="https://www.youtube-nocookie.com/embed/${this.escapeHtml(job.videoId)}" title="YouTube video player" frameborder="0" allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture" referrerpolicy="strict-origin-when-cross-origin" allowfullscreen></iframe>`;
    }

    content.innerHTML = `
      <div class="details-top">
        <div class="details-section">
          ${embedHtml}
        </div>

        <div class="details-section">
          <div class="details-row">
            <span class="details-label">${isTwitch ? "Stream ID:" : "Video ID:"}</span>
            <span class="details-value"><code>${this.escapeHtml(job.videoId)}</code><sl-icon-button class="details-copy-btn" name="clipboard" label="Copy" data-copy="${this.escapeHtml(job.videoId)}"></sl-icon-button></span>
          </div>
          <div class="details-row">
            <span class="details-label">Title:</span>
            <span class="details-value">${this.escapeHtml(job.title)}</span>
          </div>
          <div class="details-row">
            <span class="details-label">Channel:</span>
            <span class="details-value">${this.escapeHtml(job.channelName)}</span>
          </div>
          <div class="details-row">
            <span class="details-label">Status:</span>
            <span class="details-value">
              <sl-badge class="status ${statusClass}" variant="primary">${this.escapeHtml(job.status)}</sl-badge>
            </span>
          </div>
          ${job.chatStatus ? (() => {
            const chatVariantMap = { downloading: "primary", finished: "success", error: "danger", unavailable: "neutral", pending: "neutral" };
            const chatVariant = chatVariantMap[job.chatStatus] || "neutral";
            return `
          <div class="details-row">
            <span class="details-label">Chat:</span>
            <span class="details-value">
              <sl-badge variant="${chatVariant}">${this.escapeHtml(job.chatStatus)}</sl-badge>
              ${job.totalChatMessages ? ` (${job.totalChatMessages.toLocaleString()} messages)` : ""}
            </span>
          </div>`;
          })() : ""}
          ${
            job.isVod
              ? `
          <div class="details-row">
            <span class="details-label">Type:</span>
            <span class="details-value">VOD</span>
          </div>
          <div class="details-row" style="${this.formatProgress(job) !== "Complete" ? "" : "display:none"}">
            <span class="details-label">Progress:</span>
            <span class="details-value" data-field="progress">${this.formatProgress(job)}</span>
          </div>
          `
              : `
          <div class="details-row">
            <span class="details-label">Type:</span>
            <span class="details-value">Live</span>
          </div>
          ${segmentInfo}
          `
          }
          <div class="details-row" id="speed-row" style="${job.speed ? "" : "display:none"}">
            <span class="details-label">Speed:</span>
            <span class="details-value" data-field="speed">${job.speed || ""}</span>
          </div>
          ${
            job.filename
              ? `
          <div class="details-row">
            <span class="details-label">Filename:</span>
            <span class="details-value">${this.escapeHtml(job.filename)}<sl-icon-button class="details-copy-btn" name="clipboard" label="Copy" data-copy="${this.escapeHtml(job.filename)}"></sl-icon-button></span>
          </div>
          `
              : ""
          }
          <div class="details-row">
            <span class="details-label">Created:</span>
            <span class="details-value">${this.formatRelativeTime(job.createdAt)}</span>
          </div>
          <div class="details-row">
            <span class="details-label">Updated:</span>
            <span class="details-value" data-field="updated">${this.formatRelativeTime(job.updatedAt)}</span>
          </div>
          ${isTwitch && job.twitchCategory ? `
          <div class="details-row">
            <span class="details-label">Category:</span>
            <span class="details-value">${this.escapeHtml(job.twitchCategory)}</span>
          </div>
          ` : ""}
          ${isTwitch && job.twitchQuality ? `
          <div class="details-row">
            <span class="details-label">Quality:</span>
            <span class="details-value">${this.escapeHtml(job.twitchQuality)}</span>
          </div>
          ` : ""}
          ${job.streamStartTime ? (() => {
            const isScheduled = job.status === "Upcoming" && new Date(job.streamStartTime).getTime() > Date.now();
            const label = isScheduled ? "Scheduled" : "Stream Start";
            const value = isScheduled ? new Date(job.streamStartTime).toLocaleString() : this.formatRelativeTime(job.streamStartTime);
            return `
          <div class="details-row">
            <span class="details-label">${label}:</span>
            <span class="details-value">${this.escapeHtml(value)}</span>
          </div>
          ${isScheduled ? `
          <div class="details-row">
            <span class="details-label">Starts In:</span>
            <span class="details-value">${this.formatDurationSeconds(Math.floor((new Date(job.streamStartTime).getTime() - Date.now()) / 1000))}</span>
          </div>` : ""}`;
          })() : ""}
          ${job.streamEndTime ? `
          <div class="details-row">
            <span class="details-label">Stream End:</span>
            <span class="details-value">${this.formatRelativeTime(job.streamEndTime)}</span>
          </div>
          ` : ""}
          ${job.lengthSeconds && job.lengthSeconds > 0 ? `
          <div class="details-row">
            <span class="details-label">Duration:</span>
            <span class="details-value">${this.formatDurationSeconds(job.lengthSeconds)}</span>
          </div>
          ` : ""}
        </div>
      </div>

      ${!isTwitch && (job.selectedVideoItag != null || job.selectedAudioItag != null || job.startTime != null || job.endTime != null) ? `
      <div class="details-section">
        <strong>Advanced Options:</strong>
        ${job.selectedVideoItag != null ? `
        <div class="details-row">
          <span class="details-label">Video Format:</span>
          <span class="details-value">${
            job.selectedVideoItag === -1
              ? "None (audio only)"
              : `itag ${job.selectedVideoItag}`
          }</span>
        </div>
        ` : ""}
        ${job.selectedAudioItag != null ? `
        <div class="details-row">
          <span class="details-label">Audio Format:</span>
          <span class="details-value">${
            job.selectedAudioItag === -1
              ? "None (video only)"
              : `itag ${job.selectedAudioItag}`
          }</span>
        </div>
        ` : ""}
        ${job.startTime != null || job.endTime != null ? `
        <div class="details-row">
          <span class="details-label">Time Range:</span>
          <span class="details-value">
            ${this.formatTimestamp(job.startTime || 0)} - ${job.endTime != null ? this.formatTimestamp(job.endTime) : "end"}
            ${job.endTime != null && job.startTime != null ? ` (${this.formatTimestamp(job.endTime - job.startTime)})` : ""}
          </span>
        </div>
        ` : ""}
      </div>
      ` : ""}

      ${job.trims && job.trims.length > 0 ? `
      <sl-details summary="Trims (${job.trims.length})" open class="details-section">
        <div class="trim-list">
          ${job.trims.map(trim => {
            const range = `${this.formatTimestamp(trim.startTime)} - ${this.formatTimestamp(trim.endTime)}`;
            const duration = `${Math.floor(trim.duration)}s`;
            const size = trim.fileSize ? this.formatBytes(trim.fileSize) : '?';
            return `
              <div class="trim-item" style="display: flex; justify-content: space-between; align-items: center; padding: 8px; border-bottom: 1px solid #eee;">
                <span>
                  <strong>${range}</strong> (${duration}, ${size})
                </span>
                <sl-button size="small" variant="danger" onclick="app.deleteTrim('${this.escapeHtml(job.id)}', '${this.escapeHtml(trim.id)}')">
                  Delete
                </sl-button>
              </div>
            `;
          }).join('')}
        </div>
      </sl-details>
      ` : ""}

      ${
        job.error
          ? `
      <div class="details-error">
        <strong>Error:</strong> ${this.escapeHtml(job.error)}
      </div>
      `
          : ""
      }

      <div class="details-section">
        <strong>Job Logs:</strong>
        <div class="details-logs" id="job-logs-content">Loading logs...</div>
      </div>
    `;

    // Update button visibility
    this.updateDetailsButtons(job);
  }

  async loadJobLogs(jobId) {
    try {
      const response = await fetch(`/api/jobs/${jobId}/logs`);
      const logs = await response.json();
      const logsEl = document.getElementById("job-logs-content");
      if (logsEl) {
        logsEl.textContent =
          logs.length > 0 ? logs.join("\n") : "No logs for this job yet.";
      }
    } catch (e) {
      console.error("Failed to load job logs:", e);
    }
  }

  // ===== Job Actions =====

  async addVideo() {
    const input = document.getElementById("video-url-input");
    const submitBtn = document.getElementById("add-submit-btn");
    const value = input.value.trim();

    if (!value) return;

    // Try unified media target extraction
    const target = this.extractMediaTarget(value);
    if (!target) {
      this.showToast("Invalid YouTube/Twitch URL or ID", "warning");
      return;
    }

    // Show loading state immediately
    submitBtn.loading = true;

    try {
      let body;

      if (target.platform === "twitch") {
        // Reject clips client-side (not supported)
        if (target.type === "clip") {
          this.showToast("Twitch clips are not supported. Use a channel or VOD URL.", "warning");
          submitBtn.loading = false;
          return;
        }
        // Twitch job
        body = {
          platform: "twitch",
          videoId: target.id,
          twitchType: target.type,
        };
      } else {
        // YouTube job
        body = { videoId: target.videoId };
        const advancedToggle = document.getElementById("advanced-options-toggle");

        if (advancedToggle.checked) {
          const videoSelect = document.getElementById("video-format-select");
          const audioSelect = document.getElementById("audio-format-select");
          const startInput = document.getElementById("start-time-input");
          const endInput = document.getElementById("end-time-input");

          if (videoSelect.value) {
            body.selectedVideoItag = parseInt(videoSelect.value, 10);
          }
          if (audioSelect.value) {
            body.selectedAudioItag = parseInt(audioSelect.value, 10);
          }

          const startTime = this.parseTimeInput(startInput.value);
          if (startTime !== null && startTime > 0) {
            body.startTime = startTime;
          }
          const endTime = this.parseTimeInput(endInput.value);
          if (endTime !== null && endTime > 0) {
            body.endTime = endTime;
          }
        }
      }

      const response = await fetch("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });

      if (response.ok) {
        document.getElementById("add-dialog").hide();
        this.showToast("Video added successfully", "success");
      } else {
        const data = await response.json();
        this.showToast(data.error || "Failed to add video", "danger");
      }
    } catch (e) {
      this.showToast("Failed to add video: " + e.message, "danger");
    } finally {
      submitBtn.loading = false;
    }
  }

  /**
   * Parse time input string to seconds.
   * Supports HH:MM:SS, MM:SS, or raw seconds.
   * Returns null for invalid/empty input.
   */
  parseTimeInput(input) {
    const trimmed = (input || "").trim();
    if (!trimmed) return null;

    if (trimmed.includes(":")) {
      const parts = trimmed.split(":");
      if (parts.length < 2 || parts.length > 3) return null;
      const nums = parts.map(Number);
      if (nums.some(isNaN)) return null;
      if (parts.length === 3) return nums[0] * 3600 + nums[1] * 60 + nums[2];
      return nums[0] * 60 + nums[1];
    }

    const seconds = Number(trimmed);
    return isNaN(seconds) || seconds < 0 ? null : seconds;
  }

  /**
   * Reset advanced options panel to default state
   */
  resetAdvancedOptions() {
    const toggle = document.getElementById("advanced-options-toggle");
    toggle.checked = false;
    document.getElementById("advanced-options-panel").style.display = "none";
    document.getElementById("format-selection").style.display = "none";
    document.getElementById("format-skeleton").style.display = "none";

    // Clear selections
    const videoSelect = document.getElementById("video-format-select");
    const audioSelect = document.getElementById("audio-format-select");
    videoSelect.value = "";
    audioSelect.value = "";

    // Remove dynamic options (keep the "None" option)
    videoSelect.querySelectorAll("sl-option:not([value='-1'])").forEach((el) => el.remove());
    audioSelect.querySelectorAll("sl-option:not([value='-1'])").forEach((el) => el.remove());

    document.getElementById("start-time-input").value = "";
    document.getElementById("end-time-input").value = "";

    // Reset cached video ID
    this._lastFormatVideoId = null;
  }

  /**
   * Fetch available formats for the current video URL
   */
  async fetchFormatsForAdvanced() {
    const input = document.getElementById("video-url-input");
    const value = input.value.trim();
    if (!value) return;

    const videoId = this.extractVideoId(value);
    if (!videoId) return;

    // Don't re-fetch if same video
    if (this._lastFormatVideoId === videoId) return;
    this._lastFormatVideoId = videoId;

    const formatSkeleton = document.getElementById("format-skeleton");
    const formatSection = document.getElementById("format-selection");
    formatSkeleton.style.display = "block";
    formatSection.style.display = "none";

    try {
      const response = await fetch(`/api/formats/${videoId}`);
      if (!response.ok) {
        throw new Error("Failed to fetch formats");
      }

      const data = await response.json();
      this.populateFormatSelects(data);

      formatSkeleton.style.display = "none";
      formatSection.style.display = "block";
    } catch (e) {
      formatSkeleton.style.display = "none";
      formatSection.style.display = "block";
      console.error("Failed to fetch formats:", e);
      this.showToast("Could not load format options: " + e.message, "warning");
    }
  }

  /**
   * Populate video and audio format select dropdowns
   */
  populateFormatSelects(data) {
    const videoSelect = document.getElementById("video-format-select");
    const audioSelect = document.getElementById("audio-format-select");

    // Clear existing dynamic options
    videoSelect.querySelectorAll("sl-option:not([value='-1'])").forEach((el) => el.remove());
    audioSelect.querySelectorAll("sl-option:not([value='-1'])").forEach((el) => el.remove());

    const bestItags = data.bestItags || {};

    // Add video formats
    for (const fmt of data.videoFormats || []) {
      const opt = document.createElement("sl-option");
      opt.value = String(fmt.itag);

      const codec = (fmt.mimeType || "").split(";")[0]?.split("/")[1] || "";
      const container = codec.includes("webm") || (fmt.mimeType || "").includes("webm") ? "WEBM" : "MP4";
      const res = fmt.height ? `${fmt.width || "?"}x${fmt.height}` : "?";
      const fps = fmt.fps ? `@${fmt.fps}fps` : "";
      const bitrate = fmt.bitrate ? ` ${(fmt.bitrate / 1000).toFixed(0)}kbps` : "";

      let badges = "";
      if (fmt.itag === bestItags.bestWebmVideo) badges += " [Best WEBM]";
      if (fmt.itag === bestItags.bestMp4Video) badges += " [Best MP4]";

      opt.textContent = `${res}${fps} ${container}${bitrate}${badges}`;
      videoSelect.appendChild(opt);
    }

    // Add audio formats
    for (const fmt of data.audioFormats || []) {
      const opt = document.createElement("sl-option");
      opt.value = String(fmt.itag);

      const mime = fmt.mimeType || "";
      let codec = "";
      if (mime.includes("opus") || mime.includes("webm")) codec = "OPUS";
      else if (mime.includes("mp4a") || mime.includes("mp4")) codec = "AAC";
      else codec = mime.split(";")[0]?.split("/")[1] || "?";

      const bitrate = fmt.bitrate ? `${(fmt.bitrate / 1000).toFixed(0)}kbps` : "?";
      const sampleRate = fmt.audioSampleRate ? ` ${fmt.audioSampleRate}Hz` : "";

      let badges = "";
      if (fmt.itag === bestItags.bestOpusAudio) badges += " [Best OPUS]";
      if (fmt.itag === bestItags.bestAacAudio) badges += " [Best AAC]";

      opt.textContent = `${bitrate}${sampleRate} ${codec}${badges}`;
      audioSelect.appendChild(opt);
    }

    // Auto-select best formats
    if (bestItags.bestWebmVideo) videoSelect.value = String(bestItags.bestWebmVideo);
    else if (bestItags.bestMp4Video) videoSelect.value = String(bestItags.bestMp4Video);

    if (bestItags.bestOpusAudio) audioSelect.value = String(bestItags.bestOpusAudio);
    else if (bestItags.bestAacAudio) audioSelect.value = String(bestItags.bestAacAudio);

    // Update end time placeholder with video duration
    if (data.lengthSeconds && data.lengthSeconds > 0) {
      const h = Math.floor(data.lengthSeconds / 3600);
      const m = Math.floor((data.lengthSeconds % 3600) / 60);
      const s = data.lengthSeconds % 60;
      const durationStr = h > 0
        ? `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`
        : `${m}:${String(s).padStart(2, "0")}`;
      document.getElementById("end-time-input").placeholder = durationStr;
    }

    // Show duration display
    const durationEl = document.getElementById("video-duration-display");
    if (durationEl) {
      if (data.lengthSeconds && data.lengthSeconds > 0) {
        durationEl.textContent = `Duration: ${this.formatDurationSeconds(data.lengthSeconds)}`;
        durationEl.style.display = "block";
      } else {
        durationEl.style.display = "none";
      }
    }
  }

  openJobUrl() {
    if (!this.selectedJobId) return;
    const job = this.jobs.find((j) => j.id === this.selectedJobId)
      || this.archivedJobs.find((j) => j.id === this.selectedJobId);
    if (!job) return;
    const url = job.url || `https://www.youtube.com/watch?v=${job.videoId}`;
    try {
      const parsed = new URL(url);
      if (parsed.protocol === "https:" || parsed.protocol === "http:") {
        window.open(url, "_blank");
      }
    } catch {
      // Invalid URL — ignore
    }
  }

  async openJobFolder() {
    if (!this.selectedJobId) return;
    try {
      await fetch(`/api/jobs/${this.selectedJobId}/open-folder`, {
        method: "POST",
      });
    } catch (e) {
      console.error("Failed to open folder:", e);
    }
  }

  openInPlayer() {
    if (!this.selectedJobId) return;

    // Close the details dialog
    document.getElementById("details-dialog").hide();

    // Switch to the Player tab
    const tabGroup = document.querySelector("sl-tab-group");
    tabGroup.show("player");

    // Initialize player if needed, then select the job
    if (!this.player.playerInitialized) {
      this.player.initPlayer();
    }

    this.player.loadPlayerJobList().then(() => {
      const select = document.getElementById("player-job-select");
      select.value = this.selectedJobId;
      this.player.onPlayerJobSelect(this.selectedJobId);
    });
  }

  async cancelJob() {
    if (!this.selectedJobId) return;
    if (!confirm("Are you sure you want to cancel this job?")) return;

    try {
      const response = await fetch(`/api/jobs/${this.selectedJobId}/cancel`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Job cancelled", "success");
      } else {
        const data = await response.json();
        this.showToast(data.error || "Failed to cancel job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to cancel job: " + e.message, "danger");
    }
  }

  async retryJob() {
    if (!this.selectedJobId) return;

    try {
      const response = await fetch(`/api/jobs/${this.selectedJobId}/retry`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Job queued for retry", "success");
      } else {
        const data = await response.json();
        this.showToast(data.error || "Failed to retry job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to retry job: " + e.message, "danger");
    }
  }

  async deleteJob() {
    if (!this.selectedJobId) return;
    if (!confirm("Are you sure you want to delete this job?")) return;

    try {
      const response = await fetch(`/api/jobs/${this.selectedJobId}`, {
        method: "DELETE",
      });
      if (response.ok) {
        document.getElementById("details-dialog").hide();
        this.selectedJobId = null;
        this.showToast("Job deleted", "success");
      } else {
        const data = await response.json();
        this.showToast(data.error || "Failed to delete job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to delete job: " + e.message, "danger");
    }
  }

  // ===== Trim Management =====

  async createTrim(jobId, startTime, endTime) {
    try {
      const response = await fetch(`/api/jobs/${jobId}/trims`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ startTime, endTime }),
      });

      if (!response.ok) {
        const error = await response.json();
        throw new Error(error.error || 'Failed to create trim');
      }

      const { trim } = await response.json();

      // Show success notification
      this.showToast('Trim created successfully', 'success');

      // Fetch fresh job data from server (includes updated trims)
      if (this.selectedJobId === jobId) {
        const jobResponse = await fetch(`/api/jobs/${jobId}`, {
          cache: 'no-store' // Bypass browser cache to get fresh trim data
        });
        if (jobResponse.ok) {
          const updatedJob = await jobResponse.json();

          // Update job in the jobs array
          const jobIndex = this.jobs.findIndex(j => j.id === jobId);
          if (jobIndex !== -1) {
            this.jobs[jobIndex] = updatedJob;
          }

          // Refresh job details and logs with fresh data
          this.renderJobDetails(updatedJob);
          this.loadJobLogs(jobId);
        }
      }

      return trim;
    } catch (error) {
      this.showToast(error.message, 'danger');
      throw error;
    }
  }

  async deleteTrim(jobId, trimId) {
    if (!confirm('Delete this trim? This cannot be undone.')) {
      return;
    }

    try {
      const response = await fetch(`/api/jobs/${jobId}/trims/${trimId}`, {
        method: 'DELETE',
      });

      if (!response.ok) {
        throw new Error('Failed to delete trim');
      }

      this.showToast('Trim deleted', 'success');

      // Fetch fresh job data from server (includes updated trims)
      if (this.selectedJobId === jobId) {
        const jobResponse = await fetch(`/api/jobs/${jobId}`, {
          cache: 'no-store' // Bypass browser cache to get fresh trim data
        });
        if (jobResponse.ok) {
          const updatedJob = await jobResponse.json();

          // Update job in the jobs array
          const jobIndex = this.jobs.findIndex(j => j.id === jobId);
          if (jobIndex !== -1) {
            this.jobs[jobIndex] = updatedJob;
          }

          // Refresh job details and logs with fresh data
          this.renderJobDetails(updatedJob);
          this.loadJobLogs(jobId);
        }
      }
    } catch (error) {
      this.showToast(error.message, 'danger');
    }
  }

  openTrimDialog(job) {
    const trimDialog = document.getElementById('trim-dialog');
    const detailsDialog = document.getElementById('details-dialog');
    const startInput = document.getElementById('trim-start-input');
    const endInput = document.getElementById('trim-end-input');
    const submitBtn = document.getElementById('trim-submit-btn');

    // Reset inputs
    startInput.value = '';
    endInput.value = '';

    // Update dialog title with job info
    trimDialog.label = `Create Trim - ${job.title}`;

    // Add/update trim timeline preview
    let previewContainer = trimDialog.querySelector('.trim-preview-container');
    if (!previewContainer) {
      previewContainer = document.createElement('div');
      previewContainer.className = 'trim-preview-container';
      previewContainer.innerHTML = `
        <div class="trim-preview-label"></div>
        <div class="trim-preview"><div class="trim-preview-range"></div></div>
      `;
      // Insert before the footer
      trimDialog.querySelector('[slot="footer"]')?.before(previewContainer);
      // Fallback: just append
      if (!previewContainer.parentNode) {
        const body = trimDialog.querySelector('div') || trimDialog;
        body.appendChild(previewContainer);
      }
    }
    previewContainer.style.display = 'none';

    const totalDuration = job.lengthSeconds || 0;
    const updatePreview = () => {
      const start = this.parseTimeInput(startInput.value);
      const end = this.parseTimeInput(endInput.value);
      if (start !== null && end !== null && end > start && totalDuration > 0) {
        const startPct = Math.min(100, (start / totalDuration) * 100);
        const endPct = Math.min(100, (end / totalDuration) * 100);
        const rangeEl = previewContainer.querySelector('.trim-preview-range');
        rangeEl.style.left = `${startPct}%`;
        rangeEl.style.width = `${endPct - startPct}%`;
        previewContainer.querySelector('.trim-preview-label').textContent =
          `${this.formatTimestamp(start)} - ${this.formatTimestamp(end)} of ${this.formatTimestamp(totalDuration)}`;
        previewContainer.style.display = '';
      } else {
        previewContainer.style.display = 'none';
      }
    };

    // Abort previous trim listeners to prevent stacking on repeated opens
    if (this._trimListenerAbort) this._trimListenerAbort.abort();
    this._trimListenerAbort = new AbortController();
    const trimSignal = this._trimListenerAbort.signal;
    startInput.addEventListener('sl-input', updatePreview, { signal: trimSignal });
    endInput.addEventListener('sl-input', updatePreview, { signal: trimSignal });

    // Set up submit handler
    submitBtn.onclick = async () => {
      // Show loading state immediately
      submitBtn.loading = true;

      try {
        const startTime = this.parseTimeInput(startInput.value);
        const endTime = this.parseTimeInput(endInput.value);

        if (startTime === null || endTime === null) {
          this.showToast('Invalid time format', 'warning');
          return;
        }

        if (endTime <= startTime) {
          this.showToast('End time must be after start time', 'warning');
          return;
        }

        await this.createTrim(job.id, startTime, endTime);
        trimDialog.hide();
        // Reopen details dialog to show updated trims
        setTimeout(() => detailsDialog.show(), 100);
      } catch (error) {
        // Error already shown by createTrim()
      } finally {
        // Clear loading state
        submitBtn.loading = false;
      }
    };

    // Close details dialog first to avoid layering issues
    detailsDialog.hide();
    // Small delay to ensure smooth transition
    setTimeout(() => trimDialog.show(), 100);
  }

  formatTimestamp(seconds) {
    return _formatTimestamp(seconds);
  }

  formatBytes(bytes) {
    return _formatBytes(bytes);
  }

  // ===== Log Management =====

  addLog(log) {
    this.logs.push(log);
    if (this.logs.length > 500) {
      this.logs = this.logs.slice(-500);
    }
    // Debounce renderLogs to avoid rewriting entire textarea on every message
    if (this._logRenderTimer) clearTimeout(this._logRenderTimer);
    this._logRenderTimer = setTimeout(() => this.renderLogs(), 100);
  }

  getFilteredLogs() {
    if (this.logFilter === "all") return this.logs;

    // Filter hierarchy: ERROR < WARN < INFO < DEBUG
    const levelPriority = { ERROR: 0, WARN: 1, WARNING: 1, INFO: 2, DEBUG: 3 };
    const threshold = levelPriority[this.logFilter] ?? 3;

    return this.logs.filter((log) => {
      const match = log.match(/\[(DEBUG|INFO|WARN(?:ING)?|ERROR)\]/i);
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
      const div = document.createElement("div");
      // Determine log level class
      const levelMatch = log.match(/\[(DEBUG|INFO|WARN(?:ING)?|ERROR)\]/i);
      const level = levelMatch ? levelMatch[1].toUpperCase() : "INFO";
      let levelClass = "log-info";
      if (level === "ERROR") levelClass = "log-error";
      else if (level === "WARN" || level === "WARNING") levelClass = "log-warn";
      else if (level === "DEBUG") levelClass = "log-debug";
      div.className = `log-line ${levelClass}`;

      if (searchQuery) {
        // Highlight search matches
        const escaped = this.escapeHtml(log);
        const escapedQuery = this.escapeHtml(searchQuery);
        const regex = new RegExp(escapedQuery.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "gi");
        div.innerHTML = escaped.replace(regex, (match) => `<mark>${match}</mark>`);
      } else {
        div.textContent = log;
      }

      frag.appendChild(div);
    }

    viewer.innerHTML = "";
    viewer.appendChild(frag);

    const suffix = this.logFilter !== "all" ? ` (${this.logFilter}+)` : "";
    const searchSuffix = searchQuery ? `, matching "${searchQuery}"` : "";
    countEl.textContent = `${filtered.length} log entries${suffix}${searchSuffix}`;

    // Auto-scroll to bottom
    viewer.scrollTop = viewer.scrollHeight;
  }

  clearLogs() {
    this.logs = [];
    this.renderLogs();
  }

  // ===== Keyboard Shortcuts =====

  setupKeyboardShortcuts() {
    document.addEventListener("keydown", (e) => {
      // Skip when typing in input fields
      const tag = e.target.tagName;
      if (["INPUT", "TEXTAREA", "SL-INPUT", "SL-TEXTAREA", "SL-SELECT"].includes(tag)) return;
      if (e.target.contentEditable === "true") return;

      // If a dialog is open, only handle Escape
      const openDialog = document.querySelector("sl-dialog[open]");
      if (openDialog) {
        if (e.key === "Escape") openDialog.hide();
        return;
      }

      const tabGroup = document.querySelector("sl-tab-group");
      const panels = ["tasks", "archived", "player", "imports", "logs", "settings"];
      const activePanel = document.querySelector("sl-tab-panel[active]");
      const isPlayerActive = activePanel?.getAttribute("name") === "player";

      switch (e.key) {
        case "a":
          if (!isPlayerActive) {
            document.getElementById("add-video-btn").click();
            e.preventDefault();
          }
          break;
        case "?":
          document.getElementById("keyboard-help-dialog").show();
          e.preventDefault();
          break;
        case "1": case "2": case "3": case "4": case "5": case "6":
          if (tabGroup) tabGroup.show(panels[parseInt(e.key) - 1]);
          e.preventDefault();
          break;
        case "ArrowUp":
        case "ArrowDown":
          if (!isPlayerActive) {
            this.navigateJobList(e.key === "ArrowUp" ? -1 : 1);
            e.preventDefault();
          }
          break;
        case "Enter":
          if (!isPlayerActive && this.focusedJobIndex >= 0) {
            const filtered = this.getFilteredJobs();
            const sorted = this._sortJobs(filtered);
            const job = sorted[this.focusedJobIndex];
            if (job) this.showJobDetails(job);
          }
          break;
        case "f": {
          if (!isPlayerActive) {
            const searchInput = document.getElementById("tasks-search");
            if (searchInput) { searchInput.focus(); e.preventDefault(); }
          }
          break;
        }
      }
    });
  }

  navigateJobList(direction) {
    const filtered = this.getFilteredJobs();
    const sorted = this._sortJobs(filtered);
    if (sorted.length === 0) return;

    // Remove previous focus
    document.querySelectorAll(".video-item[data-focused]").forEach((el) => el.removeAttribute("data-focused"));

    this.focusedJobIndex += direction;
    if (this.focusedJobIndex < 0) this.focusedJobIndex = sorted.length - 1;
    if (this.focusedJobIndex >= sorted.length) this.focusedJobIndex = 0;

    const job = sorted[this.focusedJobIndex];
    if (job) {
      const card = document.querySelector(`.video-item[data-job-id="${job.id}"]`);
      if (card) {
        card.setAttribute("data-focused", "");
        card.scrollIntoView({ block: "nearest" });
      }
    }
  }

  // ===== Search/Filter =====

  getFilteredJobs() {
    let jobs = this.jobs;

    // Text filter
    if (this.tasksSearchQuery) {
      const query = this.tasksSearchQuery.toLowerCase();
      jobs = jobs.filter((j) =>
        (j.title || "").toLowerCase().includes(query) ||
        (j.channelName || "").toLowerCase().includes(query)
      );
    }

    // Status filter
    if (this.tasksStatusFilter) {
      const statusMap = {
        active: ["Downloading", "Live", "Upcoming", "Muxing"],
        errors: ["Error", "COOKIES?"],
        finished: ["Finished", "Cancelled"],
      };
      const allowed = statusMap[this.tasksStatusFilter];
      if (allowed) {
        jobs = jobs.filter((j) => allowed.includes(j.status));
      }
    }

    return jobs;
  }

  _sortJobs(jobs) {
    const STATUS_PRIORITY = {
      "Error": 0, "COOKIES?": 1, "Downloading": 2, "Muxing": 3,
      "Live": 4, "Upcoming": 5, "Cancelled": 6, "Finished": 7,
    };
    return [...jobs].sort((a, b) => {
      const pa = STATUS_PRIORITY[a.status] ?? 99;
      const pb = STATUS_PRIORITY[b.status] ?? 99;
      if (pa !== pb) return pa - pb;
      if (pa >= 6) return new Date(b.updatedAt) - new Date(a.updatedAt);
      return (a.title || "").localeCompare(b.title || "", undefined, { sensitivity: "base" });
    });
  }

  // ===== Quick Actions =====

  quickAction(action, jobId) {
    this.selectedJobId = jobId;
    switch (action) {
      case "cancel": this.cancelJob(); break;
      case "retry": this.retryJob(); break;
      case "delete": this.deleteJob(); break;
    }
  }

  // ===== Theme =====

  setTheme(theme) {
    this.theme = theme;
    const html = document.documentElement;
    const darkSheet = document.getElementById("sl-theme-dark");
    const lightSheet = document.getElementById("sl-theme-light");
    const toggleBtn = document.getElementById("theme-toggle");

    if (theme === "light") {
      html.className = "sl-theme-light";
      if (darkSheet) darkSheet.media = "not all";
      if (lightSheet) lightSheet.media = "";
      if (toggleBtn) toggleBtn.name = "sun";
    } else {
      html.className = "sl-theme-dark";
      if (darkSheet) darkSheet.media = "";
      if (lightSheet) lightSheet.media = "not all";
      if (toggleBtn) toggleBtn.name = "moon";
    }

    localStorage.setItem("moombox-theme", theme);
    // Update theme-color meta tag
    const metaTheme = document.querySelector('meta[name="theme-color"]');
    if (metaTheme) metaTheme.content = theme === "light" ? "#ffffff" : "#1C1B22";
  }

  // ===== Utilities =====

  /**
   * Extract a media target from input. Returns:
   * - { platform: "youtube", videoId } for YouTube
   * - { platform: "twitch", type: "channel"|"vod", id } for Twitch
   * - null if unrecognized
   */
  extractMediaTarget(input) {
    const trimmed = input.trim();
    if (!trimmed) return null;

    // Check for Twitch URLs first
    if (trimmed.includes("twitch.tv")) {
      return this.extractTwitchTarget(trimmed);
    }

    // Try YouTube
    const videoId = this.extractVideoId(trimmed);
    if (videoId) return { platform: "youtube", videoId };

    // Try Twitch raw username/VOD
    const twitchTarget = this.extractTwitchTarget(trimmed);
    if (twitchTarget) return twitchTarget;

    return null;
  }

  extractVideoId(input) {
    const trimmed = input.trim();

    // Direct video ID (11 chars)
    if (/^[a-zA-Z0-9_-]{11}$/.test(trimmed)) {
      return trimmed;
    }

    // YouTube URL patterns
    const patterns = [
      /(?:youtube\.com\/watch\?v=|youtu\.be\/|youtube\.com\/embed\/|youtube\.com\/v\/|youtube\.com\/shorts\/)([a-zA-Z0-9_-]{11})/,
      /youtube\.com\/live\/([a-zA-Z0-9_-]{11})/,
    ];

    for (const pattern of patterns) {
      const match = trimmed.match(pattern);
      if (match) {
        return match[1];
      }
    }

    return null;
  }

  extractTwitchTarget(input) {
    const trimmed = input.trim();
    if (!trimmed) return null;

    const reserved = new Set(["directory", "downloads", "jobs", "settings", "videos", "search", "p"]);

    try {
      let urlStr = trimmed;
      if (!urlStr.startsWith("http")) {
        if (urlStr.includes("twitch.tv")) urlStr = "https://" + urlStr;
      }
      if (urlStr.startsWith("http")) {
        const url = new URL(urlStr);
        const host = url.hostname.replace("www.", "");
        // clips.twitch.tv/{slug}
        if (host === "clips.twitch.tv") {
          const slug = url.pathname.split("/").filter(Boolean)[0];
          if (slug) return { platform: "twitch", type: "clip", id: slug };
        }
        if (host === "twitch.tv") {
          const parts = url.pathname.split("/").filter(Boolean);
          if (parts[0] === "videos" && parts[1] && /^\d+$/.test(parts[1])) {
            return { platform: "twitch", type: "vod", id: parts[1] };
          }
          if (parts.length >= 3 && parts[1] === "video" && /^\d+$/.test(parts[2])) {
            return { platform: "twitch", type: "vod", id: parts[2] };
          }
          if (parts.length >= 3 && parts[1] === "clip" && parts[2]) {
            return { platform: "twitch", type: "clip", id: parts[2] };
          }
          if (parts[0] && !reserved.has(parts[0].toLowerCase()) && /^[a-zA-Z0-9_]{1,25}$/.test(parts[0])) {
            return { platform: "twitch", type: "channel", id: parts[0].toLowerCase() };
          }
        }
      }
    } catch {}

    // Raw VOD ID with v prefix
    if (/^v\d{7,12}$/.test(trimmed)) {
      return { platform: "twitch", type: "vod", id: trimmed.slice(1) };
    }
    // Raw numeric VOD ID
    if (/^\d{7,12}$/.test(trimmed)) {
      return { platform: "twitch", type: "vod", id: trimmed };
    }
    // Raw username
    if (/^[a-zA-Z][a-zA-Z0-9_]{0,24}$/.test(trimmed)) {
      return { platform: "twitch", type: "channel", id: trimmed.toLowerCase() };
    }
    return null;
  }

  formatRelativeTime(isoDate) {
    return _formatRelativeTime(isoDate);
  }

  formatDurationSeconds(totalSeconds) {
    return _formatDurationSeconds(totalSeconds);
  }

  setInputValue(id, value) {
    const el = document.getElementById(id);
    if (el && value !== undefined && value !== null) {
      el.value = value;
    }
  }

  getInputValue(id) {
    const el = document.getElementById(id);
    if (!el) return "";
    const val = el.value;
    // Handle both string and number values (Shoelace number inputs return numbers)
    if (typeof val === "string") {
      return val.trim();
    }
    return val !== undefined && val !== null ? String(val) : "";
  }

  getInputNumber(id) {
    const el = document.getElementById(id);
    if (!el) return undefined;
    const val = el.value;
    // Shoelace number inputs may return number directly
    if (typeof val === "number") {
      return val;
    }
    const str = typeof val === "string" ? val.trim() : "";
    return str ? parseInt(str, 10) : undefined;
  }

  escapeHtml(text) {
    if (text == null) return "";
    return String(text)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#039;");
  }

  showToast(message, variant = "primary") {
    const alert = Object.assign(document.createElement("sl-alert"), {
      variant,
      closable: true,
      duration: 3000,
      className: "toast-alert",
      innerHTML: `<sl-icon slot="icon" name="${this.getIconForVariant(variant)}"></sl-icon>${this.escapeHtml(message)}<div class="toast-countdown"></div>`,
    });

    document.body.append(alert);
    alert.toast();
  }

  getIconForVariant(variant) {
    switch (variant) {
      case "success":
        return "check2-circle";
      case "warning":
        return "exclamation-triangle";
      case "danger":
        return "exclamation-octagon";
      default:
        return "info-circle";
    }
  }

  // --- Files tab ---

  async fetchOrphanedFiles() {
    try {
      const resp = await fetch("/api/v1/files/orphaned");
      if (!resp.ok) throw new Error("Failed to fetch");
      const data = await resp.json();
      this._orphanedFiles = data;
      this.renderOrphanedFiles(data);
    } catch (err) {
      console.error("Failed to fetch orphaned files:", err);
    }
  }

  renderOrphanedFiles(files) {
    const emptyEl = document.getElementById("files-empty");
    const tableWrapper = document.getElementById("files-table-wrapper");
    const deleteAllBtn = document.getElementById("files-delete-all-btn");

    if (!files || files.length === 0) {
      if (emptyEl) emptyEl.style.display = "";
      if (tableWrapper) tableWrapper.style.display = "none";
      if (deleteAllBtn) deleteAllBtn.disabled = true;
      return;
    }

    if (emptyEl) emptyEl.style.display = "none";
    if (tableWrapper) tableWrapper.style.display = "";
    if (deleteAllBtn) deleteAllBtn.disabled = false;

    const table = document.getElementById("files-table");
    // Remove existing rows (keep header)
    table.querySelectorAll(".files-row").forEach((row) => row.remove());

    // Sort: staging first, then output, then trim
    const typeOrder = { staging: 0, output: 1, trim: 2 };
    const sorted = [...files].sort((a, b) => (typeOrder[a.type] ?? 9) - (typeOrder[b.type] ?? 9));

    for (const file of sorted) {
      const row = document.createElement("div");
      row.className = "files-row";

      const typeBadge = `<span class="files-type-badge ${this.escapeHtml(file.type)}">${this.escapeHtml(file.type)}</span>`;
      const pathStr = `<span class="files-path" title="${this.escapeHtml(file.path)}">${this.escapeHtml(file.relPath)}</span>`;
      const sizeStr = `<span>${this.formatBytes(file.size)}</span>`;
      const modStr = `<span>${this.formatRelativeTime(file.modified)}</span>`;

      let jobStr = "";
      if (file.jobTitle) {
        jobStr = `<span class="files-job-info" title="${this.escapeHtml(file.jobId)}">${this.escapeHtml(file.jobTitle)} (${this.escapeHtml(file.jobStatus)})</span>`;
      } else {
        jobStr = `<span class="files-job-info">—</span>`;
      }

      const deleteBtn = `<sl-icon-button name="trash" label="Delete" class="files-delete-btn" data-path="${this.escapeHtml(file.path)}"></sl-icon-button>`;

      row.innerHTML = typeBadge + pathStr + sizeStr + modStr + jobStr + deleteBtn;

      row.querySelector(".files-delete-btn").addEventListener("click", () => {
        this.deleteOrphanedFile(file.path);
      });

      table.appendChild(row);
    }
  }

  async deleteOrphanedFile(path) {
    if (!confirm(`Delete this file?\n\n${path}`)) return;

    try {
      const resp = await fetch("/api/v1/files/orphaned", {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ paths: [path] }),
      });
      if (!resp.ok) throw new Error("Failed to delete");
      const result = await resp.json();
      if (result.deleted && result.deleted.length > 0) {
        this.showToast("File deleted", "success");
      } else if (result.errors && result.errors.length > 0) {
        this.showToast(`Failed: ${result.errors[0].error}`, "danger");
      }
      this.fetchOrphanedFiles();
    } catch (err) {
      this.showToast("Failed to delete file", "danger");
    }
  }

  async deleteAllOrphanedFiles() {
    if (!this._orphanedFiles || this._orphanedFiles.length === 0) return;
    if (!confirm(`Delete all ${this._orphanedFiles.length} orphaned files?`)) return;

    const paths = this._orphanedFiles.map((f) => f.path);
    try {
      const resp = await fetch("/api/v1/files/orphaned", {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ paths }),
      });
      if (!resp.ok) throw new Error("Failed to delete");
      const result = await resp.json();
      const count = result.deleted ? result.deleted.length : 0;
      const errCount = result.errors ? result.errors.length : 0;
      if (errCount > 0) {
        this.showToast(`Deleted ${count}, ${errCount} errors`, "warning");
      } else {
        this.showToast(`Deleted ${count} files`, "success");
      }
      this.fetchOrphanedFiles();
    } catch (err) {
      this.showToast("Failed to delete files", "danger");
    }
  }
}

// Intercept fetch to detect 401 (session expired) and redirect to login
const _originalFetch = window.fetch;
window.fetch = async function (...args) {
  const response = await _originalFetch.apply(this, args);
  if (response.status === 401) {
    // Don't redirect for auth endpoints themselves
    const url = typeof args[0] === "string" ? args[0] : args[0]?.url || "";
    if (!url.includes("/api/auth/")) {
      // Session expired — reload to get login page
      window.location.reload();
    }
  }
  return response;
};

// Initialize app when DOM is ready
document.addEventListener("DOMContentLoaded", () => {
  window.app = new MoomboxApp();
});
