/**
 * Stats Controller — Health/Stats dashboard
 */
import { formatBytes, formatDurationSeconds, diskGaugeLevel } from "./utils.js";

export class StatsController {
  constructor(app) {
    this.app = app;
    this._refreshInterval = null;
    // The last readings drawn, so a change of the backlog's disk gate
    // (onAdmissionHolds) redraws them without waiting for the next reading.
    this._lastGaugeDisk = null;
    this._lastIndicatorDisk = null;
  }

  /** The admission holds changed: redraw what reads the disk gate. */
  onAdmissionHolds() {
    if (this._lastGaugeDisk) this.renderDisk(this._lastGaugeDisk);
    if (this._lastIndicatorDisk) this.updateDiskIndicator(this._lastIndicatorDisk);
  }

  /** Called when the Stats tab becomes visible. */
  activate() {
    this.loadStats();
    // Clear any existing interval to prevent leaks on double-activate
    if (this._refreshInterval) clearInterval(this._refreshInterval);
    // Auto-refresh every 60 seconds while active
    this._refreshInterval = setInterval(() => this.loadStats(), 60000);
  }

  /** Called when the Stats tab is hidden. */
  deactivate() {
    if (this._refreshInterval) {
      clearInterval(this._refreshInterval);
      this._refreshInterval = null;
    }
  }

  async loadStats() {
    try {
      const response = await fetch("/api/stats");
      if (!response.ok) return;
      const data = await response.json();
      this.render(data);
    } catch (e) {
      console.error("Failed to load stats:", e);
    }
  }

  render(data) {
    if (data.disk) this.renderDisk(data.disk);
    if (data.storage) this.renderStorage(data.storage);
    if (data.activity) this.renderActivity(data.activity);
  }

  renderDisk(disk) {
    const el = document.getElementById("stats-disk");
    if (!el) return;
    this._lastGaugeDisk = disk;

    // No reading (the server sends zeros before its first successful disk
    // check, and for as long as the output directory's disk cannot be read):
    // say so rather than "0B used of 0B".
    if (!(disk.total > 0)) {
      el.textContent = "No disk reading available";
      return;
    }

    const usedPct = isFinite(disk.usedPct) ? disk.usedPct : 0;
    const pct = usedPct.toFixed(1);
    const freeStr = formatBytes(disk.free || 0);
    const totalStr = formatBytes(disk.total || 0);
    const usedStr = formatBytes((disk.total || 0) - (disk.free || 0));
    // "holding" while the backlog's disk gate holds below critical (owner
    // ruling R-D3): the reading alone said "warn" while nothing was admitted.
    const level = diskGaugeLevel(disk, this.app.admissionHolds);

    el.textContent = "";

    const container = document.createElement("div");
    container.className = "disk-bar-container";

    const bar = document.createElement("div");
    bar.className = "disk-bar";
    const fill = document.createElement("div");
    fill.className = "disk-bar-fill" + (level === "critical" ? " disk-critical" : level === "holding" ? " disk-holding" : level === "warn" ? " disk-warn" : "");
    fill.style.width = `${Math.min(usedPct, 100)}%`;
    bar.appendChild(fill);
    container.appendChild(bar);

    const labels = document.createElement("div");
    labels.className = "disk-bar-labels";
    const usedLabel = document.createElement("span");
    usedLabel.textContent = `${usedStr} used of ${totalStr}`;
    const freeLabel = document.createElement("span");
    freeLabel.textContent = `${freeStr} free (${pct}% used)${level === "holding" ? " · holding backlog" : ""}`;
    labels.appendChild(usedLabel);
    labels.appendChild(freeLabel);
    container.appendChild(labels);

    el.appendChild(container);
  }

  renderStorage(storage) {
    const el = document.getElementById("stats-storage");
    if (!el) return;
    el.innerHTML = "";

    const byPlat = storage.byPlatform || {};
    const byStat = storage.byStatus || {};

    el.appendChild(this._createCard("Total Recorded", formatBytes(storage.totalSize || 0)));
    el.appendChild(this._createCard("Total Jobs", (storage.jobCount || 0).toLocaleString()));
    el.appendChild(this._createCard("YouTube Storage", formatBytes(byPlat.youtube || 0)));
    el.appendChild(this._createCard("Twitch Storage", formatBytes(byPlat.twitch || 0)));
    el.appendChild(this._createCard("Finished", formatBytes(byStat.finished || 0), "stat-finished"));
    el.appendChild(this._createCard("Error", formatBytes(byStat.error || 0), "stat-error"));
    // Total Recorded counts cancelled jobs' files too; without this card the
    // breakdown did not add up to it.
    el.appendChild(this._createCard("Cancelled", formatBytes(byStat.cancelled || 0)));
  }

