/**
 * Moombox Dashboard Application
 */
import { SetupController } from "./modules/setup.js";
import { ImportController } from "./modules/imports.js";
import { PlayerController } from "./modules/player.js";
import { SettingsController } from "./modules/settings.js";
import { TrimController } from "./modules/trimmer.js";
import { StatsController } from "./modules/stats.js";
import { formatTimestamp, formatBytes, formatDurationSeconds, formatRelativeTime, isTypingInInput } from "./modules/utils.js";
import { parseFilterQuery, serializeToken } from "./modules/filter-parser.js";
import { applyFilterTokens } from "./modules/filter-engine.js";

// Status sets for quick action visibility (single source of truth)
const CANCEL_STATUSES = new Set(["Downloading", "Live", "Upcoming", "Muxing", "COOKIES?"]);
const RESUME_STATUSES = new Set(["Cancelled", "Error", "COOKIES?"]);
const REINIT_STATUSES = new Set(["Error", "Cancelled", "COOKIES?"]);
const MUX_STATUSES = new Set(["Cancelled", "Error"]);
const DELETE_STATUSES = new Set(["Finished", "Error", "Cancelled", "COOKIES?"]);

class MoomboxApp {
  constructor() {
    this.ws = null;
    this.jobs = [];
    this.archivedJobs = [];
    this.logs = [];
    this.config = null;
    this.selectedJobId = null;
    this._selectedTaskJobs = new Set();
    this._selectedArchivedJobs = new Set();
    this._lastCheckedJobIds = { jobs: null, archived: null };
    this.reconnectAttempts = 0;
    this.autoCookieReloginRequired = null;
    this.nextFeedCheck = 0;
    this.nextDecapiCheck = 0;
    this.nextTwitchCheck = 0;
    this._countdownInterval = null;
    this.logFilter = "all";
    this._logAutoScroll = true;
    this._logSearchQuery = "";
    this.tasksFilterTokens = [];
    this.archivedFilterTokens = [];
    this._tasksChannels = [];
    this._archivedChannels = [];
    this.focusedJobIndex = -1;
    this.theme = localStorage.getItem("moombox-theme") || (window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");

    // Module controllers
    this.setup = new SetupController(this);
    this.imports = new ImportController(this);
    this.player = new PlayerController(this);
    this.settings = new SettingsController(this);
    this.trimmer = new TrimController(this);
    this.stats = new StatsController(this);

    this.init();
  }

  /** Return the selection set for the currently active panel. */
  _activeSelectionSet() {
    const panel = document.querySelector("sl-tab-panel[active]")?.getAttribute("name");
    return panel === "archived" ? this._selectedArchivedJobs : this._selectedTaskJobs;
  }

  /** Return the selection set for a given container key ("jobs" or "archived"). */
  _selectionSetFor(containerKey) {
    return containerKey === "archived" ? this._selectedArchivedJobs : this._selectedTaskJobs;
  }

  async init() {
    // Check if this is a first-run (no config)
    const status = await this.checkSetupStatus();

    if (status.isFirstRun) {
      this.setup.show();
    } else {
      // Check FFmpeg before initializing
      if (status.ffmpegValid === false) {
        this.setup.showFFmpegOverlay();
      } else {
        this.initializeApp();
      }
    }
  }

  initializeApp() {
    if (this._initialized) return;
    this._initialized = true;

    this.setTheme(this.theme);
    this.setupEventListeners();
    this.setupKeyboardShortcuts();
    this.settings.setupListeners();
    this.connectWebSocket();
    this.loadConfig();
    this.loadStatus();
    this._countdownInterval = setInterval(() => { this.updateCheckCountdown(); this.refreshRelativeTimestamps(); }, 1000);
  }

  async checkSetupStatus() {
    try {
      const response = await fetch("/api/setup/status");
      if (response.ok) {
        return await response.json();
      }
    } catch (e) {
      console.error("Failed to check setup status:", e);
    }
    return { isFirstRun: false };
  }

  setupEventListeners() {
    // Add video button
    const addVideoBtnClick = () => {
      document.getElementById("add-dialog").show();
      document.getElementById("video-url-input").value = "";
      this.resetAdvancedOptions();
      setTimeout(() => document.getElementById("video-url-input").focus(), 100);
    };
    document.getElementById("add-video-btn").addEventListener("click", addVideoBtnClick);

    // Empty state CTA delegates to the same add-video handler
    document.getElementById("empty-state-add-btn")?.addEventListener("click", addVideoBtnClick);

    // Add video submit
    document
      .getElementById("add-submit-btn")
      .addEventListener("click", () => this.addVideo());

    // Enter key in input
    document
      .getElementById("video-url-input")
      .addEventListener("keydown", (e) => {
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
        const job = this.jobs.find(j => j.id === this.selectedJobId)
          || this.archivedJobs.find(j => j.id === this.selectedJobId);
        if (job) this.openTrimDialog(job);
      });
    document
      .getElementById("details-cancel-btn")
      .addEventListener("click", () => this.cancelJob());
    document
      .getElementById("details-resume-btn")
      .addEventListener("click", () => this.resumeJob());
    document
      .getElementById("details-reinit-btn")
      .addEventListener("click", () => this.reinitializeJob());
    document
      .getElementById("details-mux-btn")
      .addEventListener("click", () => this.muxJob());
    document
      .getElementById("details-delete-btn")
      .addEventListener("click", () => this.deleteJob());

    // Trim dialog cleanup on close
    document.getElementById("trim-dialog").addEventListener("sl-after-hide", () => {
      this.trimmer.destroy();
    });

    // Clear selected job and release embedded iframes when details dialog is
    // dismissed (Escape, overlay click, or close button) to stop unnecessary
    // updateJobDetails calls and prevent background iframe resource usage.
    document.getElementById("details-dialog").addEventListener("sl-after-hide", () => {
      // Guard: if showJobDetails() was called between the hide start and this
      // callback, the dialog is already re-opening for a new job. Don't clear.
      const dlg = document.getElementById("details-dialog");
      if (dlg.open) return;
      this.selectedJobId = null;
      // Clear content to stop YouTube/Twitch iframe embeds from running in background
      const content = document.getElementById("job-details-content");
      if (content) content.innerHTML = "";
    });

    // Copy buttons in details dialog (event delegation via data-copy attribute)
    document.getElementById("details-dialog").addEventListener("click", (e) => {
      const copyBtn = e.target.closest("[data-copy]");
      if (copyBtn) {
        navigator.clipboard.writeText(copyBtn.dataset.copy).catch(() => {
          this.showToast("Failed to copy to clipboard", "warning");
        });
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
          // Skip if openInPlayer is handling the load (prevents race condition)
          if (!this._playerOpeningFromDetails) {
            this.player.loadPlayerJobList();
          }
        } else if (e.detail.name === "imports") {
          if (!this.imports.importInitialized) {
            this.imports.initImports();
          }
        } else if (e.detail.name === "files") {
          this.fetchOrphanedFiles();
        } else if (e.detail.name === "stats") {
          this.stats.activate();
        }
        // Deactivate stats auto-refresh when leaving the tab
        if (e.detail.name !== "stats") {
          this.stats.deactivate();
        }
        // Refresh batch action bar for the newly active panel
        this.updateBatchActionBar();
      });
    }

    // Intercept tab switches when settings has unsaved changes
    document.querySelectorAll("sl-tab[slot='nav']").forEach(tab => {
      tab.addEventListener("click", (e) => {
        if (tab.panel !== "settings" && this.settings?._dirty) {
          e.preventDefault();
          e.stopImmediatePropagation();
          this.showConfirm("You have unsaved settings changes. Discard and leave?", {
            title: "Unsaved Changes",
            okLabel: "Leave Without Saving",
            okVariant: "warning"
          }).then(confirmed => {
            if (confirmed) {
              this.settings._dirty = false;
              this.settings._updateUnsavedIndicator();
              document.getElementById("settings-unsaved-banner").style.display = "none";
              this.settings.app.loadConfig();
              tab.click();
            }
          });
        }
      }, true); // capture phase — intercept before Shoelace switches the tab
    });

    // Refresh cookies button (shift+click triggers auto-cookie browser refresh)
    const refreshBtn = document.getElementById("btn-refresh-cookies");
    if (refreshBtn) {
      refreshBtn.addEventListener("click", (e) => {
        if (e.shiftKey && this.config?.cookies?.auto_enabled) {
          this.autoCookieRefresh();
        } else {
          this.recheckCookies();
        }
      });
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

    // Status warnings — delegated click (text on desktop)
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

    // Status warnings — collapsed icon click (mobile)
    const warningsIconEl = document.getElementById("status-warnings-icon");
    if (warningsIconEl) {
      warningsIconEl.addEventListener("click", () => {
        const action = warningsIconEl.dataset.action;
        if (action === "yt-relogin") this.settings.startAutoCookieSetup("youtube");
        else if (action === "tw-relogin") this.settings.startAutoCookieSetup("twitch");
      });
    }

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

    // Theme toggle
    const themeToggle = document.getElementById("theme-toggle");
    if (themeToggle) {
      themeToggle.addEventListener("click", () => {
        this.setTheme(this.theme === "dark" ? "light" : "dark");
      });
    }

    // Update dialog buttons
    const updateNowBtn = document.getElementById("update-now-btn");
    if (updateNowBtn) updateNowBtn.addEventListener("click", () => this.applyUpdate());
    const updateDismissBtn = document.getElementById("update-dismiss-btn");
    if (updateDismissBtn) updateDismissBtn.addEventListener("click", () => this.dismissUpdate());

    // Thumbnail error handling via event delegation (error events don't bubble, use capture)
    const handleThumbError = (e) => {
      const img = e.target;
      if (img.tagName !== "IMG" || !img.closest(".thumb")) return;
      const fallback = img.dataset.fallback;
      if (fallback) {
        // Remove fallback so we don't loop if it also fails
        delete img.dataset.fallback;
        img.src = fallback;
        img.classList.add("thumb-avatar");
      } else {
        img.style.display = "none";
      }
    };
    for (const id of ["jobs-container", "archived-container"]) {
      const el = document.getElementById(id);
      if (el) el.addEventListener("error", handleThumbError, true);
    }

    // Event delegation for job items (prevents memory leaks from per-item listeners)
    // Shared click handler for job containers (active + archived)
    const setupJobContainer = (container, containerKey, jobSource) => {
      if (!container) return;
      container.addEventListener("click", (e) => {
        // Batch selection checkbox
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
        // Expandable error text
        const errorText = e.target.closest(".job-error-text");
        if (errorText) {
          e.stopPropagation();
          const isExpanded = errorText.classList.toggle("expanded");
          errorText.textContent = isExpanded ? errorText.dataset.full : errorText.dataset.short;
          return;
        }
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
          const job = jobSource().find((j) => j.id === jobId);
          if (job) this.showJobDetails(job);
        }
      });
    };

    setupJobContainer(document.getElementById("jobs-container"), "jobs", () => this.jobs);
    setupJobContainer(document.getElementById("archived-container"), "archived", () => this.archivedJobs);

    // Event delegation for trim delete buttons and watch actions (avoids inline onclick)
    const detailsContent = document.getElementById("job-details-content");
    if (detailsContent) {
      detailsContent.addEventListener("click", async (e) => {
        const btn = e.target.closest("[data-delete-trim]");
        if (btn) {
          e.stopPropagation();
          this.deleteTrim(btn.dataset.jobId, btn.dataset.trimId);
          return;
        }
        if (e.target.closest("#details-mark-watched")) {
          const res = await fetch(`/api/jobs/${this.selectedJobId}/watched`, { method: "POST" });
          if (!res.ok) this.showToast("Failed to mark watched", "danger");
          return;
        }
        if (e.target.closest("#details-mark-unwatched")) {
          const res = await fetch(`/api/jobs/${this.selectedJobId}/watched`, { method: "DELETE" });
          if (!res.ok) this.showToast("Failed to mark unwatched", "danger");
          return;
        }
      });
    }

    // Batch action bar buttons
    document.getElementById("batch-cancel")?.addEventListener("click", () => this.batchAction("cancel"));
    document.getElementById("batch-resume")?.addEventListener("click", () => this.batchAction("resume"));
    document.getElementById("batch-reinit")?.addEventListener("click", () => this.batchAction("reinitialize"));
    document.getElementById("batch-delete")?.addEventListener("click", () => this.batchAction("delete"));
    document.getElementById("batch-watched")?.addEventListener("click", () => this.batchAction("watched"));
    document.getElementById("batch-unwatched")?.addEventListener("click", () => this.batchAction("unwatched"));
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
  }

