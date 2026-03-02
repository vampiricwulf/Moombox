/**
 * Setup Wizard Controller — Mode selection, simplified + advanced flows, FFmpeg check
 */
export class SetupController {
  constructor(app) {
    this.app = app;
    this.channels = []; // Client-side channel accumulator
    this.mode = null; // "quick" or "advanced"
    this.advStep = 1;
  }

  /** Escape HTML entities for safe innerHTML insertion. */
  esc(s) {
    return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }

  show() {
    document.getElementById("setup-overlay").style.display = "flex";
    this.showPage("setup-mode-select");
    this.setupListeners();
  }

  hide() {
    document.getElementById("setup-overlay").style.display = "none";
  }

  showPage(id) {
    document.querySelectorAll("#setup-overlay .setup-page").forEach((p) => {
      p.style.display = "none";
    });
    const el = document.getElementById(id);
    if (el) el.style.display = "";
  }

  setupListeners() {
    if (this._listenersAttached) return;
    this._listenersAttached = true;

    // Mode selection
    document.getElementById("setup-mode-quick")?.addEventListener("click", () => {
      this.mode = "quick";
      this.showPage("setup-simple-cookies");
    });
    document.getElementById("setup-mode-advanced")?.addEventListener("click", () => {
      this.mode = "advanced";
      this.advStep = 1;
      this.showAdvancedStep(1);
    });

    // --- Simplified flow ---
    document.getElementById("setup-cookie-yt")?.addEventListener("click", () => {
      this.startCookieSetup("youtube");
    });
    document.getElementById("setup-cookie-tw")?.addEventListener("click", () => {
      this.startCookieSetup("twitch");
    });
    document.getElementById("setup-simple-cookies-back")?.addEventListener("click", () => {
      this.showPage("setup-mode-select");
    });
    document.getElementById("setup-simple-cookies-next")?.addEventListener("click", () => {
      this.showPage("setup-simple-channels");
      this.renderChannelList("setup-channel-list");
    });
    document.getElementById("setup-simple-channels-back")?.addEventListener("click", () => {
      this.showPage("setup-simple-cookies");
    });
    document.getElementById("setup-add-channel-btn")?.addEventListener("click", () => {
      this.openAddChannelDialog("setup-channel-list");
    });
    document.getElementById("setup-simple-finish")?.addEventListener("click", () => {
      this.finishSimpleSetup();
    });

    // --- Advanced flow ---
    for (let i = 1; i <= 8; i++) {
      document.getElementById(`setup-adv-back-${i}`)?.addEventListener("click", () => {
        if (i === 1) {
          this.showPage("setup-mode-select");
        } else {
          this.showAdvancedStep(i - 1);
        }
      });
      if (i < 8) {
        document.getElementById(`setup-adv-next-${i}`)?.addEventListener("click", () => {
          this.showAdvancedStep(i + 1);
        });
      }
    }
    document.getElementById("setup-adv-add-channel-btn")?.addEventListener("click", () => {
      this.openAddChannelDialog("setup-adv-channel-list");
    });
    document.getElementById("setup-adv-finish")?.addEventListener("click", () => {
      this.finishAdvancedSetup();
    });

    // Network access warning
    const setupNetAccess = document.getElementById("setup-network-access");
    if (setupNetAccess) {
      setupNetAccess.addEventListener("sl-change", () => {
        const isExternal = setupNetAccess.value === "external";
        const w = document.getElementById("setup-external-warning");
        const p = document.getElementById("setup-external-password");
        if (w) w.style.display = isExternal ? "" : "none";
        if (p) p.style.display = isExternal ? "" : "none";
      });
    }

    // Channel dialog
    document.getElementById("setup-ch-cancel")?.addEventListener("click", () => {
      document.getElementById("setup-add-channel-dialog")?.hide();
    });
    document.getElementById("setup-ch-save")?.addEventListener("click", () => {
      this.saveChannelFromDialog();
    });

    // Auto-detect platform from channel ID input in setup dialog
    const setupChId = document.getElementById("setup-ch-id");
    if (setupChId) {
      setupChId.addEventListener("sl-input", () => {
        const val = (setupChId.value || "").trim();
        const platformSel = document.getElementById("setup-ch-platform");
        if (!platformSel) return;
        if (val.includes("youtube.com") || val.includes("youtu.be")) {
          if (platformSel.value !== "youtube") platformSel.value = "youtube";
        } else if (val.includes("twitch.tv")) {
          if (platformSel.value !== "twitch") platformSel.value = "twitch";
        }
      });
    }

    // --- FFmpeg overlay ---
    document.getElementById("ffmpeg-install-btn")?.addEventListener("click", () => {
      this.showFFmpegInstallOptions();
    });
    document.getElementById("ffmpeg-check-btn")?.addEventListener("click", () => {
      this.checkCustomFFmpegPath();
    });
    document.getElementById("ffmpeg-quit-btn")?.addEventListener("click", () => {
      window.close();
    });
    // Enter key in custom path input
    document.getElementById("ffmpeg-custom-path")?.addEventListener("keypress", (e) => {
      if (e.key === "Enter") this.checkCustomFFmpegPath();
    });
  }

