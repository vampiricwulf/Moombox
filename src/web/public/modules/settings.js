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

export class SettingsController {
  constructor(app) {
    this.app = app;
    this.editingChannelId = null;
    this._netAccessListenerAdded = false;
  }

  setupListeners() {
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

  populateConfigForm() {
    const config = this.app.config;
    if (!config) return;

    // General settings
    this.app.setInputValue("cfg-port", config.port);
    // Network access select
    const netAccessSelect = document.getElementById("cfg-network-access");
    const cfgExtWarning = document.getElementById("cfg-external-warning");
    if (netAccessSelect) {
      const level = config.network_access || "localhost";
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
    if (logLevelSelect && config.log_level) {
      logLevelSelect.value = config.log_level;
    }
    this.app.setInputValue("cfg-log-file", config.log_file_path);
    this.app.setInputValue("cfg-log-max-size", config.log_max_file_size);
    this.app.setInputValue("cfg-log-max-files", config.log_max_files);
    this.app.setInputValue("cfg-database", config.database_path);
    this.app.setInputValue("cfg-max-feed-items", config.max_feed_items);
    this.app.setInputValue("cfg-feed-check-interval", config.feed_check_interval);
    this.app.setInputValue(
      "cfg-hide-finished-days",
      config.tasklist?.hide_finished_age_days,
    );

    // Downloader settings
    if (config.downloader) {
      this.app.setInputValue("cfg-output-dir", config.downloader.output_directory);
      this.app.setInputValue("cfg-output-template", config.downloader.output_template);
      this.app.setInputValue("cfg-staging-dir", config.downloader.staging_directory);
      this.app.setInputValue("cfg-ffmpeg-path", config.downloader.ffmpeg_path);
      this.app.setInputValue("cfg-cookie-file", config.downloader.cookie_file);
      this.app.setInputValue("cfg-max-resolution", config.downloader.max_video_resolution);
      this.app.setInputValue("cfg-parallel-downloads", config.downloader.num_parallel_downloads);
      // Download chat switch
      const downloadChatSwitch = document.getElementById("cfg-download-chat");
      if (downloadChatSwitch) {
        downloadChatSwitch.checked = config.downloader.download_chat !== false;
      }
      // Prefer 60fps switch
      const prefer60fpsSwitch = document.getElementById("cfg-prefer-60fps");
      if (prefer60fpsSwitch) {
        prefer60fpsSwitch.checked = config.downloader.prefer_60fps !== false;
      }
      this.app.setInputValue("cfg-retry-delay-cap", config.downloader.segment_retry_delay_cap);
      this.app.setInputValue("cfg-live-check-retries", config.downloader.segment_live_check_retries);
    }

    // Auto cookies settings
    const autoCookiesSwitch = document.getElementById("cfg-auto-cookies-enabled");
    if (autoCookiesSwitch) {
      autoCookiesSwitch.checked = config.auto_cookies?.enabled === true;
    }
    this.app.setInputValue(
      "cfg-auto-cookies-profile-dir",
      config.auto_cookies?.browser_profile_dir,
    );
    this.updateAutoCookieUI();
  }

  async saveConfig() {
    if (!this.app.config) {
      this.app.config = { downloader: {} };
    }
    const config = this.app.config;

    // Gather values from form
    const port = this.app.getInputNumber("cfg-port");
    const logLevelSelect = document.getElementById("cfg-log-level");
    const logLevel = logLevelSelect ? logLevelSelect.value : "";
    const logFile = this.app.getInputValue("cfg-log-file");
    const logMaxSize = this.app.getInputNumber("cfg-log-max-size");
    const logMaxFiles = this.app.getInputNumber("cfg-log-max-files");
    const database = this.app.getInputValue("cfg-database");
    const maxFeedItems = this.app.getInputNumber("cfg-max-feed-items");
    const feedCheckInterval = this.app.getInputNumber("cfg-feed-check-interval");
    const hideFinishedDays = this.app.getInputNumber("cfg-hide-finished-days");

    const outputDir = this.app.getInputValue("cfg-output-dir");
    const outputTemplate = this.app.getInputValue("cfg-output-template");
    const stagingDir = this.app.getInputValue("cfg-staging-dir");
    const ffmpegPath = this.app.getInputValue("cfg-ffmpeg-path");
    const cookieFile = this.app.getInputValue("cfg-cookie-file");
    const maxResolution = this.app.getInputNumber("cfg-max-resolution");
    const parallelDownloads = this.app.getInputNumber("cfg-parallel-downloads");
    const retryDelayCap = this.app.getInputNumber("cfg-retry-delay-cap");
    const liveCheckRetries = this.app.getInputNumber("cfg-live-check-retries");

    // Network access
    const netAccessSelect = document.getElementById("cfg-network-access");
    const networkAccess = netAccessSelect ? netAccessSelect.value : "localhost";

    // Update config object
    config.port = port;
    config.network_access = networkAccess;
    config.log_level = logLevel || undefined;
    config.log_file_path = logFile || undefined;
    config.log_max_file_size = logMaxSize;
    config.log_max_files = logMaxFiles;
    config.database_path = database || undefined;
    config.max_feed_items = maxFeedItems;
    config.feed_check_interval = feedCheckInterval;

    if (!config.tasklist) config.tasklist = {};
    config.tasklist.hide_finished_age_days = hideFinishedDays;

    if (!config.downloader) config.downloader = {};
    config.downloader.output_directory = outputDir || undefined;
    config.downloader.output_template = outputTemplate || undefined;
    config.downloader.staging_directory = stagingDir || undefined;
    config.downloader.ffmpeg_path = ffmpegPath || undefined;
    config.downloader.cookie_file = cookieFile || undefined;
    config.downloader.max_video_resolution = maxResolution;
    config.downloader.num_parallel_downloads = parallelDownloads;
    config.downloader.segment_retry_delay_cap = retryDelayCap;
    config.downloader.segment_live_check_retries = liveCheckRetries;
    // Download chat switch
    const downloadChatSwitch = document.getElementById("cfg-download-chat");
    config.downloader.download_chat = downloadChatSwitch ? downloadChatSwitch.checked : true;
    // Prefer 60fps switch
    const prefer60fpsSwitch = document.getElementById("cfg-prefer-60fps");
    config.downloader.prefer_60fps = prefer60fpsSwitch ? prefer60fpsSwitch.checked : true;

    // Auto cookies
    if (!config.auto_cookies) config.auto_cookies = {};
    const autoCookiesSwitch = document.getElementById("cfg-auto-cookies-enabled");
    config.auto_cookies.enabled = autoCookiesSwitch ? autoCookiesSwitch.checked : false;
    const autoCookiesProfileDir = this.app.getInputValue("cfg-auto-cookies-profile-dir");
    config.auto_cookies.browser_profile_dir = autoCookiesProfileDir || undefined;

    try {
      const response = await fetch("/api/config", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(config),
      });

      if (response.ok) {
        this.app.showToast("Settings saved successfully", "success");
      } else {
        const data = await response.json();
        this.app.showToast(data.error || "Failed to save settings", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to save settings: " + e.message, "danger");
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
        (ch) => `
      <div class="channel-card" data-channel-id="${this.app.escapeHtml(ch.id)}">
        <div class="channel-card-header">
          <div>
            <div class="channel-card-title">${this.app.escapeHtml(ch.name || ch.id)}</div>
            <div class="channel-card-id">${this.app.escapeHtml(ch.id)}</div>
          </div>
          <div class="channel-card-actions">
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
      </div>
    `,
      )
      .join("");
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

    const dialog = document.getElementById("add-channel-dialog");
    dialog.label = channel ? "Edit Channel" : "Add Channel";
    document.getElementById("channel-save-btn").textContent = channel
      ? "Save Changes"
      : "Add Channel";

    document.getElementById("channel-id-input").disabled = !!channel;

    dialog.show();
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

  async addNotification() {
    const url = prompt("Enter webhook URL:");
    if (!url) return;

    if (!this.app.config.notifications) {
      this.app.config.notifications = [];
    }

    this.app.config.notifications.push({ url });
    await this.saveConfig();
    this.renderNotificationsList();
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
    if (resultEl) resultEl.textContent = "Extracting cookies...";

    try {
      const response = await fetch("/api/cookies/auto-setup/finish", { method: "POST" });
      const data = await response.json();

      if (data.authenticated) {
        if (resultEl) resultEl.textContent = "";
        const dialog = document.getElementById("auto-cookie-setup-dialog");
        if (dialog) dialog.style.display = "none";
        this.app.showToast("Auto-cookie setup complete — authenticated!", "success");
        this.app.loadStatus();
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
}
