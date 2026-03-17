/**
 * Stats Controller — Health/Stats dashboard
 */
import { formatBytes, formatDurationSeconds } from "./utils.js";

export class StatsController {
  constructor(app) {
    this.app = app;
    this._refreshInterval = null;
    this._active = false;
  }

  /** Called when the Stats tab becomes visible. */
  activate() {
    this._active = true;
    this.loadStats();
    // Clear any existing interval to prevent leaks on double-activate
    if (this._refreshInterval) clearInterval(this._refreshInterval);
    // Auto-refresh every 60 seconds while active
    this._refreshInterval = setInterval(() => this.loadStats(), 60000);
  }

  /** Called when the Stats tab is hidden. */
  deactivate() {
    this._active = false;
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
    this.renderDisk(data.disk);
    this.renderStorage(data.disk, data.storage);
    this.renderActivity(data.activity);
  }

  renderDisk(disk) {
    const el = document.getElementById("stats-disk");
    if (!el) return;

    const usedPct = isFinite(disk.usedPct) ? disk.usedPct : 0;
    const pct = usedPct.toFixed(1);
    const freeStr = formatBytes(disk.free || 0);
    const totalStr = formatBytes(disk.total || 0);
    const usedStr = formatBytes((disk.total || 0) - (disk.free || 0));
    const level = disk.warnLevel || "ok";

    el.textContent = "";

    const container = document.createElement("div");
    container.className = "disk-bar-container";

    const bar = document.createElement("div");
    bar.className = "disk-bar";
    const fill = document.createElement("div");
    fill.className = "disk-bar-fill" + (level === "critical" ? " disk-critical" : level === "warn" ? " disk-warn" : "");
    fill.style.width = `${Math.min(usedPct, 100)}%`;
    bar.appendChild(fill);
    container.appendChild(bar);

    const labels = document.createElement("div");
    labels.className = "disk-bar-labels";
    const usedLabel = document.createElement("span");
    usedLabel.textContent = `${usedStr} used of ${totalStr}`;
    const freeLabel = document.createElement("span");
    freeLabel.textContent = `${freeStr} free (${pct}% used)`;
    labels.appendChild(usedLabel);
    labels.appendChild(freeLabel);
    container.appendChild(labels);

    el.appendChild(container);
  }

  renderStorage(disk, storage) {
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

    if (this.app._uptimeSeconds) {
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

    if (!disk || !disk.total || !disk.warnLevel || disk.warnLevel === "ok") {
      el.style.display = "none";
      return;
    }

    el.style.display = "";
    const freeGB = ((disk.free || 0) / (1024 * 1024 * 1024)).toFixed(0);
    el.textContent = "";
    const hddIcon = document.createElement("sl-icon");
    hddIcon.name = "hdd";
    el.appendChild(hddIcon);
    el.appendChild(document.createTextNode(` ${freeGB}G free`));
    el.title = `Disk ${Math.round(isFinite(disk.usedPct) ? disk.usedPct : 0)}% used`;

    el.className = disk.warnLevel === "critical" ? "disk-indicator-critical" : "disk-indicator-warn";
  }

  /** Update status bar active download count. */
  updateActiveIndicator(jobs) {
    const el = document.getElementById("active-indicator");
    if (!el) return;

    const active = (jobs || []).filter(j =>
      j.status === "Downloading" || j.status === "Live" || j.status === "Muxing"
    ).length;

    if (active > 0) {
      el.style.display = "";
      el.textContent = `\u25B6 ${active}`;
      el.className = "active-indicator-on";
    } else {
      el.style.display = "none";
    }
  }
}