  // --- Cookie Setup ---

  async startCookieSetup(platform) {
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
          instructions.textContent = platform === "twitch"
            ? "A browser window has opened to Twitch. Please sign in, then click \"I'm Logged In\"."
            : "A browser window has opened to Google. Please sign in to YouTube, then click \"I'm Logged In\".";
        }
        if (dialog) {
          dialog.label = platform === "twitch" ? "Setup: Twitch" : "Setup: YouTube";
          // Override done button to use our handler
          const doneBtn = document.getElementById("btn-auto-cookie-done");
          const cancelBtn = document.getElementById("btn-auto-cookie-cancel");
          if (doneBtn) {
            const newDone = doneBtn.cloneNode(true);
            doneBtn.replaceWith(newDone);
            newDone.addEventListener("click", () => this.finishCookieSetup(platform));
          }
          if (cancelBtn) {
            const newCancel = cancelBtn.cloneNode(true);
            cancelBtn.replaceWith(newCancel);
            newCancel.addEventListener("click", () => this.cancelCookieSetup());
          }
          dialog.show();
        }
      } else {
        this.app.showToast(data.error || "Failed to start setup", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to start cookie setup: " + e.message, "danger");
    }
  }

  async finishCookieSetup(platform) {
    const doneBtn = document.getElementById("btn-auto-cookie-done");
    const resultEl = document.getElementById("auto-cookie-setup-result");
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
        document.getElementById("auto-cookie-setup-dialog")?.hide();
        // Update badges
        if (ytOk) {
          const badge = document.getElementById("setup-yt-badge");
          if (badge) { badge.style.display = ""; badge.variant = "success"; }
        }
        if (twOk) {
          const badge = document.getElementById("setup-tw-badge");
          if (badge) { badge.style.display = ""; badge.variant = "success"; }
        }
        this.app.showToast("Cookie setup complete!", "success");
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

  async cancelCookieSetup() {
    try {
      await fetch("/api/cookies/auto-setup/cancel", { method: "POST" });
    } catch { /* ignore */ }
    document.getElementById("auto-cookie-setup-dialog")?.hide();
    const resultEl = document.getElementById("auto-cookie-setup-result");
    if (resultEl) { resultEl.textContent = ""; resultEl.style.color = ""; }
  }

  // --- Channel Management ---

  renderChannelList(containerId) {
    const container = document.getElementById(containerId);
    if (!container) return;
    container.innerHTML = "";
    if (this.channels.length === 0) {
      container.innerHTML = '<p style="color: var(--sl-color-neutral-500); font-size: var(--sl-font-size-small);">No channels added yet.</p>';
      return;
    }
    for (let i = 0; i < this.channels.length; i++) {
      const ch = this.channels[i];
      const item = document.createElement("div");
      item.className = "setup-channel-item";
      const platformIcon = ch.platform === "twitch" ? "twitch" : "youtube";
      const platformColor = ch.platform === "twitch" ? "#9146ff" : "#ff0000";
      const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
      const displayName = esc(ch.name || ch.id);
      const displayId = esc(ch.id);
      item.innerHTML = `
        <div style="display: flex; align-items: center; gap: 0.5em; flex: 1; min-width: 0;">
          <sl-icon name="${platformIcon}" style="color: ${platformColor};"></sl-icon>
          <strong style="overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">${displayName}</strong>
          <span style="color: var(--sl-color-neutral-500); font-size: var(--sl-font-size-small);">${displayId}</span>
        </div>
        <sl-icon-button name="x-lg" data-index="${i}" class="setup-ch-remove" label="Remove"></sl-icon-button>
      `;
      container.appendChild(item);
    }
    container.querySelectorAll(".setup-ch-remove").forEach((btn) => {
      btn.addEventListener("click", () => {
        const idx = parseInt(btn.dataset.index);
        this.channels.splice(idx, 1);
        this.renderChannelList(containerId);
      });
    });
  }

  openAddChannelDialog(targetListId) {
    this._channelTargetList = targetListId;
    const dialog = document.getElementById("setup-add-channel-dialog");
    // Reset fields
    ["setup-ch-id", "setup-ch-name", "setup-ch-terms"].forEach((id) => {
      const el = document.getElementById(id);
      if (el) el.value = "";
    });
    const platformSel = document.getElementById("setup-ch-platform");
    if (platformSel) platformSel.value = "youtube";
    const nlCb = document.getElementById("setup-ch-include-non-live");
    if (nlCb) nlCb.checked = false;
    if (dialog) dialog.show();
  }

  async saveChannelFromDialog() {
    let id = (document.getElementById("setup-ch-id")?.value || "").trim();
    if (!id) {
      this.app.showToast("Channel ID is required", "warning");
      return;
    }
    let name = (document.getElementById("setup-ch-name")?.value || "").trim();
    let platform = document.getElementById("setup-ch-platform")?.value || "youtube";

    // Resolve channel URL if it looks like a URL
    if (id.includes("youtube.com") || id.includes("youtu.be") || id.includes("twitch.tv")) {
      const saveBtn = document.getElementById("setup-ch-save");
      if (saveBtn) saveBtn.loading = true;
      try {
        const resp = await fetch("/api/resolve-channel", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ input: id }),
        });
        if (resp.ok) {
          const resolved = await resp.json();
          if (resolved.id) id = resolved.id;
          if (resolved.name && !name) name = resolved.name;
          if (resolved.platform) platform = resolved.platform;
        } else {
          const data = await resp.json();
          this.app.showToast(data.error || "Failed to resolve channel URL", "danger");
          return;
        }
      } catch (e) {
        this.app.showToast("Failed to resolve channel URL: " + e.message, "danger");
        return;
      } finally {
        if (saveBtn) saveBtn.loading = false;
      }
    }

    const ch = {
      id,
      name: name || undefined,
      platform,
      terms: (document.getElementById("setup-ch-terms")?.value || "").trim() || undefined,
      include_non_live_content: document.getElementById("setup-ch-include-non-live")?.checked || undefined,
    };
    this.channels.push(ch);
    document.getElementById("setup-add-channel-dialog")?.hide();
    this.renderChannelList(this._channelTargetList || "setup-channel-list");
  }

  // --- Advanced Step Navigation ---

  showAdvancedStep(step) {
    this.advStep = step;
    // Hide all advanced + simple pages
    document.querySelectorAll("#setup-overlay .setup-page").forEach((p) => {
      p.style.display = "none";
    });
    const el = document.getElementById(`setup-adv-step-${step}`);
    if (el) el.style.display = "";

    // Update step indicators
    document.querySelectorAll("#setup-adv-steps .setup-step, #setup-adv-step-1 .setup-steps .setup-step").forEach((s) => {
      const sn = parseInt(s.dataset.step);
      s.classList.remove("active", "completed");
      if (sn === step) s.classList.add("active");
      else if (sn < step) s.classList.add("completed");
    });

    // Render channel list when entering channels step
    if (step === 7) {
      this.renderChannelList("setup-adv-channel-list");
    }
  }

  // --- Finish Setup ---

  async finishSimpleSetup() {
    const finishBtn = document.getElementById("setup-simple-finish");
    if (finishBtn) { finishBtn.loading = true; finishBtn.disabled = true; }

    const config = {};
    if (this.channels.length > 0) {
      config.channels = this.channels;
    }
    // Auto-cookies: check which platform badges are showing (cookies were set up)
    const ytBadge = document.getElementById("setup-yt-badge");
    const twBadge = document.getElementById("setup-tw-badge");
    const ytDone = ytBadge && ytBadge.style.display !== "none";
    const twDone = twBadge && twBadge.style.display !== "none";
    if (ytDone || twDone) {
      const platforms = [];
      if (ytDone) platforms.push("youtube");
      if (twDone) platforms.push("twitch");
      config.cookies = { auto_enabled: true, platforms };
    }

    await this.submitSetup(config, finishBtn);
  }

  async finishAdvancedSetup() {
    const finishBtn = document.getElementById("setup-adv-finish");
    if (finishBtn) { finishBtn.loading = true; finishBtn.disabled = true; }

    const val = (id) => (document.getElementById(id)?.value || "").trim();
    const num = (id) => { const s = val(id); return s !== "" ? parseInt(s, 10) : undefined; };

    const port = num("setup-port");
    const networkAccess = document.getElementById("setup-network-access")?.value || "localhost";
    const externalPassword = val("setup-external-password");

    // Validate password for external access
    if (networkAccess === "external" && (!externalPassword || externalPassword.length < 8)) {
      this.app.showToast("Password (min 8 characters) is required for external access", "warning");
      if (finishBtn) { finishBtn.loading = false; finishBtn.disabled = false; }
      return;
    }

    const config = {
      network: { port, network_access: networkAccess },
      ...(networkAccess === "external" && externalPassword ? { password: externalPassword } : {}),
      paths: {
        database_path: val("setup-database-path") || undefined,
        log_file_path: val("setup-log-file") || undefined,
        output_directory: val("setup-output-dir") || undefined,
        staging_directory: val("setup-staging-dir") || undefined,
        ffmpeg_path: val("setup-ffmpeg-path") || undefined,
      },
      logs: {
        log_level: document.getElementById("setup-log-level")?.value || undefined,
        log_max_file_size: num("setup-log-max-size"),
        log_max_files: num("setup-log-max-files"),
      },
      monitors: {
        max_feed_items: num("setup-max-feed-items"),
        feed_check_interval: num("setup-feed-check-interval"),
        hide_finished_age_days: num("setup-hide-age"),
      },
      downloader: {
        output_template: val("setup-output-template") || undefined,
        num_parallel_downloads: num("setup-parallel-downloads"),
        max_video_resolution: num("setup-max-resolution"),
        prefer_60fps: document.getElementById("setup-prefer-60fps")?.checked ?? true,
        download_chat: document.getElementById("setup-download-chat")?.checked ?? true,
        segment_retry_delay_cap: num("setup-retry-delay-cap"),
        segment_live_check_retries: num("setup-live-check-retries"),
      },
      cookies: {
        cookie_file: val("setup-cookie-file") || undefined,
        auto_enabled: document.getElementById("setup-auto-cookies")?.checked || false,
      },
    };

    if (this.channels.length > 0) {
      config.channels = this.channels;
    }

    // Install yt-dlp plugin if checked
    const installPlugin = document.getElementById("setup-install-ytdlp-plugin")?.checked;

    const ok = await this.submitSetup(config, finishBtn);

    if (ok && installPlugin) {
      try {
        await fetch("/api/ytdlp-plugin/install", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ force: true }),
        });
      } catch { /* ignore */ }
    }
  }

  async submitSetup(config, finishBtn) {
    try {
      const response = await fetch("/api/setup/complete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(config),
      });

      if (response.ok) {
        this.hide();
        this.app.showToast("Setup complete! Restarting...", "success");
        // Server triggers restart — poll until it comes back
        this.pollForRestart();
        return true;
      } else {
        const data = await response.json();
        this.app.showToast(data.error || "Failed to save configuration", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to save configuration: " + e.message, "danger");
    } finally {
      if (finishBtn) { finishBtn.loading = false; finishBtn.disabled = false; }
    }
    return false;
  }

  // --- Restart Polling ---

  pollForRestart() {
    if (this._polling) return; // Prevent duplicate polling chains
    this._polling = true;

    const overlay = document.getElementById("setup-overlay");
    if (overlay) overlay.style.display = "none";

    // Show a temporary message
    document.body.insertAdjacentHTML("beforeend",
      '<div id="restart-waiting" style="position: fixed; inset: 0; display: flex; align-items: center; justify-content: center; background: var(--sl-color-neutral-0); z-index: 99999;">' +
      '<div style="text-align: center;"><sl-spinner style="font-size: 2rem;"></sl-spinner><p style="margin-top: 1em;">Restarting Moombox...</p></div></div>');

    let attempts = 0;
    const maxAttempts = 60; // 2 minutes at 2s intervals

    const poll = async () => {
      attempts++;
      try {
        const resp = await fetch("/api/setup/status");
        if (resp.ok) {
          const data = await resp.json();
          if (!data.isFirstRun) {
            // Server is back and config exists
            document.getElementById("restart-waiting")?.remove();

            // Check FFmpeg
            if (data.ffmpegValid === false) {
              this.showFFmpegOverlay();
            } else {
              this.initializeApp();
            }
            return;
          }
        }
      } catch { /* server not ready yet */ }

      if (attempts >= maxAttempts) {
        const waiting = document.getElementById("restart-waiting");
        if (waiting) {
          waiting.innerHTML = '<div style="text-align: center;"><p>Server did not come back within 2 minutes.</p><sl-button variant="primary" onclick="location.reload()">Reload Page</sl-button></div>';
        }
        return;
      }
      setTimeout(poll, 2000);
    };
    setTimeout(poll, 2000);
  }

  initializeApp() {
    this.app.initializeApp();
  }

  // --- FFmpeg Overlay ---

  showFFmpegOverlay() {
    document.getElementById("ffmpeg-overlay").style.display = "flex";
    document.getElementById("ffmpeg-main-view").style.display = "";
    document.getElementById("ffmpeg-install-view").style.display = "none";
  }

  async showFFmpegInstallOptions() {
    document.getElementById("ffmpeg-main-view").style.display = "none";
    const installView = document.getElementById("ffmpeg-install-view");
    installView.style.display = "";

    const optionsEl = document.getElementById("ffmpeg-install-options");
    optionsEl.innerHTML = '<sl-spinner></sl-spinner> Checking available installers...';

    try {
      const resp = await fetch("/api/ffmpeg/install-options");
      const data = await resp.json();

      optionsEl.innerHTML = "";

      if (data.chocoAvailable) {
        this.addInstallButton(optionsEl, "Install via Chocolatey", "choco");
      } else {
        this.addInstallButton(optionsEl, "Install Chocolatey + FFmpeg", "choco-install");
      }
      if (data.wingetAvailable) {
        this.addInstallButton(optionsEl, "Install via Winget", "winget");
      }

      const cancelBtn = document.createElement("sl-button");
      cancelBtn.variant = "text";
      cancelBtn.textContent = "Cancel";
      cancelBtn.style.width = "100%";
      cancelBtn.addEventListener("click", () => {
        document.getElementById("ffmpeg-main-view").style.display = "";
        installView.style.display = "none";
      });
      optionsEl.appendChild(cancelBtn);
    } catch (e) {
      optionsEl.innerHTML = `<p style="color: var(--sl-color-danger-600);">Failed to check install options: ${this.esc(e.message)}</p>`;
    }
  }

  addInstallButton(container, label, method) {
    const btn = document.createElement("sl-button");
    btn.variant = "primary";
    btn.textContent = label;
    btn.style.cssText = "width: 100%; margin-bottom: 0.5em;";
    btn.addEventListener("click", () => this.installFFmpeg(method, btn));
    container.appendChild(btn);
  }

  async installFFmpeg(method, btn) {
    const progress = document.getElementById("ffmpeg-install-progress");
    const resultEl = document.getElementById("ffmpeg-install-result");
    const optionsEl = document.getElementById("ffmpeg-install-options");

    // Disable all buttons
    optionsEl.querySelectorAll("sl-button").forEach((b) => { b.disabled = true; });
    if (progress) progress.style.display = "flex";
    if (resultEl) resultEl.innerHTML = "";

    try {
      const resp = await fetch("/api/ffmpeg/install", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ method }),
      });
      const data = await resp.json();

      if (data.success) {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="success" open>FFmpeg installed: ${this.esc(data.version)}</sl-alert>`;
        }
        // Hide FFmpeg overlay and init app
        setTimeout(() => {
          document.getElementById("ffmpeg-overlay").style.display = "none";
          this.initializeApp();
        }, 1500);
      } else {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>${this.esc(data.error || "Install failed")}</sl-alert>`;
        }
        optionsEl.querySelectorAll("sl-button").forEach((b) => { b.disabled = false; });
      }
    } catch (e) {
      if (resultEl) {
        resultEl.innerHTML = `<sl-alert variant="danger" open>Install failed: ${this.esc(e.message)}</sl-alert>`;
      }
      optionsEl.querySelectorAll("sl-button").forEach((b) => { b.disabled = false; });
    } finally {
      if (progress) progress.style.display = "none";
    }
  }

  async checkCustomFFmpegPath() {
    const input = document.getElementById("ffmpeg-custom-path");
    const resultEl = document.getElementById("ffmpeg-check-result");
    const path = (input?.value || "").trim();
    if (!path) return;

    try {
      const resp = await fetch("/api/ffmpeg/check", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path }),
      });
      const data = await resp.json();

      if (data.valid) {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="success" open>Valid: ${this.esc(data.version)}</sl-alert>`;
        }
        setTimeout(() => {
          document.getElementById("ffmpeg-overlay").style.display = "none";
          this.initializeApp();
        }, 1500);
      } else {
        if (resultEl) {
          resultEl.innerHTML = '<sl-alert variant="danger" open>FFmpeg not found at this path</sl-alert>';
        }
      }
    } catch (e) {
      if (resultEl) {
        resultEl.innerHTML = `<sl-alert variant="danger" open>Check failed: ${this.esc(e.message)}</sl-alert>`;
      }
    }
  }

}
