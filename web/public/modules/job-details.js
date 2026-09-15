/**
 * Job Details Controller — the details dialog: render, live updates, action
 * buttons and per-job logs
 */
import { canResumeJob, streamUrl, CANCEL_STATUSES, REINIT_STATUSES, MUX_STATUSES, DELETE_STATUSES } from "./utils.js";

export class JobDetailsController {
  constructor(app) {
    this.app = app;
  }

  /** Wire up the details dialog: action buttons, dismissal cleanup, delegated clicks. */
  bind() {
    // Details dialog buttons
    document
      .getElementById("details-open-url-btn")
      .addEventListener("click", () => this.app.openJobUrl());
    document
      .getElementById("details-open-folder-btn")
      .addEventListener("click", () => this.app.openJobFolder());
    document
      .getElementById("details-play-btn")
      .addEventListener("click", () => this.app.openInPlayer());
    document
      .getElementById("details-trim-btn")
      .addEventListener("click", () => {
        const job = this.app.jobs.find(j => j.id === this.app.selectedJobId)
          || this.app.archivedJobs.find(j => j.id === this.app.selectedJobId);
        if (job) this.app.openTrimDialog(job);
      });
    document
      .getElementById("details-cancel-btn")
      .addEventListener("click", () => this.app.cancelJob());
    document
      .getElementById("details-resume-btn")
      .addEventListener("click", () => this.app.resumeJob());
    document
      .getElementById("details-reinit-btn")
      .addEventListener("click", () => this.app.reinitializeJob());
    document
      .getElementById("details-mux-btn")
      .addEventListener("click", () => this.app.muxJob());
    document
      .getElementById("details-delete-btn")
      .addEventListener("click", () => this.app.deleteJob());

    // Clear selected job and release embedded iframes when details dialog is
    // dismissed (Escape, overlay click, or close button) to stop unnecessary
    // updateJobDetails calls and prevent background iframe resource usage.
    document.getElementById("details-dialog").addEventListener("sl-after-hide", () => {
      // Guard: if showJobDetails() was called between the hide start and this
      // callback, the dialog is already re-opening for a new job. Don't clear.
      const dlg = document.getElementById("details-dialog");
      if (dlg.open) return;
      this.app.selectedJobId = null;
      // Clear content to stop YouTube/Twitch iframe embeds from running in background
      const content = document.getElementById("job-details-content");
      if (content) content.innerHTML = "";
    });

    // Copy buttons in details dialog (event delegation via data-copy attribute)
    document.getElementById("details-dialog").addEventListener("click", (e) => {
      const copyBtn = e.target.closest("[data-copy]");
      if (copyBtn) {
        this.app.copyTextToClipboard(copyBtn.dataset.copy);
      }
    });

    // Event delegation for trim delete buttons and watch actions (avoids inline onclick)
    const detailsContent = document.getElementById("job-details-content");
    if (detailsContent) {
      detailsContent.addEventListener("click", async (e) => {
        const btn = e.target.closest("[data-delete-trim]");
        if (btn) {
          e.stopPropagation();
          this.app.deleteTrim(btn.dataset.jobId, btn.dataset.trimId);
          return;
        }
        if (e.target.closest("#details-mark-watched")) {
          const res = await fetch(`/api/jobs/${this.app.selectedJobId}/watched`, { method: "POST" });
          if (!res.ok) this.app.showToast("Failed to mark watched", "danger");
          return;
        }
        if (e.target.closest("#details-mark-unwatched")) {
          const res = await fetch(`/api/jobs/${this.app.selectedJobId}/watched`, { method: "DELETE" });
          if (!res.ok) this.app.showToast("Failed to mark unwatched", "danger");
          return;
        }
      });
    }
  }

  showJobDetails(job) {
    this.app.selectedJobId = job.id;
    this.renderJobDetails(job);
    this.loadJobLogs(job.id);
    document.getElementById("details-dialog").show();
    this._fetchStagingFields(job.id);
  }

