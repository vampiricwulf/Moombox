/**
 * Setup Wizard Controller — Mode selection, simplified + advanced flows, FFmpeg check
 */
export class SetupController {
  constructor(app) {
    this.app = app;
    this.channels = []; // Client-side channel accumulator
    this.mode = null; // "quick" or "advanced"
    this.advStep = 1;
    this.cookieYTDone = false;
    this.cookieTWDone = false;
    this._redirectUrl = null; // Set when port/HTTPS changes require redirect after restart
  }

  /** Escape HTML entities for safe innerHTML insertion. */
  esc(s) {
    return this.app.escapeHtml(s);
  }

  show() {
    document.getElementById("setup-overlay").style.display = "flex";
    this.showPage("setup-mode-select");
    this._redirectUrl = null;
    this.setupListeners();
    this.checkFFmpegStatus();
  }

  /** Check FFmpeg availability and display on mode selection screen. */
  async checkFFmpegStatus() {
    const el = document.getElementById("setup-ffmpeg-status");
    if (!el) return;
    try {
      const resp = await fetch("/api/setup/status");
      if (!resp.ok) return;
      const data = await resp.json();
      if (data.ffmpegValid) {
        el.innerHTML = `<sl-icon name="check-circle" style="color: var(--sl-color-success-600);"></sl-icon> FFmpeg: ${this.esc(data.ffmpegVersion || "found")}`;
      } else {
        el.innerHTML = `<sl-icon name="exclamation-triangle" style="color: var(--sl-color-warning-600);"></sl-icon> FFmpeg not found — you'll be prompted to install it after setup`;
      }
    } catch { /* ignore */ }
  }

  hide() {
    document.getElementById("setup-overlay").style.display = "none";
  }

  showPage(id) {
    document.querySelectorAll("#setup-overlay .setup-page").forEach((p) => {
      p.style.display = "none";
    });
    // Hide advanced step indicators when navigating away from advanced steps
    const advSteps = document.getElementById("setup-adv-steps");
    if (advSteps) advSteps.style.display = "none";
    const el = document.getElementById(id);
    if (el) el.style.display = "";
  }