  async loadConfig() {
    try {
      const response = await fetch("/api/config");
      if (response.ok) {
        this.config = await response.json();
        if (this.settings._dirty) {
          // User has unsaved form changes — only update lists and status bar
          // without overwriting form fields (channel/notification operations
          // trigger loadConfig but shouldn't discard unsaved edits)
          this.settings.renderChannelsList();
          this.settings.renderNotificationsList();
        } else {
          this.settings.populateConfigForm();
          this.settings.renderChannelsList();
          this.settings.renderNotificationsList();
        }
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
        this.activePlatforms = status.activePlatforms || {};
        if (status.version) this._version = status.version;
        if (status.updateAvailable) this._updateAvailable = status.updateAvailable;
        if (status.uptime != null) {
          this._uptimeSeconds = status.uptime;
          this._uptimeCapturedAt = Date.now();
        }
        if (status.disk) this.stats.updateDiskIndicator(status.disk);
        this.updateVersionIndicator();
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
        if (data.activePlatforms) this.activePlatforms = data.activePlatforms;
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

  async autoCookieRefresh() {
    const btn = document.getElementById("btn-refresh-cookies");
    if (btn) btn.classList.add("checking");
    this.showToast("Running browser cookie refresh...", "primary");

    try {
      const response = await fetch("/api/cookies/auto-refresh", { method: "POST" });
      const data = await response.json().catch(() => ({ error: response.statusText }));
      if (response.ok && !data.error) {
        if (data.cookieStatus) this.cookieStatus = data.cookieStatus;
        if (data.twitchAuthStatus) this.twitchAuthStatus = data.twitchAuthStatus;
        this.autoCookieReloginRequired = data.autoCookieReloginRequired || null;
        if (data.activePlatforms) this.activePlatforms = data.activePlatforms;
        this.updateStatusBar();
        this.showToast(
          data.success
            ? "Browser cookie refresh successful"
            : "Browser cookie refresh completed — auth verification failed",
          data.success ? "success" : "warning",
        );
      } else {
        this.showToast(data.error || "Browser cookie refresh failed", "danger");
      }
    } catch (e) {
      this.showToast("Browser cookie refresh failed: " + e.message, "danger");
    } finally {
      if (btn) btn.classList.remove("checking");
    }
  }

  updateStatusBar() {
    const autoCookiesEnabled = this.config?.cookies?.auto_enabled === true;
    const ytActive = this.activePlatforms?.youtube === true;
    const twActive = this.activePlatforms?.twitch === true;

    // 1. Warnings — text on desktop, collapsed icon on mobile
    const warningsEl = document.getElementById("status-warnings");
    const warningsIcon = document.getElementById("status-warnings-icon");
    const warningItems = [];
    if (ytActive && autoCookiesEnabled && this.autoCookieReloginRequired?.youtube)
      warningItems.push({ action: "yt-relogin", label: "YT: Re-login" });
    if (twActive && autoCookiesEnabled && this.autoCookieReloginRequired?.twitch)
      warningItems.push({ action: "tw-relogin", label: "TW: Re-login" });

    if (warningsEl) {
      warningsEl.textContent = "";
      for (const w of warningItems) {
        const span = document.createElement("span");
        span.className = "status-warning";
        span.dataset.action = w.action;
        span.title = "Click to re-login";
        span.textContent = w.label;
        warningsEl.appendChild(span);
      }
    }
    if (warningsIcon) {
      const hasWarnings = warningItems.length > 0;
      warningsIcon.classList.toggle("active", hasWarnings);
      if (hasWarnings) {
        warningsIcon.textContent = "";
        const icon = document.createElement("sl-icon");
        icon.name = "exclamation-triangle";
        warningsIcon.appendChild(icon);
        warningsIcon.title = warningItems.map(w => w.label).join(", ");
        warningsIcon.dataset.action = warningItems[0].action;
      } else {
        // Clear stale children, title, and dataset from previous warnings
        warningsIcon.textContent = "";
        warningsIcon.title = "";
        delete warningsIcon.dataset.action;
      }
    }

    // 2. YT indicator (hidden if platform not active)
    const ytEl = document.getElementById("yt-indicator");
    if (ytEl) {
      ytEl.style.display = ytActive ? "" : "none";
      if (ytActive) {
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
    }

    // 3. TW indicator (hidden if platform not active)
    const twEl = document.getElementById("tw-indicator");
    if (twEl) {
      twEl.style.display = twActive ? "" : "none";
      if (twActive) {
        if (this.autoCookieReloginRequired?.twitch && autoCookiesEnabled) {
          twEl.className = "indicator-error"; twEl.title = "Twitch: Re-login required";
        } else if (this.twitchAuthStatus?.authenticated) {
          twEl.className = "indicator-ok"; twEl.title = "Twitch: Authenticated";
        } else {
          twEl.className = "indicator-off"; twEl.title = "Twitch: Anonymous";
        }
      }
    }

    // 4. Hide refresh button if neither platform is active
    const refreshBtn = document.getElementById("btn-refresh-cookies");
    if (refreshBtn) {
      refreshBtn.style.display = (ytActive || twActive) ? "" : "none";
    }
  }

  // ===== Version / Update Indicator =====

  updateVersionIndicator() {
    const el = document.getElementById("version-indicator");
    if (!el) return;
    if (!this._version) { el.style.display = "none"; return; }

    el.style.display = "";
    // Remove old listener by replacing with a fresh clone
    const fresh = el.cloneNode(false);
    el.replaceWith(fresh);
    if (this._updateAvailable) {
      fresh.textContent = `v${this._version} ⬆`;
      fresh.className = "version-indicator has-update";
      fresh.title = `Update available: v${this._updateAvailable.version}`;
      fresh.addEventListener("click", () => this.showUpdateDialog());
    } else {
      fresh.textContent = `v${this._version}`;
      fresh.className = "version-indicator";
      fresh.title = `Moombox v${this._version}`;
    }
  }

  showUpdateDialog() {
    const dlg = document.getElementById("update-dialog");
    const notes = document.getElementById("update-release-notes");
    if (!dlg || !this._updateAvailable) return;
    dlg.label = `Update to v${this._updateAvailable.version}`;
    notes.textContent = this._updateAvailable.releaseNotes || "No release notes available.";
    dlg.show();
  }

  async applyUpdate() {
    const btn = document.getElementById("update-now-btn");
    if (btn) { btn.loading = true; btn.disabled = true; }
    try {
      const resp = await fetch("/api/update/apply", { method: "POST" });
      if (resp.ok) {
        this.showToast("Update applied. Restarting...", "success");
        document.getElementById("update-dialog")?.hide();
      } else {
        const data = await resp.json().catch(() => ({ error: resp.statusText }));
        this.showToast("Update failed: " + (data.error || "Unknown error"), "danger");
      }
    } catch (e) {
      this.showToast("Update failed: " + e.message, "danger");
    } finally {
      if (btn) { btn.loading = false; btn.disabled = false; }
    }
  }

  async dismissUpdate() {
    try {
      await fetch("/api/update/dismiss", { method: "POST" });
      this._updateAvailable = null;
      this.updateVersionIndicator();
      document.getElementById("update-dialog")?.hide();
      this.showToast("Auto-updates disabled. Re-enable in Settings > Updates.", "primary");
    } catch (e) {
      this.showToast("Failed to dismiss: " + e.message, "danger");
    }
  }

  // ===== WebSocket Management =====

  connectWebSocket() {
    // Only clear the ping interval — countdown/timestamp timer runs
    // independently of WebSocket and should keep ticking during reconnection.
    if (this._pingInterval) {
      clearInterval(this._pingInterval);
      this._pingInterval = null;
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
      const isReconnect = this.reconnectAttempts > 0;
      this.reconnectAttempts = 0;
      this._lastPong = Date.now();
      this.updateConnectionStatus(true);
      // Refresh status (cookie + Twitch auth) on reconnect
      this.loadStatus();
      // Reload config on reconnect so the form reflects any server-side changes
      // (e.g., config saved via TUI, or restart-required settings that were applied).
      // Skip if user has unsaved changes to avoid silently overwriting their edits.
      if (isReconnect && !this.settings._dirty) this.loadConfig();
      // Start client-side heartbeat to detect half-open connections
      if (this._pingInterval) clearInterval(this._pingInterval);
      this._pingInterval = setInterval(() => {
        if (this.ws?.readyState === WebSocket.OPEN) {
          // If no pong received within 45s, connection is likely dead
          if (Date.now() - this._lastPong > 45000) {
            console.log("WebSocket heartbeat timeout — reconnecting");
            this.ws.close();
            return;
          }
          try { this.ws.send(JSON.stringify({ type: "ping" })); } catch { /* connection closed between check and send */ }
        }
      }, 15000);
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
      if (this._pingInterval) { clearInterval(this._pingInterval); this._pingInterval = null; }
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
    statusEl.className = connected ? "connected" : "disconnected";
    statusEl.textContent = "";
    const icon = document.createElement("sl-icon");
    icon.name = connected ? "wifi" : "wifi-off";
    const label = document.createElement("span");
    label.className = "status-label";
    label.textContent = connected ? " Connected" : " Disconnected";
    statusEl.appendChild(icon);
    statusEl.appendChild(label);
  }

  handleMessage(message) {
    const p = message.payload;
    switch (message.type) {
      case "initial_state":
        if (!p) break;
        this.jobs = p.jobs || [];
        this.logs = p.logs || [];
        this.nextFeedCheck = p.nextFeedCheck || 0;
        this.nextDecapiCheck = p.nextDecapiCheck || 0;
        this.nextTwitchCheck = p.nextTwitchCheck || 0;
        this.renderJobs();
        this.renderLogs();
        this.updateCheckCountdown();
        if (p.connectivity !== undefined) {
          this.handleConnectivityChange({ online: p.connectivity });
        }
        // Refresh details dialog if open — data may have changed during disconnect
        if (this.selectedJobId) {
          const job = this.jobs.find((j) => j.id === this.selectedJobId);
          if (job) {
            this.updateJobDetails(job);
          } else if (!this.archivedJobs.some(j => j.id === this.selectedJobId)) {
            this._verifyJobExists(this.selectedJobId);
          }
        }
        break;

      case "jobs_update":
        this._preserveStagingFields(this.jobs, p || []);
        this.jobs = p || [];
        this.renderJobs();
        // Update details dialog if open (but don't reload logs)
        if (this.selectedJobId) {
          const job = this.jobs.find((j) => j.id === this.selectedJobId);
          if (job) {
            this.updateJobDetails(job);
          } else if (!this.archivedJobs.some(j => j.id === this.selectedJobId)) {
            // Job not in active or cached archived lists — may have been archived or deleted.
            // Verify via API to avoid closing the dialog when a job simply transitions to archived.
            this._verifyJobExists(this.selectedJobId);
          }
        }
        break;

      case "job_update": {
        // Single job update - update in place, or full re-render if status changed
        const updatedJob = p;
        if (!updatedJob?.id) break;
        const jobIndex = this.jobs.findIndex((j) => j.id === updatedJob.id);
        if (jobIndex !== -1) {
          const oldJob = this.jobs[jobIndex];
          const oldStatus = oldJob.status;
          // Preserve computed staging fields from prior enriched state — WS
          // delivers raw DB objects without these fields.
          if (updatedJob.hasStaging === undefined && oldJob.hasStaging !== undefined) {
            updatedJob.hasStaging = oldJob.hasStaging;
          }
          if (updatedJob.hasSegments === undefined && oldJob.hasSegments !== undefined) {
            updatedJob.hasSegments = oldJob.hasSegments;
          }
          this.jobs[jobIndex] = updatedJob;
          // Status change affects sort order — do full re-render
          if (oldStatus !== updatedJob.status) {
            this.renderJobs();
          } else {
            this.updateJobCard(updatedJob);
            this.stats.updateActiveIndicator(this.jobs);
          }
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
        if (p) this.addLog(p);
        break;

      case "check_timers":
        if (!p) break;
        this.nextFeedCheck = p.nextFeedCheck || 0;
        this.nextDecapiCheck = p.nextDecapiCheck || 0;
        this.nextTwitchCheck = p.nextTwitchCheck || 0;
        this.updateCheckCountdown();
        break;

      case "disk_status":
        this.stats.updateDiskIndicator(p);
        break;

      case "update_available":
        this._updateAvailable = p;
        this.updateVersionIndicator();
        break;

      case "connectivity":
        this.handleConnectivityChange(p);
        break;

      case "pong":
        this._lastPong = Date.now();
        break;
    }
  }

  handleConnectivityChange(payload) {
    const banner = document.getElementById("connectivity-banner");
    if (!banner) return;
    if (payload.online) {
      banner.classList.remove("show");
    } else {
      banner.classList.add("show");
    }
  }

  /**
   * Verify whether a job still exists on the server (e.g. it was archived, not deleted).
   * Used when a job disappears from the active jobs list while the details dialog is open.
   * If the job exists, cache it in archivedJobs and update the dialog.
   * If the job is truly gone (404), close the dialog.
   */
  async _verifyJobExists(jobId) {
    // Prevent duplicate concurrent fetches for the same job
    if (this._verifyingJobId === jobId) return;
    this._verifyingJobId = jobId;
    try {
      const resp = await fetch(`/api/jobs/${jobId}`);
      if (this.selectedJobId !== jobId) return; // Selection changed during fetch
      if (resp.ok) {
        const job = await resp.json();
        // Cache in archivedJobs so subsequent jobs_update messages skip the API check
        const existingIdx = this.archivedJobs.findIndex(j => j.id === job.id);
        if (existingIdx !== -1) {
          this.archivedJobs[existingIdx] = job;
        } else {
          this.archivedJobs.push(job);
        }
        this.updateJobDetails(job);
      } else if (resp.status === 404) {
        // Job truly deleted — close dialog
        const dlg = document.getElementById("details-dialog");
        if (dlg?.open) dlg.hide();
        this.selectedJobId = null;
      }
      // Other errors (500, 401, etc.) — leave dialog open with last known data
    } catch {
      // Network error — leave dialog open with last known data
    } finally {
      if (this._verifyingJobId === jobId) this._verifyingJobId = null;
    }
  }

  formatCountdown(epochMs) {
    if (!epochMs) return "--";
    const remaining = Math.max(0, Math.floor((epochMs - Date.now()) / 1000));
    if (remaining <= 0) return "now";
    const minutes = Math.floor(remaining / 60);
    const seconds = remaining % 60;
    if (minutes > 0) return `${minutes}m ${seconds}s`;
    return `${seconds}s`;
  }

  refreshRelativeTimestamps() {
    document.querySelectorAll("[data-timestamp]").forEach((el) => {
      const ts = el.dataset.timestamp;
      if (!ts) return;
      const relative = this.formatRelativeTime(ts);
      const prefix = el.dataset.timestampPrefix || "";
      const text = prefix + relative;
      el.textContent = text;
      // Update title only for prefixed elements (e.g. job progress);
      // details panel spans keep their fixed full-date title set at render time
      if (prefix) el.title = text;
    });
    // Update "Starts In" countdown in details panel
    document.querySelectorAll("[data-timestamp-countdown]").forEach((el) => {
      const ts = el.dataset.timestampCountdown;
      if (!ts) return;
      const diff = Math.floor((new Date(ts).getTime() - Date.now()) / 1000);
      el.textContent = diff > 0 ? this.formatDurationSeconds(diff) : "Now";
    });
  }

  updateCheckCountdown() {
    const el = document.getElementById("check-countdown");
    if (!el) return;
    const feed = this.formatCountdown(this.nextFeedCheck);
    const decapi = this.formatCountdown(this.nextDecapiCheck);
    const twitch = this.formatCountdown(this.nextTwitchCheck);
    if (window.innerWidth <= 992) {
      el.textContent = `F ${feed} \u00b7 D ${decapi} \u00b7 T ${twitch}`;
    } else {
      el.textContent = `Feed ${feed} \u00b7 DECAPI ${decapi} \u00b7 Twitch ${twitch}`;
    }
  }

  // ===== Job Rendering =====

  renderJobs() {
    // Remove loading skeletons on first render (any message path)
    document.getElementById("jobs-skeleton")?.remove();
    this._tasksChannels = [...new Set(this.jobs.map(j => j.channelName).filter(Boolean))].sort(
      (a, b) => a.localeCompare(b, undefined, { sensitivity: "base" })
    );

    // Update active indicator in status bar
    this.stats.updateActiveIndicator(this.jobs);

    const container = document.getElementById("jobs-container");
    const emptyState = document.getElementById("empty-state");
    const filterCount = document.getElementById("tasks-filter-count");

    if (this.jobs.length === 0) {
      container.innerHTML = "";
      emptyState.style.display = "flex";
      const justSetup = sessionStorage.getItem("justCompletedSetup");
      if (justSetup) {
        sessionStorage.removeItem("justCompletedSetup");
        const icon = emptyState.querySelector("sl-icon");
        if (icon) icon.name = "check-circle";
        const msg = emptyState.querySelector("p");
        if (msg) msg.textContent = "Setup complete!";
        const subtext = emptyState.querySelector(".empty-state-subtext");
        if (subtext) subtext.textContent = "Add channels to auto-monitor streams, or add a video URL to start archiving now.";
        const cta = emptyState.querySelector(".empty-state-cta");
        if (cta) cta.style.display = "none";
        // Remove any leftover setup CTA wrapper from a previous render
        emptyState.querySelector("#empty-state-setup-cta")?.remove();
        // Inject two-button CTA
        const setupCta = document.createElement("div");
        setupCta.id = "empty-state-setup-cta";
        setupCta.style.cssText = "display:flex;gap:0.5rem;flex-wrap:wrap;justify-content:center;";
        const addChannelsBtn = document.createElement("sl-button");
        addChannelsBtn.setAttribute("variant", "default");
        addChannelsBtn.setAttribute("size", "small");
        addChannelsBtn.textContent = "Add Channels";
        addChannelsBtn.addEventListener("click", () => {
          document.querySelector('sl-tab[panel="settings"]')?.click();
          setTimeout(() => document.querySelector('[data-section="channels"]')?.click(), 100);
        });
        const addVideoBtn = document.createElement("sl-button");
        addVideoBtn.setAttribute("variant", "primary");
        addVideoBtn.setAttribute("size", "small");
        addVideoBtn.textContent = "Add Video";
        addVideoBtn.addEventListener("click", () => {
          document.getElementById("add-dialog").show();
          document.getElementById("video-url-input").value = "";
          this.resetAdvancedOptions();
          setTimeout(() => document.getElementById("video-url-input").focus(), 100);
        });
        setupCta.appendChild(addChannelsBtn);
        setupCta.appendChild(addVideoBtn);
        if (cta) cta.insertAdjacentElement("afterend", setupCta);
        else emptyState.appendChild(setupCta);
      } else {
        // Reset empty state to default content (may have been changed by a filter or setup state)
        emptyState.querySelector("#empty-state-setup-cta")?.remove();
        const icon = emptyState.querySelector("sl-icon");
        if (icon) icon.name = "inbox";
        const msg = emptyState.querySelector("p");
        if (msg) msg.textContent = "No jobs yet";
        const subtext = emptyState.querySelector(".empty-state-subtext");
        if (subtext) subtext.textContent = "Add a YouTube or Twitch URL to start archiving";
        const cta = emptyState.querySelector(".empty-state-cta");
        if (cta) cta.style.display = "";
      }
      if (filterCount) { filterCount.style.display = "none"; }
      return;
    }

    const filtered = this.getFilteredJobs();
    const isFiltered = this.tasksFilterTokens.length > 0;

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
      const icon = emptyState.querySelector("sl-icon");
      if (icon) icon.name = "search";
      const msg = emptyState.querySelector("p");
      if (msg) msg.textContent = "No matching jobs";
      const subtext = emptyState.querySelector(".empty-state-subtext");
      if (subtext) subtext.textContent = "Search matches titles and channel names";
      const cta = emptyState.querySelector(".empty-state-cta");
      if (cta) cta.style.display = "none";
      return;
    }

    emptyState.style.display = "none";

    const sortedJobs = this._sortJobs(filtered);

    // Preserve focused job across re-renders
    const focusedCard = container.querySelector(".video-item[data-focused]");
    const focusedJobId = focusedCard?.dataset.jobId;

    container.innerHTML = sortedJobs
      .map((job) => this.renderJobItem(job))
      .join("");

    // Restore focused index if the job still exists, otherwise reset
    if (focusedJobId) {
      const newIndex = sortedJobs.findIndex(j => j.id === focusedJobId);
      this.focusedJobIndex = newIndex >= 0 ? newIndex : -1;
      if (newIndex >= 0) {
        const card = container.querySelector(`.video-item[data-job-id="${CSS.escape(focusedJobId)}"]`);
        if (card) card.setAttribute("data-focused", "");
      }
    } else {
      this.focusedJobIndex = -1;
    }

    // Remove stale selected IDs (jobs that no longer exist)
    const taskJobIds = new Set(this.jobs.map(j => j.id));
    this._selectedTaskJobs.forEach(id => {
      if (!taskJobIds.has(id)) this._selectedTaskJobs.delete(id);
    });

    // Re-apply selection state and sync the batch bar
    this._selectedTaskJobs.forEach(id => {
      const card = container.querySelector(`[data-job-id="${CSS.escape(id)}"]`);
      if (card) {
        card.classList.add("selected");
        const cb = card.querySelector(".job-checkbox");
        if (cb) cb.checked = true;
      }
    });
    this.updateBatchActionBar();
  }

  async fetchArchivedJobs() {
    try {
      const response = await fetch("/api/jobs/archived");
      if (response.ok) {
        this.archivedJobs = await response.json();
      } else {
        this.archivedJobs = [];
      }
    } catch (e) {
      console.error("Failed to fetch archived jobs:", e);
      this.archivedJobs = [];
    }
    this.renderArchivedJobs();
  }

  renderArchivedJobs() {
    const container = document.getElementById("archived-container");
    const emptyState = document.getElementById("archived-empty-state");
    const table = document.getElementById("archived-table");
    const filterCount = document.getElementById("archived-filter-count");
    this._archivedChannels = [...new Set(this.archivedJobs.map(j => j.channelName).filter(Boolean))].sort(
      (a, b) => a.localeCompare(b, undefined, { sensitivity: "base" })
    );

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
    const isFiltered = this.archivedFilterTokens.length > 0;

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

    // Remove stale archived selected IDs
    const archivedJobIds = new Set(this.archivedJobs.map(j => j.id));
    this._selectedArchivedJobs.forEach(id => {
      if (!archivedJobIds.has(id)) this._selectedArchivedJobs.delete(id);
    });

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

  /** Update local job resume position and refresh UI (called by player on silent saves). */
  _updateJobResumePosition(jobId, position) {
    const job = this.jobs.find(j => j.id === jobId) || this.archivedJobs.find(j => j.id === jobId);
    if (job) {
      job.resumePosition = position;
      // Update thumbnail indicator
      this.updateJobCard(job);
      // Update details pill if this job's details are open
      if (this.selectedJobId === jobId) {
        const pill = document.querySelector("#job-details-content .watch-pill");
        const newPill = this.watchPillHtml(job);
        if (pill && newPill) {
          pill.outerHTML = newPill;
        } else if (!pill && newPill) {
          // Insert pill after status badge inside the same value span
          const badge = document.querySelector("#job-details-content .status");
          badge?.insertAdjacentHTML("afterend", " " + newPill);
        }
      }
    }
  }

  /** Returns eyeball overlay HTML for a job thumbnail, or empty string. */
  watchIndicatorHtml(job) {
    if (job.status !== "Finished") return "";
    if (job.watched) {
      return `<span class="watch-indicator watched"><svg viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3" fill="rgba(0,0,0,0.4)"/></svg></span>`;
    }
    if (job.resumePosition != null) {
      return `<span class="watch-indicator in-progress"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg></span>`;
    }
    return "";
  }

  renderJobItem(job) {
    const statusClass = job.status.toLowerCase().replace("?", "");
    const isTwitch = job.platform === "twitch";
    const rawVideoId = job.videoId || job.id;
    const ytThumb = `https://i.ytimg.com/vi/${encodeURIComponent(rawVideoId)}/mqdefault.jpg`;
    const twitchAvatarFallback = isTwitch && job.channelAvatarUrl ? job.channelAvatarUrl : "";
    const thumbnailUrl = job.thumbnailUrl || (isTwitch ? twitchAvatarFallback : ytThumb);
    const fallbackThumb = isTwitch ? twitchAvatarFallback : ytThumb;
    const isAvatarThumb = isTwitch && (!job.thumbnailUrl || thumbnailUrl === twitchAvatarFallback);
    const progress = this.formatProgress(job);
    const progressHtml = this.formatProgressHtml(job);
    const percent = job.percent || 0;
    const platformBadge = isTwitch
      ? '<sl-tag size="small" variant="primary" style="margin-right:4px;font-size:0.7em">TW</sl-tag>'
      : '';

    // Quick action button based on status
    const canCancel = CANCEL_STATUSES.has(job.status);
    const canReinit = REINIT_STATUSES.has(job.status);
    const canDelete = DELETE_STATUSES.has(job.status);

    let actionsHtml = "";
    if (canCancel) actionsHtml += `<sl-icon-button name="x-circle" label="Cancel" data-quick-action="cancel" data-job-id="${this.escapeHtml(job.id)}"></sl-icon-button>`;
    if (canReinit) actionsHtml += `<sl-icon-button name="arrow-clockwise" label="Reinitialize" data-quick-action="reinitialize" data-job-id="${this.escapeHtml(job.id)}"></sl-icon-button>`;
    if (canDelete) actionsHtml += `<sl-icon-button name="trash" label="Delete" data-quick-action="delete" data-job-id="${this.escapeHtml(job.id)}"></sl-icon-button>`;

    const isSelected = this._selectedTaskJobs.has(job.id) || this._selectedArchivedJobs.has(job.id);
    return `
      <div class="video-item${isSelected ? " selected" : ""}" data-job-id="${this.escapeHtml(job.id)}" data-status="${this.escapeHtml(statusClass)}">
        <div class="thumb">
          <input type="checkbox" class="job-checkbox" data-job-id="${this.escapeHtml(job.id)}" ${isSelected ? "checked" : ""} aria-label="Select ${this.escapeHtml(job.title)}">
          ${(thumbnailUrl || fallbackThumb) ? `<img src="${this.escapeHtml(thumbnailUrl || fallbackThumb)}" alt="" loading="lazy" referrerpolicy="no-referrer"
               class="${isAvatarThumb ? "thumb-avatar" : ""}"
               ${fallbackThumb ? `data-fallback="${this.escapeHtml(fallbackThumb)}"` : ""}>` : ""}
          ${this.watchIndicatorHtml(job)}
        </div>
        <div class="stream-info">
          <div class="stream-title" title="${this.escapeHtml(job.title)}">${platformBadge}${this.escapeHtml(job.title)}</div>
          <div class="stream-author">${this.escapeHtml(job.channelName)}</div>
        </div>
        <div>
          <sl-badge class="status ${this.escapeHtml(statusClass)}" variant="primary">${this.escapeHtml(this.displayStatus(job.status))}</sl-badge>
        </div>
        <div class="job-progress">
          <div class="job-progress-text" ${job.status === "Upcoming" && job.lastRecheckAt ? `data-timestamp="${this.escapeHtml(job.lastRecheckAt)}" data-timestamp-prefix="Last check: "` : ""} title="${this.escapeHtml(this.formatProgressTooltip(job) || progress)}">${progressHtml}</div>
          ${percent > 0 ? `<sl-progress-bar class="job-progress-bar" value="${this.escapeHtml(percent)}"></sl-progress-bar>` : ""}
        </div>
        <div class="job-quick-actions">${actionsHtml}</div>
      </div>
    `;
  }

  // Update a single job card in the list without full re-render
  updateJobCard(job) {
    const card = document.querySelector(`.video-item[data-job-id="${CSS.escape(job.id)}"]`);
    if (!card) return;

    // Update thumbnail (can change for Twitch: avatar → live thumbnail)
    const thumbImg = card.querySelector(".thumb img");
    if (thumbImg && job.thumbnailUrl && thumbImg.src !== job.thumbnailUrl) {
      thumbImg.src = job.thumbnailUrl;
      thumbImg.classList.remove("thumb-avatar");
    }

    // Update watch indicator
    const existingIndicator = card.querySelector(".thumb .watch-indicator");
    const newIndicatorHtml = this.watchIndicatorHtml(job);
    if (newIndicatorHtml) {
      if (existingIndicator) {
        existingIndicator.outerHTML = newIndicatorHtml;
      } else {
        card.querySelector(".thumb")?.insertAdjacentHTML("beforeend", newIndicatorHtml);
      }
    } else if (existingIndicator) {
      existingIndicator.remove();
    }

    // Update title only when changed (avoids recreating sl-tag shadow DOM for TW badge)
    const titleEl = card.querySelector(".stream-title");
    if (titleEl && titleEl.title !== job.title) {
      const isTwitch = job.platform === "twitch";
      const platformBadge = isTwitch
        ? '<sl-tag size="small" variant="primary" style="margin-right:4px;font-size:0.7em">TW</sl-tag>'
        : '';
      titleEl.innerHTML = platformBadge + this.escapeHtml(job.title);
      titleEl.title = job.title;
    }
    const authorEl = card.querySelector(".stream-author");
    if (authorEl && authorEl.textContent !== job.channelName) {
      authorEl.textContent = job.channelName;
    }

    // Status badge and data-status are NOT updated here — updateJobCard is
    // only called when status hasn't changed, so these are already correct.

    // Update progress text
    const progressText = card.querySelector(".job-progress-text");
    if (progressText) {
      const progress = this.formatProgress(job);
      progressText.innerHTML = this.formatProgressHtml(job);
      progressText.title = this.formatProgressTooltip(job) || progress;
      if (job.status === "Upcoming" && job.lastRecheckAt) {
        progressText.dataset.timestamp = job.lastRecheckAt;
        progressText.dataset.timestampPrefix = "Last check: ";
      } else {
        delete progressText.dataset.timestamp;
        delete progressText.dataset.timestampPrefix;
      }
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

    // Quick actions are NOT rebuilt here — updateJobCard is only called when
    // status hasn't changed (status changes trigger full renderJobs instead),
    // so the available actions are identical. Skipping avoids destroying and
    // recreating sl-icon-button shadow DOM elements on every progress update.
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
      return job.error || "Error";
    }

    if (job.status === "COOKIES?") {
      return "Needs cookie refresh";
    }

    return job.progress || "-";
  }

  // Returns progress as safe HTML — identical to formatProgress for most statuses,
  // but wraps error text in an expandable span for the Error status.
  formatProgressHtml(job) {
    if (job.status === "Error") {
      const errorText = job.error || "Error";
      const truncated = errorText.length > 50 ? errorText.substring(0, 50) + "…" : errorText;
      return `<span class="job-error-text" title="${this.escapeHtml(errorText)}" data-full="${this.escapeHtml(errorText)}" data-short="${this.escapeHtml(truncated)}">${this.escapeHtml(truncated)}</span>`;
    }
    return this.escapeHtml(this.formatProgress(job));
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

    // If status changed, rebuild the details to refresh structural elements
    // (segment rows, embed, buttons) that depend on status.
    const statusBadge = content.querySelector(".status");
    const currentStatus = statusBadge?.textContent;
    if (currentStatus && currentStatus !== this.displayStatus(job.status)) {
      this.renderJobDetails(job);
      this.loadJobLogs(job.id);
      return;
    }

    // If watch state changed, rebuild to refresh pill and action buttons
    const watchPill = content.querySelector(".watch-pill");
    const watchedNow = !!job.watched;
    const hadWatchPill = !!watchPill;
    const watchPillStale = watchedNow !== (watchPill?.classList.contains("watched") ?? false)
      || (!watchedNow && (job.resumePosition != null) !== (watchPill?.classList.contains("in-progress") ?? false))
      || (watchedNow && !hadWatchPill) || (!watchedNow && !job.resumePosition && hadWatchPill);
    if (watchPillStale) {
      this.renderJobDetails(job);
      this.loadJobLogs(job.id);
      return;
    }

    // Update status badge
    if (statusBadge) {
      const statusClass = job.status.toLowerCase().replace("?", "");
      statusBadge.className = `status ${statusClass}`;
      statusBadge.textContent = this.displayStatus(job.status);
    }

    // Update title and channel (can change for live streams mid-broadcast)
    const rows = content.querySelectorAll(".details-row");
    for (const row of rows) {
      const label = row.querySelector(".details-label");
      if (!label) continue;
      const labelText = label.textContent;
      const valueEl = row.querySelector(".details-value");
      if (!valueEl) continue;
      if (labelText === "Title:") {
        valueEl.textContent = job.title;
      } else if (labelText === "Channel:") {
        valueEl.textContent = job.channelName;
      } else if (labelText === "Category:" && job.twitchCategory) {
        valueEl.textContent = job.twitchCategory;
      }
    }

    // Update progress text
    const progressRow = content.querySelector('[data-field="progress"]');
    if (progressRow) {
      progressRow.textContent = this.formatProgress(job);
    }

    // Update segment counts
    const segField = content.querySelector('[data-field="segments"]');
    if (segField && (job.lastVideoSeq || job.lastAudioSeq)) {
      const isTwitchSeg = job.platform === "twitch";
      const vCurrent = job.lastVideoSeq || 0;
      const aCurrent = job.lastAudioSeq || 0;
      const vTotal = job.totalVideoSeq;
      const aTotal = job.totalAudioSeq;
      const vDisplay = vTotal ? `${vCurrent}/${vTotal}` : vCurrent;
      const aDisplay = aTotal ? `${aCurrent}/${aTotal}` : aCurrent;
      segField.textContent = isTwitchSeg ? vDisplay : `V: ${vDisplay} | A: ${aDisplay}`;
    }

    // Update chat status
    const chatField = content.querySelector('[data-field="chat"]');
    if (chatField && job.chatStatus) {
      const chatVariantMap = { downloading: "primary", finished: "success", error: "danger", unavailable: "neutral", pending: "neutral" };
      const badge = chatField.querySelector("sl-badge");
      if (badge) {
        badge.variant = chatVariantMap[job.chatStatus] || "neutral";
        badge.textContent = job.chatStatus;
      }
      // Update message count — text node after the badge
      const existingText = badge && badge.nextSibling && badge.nextSibling.nodeType === Node.TEXT_NODE
        ? badge.nextSibling : null;
      const countText = job.totalChatMessages ? ` (${job.totalChatMessages.toLocaleString()} messages)` : "";
      if (existingText) {
        existingText.textContent = countText;
      } else if (countText) {
        chatField.appendChild(document.createTextNode(countText));
      }
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
      updatedRow.dataset.timestamp = job.updatedAt;
      updatedRow.title = new Date(job.updatedAt).toLocaleString();
    }

    // Update error display
    const errorDiv = content.querySelector(".details-error");
    if (job.error && !errorDiv) {
      const logsSection = content.querySelector(".details-section:last-child");
      if (logsSection) {
        const newErrorDiv = document.createElement("div");
        newErrorDiv.className = "details-error";
        const strong = document.createElement("strong");
        strong.textContent = "Error:";
        newErrorDiv.appendChild(strong);
        newErrorDiv.appendChild(document.createTextNode(" " + job.error));
        logsSection.parentNode.insertBefore(newErrorDiv, logsSection);
      }
    } else if (!job.error && errorDiv) {
      errorDiv.remove();
    } else if (job.error && errorDiv) {
      errorDiv.textContent = "";
      const strong = document.createElement("strong");
      strong.textContent = "Error:";
      errorDiv.appendChild(strong);
      errorDiv.appendChild(document.createTextNode(" " + job.error));
    }

    // Update button visibility
    this.updateDetailsButtons(job);
  }

  updateDetailsButtons(job) {
    const canCancel = CANCEL_STATUSES.has(job.status);
    const canResume = RESUME_STATUSES.has(job.status) && job.platform === "youtube" && job.hasStaging;
    const canReinit = REINIT_STATUSES.has(job.status);
    const canMux = MUX_STATUSES.has(job.status) && job.hasSegments;
    const canDelete = DELETE_STATUSES.has(job.status);
    const hasFile = job.status === "Finished" && job.filename;
    const isActive = ["Upcoming", "Live", "Downloading", "Muxing"].includes(
      job.status,
    );

    document.getElementById("details-cancel-btn").style.display = canCancel
      ? ""
      : "none";
    document.getElementById("details-resume-btn").style.display = canResume
      ? ""
      : "none";
    document.getElementById("details-reinit-btn").style.display = canReinit
      ? ""
      : "none";
    document.getElementById("details-mux-btn").style.display = canMux
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
      (hasFile || isActive) && isLocalhost ? "" : "none";
    document.getElementById("details-play-btn").style.display = hasFile
      ? ""
      : "none";
  }

  renderJobDetails(job) {
    const content = document.getElementById("job-details-content");
    const statusClass = job.status.toLowerCase().replace("?", "");

    // Show segment counts for Live/Downloading/Muxing/Finished status.
    // Always create the row for eligible statuses so updateJobDetails can
    // update it when the first segments arrive (avoids silent no-op when
    // the dialog was opened before any segments were recorded).
    const showSegments = ["Live", "Downloading", "Muxing", "Finished"].includes(job.status);
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
      const segDisplayValue = isTwitchSegments ? vDisplay : `V: ${vDisplay} | A: ${aDisplay}`;
      segmentInfo = `<div class="details-row" id="segments-row">
          <span class="details-label">Segments:</span>
          <span class="details-value" data-field="segments">${this.escapeHtml(segDisplayValue)}</span>
        </div>`;
    }

    const isTwitch = job.platform === "twitch";
    // Extract Twitch login from URL or channelName for embed
    const twitchLogin = isTwitch
      ? (job.url ? job.url.replace(/.*twitch\.tv\//, "").split("/")[0].split("?")[0] : job.channelName || "").toLowerCase()
      : "";
    const twitchVodId = isTwitch && job.videoId.startsWith("tw_v") ? job.videoId.slice(4) : "";

    // Build embed HTML
    let embedHtml;
    if (isTwitch && twitchVodId) {
      embedHtml = `<iframe class="details-embed" src="https://player.twitch.tv/?video=${this.escapeHtml(twitchVodId)}&parent=${this.escapeHtml(location.hostname)}&autoplay=false&muted=true" allowfullscreen></iframe>`;
    } else if (isTwitch && twitchLogin) {
      embedHtml = `<iframe class="details-embed" src="https://player.twitch.tv/?channel=${this.escapeHtml(twitchLogin)}&parent=${this.escapeHtml(location.hostname)}&autoplay=false&muted=true" allowfullscreen></iframe>`;
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
              <sl-badge class="status ${this.escapeHtml(statusClass)}" variant="primary">${this.escapeHtml(this.displayStatus(job.status))}</sl-badge>
              ${this.watchPillHtml(job)}
            </span>
          </div>
          ${job.status === "Finished" ? `
          <div class="details-row" id="watch-actions-row">
            <span class="details-label"></span>
            <span class="details-value">
              ${!job.watched ? `<sl-button id="details-mark-watched" variant="success" size="small"><sl-icon slot="prefix" name="eye"></sl-icon> Mark Watched</sl-button>` : ""}
              ${job.watched || job.resumePosition != null ? `<sl-button id="details-mark-unwatched" variant="neutral" size="small"><sl-icon slot="prefix" name="eye-slash"></sl-icon> Mark Unwatched</sl-button>` : ""}
            </span>
          </div>` : ""}
          ${job.chatStatus ? (() => {
            const chatVariantMap = { downloading: "primary", finished: "success", error: "danger", unavailable: "neutral", pending: "neutral" };
            const chatVariant = chatVariantMap[job.chatStatus] || "neutral";
            return `
          <div class="details-row">
            <span class="details-label">Chat:</span>
            <span class="details-value" data-field="chat">
              <sl-badge variant="${chatVariant}">${this.escapeHtml(job.chatStatus)}</sl-badge>
              ${job.totalChatMessages ? ` (${this.escapeHtml(job.totalChatMessages.toLocaleString())} messages)` : ""}
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
            <span class="details-value" data-field="progress">${this.escapeHtml(this.formatProgress(job))}</span>
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
            <span class="details-value" data-field="speed">${this.escapeHtml(job.speed || "")}</span>
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
            <span class="details-value" data-timestamp="${this.escapeHtml(job.createdAt)}" title="${new Date(job.createdAt).toLocaleString()}">${this.escapeHtml(this.formatRelativeTime(job.createdAt))}</span>
          </div>
          ${job.downloadStartedAt ? `
          <div class="details-row">
            <span class="details-label">DL Started:</span>
            <span class="details-value" data-timestamp="${this.escapeHtml(job.downloadStartedAt)}" title="${new Date(job.downloadStartedAt).toLocaleString()}">${this.escapeHtml(this.formatRelativeTime(job.downloadStartedAt))}</span>
          </div>
          ` : ""}
          <div class="details-row">
            <span class="details-label">Updated:</span>
            <span class="details-value" data-field="updated" data-timestamp="${this.escapeHtml(job.updatedAt)}" title="${new Date(job.updatedAt).toLocaleString()}">${this.escapeHtml(this.formatRelativeTime(job.updatedAt))}</span>
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
            const tsAttr = isScheduled ? "" : ` data-timestamp="${this.escapeHtml(job.streamStartTime)}" title="${new Date(job.streamStartTime).toLocaleString()}"`;
            return `
          <div class="details-row">
            <span class="details-label">${label}:</span>
            <span class="details-value"${tsAttr}>${this.escapeHtml(value)}</span>
          </div>
          ${isScheduled ? `
          <div class="details-row">
            <span class="details-label">Starts In:</span>
            <span class="details-value" data-timestamp-countdown="${this.escapeHtml(job.streamStartTime)}">${this.escapeHtml((() => { const diff = Math.floor((new Date(job.streamStartTime).getTime() - Date.now()) / 1000); return diff > 0 ? this.formatDurationSeconds(diff) : "Now"; })())}</span>
          </div>` : ""}`;
          })() : ""}
          ${job.streamEndTime ? `
          <div class="details-row">
            <span class="details-label">Stream End:</span>
            <span class="details-value" data-timestamp="${this.escapeHtml(job.streamEndTime)}" title="${new Date(job.streamEndTime).toLocaleString()}">${this.escapeHtml(this.formatRelativeTime(job.streamEndTime))}</span>
          </div>
          ` : ""}
          ${job.lengthSeconds && job.lengthSeconds > 0 ? `
          <div class="details-row">
            <span class="details-label">Duration:</span>
            <span class="details-value">${this.escapeHtml(this.formatDurationSeconds(job.lengthSeconds))}</span>
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
              : `itag ${this.escapeHtml(job.selectedVideoItag)}`
          }</span>
        </div>
        ` : ""}
        ${job.selectedAudioItag != null ? `
        <div class="details-row">
          <span class="details-label">Audio Format:</span>
          <span class="details-value">${
            job.selectedAudioItag === -1
              ? "None (video only)"
              : `itag ${this.escapeHtml(job.selectedAudioItag)}`
          }</span>
        </div>
        ` : ""}
        ${job.startTime != null || job.endTime != null ? `
        <div class="details-row">
          <span class="details-label">Time Range:</span>
          <span class="details-value">
            ${this.escapeHtml(this.formatTimestamp(job.startTime || 0))} - ${job.endTime != null ? this.escapeHtml(this.formatTimestamp(job.endTime)) : "end"}
            ${job.endTime != null && job.startTime != null ? ` (${this.escapeHtml(this.formatTimestamp(job.endTime - job.startTime))})` : ""}
          </span>
        </div>
        ` : ""}
      </div>
      ` : ""}

      ${job.trims && job.trims.length > 0 ? `
      <sl-details summary="Trims (${this.escapeHtml(job.trims.length)})" open class="details-section">
        <div class="trim-list">
          ${job.trims.map(trim => {
            const range = `${this.escapeHtml(this.formatTimestamp(trim.startTime))} - ${this.escapeHtml(this.formatTimestamp(trim.endTime))}`;
            const duration = `${this.escapeHtml(Math.floor(trim.duration))}s`;
            const size = trim.fileSize ? this.escapeHtml(this.formatBytes(trim.fileSize)) : '?';
            return `
              <div class="trim-item" style="display: flex; justify-content: space-between; align-items: center; padding: 8px; border-bottom: 1px solid var(--sl-color-neutral-200);">
                <span>
                  <strong>${range}</strong> (${duration}, ${size})
                </span>
                <sl-button size="small" variant="danger" data-delete-trim data-job-id="${this.escapeHtml(job.id)}" data-trim-id="${this.escapeHtml(trim.id)}">
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

      ${(() => {
        const hasResolution = job.videoWidth && job.videoHeight;
        const hasFps = job.videoFps && job.videoFps > 0;
        const hasFileSize = job.fileSize && job.fileSize > 0;
        const isFinished = job.status === "Finished";
        const hasFinishedSegs = isFinished && (job.lastVideoSeq || job.lastAudioSeq);
        const hasGaps = job.gaps && job.gaps.length > 0;
        if (!hasResolution && !hasFps && !hasFileSize && !hasFinishedSegs && !hasGaps) return "";

        let rows = "";
        if (hasResolution) {
          rows += `<div class="details-row">
            <span class="details-label">Resolution:</span>
            <span class="details-value">${this.escapeHtml(job.videoWidth)}x${this.escapeHtml(job.videoHeight)}</span>
          </div>`;
        }
        if (hasFps) {
          rows += `<div class="details-row">
            <span class="details-label">FPS:</span>
            <span class="details-value">${this.escapeHtml(job.videoFps)}</span>
          </div>`;
        }
        if (hasFileSize) {
          rows += `<div class="details-row">
            <span class="details-label">File Size:</span>
            <span class="details-value">${this.escapeHtml(this.formatBytes(job.fileSize))}</span>
          </div>`;
        }
        if (hasFinishedSegs) {
          const isTwitchSeg = job.platform === "twitch";
          const vCurrent = job.lastVideoSeq || 0;
          const aCurrent = job.lastAudioSeq || 0;
          const vTotal = job.totalVideoSeq;
          const aTotal = job.totalAudioSeq;
          const vDisplay = vTotal ? `${vCurrent}/${vTotal}` : vCurrent;
          const aDisplay = aTotal ? `${aCurrent}/${aTotal}` : aCurrent;
          const segValue = isTwitchSeg ? vDisplay : `V: ${vDisplay} | A: ${aDisplay}`;
          rows += `<div class="details-row">
            <span class="details-label">Segments:</span>
            <span class="details-value">${this.escapeHtml(segValue)}</span>
          </div>`;
        }
        if (hasGaps) {
          let videoGaps = 0, audioGaps = 0;
          for (const g of job.gaps) {
            if (g.stream === "video") videoGaps++;
            else if (g.stream === "audio") audioGaps++;
          }
          const parts = [];
          if (videoGaps > 0) parts.push(`video: ${videoGaps}`);
          if (audioGaps > 0) parts.push(`audio: ${audioGaps}`);
          const detail = parts.length > 0 ? ` (${parts.join(", ")})` : "";
          rows += `<div class="details-row">
            <span class="details-label">Gaps:</span>
            <span class="details-value" style="color: var(--sl-color-warning-600)">${this.escapeHtml(job.gaps.length)} segments${this.escapeHtml(detail)}</span>
          </div>`;
        }
        return `<div class="details-section"><strong>Media:</strong>${rows}</div>`;
      })()}

      ${(() => {
        if (!job.segments || job.segments.length === 0) return "";
        let segRows = "";
        job.segments.forEach((seg, i) => {
          const dur = seg.durationSeconds ? `${Math.round(seg.durationSeconds)}s` : "—";
          const size = seg.fileSize ? this.formatBytes(seg.fileSize) : "—";
          const res = seg.videoWidth && seg.videoHeight ? `${this.escapeHtml(seg.videoWidth)}x${this.escapeHtml(seg.videoHeight)}` : "";
          segRows += `<div class="details-row" style="padding-left:8px;">
            <span class="details-label">Segment ${this.escapeHtml(i)}:</span>
            <span class="details-value">${this.escapeHtml(seg.quality)} — ${this.escapeHtml(dur)} — ${this.escapeHtml(size)}${res ? ` — ${res}` : ""}</span>
          </div>`;
        });
        return `<div class="details-section"><strong>Quality Segments:</strong>${segRows}</div>`;
      })()}

      ${job.description ? `
      <div class="details-section">
        <strong>Description:</strong>
        <div style="white-space: pre-wrap; word-break: break-word; color: var(--sl-color-neutral-600); margin-top: 4px; font-size: 0.9em;">${this.escapeHtml(job.description)}</div>
      </div>
      ` : ""}

      <div class="details-section">
        <strong>Job Logs:</strong>
        <div class="details-logs" id="job-logs-content">Loading logs...</div>
      </div>
    `;

    // Update button visibility
    this.updateDetailsButtons(job);
  }

  async loadJobLogs(jobId) {
    // Abort any in-flight log fetch (e.g. user switched to a different job)
    if (this._jobLogsAbort) this._jobLogsAbort.abort();
    this._jobLogsAbort = new AbortController();

    try {
      const response = await fetch(`/api/jobs/${jobId}/logs`, { signal: this._jobLogsAbort.signal });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const logs = await response.json();
      // Only update if this job is still selected
      if (this.selectedJobId !== jobId) return;
      const logsEl = document.getElementById("job-logs-content");
      if (logsEl) {
        logsEl.textContent =
          Array.isArray(logs) && logs.length > 0 ? logs.join("\n") : "No logs for this job yet.";
      }
    } catch (e) {
      if (e.name === "AbortError") return; // Superseded by a new fetch
      console.error("Failed to load job logs:", e);
      const logsEl = document.getElementById("job-logs-content");
      if (logsEl) logsEl.textContent = "Failed to load logs.";
    }
  }

  // ===== Job Actions =====

  async addVideo() {
    const input = document.getElementById("video-url-input");
    const submitBtn = document.getElementById("add-submit-btn");
    if (submitBtn.disabled) return; // Prevent double-submit (Enter key bypasses disabled button)
    const value = input.value.trim();

    if (!value) return;

    // Try unified media target extraction
    const target = this.extractMediaTarget(value);
    if (!target) {
      this.showToast("Invalid YouTube/Twitch URL or ID", "warning");
      return;
    }

    // Show loading state and prevent double-submit
    submitBtn.loading = true;
    submitBtn.disabled = true;

    try {
      let body;

      if (target.platform === "twitch") {
        // Reject clips client-side (not supported)
        if (target.type === "clip") {
          this.showToast("Twitch clips are not supported. Use a channel or VOD URL.", "warning");
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

          // Validate time range if both are provided
          if (body.startTime != null && body.endTime != null && body.endTime <= body.startTime) {
            this.showToast("End time must be after start time", "warning");
            submitBtn.loading = false;
            submitBtn.disabled = false;
            return;
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
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to add video", "danger");
      }
    } catch (e) {
      this.showToast("Failed to add video: " + e.message, "danger");
    } finally {
      submitBtn.loading = false;
      submitBtn.disabled = false;
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
      if (nums.some(n => isNaN(n) || n < 0)) return null;
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
    if (!videoId) {
      // Non-YouTube URL (e.g. Twitch) — hide stale format options
      document.getElementById("format-skeleton").style.display = "none";
      document.getElementById("format-selection").style.display = "none";
      this._lastFormatVideoId = null;
      return;
    }

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

      // Discard stale response if user changed the URL during fetch
      if (this._lastFormatVideoId !== videoId) return;

      const data = await response.json();
      await this.populateFormatSelects(data);

      formatSkeleton.style.display = "none";
      formatSection.style.display = "block";
    } catch (e) {
      // Clear cache so the user can retry (toggling advanced off/on)
      if (this._lastFormatVideoId === videoId) {
        this._lastFormatVideoId = null;
      }
      formatSkeleton.style.display = "none";
      // Keep format section hidden on error — empty selects are confusing
      formatSection.style.display = "none";
      console.error("Failed to fetch formats:", e);
      this.showToast("Could not load format options: " + e.message, "warning");
    }
  }

  /**
   * Populate video and audio format select dropdowns
   */
  async populateFormatSelects(data) {
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

    // Wait for Shoelace to register the new options before setting values.
    // Without this, the select's internal state may not recognize the options yet.
    await Promise.all([
      videoSelect.updateComplete?.catch(() => {}),
      audioSelect.updateComplete?.catch(() => {}),
    ]);

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
    let url = job.url;
    if (!url) {
      if (job.platform === "twitch") {
        url = `https://www.twitch.tv/${job.channelName || job.videoId}`;
      } else {
        url = `https://www.youtube.com/watch?v=${job.videoId}`;
      }
    }
    try {
      const parsed = new URL(url);
      if (parsed.protocol === "https:" || parsed.protocol === "http:") {
        window.open(url, "_blank", "noopener");
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
    const jobId = this.selectedJobId;

    // Close the details dialog
    document.getElementById("details-dialog").hide();

    // Flag prevents the sl-tab-show handler from calling loadPlayerJobList
    // (which would race with our call and potentially overwrite the selection).
    // Cleared after our own loadPlayerJobList finishes, not synchronously,
    // because Shoelace may fire sl-tab-show asynchronously after show().
    this._playerOpeningFromDetails = true;

    // Switch to the Player tab
    const tabGroup = document.querySelector("sl-tab-group");
    tabGroup.show("player");

    // Initialize player if needed, then select the job
    if (!this.player.playerInitialized) {
      this.player.initPlayer();
    }

    this.player.loadPlayerJobList().then(() => {
      const select = document.getElementById("player-job-select");
      select.value = jobId;
      this.player.onPlayerJobSelect(jobId);
    }).catch(() => {}).finally(() => {
      this._playerOpeningFromDetails = false;
    });
  }

  async cancelJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;
    if (!await this.showConfirm("Are you sure you want to cancel this job?", { okLabel: "Cancel Job", okVariant: "danger" })) return;

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}/cancel`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Job cancelled", "success");
        // Update archived job status if present (no WebSocket update for archived jobs)
        const archivedJob = this.archivedJobs.find(j => j.id === id);
        if (archivedJob) {
          archivedJob.status = "Cancelled";
          this.renderArchivedJobs();
          // Update the open details dialog so buttons/status reflect the new state
          if (this.selectedJobId === id) {
            this.updateJobDetails(archivedJob);
          }
        }
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to cancel job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to cancel job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  async resumeJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}/resume`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Job resumed", "success");
        if (this.selectedJobId === id) {
          const dlg = document.getElementById("details-dialog");
          if (dlg?.open) dlg.hide();
          this.selectedJobId = null;
        }
        const archivedIdx = this.archivedJobs.findIndex(j => j.id === id);
        if (archivedIdx !== -1) {
          this.archivedJobs.splice(archivedIdx, 1);
          this.renderArchivedJobs();
        }
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to resume job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to resume job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  async reinitializeJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}/reinitialize`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Job reinitialized", "success");
        if (this.selectedJobId === id) {
          const dlg = document.getElementById("details-dialog");
          if (dlg?.open) dlg.hide();
          this.selectedJobId = null;
        }
        const archivedIdx = this.archivedJobs.findIndex(j => j.id === id);
        if (archivedIdx !== -1) {
          this.archivedJobs.splice(archivedIdx, 1);
          this.renderArchivedJobs();
        }
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to reinitialize job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to reinitialize job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  async muxJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}/mux`, {
        method: "POST",
      });
      if (response.ok) {
        this.showToast("Muxing started", "success");
        // Do NOT close dialog — job stays visible during mux
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to mux job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to mux job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
    }
  }

  async deleteJob(jobId) {
    const id = jobId || this.selectedJobId;
    if (!id || this._jobActionsInFlight.has(id)) return;
    if (!await this.showConfirm("Are you sure you want to delete this job?", { okLabel: "Delete", okVariant: "danger" })) return;

    this._jobActionsInFlight.add(id);
    try {
      const response = await fetch(`/api/jobs/${id}`, {
        method: "DELETE",
      });
      if (response.ok) {
        if (this.selectedJobId === id) {
          const dlg = document.getElementById("details-dialog");
          if (dlg?.open) dlg.hide();
          this.selectedJobId = null;
        }
        this.showToast("Job deleted", "success");
        // Optimistically remove from active jobs so the card disappears
        // immediately (instead of waiting for the next WebSocket update)
        const activeIdx = this.jobs.findIndex(j => j.id === id);
        if (activeIdx !== -1) {
          this.jobs.splice(activeIdx, 1);
          this.renderJobs();
        }
        // Remove from archived jobs if present (no WebSocket update for archived jobs)
        const archivedIdx = this.archivedJobs.findIndex(j => j.id === id);
        if (archivedIdx !== -1) {
          this.archivedJobs.splice(archivedIdx, 1);
          this.renderArchivedJobs();
        }
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.showToast(data.error || "Failed to delete job", "danger");
      }
    } catch (e) {
      this.showToast("Failed to delete job: " + e.message, "danger");
    } finally {
      this._jobActionsInFlight.delete(id);
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
        const error = await response.json().catch(() => ({ error: response.statusText }));
        throw new Error(error.error || 'Failed to create trim');
      }

      const { trim } = await response.json();
      this.showToast('Trim created successfully', 'success');
      await this._refreshJobDetails(jobId);
      return trim;
    } catch (error) {
      this.showToast(error.message, 'danger');
      throw error;
    }
  }

  async deleteTrim(jobId, trimId) {
    if (!await this.showConfirm("Delete this trim? This cannot be undone.", { okLabel: "Delete", okVariant: "danger" })) {
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
      await this._refreshJobDetails(jobId);
    } catch (error) {
      this.showToast(error.message, 'danger');
    }
  }

  /** Fetch fresh job data and update the details dialog and jobs array. */
  async _refreshJobDetails(jobId) {
    if (this.selectedJobId !== jobId) return;
    try {
      const jobResponse = await fetch(`/api/jobs/${jobId}`, {
        cache: 'no-store',
      });
      if (jobResponse.ok) {
        const updatedJob = await jobResponse.json();
        const jobIndex = this.jobs.findIndex(j => j.id === jobId);
        if (jobIndex !== -1) {
          this.jobs[jobIndex] = updatedJob;
        } else {
          const archivedIndex = this.archivedJobs.findIndex(j => j.id === jobId);
          if (archivedIndex !== -1) {
            this.archivedJobs[archivedIndex] = updatedJob;
            this.renderArchivedJobs();
          }
        }
        this.renderJobDetails(updatedJob);
        this.loadJobLogs(jobId);
      }
    } catch {
      // Non-critical — job will sync via WebSocket
    }
  }

  openTrimDialog(job) {
    this.trimmer.open(job);
  }

  formatTimestamp(seconds) {
    return formatTimestamp(seconds);
  }

  formatBytes(bytes) {
    return formatBytes(bytes);
  }

  // ===== Log Management =====

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

  // ===== Keyboard Shortcuts =====

  setupKeyboardShortcuts() {
    document.addEventListener("keydown", (e) => {
      // Skip when typing in input fields (composedPath handles Shoelace shadow DOM)
      if (isTypingInInput(e)) return;

      // If a dialog is open, block all shortcuts and let Shoelace
      // handle Escape natively (respects sl-request-close prevention)
      if (document.querySelector("sl-dialog[open]")) {
        return;
      }

      const tabGroup = document.querySelector("sl-tab-group");
      const panels = ["tasks", "archived", "player", "imports", "files", "stats", "logs", "settings"];
      const activePanel = document.querySelector("sl-tab-panel[active]");
      const isPlayerActive = activePanel?.getAttribute("name") === "player";

      const isTasksActive = activePanel?.getAttribute("name") === "tasks";

      switch (e.key) {
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
        case "1": case "2": case "3": case "4": case "5": case "6": case "7": case "8":
          if (tabGroup) tabGroup.show(panels[parseInt(e.key) - 1]);
          e.preventDefault();
          break;
        case "ArrowUp":
        case "ArrowDown":
          if (isTasksActive) {
            this.navigateJobList(e.key === "ArrowUp" ? -1 : 1);
            e.preventDefault();
          }
          break;
        case "Enter":
          if (isTasksActive && this.focusedJobIndex >= 0) {
            const filtered = this.getFilteredJobs();
            const sorted = this._sortJobs(filtered);
            const job = sorted[this.focusedJobIndex];
            if (job) this.showJobDetails(job);
          }
          break;
        case "f": {
          const panel = activePanel?.getAttribute("name");
          const filterId = panel === "archived" ? "archived-filter" : "tasks-filter";
          const filterInput = document.querySelector(`#${filterId} .unified-filter-input`);
          if (filterInput) { filterInput.focus(); e.preventDefault(); }
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
      const card = document.querySelector(`.video-item[data-job-id="${CSS.escape(job.id)}"]`);
      if (card) {
        card.setAttribute("data-focused", "");
        card.scrollIntoView({ block: "nearest" });
      }
    }
  }

  // ===== Search/Filter =====

  getFilteredJobs() {
    return applyFilterTokens(this.jobs, this.tasksFilterTokens);
  }

  getFilteredArchivedJobs() {
    return applyFilterTokens(this.archivedJobs, this.archivedFilterTokens);
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

  // ===== Quick Actions =====

  /** Set of job IDs currently being acted on — prevents concurrent operations */
  _jobActionsInFlight = new Set();

  quickAction(action, jobId) {
    if (this._jobActionsInFlight.has(jobId)) return; // prevent double-click race
    switch (action) {
      case "cancel": this.cancelJob(jobId); break;
      case "reinitialize": this.reinitializeJob(jobId); break;
      case "resume": this.resumeJob(jobId); break;
      case "mux": this.muxJob(jobId); break;
      case "delete": this.deleteJob(jobId); break;
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

  // ===== Status Display =====

  displayStatus(status) {
    if (status === "COOKIES?") return "Auth Required";
    return status;
  }

  formatProgressTooltip(job) {
    // Show full error in tooltip (card truncates to 50 chars)
    if (job.status === "Error" && job.error) return job.error;

    const p = job.progress || "";
    // DASH: (A: 123/456 V: 789/1000 C: 50)
    const dashMatch = p.match(/\(A:\s*(\S+)\s+V:\s*(\S+)(?:\s+C:\s*(\d+))?\)/);
    if (dashMatch) {
      let tip = `Audio: ${dashMatch[1]} segments, Video: ${dashMatch[2]} segments`;
      if (dashMatch[3]) tip += `, Chat: ${parseInt(dashMatch[3]).toLocaleString()} messages`;
      return tip;
    }
    // VOD: V:95.3% C: 456
    const vodMatch = p.match(/^V:([\d.]+%)(?:\s+C:\s*(\d+))?$/);
    if (vodMatch) {
      let tip = `Video: ${vodMatch[1]} downloaded`;
      if (vodMatch[2]) tip += `, Chat: ${parseInt(vodMatch[2]).toLocaleString()} messages`;
      return tip;
    }
    // HLS: Seq: 123 C: 456
    const hlsMatch = p.match(/^Seq:\s*(\d+)(?:\s+C:\s*(\d+))?$/);
    if (hlsMatch) {
      let tip = `Segments: ${parseInt(hlsMatch[1]).toLocaleString()}`;
      if (hlsMatch[2]) tip += `, Chat: ${parseInt(hlsMatch[2]).toLocaleString()} messages`;
      return tip;
    }
    // Frontend fallback: V: 123 / A: 456
    if (job.lastVideoSeq !== undefined) {
      return `Video: ${job.lastVideoSeq || 0} segments, Audio: ${job.lastAudioSeq || 0} segments`;
    }
    return "";
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
    // Raw username (exclude reserved Twitch path segments)
    if (/^[a-zA-Z][a-zA-Z0-9_]{0,24}$/.test(trimmed) && !reserved.has(trimmed.toLowerCase())) {
      return { platform: "twitch", type: "channel", id: trimmed.toLowerCase() };
    }
    return null;
  }

  formatRelativeTime(isoDate) {
    return formatRelativeTime(isoDate);
  }

  formatDurationSeconds(totalSeconds) {
    return formatDurationSeconds(totalSeconds);
  }

  /** Returns the compact watch status pill HTML for job details. */
  watchPillHtml(job) {
    if (job.status !== "Finished") return "";
    const resumeStr = job.resumePosition != null ? formatTimestamp(job.resumePosition) : "";
    if (job.watched && resumeStr) {
      return `<span class="watch-pill watched"><svg viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3" fill="rgba(255,255,255,0.6)"/></svg>Watched · Paused at ${this.escapeHtml(resumeStr)}</span>`;
    }
    if (job.watched) {
      return `<span class="watch-pill watched"><svg viewBox="0 0 24 24" fill="currentColor" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3" fill="rgba(255,255,255,0.6)"/></svg>Watched</span>`;
    }
    if (resumeStr) {
      return `<span class="watch-pill in-progress"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>Paused at ${this.escapeHtml(resumeStr)}</span>`;
    }
    return "";
  }

  setInputValue(id, value) {
    const el = document.getElementById(id);
    if (!el) return;
    // Default to empty string for undefined/null so fields are cleared
    // when a config value is removed (e.g. omitempty field reset server-side)
    el.value = value ?? "";
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
      return isNaN(val) ? undefined : val;
    }
    const str = typeof val === "string" ? val.trim() : "";
    if (!str) return undefined;
    // Use Number() instead of parseInt() to reject partial numbers like "123abc"
    // Don't truncate — FlexDuration fields accept fractional values (e.g. 2.5 minutes).
    // Integer fields are truncated server-side by Go's int() conversion.
    const num = Number(str);
    return isNaN(num) ? undefined : num;
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

  /**
   * Show a themed confirmation dialog. Returns a Promise<boolean>.
   * @param {string} message — text to display
   * @param {object} [opts] — { okLabel, okVariant, title }
   */
  showConfirm(message, opts = {}) {
    return new Promise((resolve) => {
      const dlg = document.getElementById("confirm-dialog");
      const msg = document.getElementById("confirm-dialog-message");
      const okBtn = document.getElementById("confirm-dialog-ok");
      const cancelBtn = document.getElementById("confirm-dialog-cancel");

      dlg.label = opts.title || "Confirm";
      msg.textContent = message;

      const cleanup = (result) => {
        dlg.removeEventListener("sl-after-hide", onHide);
        resolve(result);
      };

      const onOk = () => { cleanup(true); dlg.hide(); };
      const onCancel = () => { cleanup(false); dlg.hide(); };
      const onHide = () => { cleanup(false); };

      // Replace listeners to avoid stacking.
      // Set properties on the CLONE (not the original) — Lit reactive properties
      // like `variant` aren't reflected to DOM attributes until the next render
      // cycle, so cloneNode would copy the stale attribute from the original.
      const newOk = okBtn.cloneNode(true);
      okBtn.replaceWith(newOk);
      newOk.textContent = opts.okLabel || "OK";
      newOk.variant = opts.okVariant || "primary";
      newOk.addEventListener("click", onOk);

      const newCancel = cancelBtn.cloneNode(true);
      cancelBtn.replaceWith(newCancel);
      newCancel.addEventListener("click", onCancel);

      dlg.addEventListener("sl-after-hide", onHide, { once: true });
      dlg.show();
    });
  }

  showToast(message, variant = "primary") {
    const alert = document.createElement("sl-alert");
    alert.variant = variant;
    alert.closable = true;
    alert.duration = 3000;
    alert.className = "toast-alert";

    const icon = document.createElement("sl-icon");
    icon.slot = "icon";
    icon.name = this.getIconForVariant(variant);
    alert.appendChild(icon);

    alert.appendChild(document.createTextNode(message));

    const countdown = document.createElement("div");
    countdown.className = "toast-countdown";
    alert.appendChild(countdown);

    // Remove from DOM after hide to prevent leak over long sessions
    alert.addEventListener("sl-after-hide", () => alert.remove());
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
    const refreshBtn = document.getElementById("files-refresh-btn");
    if (refreshBtn) refreshBtn.loading = true;
    try {
      const resp = await fetch("/api/files/orphaned");
      if (!resp.ok) throw new Error("Failed to fetch");
      const data = await resp.json();
      this._orphanedFiles = data;
      this.renderOrphanedFiles(data);
    } catch (err) {
      console.error("Failed to fetch orphaned files:", err);
      this._orphanedFiles = [];
      this.renderOrphanedFiles(null); // null signals error vs empty
    } finally {
      if (refreshBtn) refreshBtn.loading = false;
    }
  }

  renderOrphanedFiles(files) {
    const emptyEl = document.getElementById("files-empty");
    const tableWrapper = document.getElementById("files-table-wrapper");
    const deleteAllBtn = document.getElementById("files-delete-all-btn");

    if (files === null || (Array.isArray(files) && files.length === 0)) {
      if (emptyEl) {
        emptyEl.style.display = "";
        const msg = emptyEl.querySelector("p");
        if (msg) msg.textContent = files === null
          ? "Failed to load orphaned files. Try refreshing."
          : "No orphaned files found.";
      }
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
      const sizeStr = `<span>${this.escapeHtml(this.formatBytes(file.size))}</span>`;
      const modStr = `<span data-timestamp="${this.escapeHtml(file.modified)}" title="${new Date(file.modified).toLocaleString()}">${this.escapeHtml(this.formatRelativeTime(file.modified))}</span>`;

      let jobStr = "";
      if (file.jobTitle) {
        jobStr = `<span class="files-job-info" title="${this.escapeHtml(file.jobId)}">${this.escapeHtml(file.jobTitle)} (${this.escapeHtml(file.jobStatus)})</span>`;
      } else {
        jobStr = `<span class="files-job-info">—</span>`;
      }

      const deleteBtn = `<sl-icon-button name="trash" label="Delete" class="files-delete-btn" data-path="${this.escapeHtml(file.path)}"></sl-icon-button>`;

      row.innerHTML = typeBadge + pathStr + sizeStr + modStr + jobStr + deleteBtn;
      table.appendChild(row);
    }

    // Event delegation for delete buttons — attach once
    if (!table._filesDelegated) {
      table._filesDelegated = true;
      table.addEventListener("click", (e) => {
        const btn = e.target.closest(".files-delete-btn");
        if (btn) this.deleteOrphanedFile(btn.dataset.path);
      });
    }
  }

  async deleteOrphanedFile(path) {
    if (!await this.showConfirm(`Delete this file?\n\n${path}`, { okLabel: "Delete", okVariant: "danger" })) return;

    try {
      const resp = await fetch("/api/files/orphaned", {
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
      await this.fetchOrphanedFiles();
    } catch (err) {
      this.showToast("Failed to delete file", "danger");
    }
  }

  async deleteAllOrphanedFiles() {
    if (!this._orphanedFiles || this._orphanedFiles.length === 0) return;
    const fileCount = this._orphanedFiles.length;
    if (!await this.showConfirm(`Delete ${fileCount === 1 ? "this" : `all ${fileCount}`} orphaned file${fileCount === 1 ? "" : "s"}?`, { okLabel: "Delete All", okVariant: "danger" })) return;

    const deleteAllBtn = document.getElementById("files-delete-all-btn");
    if (deleteAllBtn) { deleteAllBtn.loading = true; deleteAllBtn.disabled = true; }

    const paths = this._orphanedFiles.map((f) => f.path);
    try {
      const resp = await fetch("/api/files/orphaned", {
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
      await this.fetchOrphanedFiles();
    } catch (err) {
      this.showToast("Failed to delete files", "danger");
    } finally {
      if (deleteAllBtn) {
        deleteAllBtn.loading = false;
        // Let renderOrphanedFiles control disabled state — it disables the
        // button when the list is empty. Only force-enable here if the
        // fetch/render didn't run (e.g. DELETE request failed).
        const hasFiles = this._orphanedFiles && this._orphanedFiles.length > 0;
        deleteAllBtn.disabled = !hasFiles;
      }
    }
  }

  /** Return selected jobs for the currently active panel. */
  _getSelectedJobs() {
    const panel = document.querySelector("sl-tab-panel[active]")?.getAttribute("name");
    if (panel === "archived") {
      return this.archivedJobs.filter(j => this._selectedArchivedJobs.has(j.id));
    }
    return this.jobs.filter(j => this._selectedTaskJobs.has(j.id));
  }

  updateBatchActionBar() {
    const bar = document.getElementById("batch-action-bar");
    if (!bar) return;

    const count = this._activeSelectionSet().size;
    if (count === 0) {
      if (bar.style.display === "none") return; // already hidden
      bar.classList.remove("visible");
      document.body.classList.remove("batch-bar-active");
      bar.addEventListener("transitionend", () => {
        if (!bar.classList.contains("visible")) {
          bar.style.display = "none";
        }
      }, { once: true });
      return;
    }
    bar.style.display = "";
    bar.classList.add("visible");
    document.body.classList.add("batch-bar-active");
    document.getElementById("batch-count").textContent = `${count} selected`;

    // Update Select All label with count when filters are active
    const selectAllBtn = document.getElementById("batch-select-all");
    if (selectAllBtn) {
      const panel = document.querySelector("sl-tab-panel[active]")?.getAttribute("name");
      const isFiltered = panel === "archived"
        ? this.archivedFilterTokens.length > 0
        : this.tasksFilterTokens.length > 0;
      if (isFiltered) {
        const totalVisible = panel === "archived"
          ? this.getFilteredArchivedJobs().length
          : this.getFilteredJobs().length;
        selectAllBtn.textContent = `Select All (${totalVisible})`;
      } else {
        selectAllBtn.textContent = "Select All";
      }
    }

    const selectedJobs = this._getSelectedJobs();
    const canCancel = selectedJobs.some(j => CANCEL_STATUSES.has(j.status));
    const canResume = selectedJobs.some(j => RESUME_STATUSES.has(j.status));
    const canReinit = selectedJobs.some(j => REINIT_STATUSES.has(j.status));
    const canDelete = selectedJobs.some(j => DELETE_STATUSES.has(j.status));

    document.getElementById("batch-cancel").style.display = canCancel ? "" : "none";
    document.getElementById("batch-resume").style.display = canResume ? "" : "none";
    document.getElementById("batch-reinit").style.display = canReinit ? "" : "none";
    document.getElementById("batch-delete").style.display = canDelete ? "" : "none";
    const canWatch = selectedJobs.some(j => j.status === "Finished" && !j.watched);
    const canUnwatch = selectedJobs.some(j => j.status === "Finished" && (j.watched || j.resumePosition != null));
    document.getElementById("batch-watched").style.display = canWatch ? "" : "none";
    document.getElementById("batch-unwatched").style.display = canUnwatch ? "" : "none";

    // Hide divider between standard and watch buttons when either side is empty
    const divider = bar.querySelector("sl-divider");
    if (divider) {
      const hasStandard = canCancel || canResume || canReinit || canDelete;
      const hasWatch = canWatch || canUnwatch;
      divider.style.display = (hasStandard && hasWatch) ? "" : "none";
    }
  }

  async batchAction(action) {
    const selectedJobs = this._getSelectedJobs();
    let targets, confirmMsg, apiCall;

    switch (action) {
      case "cancel":
        targets = selectedJobs.filter(j => CANCEL_STATUSES.has(j.status));
        confirmMsg = `Cancel ${targets.length} job${targets.length !== 1 ? "s" : ""}?`;
        apiCall = (id) => fetch(`/api/jobs/${id}/cancel`, { method: "POST" });
        break;
      case "resume":
        targets = selectedJobs.filter(j => RESUME_STATUSES.has(j.status));
        confirmMsg = `Resume ${targets.length} job${targets.length !== 1 ? "s" : ""}?`;
        apiCall = (id) => fetch(`/api/jobs/${id}/resume`, { method: "POST" });
        break;
      case "reinitialize":
        targets = selectedJobs.filter(j => REINIT_STATUSES.has(j.status));
        confirmMsg = `Reinitialize ${targets.length} job${targets.length !== 1 ? "s" : ""}?`;
        apiCall = (id) => fetch(`/api/jobs/${id}/reinitialize`, { method: "POST" });
        break;
      case "delete":
        targets = selectedJobs.filter(j => DELETE_STATUSES.has(j.status));
        confirmMsg = `Delete ${targets.length} job${targets.length !== 1 ? "s" : ""}?`;
        apiCall = (id) => fetch(`/api/jobs/${id}`, { method: "DELETE" });
        break;
      case "watched":
        targets = selectedJobs.filter(j => j.status === "Finished" && !j.watched);
        confirmMsg = `Mark ${targets.length} job${targets.length !== 1 ? "s" : ""} as watched?`;
        apiCall = () => fetch("/api/jobs/batch/watched", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jobIds: targets.map(j => j.id) }),
        });
        break;
      case "unwatched":
        targets = selectedJobs.filter(j => j.status === "Finished" && (j.watched || j.resumePosition != null));
        confirmMsg = `Mark ${targets.length} job${targets.length !== 1 ? "s" : ""} as unwatched?`;
        apiCall = () => fetch("/api/jobs/batch/watched", {
          method: "DELETE",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jobIds: targets.map(j => j.id) }),
        });
        break;
    }

    if (!targets || targets.length === 0) return;

    const confirmed = await this.showConfirm(confirmMsg, {
      title: "Batch " + action.charAt(0).toUpperCase() + action.slice(1),
      okLabel: action.charAt(0).toUpperCase() + action.slice(1),
      okVariant: action === "delete" ? "danger" : action === "cancel" ? "warning" : "primary"
    });
    if (!confirmed) return;

    let succeeded, failed;
    if (action === "watched" || action === "unwatched") {
      // Single batch API call
      try {
        const res = await apiCall();
        succeeded = res.ok ? targets.length : 0;
        failed = res.ok ? 0 : targets.length;
      } catch {
        succeeded = 0;
        failed = targets.length;
      }
    } else {
      // Per-job API calls
      const results = await Promise.allSettled(targets.map(j => apiCall(j.id)));
      succeeded = results.filter(r => r.status === "fulfilled" && r.value?.ok).length;
      failed = targets.length - succeeded;
    }

    this._activeSelectionSet().clear();
    this.updateBatchActionBar();

    if (failed === 0) {
      const verbs = { delete: "Deleted", cancel: "Cancelled", resume: "Resumed", reinitialize: "Reinitialized", watched: "Marked watched", unwatched: "Marked unwatched" };
      const verb = verbs[action] || action;
      this.showToast(`${verb}: ${succeeded} job${succeeded !== 1 ? "s" : ""}`, "success");
    } else {
      this.showToast(`${succeeded} of ${targets.length} succeeded. ${failed} failed.`, "warning");
    }
  }

  /**
   * Preserve computed hasStaging/hasSegments fields from oldJobs onto newJobs.
   * WebSocket bulk updates deliver raw DB objects without these enriched fields;
   * carrying them forward avoids Resume/Mux buttons flickering out in the details dialog.
   */
  _preserveStagingFields(oldJobs, newJobs) {
    if (!oldJobs?.length || !newJobs?.length) return;
    const oldMap = new Map(oldJobs.map(j => [j.id, j]));
    for (const job of newJobs) {
      const old = oldMap.get(job.id);
      if (!old) continue;
      if (job.hasStaging === undefined && old.hasStaging !== undefined) {
        job.hasStaging = old.hasStaging;
      }
      if (job.hasSegments === undefined && old.hasSegments !== undefined) {
        job.hasSegments = old.hasSegments;
      }
    }
  }
}

// Intercept fetch to detect 401 (session expired) and redirect to login
const _originalFetch = window.fetch;
let _reloading = false;
window.fetch = async function (...args) {
  const response = await _originalFetch.apply(this, args);
  if (response.status === 401 && !_reloading) {
    // Don't redirect for auth endpoints themselves
    const url = typeof args[0] === "string" ? args[0] : args[0]?.url || "";
    if (!url.includes("/api/auth/")) {
      // Session expired — reload to get login page
      _reloading = true;
      window.location.reload();
    }
  }
  return response;
};

// Initialize app when DOM is ready
document.addEventListener("DOMContentLoaded", () => {
  window.app = new MoomboxApp();
});
