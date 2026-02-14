/**
 * Moombox Dashboard Application
 */

const ALL_NOTIFICATION_EVENTS = [
  { id: "found", label: "Found" },
  { id: "added", label: "Added" },
  { id: "scheduled", label: "Scheduled" },
  { id: "live", label: "Live" },
  { id: "downloading", label: "Downloading" },
  { id: "muxing", label: "Muxing" },
  { id: "finished", label: "Finished" },
  { id: "error", label: "Error" },
  { id: "cancelled", label: "Cancelled" },
  { id: "auth", label: "Auth" },
];
const ALL_EVENT_IDS = ALL_NOTIFICATION_EVENTS.map((e) => e.id);

class MoomboxApp {
  constructor() {
    this.ws = null;
    this.jobs = [];
    this.archivedJobs = [];
    this.logs = [];
    this.config = null;
    this.selectedJobId = null;
    this.editingChannelId = null;
    this.reconnectAttempts = 0;
    this.maxReconnectAttempts = 10;
    this.reconnectDelay = 1000;
    this.currentSetupStep = 1;

    // Import state
    this.importFile = null;
    this.importUploading = false;
    this.importInitialized = false;

    // Player state
    this.playerJob = null;
    this.playerChatData = null;
    this.playerChatMessages = [];
    this.playerAutoScroll = true;
    this.playerScrollLock = false;
    this.playerActiveChatIndex = 0;
    this.nicoEnabled = true;
    this.nicoLaneCount = 15;
    this.nicoLaneAvail = [];
    this.nicoLastSpawnMs = -1;
    this.playerInitialized = false;

    this.init();
  }