  setupListeners() {
    if (this._listenersAttached) return;
    this._listenersAttached = true;

    // Prevent Escape/overlay-click from closing the cookie dialog without
    // properly cancelling the setup (which would orphan the browser process).
    // Only handle during setup wizard — SettingsController registers its own
    // handler for the same dialog, so skip when setup is complete to avoid
    // double cancel API calls.
    const autoCookieDialog = document.getElementById("auto-cookie-setup-dialog");
    if (autoCookieDialog) {
      autoCookieDialog.addEventListener("sl-request-close", (e) => {
        if (document.getElementById("setup-overlay").style.display === "none") return;
        e.preventDefault();
        this.cancelCookieSetup();
      });
    }

    // Mode selection
    document.getElementById("setup-mode-quick")?.addEventListener("click", () => {
      this.mode = "quick";
      this.channels = [];
      this.showPage("setup-simple-cookies");
    });
    document.getElementById("setup-mode-advanced")?.addEventListener("click", () => {
      this.mode = "advanced";
      this.channels = [];
      this.advStep = 1;
      this.showAdvancedStep(1);
    });
    document.getElementById("setup-mode-defaults")?.addEventListener("click", () => {
      this.finishWithDefaults();
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

    // --- Advanced flow cookie buttons ---
    document.getElementById("setup-adv-cookie-yt")?.addEventListener("click", () => {
      this.startCookieSetup("youtube");
    });
    document.getElementById("setup-adv-cookie-tw")?.addEventListener("click", () => {
      this.startCookieSetup("twitch");
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
        this.updateChannelDialogFields();
      });
    }

    // Hide YouTube-only fields when Twitch is selected
    const setupChPlatform = document.getElementById("setup-ch-platform");
    if (setupChPlatform) {
      setupChPlatform.addEventListener("sl-change", () => this.updateChannelDialogFields());
    }

    // FFmpeg overlay listeners (also called from showFFmpegOverlay for non-first-run)
    this.setupFFmpegListeners();
  }

  /** Wire up FFmpeg overlay button listeners. Idempotent — safe to call multiple times. */
  setupFFmpegListeners() {
    if (this._ffmpegListenersAdded) return;
    this._ffmpegListenersAdded = true;

    document.getElementById("ffmpeg-install-btn")?.addEventListener("click", () => {
      this.showFFmpegInstallOptions();
    });
    document.getElementById("ffmpeg-check-btn")?.addEventListener("click", () => {
      this.checkFFmpegPath("ffmpeg-custom-path", "ffmpeg-check-result", "ffmpeg-check-btn");
    });
    document.getElementById("ffmpeg-skip-btn")?.addEventListener("click", () => {
      document.getElementById("ffmpeg-overlay").style.display = "none";
      this.initializeApp();
    });
    document.getElementById("ffmpeg-quit-btn")?.addEventListener("click", () => {
      window.close();
      // window.close() only works if the page was opened by script.
      // Show fallback message after a short delay if still open.
      setTimeout(() => {
        const quitBtn = document.getElementById("ffmpeg-quit-btn");
        if (quitBtn) {
          quitBtn.disabled = true;
          quitBtn.textContent = "You can close this tab manually";
        }
      }, 300);
    });
    // Enter key in custom path input
    document.getElementById("ffmpeg-custom-path")?.addEventListener("keydown", (e) => {
      if (e.key === "Enter") this.checkFFmpegPath("ffmpeg-custom-path", "ffmpeg-check-result", "ffmpeg-check-btn");
    });

    // Manual install view
    document.getElementById("ffmpeg-manual-check-btn")?.addEventListener("click", () => {
      this.checkFFmpegPath("ffmpeg-manual-path", "ffmpeg-manual-result", "ffmpeg-manual-check-btn");
    });
    document.getElementById("ffmpeg-manual-path")?.addEventListener("keydown", (e) => {
      if (e.key === "Enter") this.checkFFmpegPath("ffmpeg-manual-path", "ffmpeg-manual-result", "ffmpeg-manual-check-btn");
    });
    document.getElementById("ffmpeg-manual-back-btn")?.addEventListener("click", () => {
      document.getElementById("ffmpeg-manual-install").style.display = "none";
      document.getElementById("ffmpeg-main-view").style.display = "";
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
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
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
    const countdownEl = document.getElementById("auto-cookie-countdown");
    const timeoutResultEl = document.getElementById("auto-cookie-result");
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

    try {
      const response = await fetch("/api/cookies/auto-setup/finish", { method: "POST", signal: controller.signal });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const data = await response.json();
      const ytOk = data.authenticated;
      const twOk = data.twitchAuthenticated;

      if (ytOk || twOk) {
        if (resultEl) resultEl.textContent = "";
        document.getElementById("auto-cookie-setup-dialog")?.hide();
        // Track completion in state and update badges (both simple and advanced)
        if (ytOk) {
          this.cookieYTDone = true;
          for (const id of ["setup-yt-badge", "setup-adv-yt-badge"]) {
            const badge = document.getElementById(id);
            if (badge) { badge.style.display = ""; badge.variant = "success"; }
          }
        }
        if (twOk) {
          this.cookieTWDone = true;
          for (const id of ["setup-tw-badge", "setup-adv-tw-badge"]) {
            const badge = document.getElementById(id);
            if (badge) { badge.style.display = ""; badge.variant = "success"; }
          }
        }
        if (ytOk) {
          this.app.showToast("YouTube cookies configured", "success");
        }
        if (twOk) {
          this.app.showToast("Twitch cookies configured", "success");
        }
      } else {
        if (resultEl) {
          resultEl.textContent = data.error || "No login detected. Try again.";
          resultEl.style.color = "var(--sl-color-danger-600)";
        }
      }
    } catch (e) {
      if (e.name === "AbortError") {
        if (resultEl) resultEl.textContent = "";
        if (timeoutResultEl) {
          timeoutResultEl.innerHTML = `
            <sl-alert variant="warning" open>
              <sl-icon slot="icon" name="clock"></sl-icon>
              Cookie extraction timed out. The browser window may still be open.
            </sl-alert>
            <div style="display: flex; gap: 0.5em; margin-top: 0.75em;">
              <sl-button variant="primary" size="small" id="cookie-retry-btn">Try Again</sl-button>
              <sl-button variant="default" size="small" id="cookie-skip-btn">Skip</sl-button>
            </div>`;
          document.getElementById("cookie-retry-btn")?.addEventListener("click", () => {
            timeoutResultEl.innerHTML = "";
            if (countdownEl) countdownEl.textContent = "";
            this.startCookieSetup(platform);
          });
          document.getElementById("cookie-skip-btn")?.addEventListener("click", () => {
            document.getElementById("auto-cookie-setup-dialog")?.hide();
            if (countdownEl) countdownEl.textContent = "";
            timeoutResultEl.innerHTML = "";
          });
        }
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

  async cancelCookieSetup() {
    try {
      await fetch("/api/cookies/auto-setup/cancel", { method: "POST" });
    } catch { /* ignore */ }
    document.getElementById("auto-cookie-setup-dialog")?.hide();
    const resultEl = document.getElementById("auto-cookie-setup-result");
    if (resultEl) { resultEl.textContent = ""; resultEl.style.color = ""; }
    const countdownEl = document.getElementById("auto-cookie-countdown");
    if (countdownEl) countdownEl.textContent = "";
    const timeoutResultEl = document.getElementById("auto-cookie-result");
    if (timeoutResultEl) timeoutResultEl.innerHTML = "";
  }

  /** Show/hide YouTube-only fields in the channel dialog based on platform. */
  updateChannelDialogFields() {
    const platform = document.getElementById("setup-ch-platform")?.value || "youtube";
    const nonLive = document.getElementById("setup-ch-include-non-live");
    // include_non_live is YouTube-only
    if (nonLive) nonLive.style.display = platform === "youtube" ? "" : "none";
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
      const displayName = this.esc(ch.name || ch.id);
      const displayId = this.esc(ch.id);
      item.innerHTML = `
        <div style="display: flex; align-items: center; gap: 0.5em; flex: 1; min-width: 0;">
          <sl-icon name="${platformIcon}" style="color: ${platformColor};"></sl-icon>
          <strong style="overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">${displayName}</strong>
          <span style="color: var(--sl-color-neutral-500); font-size: var(--sl-font-size-small); overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">${displayId}</span>
        </div>
        <div style="display: flex; gap: 0.25em;">
          <sl-icon-button name="pencil" data-index="${i}" class="setup-ch-edit" label="Edit"></sl-icon-button>
          <sl-icon-button name="x-lg" data-index="${i}" class="setup-ch-remove" label="Remove"></sl-icon-button>
        </div>
      `;
      container.appendChild(item);
    }
    // Event delegation — attach once per container to avoid listener leaks on re-render
    if (!container._setupChDelegated) {
      container._setupChDelegated = true;
      container.addEventListener("click", (e) => {
        const editBtn = e.target.closest(".setup-ch-edit");
        if (editBtn) {
          const idx = parseInt(editBtn.dataset.index);
          this.openEditChannelDialog(container.id, idx);
          return;
        }
        const removeBtn = e.target.closest(".setup-ch-remove");
        if (removeBtn) {
          const idx = parseInt(removeBtn.dataset.index);
          this.channels.splice(idx, 1);
          this.renderChannelList(container.id);
        }
      });
    }
  }

  openAddChannelDialog(targetListId) {
    this._channelTargetList = targetListId;
    this._editingIndex = -1; // -1 = adding new
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
    const qualitySel = document.getElementById("setup-ch-quality");
    if (qualitySel) qualitySel.value = "best";
    const enabledCb = document.getElementById("setup-ch-enabled");
    if (enabledCb) enabledCb.checked = true;
    const saveBtn = document.getElementById("setup-ch-save");
    if (saveBtn) { saveBtn.textContent = "Add Channel"; saveBtn.loading = false; }
    this.updateChannelDialogFields();
    if (dialog) dialog.show();
  }

  openEditChannelDialog(targetListId, index) {
    this._channelTargetList = targetListId;
    this._editingIndex = index;
    const ch = this.channels[index];
    if (!ch) return;
    const dialog = document.getElementById("setup-add-channel-dialog");
    const idEl = document.getElementById("setup-ch-id");
    const nameEl = document.getElementById("setup-ch-name");
    const termsEl = document.getElementById("setup-ch-terms");
    const platformSel = document.getElementById("setup-ch-platform");
    const nlCb = document.getElementById("setup-ch-include-non-live");
    if (idEl) idEl.value = ch.id || "";
    if (nameEl) nameEl.value = ch.name || "";
    if (termsEl) termsEl.value = typeof ch.terms === "object" && ch.terms !== null ? (ch.terms.stream || "") : (ch.terms || "");
    if (platformSel) platformSel.value = ch.platform || "youtube";
    if (nlCb) nlCb.checked = !!ch.include_non_live_content;
    const qualitySel = document.getElementById("setup-ch-quality");
    if (qualitySel) qualitySel.value = ch.quality_preference || "best";
    const enabledCb = document.getElementById("setup-ch-enabled");
    if (enabledCb) enabledCb.checked = ch.enabled !== false;
    const saveBtn = document.getElementById("setup-ch-save");
    if (saveBtn) saveBtn.textContent = "Save Channel";
    this.updateChannelDialogFields();
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
      if (saveBtn) { saveBtn.loading = true; saveBtn.disabled = true; }
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

    // Check for duplicate channel ID (skip self when editing)
    const editIdx = this._editingIndex ?? -1;
    if (this.channels.some((c, i) => c.id.toLowerCase() === id.toLowerCase() && i !== editIdx)) {
      this.app.showToast(`Channel "${id}" already added`, "warning");
      return;
    }

    const quality = document.getElementById("setup-ch-quality")?.value || "best";
    const enabled = document.getElementById("setup-ch-enabled")?.checked ?? true;
    const ch = {
      id,
      name: name || undefined,
      platform,
      terms: (document.getElementById("setup-ch-terms")?.value || "").trim() || undefined,
      include_non_live_content: platform === "youtube" ? (document.getElementById("setup-ch-include-non-live")?.checked || undefined) : undefined,
      quality_preference: quality !== "best" ? quality : undefined,
      enabled: enabled ? undefined : false, // only set when disabled (default is enabled)
    };
    if (editIdx >= 0 && editIdx < this.channels.length) {
      this.channels[editIdx] = ch; // Edit existing
    } else {
      this.channels.push(ch); // Add new
    }
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

    // Show and update step indicators
    const advSteps = document.getElementById("setup-adv-steps");
    if (advSteps) advSteps.style.display = "";
    document.querySelectorAll("#setup-adv-steps .setup-step").forEach((s) => {
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

  async finishWithDefaults() {
    const btn = document.getElementById("setup-mode-defaults");
    if (btn) { btn.loading = true; btn.disabled = true; }
    await this.submitSetup({}, btn);
  }

  async finishSimpleSetup() {
    const finishBtn = document.getElementById("setup-simple-finish");
    if (finishBtn) { finishBtn.loading = true; finishBtn.disabled = true; }

    const config = {};
    if (this.channels.length > 0) {
      config.channels = this.channels;
    }
    if (this.cookieYTDone || this.cookieTWDone) {
      const active_platforms = [];
      if (this.cookieYTDone) active_platforms.push("youtube");
      if (this.cookieTWDone) active_platforms.push("twitch");
      config.cookies = { auto_enabled: true, active_platforms };
    }

    await this.submitSetup(config, finishBtn);
  }

  async finishAdvancedSetup() {
    const finishBtn = document.getElementById("setup-adv-finish");
    if (finishBtn) { finishBtn.loading = true; finishBtn.disabled = true; }

    const val = (id) => (document.getElementById(id)?.value || "").trim();
    // Use Number() instead of parseInt() to preserve decimals for FlexDuration
    // fields (e.g. feed_check_interval accepts 2.5 minutes). Integer fields
    // are truncated server-side by Go's int() conversion.
    const num = (id) => { const s = val(id); if (s === "") return undefined; const n = Number(s); return isNaN(n) ? undefined : n; };

    const port = num("setup-port");
    const networkAccess = document.getElementById("setup-network-access")?.value || "localhost";
    const externalPassword = val("setup-external-password");

    // Validate password for external access — navigate to Network step where the field lives
    if (networkAccess === "external" && (!externalPassword || externalPassword.length < 8)) {
      this.app.showToast("Password (min 8 characters) is required for external access", "warning");
      if (finishBtn) { finishBtn.loading = false; finishBtn.disabled = false; }
      this.showAdvancedStep(1);
      document.getElementById("setup-external-password")?.focus();
      return;
    }

    const httpsEnabled = document.getElementById("setup-https-enabled")?.checked || false;

    const config = {
      network: { port, network_access: networkAccess, https_enabled: httpsEnabled },
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
        ...(this.cookieYTDone || this.cookieTWDone ? {
          auto_enabled: true,
          active_platforms: [
            ...(this.cookieYTDone ? ["youtube"] : []),
            ...(this.cookieTWDone ? ["twitch"] : []),
          ],
        } : {}),
      },
    };

    if (this.channels.length > 0) {
      config.channels = this.channels;
    }

    // Include yt-dlp plugin flag — server handles install before restart
    if (document.getElementById("setup-install-ytdlp-plugin")?.checked) {
      config.install_ytdlp_plugin = true;
    }

    // Detect if dashboard URL will change (port or HTTPS toggle).
    // After restart the old URL is dead, so we redirect instead of polling.
    const newPort = port || 774;
    const currentPort = parseInt(location.port) || (location.protocol === "https:" ? 443 : 80);
    const currentHttps = location.protocol === "https:";
    if (newPort !== currentPort || httpsEnabled !== currentHttps) {
      const protocol = httpsEnabled ? "https" : "http";
      this._redirectUrl = `${protocol}://${location.hostname}:${newPort}`;
    } else {
      this._redirectUrl = null;
    }

    await this.submitSetup(config, finishBtn);
  }

  async submitSetup(config, finishBtn) {
    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 30000); // 30s timeout

    try {
      const response = await fetch("/api/setup/complete", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(config),
        signal: controller.signal,
      });

      if (response.ok) {
        this.hide();
        this.app.showToast("Setup complete! Restarting...", "success");
        // Server triggers restart — poll until it comes back
        this.pollForRestart();
        return true;
      } else {
        const data = await response.json().catch(() => ({ error: response.statusText }));
        // Show specific field validation errors if available
        let msg = data.error || "Failed to save configuration";
        if (data.details && typeof data.details === "object") {
          const fieldErrors = Object.values(data.details);
          if (fieldErrors.length > 0) msg = fieldErrors.join("; ");
        }
        this.app.showToast(msg, "danger");
      }
    } catch (e) {
      const msg = e.name === "AbortError"
        ? "Setup save timed out — please try again"
        : "Failed to save configuration: " + e.message;
      this.app.showToast(msg, "danger");
    } finally {
      clearTimeout(timeoutId);
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

    // If port or HTTPS changed, the old URL is dead after restart.
    // Cross-origin restrictions prevent polling the new URL, so redirect directly.
    if (this._redirectUrl) {
      const waiting = document.createElement("div");
      waiting.id = "restart-waiting";
      waiting.style.cssText = "position: fixed; inset: 0; display: flex; align-items: center; justify-content: center; background: var(--sl-color-neutral-0); z-index: 99999;";
      const inner = document.createElement("div");
      inner.style.textAlign = "center";
      const icon = document.createElement("sl-icon");
      icon.name = "arrow-right-circle";
      icon.style.cssText = "font-size: 2rem; color: var(--sl-color-primary-600);";
      const heading = document.createElement("p");
      heading.style.cssText = "margin-top: 0.75em; font-weight: 600;";
      heading.textContent = "Dashboard address has changed";
      const urlText = document.createElement("p");
      urlText.style.cssText = "margin-top: 0.25em; font-family: monospace; color: var(--sl-color-neutral-600); word-break: break-all;";
      urlText.textContent = this._redirectUrl;
      const note = document.createElement("p");
      note.style.cssText = "margin-top: 0.5em; font-size: var(--sl-font-size-small); color: var(--sl-color-neutral-500);";
      note.textContent = "Redirecting in a few seconds...";
      const btn = document.createElement("sl-button");
      btn.variant = "primary";
      btn.style.marginTop = "1em";
      btn.textContent = "Open New Dashboard";
      const redirectUrl = this._redirectUrl;
      btn.addEventListener("click", () => { window.location.href = redirectUrl; });
      inner.append(icon, heading, urlText, note, btn);
      waiting.appendChild(inner);
      document.body.appendChild(waiting);

      // Auto-redirect after a brief delay to let the server restart
      setTimeout(() => { window.location.href = redirectUrl; }, 3000);
      this._polling = false;
      this._redirectUrl = null;
      return;
    }

    // Show a temporary message using safe DOM methods
    const waiting = document.createElement("div");
    waiting.id = "restart-waiting";
    waiting.style.cssText = "position: fixed; inset: 0; display: flex; align-items: center; justify-content: center; background: var(--sl-color-neutral-0); z-index: 99999;";
    const inner = document.createElement("div");
    inner.style.textAlign = "center";
    const spinner = document.createElement("sl-spinner");
    spinner.style.fontSize = "2rem";
    const msg = document.createElement("p");
    msg.style.marginTop = "1em";
    msg.textContent = "Restarting Moombox...";
    inner.appendChild(spinner);
    inner.appendChild(msg);
    waiting.appendChild(inner);
    document.body.appendChild(waiting);

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
            this._polling = false;
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
        this._polling = false;
        const waitingEl = document.getElementById("restart-waiting");
        if (waitingEl) {
          waitingEl.textContent = "";
          const failInner = document.createElement("div");
          failInner.style.textAlign = "center";
          const failMsg = document.createElement("p");
          failMsg.textContent = "Server did not come back within 2 minutes.";
          const reloadBtn = document.createElement("sl-button");
          reloadBtn.variant = "primary";
          reloadBtn.textContent = "Reload Page";
          reloadBtn.addEventListener("click", () => location.reload());
          failInner.appendChild(failMsg);
          failInner.appendChild(reloadBtn);
          waitingEl.appendChild(failInner);
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
    // Ensure FFmpeg button listeners are wired up (may not have been if
    // setupListeners wasn't called — e.g. non-first-run with missing FFmpeg)
    this.setupFFmpegListeners();

    document.getElementById("ffmpeg-overlay").style.display = "flex";
    document.getElementById("ffmpeg-main-view").style.display = "";
    document.getElementById("ffmpeg-install-view").style.display = "none";
    document.getElementById("ffmpeg-script-review").style.display = "none";
    document.getElementById("ffmpeg-manual-install").style.display = "none";
    // Reset stale state from previous overlay opens
    for (const id of ["ffmpeg-check-result", "ffmpeg-install-result",
                       "ffmpeg-confirm-result", "ffmpeg-manual-result"]) {
      const el = document.getElementById(id);
      if (el) el.innerHTML = "";
    }
    // Reset quit button (may have been disabled by close-tab fallback)
    const quitBtn = document.getElementById("ffmpeg-quit-btn");
    if (quitBtn) {
      quitBtn.disabled = false;
      quitBtn.textContent = "Quit Moombox";
    }
  }

  async showFFmpegInstallOptions() {
    document.getElementById("ffmpeg-main-view").style.display = "none";
    const installView = document.getElementById("ffmpeg-install-view");
    installView.style.display = "";

    const optionsEl = document.getElementById("ffmpeg-install-options");
    optionsEl.innerHTML = '<sl-spinner></sl-spinner> Checking available installers...';

    try {
      const resp = await fetch("/api/ffmpeg/install-options");
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
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
      optionsEl.innerHTML = `<sl-alert variant="danger" open>Failed to check install options: ${this.esc(e.message)}</sl-alert>`;
      const backBtn = document.createElement("sl-button");
      backBtn.variant = "text";
      backBtn.style.cssText = "width: 100%; margin-top: 0.5em;";
      backBtn.innerHTML = '<sl-icon slot="prefix" name="arrow-left"></sl-icon> Back';
      backBtn.addEventListener("click", () => {
        document.getElementById("ffmpeg-install-view").style.display = "none";
        document.getElementById("ffmpeg-main-view").style.display = "";
      });
      optionsEl.appendChild(backBtn);
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

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 360000); // 6 minutes

    try {
      const resp = await fetch("/api/ffmpeg/install", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ method }),
        signal: controller.signal,
      });
      if (!resp.ok) {
        const errData = await resp.json().catch(() => ({}));
        throw new Error(errData.error || `HTTP ${resp.status}`);
      }
      const data = await resp.json();

      if (data.needsElevation) {
        // Show script review view
        if (progress) progress.style.display = "none";
        document.getElementById("ffmpeg-install-view").style.display = "none";
        this.showScriptReview(data.script, data.token);
        return;
      }

      if (data.success) {
        this.showInstallSuccess(resultEl, data);
      } else {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>${this.esc(data.error || "Install failed")}</sl-alert>`;
        }
        optionsEl.querySelectorAll("sl-button").forEach((b) => { b.disabled = false; });
      }
    } catch (e) {
      if (e.name === "AbortError") {
        if (resultEl) {
          resultEl.innerHTML = '<sl-alert variant="warning" open>Install timed out \u2014 try running \'ffmpeg -version\' in a terminal to check if it succeeded.</sl-alert>';
        }
      } else {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>Install failed: ${this.esc(e.message)}</sl-alert>`;
        }
      }
      optionsEl.querySelectorAll("sl-button").forEach((b) => { b.disabled = false; });
    } finally {
      clearTimeout(timeoutId);
      if (progress) progress.style.display = "none";
    }
  }

  showInstallSuccess(resultEl, data) {
    let html = `<sl-alert variant="success" open>FFmpeg installed: ${this.esc(data.version)}</sl-alert>`;
    if (data.warning) {
      html += `<sl-alert variant="warning" open style="margin-top: 0.5em;">${this.esc(data.warning)}</sl-alert>`;
      html += `<sl-button variant="primary" style="width: 100%; margin-top: 0.75em;" id="ffmpeg-success-continue">Continue</sl-button>`;
    }
    if (resultEl) {
      resultEl.innerHTML = html;
    }
    if (data.warning) {
      document.getElementById("ffmpeg-success-continue")?.addEventListener("click", () => {
        document.getElementById("ffmpeg-overlay").style.display = "none";
        this.initializeApp();
      });
    } else {
      setTimeout(() => {
        document.getElementById("ffmpeg-overlay").style.display = "none";
        this.initializeApp();
      }, 1500);
    }
  }

  showScriptReview(script, token) {
    const reviewEl = document.getElementById("ffmpeg-script-review");
    const codeEl = document.getElementById("ffmpeg-review-script");
    if (codeEl) codeEl.textContent = script;
    if (reviewEl) reviewEl.style.display = "";

    // Wire trust button
    const trustBtn = document.getElementById("ffmpeg-trust-btn");
    const distrustBtn = document.getElementById("ffmpeg-distrust-btn");

    const newTrust = trustBtn.cloneNode(true);
    trustBtn.replaceWith(newTrust);
    newTrust.addEventListener("click", () => this.confirmElevatedInstall(token));

    const newDistrust = distrustBtn.cloneNode(true);
    distrustBtn.replaceWith(newDistrust);
    newDistrust.addEventListener("click", () => this.rejectElevatedInstall(token));

    // Wire cancel button — rejects token and goes back to install options (not manual)
    const cancelBtn = document.getElementById("ffmpeg-review-cancel-btn");
    if (cancelBtn) {
      const newCancel = cancelBtn.cloneNode(true);
      cancelBtn.replaceWith(newCancel);
      newCancel.addEventListener("click", async () => {
        try {
          await fetch("/api/ffmpeg/install/reject", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ token }),
          });
        } catch { /* ignore */ }
        document.getElementById("ffmpeg-script-review").style.display = "none";
        document.getElementById("ffmpeg-install-view").style.display = "";
        // Re-enable install buttons (disabled by installFFmpeg before entering review)
        document.getElementById("ffmpeg-install-options")
          ?.querySelectorAll("sl-button")
          .forEach((b) => { b.disabled = false; });
      });
    }
  }