  renderActivity(activity) {
    const el = document.getElementById("stats-activity");
    if (!el) return;
    el.innerHTML = "";

    const byPlat = activity.byPlatform || {};

    el.appendChild(this._createCard("Streams Archived", (activity.totalFinished || 0).toLocaleString()));
    el.appendChild(this._createCard("Total Recording Time", formatDurationSeconds(activity.totalDuration || 0)));
    el.appendChild(this._createCard("Chat Messages", (activity.totalChatMessages || 0).toLocaleString()));
    el.appendChild(this._createCard("Active Downloads", (activity.activeDownloads || 0).toString(), "stat-active"));
    el.appendChild(this._createCard("Muxing", (activity.activeMuxing || 0).toString(), "stat-muxing"));
    el.appendChild(this._createCard("YouTube Jobs", (byPlat.youtube || 0).toLocaleString()));
    el.appendChild(this._createCard("Twitch Jobs", (byPlat.twitch || 0).toLocaleString()));

    if (this.app._uptimeSeconds != null) {
      const elapsed = this.app._uptimeCapturedAt
        ? Math.floor((Date.now() - this.app._uptimeCapturedAt) / 1000)
        : 0;
      el.appendChild(this._createCard("Uptime", formatDurationSeconds(this.app._uptimeSeconds + elapsed)));
    }
  }

  /** Create a stat card element using safe DOM methods. */
  _createCard(label, value, extraClass) {
    const card = document.createElement("div");
    card.className = "stat-card" + (extraClass ? " " + extraClass : "");
    const valDiv = document.createElement("div");
    valDiv.className = "stat-value";
    valDiv.textContent = value;
    const labelDiv = document.createElement("div");
    labelDiv.className = "stat-label";
    labelDiv.textContent = label;
    card.appendChild(valDiv);
    card.appendChild(labelDiv);
    return card;
  }

  /** Update status bar disk indicator from WebSocket or status API data. */
  updateDiskIndicator(disk) {
    const el = document.getElementById("disk-indicator");
    if (!el) return;
    this._lastIndicatorDisk = disk;
    const level = diskGaugeLevel(disk, this.app.admissionHolds);

    if (!disk || !disk.total || level === "ok") {
      el.style.display = "none";
      return;
    }

    el.style.display = "";
    const freeGB = ((disk.free || 0) / (1024 * 1024 * 1024)).toFixed(0);
    el.textContent = "";
    const hddIcon = document.createElement("sl-icon");
    hddIcon.name = "hdd";
    el.appendChild(hddIcon);
    el.appendChild(document.createTextNode(` ${freeGB}G free${level === "holding" ? " · holding" : ""}`));
    el.title = `Disk ${Math.round(isFinite(disk.usedPct) ? disk.usedPct : 0)}% used`
      + (level === "holding" ? " — backlog admission held until usage is back under the critical threshold's margin" : "");

    el.className = level === "critical" ? "disk-indicator-critical" : level === "holding" ? "disk-indicator-warn disk-indicator-holding" : "disk-indicator-warn";
  }

  /** Update status bar active download count. */
  updateActiveIndicator(jobs) {
    const el = document.getElementById("active-indicator");
    if (!el) return;

    // "Queued" is deliberately absent — a queued job is waiting for an
    // archive slot, not an active download.
    const active = (jobs || []).filter(j =>
      j.status === "Downloading" || j.status === "Live" || j.status === "Muxing"
    ).length;

    // Called where a status can have moved: every full render, a card
    // insert, move or removal. Not on a progress tick, which leaves every
    // status as it was. Diff before writing (sweep T2-21).
    if (active > 0) {
      if (el.style.display !== "") el.style.display = "";
      const text = `\u25B6 ${active}`;
      if (el.textContent !== text) el.textContent = text;
      if (el.className !== "active-indicator-on") el.className = "active-indicator-on";
    } else if (el.style.display !== "none") {
      el.style.display = "none";
    }
  }
}