  async init() {
    // Check if this is a first-run (no config)
    const isFirstRun = await this.checkFirstRun();

    if (isFirstRun) {
      this.showSetupWizard();
    } else {
      this.setupEventListeners();
      this.setupSettingsListeners();
      this.connectWebSocket();
      this.loadConfig();
      this.loadStatus();
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

  showSetupWizard() {
    document.getElementById("setup-overlay").style.display = "flex";
    this.setupSetupListeners();
  }

  hideSetupWizard() {
    document.getElementById("setup-overlay").style.display = "none";
    // Now initialize the main app
    this.setupEventListeners();
    this.setupSettingsListeners();
    this.connectWebSocket();
    this.loadConfig();
    this.loadStatus();
  }

  setupSetupListeners() {
    // Step navigation: next buttons
    document.getElementById("setup-next-1").addEventListener("click", () => this.goToSetupStep(2));
    document.getElementById("setup-next-2").addEventListener("click", () => this.goToSetupStep(3));
    document.getElementById("setup-next-3").addEventListener("click", () => this.goToSetupStep(4));
    document.getElementById("setup-next-4").addEventListener("click", () => this.goToSetupStep(5));

    // Step navigation: back buttons
    document.getElementById("setup-back-2").addEventListener("click", () => this.goToSetupStep(1));
    document.getElementById("setup-back-3").addEventListener("click", () => this.goToSetupStep(2));
    document.getElementById("setup-back-4").addEventListener("click", () => this.goToSetupStep(3));
    document.getElementById("setup-back-5").addEventListener("click", () => this.goToSetupStep(4));

    // Setup: Network access select — show warning when External is chosen
    const setupNetAccess = document.getElementById("setup-network-access");
    const setupExtWarning = document.getElementById("setup-external-warning");
    if (setupNetAccess && setupExtWarning) {
      setupNetAccess.addEventListener("sl-change", () => {
        setupExtWarning.style.display = setupNetAccess.value === "external" ? "" : "none";
      });
    }

    // Finish setup
    document.getElementById("setup-finish").addEventListener("click", () => this.finishSetup());
  }

  goToSetupStep(step) {
    // Hide all pages
    document.querySelectorAll(".setup-page").forEach((page) => {
      page.style.display = "none";
    });

    // Show target page
    document.getElementById(`setup-step-${step}`).style.display = "";

    // Update step indicators
    document.querySelectorAll(".setup-step").forEach((stepEl) => {
      const stepNum = parseInt(stepEl.dataset.step);
      stepEl.classList.remove("active", "completed");
      if (stepNum === step) {
        stepEl.classList.add("active");
      } else if (stepNum < step) {
        stepEl.classList.add("completed");
      }
    });

    this.currentSetupStep = step;
  }

  async finishSetup() {
    const finishBtn = document.getElementById("setup-finish");
    finishBtn.loading = true;
    finishBtn.disabled = true;

    // Helper to read trimmed input value
    const val = (id) => (document.getElementById(id)?.value || "").trim();
    const num = (id) => {
      const s = val(id);
      return s ? parseInt(s, 10) : undefined;
    };

    // Step 1: Output & Paths
    const outputDir = val("setup-output-dir") || undefined;
    const outputTemplate = val("setup-output-template") || undefined;
    const stagingDir = val("setup-staging-dir") || undefined;
    const databasePath = val("setup-database-path") || undefined;
    const ffmpegPath = val("setup-ffmpeg-path") || undefined;

    // Step 2: Download Preferences
    const maxResolution = num("setup-max-resolution");
    const prefer60fps = document.getElementById("setup-prefer-60fps")?.checked ?? true;
    const numParallel = num("setup-parallel-downloads");
    const downloadChat = document.getElementById("setup-download-chat")?.checked ?? true;
    const cookieFile = val("setup-cookie-file") || undefined;
    const autoCookiesEnabled = document.getElementById("setup-auto-cookies")?.checked || false;

    // Step 3: Logging
    const logLevelSelect = document.getElementById("setup-log-level");
    const logLevel = logLevelSelect ? logLevelSelect.value : undefined;
    const logFilePath = val("setup-log-file") || undefined;
    const logMaxSize = num("setup-log-max-size");
    const logMaxFiles = num("setup-log-max-files");

    // Step 4: Display & Monitoring
    const hideAge = num("setup-hide-age");
    const maxFeedItems = num("setup-max-feed-items");
    const port = num("setup-port");
    const setupNetworkAccess = document.getElementById("setup-network-access")?.value || "localhost";

    // Step 5: Channel
    const channelId = val("setup-channel-id");
    const channelName = val("setup-channel-name");
    const channelTerms = val("setup-channel-terms");
    const includeNonLive = document.getElementById("setup-include-non-live")?.checked || false;

    // Build config
    const config = {
      port: port,
      network_access: setupNetworkAccess,
      log_level: logLevel,
      log_file_path: logFilePath,
      log_max_file_size: logMaxSize,
      log_max_files: logMaxFiles,
      database_path: databasePath,
      max_feed_items: maxFeedItems,
      downloader: {
        output_directory: outputDir,
        output_template: outputTemplate,
        staging_directory: stagingDir,
        num_parallel_downloads: numParallel,
        max_video_resolution: maxResolution,
        prefer_60fps: prefer60fps,
        download_chat: downloadChat,
        ffmpeg_path: ffmpegPath,
        cookie_file: cookieFile,
      },
      tasklist: {
        hide_finished_age_days: hideAge,
      },
      auto_cookies: {
        enabled: autoCookiesEnabled,
      },
    };

    if (channelId) {
      config.channels = [
        {
          id: channelId,
          name: channelName || undefined,
          terms: channelTerms || undefined,
          include_non_live_content: includeNonLive || undefined,
        },
      ];
    }

    try {
      const response = await fetch("/api/setup/complete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(config),
      });

      if (response.ok) {
        // Install yt-dlp plugin if checkbox is checked
        const installPlugin = document.getElementById("setup-install-ytdlp-plugin")?.checked;
        if (installPlugin) {
          try {
            await fetch("/api/ytdlp-plugin/install", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ force: true }),
            });
          } catch (e) {
            console.error("Failed to install yt-dlp plugin during setup:", e);
          }
        }

        this.hideSetupWizard();
        this.showToast("Setup complete! Welcome to Moombox.", "success");

        // Trigger auto-cookie setup if enabled in wizard
        if (autoCookiesEnabled) {
          this.startAutoCookieSetup();
        }
      } else {
        const data = await response.json();
        this.showToast(data.error || "Failed to save configuration", "danger");
      }
    } catch (e) {
      this.showToast("Failed to save configuration: " + e.message, "danger");
    } finally {
      finishBtn.loading = false;
      finishBtn.disabled = false;
    }
  }

  setupEventListeners() {
    // Add video button
    document.getElementById("add-video-btn").addEventListener("click", () => {
      document.getElementById("add-dialog").show();
      document.getElementById("video-url-input").value = "";
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
      .getElementById("details-cancel-btn")
      .addEventListener("click", () => this.cancelJob());
    document
      .getElementById("details-retry-btn")
      .addEventListener("click", () => this.retryJob());
    document
      .getElementById("details-delete-btn")
      .addEventListener("click", () => this.deleteJob());

    // Clear logs
    document
      .getElementById("clear-logs-btn")
      .addEventListener("click", () => this.clearLogs());

    // Tab activation handlers
    const tabGroup = document.querySelector("sl-tab-group");
    if (tabGroup) {
      tabGroup.addEventListener("sl-tab-show", (e) => {
        if (e.detail.name === "archived") {
          this.fetchArchivedJobs();
        } else if (e.detail.name === "player") {
          if (!this.playerInitialized) {
            this.initPlayer();
          }
          this.loadPlayerJobList();
        } else if (e.detail.name === "imports") {
          if (!this.importInitialized) {
            this.initImports();
          }
        }
      });
    }

    // Cookie status click to recheck
    const cookieStatus = document.getElementById("cookie-status");
    if (cookieStatus) {
      cookieStatus.style.cursor = "pointer";
      cookieStatus.title = "Click to recheck cookies";
      cookieStatus.addEventListener("click", () => this.recheckCookies());
    }
  }

  setupSettingsListeners() {
    // Settings navigation
    const settingsNav = document.getElementById("settings-nav");
    if (settingsNav) {
      settingsNav.addEventListener("sl-select", (e) => {
        const section = e.detail.item.value;
        this.showSettingsSection(section);

        // Update active state using attribute
        settingsNav.querySelectorAll("sl-menu-item").forEach((item) => {
          if (item.value === section) {
            item.setAttribute("aria-current", "page");
          } else {
            item.removeAttribute("aria-current");
          }
        });
      });
    }

    // Save config
    const saveBtn = document.getElementById("save-config-btn");
    if (saveBtn) {
      saveBtn.addEventListener("click", () => this.saveConfig());
    }

    // Reload config
    const reloadBtn = document.getElementById("reload-config-btn");
    if (reloadBtn) {
      reloadBtn.addEventListener("click", () => this.loadConfig());
    }

    // Add channel
    const addChannelBtn = document.getElementById("add-channel-btn");
    if (addChannelBtn) {
      addChannelBtn.addEventListener("click", () =>
        this.showAddChannelDialog(),
      );
    }

    // Channel dialog buttons
    const channelCancelBtn = document.getElementById("channel-cancel-btn");
    if (channelCancelBtn) {
      channelCancelBtn.addEventListener("click", () => {
        document.getElementById("add-channel-dialog").hide();
      });
    }

    const channelSaveBtn = document.getElementById("channel-save-btn");
    if (channelSaveBtn) {
      channelSaveBtn.addEventListener("click", () => this.saveChannel());
    }

    // Add notification
    const addNotifBtn = document.getElementById("add-notification-btn");
    if (addNotifBtn) {
      addNotifBtn.addEventListener("click", () => this.addNotification());
    }

    // yt-dlp plugin install
    const ytdlpInstallBtn = document.getElementById("ytdlp-install-btn");
    if (ytdlpInstallBtn) {
      ytdlpInstallBtn.addEventListener("click", () => this.installYtdlpPlugin());
    }

    // yt-dlp plugin refresh
    const ytdlpRefreshBtn = document.getElementById("ytdlp-refresh-btn");
    if (ytdlpRefreshBtn) {
      ytdlpRefreshBtn.addEventListener("click", () => this.loadYtdlpPluginStatus());
    }

    // Auto-cookie toggle → show/hide action buttons
    const autoCookiesSwitch = document.getElementById("cfg-auto-cookies-enabled");
    if (autoCookiesSwitch) {
      autoCookiesSwitch.addEventListener("sl-change", () => this.updateAutoCookieUI());
    }

    // Auto-cookie setup button
    const autoCookieSetupBtn = document.getElementById("btn-auto-cookie-setup");
    if (autoCookieSetupBtn) {
      autoCookieSetupBtn.addEventListener("click", () => this.startAutoCookieSetup());
    }

    // Auto-cookie "I'm Logged In" button
    const autoCookieDoneBtn = document.getElementById("btn-auto-cookie-done");
    if (autoCookieDoneBtn) {
      autoCookieDoneBtn.addEventListener("click", () => this.finishAutoCookieSetup());
    }

    // Auto-cookie cancel button
    const autoCookieCancelBtn = document.getElementById("btn-auto-cookie-cancel");
    if (autoCookieCancelBtn) {
      autoCookieCancelBtn.addEventListener("click", () => this.cancelAutoCookieSetup());
    }
  }

  showSettingsSection(section) {
    // Hide all sections
    document.querySelectorAll(".settings-section").forEach((el) => {
      el.style.display = "none";
    });

    // Show selected section
    const sectionEl = document.getElementById(`settings-${section}`);
    if (sectionEl) {
      sectionEl.style.display = "";
    }

    // Load integrations data when that section is shown
    if (section === "integrations") {
      this.loadYtdlpPluginStatus();
    }
  }

  async loadConfig() {
    try {
      const response = await fetch("/api/config");
      if (response.ok) {
        this.config = await response.json();
        this.populateConfigForm();
        this.renderChannelsList();
        this.renderNotificationsList();
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
        this.updateCookieStatus(status.cookieStatus);
      }
    } catch (e) {
      console.error("Failed to load status:", e);
    }
  }

  async recheckCookies() {
    const el = document.getElementById("cookie-status");
    if (!el) return;

    // Show checking state
    const originalHtml = el.innerHTML;
    el.innerHTML = '<sl-icon name="arrow-repeat"></sl-icon> Checking...';
    el.className = "checking";

    try {
      const response = await fetch("/api/cookies/recheck", { method: "POST" });
      if (response.ok) {
        const data = await response.json();
        this.updateCookieStatus(data.cookieStatus);
        this.showToast(
          data.success
            ? "Cookies refreshed successfully"
            : "Cookie check completed",
          data.success ? "success" : "primary",
        );
      } else {
        el.innerHTML = originalHtml;
        this.showToast("Failed to recheck cookies", "danger");
      }
    } catch (e) {
      el.innerHTML = originalHtml;
      this.showToast("Failed to recheck cookies: " + e.message, "danger");
    }
  }

  updateCookieStatus(cookieStatus) {
    const el = document.getElementById("cookie-status");
    if (!el) return;

    if (!cookieStatus || !cookieStatus.found) {
      el.className = "no-cookies";
      el.innerHTML = '<sl-icon name="cookie"></sl-icon> No cookies';
    } else if (cookieStatus.authenticated) {
      el.className = "authenticated";
      el.innerHTML = '<sl-icon name="shield-check"></sl-icon> Authenticated';
    } else {
      el.className = "not-authenticated";
      el.innerHTML =
        '<sl-icon name="shield-exclamation"></sl-icon> Cookies found (not verified)';
    }
  }

  populateConfigForm() {
    if (!this.config) return;

    // General settings
    this.setInputValue("cfg-port", this.config.port);
    // Network access select
    const netAccessSelect = document.getElementById("cfg-network-access");
    const cfgExtWarning = document.getElementById("cfg-external-warning");
    if (netAccessSelect) {
      const level = this.config.network_access || "localhost";
      netAccessSelect.value = level;
      if (cfgExtWarning) {
        cfgExtWarning.style.display = level === "external" ? "" : "none";
        if (!this._netAccessListenerAdded) {
          this._netAccessListenerAdded = true;
          netAccessSelect.addEventListener("sl-change", () => {
            cfgExtWarning.style.display = netAccessSelect.value === "external" ? "" : "none";
          });
        }
      }
    }
    // Set log level select value
    const logLevelSelect = document.getElementById("cfg-log-level");
    if (logLevelSelect && this.config.log_level) {
      logLevelSelect.value = this.config.log_level;
    }
    this.setInputValue("cfg-log-file", this.config.log_file_path);
    this.setInputValue("cfg-log-max-size", this.config.log_max_file_size);
    this.setInputValue("cfg-log-max-files", this.config.log_max_files);
    this.setInputValue("cfg-database", this.config.database_path);
    this.setInputValue("cfg-max-feed-items", this.config.max_feed_items);
    this.setInputValue(
      "cfg-hide-finished-days",
      this.config.tasklist?.hide_finished_age_days,
    );

    // Downloader settings
    if (this.config.downloader) {
      this.setInputValue(
        "cfg-output-dir",
        this.config.downloader.output_directory,
      );
      this.setInputValue(
        "cfg-output-template",
        this.config.downloader.output_template,
      );
      this.setInputValue(
        "cfg-staging-dir",
        this.config.downloader.staging_directory,
      );
      this.setInputValue("cfg-ffmpeg-path", this.config.downloader.ffmpeg_path);
      this.setInputValue("cfg-cookie-file", this.config.downloader.cookie_file);
      this.setInputValue(
        "cfg-max-resolution",
        this.config.downloader.max_video_resolution,
      );
      this.setInputValue(
        "cfg-parallel-downloads",
        this.config.downloader.num_parallel_downloads,
      );
      // Download chat switch
      const downloadChatSwitch = document.getElementById("cfg-download-chat");
      if (downloadChatSwitch) {
        downloadChatSwitch.checked =
          this.config.downloader.download_chat !== false;
      }
      // Prefer 60fps switch
      const prefer60fpsSwitch = document.getElementById("cfg-prefer-60fps");
      if (prefer60fpsSwitch) {
        prefer60fpsSwitch.checked =
          this.config.downloader.prefer_60fps !== false;
      }
    }

    // Auto cookies settings
    const autoCookiesSwitch = document.getElementById("cfg-auto-cookies-enabled");
    if (autoCookiesSwitch) {
      autoCookiesSwitch.checked = this.config.auto_cookies?.enabled === true;
    }
    this.setInputValue(
      "cfg-auto-cookies-profile-dir",
      this.config.auto_cookies?.browser_profile_dir,
    );
    this.updateAutoCookieUI();
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

  async saveConfig() {
    if (!this.config) {
      this.config = { downloader: {} };
    }

    // Gather values from form
    const port = this.getInputNumber("cfg-port");
    const logLevelSelect = document.getElementById("cfg-log-level");
    const logLevel = logLevelSelect ? logLevelSelect.value : "";
    const logFile = this.getInputValue("cfg-log-file");
    const logMaxSize = this.getInputNumber("cfg-log-max-size");
    const logMaxFiles = this.getInputNumber("cfg-log-max-files");
    const database = this.getInputValue("cfg-database");
    const maxFeedItems = this.getInputNumber("cfg-max-feed-items");
    const hideFinishedDays = this.getInputNumber("cfg-hide-finished-days");

    const outputDir = this.getInputValue("cfg-output-dir");
    const outputTemplate = this.getInputValue("cfg-output-template");
    const stagingDir = this.getInputValue("cfg-staging-dir");
    const ffmpegPath = this.getInputValue("cfg-ffmpeg-path");
    const cookieFile = this.getInputValue("cfg-cookie-file");
    const maxResolution = this.getInputNumber("cfg-max-resolution");
    const parallelDownloads = this.getInputNumber("cfg-parallel-downloads");

    // Network access
    const netAccessSelect = document.getElementById("cfg-network-access");
    const networkAccess = netAccessSelect ? netAccessSelect.value : "localhost";

    // Update config object - always set values from form (empty string clears the field)
    this.config.port = port;
    this.config.network_access = networkAccess;
    this.config.log_level = logLevel || undefined;
    this.config.log_file_path = logFile || undefined;
    this.config.log_max_file_size = logMaxSize;
    this.config.log_max_files = logMaxFiles;
    this.config.database_path = database || undefined;
    this.config.max_feed_items = maxFeedItems;

    if (!this.config.tasklist) this.config.tasklist = {};
    this.config.tasklist.hide_finished_age_days = hideFinishedDays;

    if (!this.config.downloader) this.config.downloader = {};
    this.config.downloader.output_directory = outputDir || undefined;
    this.config.downloader.output_template = outputTemplate || undefined;
    this.config.downloader.staging_directory = stagingDir || undefined;
    this.config.downloader.ffmpeg_path = ffmpegPath || undefined;
    this.config.downloader.cookie_file = cookieFile || undefined;
    this.config.downloader.max_video_resolution = maxResolution;
    this.config.downloader.num_parallel_downloads = parallelDownloads;
    // Download chat switch
    const downloadChatSwitch = document.getElementById("cfg-download-chat");
    this.config.downloader.download_chat = downloadChatSwitch
      ? downloadChatSwitch.checked
      : true;
    // Prefer 60fps switch
    const prefer60fpsSwitch = document.getElementById("cfg-prefer-60fps");
    this.config.downloader.prefer_60fps = prefer60fpsSwitch
      ? prefer60fpsSwitch.checked
      : true;

    // Auto cookies
    if (!this.config.auto_cookies) this.config.auto_cookies = {};
    const autoCookiesSwitch = document.getElementById("cfg-auto-cookies-enabled");
    this.config.auto_cookies.enabled = autoCookiesSwitch
      ? autoCookiesSwitch.checked
      : false;
    const autoCookiesProfileDir = this.getInputValue("cfg-auto-cookies-profile-dir");
    this.config.auto_cookies.browser_profile_dir = autoCookiesProfileDir || undefined;

    try {
      const response = await fetch("/api/config", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(this.config),
      });

      if (response.ok) {
        this.showToast("Settings saved successfully", "success");
      } else {
        const data = await response.json();
        this.showToast(data.error || "Failed to save settings", "danger");
      }
    } catch (e) {
      this.showToast("Failed to save settings: " + e.message, "danger");
    }
  }

  renderChannelsList() {
    const container = document.getElementById("channels-list");
    if (!container || !this.config) return;

    const channels = this.config.channels || [];

    if (channels.length === 0) {
      container.innerHTML =
        '<p class="settings-help">No channels configured yet.</p>';
      return;
    }

    container.innerHTML = channels
      .map(
        (ch) => `
      <div class="channel-card" data-channel-id="${this.escapeHtml(ch.id)}">
        <div class="channel-card-header">
          <div>
            <div class="channel-card-title">${this.escapeHtml(ch.name || ch.id)}</div>
            <div class="channel-card-id">${this.escapeHtml(ch.id)}</div>
          </div>
          <div class="channel-card-actions">
            <sl-icon-button name="pencil" label="Edit" onclick="app.editChannel('${this.escapeHtml(ch.id)}')"></sl-icon-button>
            <sl-icon-button name="trash" label="Delete" onclick="app.deleteChannel('${this.escapeHtml(ch.id)}')"></sl-icon-button>
          </div>
        </div>
        ${
          ch.terms
            ? `<div class="channel-card-terms">Filter: <code>${this.escapeHtml(typeof ch.terms === "string" ? ch.terms : ch.terms.stream || "")}</code></div>`
            : ""
        }
        ${ch.include_non_live_content ? '<sl-badge variant="neutral">Include VODs</sl-badge>' : ""}
      </div>
    `,
      )
      .join("");
  }

  showAddChannelDialog(channel = null) {
    this.editingChannelId = channel ? channel.id : null;

    document.getElementById("channel-id-input").value = channel?.id || "";
    document.getElementById("channel-name-input").value = channel?.name || "";
    // Handle both old format (terms.stream) and new format (terms as string)
    const termsValue = channel?.terms
      ? typeof channel.terms === "string"
        ? channel.terms
        : channel.terms.stream || ""
      : "";
    document.getElementById("channel-terms-input").value = termsValue;
    document.getElementById("channel-include-vods").checked =
      channel?.include_non_live_content || false;

    // Update dialog label and button
    const dialog = document.getElementById("add-channel-dialog");
    dialog.label = channel ? "Edit Channel" : "Add Channel";
    document.getElementById("channel-save-btn").textContent = channel
      ? "Save Changes"
      : "Add Channel";

    // Disable ID field when editing
    document.getElementById("channel-id-input").disabled = !!channel;

    dialog.show();
  }

  editChannel(channelId) {
    const channel = this.config?.channels?.find((c) => c.id === channelId);
    if (channel) {
      this.showAddChannelDialog(channel);
    }
  }

  async saveChannel() {
    const id = document.getElementById("channel-id-input").value.trim();
    const name = document.getElementById("channel-name-input").value.trim();
    const termsValue = document
      .getElementById("channel-terms-input")
      .value.trim();
    const includeVods = document.getElementById("channel-include-vods").checked;

    if (!id) {
      this.showToast("Channel ID is required", "warning");
      return;
    }

    const channel = {
      id,
      name: name || undefined,
      terms: termsValue || undefined,
      include_non_live_content: includeVods || undefined,
    };

    try {
      const response = await fetch("/api/config/channels", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(channel),
      });

      if (response.ok) {
        document.getElementById("add-channel-dialog").hide();
        this.showToast(
          this.editingChannelId ? "Channel updated" : "Channel added",
          "success",
        );
        this.loadConfig();
      } else {
        const data = await response.json();
        this.showToast(data.error || "Failed to save channel", "danger");
      }
    } catch (e) {
      this.showToast("Failed to save channel: " + e.message, "danger");
    }
  }

  async deleteChannel(channelId) {
    if (!confirm(`Are you sure you want to remove this channel?`)) return;

    try {
      const response = await fetch(
        `/api/config/channels/${encodeURIComponent(channelId)}`,
        {
          method: "DELETE",
        },
      );

      if (response.ok) {
        this.showToast("Channel removed", "success");
        this.loadConfig();
      } else {
        const data = await response.json();
        this.showToast(data.error || "Failed to remove channel", "danger");
      }
    } catch (e) {
      this.showToast("Failed to remove channel: " + e.message, "danger");
    }
  }

  renderNotificationsList() {
    const container = document.getElementById("notifications-list");
    if (!container || !this.config) return;

    const notifications = this.config.notifications || [];

    if (notifications.length === 0) {
      container.innerHTML =
        '<p class="settings-help">No notification webhooks configured.</p>';
      return;
    }

    container.innerHTML = notifications
      .map((notif, idx) => {
        const hasFilter = Array.isArray(notif.events) && notif.events.length > 0;
        let eventsHtml;

        if (hasFilter) {
          const chips = ALL_NOTIFICATION_EVENTS.map((evt) => {
            const active = notif.events.includes(evt.id);
            const variant = active ? 'variant="primary"' : "";
            return `<sl-tag size="small" ${variant} onclick="app.toggleNotificationEvent(${idx}, '${evt.id}')">${evt.label}</sl-tag>`;
          }).join("");
          eventsHtml = `
            <div class="notification-events">
              <span class="notification-events-label">Events:</span>
              ${chips}
              <sl-tag size="small" variant="neutral" onclick="app.clearNotificationFilter(${idx})">Clear filter</sl-tag>
            </div>`;
        } else {
          eventsHtml = `
            <div class="notification-events">
              <span class="notification-events-label">Events:</span>
              <sl-tag size="small" variant="success">All events</sl-tag>
              <sl-tag size="small" variant="neutral" onclick="app.enableNotificationFilter(${idx})">Filter...</sl-tag>
            </div>`;
        }

        return `
      <div class="notification-card" data-index="${idx}">
        <div class="notification-card-header">
          <div class="notification-card-url">${this.escapeHtml(notif.url || "")}</div>
          <sl-icon-button name="trash" label="Delete" onclick="app.deleteNotification(${idx})"></sl-icon-button>
        </div>
        ${eventsHtml}
      </div>`;
      })
      .join("");
  }

  async addNotification() {
    const url = prompt("Enter webhook URL:");
    if (!url) return;

    if (!this.config.notifications) {
      this.config.notifications = [];
    }

    this.config.notifications.push({ url });
    await this.saveConfig();
    this.renderNotificationsList();
  }

  async deleteNotification(index) {
    if (!confirm("Remove this notification webhook?")) return;

    if (this.config.notifications) {
      this.config.notifications.splice(index, 1);
      await this.saveConfig();
      this.renderNotificationsList();
    }
  }

  async toggleNotificationEvent(index, eventId) {
    const notif = this.config.notifications?.[index];
    if (!notif || !Array.isArray(notif.events)) return;

    const idx = notif.events.indexOf(eventId);
    if (idx >= 0) {
      notif.events.splice(idx, 1);
    } else {
      notif.events.push(eventId);
    }

    // If all events are selected, clear the filter
    if (ALL_EVENT_IDS.every((id) => notif.events.includes(id))) {
      delete notif.events;
    }

    await this.saveConfig();
    this.renderNotificationsList();
  }

  async enableNotificationFilter(index) {
    const notif = this.config.notifications?.[index];
    if (!notif) return;

    notif.events = [...ALL_EVENT_IDS];
    await this.saveConfig();
    this.renderNotificationsList();
  }

  async clearNotificationFilter(index) {
    const notif = this.config.notifications?.[index];
    if (!notif) return;

    delete notif.events;
    await this.saveConfig();
    this.renderNotificationsList();
  }

  // ===== yt-dlp Plugin Methods =====

  async loadYtdlpPluginStatus() {
    const badge = document.getElementById("ytdlp-plugin-status-badge");
    const dirRow = document.getElementById("ytdlp-plugin-dir-row");
    const dirEl = document.getElementById("ytdlp-plugin-dir");
    const portRow = document.getElementById("ytdlp-plugin-port-row");
    const portEl = document.getElementById("ytdlp-plugin-port");
    const mismatchWarning = document.getElementById("ytdlp-port-mismatch-warning");
    const installBtn = document.getElementById("ytdlp-install-btn");
    const manualCmd = document.getElementById("ytdlp-manual-cmd");
    const extractorCmd = document.getElementById("ytdlp-extractor-cmd");

    try {
      const response = await fetch("/api/ytdlp-plugin/status");
      if (!response.ok) throw new Error("Failed to fetch status");
      const status = await response.json();

      // Update badge
      if (status.installed && !status.portMismatch) {
        badge.variant = "success";
        badge.textContent = "Installed";
      } else if (status.installed && status.portMismatch) {
        badge.variant = "warning";
        badge.textContent = "Port mismatch";
      } else {
        badge.variant = "neutral";
        badge.textContent = "Not installed";
      }

      // Show plugin dir
      dirRow.style.display = "";
      dirEl.textContent = status.pluginDir;

      // Show installed port if relevant
      if (status.installed && status.installedPort) {
        portRow.style.display = "";
        portEl.textContent = `${status.installedPort}${status.portMismatch ? ` (current: ${status.currentPort})` : ""}`;
      } else {
        portRow.style.display = "none";
      }

      // Port mismatch warning
      mismatchWarning.style.display = status.portMismatch ? "" : "none";

      // Update button text
      if (status.installed) {
        const icon = installBtn.querySelector("sl-icon");
        icon.name = "arrow-clockwise";
        installBtn.textContent = "";
        installBtn.prepend(icon);
        installBtn.append(status.portMismatch ? "Update" : "Reinstall");
      } else {
        const icon = installBtn.querySelector("sl-icon");
        icon.name = "download";
        installBtn.textContent = "";
        installBtn.prepend(icon);
        installBtn.append("Install");
      }

      // Update manual command paths
      if (status.extractedPath) {
        manualCmd.textContent = `yt-dlp --plugin-dirs "${status.extractedPath}" <URL>`;
      }
      extractorCmd.textContent = `yt-dlp --extractor-args "youtube:player-client=web;po_token=web.gvs+http://127.0.0.1:${status.currentPort}/get_pot" <URL>`;
    } catch (e) {
      badge.variant = "danger";
      badge.textContent = "Error";
      console.error("Failed to load yt-dlp plugin status:", e);
    }
  }

  async installYtdlpPlugin(force = false) {
    const installBtn = document.getElementById("ytdlp-install-btn");
    installBtn.loading = true;
    installBtn.disabled = true;

    try {
      const response = await fetch("/api/ytdlp-plugin/install", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ force }),
      });

      const data = await response.json();

      if (response.ok && data.success) {
        if (data.alreadyInstalled) {
          this.showToast("yt-dlp plugin already installed with correct port", "primary");
        } else {
          this.showToast("yt-dlp plugin installed successfully", "success");
        }
        // Refresh status
        await this.loadYtdlpPluginStatus();
      } else {
        this.showToast(data.error || "Failed to install plugin", "danger");
      }
    } catch (e) {
      this.showToast("Failed to install plugin: " + e.message, "danger");
    } finally {
      installBtn.loading = false;
      installBtn.disabled = false;
    }
  }

  // ─── Auto Cookie Methods ─────────────────────────────────────

  updateAutoCookieUI() {
    const enabled = document.getElementById("cfg-auto-cookies-enabled")?.checked;
    const actionsDiv = document.getElementById("auto-cookie-actions");
    if (actionsDiv) {
      actionsDiv.style.display = enabled ? "" : "none";
    }
    // Fetch and show detected browser info
    if (enabled) {
      this.loadAutoCookieStatus();
    }
  }

  async loadAutoCookieStatus() {
    try {
      const response = await fetch("/api/cookies/auto-status");
      if (response.ok) {
        const status = await response.json();
        const infoEl = document.getElementById("auto-cookie-browser-info");
        if (infoEl) {
          if (status.browser) {
            let info = `Detected: ${status.browser.type}`;
            if (status.lastRefresh) {
              const d = new Date(status.lastRefresh);
              info += ` | Last refresh: ${d.toLocaleString()}`;
            }
            infoEl.textContent = info;
          } else {
            infoEl.textContent = "No supported browser found";
          }
        }

        // If setup is in progress, show the dialog
        const dialog = document.getElementById("auto-cookie-setup-dialog");
        if (dialog) {
          dialog.style.display = status.setupInProgress ? "" : "none";
        }
      }
    } catch (e) {
      console.error("Failed to load auto-cookie status:", e);
    }
  }

  async startAutoCookieSetup() {
    const resultEl = document.getElementById("auto-cookie-setup-result");
    if (resultEl) resultEl.textContent = "";

    // Save config first so enabled state and profile dir are persisted
    await this.saveConfig();

    try {
      const response = await fetch("/api/cookies/auto-setup/start", { method: "POST" });
      const data = await response.json();

      if (data.success) {
        const dialog = document.getElementById("auto-cookie-setup-dialog");
        if (dialog) dialog.style.display = "";
      } else {
        this.showToast(data.error || "Failed to start setup", "danger");
      }
    } catch (e) {
      this.showToast("Failed to start auto-cookie setup: " + e.message, "danger");
    }
  }

  async finishAutoCookieSetup() {
    const resultEl = document.getElementById("auto-cookie-setup-result");
    const doneBtn = document.getElementById("btn-auto-cookie-done");
    if (doneBtn) doneBtn.loading = true;
    if (resultEl) resultEl.textContent = "Extracting cookies...";

    try {
      const response = await fetch("/api/cookies/auto-setup/finish", { method: "POST" });
      const data = await response.json();

      if (data.authenticated) {
        if (resultEl) resultEl.textContent = "";
        const dialog = document.getElementById("auto-cookie-setup-dialog");
        if (dialog) dialog.style.display = "none";
        this.showToast("Auto-cookie setup complete — authenticated!", "success");
        this.loadStatus();
      } else {
        if (resultEl) {
          resultEl.textContent = data.error || "Login not detected. Try again.";
          resultEl.style.color = "var(--sl-color-danger-600)";
        }
      }
    } catch (e) {
      if (resultEl) resultEl.textContent = "Error: " + e.message;
    } finally {
      if (doneBtn) doneBtn.loading = false;
    }
  }

  async cancelAutoCookieSetup() {
    try {
      await fetch("/api/cookies/auto-setup/cancel", { method: "POST" });
    } catch {
      // Ignore
    }
    const dialog = document.getElementById("auto-cookie-setup-dialog");
    if (dialog) dialog.style.display = "none";
    const resultEl = document.getElementById("auto-cookie-setup-result");
    if (resultEl) resultEl.textContent = "";
  }

  connectWebSocket() {
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
      this.reconnectDelay * Math.pow(1.5, this.reconnectAttempts - 1),
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
      statusEl.innerHTML = '<sl-icon name="wifi"></sl-icon> Connected';
    } else {
      statusEl.className = "disconnected";
      statusEl.innerHTML = '<sl-icon name="wifi-off"></sl-icon> Disconnected';
    }
  }

  handleMessage(message) {
    switch (message.type) {
      case "initial_state":
        this.jobs = message.payload.jobs || [];
        this.logs = message.payload.logs || [];
        this.renderJobs();
        this.renderLogs();
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

      case "job_update":
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

      case "job_added":
        // Will be included in next jobs_update
        break;

      case "log":
        this.addLog(message.payload);
        break;

      case "pong":
        // Heartbeat response
        break;
    }
  }

  renderJobs() {
    const container = document.getElementById("jobs-container");
    const emptyState = document.getElementById("empty-state");

    if (this.jobs.length === 0) {
      container.innerHTML = "";
      emptyState.style.display = "flex";
      return;
    }

    emptyState.style.display = "none";

    // Sort jobs: active first, then by updatedAt
    const sortedJobs = [...this.jobs].sort((a, b) => {
      const activeStatuses = ["Live", "Downloading", "Muxing", "Upcoming"];
      const aActive = activeStatuses.includes(a.status);
      const bActive = activeStatuses.includes(b.status);

      if (aActive && !bActive) return -1;
      if (!aActive && bActive) return 1;

      return new Date(b.updatedAt) - new Date(a.updatedAt);
    });

    container.innerHTML = sortedJobs
      .map((job) => this.renderJobItem(job))
      .join("");

    // Add click handlers
    container.querySelectorAll(".video-item").forEach((item) => {
      item.addEventListener("click", () => {
        const jobId = item.dataset.jobId;
        const job = this.jobs.find((j) => j.id === jobId);
        if (job) this.showJobDetails(job);
      });
    });
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

    // Add click handlers
    container.querySelectorAll(".video-item").forEach((item) => {
      item.addEventListener("click", () => {
        const jobId = item.dataset.jobId;
        const job = this.archivedJobs.find((j) => j.id === jobId);
        if (job) this.showJobDetails(job);
      });
    });
  }

  renderJobItem(job) {
    const statusClass = job.status.toLowerCase().replace("?", "");
    const safeVideoId = this.escapeHtml(job.videoId || job.id);
    const thumbnailUrl =
      job.thumbnailUrl || `https://i.ytimg.com/vi/${safeVideoId}/mqdefault.jpg`;
    const progress = this.formatProgress(job);
    const percent = job.percent || 0;

    return `
      <div class="video-item" data-job-id="${this.escapeHtml(job.id)}">
        <div class="thumb">
          <img src="${this.escapeHtml(thumbnailUrl)}" alt="" loading="lazy" referrerpolicy="no-referrer"
               onerror="this.src='https://i.ytimg.com/vi/${safeVideoId}/mqdefault.jpg'">
        </div>
        <div class="stream-info">
          <div class="stream-title" title="${this.escapeHtml(job.title)}">${this.escapeHtml(job.title)}</div>
          <div class="stream-author">${this.escapeHtml(job.channelName)}</div>
        </div>
        <div>
          <sl-badge class="status ${statusClass}" variant="primary">${this.escapeHtml(job.status)}</sl-badge>
        </div>
        <div class="job-progress">
          <div class="job-progress-text">${this.escapeHtml(progress)}</div>
          ${percent > 0 ? `<sl-progress-bar class="job-progress-bar" value="${percent}"></sl-progress-bar>` : ""}
        </div>
      </div>
    `;
  }

  // Update a single job card in the list without full re-render
  updateJobCard(job) {
    const card = document.querySelector(`.video-item[data-job-id="${job.id}"]`);
    if (!card) return;

    // Update status badge
    const statusBadge = card.querySelector(".status");
    if (statusBadge) {
      const statusClass = job.status.toLowerCase().replace("?", "");
      statusBadge.className = `status ${statusClass}`;
      statusBadge.setAttribute("variant", "primary");
      statusBadge.textContent = job.status;
    }

    // Update progress text
    const progressText = card.querySelector(".job-progress-text");
    if (progressText) {
      progressText.textContent = this.formatProgress(job);
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

      segmentInfo = `<div class="details-row" id="segments-row">
          <span class="details-label">Segments:</span>
          <span class="details-value" data-field="segments">V: ${vDisplay} | A: ${aDisplay}</span>
        </div>`;
    }

    content.innerHTML = `
      <div class="details-top">
        <div class="details-section">
          <iframe
            class="details-embed"
            src="https://www.youtube-nocookie.com/embed/${this.escapeHtml(job.videoId)}"
            title="YouTube video player"
            frameborder="0"
            allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture"
            referrerpolicy="strict-origin-when-cross-origin"
            allowfullscreen>
          </iframe>
        </div>

        <div class="details-section">
          <div class="details-row">
            <span class="details-label">Video ID:</span>
            <span class="details-value"><code>${this.escapeHtml(job.videoId)}</code></span>
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
            <span class="details-value">${this.escapeHtml(job.filename)}</span>
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

  async addVideo() {
    const input = document.getElementById("video-url-input");
    const value = input.value.trim();

    if (!value) return;

    const videoId = this.extractVideoId(value);
    if (!videoId) {
      this.showToast("Invalid YouTube URL or video ID", "warning");
      return;
    }

    try {
      const response = await fetch("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ videoId }),
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
    }
  }

  openJobUrl() {
    if (!this.selectedJobId) return;
    const job = this.jobs.find((j) => j.id === this.selectedJobId)
      || this.archivedJobs.find((j) => j.id === this.selectedJobId);
    if (!job) return;
    const url = job.url || `https://www.youtube.com/watch?v=${job.videoId}`;
    window.open(url, "_blank");
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
    if (!this.playerInitialized) {
      this.initPlayer();
    }

    this.loadPlayerJobList().then(() => {
      const select = document.getElementById("player-job-select");
      select.value = this.selectedJobId;
      this.onPlayerJobSelect(this.selectedJobId);
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

  addLog(log) {
    this.logs.push(log);
    if (this.logs.length > 500) {
      this.logs = this.logs.slice(-500);
    }
    // Debounce renderLogs to avoid rewriting entire textarea on every message
    if (this._logRenderTimer) clearTimeout(this._logRenderTimer);
    this._logRenderTimer = setTimeout(() => this.renderLogs(), 100);
  }

  renderLogs() {
    const textarea = document.getElementById("logs-textarea");
    const countEl = document.getElementById("log-count");

    textarea.value = this.logs.join("\n");
    countEl.textContent = `${this.logs.length} log entries`;

    // Auto-scroll to bottom
    textarea.scrollTop = textarea.scrollHeight;
  }

  clearLogs() {
    this.logs = [];
    this.renderLogs();
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

  formatRelativeTime(isoDate) {
    const date = new Date(isoDate);
    const now = new Date();
    const diffMs = now - date;
    const diffSecs = Math.floor(diffMs / 1000);

    if (diffSecs < 60) return `${diffSecs}s ago`;
    const diffMins = Math.floor(diffSecs / 60);
    if (diffMins < 60) return `${diffMins}m ago`;
    const diffHours = Math.floor(diffMins / 60);
    if (diffHours < 24) return `${diffHours}h ago`;
    const diffDays = Math.floor(diffHours / 24);
    return `${diffDays}d ago`;
  }

  formatDurationSeconds(totalSeconds) {
    const hours = Math.floor(totalSeconds / 3600);
    const minutes = Math.floor((totalSeconds % 3600) / 60);
    const seconds = totalSeconds % 60;
    if (hours > 0) return `${hours}h ${minutes}m ${seconds}s`;
    if (minutes > 0) return `${minutes}m ${seconds}s`;
    return `${seconds}s`;
  }

  // ===== Import Methods =====

  initImports() {
    this.importInitialized = true;

    const dropzone = document.getElementById("import-dropzone");
    const fileInput = document.getElementById("import-file-input");
    const submitBtn = document.getElementById("import-submit-btn");
    const clearBtn = document.getElementById("import-clear-btn");

    // Click dropzone to browse
    dropzone.addEventListener("click", () => fileInput.click());

    // File input change
    fileInput.addEventListener("change", () => {
      if (fileInput.files.length > 0) {
        this.setImportFile(fileInput.files[0]);
      }
    });

    // Drag & drop
    dropzone.addEventListener("dragover", (e) => {
      e.preventDefault();
      dropzone.classList.add("drag-over");
    });

    dropzone.addEventListener("dragleave", () => {
      dropzone.classList.remove("drag-over");
    });

    dropzone.addEventListener("drop", (e) => {
      e.preventDefault();
      dropzone.classList.remove("drag-over");
      const files = e.dataTransfer.files;
      if (files.length > 0 && files[0].name.toLowerCase().endsWith(".zip")) {
        this.setImportFile(files[0]);
      } else {
        this.showToast("Please drop a .zip file", "warning");
      }
    });

    // Clear button
    clearBtn.addEventListener("click", () => this.clearImportFile());

    // Submit
    submitBtn.addEventListener("click", () => this.uploadImport());
  }

  setImportFile(file) {
    this.importFile = file;

    const dropzone = document.getElementById("import-dropzone");
    const fileInfo = document.getElementById("import-file-info");
    const fileName = document.getElementById("import-file-name");
    const options = document.getElementById("import-options");
    const submitBtn = document.getElementById("import-submit-btn");

    dropzone.classList.add("has-file");
    fileInfo.style.display = "";
    fileName.textContent = `${file.name} (${this.formatFileSize(file.size)})`;
    options.style.display = "";
    submitBtn.style.display = "";
  }

  clearImportFile() {
    this.importFile = null;

    const dropzone = document.getElementById("import-dropzone");
    const fileInput = document.getElementById("import-file-input");
    const fileInfo = document.getElementById("import-file-info");
    const options = document.getElementById("import-options");
    const submitBtn = document.getElementById("import-submit-btn");
    const progress = document.getElementById("import-progress");

    dropzone.classList.remove("has-file");
    fileInfo.style.display = "none";
    options.style.display = "none";
    submitBtn.style.display = "none";
    progress.style.display = "none";
    fileInput.value = "";

    document.getElementById("import-title").value = "";
    document.getElementById("import-channel").value = "";
  }

  uploadImport() {
    if (!this.importFile || this.importUploading) return;

    this.importUploading = true;

    const submitBtn = document.getElementById("import-submit-btn");
    const progress = document.getElementById("import-progress");
    const progressBar = document.getElementById("import-progress-bar");
    const statusText = document.getElementById("import-status-text");

    submitBtn.disabled = true;
    submitBtn.loading = true;
    progress.style.display = "";
    progressBar.value = 0;
    statusText.textContent = "Uploading...";

    const title = document.getElementById("import-title").value.trim();
    const channel = document.getElementById("import-channel").value.trim();

    const xhr = new XMLHttpRequest();
    xhr.open("POST", "/api/import");
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    if (title) xhr.setRequestHeader("X-Import-Title", title);
    if (channel) xhr.setRequestHeader("X-Import-Channel", channel);

    xhr.upload.addEventListener("progress", (e) => {
      if (e.lengthComputable) {
        const pct = Math.round((e.loaded / e.total) * 100);
        progressBar.value = pct;
        statusText.textContent = `Uploading... ${pct}% (${this.formatFileSize(e.loaded)} / ${this.formatFileSize(e.total)})`;
      }
    });

    xhr.addEventListener("load", () => {
      this.importUploading = false;
      submitBtn.disabled = false;
      submitBtn.loading = false;

      if (xhr.status === 201) {
        statusText.textContent = "Import complete!";
        progressBar.value = 100;
        this.showToast("Archive imported successfully", "success");

        // Reset form after delay
        setTimeout(() => this.clearImportFile(), 1500);

        // Refresh player job list if initialized
        if (this.playerInitialized) {
          this.loadPlayerJobList();
        }
      } else {
        let errorMsg = "Import failed";
        try {
          const data = JSON.parse(xhr.responseText);
          errorMsg = data.error || errorMsg;
        } catch {}
        statusText.textContent = errorMsg;
        this.showToast(errorMsg, "danger");
      }
    });

    xhr.addEventListener("error", () => {
      this.importUploading = false;
      submitBtn.disabled = false;
      submitBtn.loading = false;
      statusText.textContent = "Upload failed (network error)";
      this.showToast("Upload failed: network error", "danger");
    });

    xhr.send(this.importFile);
  }

  formatFileSize(bytes) {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    if (bytes < 1024 * 1024 * 1024)
      return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
    return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
  }

  // ===== Player Methods =====

  initPlayer() {
    this.playerInitialized = true;

    const jobSelect = document.getElementById("player-job-select");
    const nicoToggle = document.getElementById("player-nico-toggle");
    const sidebarToggle = document.getElementById("player-sidebar-toggle");
    const video = document.getElementById("player-video");
    const sidebarMessages = document.getElementById("player-sidebar-messages");
    const syncBtn = document.getElementById("player-sync-btn");

    // Job selection
    jobSelect.addEventListener("sl-change", () => {
      const val = jobSelect.value;
      if (val) {
        this.onPlayerJobSelect(val);
      } else {
        this.clearPlayer();
      }
    });

    // Restore saved toggle state
    const savedNico = localStorage.getItem("player-nico-toggle");
    const savedSidebar = localStorage.getItem("player-sidebar-toggle");
    if (savedNico !== null) {
      nicoToggle.checked = savedNico === "true";
      this.nicoEnabled = nicoToggle.checked;
      document.getElementById("player-nico-overlay").style.display = this.nicoEnabled ? "" : "none";
    }
    if (savedSidebar !== null) {
      sidebarToggle.checked = savedSidebar === "true";
      document.getElementById("player-sidebar").style.display = sidebarToggle.checked ? "" : "none";
    }

    // Nico toggle
    nicoToggle.addEventListener("sl-change", () => {
      this.nicoEnabled = nicoToggle.checked;
      localStorage.setItem("player-nico-toggle", nicoToggle.checked);
      const overlay = document.getElementById("player-nico-overlay");
      overlay.style.display = this.nicoEnabled ? "" : "none";
      if (!this.nicoEnabled) {
        this.clearNicoOverlay();
      }
    });

    // Sidebar toggle
    sidebarToggle.addEventListener("sl-change", () => {
      localStorage.setItem("player-sidebar-toggle", sidebarToggle.checked);
      const sidebar = document.getElementById("player-sidebar");
      sidebar.style.display = sidebarToggle.checked ? "" : "none";
      if (sidebarToggle.checked && this.playerChatMessages.length > 0) {
        const currentMs = video.currentTime * 1000;
        this.resetSidebarToTime(currentMs);
        this.syncSidebarToTime();
      }
    });

    // Video timeupdate
    video.addEventListener("timeupdate", () => this.onPlayerTimeUpdate());

    // Video seeked — reset both systems
    video.addEventListener("seeked", () => {
      const currentMs = video.currentTime * 1000;
      this.resetSidebarToTime(currentMs);
      this.clearNicoOverlay();
      this.nicoLastSpawnMs = currentMs;
    });

    // Pause/play nico animations
    video.addEventListener("pause", () => {
      document.querySelectorAll(".nico-message").forEach((el) => {
        if (el._nicoAnim) el._nicoAnim.pause();
      });
    });

    video.addEventListener("play", () => {
      document.querySelectorAll(".nico-message").forEach((el) => {
        if (el._nicoAnim) el._nicoAnim.play();
      });
    });

    // Sidebar scroll locking
    sidebarMessages.addEventListener("mouseenter", () => {
      this.playerScrollLock = true;
    });

    sidebarMessages.addEventListener("mouseleave", () => {
      if (this.playerAutoScroll) {
        this.playerScrollLock = false;
      }
    });

    sidebarMessages.addEventListener("scroll", () => {
      if (!this._programmaticScroll) {
        this.playerAutoScroll = false;
      }
    });

    // Sync button
    syncBtn.addEventListener("click", () => {
      this.playerAutoScroll = true;
      this.playerScrollLock = false;
      this.syncSidebarToTime();
    });
  }

  clearPlayer() {
    this.playerJob = null;
    this.playerChatData = null;
    this.playerChatMessages = [];
    this.playerActiveChatIndex = 0;
    this.nicoLastSpawnMs = -1;

    const video = document.getElementById("player-video");
    video.removeAttribute("src");
    video.load();

    document.getElementById("player-viewport").style.display = "none";
    document.getElementById("player-empty-state").style.display = "";
    document.getElementById("player-sidebar-messages").innerHTML = "";
    this.clearNicoOverlay();
  }

  async loadPlayerJobList() {
    const select = document.getElementById("player-job-select");
    const currentValue = select.value;

    try {
      const [jobsRes, archivedRes] = await Promise.all([
        fetch("/api/jobs"),
        fetch("/api/jobs/archived"),
      ]);
      const jobs = jobsRes.ok ? await jobsRes.json() : [];
      const archived = archivedRes.ok ? await archivedRes.json() : [];

      const all = [...jobs, ...archived]
        .filter((j) => j.status === "Finished" && j.filename)
        .sort((a, b) => new Date(b.updatedAt) - new Date(a.updatedAt));

      // Remove existing options
      select.querySelectorAll("sl-option").forEach((o) => o.remove());

      all.forEach((job) => {
        const opt = document.createElement("sl-option");
        opt.value = job.id;
        const noChat = !job.chatFilename ? " (no chat)" : "";
        opt.textContent = `${job.title} — ${job.channelName}${noChat}`;
        select.appendChild(opt);
      });

      // Restore selection if still valid
      if (currentValue && all.some((j) => j.id === currentValue)) {
        select.value = currentValue;
      }

      // Show/hide empty state
      const emptyState = document.getElementById("player-empty-state");
      if (all.length === 0) {
        emptyState.style.display = "";
        emptyState.querySelector("p").textContent = "No finished videos available.";
      } else if (!select.value) {
        emptyState.style.display = "";
        emptyState.querySelector("p").textContent = "Select a finished video to play.";
      }
    } catch (e) {
      console.error("Failed to load player job list:", e);
    }
  }

  async onPlayerJobSelect(jobId) {
    const video = document.getElementById("player-video");
    const nicoToggle = document.getElementById("player-nico-toggle");
    const sidebarToggle = document.getElementById("player-sidebar-toggle");

    // Fetch job details
    try {
      const res = await fetch(`/api/jobs/${jobId}`);
      if (!res.ok) return;
      this.playerJob = await res.json();
    } catch (e) {
      console.error("Failed to fetch job:", e);
      return;
    }

    // Show viewport, hide empty state
    document.getElementById("player-viewport").style.display = "";
    document.getElementById("player-empty-state").style.display = "none";

    // Set video source
    video.src = `/api/jobs/${jobId}/video`;

    // Load chat if available
    this.playerChatMessages = [];
    this.playerChatData = null;
    this.playerActiveChatIndex = 0;
    this.nicoLastSpawnMs = -1;

    if (this.playerJob.chatFilename) {
      try {
        const chatRes = await fetch(`/api/jobs/${jobId}/chat`);
        if (chatRes.ok) {
          this.playerChatData = await chatRes.json();
          this.playerChatMessages = (this.playerChatData.messages || [])
            .map((m) => ({
              ...m,
              offsetMs: m.offsetMs || 0,
            }))
            .sort((a, b) => a.offsetMs - b.offsetMs);
        }
      } catch (e) {
        console.error("Failed to load chat:", e);
      }

      // Enable chat toggles
      nicoToggle.disabled = false;
      sidebarToggle.disabled = false;
    } else {
      // No chat — disable toggles
      nicoToggle.disabled = true;
      sidebarToggle.disabled = true;
      sidebarToggle.checked = false;
      document.getElementById("player-sidebar").style.display = "none";
    }

    // Build sidebar chat
    this.buildSidebarChat();
    this.clearNicoOverlay();

    // Update sidebar header
    document.getElementById("player-sidebar-header").textContent =
      `${this.playerChatMessages.length} messages`;
  }

  buildSidebarChat() {
    const container = document.getElementById("player-sidebar-messages");
    container.innerHTML = "";

    if (this.playerChatMessages.length === 0) return;

    const frag = document.createDocumentFragment();

    this.playerChatMessages.forEach((msg) => {
      const div = document.createElement("div");
      div.className = "chat-msg future";
      div.dataset.offset = msg.offsetMs;

      if (msg.superchat) {
        div.classList.add("superchat");
      }

      // Timestamp
      const timeSpan = document.createElement("span");
      timeSpan.className = "chat-msg-time";
      timeSpan.textContent = this.formatMsToTime(msg.offsetMs);
      div.appendChild(timeSpan);

      // Superchat amount
      if (msg.superchat) {
        const scSpan = document.createElement("span");
        scSpan.className = "chat-msg-superchat";
        scSpan.textContent = msg.superchat.amount;
        div.appendChild(scSpan);
      }

      // Author
      const authorSpan = document.createElement("span");
      authorSpan.className = "chat-msg-author";
      if (msg.authorBadges) {
        if (msg.authorBadges.includes("owner")) authorSpan.classList.add("owner");
        else if (msg.authorBadges.includes("moderator")) authorSpan.classList.add("moderator");
        else if (msg.authorBadges.includes("member")) authorSpan.classList.add("member");
      }
      authorSpan.textContent = msg.authorName + ": ";
      div.appendChild(authorSpan);

      // Message content
      const contentSpan = document.createElement("span");
      contentSpan.innerHTML = this.renderChatMessageParts(msg.message || []);
      div.appendChild(contentSpan);

      frag.appendChild(div);
    });

    container.appendChild(frag);
    this.playerActiveChatIndex = 0;
  }

  onPlayerTimeUpdate() {
    const video = document.getElementById("player-video");
    if (!video || !this.playerChatMessages.length) return;

    const currentMs = video.currentTime * 1000;

    // Update sidebar active state
    this.updateSidebarActiveState(currentMs);

    // Spawn nico messages
    if (this.nicoEnabled) {
      this.spawnNicoMessages(currentMs);
    }

    // Auto-scroll sidebar
    if (this.playerAutoScroll && !this.playerScrollLock) {
      this.syncSidebarToTime();
    }
  }

  updateSidebarActiveState(currentMs) {
    const container = document.getElementById("player-sidebar-messages");
    const children = container.children;

    // Walk forward from current index
    while (
      this.playerActiveChatIndex < this.playerChatMessages.length &&
      this.playerChatMessages[this.playerActiveChatIndex].offsetMs <= currentMs
    ) {
      const child = children[this.playerActiveChatIndex];
      if (child) {
        child.classList.remove("future");
        child.classList.add("active");
      }
      this.playerActiveChatIndex++;
    }
  }

  syncSidebarToTime() {
    const container = document.getElementById("player-sidebar-messages");
    if (!container || this.playerActiveChatIndex === 0) return;

    const targetChild = container.children[this.playerActiveChatIndex - 1];
    if (!targetChild) return;

    // Scroll so last active message is at ~70% from top
    const containerHeight = container.clientHeight;
    const targetOffset = targetChild.offsetTop - containerHeight * 0.7;

    this._programmaticScroll = true;
    container.scrollTop = Math.max(0, targetOffset);
    requestAnimationFrame(() => {
      this._programmaticScroll = false;
    });
  }

  resetSidebarToTime(currentMs) {
    const container = document.getElementById("player-sidebar-messages");
    const children = container.children;

    this.playerActiveChatIndex = 0;

    for (let i = 0; i < this.playerChatMessages.length; i++) {
      const child = children[i];
      if (!child) continue;

      if (this.playerChatMessages[i].offsetMs <= currentMs) {
        child.classList.remove("future");
        child.classList.add("active");
        this.playerActiveChatIndex = i + 1;
      } else {
        child.classList.remove("active");
        child.classList.add("future");
      }
    }
  }

  // Niconico overlay engine

  clearNicoOverlay() {
    const overlay = document.getElementById("player-nico-overlay");
    if (overlay) overlay.innerHTML = "";
    this.nicoLaneAvail = new Array(this.nicoLaneCount).fill(0);
  }

  spawnNicoMessages(currentMs) {
    // On first call, set cursor to -5001 so messages within 5s before stream
    // start (offsetMs >= -5000) deploy instantly. Older pre-stream messages
    // are permanently skipped from the overlay (still visible in sidebar).
    const firstSpawn = this.nicoLastSpawnMs < 0;
    if (firstSpawn) {
      this.nicoLastSpawnMs = -5001;
    }

    const messages = this.playerChatMessages;
    if (!messages.length) return;

    // Binary search for start of window (nicoLastSpawnMs, currentMs]
    let lo = 0;
    let hi = messages.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (messages[mid].offsetMs <= this.nicoLastSpawnMs) {
        lo = mid + 1;
      } else {
        hi = mid;
      }
    }

    const overlay = document.getElementById("player-nico-overlay");
    if (!overlay) return;

    const overlayWidth = overlay.clientWidth;
    const overlayHeight = overlay.clientHeight;
    if (!overlayWidth || !overlayHeight) return;

    const laneHeight = overlayHeight / this.nicoLaneCount;
    const now = performance.now();
    const duration = 8000;

    let spawned = 0;
    // No limit on first spawn so all offsetMs=0 pre-stream messages deploy at once
    const maxPerFrame = firstSpawn ? Infinity : 10;

    for (let i = lo; i < messages.length && messages[i].offsetMs <= currentMs; i++) {
      if (spawned >= maxPerFrame) break;

      const msg = messages[i];
      const parts = msg.message || [];
      const html = this.renderChatMessageParts(parts);
      if (!html) continue;

      // Create element off-screen to measure its actual height
      const el = document.createElement("div");
      el.className = "nico-message";
      el.innerHTML = html;
      el.style.top = "0";
      el.style.left = `${overlayWidth}px`;
      overlay.appendChild(el);

      // Measure actual dimensions (emotes can make height > laneHeight)
      const msgWidth = el.offsetWidth;
      const msgHeight = el.offsetHeight;
      const lanesNeeded = Math.max(1, Math.ceil(msgHeight / laneHeight));

      // Find a run of consecutive available lanes
      let lane = -1;
      for (let l = 0; l <= this.nicoLaneCount - lanesNeeded; l++) {
        let allFree = true;
        for (let k = 0; k < lanesNeeded; k++) {
          if (this.nicoLaneAvail[l + k] > now) {
            allFree = false;
            break;
          }
        }
        if (allFree) {
          lane = l;
          break;
        }
      }
      if (lane === -1) {
        el.remove();
        continue; // All lanes busy
      }

      // Position at the chosen lane
      el.style.top = `${lane * laneHeight}px`;

      const totalTravel = overlayWidth + msgWidth;

      // Animate using Web Animations API
      const anim = el.animate(
        [
          { transform: "translateX(0)" },
          { transform: `translateX(-${totalTravel}px)` },
        ],
        { duration, fill: "forwards" },
      );
      el._nicoAnim = anim;

      // If video is paused, pause animation immediately
      const video = document.getElementById("player-video");
      if (video && video.paused) {
        anim.pause();
      }

      anim.onfinish = () => el.remove();

      // Calculate when these lanes become available (when the message clears the right edge)
      const clearTime = (msgWidth / totalTravel) * duration;
      const availAt = now + clearTime + 200; // 200ms buffer
      for (let k = 0; k < lanesNeeded; k++) {
        this.nicoLaneAvail[lane + k] = availAt;
      }

      spawned++;
    }

    this.nicoLastSpawnMs = currentMs;
  }

  renderChatMessageParts(parts) {
    return parts
      .map((part) => {
        if (part.type === "emoji" && part.emojiUrl) {
          const alt = part.text || part.emojiId || "";
          const url = part.emojiUrl.replace(/=[^/]*$/, "");
          return `<img class="chat-emoji" src="${this.escapeHtml(url)}" alt="${this.escapeHtml(alt)}" loading="lazy" referrerpolicy="no-referrer">`;
        }
        return this.escapeHtml(part.text || "");
      })
      .join("");
  }

  formatMsToTime(ms) {
    const negative = ms < 0;
    const absTotalSec = Math.floor(Math.abs(ms) / 1000);
    const h = Math.floor(absTotalSec / 3600);
    const m = Math.floor((absTotalSec % 3600) / 60);
    const s = absTotalSec % 60;

    const prefix = negative ? "-" : "";
    if (h > 0) {
      return `${prefix}${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
    }
    return `${prefix}${m}:${String(s).padStart(2, "0")}`;
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
      innerHTML: `<sl-icon slot="icon" name="${this.getIconForVariant(variant)}"></sl-icon>${this.escapeHtml(message)}`,
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
}

// Initialize app when DOM is ready
document.addEventListener("DOMContentLoaded", () => {
  window.app = new MoomboxApp();
});
