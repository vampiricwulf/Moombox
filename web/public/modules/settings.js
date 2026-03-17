/**
 * Settings Controller — Config UI, channels, notifications, cookies, yt-dlp plugin
 */
import { formatRelativeTime } from "./utils.js";

const NOTIFICATION_EVENT_GROUPS = [
  {
    name: "Job Lifecycle",
    events: [
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
    ],
  },
  {
    name: "Trim",
    events: [
      { id: "trim_created", label: "Trim Created" },
      { id: "trim_deleted", label: "Trim Deleted" },
      { id: "trim_error", label: "Trim Error" },
    ],
  },
  {
    name: "System",
    events: [
      { id: "disk_warning", label: "Disk Warning" },
      { id: "update_available", label: "Update Available" },
    ],
  },
];
const ALL_NOTIFICATION_EVENTS = NOTIFICATION_EVENT_GROUPS.flatMap((g) => g.events);
const ALL_EVENT_IDS = ALL_NOTIFICATION_EVENTS.map((e) => e.id);

// Settings that require a process restart to take effect
// Each entry is [nestedPath, formElementId] for change detection
const RESTART_REQUIRED_FIELDS = [
  { path: "network.port", id: "cfg-port" },
  { path: "network.network_access", id: "cfg-network-access" },
  { path: "network.https_enabled", id: "cfg-https-enabled" },
  { path: "network.tls_cert_path", id: "cfg-tls-cert-path" },
  { path: "network.tls_key_path", id: "cfg-tls-key-path" },
  { path: "paths.database_path", id: "cfg-database" },
  { path: "paths.log_file_path", id: "cfg-log-file" },
  { path: "logs.log_max_file_size", id: "cfg-log-max-size" },
  { path: "logs.log_max_files", id: "cfg-log-max-files" },
];

export class SettingsController {
  constructor(app) {
    this.app = app;
    this.editingChannelId = null;
    this._netAccessListenerAdded = false;
    /** Snapshot of restart-required values taken when config is loaded */
    this._originalRestartValues = {};
    this._dirty = false;
    this._dirtyListenersAdded = false;
  }

