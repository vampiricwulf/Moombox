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

    const pct = disk.usedPct?.toFixed(1) ?? "0.0";
    const freeStr = formatBytes(disk.free || 0);
    const totalStr = formatBytes(disk.total || 0);
    const usedStr = formatBytes((disk.total || 0) - (disk.free || 0));
    const level = disk.warnLevel || "ok";

    let barClass = "disk-bar-fill";
    if (level === "critical") barClass += " disk-critical";
    else if (level === "warn") barClass += " disk-warn";

    el.innerHTML = `
      <div class="disk-bar-container">
        <div class="disk-bar">
          <div class="${barClass}" style="width: ${Math.min(disk.usedPct || 0, 100)}%"></div>
        </div>
        <div class="disk-bar-labels">
          <span>${usedStr} used of ${totalStr}</span>
          <span>${freeStr} free (${pct}% used)</span>
        </div>
      </div>
    `;
  }

  renderStorage(disk, storage) {
    const el = document.getElementById("stats-storage");
    if (!el) return;

    const cards = [];
    cards.push(this._card("Total Recorded", formatBytes(storage.totalSize || 0)));
    cards.push(this._card("Total Jobs", (storage.jobCount || 0).toLocaleString()));

    // Platform breakdown
    const byPlat = storage.byPlatform || {};
    cards.push(this._card("YouTube Storage", formatBytes(byPlat.youtube || 0)));
    cards.push(this._card("Twitch Storage", formatBytes(byPlat.twitch || 0)));

    // Status breakdown
    const byStat = storage.byStatus || {};
    cards.push(this._card("Finished", formatBytes(byStat.finished || 0), "stat-finished"));
    cards.push(this._card("Error", formatBytes(byStat.error || 0), "stat-error"));

    el.innerHTML = cards.join("");
  }

  renderActivity(activity) {
    const el = document.getElementById("stats-activity");
    if (!el) return;

    const cards = [];
    cards.push(this._card("Streams Archived", (activity.totalFinished || 0).toLocaleString()));
    cards.push(this._card("Total Recording Time", formatDurationSeconds(activity.totalDuration || 0)));
    cards.push(this._card("Chat Messages", (activity.totalChatMessages || 0).toLocaleString()));
    cards.push(this._card("Active Downloads", (activity.activeDownloads || 0).toString(), "stat-active"));
    cards.push(this._card("Muxing", (activity.activeMuxing || 0).toString(), "stat-muxing"));

    const byPlat = activity.byPlatform || {};
    cards.push(this._card("YouTube Jobs", (byPlat.youtube || 0).toLocaleString()));
    cards.push(this._card("Twitch Jobs", (byPlat.twitch || 0).toLocaleString()));

    // Uptime from app state (available in status API)
    if (this.app._uptimeSeconds) {
      cards.push(this._card("Uptime", formatDurationSeconds(Math.floor(this.app._uptimeSeconds))));
    }

    el.innerHTML = cards.join("");
  }

  _card(label, value, extraClass) {
    const cls = "stat-card" + (extraClass ? " " + extraClass : "");
    return `<div class="${cls}"><div class="stat-value">${value}</div><div class="stat-label">${label}</div></div>`;
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
    const freeGB = (disk.free / (1024 * 1024 * 1024)).toFixed(0);
    el.innerHTML = `<sl-icon name="hdd"></sl-icon> ${freeGB}G free`;
    el.title = `Disk ${Math.round(disk.usedPct)}% used`;

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
