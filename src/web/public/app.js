/**
 * Moombox Dashboard Application
 */
import { SetupController } from "./modules/setup.js";
import { ImportController } from "./modules/imports.js";
import { PlayerController } from "./modules/player.js";
import { SettingsController } from "./modules/settings.js";

class MoomboxApp {
  constructor() {
    this.ws = null;
    this.jobs = [];
    this.archivedJobs = [];
    this.logs = [];
    this.config = null;
    this.selectedJobId = null;
    this.reconnectAttempts = 0;
    this.maxReconnectAttempts = 10;
    this.reconnectDelay = 1000;

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
      this.setupEventListeners();
      this.settings.setupListeners();
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
          if (!this.player.playerInitialized) {
            this.player.initPlayer();
          }
          this.player.loadPlayerJobList();
        } else if (e.detail.name === "imports") {
          if (!this.imports.importInitialized) {
            this.imports.initImports();
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

  async loadConfig() {
    try {
      const response = await fetch("/api/config");
      if (response.ok) {
        this.config = await response.json();
        this.settings.populateConfigForm();
        this.settings.renderChannelsList();
        this.settings.renderNotificationsList();
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

  // ===== WebSocket Management =====

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

  // ===== Job Rendering =====

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

      ${job.selectedVideoItag != null || job.selectedAudioItag != null || job.startTime != null || job.endTime != null ? `
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
            ${this.formatDurationSeconds(job.startTime || 0)} - ${job.endTime != null ? this.formatDurationSeconds(job.endTime) : "end"}
            ${job.endTime != null && job.startTime != null ? ` (${this.formatDurationSeconds(job.endTime - job.startTime)})` : ""}
          </span>
        </div>
        ` : ""}
      </div>
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
    const value = input.value.trim();

    if (!value) return;

    const videoId = this.extractVideoId(value);
    if (!videoId) {
      this.showToast("Invalid YouTube URL or video ID", "warning");
      return;
    }

    // Build request body with optional advanced options
    const body = { videoId };
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

    try {
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
    document.getElementById("format-loading-spinner").style.display = "none";

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

    const spinner = document.getElementById("format-loading-spinner");
    const formatSection = document.getElementById("format-selection");
    spinner.style.display = "block";
    formatSection.style.display = "none";

    try {
      const response = await fetch(`/api/formats/${videoId}`);
      if (!response.ok) {
        throw new Error("Failed to fetch formats");
      }

      const data = await response.json();
      this.populateFormatSelects(data);

      spinner.style.display = "none";
      formatSection.style.display = "block";
    } catch (e) {
      spinner.style.display = "none";
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

  // ===== Utilities =====

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

  formatFileSize(bytes) {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    if (bytes < 1024 * 1024 * 1024)
      return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
    return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
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