  setupListeners() {
    // Security: set password button
    const setPasswordBtn = document.getElementById("security-set-password-btn");
    if (setPasswordBtn) {
      setPasswordBtn.addEventListener("click", () => this.setPassword());
    }

    // Security: remove password button
    const removePasswordBtn = document.getElementById("security-remove-password-btn");
    if (removePasswordBtn) {
      removePasswordBtn.addEventListener("click", () => this.removePassword());
    }

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

    // Reload config — clear dirty flag so loadConfig repopulates the form
    const reloadBtn = document.getElementById("reload-config-btn");
    if (reloadBtn) {
      reloadBtn.addEventListener("click", () => {
        this._dirty = false;
        this._updateUnsavedIndicator();
        this.app.loadConfig();
      });
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

    // Channel platform selector
    const channelPlatformSelect = document.getElementById("channel-platform-select");
    if (channelPlatformSelect) {
      channelPlatformSelect.addEventListener("sl-change", (e) => {
        this.updateChannelDialogForPlatform(e.target.value);
      });
    }

    // Auto-detect platform from channel ID input
    const channelIdInput = document.getElementById("channel-id-input");
    if (channelIdInput) {
      channelIdInput.addEventListener("sl-input", () => {
        if (this.editingChannelId) return; // Don't auto-switch when editing
        const val = (channelIdInput.value || "").trim();
        const platformSelect = document.getElementById("channel-platform-select");
        if (!platformSelect || platformSelect.disabled) return;
        if (val.includes("youtube.com") || val.includes("youtu.be")) {
          if (platformSelect.value !== "youtube") {
            platformSelect.value = "youtube";
            this.updateChannelDialogForPlatform("youtube");
          }
        } else if (val.includes("twitch.tv")) {
          if (platformSelect.value !== "twitch") {
            platformSelect.value = "twitch";
            this.updateChannelDialogForPlatform("twitch");
          }
        }
      });
    }

    // Add notification
    const addNotifBtn = document.getElementById("add-notification-btn");
    if (addNotifBtn) {
      addNotifBtn.addEventListener("click", () => this.addNotification());
    }

    // Webhook cancel button
    const webhookCancelBtn = document.getElementById("webhook-cancel-btn");
    if (webhookCancelBtn) {
      webhookCancelBtn.addEventListener("click", () => {
        document.getElementById("webhook-dialog")?.hide();
      });
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

    // Check Now button
    document.getElementById("btn-check-update-now")?.addEventListener("click", async () => {
      const btn = document.getElementById("btn-check-update-now");
      const result = document.getElementById("update-check-result");
      btn.loading = true;
      result.textContent = "";
      try {
        const resp = await fetch("/api/update/check", { method: "POST" });
        if (!resp.ok) throw new Error(resp.statusText);
        const data = await resp.json();
        if (data.available) {
          result.textContent = `v${data.version} available!`;
          result.style.color = "var(--sl-color-success-600)";
          this.app._updateAvailable = data;
          this.app.updateVersionIndicator();
        } else {
          result.textContent = "Up to date";
          result.style.color = "var(--sl-color-neutral-500)";
        }
      } catch {
        result.textContent = "Check failed";
        result.style.color = "var(--sl-color-danger-500)";
      }
      btn.loading = false;
    });

    // Verify Signature button
    document
      .getElementById("btn-verify-signature")
      ?.addEventListener("click", async () => {
        const btn = document.getElementById("btn-verify-signature");
        const result = document.getElementById("verify-signature-result");
        btn.loading = true;
        result.textContent = "";
        try {
          const resp = await fetch("/api/update/verify", { method: "POST" });
          if (!resp.ok) throw new Error(resp.statusText);
          const data = await resp.json();
          if (data.verified) {
            result.textContent = "Signature valid";
            result.style.color = "var(--sl-color-success-600)";
          } else {
            result.textContent = data.error || "Verification failed";
            result.style.color = "var(--sl-color-danger-500)";
          }
        } catch {
          result.textContent = "Verification failed";
          result.style.color = "var(--sl-color-danger-500)";
        }
        btn.loading = false;
      });

    // Auto-cookie toggle → show/hide action buttons
    const autoCookiesSwitch = document.getElementById("cfg-auto-cookies-enabled");
    if (autoCookiesSwitch) {
      autoCookiesSwitch.addEventListener("sl-change", () => this.updateAutoCookieUI());
    }

    // Per-platform setup buttons
    const setupYtBtn = document.getElementById("btn-auto-cookie-setup-yt");
    if (setupYtBtn) {
      setupYtBtn.addEventListener("click", () => this.startAutoCookieSetup("youtube"));
    }
    const setupTwBtn = document.getElementById("btn-auto-cookie-setup-tw");
    if (setupTwBtn) {
      setupTwBtn.addEventListener("click", () => this.startAutoCookieSetup("twitch"));
    }

    // Auto-cookie dialog buttons
    const autoCookieDoneBtn = document.getElementById("btn-auto-cookie-done");
    if (autoCookieDoneBtn) {
      autoCookieDoneBtn.addEventListener("click", () => this.finishAutoCookieSetup());
    }
    const autoCookieCancelBtn = document.getElementById("btn-auto-cookie-cancel");
    if (autoCookieCancelBtn) {
      autoCookieCancelBtn.addEventListener("click", () => this.cancelAutoCookieSetup());
    }

    // Active platform toggles
    const activeYtSwitch = document.getElementById("cfg-active-youtube");
    if (activeYtSwitch) {
      activeYtSwitch.addEventListener("sl-change", () => this.updateAutoCookieUI());
    }
    const activeTwSwitch = document.getElementById("cfg-active-twitch");
    if (activeTwSwitch) {
      activeTwSwitch.addEventListener("sl-change", () => this.updateAutoCookieUI());
    }

    // Unsaved changes warning
    window.addEventListener("beforeunload", (e) => {
      if (this._dirty) {
        e.preventDefault();
        e.returnValue = "";
      }
    });

    // Handle sl-dialog close (Escape/overlay click)
    const autoCookieDialog = document.getElementById("auto-cookie-setup-dialog");
    if (autoCookieDialog) {
      autoCookieDialog.addEventListener("sl-request-close", (e) => {
        e.preventDefault();
        this.cancelAutoCookieSetup();
      });
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

    // Load security status when network section is shown (password UI is in network)
    if (section === "network") {
      this.loadSecurityStatus();
    }
  }

  populateConfigForm() {
    const config = this.app.config;
    if (!config) return;

    // Network settings
    this.app.setInputValue("cfg-port", config.network?.port);
    const netAccessSelect = document.getElementById("cfg-network-access");
    const cfgExtWarning = document.getElementById("cfg-external-warning");
    if (netAccessSelect) {
      const level = config.network?.network_access || "localhost";
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
    // HTTPS switch
    const httpsSwitch = document.getElementById("cfg-https-enabled");
    if (httpsSwitch) {
      httpsSwitch.checked = config.network?.https_enabled === true;
    }
    this.app.setInputValue("cfg-tls-cert-path", config.network?.tls_cert_path);
    this.app.setInputValue("cfg-tls-key-path", config.network?.tls_key_path);

    // Paths settings
    this.app.setInputValue("cfg-database", config.paths?.database_path);
    this.app.setInputValue("cfg-log-file", config.paths?.log_file_path);
    this.app.setInputValue("cfg-output-dir", config.paths?.output_directory);
    this.app.setInputValue("cfg-staging-dir", config.paths?.staging_directory);
    this.app.setInputValue("cfg-ffmpeg-path", config.paths?.ffmpeg_path);

    // Logs settings
    const logLevelSelect = document.getElementById("cfg-log-level");
    if (logLevelSelect && config.logs?.log_level) {
      logLevelSelect.value = config.logs.log_level;
    }
    this.app.setInputValue("cfg-log-max-size", config.logs?.log_max_file_size);
    this.app.setInputValue("cfg-log-max-files", config.logs?.log_max_files);

    // Monitors settings
    this.app.setInputValue("cfg-max-feed-items", config.monitors?.max_feed_items);
    this.app.setInputValue("cfg-feed-check-interval", config.monitors?.feed_check_interval);
    this.app.setInputValue("cfg-decapi-check-interval", config.monitors?.decapi_check_interval ?? "");
    this.app.setInputValue("cfg-twitch-check-interval", config.monitors?.twitch_check_interval ?? "");
    this.app.setInputValue("cfg-hide-finished-days", config.monitors?.hide_finished_age_days);

    // Downloader settings
    this.app.setInputValue("cfg-output-template", config.downloader?.output_template);
    this.app.setInputValue("cfg-max-resolution", config.downloader?.max_video_resolution);
    this.app.setInputValue("cfg-parallel-downloads", config.downloader?.num_parallel_downloads);
    // Download chat switch
    const downloadChatSwitch = document.getElementById("cfg-download-chat");
    if (downloadChatSwitch) {
      downloadChatSwitch.checked = config.downloader?.download_chat !== false;
    }
    // Prefer 60fps switch
    const prefer60fpsSwitch = document.getElementById("cfg-prefer-60fps");
    if (prefer60fpsSwitch) {
      prefer60fpsSwitch.checked = config.downloader?.prefer_60fps !== false;
    }
    this.app.setInputValue("cfg-retry-delay-cap", config.downloader?.segment_retry_delay_cap);
    this.app.setInputValue("cfg-live-check-retries", config.downloader?.segment_live_check_retries);

    // Cookies settings
    this.app.setInputValue("cfg-cookie-file", config.cookies?.cookie_file);
    this.app.setInputValue("cfg-cookie-refresh-interval", config.cookies?.refresh_interval);

    // Active platform toggles — use config value if present (even empty array),
    // fall back to live status only when config field is missing (pre-migration).
    const activePlats = config.cookies?.active_platforms;
    const activeYtSwitch = document.getElementById("cfg-active-youtube");
    const activeTwSwitch = document.getElementById("cfg-active-twitch");
    if (activeYtSwitch) {
      if (Array.isArray(activePlats)) {
        activeYtSwitch.checked = activePlats.includes("youtube");
      } else {
        activeYtSwitch.checked = this.app.activePlatforms?.youtube === true;
      }
    }
    if (activeTwSwitch) {
      if (Array.isArray(activePlats)) {
        activeTwSwitch.checked = activePlats.includes("twitch");
      } else {
        activeTwSwitch.checked = this.app.activePlatforms?.twitch === true;
      }
    }

    const autoCookiesSwitch = document.getElementById("cfg-auto-cookies-enabled");
    if (autoCookiesSwitch) {
      autoCookiesSwitch.checked = config.cookies?.auto_enabled === true;
    }
    this.app.setInputValue("cfg-auto-cookies-profile-dir", config.cookies?.browser_profile_dir);
    this.updateAutoCookieUI();

    // Disk settings
    this.app.setInputValue("cfg-disk-warn-percent", config.disk?.disk_warn_percent);
    this.app.setInputValue("cfg-disk-critical-percent", config.disk?.disk_critical_percent);

    // Updates settings
    const autoCheckSwitch = document.getElementById("cfg-auto-check-updates");
    if (autoCheckSwitch) {
      autoCheckSwitch.checked = config.updates?.auto_check_updates !== false;
    }

    // Track dirty state
    this._dirty = false;
    this._updateUnsavedIndicator();
    if (!this._dirtyListenersAdded) {
      this._dirtyListenersAdded = true;
      const settingsContent = document.querySelector(".settings-content");
      if (settingsContent) {
        settingsContent.addEventListener("sl-change", () => this._markDirty());
        settingsContent.addEventListener("sl-input", () => this._markDirty());
      }
    }

    // Add restart-required badges to relevant fields
    this._addRestartBadges();

    // Snapshot restart-required values for change detection
    this._originalRestartValues = {
      "network.port": config.network?.port,
      "network.network_access": config.network?.network_access,
      "network.https_enabled": config.network?.https_enabled,
      "network.tls_cert_path": config.network?.tls_cert_path,
      "network.tls_key_path": config.network?.tls_key_path,
      "paths.database_path": config.paths?.database_path,
      "paths.log_file_path": config.paths?.log_file_path,
      "logs.log_max_file_size": config.logs?.log_max_file_size,
      "logs.log_max_files": config.logs?.log_max_files,
    };

    // Network is the default visible section, so load security status now
    this.loadSecurityStatus();
  }

  async saveConfig() {
    const saveBtn = document.getElementById("save-config-btn");
    if (saveBtn) { saveBtn.loading = true; saveBtn.disabled = true; }

    if (!this.app.config) {
      this.app.config = {};
    }
    const config = this.app.config;

    // Gather values from form
    const port = this.app.getInputNumber("cfg-port");
    const netAccessSelect = document.getElementById("cfg-network-access");
    const networkAccess = netAccessSelect ? netAccessSelect.value : "localhost";
    const httpsSwitch = document.getElementById("cfg-https-enabled");
    const httpsEnabled = httpsSwitch ? httpsSwitch.checked : false;
    const tlsCertPath = this.app.getInputValue("cfg-tls-cert-path");
    const tlsKeyPath = this.app.getInputValue("cfg-tls-key-path");

    const database = this.app.getInputValue("cfg-database");
    const logFile = this.app.getInputValue("cfg-log-file");
    const outputDir = this.app.getInputValue("cfg-output-dir");
    const stagingDir = this.app.getInputValue("cfg-staging-dir");
    const ffmpegPath = this.app.getInputValue("cfg-ffmpeg-path");

    const logLevelSelect = document.getElementById("cfg-log-level");
    const logLevel = logLevelSelect ? logLevelSelect.value : "";
    const logMaxSize = this.app.getInputNumber("cfg-log-max-size");
    const logMaxFiles = this.app.getInputNumber("cfg-log-max-files");

    const maxFeedItems = this.app.getInputNumber("cfg-max-feed-items");
    const feedCheckInterval = this.app.getInputNumber("cfg-feed-check-interval");
    const decapiCheckInterval = this.app.getInputNumber("cfg-decapi-check-interval");
    const twitchCheckInterval = this.app.getInputNumber("cfg-twitch-check-interval");
    const hideFinishedDays = this.app.getInputNumber("cfg-hide-finished-days");

    const outputTemplate = this.app.getInputValue("cfg-output-template");
    const maxResolution = this.app.getInputNumber("cfg-max-resolution");
    const parallelDownloads = this.app.getInputNumber("cfg-parallel-downloads");
    const retryDelayCap = this.app.getInputNumber("cfg-retry-delay-cap");
    const liveCheckRetries = this.app.getInputNumber("cfg-live-check-retries");
    const downloadChatSwitch = document.getElementById("cfg-download-chat");
    const downloadChat = downloadChatSwitch ? downloadChatSwitch.checked : true;
    const prefer60fpsSwitch = document.getElementById("cfg-prefer-60fps");
    const prefer60fps = prefer60fpsSwitch ? prefer60fpsSwitch.checked : true;

    const cookieFile = this.app.getInputValue("cfg-cookie-file");
    const activeYtSwitch = document.getElementById("cfg-active-youtube");
    const activeTwSwitch = document.getElementById("cfg-active-twitch");
    const activePlatforms = [];
    if (activeYtSwitch?.checked) activePlatforms.push("youtube");
    if (activeTwSwitch?.checked) activePlatforms.push("twitch");
    const autoCookiesSwitch = document.getElementById("cfg-auto-cookies-enabled");
    const autoEnabled = autoCookiesSwitch ? autoCookiesSwitch.checked : false;
    const autoCookiesProfileDir = this.app.getInputValue("cfg-auto-cookies-profile-dir");
    const cookieRefreshInterval = this.app.getInputNumber("cfg-cookie-refresh-interval");

    const diskWarnPercent = this.app.getInputNumber("cfg-disk-warn-percent");
    const diskCriticalPercent = this.app.getInputNumber("cfg-disk-critical-percent");

    const autoCheckUpdatesSwitch = document.getElementById("cfg-auto-check-updates");
    const autoCheckUpdates = autoCheckUpdatesSwitch ? autoCheckUpdatesSwitch.checked : true;

    // Build payload with only form-managed sections (don't include channels
    // to avoid overwriting concurrent changes from TUI).
    const payload = {
      network: {
        port,
        network_access: networkAccess,
        https_enabled: httpsEnabled,
        tls_cert_path: tlsCertPath,
        tls_key_path: tlsKeyPath,
      },
      paths: {
        database_path: database,
        log_file_path: logFile,
        output_directory: outputDir,
        staging_directory: stagingDir,
        ffmpeg_path: ffmpegPath,
      },
      logs: {
        log_level: logLevel,
        log_max_file_size: logMaxSize,
        log_max_files: logMaxFiles,
      },
      monitors: {
        max_feed_items: maxFeedItems,
        feed_check_interval: feedCheckInterval,
        hide_finished_age_days: hideFinishedDays,
        decapi_check_interval: decapiCheckInterval ?? null,
        twitch_check_interval: twitchCheckInterval ?? null,
      },
      downloader: {
        output_template: outputTemplate,
        max_video_resolution: maxResolution,
        num_parallel_downloads: parallelDownloads,
        download_chat: downloadChat,
        prefer_60fps: prefer60fps,
        segment_retry_delay_cap: retryDelayCap,
        segment_live_check_retries: liveCheckRetries,
      },
      cookies: {
        cookie_file: cookieFile,
        active_platforms: activePlatforms,
        auto_enabled: autoEnabled,
        browser_profile_dir: autoCookiesProfileDir,
        refresh_interval: cookieRefreshInterval ?? null,
      },
      disk: {
        disk_warn_percent: diskWarnPercent,
        disk_critical_percent: diskCriticalPercent,
      },
      updates: {
        auto_check_updates: autoCheckUpdates,
      },
    };

    // Include notifications — managed entirely via the web UI, so no
    // concurrent modification risk (unlike channels which the TUI can edit).
    if (config.notifications) {
      payload.notifications = config.notifications;
    }

    try {
      const response = await fetch("/api/config", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });

      if (response.ok) {
        // Update local config cache only after successful save
        Object.assign(config, payload);
        this._dirty = false;
        this._updateUnsavedIndicator();
        this.app.showToast("Settings saved successfully", "success");
        // Refresh status bar (platform indicators, cookie status, etc.)
        this.app.loadStatus();
        // Check if any restart-required settings changed
        this._checkRestartRequired(config);
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        let msg = data.error || "Failed to save settings";
        if (data.details) {
          const fields = Object.entries(data.details).map(([k, v]) => `${k}: ${v}`).join(", ");
          msg += ` (${fields})`;
        }
        this.app.showToast(msg, "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to save settings: " + e.message, "danger");
    } finally {
      if (saveBtn) { saveBtn.loading = false; saveBtn.disabled = false; }
    }
  }

  /**
   * After a successful save, check if restart-requiring settings changed.
   * If so, prompt the user and call POST /api/restart.
   */
  async _checkRestartRequired(config) {
    /** Resolve a dotted path like "network.port" from the config object */
    const resolve = (obj, path) => {
      const parts = path.split(".");
      let v = obj;
      for (const p of parts) {
        if (v == null) return undefined;
        v = v[p];
      }
      return v;
    };

    const current = {};
    for (const { path } of RESTART_REQUIRED_FIELDS) {
      current[path] = resolve(config, path);
    }

    const changed = RESTART_REQUIRED_FIELDS.some(({ path }) => {
      const a = current[path];
      const b = this._originalRestartValues[path];
      // For booleans: treat null/undefined as false to avoid false positives
      // when the server omits a field that defaults to false
      if (typeof a === "boolean" || typeof b === "boolean") {
        return !!a !== !!b;
      }
      return String(a ?? "") !== String(b ?? "");
    });
    if (!changed) return;

    // Capture old network values before updating snapshot (for redirect detection)
    const oldPort = this._originalRestartValues["network.port"] || 774;
    const oldHttps = !!this._originalRestartValues["network.https_enabled"];

    // Update snapshot so we don't prompt again
    this._originalRestartValues = { ...current };

    const shouldRestart = await this.app.showConfirm(
      "Some settings require a restart to take effect (port, network access, database path, log settings).\n\nRestart Moombox now?",
      { okLabel: "Restart", okVariant: "primary", title: "Restart Required" },
    );
    if (!shouldRestart) {
      return;
    }

    // Check if network address changed — browser needs redirect after restart
    const newPort = current["network.port"] || 774;
    const newHttps = !!current["network.https_enabled"];
    const portChanged = String(newPort) !== String(oldPort);
    const httpsChanged = newHttps !== oldHttps;

    this.app.showToast("Restarting Moombox...", "primary");

    fetch("/api/restart", { method: "POST" }).catch(() => {
      // Connection will drop during restart — expected
    });

    if (portChanged || httpsChanged) {
      const proto = newHttps ? "https:" : "http:";
      const host = window.location.hostname;
      const redirectUrl = `${proto}//${host}:${newPort}/`;
      this.app.showToast(`Redirecting to ${redirectUrl} in a few seconds...`, "primary");
      setTimeout(() => { window.location.href = redirectUrl; }, 3500);
    }
  }

  _markDirty() {
    this._dirty = true;
    this._updateUnsavedIndicator();
  }

  _updateUnsavedIndicator() {
    let indicator = document.getElementById("unsaved-indicator");
    const saveBtn = document.getElementById("save-config-btn");
    if (this._dirty) {
      if (!indicator && saveBtn) {
        indicator = document.createElement("span");
        indicator.id = "unsaved-indicator";
        indicator.className = "unsaved-warning";
        indicator.textContent = "Unsaved changes";
        saveBtn.parentElement.insertBefore(indicator, saveBtn);
      }
    } else if (indicator) {
      indicator.remove();
    }
  }

  _addRestartBadges() {
    for (const { id } of RESTART_REQUIRED_FIELDS) {
      const el = document.getElementById(id);
      if (!el) continue;
      // Avoid duplicate badges
      const parent = el.parentElement;
      if (parent && !parent.querySelector(".restart-badge")) {
        const badge = document.createElement("sl-tag");
        badge.size = "small";
        badge.variant = "warning";
        badge.className = "restart-badge";
        badge.textContent = "Restart";
        // Insert after the element's label (next sibling or append to parent)
        el.insertAdjacentElement("afterend", badge);
      }
    }
  }

  renderChannelsList() {
    const container = document.getElementById("channels-list");
    if (!container || !this.app.config) return;

    const channels = this.app.config.channels || [];

    if (channels.length === 0) {
      container.innerHTML =
        '<p class="settings-help">No channels configured yet.</p>';
      return;
    }

    container.innerHTML = channels
      .map(
        (ch, index) => {
          const isTwitch = ch.platform === "twitch";
          const isEnabled = ch.enabled !== false;
          const platformTag = isTwitch
            ? '<sl-tag size="small" variant="primary">Twitch</sl-tag> '
            : '<sl-tag size="small" variant="danger">YouTube</sl-tag> ';
          return `
      <div class="channel-card${isEnabled ? "" : " channel-card--disabled"}" data-channel-id="${this.app.escapeHtml(ch.id)}" data-index="${index}" draggable="true">
        <div class="channel-card-header">
          <div style="display:flex;align-items:center">
            <sl-icon name="grip-vertical" class="channel-drag-handle"></sl-icon>
            <div>
              <div class="channel-card-title">${platformTag}${this.app.escapeHtml(ch.name || ch.id)}</div>
              <div class="channel-card-id">${this.app.escapeHtml(ch.id)}</div>
            </div>
          </div>
          <div class="channel-card-actions">
            <sl-switch size="small" ${isEnabled ? "checked" : ""} title="${isEnabled ? "Monitoring enabled" : "Monitoring disabled"}" data-action="toggle" data-channel-id="${this.app.escapeHtml(ch.id)}"></sl-switch>
            <sl-icon-button name="pencil" label="Edit" data-action="edit" data-channel-id="${this.app.escapeHtml(ch.id)}"></sl-icon-button>
            <sl-icon-button name="trash" label="Delete" data-action="delete" data-channel-id="${this.app.escapeHtml(ch.id)}"></sl-icon-button>
          </div>
        </div>
        ${
          ch.terms
            ? `<div class="channel-card-terms">Filter: <code>${this.app.escapeHtml(typeof ch.terms === "string" ? ch.terms : ch.terms.stream || "")}</code></div>`
            : ""
        }
        ${ch.include_non_live_content ? '<sl-badge variant="neutral">Include VODs</sl-badge>' : ""}
        ${ch.quality_preference ? `<sl-badge variant="neutral">Quality: ${this.app.escapeHtml(ch.quality_preference)}</sl-badge>` : ""}
      </div>
    `;
        },
      )
      .join("");

    // Event delegation for channel actions — attach once
    if (!container._channelDelegated) {
      container._channelDelegated = true;
      container.addEventListener("click", (e) => {
        const btn = e.target.closest("[data-action]");
        if (!btn) return;
        const action = btn.dataset.action;
        const channelId = btn.dataset.channelId;
        if (action === "edit") this.editChannel(channelId);
        else if (action === "delete") this.deleteChannel(channelId);
      });
      container.addEventListener("sl-change", (e) => {
        const toggle = e.target.closest('[data-action="toggle"]');
        if (!toggle) return;
        e.stopPropagation();
        this.toggleChannel(toggle.dataset.channelId, toggle.checked);
      });
    }

    this.setupChannelDragDrop();
  }

  setupChannelDragDrop() {
    const container = document.getElementById("channels-list");
    if (!container) return;

    // Use event delegation to avoid per-card listeners that leak on re-render
    if (container._dragDelegated) return;
    container._dragDelegated = true;

    let draggedIndex = null;

    container.addEventListener("dragstart", (e) => {
      const card = e.target.closest(".channel-card[draggable]");
      if (!card) return;
      draggedIndex = parseInt(card.dataset.index);
      card.classList.add("dragging");
      e.dataTransfer.effectAllowed = "move";
    });

    container.addEventListener("dragover", (e) => {
      const card = e.target.closest(".channel-card[draggable]");
      if (!card) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = "move";
      container.querySelectorAll(".drag-over").forEach((el) => el.classList.remove("drag-over"));
      card.classList.add("drag-over");
    });

    container.addEventListener("dragleave", (e) => {
      const card = e.target.closest(".channel-card[draggable]");
      if (card) card.classList.remove("drag-over");
    });

    container.addEventListener("drop", (e) => {
      const card = e.target.closest(".channel-card[draggable]");
      if (!card) return;
      e.preventDefault();
      const targetIndex = parseInt(card.dataset.index);
      card.classList.remove("drag-over");

      if (draggedIndex !== null && draggedIndex !== targetIndex && this.app.config?.channels) {
        const channels = this.app.config.channels;
        const [moved] = channels.splice(draggedIndex, 1);
        channels.splice(targetIndex, 0, moved);

        this.saveChannelOrder(channels).then(() => {
          this.renderChannelsList();
        }).catch(() => {});
      }
    });

    container.addEventListener("dragend", () => {
      draggedIndex = null;
      container.querySelectorAll(".dragging, .drag-over").forEach((el) => {
        el.classList.remove("dragging", "drag-over");
      });
    });
  }

  async saveChannelOrder(channels) {
    try {
      const response = await fetch("/api/config/channels/reorder", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ids: channels.map((c) => c.id) }),
      });
      if (response.ok) {
        this.app.showToast("Channel order updated", "success");
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.app.showToast(data.error || "Failed to save channel order", "danger");
        this.app.loadConfig();
      }
    } catch (e) {
      this.app.showToast("Failed to save channel order: " + e.message, "danger");
      this.app.loadConfig();
    }
  }

  showAddChannelDialog(channel = null) {
    this.editingChannelId = channel ? channel.id : null;

    document.getElementById("channel-id-input").value = channel?.id || "";
    document.getElementById("channel-name-input").value = channel?.name || "";
    const termsValue = channel?.terms
      ? typeof channel.terms === "string"
        ? channel.terms
        : channel.terms.stream || ""
      : "";
    document.getElementById("channel-terms-input").value = termsValue;
    document.getElementById("channel-include-vods").checked =
      channel?.include_non_live_content || false;

    // Platform selector
    const platformSelect = document.getElementById("channel-platform-select");
    if (platformSelect) {
      platformSelect.value = channel?.platform || "youtube";
      this.updateChannelDialogForPlatform(platformSelect.value);
    }

    // Enabled switch (defaults to true for new channels)
    const enabledSwitch = document.getElementById("channel-enabled-switch");
    if (enabledSwitch) {
      enabledSwitch.checked = channel ? channel.enabled !== false : true;
    }

    // Twitch quality preference
    const qualitySelect = document.getElementById("channel-quality-select");
    if (qualitySelect) {
      qualitySelect.value = channel?.quality_preference || "best";
    }

    const dialog = document.getElementById("add-channel-dialog");
    dialog.label = channel ? "Edit Channel" : "Add Channel";
    document.getElementById("channel-save-btn").textContent = channel
      ? "Save Changes"
      : "Add Channel";

    document.getElementById("channel-id-input").disabled = !!channel;

    // Lock platform when editing (can't change platform of existing channel)
    if (platformSelect) {
      platformSelect.disabled = !!channel;
    }

    dialog.show();
  }

  updateChannelDialogForPlatform(platform) {
    const isTwitch = platform === "twitch";
    const idInput = document.getElementById("channel-id-input");
    const includeVodsRow = document.getElementById("channel-include-vods-row");
    const qualityRow = document.getElementById("channel-quality-row");

    if (idInput) {
      idInput.label = isTwitch ? "Channel Login" : "Channel ID";
      idInput.placeholder = isTwitch ? "username or channel URL" : "UC... or @handle or channel URL";
      idInput.helpText = isTwitch ? "Channel login name or twitch.tv URL" : "Channel ID, @handle, or channel URL";
    }

    // Toggle YouTube-only fields (quality preference applies to both platforms)
    if (includeVodsRow) includeVodsRow.style.display = isTwitch ? "none" : "";
    if (qualityRow) qualityRow.style.display = "";
  }

  editChannel(channelId) {
    const channel = this.app.config?.channels?.find((c) => c.id === channelId);
    if (channel) {
      this.showAddChannelDialog(channel);
    }
  }

  async saveChannel() {
    let id = document.getElementById("channel-id-input").value.trim();
    let name = document.getElementById("channel-name-input").value.trim();
    const termsValue = document
      .getElementById("channel-terms-input")
      .value.trim();
    const includeVods = document.getElementById("channel-include-vods").checked;

    if (!id) {
      this.app.showToast("Channel ID is required", "warning");
      return;
    }

    // Read platform and quality
    const platformSelect = document.getElementById("channel-platform-select");
    let platform = platformSelect ? platformSelect.value : "youtube";

    const enabledSwitch = document.getElementById("channel-enabled-switch");
    const enabled = enabledSwitch ? enabledSwitch.checked : true;

    // Resolve channel URL if it looks like a URL (only for new channels)
    if (!this.editingChannelId && (id.includes("youtube.com") || id.includes("youtu.be") || id.includes("twitch.tv"))) {
      const saveBtn = document.getElementById("channel-save-btn");
      if (saveBtn) { saveBtn.loading = true; saveBtn.disabled = true; }
      try {
        const resp = await fetch("/api/resolve-channel", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ input: id }),
        });
        if (resp.ok) {
          const resolved = await resp.json();
          if (resolved.id) {
            id = resolved.id;
            document.getElementById("channel-id-input").value = id;
          }
          if (resolved.name && !name) {
            name = resolved.name;
            document.getElementById("channel-name-input").value = name;
          }
          if (resolved.platform) {
            platform = resolved.platform;
            if (platformSelect) {
              platformSelect.value = platform;
              this.updateChannelDialogForPlatform(platform);
            }
          }
        } else {
          const data = await resp.json().catch(() => ({ error: resp.statusText }));
          this.app.showToast(data.error || "Failed to resolve channel URL", "danger");
          return;
        }
      } catch (e) {
        this.app.showToast("Failed to resolve channel URL: " + e.message, "danger");
        return;
      } finally {
        if (saveBtn) { saveBtn.loading = false; saveBtn.disabled = false; }
      }
    }

    const isTwitch = platform === "twitch";

    const channel = {
      id,
      name: name || undefined,
      terms: termsValue || undefined,
      enabled,
      ...(isTwitch ? { platform: "twitch" } : {}),
      ...(!isTwitch ? { include_non_live_content: includeVods || undefined } : {}),
    };

    // Add quality preference (both platforms)
    const qualitySelect = document.getElementById("channel-quality-select");
    const quality = qualitySelect ? qualitySelect.value : "best";
    if (quality !== "best") {
      channel.quality_preference = quality;
    }

    const saveBtn = document.getElementById("channel-save-btn");
    if (saveBtn) { saveBtn.loading = true; saveBtn.disabled = true; }
    try {
      const response = await fetch("/api/config/channels", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(channel),
      });

      if (response.ok) {
        document.getElementById("add-channel-dialog").hide();
        this.app.showToast(
          this.editingChannelId ? "Channel updated" : "Channel added",
          "success",
        );
        this.app.loadConfig();
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.app.showToast(data.error || "Failed to save channel", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to save channel: " + e.message, "danger");
    } finally {
      if (saveBtn) { saveBtn.loading = false; saveBtn.disabled = false; }
    }
  }

  async deleteChannel(channelId) {
    if (!await this.app.showConfirm("Are you sure you want to remove this channel?", { okLabel: "Remove", okVariant: "danger" })) return;

    try {
      const response = await fetch(
        `/api/config/channels/${encodeURIComponent(channelId)}`,
        {
          method: "DELETE",
        },
      );

      if (response.ok) {
        this.app.showToast("Channel removed", "success");
        this.app.loadConfig();
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.app.showToast(data.error || "Failed to remove channel", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to remove channel: " + e.message, "danger");
    }
  }

  async toggleChannel(channelId, enabled) {
    const channel = this.app.config?.channels?.find((c) => c.id === channelId);
    if (!channel) return;

    try {
      const response = await fetch("/api/config/channels", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...channel, enabled }),
      });

      if (response.ok) {
        this.app.loadConfig();
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.app.showToast(data.error || "Failed to update channel", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to update channel: " + e.message, "danger");
    }
  }

  renderNotificationsList() {
    const container = document.getElementById("notifications-list");
    if (!container || !this.app.config) return;

    const notifications = this.app.config.notifications || [];

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
          const grouped = NOTIFICATION_EVENT_GROUPS.map((group) => {
            const chips = group.events
              .map((evt) => {
                const active = notif.events.includes(evt.id);
                const variant = active ? 'variant="primary"' : "";
                return `<sl-tag size="small" ${variant} data-notif-action="toggle-event" data-notif-index="${idx}" data-event-id="${evt.id}">${evt.label}</sl-tag>`;
              })
              .join("");
            return `<div class="notification-event-group"><span class="notification-group-label">${group.name}:</span>${chips}</div>`;
          }).join("");
          eventsHtml = `
            <div class="notification-events">
              ${grouped}
              <div class="notification-event-group">
                <sl-tag size="small" variant="neutral" data-notif-action="clear-filter" data-notif-index="${idx}">Clear filter</sl-tag>
              </div>
            </div>`;
        } else {
          eventsHtml = `
            <div class="notification-events">
              <div class="notification-event-group">
                <span class="notification-events-label">Events:</span>
                <sl-tag size="small" variant="success">All events</sl-tag>
                <sl-tag size="small" variant="neutral" data-notif-action="enable-filter" data-notif-index="${idx}">Filter...</sl-tag>
              </div>
            </div>`;
        }

        return `
      <div class="notification-card" data-index="${idx}">
        <div class="notification-card-header">
          <div class="notification-card-url" title="${this.app.escapeHtml(notif.url || "")}">${this.app.escapeHtml(notif.url || "")}</div>
          <sl-icon-button name="trash" label="Delete" data-notif-action="delete" data-notif-index="${idx}"></sl-icon-button>
        </div>
        ${eventsHtml}
      </div>`;
      })
      .join("");

    // Event delegation for notification actions — attach once
    if (!container._notifDelegated) {
      container._notifDelegated = true;
      container.addEventListener("click", (e) => {
        const el = e.target.closest("[data-notif-action]");
        if (!el) return;
        const action = el.dataset.notifAction;
        const idx = parseInt(el.dataset.notifIndex);
        if (action === "delete") this.deleteNotification(idx);
        else if (action === "toggle-event") this.toggleNotificationEvent(idx, el.dataset.eventId);
        else if (action === "clear-filter") this.clearNotificationFilter(idx);
        else if (action === "enable-filter") this.enableNotificationFilter(idx);
      });
    }
  }

  addNotification() {
    const dialog = document.getElementById("webhook-dialog");
    const input = document.getElementById("webhook-url-input");
    const confirmBtn = document.getElementById("webhook-confirm-btn");
    input.value = "";

    // Replace to avoid stacking listeners (consistent with showConfirm/removePassword)
    const newConfirm = confirmBtn.cloneNode(true);
    confirmBtn.replaceWith(newConfirm);
    newConfirm.addEventListener("click", async () => {
      const url = input.value.trim();
      if (!url) {
        this.app.showToast("Please enter a URL", "warning");
        return;
      }

      if (!this.app.config.notifications) {
        this.app.config.notifications = [];
      }

      this.app.config.notifications.push({ url });
      try {
        await this._saveNotificationsOnly();
        dialog.hide();
        this.renderNotificationsList();
      } catch {
        // Revert local change if save failed
        this.app.config.notifications.pop();
      }
    });

    dialog.show();
    setTimeout(() => input.focus(), 100);
  }

  async deleteNotification(index) {
    if (!await this.app.showConfirm("Remove this notification webhook?", { okLabel: "Remove", okVariant: "danger" })) return;

    if (this.app.config.notifications) {
      const [removed] = this.app.config.notifications.splice(index, 1);
      try {
        await this._saveNotificationsOnly();
      } catch {
        // Revert on failure
        this.app.config.notifications.splice(index, 0, removed);
      }
      this.renderNotificationsList();
    }
  }

  async toggleNotificationEvent(index, eventId) {
    const notif = this.app.config.notifications?.[index];
    if (!notif || !Array.isArray(notif.events)) return;

    // Save a copy in case the save fails
    const previousEvents = [...notif.events];

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

    try {
      await this._saveNotificationsOnly();
    } catch {
      // Revert on failure
      notif.events = previousEvents;
    }
    this.renderNotificationsList();
  }

  async enableNotificationFilter(index) {
    const notif = this.app.config.notifications?.[index];
    if (!notif) return;

    const previousEvents = notif.events;
    notif.events = [...ALL_EVENT_IDS];
    try {
      await this._saveNotificationsOnly();
    } catch {
      notif.events = previousEvents;
    }
    this.renderNotificationsList();
  }

  async clearNotificationFilter(index) {
    const notif = this.app.config.notifications?.[index];
    if (!notif) return;

    const previousEvents = notif.events;
    delete notif.events;
    try {
      await this._saveNotificationsOnly();
    } catch {
      notif.events = previousEvents;
    }
    this.renderNotificationsList();
  }

  /**
   * Save only the notifications section to avoid side-effects on other unsaved
   * settings. The server-side PUT /api/config merges — omitted sections are untouched.
   */
  async _saveNotificationsOnly() {
    let response;
    try {
      response = await fetch("/api/config", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ notifications: this.app.config.notifications || [] }),
      });
    } catch (e) {
      this.app.showToast("Failed to save notifications: " + e.message, "danger");
      throw e;
    }
    if (response.ok) {
      this.app.showToast("Notifications updated", "success");
    } else {
      const data = await response.json().catch(() => ({ error: response.statusText }));
      this.app.showToast(data.error || "Failed to save notifications", "danger");
      throw new Error(data.error || "Failed to save notifications");
    }
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
        badge.textContent = "Config mismatch";
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
      const scheme = status.httpsEnabled ? "https" : "http";
      extractorCmd.textContent = `yt-dlp --extractor-args "youtube:player-client=web;po_token=web.gvs+${scheme}://127.0.0.1:${status.currentPort}/get_pot" <URL>`;
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
          this.app.showToast("yt-dlp plugin already installed with correct port", "primary");
        } else {
          this.app.showToast("yt-dlp plugin installed successfully", "success");
        }
        // Refresh status
        await this.loadYtdlpPluginStatus();
      } else {
        this.app.showToast(data.error || "Failed to install plugin", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to install plugin: " + e.message, "danger");
    } finally {
      installBtn.loading = false;
      installBtn.disabled = false;
    }
  }

  // ─── Security / Password Methods ─────────────────────────────

  async loadSecurityStatus() {
    const badge = document.getElementById("security-password-badge");
    const currentPasswordInput = document.getElementById("security-current-password");
    const setPasswordBtn = document.getElementById("security-set-password-btn");
    const removeSection = document.getElementById("security-remove-section");

    try {
      const response = await fetch("/api/auth/status");
      if (!response.ok) return;
      const status = await response.json();

      if (status.hasPassword) {
        badge.variant = "success";
        badge.textContent = "Password set";
        if (currentPasswordInput) currentPasswordInput.style.display = "";
        if (setPasswordBtn) {
          setPasswordBtn.textContent = "";
          const icon = document.createElement("sl-icon");
          icon.setAttribute("slot", "prefix");
          icon.setAttribute("name", "key");
          setPasswordBtn.appendChild(icon);
          setPasswordBtn.appendChild(document.createTextNode(" Change Password"));
        }
        if (removeSection) removeSection.style.display = "";
      } else {
        badge.variant = "neutral";
        badge.textContent = "No password set";
        if (currentPasswordInput) currentPasswordInput.style.display = "none";
        if (setPasswordBtn) {
          setPasswordBtn.textContent = "";
          const icon = document.createElement("sl-icon");
          icon.setAttribute("slot", "prefix");
          icon.setAttribute("name", "key");
          setPasswordBtn.appendChild(icon);
          setPasswordBtn.appendChild(document.createTextNode(" Set Password"));
        }
        if (removeSection) removeSection.style.display = "none";
      }
    } catch (e) {
      console.error("Failed to load security status:", e);
    }

    // Clear form fields
    if (currentPasswordInput) currentPasswordInput.value = "";
    document.getElementById("security-new-password").value = "";
    document.getElementById("security-confirm-password").value = "";

    // Load connected clients
    this.loadClientTokens();
  }

  async setPassword() {
    const currentPasswordInput = document.getElementById("security-current-password");
    const newPasswordInput = document.getElementById("security-new-password");
    const confirmPasswordInput = document.getElementById("security-confirm-password");
    const btn = document.getElementById("security-set-password-btn");

    const newPassword = newPasswordInput.value;
    const confirmPassword = confirmPasswordInput.value;

    if (!newPassword || newPassword.length < 8) {
      this.app.showToast("Password must be at least 8 characters", "warning");
      return;
    }

    if (newPassword !== confirmPassword) {
      this.app.showToast("Passwords do not match", "warning");
      return;
    }

    const body = { newPassword };
    // Include current password if the field is visible (password already set)
    if (currentPasswordInput && currentPasswordInput.style.display !== "none") {
      body.currentPassword = currentPasswordInput.value;
      if (!body.currentPassword) {
        this.app.showToast("Current password is required", "warning");
        return;
      }
    }

    btn.loading = true;
    try {
      const response = await fetch("/api/auth/set-password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });

      if (response.ok) {
        this.app.showToast("Password updated successfully", "success");
        this.loadSecurityStatus();
        // Refresh config to pick up hasPassword change
        this.app.loadConfig();
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.app.showToast(data.error || "Failed to set password", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to set password: " + e.message, "danger");
    } finally {
      btn.loading = false;
    }
  }

  removePassword() {
    const dialog = document.getElementById("remove-password-dialog");
    const input = document.getElementById("remove-password-input");
    const confirmBtn = document.getElementById("remove-password-confirm-btn");
    const cancelBtn = document.getElementById("remove-password-cancel-btn");

    input.value = "";

    const cleanup = () => {
      dialog.removeEventListener("sl-after-hide", onHide);
    };
    const onHide = () => cleanup();

    const newConfirm = confirmBtn.cloneNode(true);
    confirmBtn.replaceWith(newConfirm);
    newConfirm.addEventListener("click", async () => {
      const currentPassword = input.value;
      if (!currentPassword) {
        this.app.showToast("Password is required", "warning");
        return;
      }

      newConfirm.loading = true;
      try {
        const response = await fetch("/api/auth/remove-password", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ currentPassword }),
        });

        if (response.ok) {
          dialog.hide();
          this.app.showToast("Password removed. Network access set to Localhost Only.", "success");
          this.loadSecurityStatus();
          this.app.loadConfig();
        } else {
          const data = await response.json().catch(() => ({ error: response.statusText }));
          this.app.showToast(data.error || "Failed to remove password", "danger");
        }
      } catch (e) {
        this.app.showToast("Failed to remove password: " + e.message, "danger");
      } finally {
        newConfirm.loading = false;
      }
    });

    const newCancel = cancelBtn.cloneNode(true);
    cancelBtn.replaceWith(newCancel);
    newCancel.addEventListener("click", () => dialog.hide());

    dialog.addEventListener("sl-after-hide", onHide, { once: true });
    dialog.show();
    setTimeout(() => input.focus(), 100);
  }

  // ─── Client Token Methods ────────────────────────────────────

  async loadClientTokens() {
    const container = document.getElementById("client-tokens-list");
    if (!container) return;

    try {
      const response = await fetch("/api/client-tokens");
      if (!response.ok) {
        container.innerHTML = '<span style="color: var(--sl-color-neutral-500);">Unable to load tokens</span>';
        return;
      }
      const tokens = await response.json();

      if (!tokens || tokens.length === 0) {
        container.innerHTML = '<span style="color: var(--sl-color-neutral-500);">No connected clients</span>';
        return;
      }

      container.innerHTML = "";
      for (const token of tokens) {
        const row = document.createElement("div");
        row.className = "client-token-row";

        const label = document.createElement("span");
        label.className = "client-token-label";
        label.textContent = token.label || "Unknown";
        label.title = token.label || "";

        const meta = document.createElement("span");
        meta.className = "client-token-meta";
        const lastUsed = token.lastUsedAt ? this.formatRelativeTime(token.lastUsedAt) : "never";
        meta.textContent = lastUsed;

        const revokeBtn = document.createElement("sl-icon-button");
        revokeBtn.name = "x-circle";
        revokeBtn.label = "Revoke";
        revokeBtn.style.fontSize = "1rem";
        revokeBtn.addEventListener("click", () => this.revokeClientToken(token.id));

        row.appendChild(label);
        row.appendChild(meta);
        row.appendChild(revokeBtn);
        container.appendChild(row);
      }
    } catch (e) {
      console.error("Failed to load client tokens:", e);
      container.innerHTML = '<span style="color: var(--sl-color-neutral-500);">Error loading tokens</span>';
    }
  }

  async revokeClientToken(id) {
    try {
      const response = await fetch(`/api/client-tokens/${id}`, { method: "DELETE" });
      if (response.ok) {
        this.app.showToast("Client token revoked", "success");
        this.loadClientTokens();
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        this.app.showToast(data.error || "Failed to revoke token", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to revoke token: " + e.message, "danger");
    }
  }

  formatRelativeTime(isoDate) {
    if (!isoDate) return "never";
    return formatRelativeTime(isoDate);
  }

  // ─── Auto Cookie Methods ─────────────────────────────────────

  updateAutoCookieUI() {
    const enabled = document.getElementById("cfg-auto-cookies-enabled")?.checked;
    const actionsDiv = document.getElementById("auto-cookie-actions");
    if (actionsDiv) {
      actionsDiv.style.display = enabled ? "" : "none";
    }
    // Show/hide per-platform setup buttons based on active toggles
    const ytActive = document.getElementById("cfg-active-youtube")?.checked;
    const twActive = document.getElementById("cfg-active-twitch")?.checked;
    const ytBtn = document.getElementById("btn-auto-cookie-setup-yt");
    const twBtn = document.getElementById("btn-auto-cookie-setup-tw");
    if (ytBtn) ytBtn.style.display = (enabled && ytActive) ? "" : "none";
    if (twBtn) twBtn.style.display = (enabled && twActive) ? "" : "none";
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
            let info = `Detected: ${status.browser.name || status.browser.type}`;
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
        if (status.setupInProgress) {
          const dialog = document.getElementById("auto-cookie-setup-dialog");
          if (dialog && !dialog.open) dialog.show();
        }
      }
    } catch (e) {
      console.error("Failed to load auto-cookie status:", e);
    }
  }

  async startAutoCookieSetup(platform) {
    if (!platform) return;

    const resultEl = document.getElementById("auto-cookie-setup-result");
    if (resultEl) {
      resultEl.textContent = "";
      resultEl.style.color = "";
    }

    try {
      const response = await fetch("/api/cookies/auto-setup/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ platform }),
      });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const data = await response.json();

      if (data.success) {
        const dialog = document.getElementById("auto-cookie-setup-dialog");
        const instructions = document.getElementById("auto-cookie-instructions");

        if (instructions) {
          if (platform === "twitch") {
            instructions.textContent = "A browser window has opened to Twitch. Please sign in, then click \"I'm Logged In\".";
          } else {
            instructions.textContent = "A browser window has opened to Google. Please sign in to YouTube, then click \"I'm Logged In\".";
          }
        }

        if (dialog) {
          dialog.label = platform === "twitch" ? "Setup: Twitch" : "Setup: YouTube";
          dialog.show();
        }
      } else {
        this.app.showToast(data.error || "Failed to start setup", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to start auto-cookie setup: " + e.message, "danger");
    }
  }

  async finishAutoCookieSetup() {
    const resultEl = document.getElementById("auto-cookie-setup-result");
    const doneBtn = document.getElementById("btn-auto-cookie-done");
    if (doneBtn) doneBtn.loading = true;
    if (resultEl) {
      resultEl.textContent = "Extracting cookies...";
      resultEl.style.color = "";
    }

    try {
      const response = await fetch("/api/cookies/auto-setup/finish", { method: "POST" });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const data = await response.json();

      const ytOk = data.authenticated;
      const twOk = data.twitchAuthenticated;

      if (ytOk || twOk) {
        if (resultEl) resultEl.textContent = "";
        const dialog = document.getElementById("auto-cookie-setup-dialog");
        if (dialog) dialog.hide();
        const parts = [];
        if (ytOk) parts.push("YouTube: \u2713");
        if (twOk) parts.push("Twitch: \u2713");
        this.app.showToast(`Setup complete \u2014 ${parts.join(", ")}`, "success");
        this.app.loadStatus();
      } else {
        if (resultEl) {
          resultEl.textContent = data.error || "No login detected. Try again.";
          resultEl.style.color = "var(--sl-color-danger-600)";
        }
      }
    } catch (e) {
      if (resultEl) {
        resultEl.textContent = "Error: " + e.message;
        resultEl.style.color = "var(--sl-color-danger-600)";
      }
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
    if (dialog) dialog.hide();
    const resultEl = document.getElementById("auto-cookie-setup-result");
    if (resultEl) {
      resultEl.textContent = "";
      resultEl.style.color = "";
    }
  }
}
