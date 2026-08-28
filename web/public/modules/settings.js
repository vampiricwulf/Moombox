/**
 * Settings Controller — Config UI, channels, notifications, cookies, yt-dlp plugin
 */
import {
  browserPathValidationOutcome,
  cookieSetupAbortReport,
  cookieSetupAcceptedToast,
  cookieSetupProbe,
  cookieSetupRejectedMessage,
  formatRelativeTime,
  serverErrorMessage,
} from "./utils.js";

const NOTIFICATION_EVENT_GROUPS = [
  {
    name: "Job Lifecycle",
    events: [
      { id: "found", label: "Found" },
      { id: "added", label: "Added" },
      { id: "scheduled", label: "Scheduled" },
      { id: "rescheduled", label: "Rescheduled" },
      { id: "downloading", label: "Downloading" },
      { id: "quality_split", label: "Quality Split" },
      { id: "gap_split", label: "Gap Split" },
      { id: "muxing", label: "Muxing" },
      { id: "finished", label: "Finished" },
      { id: "error", label: "Error" },
      { id: "cancelled", label: "Cancelled" },
      { id: "auth", label: "Auth" },
    ],
  },
  {
    name: "Connectivity",
    events: [
      { id: "connectivity_pause", label: "Download Paused (Offline)" },
      { id: "connectivity_resume", label: "Download Resumed" },
      { id: "connectivity_split", label: "Finalized During Outage" },
      { id: "connectivity_restored", label: "Outage Alert" },
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
      { id: "disk_critical", label: "Disk Critical" },
      { id: "update_available", label: "Update Available" },
      { id: "update_applied", label: "Update Applied" },
      { id: "update_failed", label: "Update Failed" },
      { id: "crash_recovered", label: "Crash Recovered" },
      { id: "channel_unhealthy", label: "Channel Unhealthy" },
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

/** Render a template preview string using sample data. */
export function renderTemplatePreview(template) {
  const now = new Date();
  const vars = {
    channel: "Miko Ch",
    title: "Singing Stream",
    id: "dQw4w9WgXcQ",
    start_date: now.toISOString().split("T")[0],
    start_time: "20-00-00",
  };
  let result = template || "";
  for (const [key, val] of Object.entries(vars)) {
    result = result.replaceAll("${" + key + "}", val);
  }
  return result ? "Example: " + result + ".mkv" : "";
}

export class SettingsController {
  constructor(app) {
    this.app = app;
    this.editingChannelId = null;
    this._netAccessListenerAdded = false;
    /** Snapshot of restart-required values taken when config is loaded */
    this._originalRestartValues = {};
    this._dirty = false;
    this._dirtyListenersAdded = false;
    /** True once populateBrowserSelector has filled the browser <sl-select> —
     *  saves must not touch browser_path/browser_type before then. */
    this._browserSelectLoaded = false;
    /** True when the yt-dlp plugin button reads "Reinstall" (installed,
     *  matching port) — the install call must then force a rewrite. */
    this._ytdlpReinstall = false;
    /** True while the SERVER is holding a setup slot this controller opened —
     *  not while the dialog is merely visible. The pagehide beacon reads it. */
    this._cookieSetupActive = false;
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
        document.getElementById("settings-unsaved-banner").style.display = "none";
        this.app.loadConfig();
      });
    }

    // Unsaved changes banner buttons
    document.getElementById("settings-banner-discard")?.addEventListener("click", () => {
      this._dirty = false;
      this._updateUnsavedIndicator();
      document.getElementById("settings-unsaved-banner").style.display = "none";
      this.app.loadConfig();
    });
    document.getElementById("settings-banner-save")?.addEventListener("click", () => {
      this.saveConfig();
    });

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

    // FFmpeg install from settings
    document.getElementById("cfg-ffmpeg-install-btn")?.addEventListener("click", () => {
      this.app.setup.showFFmpegOverlay();
    });

    // yt-dlp plugin install
    const ytdlpInstallBtn = document.getElementById("ytdlp-install-btn");
    if (ytdlpInstallBtn) {
      // Force when the button reads "Reinstall" (already installed, port OK) —
      // see loadYtdlpPluginStatus, which maintains the flag.
      ytdlpInstallBtn.addEventListener("click", () => this.installYtdlpPlugin(this._ytdlpReinstall));
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

    // Restart Moombox button (System settings section)
    document.getElementById("btn-restart-now")?.addEventListener("click", () => this.restartNow());

    // View Release Notes button
    const viewNotesBtn = document.getElementById("btn-view-release-notes");
    if (viewNotesBtn) {
      viewNotesBtn.addEventListener("click", async () => {
        viewNotesBtn.loading = true;
        try {
          const resp = await fetch("/api/update/release-notes");
          if (!resp.ok) {
            const err = await resp.text();
            alert("Failed to fetch release notes: " + err);
            return;
          }
          const data = await resp.json();
          // Reuse the existing update-dialog to display the notes
          const dlg = document.getElementById("update-dialog");
          const notes = document.getElementById("update-release-notes");
          if (dlg && notes) {
            dlg.label = "Release Notes — " + data.tagName;
            if (data.releaseNotesHtml) {
              // SECURITY CONTRACT: raw-HTML sink safe ONLY because the server
              // sanitizes via bluemonday.UGCPolicy() (internal/updater) — see
              // the matching comment in app.js showUpdateDialog before
              // changing either side.
              notes.innerHTML = data.releaseNotesHtml;
            } else {
              notes.textContent = data.releaseNotes || "No release notes available.";
            }
            // Viewer mode: this is just a notes viewer, not the update prompt.
            // Hide "Update Now", relabel the dismiss button to "Close", and
            // flag the dialog so app.js's dismissUpdate() plain-hides instead
            // of POSTing /api/update/dismiss (which errors "no update pending"
            // here, and would wrongly SKIP a version if one were pending).
            const updateBtn = document.getElementById("update-now-btn");
            const dismissBtn = document.getElementById("update-dismiss-btn");
            if (updateBtn) updateBtn.style.display = "none";
            if (dismissBtn) dismissBtn.textContent = "Close";
            dlg.dataset.viewerMode = "true";
            dlg.show();
            // Restore the update-prompt state when the dialog closes. Text must
            // match index.html's dismiss button ("Skip this version").
            dlg.addEventListener("sl-after-hide", () => {
              if (updateBtn) updateBtn.style.display = "";
              if (dismissBtn) dismissBtn.textContent = "Skip this version";
              delete dlg.dataset.viewerMode;
            }, { once: true });
          }
        } finally {
          viewNotesBtn.loading = false;
        }
      });
    }

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
          const data = await resp.json();
          if (resp.ok && data.verified) {
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

    // Tell the server when the tab goes away mid-setup. Nothing did, so closing
    // the tab left a browser registered server-side and every later setup and
    // periodic refresh refused ("setup in progress") until something released
    // the slot. On Windows the server-side reap does that on its own, keyed on
    // the browser actually being gone; where there is no Job Object to ask —
    // Linux, Docker — this beacon is the only release there is.
    //
    // IT POSTS /abandon, NOT /cancel, AND THE DIFFERENCE IS A BROWSER WINDOW.
    // Cancel is the user's abort and closes the setup browser; once the Firefox
    // setup gained a Job Object, that became true on the default Windows path
    // too. But this flow's own instructions send the user AWAY from this tab —
    // "a browser window has opened… please sign in" — so closing the now-idle
    // dashboard tab is an entirely natural thing to do MID-LOGIN, and pointing
    // this beacon at /cancel made that a remote kill of the window they were
    // typing their password into. /abandon releases the slot without touching
    // the browser, and declines even to do that where releasing would itself
    // close a Job Object. A click is consent; a tab unload is not.
    //
    // pagehide, NOT beforeunload, and the difference is not cosmetic: the
    // beforeunload above can put a "Leave site?" confirm in front of the user,
    // and a beacon fired from there cancels the setup even when they choose to
    // stay. pagehide only fires once the page really is going. sendBeacon
    // survives the unload where fetch does not (player.js does the same for the
    // resume position); its POST carries an Origin, so it passes CSRF.
    //
    // e.persisted is the other half of picking pagehide. It also fires when the
    // page goes into the back/forward cache — a back navigation, or a phone
    // backgrounding the tab — and that page is coming BACK. Releasing there
    // tears down a live setup (on the server host, which in a LAN or Docker
    // deployment is not even the same machine) and, because the flag is cleared
    // on the way out, leaves the restored page unable to cancel it: the dialog
    // is still up, "I'm Logged In" 404s, and nothing can clean up. Skipping the
    // bfcache case needs no pageshow re-arm precisely because the flag is left
    // alone.
    //
    // visibilitychange, the event usually paired with pagehide for unload
    // reliability, must NOT be added here for the same reason only louder: the
    // one thing this flow asks of the user is to switch to the browser window
    // Moombox just opened, which hides this tab.
    window.addEventListener("pagehide", (e) => {
      if (e.persisted) return;
      if (!this._cookieSetupActive) return;
      this._cookieSetupActive = false;
      navigator.sendBeacon("/api/cookies/auto-setup/abandon");
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
      // A config value with no matching <sl-option> (today: the "public"
      // alias, config-file-only by design) leaves the select blank, because
      // Shoelace drops a .value that matches no option. Say so in the field's
      // own help text rather than leaving an unexplained empty dropdown —
      // saveConfig omits the key in that state, so the stored value is kept.
      if (this._netAccessHelpText === undefined) {
        this._netAccessHelpText = netAccessSelect.getAttribute("help-text") || "";
      }
      const representable = Array.from(netAccessSelect.querySelectorAll("sl-option")).some(
        (opt) => opt.getAttribute("value") === level,
      );
      netAccessSelect.setAttribute(
        "help-text",
        representable
          ? this._netAccessHelpText
          : `Currently "${level}", which is set in config.toml and not offered here. ` +
              `Leave blank to keep it; picking an option replaces it. Requires restart.`,
      );
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
    this.app.setInputValue("cfg-archive-window-days", config.monitors?.archive_window_days);
    this.app.setInputValue("cfg-archive-slots", config.monitors?.archive_slots);
    this.app.setInputValue("cfg-feed-check-interval", config.monitors?.feed_check_interval);
    this.app.setInputValue("cfg-decapi-check-interval", config.monitors?.decapi_check_interval ?? "");
    this.app.setInputValue("cfg-twitch-check-interval", config.monitors?.twitch_check_interval ?? "");
    this.app.setInputValue("cfg-hide-finished-days", config.monitors?.hide_finished_age_days);
    this.app.setInputValue("cfg-probe-cooldown", config.monitors?.probe_cooldown);
    // Membership stream discovery switch (default on: absent/nil => checked)
    const membershipSwitch = document.getElementById("cfg-membership-discovery");
    if (membershipSwitch) {
      membershipSwitch.checked = config.monitors?.membership_discovery !== false;
    }

    // Downloader settings
    this.app.setInputValue("cfg-output-template", config.downloader?.output_template);
    this._wireTemplatePreview();
    this.app.setInputValue("cfg-max-resolution", config.downloader?.max_video_resolution);
    this.app.setInputValue("cfg-parallel-downloads", config.downloader?.num_parallel_downloads);
    this.app.setInputValue("cfg-segment-workers", config.downloader?.segment_workers);
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
    this.app.setInputValue("cfg-maximum-timeout", config.downloader?.maximum_timeout);
    this.app.setInputValue("cfg-interruption-timeout", config.downloader?.interruption_timeout);
    this.app.setInputValue("cfg-incomplete-staging-expiry", config.downloader?.incomplete_staging_expiry_days);

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

    // Memory settings
    this.app.setInputValue("cfg-memory-go-soft-limit-mb", config.memory?.go_soft_limit_mb);
    this.app.setInputValue("cfg-memory-sidecar-soft-limit-mb", config.memory?.sidecar_soft_limit_mb);
    this.app.setInputValue("cfg-memory-sidecar-hard-limit-mb", config.memory?.sidecar_hard_limit_mb);

    // BotGuard sidecar
    const useSidecarSwitch = document.getElementById("cfg-bgutils-use-sidecar");
    if (useSidecarSwitch) {
      useSidecarSwitch.checked = config.bgutils?.use_sidecar !== false;
    }

    // Reverse-proxy / DPAPI toggles
    const trustForwardedProtoSwitch = document.getElementById("cfg-trust-forwarded-proto");
    if (trustForwardedProtoSwitch) {
      trustForwardedProtoSwitch.checked = !!config.network?.trust_forwarded_proto;
    }
    this.app.setInputValue("cfg-trusted-proxies", (config.network?.trusted_proxies || []).join(", "));
    const dpapiFallbackSwitch = document.getElementById("cfg-cookies-dpapi-fallback");
    if (dpapiFallbackSwitch) {
      dpapiFallbackSwitch.checked = !!config.cookies?.dpapi_fallback;
    }

    // Track dirty state
    this._dirty = false;
    this._updateUnsavedIndicator();
    document.getElementById("settings-unsaved-banner")?.style.setProperty("display", "none");
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

    // Network is the default visible section, so load security status now.
    // Reset flag so the form fields are cleared on a full config repopulate —
    // but only if the user isn't mid-entry in a password field (channel/notification
    // operations trigger loadConfig → populateConfigForm, which would clear their input).
    const hasPasswordInput =
      document.getElementById("security-new-password")?.value ||
      document.getElementById("security-confirm-password")?.value ||
      document.getElementById("security-current-password")?.value;
    if (!hasPasswordInput) {
      this._securityStatusLoaded = false;
    }
    this.loadSecurityStatus();
  }

  /** Wire up the live template preview below the output template input. */
  _wireTemplatePreview() {
    const templateInput = document.getElementById("cfg-output-template");
    const templatePreview = document.getElementById("cfg-template-preview");
    if (templateInput && templatePreview) {
      const updatePreview = () => {
        const val = templateInput.value || templateInput.placeholder;
        templatePreview.textContent = renderTemplatePreview(val);
        templatePreview.style.display = val ? "" : "none";
      };
      if (!this._templatePreviewWired) {
        this._templatePreviewWired = true;
        templateInput.addEventListener("sl-input", updatePreview);
      }
      updatePreview();
    }
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

    const archiveWindowDays = this.app.getInputNumber("cfg-archive-window-days");
    const archiveSlots = this.app.getInputNumber("cfg-archive-slots");
    const feedCheckInterval = this.app.getInputNumber("cfg-feed-check-interval");
    const decapiCheckInterval = this.app.getInputNumber("cfg-decapi-check-interval");
    const twitchCheckInterval = this.app.getInputNumber("cfg-twitch-check-interval");
    const hideFinishedDays = this.app.getInputNumber("cfg-hide-finished-days");
    const probeCooldown = this.app.getInputNumber("cfg-probe-cooldown");
    const membershipSwitch = document.getElementById("cfg-membership-discovery");
    const membershipDiscovery = membershipSwitch ? membershipSwitch.checked : true;

    const outputTemplate = this.app.getInputValue("cfg-output-template");
    const maxResolution = this.app.getInputNumber("cfg-max-resolution");
    const parallelDownloads = this.app.getInputNumber("cfg-parallel-downloads");
    const segmentWorkers = this.app.getInputNumber("cfg-segment-workers");
    const maximumTimeout = this.app.getInputNumber("cfg-maximum-timeout");
    const interruptionTimeout = this.app.getInputNumber("cfg-interruption-timeout");
    const incompleteStagingExpiry = this.app.getInputNumber("cfg-incomplete-staging-expiry");
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

    // Browser selection — resolved later after payload is built so we can
    // validate and potentially throw before the fetch
    const browserSelectEl = document.getElementById("cfg-cookies-browser-select");
    const browserSelectVal = browserSelectEl?.value || "";

    const diskWarnPercent = this.app.getInputNumber("cfg-disk-warn-percent");
    const diskCriticalPercent = this.app.getInputNumber("cfg-disk-critical-percent");

    const autoCheckUpdatesSwitch = document.getElementById("cfg-auto-check-updates");
    const autoCheckUpdates = autoCheckUpdatesSwitch ? autoCheckUpdatesSwitch.checked : true;

    const memoryGoSoftMB = this.app.getInputNumber("cfg-memory-go-soft-limit-mb");
    const memorySidecarSoftMB = this.app.getInputNumber("cfg-memory-sidecar-soft-limit-mb");
    const memorySidecarHardMB = this.app.getInputNumber("cfg-memory-sidecar-hard-limit-mb");

    const useSidecarSwitch = document.getElementById("cfg-bgutils-use-sidecar");
    const useSidecar = useSidecarSwitch ? useSidecarSwitch.checked : true;

    const trustForwardedProtoSwitch = document.getElementById("cfg-trust-forwarded-proto");
    const trustForwardedProto = trustForwardedProtoSwitch ? trustForwardedProtoSwitch.checked : false;

    const trustedProxiesEl = document.getElementById("cfg-trusted-proxies");
    const trustedProxies = (trustedProxiesEl?.value || "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);

    const dpapiFallbackSwitch = document.getElementById("cfg-cookies-dpapi-fallback");
    const dpapiFallback = dpapiFallbackSwitch ? dpapiFallbackSwitch.checked : false;

    // Build payload with only form-managed sections (don't include channels
    // to avoid overwriting concurrent changes from TUI).
    const network = {
      port,
      https_enabled: httpsEnabled,
      tls_cert_path: tlsCertPath,
      tls_key_path: tlsKeyPath,
      trust_forwarded_proto: trustForwardedProto,
      trusted_proxies: trustedProxies,
    };
    // network_access is sent only when the select actually holds a value.
    //
    // It can be empty for exactly one reason: the select is populated from the
    // loaded config (populateConfigForm), and when that config carries a value
    // this dropdown has no <sl-option> for, Shoelace resolves .value to "".
    // The live case is network_access = "public" — a config-file-level alias
    // for "external" used behind an authenticating reverse proxy, deliberately
    // absent from this dropdown so the UI never offers it as a choice.
    //
    // Sending "" would fail validateConfigUpdates and 400 the entire request,
    // making every OTHER setting on this page unsavable for those deployments.
    // Omitting the key instead preserves the stored value: both
    // validateConfigUpdates and applyConfigUpdates skip absent network keys
    // (type-assertion guarded), so "public" survives the save untouched.
    if (networkAccess !== "") {
      network.network_access = networkAccess;
    }

    const payload = {
      network,
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
        archive_window_days: archiveWindowDays,
        archive_slots: archiveSlots,
        feed_check_interval: feedCheckInterval,
        hide_finished_age_days: hideFinishedDays,
        probe_cooldown: probeCooldown,
        decapi_check_interval: decapiCheckInterval ?? null,
        twitch_check_interval: twitchCheckInterval ?? null,
        membership_discovery: membershipDiscovery,
      },
      downloader: {
        output_template: outputTemplate,
        max_video_resolution: maxResolution,
        num_parallel_downloads: parallelDownloads,
        segment_workers: segmentWorkers,
        download_chat: downloadChat,
        prefer_60fps: prefer60fps,
        maximum_timeout: maximumTimeout,
        interruption_timeout: interruptionTimeout,
        incomplete_staging_expiry_days: incompleteStagingExpiry,
      },
      cookies: {
        cookie_file: cookieFile,
        active_platforms: activePlatforms,
        auto_enabled: autoEnabled,
        browser_profile_dir: autoCookiesProfileDir,
        dpapi_fallback: dpapiFallback,
        // refresh_interval / browser_path / browser_type resolved below
      },
      disk: {
        disk_warn_percent: diskWarnPercent,
        disk_critical_percent: diskCriticalPercent,
      },
      updates: {
        auto_check_updates: autoCheckUpdates,
      },
      memory: {
        go_soft_limit_mb: memoryGoSoftMB,
        sidecar_soft_limit_mb: memorySidecarSoftMB,
        sidecar_hard_limit_mb: memorySidecarHardMB,
      },
      bgutils: {
        use_sidecar: useSidecar,
      },
    };

    // Only send refresh_interval when the field has a value — the server
    // turns null into 0, which fails the 10..10080 range validation and
    // rejects the entire save. Omitting the key keeps the stored value.
    if (cookieRefreshInterval !== undefined) {
      payload.cookies.refresh_interval = cookieRefreshInterval;
    }

    // Include notifications — managed entirely via the web UI, so no
    // concurrent modification risk (unlike channels which the TUI can edit).
    if (config.notifications) {
      payload.notifications = config.notifications;
    }

    // Resolve browser selection into payload.cookies.browser_path / browser_type.
    // Must happen before the fetch so validation errors can abort early.
    // Only when the selector has actually been populated (loadAutoCookieStatus
    // runs async, and only with auto-cookies enabled) — otherwise the select
    // reads "" and the save would silently erase a configured browser. Omitting
    // both keys keeps the server's stored values.
    if (this._browserSelectLoaded) {
      if (browserSelectVal === "__custom__") {
        const path = document.getElementById("cfg-cookies-browser-path")?.value || "";
        const type = document.getElementById("cfg-cookies-browser-type")?.value || "firefox";
        const msgEl = document.getElementById("custom-browser-validation-msg");
        // Clear any previous validation message
        if (msgEl) { msgEl.textContent = ""; msgEl.style.color = ""; }
        // Only a verdict of "invalid" aborts the save. A validator that could
        // not be reached, answered non-200 (the heavy limiter's 429 is the
        // realistic one), or replied with something unparseable has told us
        // nothing about the path — and abandoning the save over it discards
        // every other setting edited in the same form. browserPathValidation-
        // Outcome owns that distinction so it can be executed in a test; this
        // site only renders the answer and obeys `block`.
        let outcome;
        try {
          const validateResp = await fetch("/api/auto-cookies/validate-browser-path", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ path, type }),
          });
          outcome = validateResp.ok
            ? browserPathValidationOutcome({ reached: true, body: await validateResp.json() })
            : browserPathValidationOutcome({ reached: false, detail: await serverErrorMessage(validateResp) });
        } catch (e) {
          // Unreachable server, or a 200 whose body was not JSON. Either way
          // the check did not happen — it must not leave the Save button stuck
          // spinning, and it must not stand in for a rejection.
          outcome = browserPathValidationOutcome({ reached: false, detail: e.message });
        }
        if (msgEl) {
          msgEl.textContent = outcome.message;
          msgEl.style.color = `var(--sl-color-${outcome.variant}-600)`;
        }
        if (outcome.block) {
          if (saveBtn) { saveBtn.loading = false; saveBtn.disabled = false; }
          return;
        }
        if (outcome.variant === "warning") {
          // The inline message sits in a section the user may have scrolled
          // past; the save is proceeding, so say so where it will be seen.
          this.app.showToast(outcome.message, "warning");
        }
        payload.cookies.browser_path = path;
        payload.cookies.browser_type = type;
      } else if (browserSelectVal === "") {
        // Auto-detect — clear any explicit browser selection
        payload.cookies.browser_path = "";
        payload.cookies.browser_type = "";
      } else {
        // A detected browser was chosen — the option value is an index
        // (Shoelace mangles spaces in values); the real path and family
        // live in data attributes.
        const opt = browserSelectEl?.querySelector(`sl-option[value="${CSS.escape(browserSelectVal)}"]`);
        payload.cookies.browser_path = opt?.dataset.path || "";
        payload.cookies.browser_type = opt?.dataset.type || "";
      }
    }

    try {
      const response = await fetch("/api/config", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });

      if (response.ok) {
        // Deep-merge payload into local config cache to preserve server-only
        // nested fields (Object.assign would replace entire sub-objects).
        // Drop undefined-valued keys (cleared numeric fields) first —
        // JSON.stringify omitted them from the request, so the server kept
        // its stored values; copying undefined would desync the local cache
        // and trigger spurious "Restart Required" prompts.
        for (const [key, val] of Object.entries(payload)) {
          if (val && typeof val === "object" && !Array.isArray(val) && config[key] && typeof config[key] === "object") {
            const defined = Object.fromEntries(
              Object.entries(val).filter(([, v]) => v !== undefined),
            );
            Object.assign(config[key], defined);
          } else {
            config[key] = val;
          }
        }
        this._dirty = false;
        this._updateUnsavedIndicator();
        document.getElementById("settings-unsaved-banner").style.display = "none";
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

    // Capture old network values for redirect detection
    const oldPort = this._originalRestartValues["network.port"] || 774;
    const oldHttps = !!this._originalRestartValues["network.https_enabled"];

    const shouldRestart = await this.app.showConfirm(
      "Some settings require a restart to take effect (port, network access, database path, log settings).\n\nRestart Moombox now?",
      { okLabel: "Restart", okVariant: "primary", title: "Restart Required" },
    );
    if (!shouldRestart) {
      return;
    }

    // Update snapshot only after user confirms (otherwise declining would
    // suppress the prompt on subsequent saves until page reload)
    this._originalRestartValues = { ...current };

    // Check if network address changed — browser needs redirect after restart
    const newPort = current["network.port"] || 774;
    const newHttps = !!current["network.https_enabled"];
    const portChanged = String(newPort) !== String(oldPort);
    const httpsChanged = newHttps !== oldHttps;

    this.app.showToast("Restarting Moombox...", "primary");

    // Arm the redirect timer synchronously up front so the .then()
    // error branch can always clearTimeout(id); previously it was
    // assigned inside .then() which could race the 3500ms timer.
    let redirectTimeoutId = null;
    if (portChanged || httpsChanged) {
      const proto = newHttps ? "https:" : "http:";
      const host = window.location.hostname;
      const redirectUrl = `${proto}//${host}:${newPort}/`;
      this.app.showToast(`Redirecting to ${redirectUrl} in a few seconds...`, "primary");
      redirectTimeoutId = setTimeout(() => { window.location.href = redirectUrl; }, 3500);
    }

    fetch("/api/restart", { method: "POST" })
      .then(resp => {
        if (!resp.ok) {
          // Restart failed — cancel any pending redirect and notify user
          if (redirectTimeoutId) clearTimeout(redirectTimeoutId);
          this.app.showToast("Failed to restart: " + resp.statusText, "danger");
        }
      })
      .catch(() => {
        // Connection will drop during restart — expected
      });
  }

  /**
   * Manual restart triggered by the "Restart Moombox" button in the System
   * settings section. Confirms, then POSTs /api/restart. Server-side access
   * control (IPGate by network_access + CSRF + session auth for external
   * clients) governs who may call it, so this works for any connection the
   * operator allows — local, LAN, or authenticated external.
   */
  async restartNow() {
    const confirmed = await this.app.showConfirm(
      "Restart Moombox now? In-progress downloads are saved on shutdown and resume automatically once it comes back up.",
      { okLabel: "Restart", okVariant: "danger", title: "Restart Moombox" },
    );
    if (!confirmed) return;

    const btn = document.getElementById("btn-restart-now");
    const result = document.getElementById("restart-result");
    const setResult = (text, color) => {
      if (result) { result.textContent = text; result.style.color = color; }
    };
    const resetButton = () => { if (btn) { btn.loading = false; btn.disabled = false; } };

    if (btn) { btn.loading = true; btn.disabled = true; }
    setResult("Restarting…", "var(--sl-color-neutral-500)");
    this.app.showToast("Restarting Moombox…", "primary");

    try {
      const resp = await fetch("/api/restart", { method: "POST" });
      if (!resp.ok) {
        const data = await resp.json().catch(() => ({ error: resp.statusText }));
        const msg = "Failed to restart: " + (data.error || resp.statusText);
        this.app.showToast(msg, "danger");
        setResult(msg, "var(--sl-color-danger-500)");
        resetButton();
        return;
      }
    } catch {
      // A connection reset mid-request is expected once the server begins
      // shutting down — fall through to the reconnect path below.
    }

    // Success (or the socket dropped as the server began shutting down). The
    // server replies first, then drains and exits ~0.5s later; the WebSocket
    // reconnect logic restores the UI once the new process is up. Reset the
    // button as a fallback in case the page outlives the restart (same
    // host:port — there is no navigation/reload).
    setResult("Reconnecting once Moombox is back…", "var(--sl-color-neutral-500)");
    setTimeout(() => { resetButton(); setResult("", ""); }, 10000);
  }

  _markDirty() {
    this._dirty = true;
    this._updateUnsavedIndicator();
    const banner = document.getElementById("settings-unsaved-banner");
    if (banner) banner.style.display = "";
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
      // Avoid duplicate badges — check this specific element's next sibling,
      // not the parent (multiple restart fields can share the same parent container)
      if (el.nextElementSibling?.classList.contains("restart-badge")) continue;
      const badge = document.createElement("sl-tag");
      badge.size = "small";
      badge.variant = "warning";
      badge.className = "restart-badge";
      badge.textContent = "Restart";
      el.insertAdjacentElement("afterend", badge);
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
        <span class="channel-backfill" data-backfill-for="${this.app.escapeHtml(ch.id)}">${this._backfillBadgeHtml(ch.id)}</span>
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

  /**
   * Badge markup for a channel's feed-history backfill state ("" when
   * inactive). "scanning" shows tab + page progress; "error" shows the
   * sweep-will-retry state. Data comes from app.backfillStatus (WS
   * backfill_status events + the initial_state seed).
   */
  _backfillBadgeHtml(channelId) {
    const bf = this.app.backfillStatus?.[channelId];
    if (!bf) return "";
    if (bf.state === "scanning") {
      return `<sl-badge variant="primary" pill>Backfill: ${this.app.escapeHtml(bf.tab || "")} p${Number(bf.pages) || 0}</sl-badge>`;
    }
    if (bf.state === "error") {
      return `<sl-badge variant="warning" pill>Backfill failed — will retry</sl-badge>`;
    }
    return "";
  }

  /** Patch one channel card's backfill badge in place (no-op when the card isn't rendered). */
  updateChannelBackfillBadge(channelId) {
    const slot = document.querySelector(`#channels-list [data-backfill-for="${CSS.escape(channelId)}"]`);
    if (slot) slot.innerHTML = this._backfillBadgeHtml(channelId);
  }

  /** Refresh every rendered card's backfill badge (initial_state seed / reconnect). */
  refreshBackfillBadges() {
    document.querySelectorAll("#channels-list [data-backfill-for]").forEach((slot) => {
      slot.innerHTML = this._backfillBadgeHtml(slot.dataset.backfillFor);
    });
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

    // When editing, start from the existing channel to preserve fields
    // the UI doesn't expose (num_desc_lookbehind, output_directory,
    // archive_window_days, archive_slots).
    const existingChannel = this.editingChannelId
      ? this.app.config?.channels?.find(c => c.id === this.editingChannelId)
      : null;

    const channel = {
      ...(existingChannel || {}),
      id,
      name: name || undefined,
      enabled,
      ...(isTwitch ? { platform: "twitch" } : {}),
      ...(!isTwitch ? { include_non_live_content: includeVods || undefined } : {}),
    };

    // Preserve terms structure: if existing terms was a named map (e.g. {stream, vod}),
    // only update the "stream" key and keep other keys intact.
    if (termsValue) {
      if (existingChannel?.terms && typeof existingChannel.terms === "object" && !Array.isArray(existingChannel.terms) && existingChannel.terms.stream !== undefined) {
        channel.terms = { ...existingChannel.terms, stream: termsValue };
      } else {
        channel.terms = termsValue;
      }
    } else {
      channel.terms = undefined;
    }

    // Add quality preference (both platforms)
    const qualitySelect = document.getElementById("channel-quality-select");
    const quality = qualitySelect ? qualitySelect.value : "best";
    if (quality !== "best") {
      channel.quality_preference = quality;
    } else {
      delete channel.quality_preference;
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
        this.renderChannelsList(); // Revert toggle to match local config state
      }
    } catch (e) {
      this.app.showToast("Failed to update channel: " + e.message, "danger");
      this.renderChannelsList(); // Revert toggle to match local config state
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
          <sl-icon-button name="send" label="Send test notification" data-notif-action="test" data-notif-index="${idx}"></sl-icon-button>
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
        else if (action === "test") this.testNotification(this.app.config.notifications?.[idx]?.url, el);
        else if (action === "toggle-event") this.toggleNotificationEvent(idx, el.dataset.eventId);
        else if (action === "clear-filter") this.clearNotificationFilter(idx);
        else if (action === "enable-filter") this.enableNotificationFilter(idx);
      });
    }
  }

  /**
   * Send a test embed to a webhook URL via POST /api/notifications/test.
   * Works for saved targets (card button) AND the unsaved dialog value —
   * validation + delivery outcome surfaces as a toast. `busyEl` (optional)
   * gets loading/disabled state while the request runs.
   */
  async testNotification(url, busyEl) {
    url = (url || "").trim();
    if (!url) {
      this.app.showToast("Enter a webhook URL first", "warning");
      return;
    }
    if (busyEl) { busyEl.loading = true; busyEl.disabled = true; }
    try {
      const resp = await fetch("/api/notifications/test", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ url }),
      });
      if (resp.ok) {
        this.app.showToast("Test notification delivered", "success");
      } else {
        const data = await resp.json().catch(() => ({}));
        this.app.showToast(data.error || `Test failed (HTTP ${resp.status})`, "danger");
      }
    } catch {
      this.app.showToast("Test failed: could not reach server", "danger");
    } finally {
      if (busyEl) { busyEl.loading = false; busyEl.disabled = false; }
    }
  }

  addNotification() {
    const dialog = document.getElementById("webhook-dialog");
    const input = document.getElementById("webhook-url-input");
    const confirmBtn = document.getElementById("webhook-confirm-btn");
    const testBtn = document.getElementById("webhook-test-btn");
    input.value = "";

    // Replace to avoid stacking listeners (consistent with showConfirm/removePassword)
    const newConfirm = confirmBtn.cloneNode(true);
    confirmBtn.replaceWith(newConfirm);
    // Test button: exercises the UNSAVED dialog value so a bad paste fails
    // here instead of after saving.
    const newTest = testBtn.cloneNode(true);
    testBtn.replaceWith(newTest);
    newTest.addEventListener("click", () => this.testNotification(input.value, newTest));
    newConfirm.addEventListener("click", async () => {
      const url = input.value.trim();
      if (!url) {
        this.app.showToast("Please enter a URL", "warning");
        return;
      }

      newConfirm.loading = true;
      newConfirm.disabled = true;

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
      } finally {
        newConfirm.loading = false;
        newConfirm.disabled = false;
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

    // If all events are selected or none remain, clear the filter
    // (empty array is ambiguous — treat as "all events" by removing the filter)
    if (notif.events.length === 0 || ALL_EVENT_IDS.every((id) => notif.events.includes(id))) {
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

      // Update button text. When it reads "Reinstall" the install call must
      // pass force — otherwise the server short-circuits "already installed"
      // and never rewrites the file. ("Update" rewrites anyway: the port or
      // scheme mismatch defeats the short-circuit.)
      this._ytdlpReinstall = !!status.installed && !status.portMismatch;
      if (status.installed) {
        let icon = installBtn.querySelector("sl-icon");
        if (!icon) { icon = document.createElement("sl-icon"); icon.slot = "prefix"; }
        icon.name = "arrow-clockwise";
        installBtn.textContent = "";
        installBtn.prepend(icon);
        installBtn.append(status.portMismatch ? "Update" : "Reinstall");
      } else {
        let icon = installBtn.querySelector("sl-icon");
        if (!icon) { icon = document.createElement("sl-icon"); icon.slot = "prefix"; }
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

      // Keep the global banner in sync when the user fixes (or creates)
      // the passwordless-external state from the Security section.
      document
        .getElementById("security-banner")
        ?.classList.toggle("show", !!status.passwordlessExternal);

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

    // Only clear form fields on the initial load (not when re-navigating to Network
    // section), so partially-entered passwords aren't lost. The fields are cleared
    // explicitly after successful setPassword/removePassword via their own loadSecurityStatus calls.
    if (!this._securityStatusLoaded) {
      this._securityStatusLoaded = true;
      if (currentPasswordInput) currentPasswordInput.value = "";
      document.getElementById("security-new-password").value = "";
      document.getElementById("security-confirm-password").value = "";
    }

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
    btn.disabled = true;
    try {
      const response = await fetch("/api/auth/set-password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });

      if (response.ok) {
        this.app.showToast("Password updated successfully", "success");
        // Reset flag so loadSecurityStatus clears the form fields
        this._securityStatusLoaded = false;
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
      btn.disabled = false;
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
      newConfirm.disabled = true;
      try {
        const response = await fetch("/api/auth/remove-password", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ currentPassword }),
        });

        if (response.ok) {
          dialog.hide();
          this.app.showToast("Password removed. Network access set to Localhost Only.", "success");
          // Reset flag so loadSecurityStatus clears the form fields
          this._securityStatusLoaded = false;
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
        newConfirm.disabled = false;
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
        const lastUsed = token.lastUsedAt ? formatRelativeTime(token.lastUsedAt) : "never";
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
    if (!await this.app.showConfirm("Revoke this client token? The client will need to re-authenticate.", { okLabel: "Revoke", okVariant: "danger" })) return;
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
    const selectorDiv = document.getElementById("auto-cookie-browser-selector");
    if (selectorDiv) {
      selectorDiv.style.display = enabled ? "" : "none";
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

  populateBrowserSelector(status) {
    const select = document.getElementById("cfg-cookies-browser-select");
    if (!select) return;

    // Clear existing options
    select.innerHTML = "";

    // Auto-detect option first
    const autoOpt = document.createElement("sl-option");
    autoOpt.value = "";
    const detectedName = status.browser ? status.browser.name : null;
    autoOpt.textContent = detectedName
      ? `Auto-detect (currently: ${detectedName})`
      : "Auto-detect (no browser found)";
    select.appendChild(autoOpt);

    // Detected browsers. Shoelace replaces spaces in option values with
    // underscores, which corrupts real paths (C:\Program Files\...) — use the
    // index as the value and carry the actual path in a data attribute.
    const available = status.availableBrowsers || [];
    available.forEach((b, i) => {
      const opt = document.createElement("sl-option");
      opt.value = String(i);
      opt.dataset.path = b.path;
      opt.dataset.type = b.type;
      opt.textContent = b.name;
      opt.title = b.path;
      select.appendChild(opt);
    });

    // Custom path entry
    const customOpt = document.createElement("sl-option");
    customOpt.value = "__custom__";
    customOpt.textContent = "Custom path…";
    select.appendChild(customOpt);

    // Pre-select based on configuredBrowserPath
    const cfgPath = status.configuredBrowserPath || "";
    const cfgType = status.configuredBrowserType || "";
    const customWrap = document.getElementById("custom-browser-fields");
    const pathInput = document.getElementById("cfg-cookies-browser-path");
    const typeInput = document.getElementById("cfg-cookies-browser-type");

    const detectedPaths = new Set(available.map((b) => b.path));
    if (cfgPath && !detectedPaths.has(cfgPath)) {
      // Custom path configured — show custom fields
      select.value = "__custom__";
      if (customWrap) customWrap.style.display = "block";
      if (pathInput) pathInput.value = cfgPath;
      if (typeInput) {
        const ft = cfgType.toLowerCase();
        const familyValue = ["firefox", "waterfox", "librewolf", "zen"].includes(ft)
          ? "firefox"
          : "chrome";
        typeInput.value = familyValue;
      }
    } else {
      // Map the configured path back to its detected option (values are
      // indexes — see above); no configured path = auto-detect ("").
      const cfgIdx = available.findIndex((b) => b.path === cfgPath);
      select.value = cfgPath && cfgIdx >= 0 ? String(cfgIdx) : "";
      if (customWrap) customWrap.style.display = "none";
    }

    this._browserSelectLoaded = true;

    // Wire change listener once (idempotent)
    if (!select._customListenerAttached) {
      select.addEventListener("sl-change", () => {
        if (customWrap) {
          customWrap.style.display = select.value === "__custom__" ? "block" : "none";
        }
      });
      select._customListenerAttached = true;
    }
  }

  async loadAutoCookieStatus() {
    try {
      const response = await fetch("/api/cookies/auto-status");
      if (response.ok) {
        const status = await response.json();
        this.populateBrowserSelector(status);

        const infoEl = document.getElementById("auto-cookie-browser-info");
        if (infoEl) {
          const available = status.availableBrowsers || [];
          const parts = [];
          if (available.length === 0) {
            parts.push("No supported browser detected — choose Custom path…");
          }
          if (status.lastRefresh) {
            const d = new Date(status.lastRefresh);
            parts.push(`Last refresh: ${d.toLocaleString()}`);
          }
          infoEl.textContent = parts.join(" | ");
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

    // Show loading on the platform-specific setup button
    const btnId = platform === "twitch" ? "btn-auto-cookie-setup-tw" : "btn-auto-cookie-setup-yt";
    const setupBtn = document.getElementById(btnId);
    if (setupBtn) { setupBtn.loading = true; setupBtn.disabled = true; }

    try {
      const response = await fetch("/api/cookies/auto-setup/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ platform }),
      });
      // The server said why. "no supported browser installed" (424) and the
      // shutdown 503 both arrived here as a bare `HTTP <n>` before this — see
      // serverErrorMessage.
      if (!response.ok) throw new Error(await serverErrorMessage(response));
      const data = await response.json();

      if (data.success) {
        // A browser is now open and the server is holding the setup slot. Until
        // this is cleared, an unload cancels it (see the pagehide beacon).
        this._cookieSetupActive = true;
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
          // Clone done/cancel buttons to clear any stale handlers (e.g., from
          // setup wizard's startCookieSetup which clones with its own handlers —
          // without re-cloning here, both setup and settings handlers would fire)
          const doneBtn = document.getElementById("btn-auto-cookie-done");
          if (doneBtn) {
            const newDone = doneBtn.cloneNode(true);
            doneBtn.replaceWith(newDone);
            newDone.addEventListener("click", () => this.finishAutoCookieSetup());
          }
          const cancelBtn = document.getElementById("btn-auto-cookie-cancel");
          if (cancelBtn) {
            const newCancel = cancelBtn.cloneNode(true);
            cancelBtn.replaceWith(newCancel);
            newCancel.addEventListener("click", () => this.cancelAutoCookieSetup());
          }
          dialog.show();
        }
      } else {
        this.app.showToast(data.error || "Failed to start setup", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to start auto-cookie setup: " + e.message, "danger");
    } finally {
      if (setupBtn) { setupBtn.loading = false; setupBtn.disabled = false; }
    }
  }

  async finishAutoCookieSetup() {
    const resultEl = document.getElementById("auto-cookie-setup-result");
    const doneBtn = document.getElementById("btn-auto-cookie-done");
    const countdownEl = document.getElementById("auto-cookie-countdown");
    const timeoutResultEl = document.getElementById("auto-cookie-result");
    // Which platform this dialog belongs to, from the label
    // startAutoCookieSetup set. Hoisted out of the abort path, which used to be
    // the only branch that needed it: the rejected-outcome copy below now
    // speaks for the platform the user just signed in to.
    const dialogLabel = document.getElementById("auto-cookie-setup-dialog")?.label || "";
    const platform = dialogLabel.includes("Twitch") ? "twitch" : "youtube";
    if (doneBtn) { doneBtn.loading = true; doneBtn.disabled = true; }
    if (resultEl) {
      resultEl.textContent = "Extracting cookies...";
      resultEl.style.color = "";
    }
    if (timeoutResultEl) timeoutResultEl.innerHTML = "";

    let remaining = 60;
    if (countdownEl) {
      countdownEl.textContent = `${remaining}s remaining`;
      countdownEl.style.color = "";
    }
    const countdownInterval = setInterval(() => {
      remaining--;
      if (countdownEl) {
        countdownEl.textContent = `${remaining}s remaining`;
        if (remaining <= 10) {
          countdownEl.style.color = "var(--sl-color-warning-600)";
        }
      }
      if (remaining <= 0) clearInterval(countdownInterval);
    }, 1000);

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 60000); // 60s timeout

    // Baseline for the abort path, dispatched BEFORE the finish so the
    // lastRefresh comparison there is server clock against server clock and no
    // browser/server skew can turn an old stamp into a fresh one.
    //
    // Deliberately not awaited: it must not add a round trip to the finish, and
    // only the abort branch ever reads it. cookieSetupProbe never rejects, so
    // an unread promise cannot surface as an unhandled rejection.
    const abortBaseline = cookieSetupProbe();

    try {
      const response = await fetch("/api/cookies/auto-setup/finish", { method: "POST", signal: controller.signal });
      // Set before the ok check: any ANSWER means FinishSetup reached a
      // conclusion and released the slot, success or not. Only the abort path
      // below skips this line, and it must — an abort says nothing about
      // whether the setup is still live, which is exactly when the Skip button
      // and the unload beacon still have something to cancel. That path clears
      // the flag itself, but only once the server has confirmed the slot is
      // free.
      this._cookieSetupActive = false;
      // The finish handler's discriminated errors — 422 for an unreadable
      // cookies.txt or a broken profile, 409 for a locked cookie DB — used to
      // reach the user as `HTTP <n>`, which points at none of them.
      if (!response.ok) throw new Error(await serverErrorMessage(response));
      const data = await response.json();

      const ytOk = data.authenticated;
      const twOk = data.twitchAuthenticated;

      if (ytOk || twOk) {
        if (resultEl) resultEl.textContent = "";
        const dialog = document.getElementById("auto-cookie-setup-dialog");
        if (dialog) dialog.hide();
        // Accepted is not verified — see cookieSetupAcceptedToast. A sign-in
        // Moombox could not reach the site to confirm was reported here as an
        // unqualified success, so a user whose network blipped mid-check was
        // told their cookies were configured with nothing having checked them.
        if (ytOk) {
          const toast = cookieSetupAcceptedToast("YouTube", data.youtubeVerification);
          this.app.showToast(toast.message, toast.variant);
        }
        if (twOk) {
          const toast = cookieSetupAcceptedToast("Twitch", data.twitchVerification);
          this.app.showToast(toast.message, toast.variant);
        }
        this.app.loadStatus();
      } else {
        if (resultEl) {
          // Speaks from the verification fields, not from `data.error`: that
          // never exists on a 200, so the fallback was the only thing this
          // branch ever rendered.
          resultEl.textContent = cookieSetupRejectedMessage(
            platform === "twitch" ? data.twitchVerification : data.youtubeVerification,
          );
          resultEl.style.color = "var(--sl-color-danger-600)";
        }
      }
    } catch (e) {
      if (e.name === "AbortError") {
        if (resultEl) resultEl.textContent = "";
        // Report what actually happened rather than asserting a timeout. The
        // server writes the merged cookies.txt and reloads the jar BEFORE it
        // verifies, so this abort can fire over work that already committed.
        // The 60 s cap stays as it is — the server-side setup grace window is
        // priced against that exact number.
        const probe = await cookieSetupProbe();
        // Only a definite "no setup here" releases the flag. An unanswered
        // probe keeps it raised, which is the safe direction: the Skip button
        // and the unload beacon then still have something to tell the server
        // about.
        if (probe.ok && !probe.inProgress) this._cookieSetupActive = false;
        if (timeoutResultEl) {
          const report = cookieSetupAbortReport(probe, await abortBaseline);
          timeoutResultEl.innerHTML = `
            <sl-alert variant="${report.variant}" open>
              <sl-icon slot="icon" name="${report.icon}"></sl-icon>
              ${this.app.escapeHtml(report.message)}
            </sl-alert>
            <div style="display: flex; gap: 0.5em; margin-top: 0.75em;">
              <sl-button variant="primary" size="small" id="cookie-retry-btn">Try Again</sl-button>
              <sl-button variant="default" size="small" id="cookie-skip-btn">Skip</sl-button>
            </div>`;
          document.getElementById("cookie-retry-btn")?.addEventListener("click", () => {
            timeoutResultEl.innerHTML = "";
            if (countdownEl) countdownEl.textContent = "";
            this.startAutoCookieSetup(platform);
          });
          // Skip goes through the cancel path. It used to hide() the dialog and
          // nothing else, which does not fire sl-request-close, so the server
          // was never told — the setup it is still holding then blocked every
          // later setup and refresh. cancelAutoCookieSetup hides the dialog and
          // clears these same two elements itself.
          document.getElementById("cookie-skip-btn")?.addEventListener("click", () => {
            this.cancelAutoCookieSetup();
          });
        }
        // The finish may well have committed while we stopped waiting, so the
        // badges and the panel's own status are both potentially stale.
        this.app.loadStatus();
        this.loadAutoCookieStatus();
        return;
      }
      if (resultEl) {
        resultEl.textContent = "Error: " + e.message;
        resultEl.style.color = "var(--sl-color-danger-600)";
      }
    } finally {
      clearInterval(countdownInterval);
      clearTimeout(timeoutId);
      if (countdownEl) countdownEl.textContent = "";
      if (doneBtn) { doneBtn.loading = false; doneBtn.disabled = false; }
    }
  }

  async cancelAutoCookieSetup() {
    this._cookieSetupActive = false;
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
    const countdownEl = document.getElementById("auto-cookie-countdown");
    if (countdownEl) countdownEl.textContent = "";
    const timeoutResultEl = document.getElementById("auto-cookie-result");
    if (timeoutResultEl) timeoutResultEl.innerHTML = "";
  }
}