  /**
   * Fetch the enriched job (GET /api/jobs/{id}) to seed hasStaging/hasSegments.
   * Every payload that populates this.app.jobs (WS initial_state/jobs_update/
   * job_update, /api/jobs/archived) is a raw DB row WITHOUT these computed
   * fields, so the Resume/Mux buttons in the details dialog would never
   * appear without this. _preserveStagingFields keeps them alive across
   * subsequent WS updates. Failures are silent — buttons just stay hidden.
   */
  async _fetchStagingFields(jobId) {
    try {
      const response = await fetch(`/api/jobs/${encodeURIComponent(jobId)}`, { cache: "no-store" });
      if (!response.ok) return;
      const enriched = await response.json();
      // Look the job up again — a jobs_update may have replaced the array
      // (and the object) while the fetch was in flight.
      const job = this.app.jobs.find((j) => j.id === jobId) ||
        this.app.archivedJobs.find((j) => j.id === jobId);
      if (!job) return;
      job.hasStaging = enriched.hasStaging;
      job.hasSegments = enriched.hasSegments;
      // Re-evaluate button visibility if the dialog is still on this job
      if (this.app.selectedJobId === jobId && document.getElementById("details-dialog").open) {
        this.updateDetailsButtons(job);
      }
    } catch { /* network blip — buttons stay hidden, same as before the fetch */ }
  }