  async confirmElevatedInstall(token) {
    const progress = document.getElementById("ffmpeg-confirm-progress");
    const resultEl = document.getElementById("ffmpeg-confirm-result");
    const trustBtn = document.getElementById("ffmpeg-trust-btn");
    const distrustBtn = document.getElementById("ffmpeg-distrust-btn");

    if (trustBtn) trustBtn.disabled = true;
    if (distrustBtn) distrustBtn.disabled = true;
    if (progress) progress.style.display = "flex";
    if (resultEl) resultEl.innerHTML = "";

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 360000); // 6 minutes

    try {
      const resp = await fetch("/api/ffmpeg/install/confirm", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token }),
        signal: controller.signal,
      });
      const data = await resp.json();

      if (data.success) {
        this.showInstallSuccess(resultEl, data);
      } else {
        // HTTP error response — token was consumed by backend, retry won't work (F10).
        // Show error with Back button instead of re-enabling Trust.
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>${this.esc(data.error || "Install failed")}</sl-alert>`;
        }
        this.appendBackToInstallButton(resultEl);
      }
    } catch (e) {
      // Both timeout and network errors leave token state unknown — re-enable buttons
      if (trustBtn) trustBtn.disabled = false;
      if (distrustBtn) distrustBtn.disabled = false;
      if (e.name === "AbortError") {
        if (resultEl) {
          resultEl.innerHTML = '<sl-alert variant="warning" open>Install timed out \u2014 try running \'ffmpeg -version\' in a terminal to check if it succeeded.</sl-alert>';
        }
      } else {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>Install failed: ${this.esc(e.message)}</sl-alert>`;
        }
      }
    } finally {
      clearTimeout(timeoutId);
      if (progress) progress.style.display = "none";
    }
  }

  /** Append a "Back to install options" button to the given result container. */
  appendBackToInstallButton(resultEl) {
    if (!resultEl) return;
    const backBtn = document.createElement("sl-button");
    backBtn.variant = "text";
    backBtn.style.cssText = "width: 100%; margin-top: 0.5em;";
    backBtn.innerHTML = '<sl-icon slot="prefix" name="arrow-left"></sl-icon> Back to install options';
    backBtn.addEventListener("click", () => {
      document.getElementById("ffmpeg-script-review").style.display = "none";
      document.getElementById("ffmpeg-install-view").style.display = "";
      // Re-enable install buttons (disabled by installFFmpeg before entering review)
      document.getElementById("ffmpeg-install-options")
        ?.querySelectorAll("sl-button")
        .forEach((b) => { b.disabled = false; });
    });
    resultEl.appendChild(backBtn);
  }

  async rejectElevatedInstall(token) {
    try {
      await fetch("/api/ffmpeg/install/reject", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token }),
      });
    } catch { /* ignore */ }

    // Show manual install view
    document.getElementById("ffmpeg-script-review").style.display = "none";
    document.getElementById("ffmpeg-manual-install").style.display = "";
  }

  async checkFFmpegPath(inputId, resultId, btnId) {
    const input = document.getElementById(inputId);
    const resultEl = document.getElementById(resultId);
    const btn = btnId ? document.getElementById(btnId) : null;
    const path = (input?.value || "").trim();
    if (!path) return;

    // Disable controls to prevent concurrent checks (F3)
    if (btn) btn.disabled = true;
    if (input) input.disabled = true;
    if (resultEl) resultEl.innerHTML = '<sl-spinner style="font-size: 1rem;"></sl-spinner> Checking...';

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 15000); // 15s (backend has 10s timeout)

    try {
      const resp = await fetch("/api/ffmpeg/check", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path }),
        signal: controller.signal,
      });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const data = await resp.json();

      if (data.valid) {
        // POST /api/ffmpeg/check already saves the path to config on the server side

        let html = `<sl-alert variant="success" open>Valid: ${this.esc(data.version)}</sl-alert>`;
        if (data.warning) {
          html += `<sl-alert variant="warning" open style="margin-top: 0.5em;">${this.esc(data.warning)}</sl-alert>`;
          html += `<sl-button variant="primary" style="width: 100%; margin-top: 0.75em;" id="ffmpeg-path-continue">Continue</sl-button>`;
        }
        if (resultEl) resultEl.innerHTML = html;
        if (data.warning) {
          document.getElementById("ffmpeg-path-continue")?.addEventListener("click", () => {
            document.getElementById("ffmpeg-overlay").style.display = "none";
            this.initializeApp();
          });
        } else {
          setTimeout(() => {
            document.getElementById("ffmpeg-overlay").style.display = "none";
            this.initializeApp();
          }, 1500);
        }
      } else {
        if (resultEl) {
          resultEl.innerHTML = '<sl-alert variant="danger" open>FFmpeg not found at this path</sl-alert>';
        }
      }
    } catch (e) {
      if (e.name === "AbortError") {
        if (resultEl) {
          resultEl.innerHTML = '<sl-alert variant="warning" open>Check timed out \u2014 try running \'ffmpeg -version\' in a terminal to verify.</sl-alert>';
        }
      } else {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>Check failed: ${this.esc(e.message)}</sl-alert>`;
        }
      }
    } finally {
      clearTimeout(timeoutId);
      if (btn) btn.disabled = false;
      if (input) input.disabled = false;
    }
  }

}
