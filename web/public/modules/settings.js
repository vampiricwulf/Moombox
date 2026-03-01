/**
 * Settings Controller — Config UI, channels, notifications, cookies, yt-dlp plugin
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

    // Reload config
    const reloadBtn = document.getElementById("reload-config-btn");
    if (reloadBtn) {
      reloadBtn.addEventListener("click", () => this.app.loadConfig());
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

    // Active platform toggles — use explicit override or infer from activePlatforms status
    const activePlats = config.cookies?.active_platforms;
    const activeYtSwitch = document.getElementById("cfg-active-youtube");
    const activeTwSwitch = document.getElementById("cfg-active-twitch");
    if (activeYtSwitch) {
      if (activePlats && activePlats.length > 0) {
        activeYtSwitch.checked = activePlats.includes("youtube");
      } else {
        activeYtSwitch.checked = this.app.activePlatforms?.youtube !== false;
      }
    }
    if (activeTwSwitch) {
      if (activePlats && activePlats.length > 0) {
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

    // Build nested config object
    config.network = {
      port,
      network_access: networkAccess,
      https_enabled: httpsEnabled,
      ...(tlsCertPath ? { tls_cert_path: tlsCertPath } : {}),
      ...(tlsKeyPath ? { tls_key_path: tlsKeyPath } : {}),
    };

    config.paths = {
      database_path: database || undefined,
      log_file_path: logFile || undefined,
      output_directory: outputDir || undefined,
      staging_directory: stagingDir || undefined,
      ffmpeg_path: ffmpegPath || undefined,
    };

    config.logs = {
      log_level: logLevel || undefined,
      log_max_file_size: logMaxSize,
      log_max_files: logMaxFiles,
    };

    config.monitors = {
      max_feed_items: maxFeedItems,
      feed_check_interval: feedCheckInterval,
      hide_finished_age_days: hideFinishedDays,
    };
    if (decapiCheckInterval) {
      config.monitors.decapi_check_interval = decapiCheckInterval;
    }
    if (twitchCheckInterval) {
      config.monitors.twitch_check_interval = twitchCheckInterval;
    }

    config.downloader = {
      output_template: outputTemplate || undefined,
      max_video_resolution: maxResolution,
      num_parallel_downloads: parallelDownloads,
      download_chat: downloadChat,
      prefer_60fps: prefer60fps,
      segment_retry_delay_cap: retryDelayCap,
      segment_live_check_retries: liveCheckRetries,
    };

    config.cookies = {
      cookie_file: cookieFile || undefined,
      active_platforms: activePlatforms,
      auto_enabled: autoEnabled,
      browser_profile_dir: autoCookiesProfileDir || undefined,
    };

    try {
      const response = await fetch("/api/config", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(config),
      });

      if (response.ok) {
        this._dirty = false;
        this._updateUnsavedIndicator();
        this.app.showToast("Settings saved successfully", "success");
        // Check if any restart-required settings changed
        this._checkRestartRequired(config);
      } else {
        const data = await response.json();
        let msg = data.error || "Failed to save settings";
        if (data.details) {
          const fields = Object.entries(data.details).map(([k, v]) => `${k}: ${v}`).join(", ");
          msg += ` (${fields})`;
        }
        this.app.showToast(msg, "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to save settings: " + e.message, "danger");
    }
  }

  /**
   * After a successful save, check if restart-requiring settings changed.
   * If so, prompt the user and call POST /api/restart.
   */
  _checkRestartRequired(config) {
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

    const changed = RESTART_REQUIRED_FIELDS.some(
      ({ path }) => String(current[path] ?? "") !== String(this._originalRestartValues[path] ?? ""),
    );
    if (!changed) return;

    // Update snapshot so we don't prompt again
    this._originalRestartValues = { ...current };

    if (!confirm("Some settings require a restart to take effect (port, network access, database path, log settings).\n\nRestart Moombox now?")) {
      return;
    }

    this.app.showToast("Restarting Moombox...", "primary");

    fetch("/api/restart", { method: "POST" }).catch(() => {
      // Connection will drop during restart — expected
    });
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
            <sl-switch size="small" ${isEnabled ? "checked" : ""} title="${isEnabled ? "Monitoring enabled" : "Monitoring disabled"}" onclick="app.settings.toggleChannel('${this.app.escapeHtml(ch.id)}', this.checked); event.stopPropagation();"></sl-switch>
            <sl-icon-button name="pencil" label="Edit" onclick="app.settings.editChannel('${this.app.escapeHtml(ch.id)}')"></sl-icon-button>
            <sl-icon-button name="trash" label="Delete" onclick="app.settings.deleteChannel('${this.app.escapeHtml(ch.id)}')"></sl-icon-button>
          </div>
        </div>
        ${
          ch.terms
            ? `<div class="channel-card-terms">Filter: <code>${this.app.escapeHtml(typeof ch.terms === "string" ? ch.terms : ch.terms.stream || "")}</code></div>`
            : ""
        }
        ${ch.include_non_live_content ? '<sl-badge variant="neutral">Include VODs</sl-badge>' : ""}
        ${isTwitch && ch.quality_preference ? `<sl-badge variant="neutral">Quality: ${this.app.escapeHtml(ch.quality_preference)}</sl-badge>` : ""}
      </div>
    `;
        },
      )
      .join("");

    this.setupChannelDragDrop();
  }

  setupChannelDragDrop() {
    const container = document.getElementById("channels-list");
    if (!container) return;

    let draggedIndex = null;

    container.querySelectorAll(".channel-card[draggable]").forEach((card) => {
      card.addEventListener("dragstart", (e) => {
        draggedIndex = parseInt(card.dataset.index);
        card.classList.add("dragging");
        e.dataTransfer.effectAllowed = "move";
      });

      card.addEventListener("dragover", (e) => {
        e.preventDefault();
        e.dataTransfer.dropEffect = "move";
        // Remove previous drag-over indicators
        container.querySelectorAll(".drag-over").forEach((el) => el.classList.remove("drag-over"));
        card.classList.add("drag-over");
      });

      card.addEventListener("dragleave", () => {
        card.classList.remove("drag-over");
      });

      card.addEventListener("drop", (e) => {
        e.preventDefault();
        const targetIndex = parseInt(card.dataset.index);
        card.classList.remove("drag-over");

        if (draggedIndex !== null && draggedIndex !== targetIndex && this.app.config?.channels) {
          const channels = this.app.config.channels;
          const [moved] = channels.splice(draggedIndex, 1);
          channels.splice(targetIndex, 0, moved);

          // Save reordered channels
          this.saveConfig().then(() => {
            this.renderChannelsList();
            this.app.showToast("Channel order updated", "success");
          });
        }
      });

      card.addEventListener("dragend", () => {
        draggedIndex = null;
        container.querySelectorAll(".dragging, .drag-over").forEach((el) => {
          el.classList.remove("dragging", "drag-over");
        });
      });
    });
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
      idInput.placeholder = isTwitch ? "streamer_username" : "UCxxxxxxxxxxxxxxxxxxxxxxxx";
      idInput.helpText = isTwitch ? "Twitch channel login name (lowercase)" : "YouTube channel ID (starts with UC)";
    }

    // Toggle YouTube-only and Twitch-only fields
    if (includeVodsRow) includeVodsRow.style.display = isTwitch ? "none" : "";
    if (qualityRow) qualityRow.style.display = isTwitch ? "" : "none";
  }

  editChannel(channelId) {
    const channel = this.app.config?.channels?.find((c) => c.id === channelId);
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
      this.app.showToast("Channel ID is required", "warning");
      return;
    }

    // Read platform and quality
    const platformSelect = document.getElementById("channel-platform-select");
    const platform = platformSelect ? platformSelect.value : "youtube";
    const isTwitch = platform === "twitch";

    const enabledSwitch = document.getElementById("channel-enabled-switch");
    const enabled = enabledSwitch ? enabledSwitch.checked : true;

    const channel = {
      id,
      name: name || undefined,
      terms: termsValue || undefined,
      enabled,
      ...(isTwitch ? { platform: "twitch" } : {}),
      ...(!isTwitch ? { include_non_live_content: includeVods || undefined } : {}),
    };

    // Add Twitch quality preference
    if (isTwitch) {
      const qualitySelect = document.getElementById("channel-quality-select");
      const quality = qualitySelect ? qualitySelect.value : "best";
      if (quality !== "best") {
        channel.quality_preference = quality;
      }
    }

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
        const data = await response.json();
        this.app.showToast(data.error || "Failed to save channel", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to save channel: " + e.message, "danger");
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
        this.app.showToast("Channel removed", "success");
        this.app.loadConfig();
      } else {
        const data = await response.json();
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
        const data = await response.json();
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
          const chips = ALL_NOTIFICATION_EVENTS.map((evt) => {
            const active = notif.events.includes(evt.id);
            const variant = active ? 'variant="primary"' : "";
            return `<sl-tag size="small" ${variant} onclick="app.settings.toggleNotificationEvent(${idx}, '${evt.id}')">${evt.label}</sl-tag>`;
          }).join("");
          eventsHtml = `
            <div class="notification-events">
              <span class="notification-events-label">Events:</span>
              ${chips}
              <sl-tag size="small" variant="neutral" onclick="app.settings.clearNotificationFilter(${idx})">Clear filter</sl-tag>
            </div>`;
        } else {
          eventsHtml = `
            <div class="notification-events">
              <span class="notification-events-label">Events:</span>
              <sl-tag size="small" variant="success">All events</sl-tag>
              <sl-tag size="small" variant="neutral" onclick="app.settings.enableNotificationFilter(${idx})">Filter...</sl-tag>
            </div>`;
        }

        return `
      <div class="notification-card" data-index="${idx}">
        <div class="notification-card-header">
          <div class="notification-card-url">${this.app.escapeHtml(notif.url || "")}</div>
          <sl-icon-button name="trash" label="Delete" onclick="app.settings.deleteNotification(${idx})"></sl-icon-button>
        </div>
        ${eventsHtml}
      </div>`;
      })
      .join("");
  }

  addNotification() {
    const dialog = document.getElementById("webhook-dialog");
    const input = document.getElementById("webhook-url-input");
    const confirmBtn = document.getElementById("webhook-confirm-btn");
    input.value = "";

    // Wire confirm handler (replace to avoid stacking listeners)
    confirmBtn.onclick = async () => {
      const url = input.value.trim();
      if (!url) {
        this.app.showToast("Please enter a URL", "warning");
        return;
      }

      if (!this.app.config.notifications) {
        this.app.config.notifications = [];
      }

      this.app.config.notifications.push({ url });
      dialog.hide();
      await this.saveConfig();
      this.renderNotificationsList();
    };

    dialog.show();
    setTimeout(() => input.focus(), 100);
  }

  async deleteNotification(index) {
    if (!confirm("Remove this notification webhook?")) return;

    if (this.app.config.notifications) {
      this.app.config.notifications.splice(index, 1);
      await this.saveConfig();
      this.renderNotificationsList();
    }
  }

  async toggleNotificationEvent(index, eventId) {
    const notif = this.app.config.notifications?.[index];
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
    const notif = this.app.config.notifications?.[index];
    if (!notif) return;

    notif.events = [...ALL_EVENT_IDS];
    await this.saveConfig();
    this.renderNotificationsList();
  }

  async clearNotificationFilter(index) {
    const notif = this.app.config.notifications?.[index];
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
        const data = await response.json();
        this.app.showToast(data.error || "Failed to set password", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to set password: " + e.message, "danger");
    } finally {
      btn.loading = false;
    }
  }

  async removePassword() {
    const currentPassword = prompt("Enter your current password to confirm removal:");
    if (!currentPassword) return;

    const btn = document.getElementById("security-remove-password-btn");
    btn.loading = true;

    try {
      const response = await fetch("/api/auth/remove-password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ currentPassword }),
      });

      if (response.ok) {
        const data = await response.json();
        this.app.showToast("Password removed. Network access set to Localhost Only.", "success");
        this.loadSecurityStatus();
        // Refresh config to pick up changes
        this.app.loadConfig();
      } else {
        const data = await response.json();
        this.app.showToast(data.error || "Failed to remove password", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to remove password: " + e.message, "danger");
    } finally {
      btn.loading = false;
    }
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