  // Update job details without rebuilding logs section
  updateJobDetails(job) {
    const content = document.getElementById("job-details-content");
    if (!content) return;

    // If status changed, rebuild the details to refresh structural elements
    // (segment rows, embed, buttons) that depend on status.
    const statusBadge = content.querySelector(".status");
    const currentStatus = statusBadge?.textContent;
    if (currentStatus && currentStatus !== this.app.displayStatus(job.status)) {
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

    // Update status badge. Every write below is diffed first: this method runs
    // on the ~60 Hz job_update path, and re-assigning an identical string still
    // dirties layout (the pattern app.js:1382 established). Sweep T2-21.
    if (statusBadge) {
      const statusClass = `status ${job.status.toLowerCase().replace("?", "")}`;
      if (statusBadge.className !== statusClass) statusBadge.className = statusClass;
      const statusText = this.app.displayStatus(job.status);
      if (statusBadge.textContent !== statusText) statusBadge.textContent = statusText;
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
        if (valueEl.textContent !== job.title) valueEl.textContent = job.title;
      } else if (labelText === "Channel:") {
        if (valueEl.textContent !== job.channelName) valueEl.textContent = job.channelName;
      } else if (labelText === "Category:" && job.twitchCategory) {
        if (valueEl.textContent !== job.twitchCategory) valueEl.textContent = job.twitchCategory;
      }
    }

    // Update progress text
    const progressRow = content.querySelector('[data-field="progress"]');
    if (progressRow) {
      const progressText = this.app.formatProgress(job);
      if (progressRow.textContent !== progressText) progressRow.textContent = progressText;
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
      const segText = isTwitchSeg ? vDisplay : `V: ${vDisplay} | A: ${aDisplay}`;
      if (segField.textContent !== segText) segField.textContent = segText;
    }

    // Update chat status
    const chatField = content.querySelector('[data-field="chat"]');
    if (chatField && job.chatStatus) {
      const chatVariantMap = { downloading: "primary", finished: "success", error: "danger", unavailable: "neutral", pending: "neutral" };
      const badge = chatField.querySelector("sl-badge");
      if (badge) {
        const variant = chatVariantMap[job.chatStatus] || "neutral";
        if (badge.variant !== variant) badge.variant = variant;
        if (badge.textContent !== job.chatStatus) badge.textContent = job.chatStatus;
      }
      // Update message count — text node after the badge
      const existingText = badge && badge.nextSibling && badge.nextSibling.nodeType === Node.TEXT_NODE
        ? badge.nextSibling : null;
      // toLocaleString is the expensive half: skipped unless the count moved.
      const countText = job.totalChatMessages ? ` (${job.totalChatMessages.toLocaleString()} messages)` : "";
      if (existingText) {
        if (existingText.textContent !== countText) existingText.textContent = countText;
      } else if (countText) {
        chatField.appendChild(document.createTextNode(countText));
      }
    }

    // Update incomplete-tail badge — its presence is conditional (job.status ===
    // "Finished" && job.incompleteTail), like the error div below, so it needs
    // create/remove handling rather than a plain value patch.
    const incompleteTailValue = content.querySelector('[data-field="incomplete-tail"]');
    const shouldShowIncompleteTail = job.status === "Finished" && job.incompleteTail;
    if (shouldShowIncompleteTail && !incompleteTailValue) {
      let typeRow = null;
      for (const row of content.querySelectorAll(".details-row")) {
        const label = row.querySelector(".details-label");
        if (label && label.textContent === "Type:") { typeRow = row; break; }
      }
      if (typeRow) {
        const newRow = document.createElement("div");
        newRow.className = "details-row";
        newRow.innerHTML = '<span class="details-label"></span><span class="details-value" data-field="incomplete-tail"><sl-badge variant="warning">Incomplete tail</sl-badge></span>';
        typeRow.parentNode.insertBefore(newRow, typeRow);
      }
    } else if (!shouldShowIncompleteTail && incompleteTailValue) {
      incompleteTailValue.closest(".details-row")?.remove();
    }

    // Update speed if present
    const speedRow = document.getElementById("speed-row");
    const speedValue = content.querySelector('[data-field="speed"]');
    if (speedRow && speedValue) {
      const speedText = job.speed || "";
      if (speedValue.textContent !== speedText) speedValue.textContent = speedText;
      const speedDisplay = job.speed ? "" : "none";
      if (speedRow.style.display !== speedDisplay) speedRow.style.display = speedDisplay;
    }

    // Update updated time
    const updatedRow = content.querySelector('[data-field="updated"]');
    if (updatedRow) {
      const updatedText = this.app.formatRelativeTime(job.updatedAt);
      if (updatedRow.textContent !== updatedText) updatedRow.textContent = updatedText;
      // The full-date title comes from toLocaleString — an Intl format on every
      // tick — so it is rebuilt only when the SOURCE timestamp moves, which is
      // also exactly when data-timestamp has to be re-stamped.
      if (updatedRow.dataset.timestamp !== job.updatedAt) {
        updatedRow.dataset.timestamp = job.updatedAt;
        updatedRow.title = new Date(job.updatedAt).toLocaleString();
      }
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
    // the details view fetches staging; hide until it is known
    const canResume = canResumeJob(job, { requireKnownStaging: true });
    const canReinit = REINIT_STATUSES.has(job.status);
    const canMux = MUX_STATUSES.has(job.status) && job.hasSegments;
    const canDelete = DELETE_STATUSES.has(job.status);
    const hasFile = job.status === "Finished" && job.filename;
    const isActive = ["Upcoming", "Live", "Downloading", "Muxing"].includes(
      job.status,
    );

    // Same ~60 Hz path as updateJobDetails: diff before writing. Assigning an
    // identical style.display still invalidates style on that element.
    const setDisplay = (id, shown) => {
      const el = document.getElementById(id);
      if (!el) return;
      const value = shown ? "" : "none";
      if (el.style.display !== value) el.style.display = value;
    };

    setDisplay("details-cancel-btn", canCancel);
    setDisplay("details-resume-btn", canResume);
    setDisplay("details-reinit-btn", canReinit);
    setDisplay("details-mux-btn", canMux);
    setDisplay("details-delete-btn", canDelete);
    setDisplay("details-trim-btn", hasFile);
    const isLocalhost = ["localhost", "127.0.0.1", "::1"].includes(
      window.location.hostname,
    );
    setDisplay("details-open-folder-btn", (hasFile || isActive) && isLocalhost);
    setDisplay("details-play-btn", hasFile);
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
          <span class="details-value" data-field="segments">${this.app.escapeHtml(segDisplayValue)}</span>
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
      embedHtml = `<iframe class="details-embed" src="https://player.twitch.tv/?video=${this.app.escapeHtml(twitchVodId)}&parent=${this.app.escapeHtml(location.hostname)}&autoplay=false&muted=true" allowfullscreen></iframe>`;
    } else if (isTwitch && twitchLogin) {
      embedHtml = `<iframe class="details-embed" src="https://player.twitch.tv/?channel=${this.app.escapeHtml(twitchLogin)}&parent=${this.app.escapeHtml(location.hostname)}&autoplay=false&muted=true" allowfullscreen></iframe>`;
    } else {
      embedHtml = `<iframe class="details-embed" src="https://www.youtube-nocookie.com/embed/${this.app.escapeHtml(job.videoId)}" title="YouTube video player" frameborder="0" allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture" referrerpolicy="strict-origin-when-cross-origin" allowfullscreen></iframe>`;
    }

    content.innerHTML = `
      <div class="details-top">
        <div class="details-section">
          ${embedHtml}
        </div>

        <div class="details-section">
          <div class="details-row">
            <span class="details-label">${isTwitch ? "Stream ID:" : "Video ID:"}</span>
            <span class="details-value"><code>${this.app.escapeHtml(job.videoId)}</code><sl-icon-button class="details-copy-btn" name="clipboard" label="Copy" data-copy="${this.app.escapeHtml(job.videoId)}"></sl-icon-button></span>
          </div>
          ${streamUrl(job) ? `
          <div class="details-row">
            <span class="details-label">Stream URL:</span>
            <span class="details-value"><code>${this.app.escapeHtml(streamUrl(job))}</code><sl-icon-button class="details-copy-btn" name="clipboard" label="Copy stream URL" data-copy="${this.app.escapeHtml(streamUrl(job))}"></sl-icon-button></span>
          </div>` : ""}
          <div class="details-row">
            <span class="details-label">Title:</span>
            <span class="details-value">${this.app.escapeHtml(job.title)}</span>
          </div>
          <div class="details-row">
            <span class="details-label">Channel:</span>
            <span class="details-value">${this.app.escapeHtml(job.channelName)}</span>
          </div>
          <div class="details-row">
            <span class="details-label">Status:</span>
            <span class="details-value">
              <sl-badge class="status ${this.app.escapeHtml(statusClass)}" variant="primary">${this.app.escapeHtml(this.app.displayStatus(job.status))}</sl-badge>
              ${this.app.watchPillHtml(job)}
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
              <sl-badge variant="${chatVariant}">${this.app.escapeHtml(job.chatStatus)}</sl-badge>
              ${job.totalChatMessages ? ` (${this.app.escapeHtml(job.totalChatMessages.toLocaleString())} messages)` : ""}
            </span>
          </div>`;
          })() : ""}
          ${job.status === "Finished" && job.incompleteTail ? `
          <div class="details-row">
            <span class="details-label"></span>
            <span class="details-value" data-field="incomplete-tail"><sl-badge variant="warning">Incomplete tail</sl-badge></span>
          </div>` : ""}
          ${
            job.isVod
              ? `
          <div class="details-row">
            <span class="details-label">Type:</span>
            <span class="details-value">VOD</span>
          </div>
          <div class="details-row" style="${this.app.formatProgress(job) !== "Complete" ? "" : "display:none"}">
            <span class="details-label">Progress:</span>
            <span class="details-value" data-field="progress">${this.app.escapeHtml(this.app.formatProgress(job))}</span>
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
            <span class="details-value" data-field="speed">${this.app.escapeHtml(job.speed || "")}</span>
          </div>
          ${
            job.filename
              ? `
          <div class="details-row">
            <span class="details-label">Filename:</span>
            <span class="details-value">${this.app.escapeHtml(job.filename)}<sl-icon-button class="details-copy-btn" name="clipboard" label="Copy" data-copy="${this.app.escapeHtml(job.filename)}"></sl-icon-button></span>
          </div>
          `
              : ""
          }
          <div class="details-row">
            <span class="details-label">Created:</span>
            <span class="details-value" data-timestamp="${this.app.escapeHtml(job.createdAt)}" title="${new Date(job.createdAt).toLocaleString()}">${this.app.escapeHtml(this.app.formatRelativeTime(job.createdAt))}</span>
          </div>
          ${job.downloadStartedAt ? `
          <div class="details-row">
            <span class="details-label">DL Started:</span>
            <span class="details-value" data-timestamp="${this.app.escapeHtml(job.downloadStartedAt)}" title="${new Date(job.downloadStartedAt).toLocaleString()}">${this.app.escapeHtml(this.app.formatRelativeTime(job.downloadStartedAt))}</span>
          </div>
          ` : ""}
          <div class="details-row">
            <span class="details-label">Updated:</span>
            <span class="details-value" data-field="updated" data-timestamp="${this.app.escapeHtml(job.updatedAt)}" title="${new Date(job.updatedAt).toLocaleString()}">${this.app.escapeHtml(this.app.formatRelativeTime(job.updatedAt))}</span>
          </div>
          ${isTwitch && job.twitchCategory ? `
          <div class="details-row">
            <span class="details-label">Category:</span>
            <span class="details-value">${this.app.escapeHtml(job.twitchCategory)}</span>
          </div>
          ` : ""}
          ${isTwitch && job.twitchQuality ? `
          <div class="details-row">
            <span class="details-label">Quality:</span>
            <span class="details-value">${this.app.escapeHtml(job.twitchQuality)}</span>
          </div>
          ` : ""}
          ${job.streamStartTime ? (() => {
            const isScheduled = job.status === "Upcoming" && new Date(job.streamStartTime).getTime() > Date.now();
            const label = isScheduled ? "Scheduled" : "Stream Start";
            const value = isScheduled ? new Date(job.streamStartTime).toLocaleString() : this.app.formatRelativeTime(job.streamStartTime);
            const tsAttr = isScheduled ? "" : ` data-timestamp="${this.app.escapeHtml(job.streamStartTime)}" title="${new Date(job.streamStartTime).toLocaleString()}"`;
            return `
          <div class="details-row">
            <span class="details-label">${label}:</span>
            <span class="details-value"${tsAttr}>${this.app.escapeHtml(value)}</span>
          </div>
          ${isScheduled ? `
          <div class="details-row">
            <span class="details-label">Starts In:</span>
            <span class="details-value" data-timestamp-countdown="${this.app.escapeHtml(job.streamStartTime)}">${this.app.escapeHtml((() => { const diff = Math.floor((new Date(job.streamStartTime).getTime() - Date.now()) / 1000); return diff > 0 ? this.app.formatDurationSeconds(diff) : "Now"; })())}</span>
          </div>` : ""}`;
          })() : ""}
          ${job.streamEndTime ? `
          <div class="details-row">
            <span class="details-label">Stream End:</span>
            <span class="details-value" data-timestamp="${this.app.escapeHtml(job.streamEndTime)}" title="${new Date(job.streamEndTime).toLocaleString()}">${this.app.escapeHtml(this.app.formatRelativeTime(job.streamEndTime))}</span>
          </div>
          ` : ""}
          ${job.lengthSeconds && job.lengthSeconds > 0 ? `
          <div class="details-row">
            <span class="details-label">Duration:</span>
            <span class="details-value">${this.app.escapeHtml(this.app.formatDurationSeconds(job.lengthSeconds))}</span>
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
              : `itag ${this.app.escapeHtml(job.selectedVideoItag)}`
          }</span>
        </div>
        ` : ""}
        ${job.selectedAudioItag != null ? `
        <div class="details-row">
          <span class="details-label">Audio Format:</span>
          <span class="details-value">${
            job.selectedAudioItag === -1
              ? "None (video only)"
              : `itag ${this.app.escapeHtml(job.selectedAudioItag)}`
          }</span>
        </div>
        ` : ""}
        ${job.startTime != null || job.endTime != null ? `
        <div class="details-row">
          <span class="details-label">Time Range:</span>
          <span class="details-value">
            ${this.app.escapeHtml(this.app.formatTimestamp(job.startTime || 0))} - ${job.endTime != null ? this.app.escapeHtml(this.app.formatTimestamp(job.endTime)) : "end"}
            ${job.endTime != null && job.startTime != null ? ` (${this.app.escapeHtml(this.app.formatTimestamp(job.endTime - job.startTime))})` : ""}
          </span>
        </div>
        ` : ""}
      </div>
      ` : ""}

      ${job.trims && job.trims.length > 0 ? `
      <sl-details summary="Trims (${this.app.escapeHtml(job.trims.length)})" open class="details-section">
        <div class="trim-list">
          ${job.trims.map(trim => {
            const range = `${this.app.escapeHtml(this.app.formatTimestamp(trim.startTime))} - ${this.app.escapeHtml(this.app.formatTimestamp(trim.endTime))}`;
            const duration = `${this.app.escapeHtml(Math.floor(trim.duration))}s`;
            const size = trim.fileSize ? this.app.escapeHtml(this.app.formatBytes(trim.fileSize)) : '?';
            return `
              <div class="trim-item" style="display: flex; justify-content: space-between; align-items: center; padding: 8px; border-bottom: 1px solid var(--sl-color-neutral-200);">
                <span>
                  <strong>${range}</strong> (${duration}, ${size})
                </span>
                <sl-button size="small" variant="danger" data-delete-trim data-job-id="${this.app.escapeHtml(job.id)}" data-trim-id="${this.app.escapeHtml(trim.id)}">
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
        <strong>Error:</strong> ${this.app.escapeHtml(job.error)}
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
            <span class="details-value">${this.app.escapeHtml(job.videoWidth)}x${this.app.escapeHtml(job.videoHeight)}</span>
          </div>`;
        }
        if (hasFps) {
          rows += `<div class="details-row">
            <span class="details-label">FPS:</span>
            <span class="details-value">${this.app.escapeHtml(job.videoFps)}</span>
          </div>`;
        }
        if (hasFileSize) {
          rows += `<div class="details-row">
            <span class="details-label">File Size:</span>
            <span class="details-value">${this.app.escapeHtml(this.app.formatBytes(job.fileSize))}</span>
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
            <span class="details-value">${this.app.escapeHtml(segValue)}</span>
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
            <span class="details-value" style="color: var(--sl-color-warning-600)">${this.app.escapeHtml(job.gaps.length)} segments${this.app.escapeHtml(detail)}</span>
          </div>`;
        }
        return `<div class="details-section"><strong>Media:</strong>${rows}</div>`;
      })()}

      ${(() => {
        if (!job.segments || job.segments.length === 0) return "";
        let segRows = "";
        job.segments.forEach((seg) => {
          const dur = seg.durationSeconds ? `${Math.round(seg.durationSeconds)}s` : "—";
          const size = seg.fileSize ? this.app.formatBytes(seg.fileSize) : "—";
          const res = seg.videoWidth && seg.videoHeight ? `${this.app.escapeHtml(seg.videoWidth)}x${this.app.escapeHtml(seg.videoHeight)}` : "";
          // Part number from segmentIndex (matches the " - partN" filename),
          // not the loop index — short-skipped spans can leave holes.
          const partNo = (seg.segmentIndex ?? 0) + 1;
          const chat = seg.chatFile ? " — chat" : "";
          segRows += `<div class="details-row" style="padding-left:8px;">
            <span class="details-label">Part ${this.app.escapeHtml(partNo)}:</span>
            <span class="details-value">${this.app.escapeHtml(seg.quality)} — ${this.app.escapeHtml(dur)} — ${this.app.escapeHtml(size)}${res ? ` — ${res}` : ""}${chat}</span>
          </div>`;
        });
        return `<div class="details-section"><strong>Parts:</strong>${segRows}</div>`;
      })()}

      ${job.description ? `
      <div class="details-section">
        <strong>Description:</strong>
        <div style="white-space: pre-wrap; word-break: break-word; color: var(--sl-color-neutral-600); margin-top: 4px; font-size: 0.9em;">${this.app.escapeHtml(job.description)}</div>
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
    const abort = new AbortController();
    this._jobLogsAbort = abort;

    try {
      const response = await fetch(`/api/jobs/${jobId}/logs`, { signal: abort.signal });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      // Check selection before parsing the body; a switched-away job should
      // short-circuit instead of wasting CPU decoding JSON we'll discard.
      if (this.app.selectedJobId !== jobId || abort.signal.aborted) {
        abort.abort();
        return;
      }
      const logs = await response.json();
      // Re-check after the async body parse — selection may have moved on.
      if (this.app.selectedJobId !== jobId || abort.signal.aborted) return;
      const logsEl = document.getElementById("job-logs-content");
      if (logsEl) {
        logsEl.textContent =
          Array.isArray(logs) && logs.length > 0 ? logs.join("\n") : "No logs for this job yet.";
      }
    } catch (e) {
      if (e.name === "AbortError") return; // Superseded by a new fetch
      console.error("Failed to load job logs:", e);
      if (this.app.selectedJobId !== jobId) return;
      const logsEl = document.getElementById("job-logs-content");
      if (logsEl) logsEl.textContent = "Failed to load logs.";
    }
  }

  /** Fetch fresh job data and update the details dialog and jobs array. */
  async _refreshJobDetails(jobId) {
    if (this.app.selectedJobId !== jobId) return;
    try {
      const jobResponse = await fetch(`/api/jobs/${jobId}`, {
        cache: 'no-store',
      });
      if (jobResponse.ok) {
        const updatedJob = await jobResponse.json();
        const jobIndex = this.app.jobs.findIndex(j => j.id === jobId);
        if (jobIndex !== -1) {
          this.app.jobs[jobIndex] = updatedJob;
        } else {
          const archivedIndex = this.app.archivedJobs.findIndex(j => j.id === jobId);
          if (archivedIndex !== -1) {
            this.app.archivedJobs[archivedIndex] = updatedJob;
            this.app.renderArchivedJobs();
          }
        }
        this.renderJobDetails(updatedJob);
        this.loadJobLogs(jobId);
      }
    } catch {
      // Non-critical — job will sync via WebSocket
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
