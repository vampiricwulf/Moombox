/**
 * Setup Wizard Controller
 */
export class SetupController {
  constructor(app) {
    this.app = app;
  }

  show() {
    document.getElementById("setup-overlay").style.display = "flex";
    this.setupListeners();
  }

  hide() {
    document.getElementById("setup-overlay").style.display = "none";
    // Now initialize the main app
    this.app.setupEventListeners();
    this.app.settings.setupListeners();
    this.app.connectWebSocket();
    this.app.loadConfig();
    this.app.loadStatus();
  }

  setupListeners() {
    // Step navigation: next buttons
    document.getElementById("setup-next-1").addEventListener("click", () => this.goToStep(2));
    document.getElementById("setup-next-2").addEventListener("click", () => this.goToStep(3));
    document.getElementById("setup-next-3").addEventListener("click", () => this.goToStep(4));
    document.getElementById("setup-next-4").addEventListener("click", () => this.goToStep(5));

    // Step navigation: back buttons
    document.getElementById("setup-back-2").addEventListener("click", () => this.goToStep(1));
    document.getElementById("setup-back-3").addEventListener("click", () => this.goToStep(2));
    document.getElementById("setup-back-4").addEventListener("click", () => this.goToStep(3));
    document.getElementById("setup-back-5").addEventListener("click", () => this.goToStep(4));

    // Setup: Network access select — show warning + password when External is chosen
    const setupNetAccess = document.getElementById("setup-network-access");
    const setupExtWarning = document.getElementById("setup-external-warning");
    const setupExtPassword = document.getElementById("setup-external-password");
    if (setupNetAccess) {
      setupNetAccess.addEventListener("sl-change", () => {
        const isExternal = setupNetAccess.value === "external";
        if (setupExtWarning) setupExtWarning.style.display = isExternal ? "" : "none";
        if (setupExtPassword) setupExtPassword.style.display = isExternal ? "" : "none";
      });
    }

    // Finish setup
    document.getElementById("setup-finish").addEventListener("click", () => this.finishSetup());
  }

  goToStep(step) {
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

    this.app.currentSetupStep = step;
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
    const retryDelayCap = num("setup-retry-delay-cap");
    const liveCheckRetries = num("setup-live-check-retries");

    // Step 3: Logging
    const logLevelSelect = document.getElementById("setup-log-level");
    const logLevel = logLevelSelect ? logLevelSelect.value : undefined;
    const logFilePath = val("setup-log-file") || undefined;
    const logMaxSize = num("setup-log-max-size");
    const logMaxFiles = num("setup-log-max-files");

    // Step 4: Display & Monitoring
    const hideAge = num("setup-hide-age");
    const maxFeedItems = num("setup-max-feed-items");
    const feedCheckInterval = num("setup-feed-check-interval");
    const port = num("setup-port");
    const setupNetworkAccess = document.getElementById("setup-network-access")?.value || "localhost";
    const externalPassword = val("setup-external-password");

    // Step 5: Channel
    const channelId = val("setup-channel-id");
    const channelName = val("setup-channel-name");
    const channelTerms = val("setup-channel-terms");
    const includeNonLive = document.getElementById("setup-include-non-live")?.checked || false;

    // Validate password for external access
    if (setupNetworkAccess === "external") {
      if (!externalPassword || externalPassword.length < 8) {
        this.app.showToast("Password (min 8 characters) is required for external access", "warning");
        finishBtn.loading = false;
        finishBtn.disabled = false;
        return;
      }
    }

    // Build config
    const config = {
      port: port,
      network_access: setupNetworkAccess,
      // Include password for server to hash (only for external access)
      ...(setupNetworkAccess === "external" && externalPassword ? { password: externalPassword } : {}),
      log_level: logLevel,
      log_file_path: logFilePath,
      log_max_file_size: logMaxSize,
      log_max_files: logMaxFiles,
      database_path: databasePath,
      max_feed_items: maxFeedItems,
      feed_check_interval: feedCheckInterval,
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
        segment_retry_delay_cap: retryDelayCap,
        segment_live_check_retries: liveCheckRetries,
      },
      hide_finished_age_days: hideAge,
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

        this.hide();
        this.app.showToast("Setup complete! Welcome to Moombox.", "success");

        // Trigger auto-cookie setup if enabled in wizard
        if (autoCookiesEnabled) {
          this.app.settings.startAutoCookieSetup();
        }
      } else {
        const data = await response.json();
        this.app.showToast(data.error || "Failed to save configuration", "danger");
      }
    } catch (e) {
      this.app.showToast("Failed to save configuration: " + e.message, "danger");
    } finally {
      finishBtn.loading = false;
      finishBtn.disabled = false;
    }
  }
}
