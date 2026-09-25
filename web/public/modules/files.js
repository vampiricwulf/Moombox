/**
 * Files Controller — Files tab (orphaned output files + orphaned feed-history entries)
 */
import { formatBytes, formatRelativeTime } from "./utils.js";

export class FilesController {
  constructor(app) {
    this.app = app;
  }

  /** Wire up the Files tab's refresh/delete-all buttons. */
  bind() {
    const filesRefreshBtn = document.getElementById("files-refresh-btn");
    if (filesRefreshBtn) {
      filesRefreshBtn.addEventListener("click", () => this.fetchOrphanedFiles());
    }
    const filesDeleteAllBtn = document.getElementById("files-delete-all-btn");
    if (filesDeleteAllBtn) {
      filesDeleteAllBtn.addEventListener("click", () => this.deleteAllOrphanedFiles());
    }
    const historyRefreshBtn = document.getElementById("history-refresh-btn");
    if (historyRefreshBtn) {
      historyRefreshBtn.addEventListener("click", () => this.fetchOrphanedHistory());
    }
    const historyDeleteAllBtn = document.getElementById("history-delete-all-btn");
    if (historyDeleteAllBtn) {
      historyDeleteAllBtn.addEventListener("click", () => this.deleteAllOrphanedHistory());
    }
  }

  async fetchOrphanedFiles() {
    const refreshBtn = document.getElementById("files-refresh-btn");
    if (refreshBtn) refreshBtn.loading = true;
    try {
      const resp = await fetch("/api/files/orphaned");
      if (!resp.ok) throw new Error("Failed to fetch");
      const data = await resp.json();
      this._orphanedFiles = data;
      this.renderOrphanedFiles(data);
    } catch (err) {
      console.error("Failed to fetch orphaned files:", err);
      this._orphanedFiles = [];
      this.renderOrphanedFiles(null); // null signals error vs empty
    } finally {
      if (refreshBtn) refreshBtn.loading = false;
    }
  }

  renderOrphanedFiles(files) {
    const emptyEl = document.getElementById("files-empty");
    const tableWrapper = document.getElementById("files-table-wrapper");
    const deleteAllBtn = document.getElementById("files-delete-all-btn");

    if (files === null || (Array.isArray(files) && files.length === 0)) {
      if (emptyEl) {
        emptyEl.style.display = "";
        const msg = emptyEl.querySelector("p");
        if (msg) msg.textContent = files === null
          ? "Failed to load orphaned files. Try refreshing."
          : "No orphaned files found.";
      }
      if (tableWrapper) tableWrapper.style.display = "none";
      if (deleteAllBtn) deleteAllBtn.disabled = true;
      return;
    }

    if (emptyEl) emptyEl.style.display = "none";
    if (tableWrapper) tableWrapper.style.display = "";
    if (deleteAllBtn) deleteAllBtn.disabled = false;

    const table = document.getElementById("files-table");
    // Remove existing rows (keep header)
    table.querySelectorAll(".files-row").forEach((row) => row.remove());

    // Sort: staging first, then output, then trim
    const typeOrder = { staging: 0, output: 1, trim: 2 };
    const sorted = [...files].sort((a, b) => (typeOrder[a.type] ?? 9) - (typeOrder[b.type] ?? 9));

    for (const file of sorted) {
      const row = document.createElement("div");
      row.className = "files-row";

      const typeBadge = `<span class="files-type-badge ${this.app.escapeHtml(file.type)}">${this.app.escapeHtml(file.type)}</span>`;
      // A staging dir that still holds set-aside recordings is captured
      // footage, not scratch space — the sweep sends the names for exactly
      // this reason, and showing them is what lets an operator tell the
      // difference before clicking Delete.
      let pathInner = this.app.escapeHtml(file.relPath);
      if (Array.isArray(file.asides) && file.asides.length > 0) {
        const n = file.asides.length;
        pathInner += `<br><span class="files-asides" style="color: var(--sl-color-warning-600); font-size: 0.85em;">`
          + `${n} set-aside recording${n === 1 ? "" : "s"}: ${this.app.escapeHtml(file.asides.join(", "))}</span>`;
      }
      const pathStr = `<span class="files-path" title="${this.app.escapeHtml(file.path)}">${pathInner}</span>`;
      const sizeStr = `<span>${this.app.escapeHtml(formatBytes(file.size))}</span>`;
      const modStr = `<span data-timestamp="${this.app.escapeHtml(file.modified)}" title="${new Date(file.modified).toLocaleString()}">${this.app.escapeHtml(formatRelativeTime(file.modified))}</span>`;

      let jobStr = "";
      if (file.jobTitle) {
        jobStr = `<span class="files-job-info" title="${this.app.escapeHtml(file.jobId)}">${this.app.escapeHtml(file.jobTitle)} (${this.app.escapeHtml(file.jobStatus)})</span>`;
      } else {
        jobStr = `<span class="files-job-info">—</span>`;
      }

      const deleteBtn = `<sl-icon-button name="trash" label="Delete" class="files-delete-btn" data-path="${this.app.escapeHtml(file.path)}"></sl-icon-button>`;

      row.innerHTML = typeBadge + pathStr + sizeStr + modStr + jobStr + deleteBtn;
      table.appendChild(row);
    }

    // Event delegation for delete buttons — attach once
    if (!table._filesDelegated) {
      table._filesDelegated = true;
      table.addEventListener("click", (e) => {
        const btn = e.target.closest(".files-delete-btn");
        if (btn) this.deleteOrphanedFile(btn.dataset.path);
      });
    }
  }

  async deleteOrphanedFile(path) {
    if (!await this.app.showConfirm(`Delete this file?\n\n${path}`, { okLabel: "Delete", okVariant: "danger" })) return;

    try {
      const resp = await fetch("/api/files/orphaned", {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ paths: [path] }),
      });
      if (!resp.ok) throw new Error("Failed to delete");
      const result = await resp.json();
      if (result.deleted && result.deleted.length > 0) {
        this.app.showToast("File deleted", "success");
      } else if (result.errors && result.errors.length > 0) {
        this.app.showToast(`Failed: ${result.errors[0].error}`, "danger");
      }
      await this.fetchOrphanedFiles();
    } catch (err) {
      this.app.showToast("Failed to delete file", "danger");
    }
  }

  async deleteAllOrphanedFiles() {
    if (!this._orphanedFiles || this._orphanedFiles.length === 0) return;
    const fileCount = this._orphanedFiles.length;
    if (!await this.app.showConfirm(`Delete ${fileCount === 1 ? "this" : `all ${fileCount}`} orphaned file${fileCount === 1 ? "" : "s"}?`, { okLabel: "Delete All", okVariant: "danger" })) return;

    const deleteAllBtn = document.getElementById("files-delete-all-btn");
    if (deleteAllBtn) { deleteAllBtn.loading = true; deleteAllBtn.disabled = true; }

    const paths = this._orphanedFiles.map((f) => f.path);
    try {
      const resp = await fetch("/api/files/orphaned", {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ paths }),
      });
      if (!resp.ok) throw new Error("Failed to delete");
      const result = await resp.json();
      const count = result.deleted ? result.deleted.length : 0;
      const errCount = result.errors ? result.errors.length : 0;
      if (errCount > 0) {
        this.app.showToast(`Deleted ${count}, ${errCount} errors`, "warning");
      } else {
        this.app.showToast(`Deleted ${count} files`, "success");
      }
      await this.fetchOrphanedFiles();
    } catch (err) {
      this.app.showToast("Failed to delete files", "danger");
    } finally {
      if (deleteAllBtn) {
        deleteAllBtn.loading = false;
        // Let renderOrphanedFiles control disabled state — it disables the
        // button when the list is empty. Only force-enable here if the
        // fetch/render didn't run (e.g. DELETE request failed).
        const hasFiles = this._orphanedFiles && this._orphanedFiles.length > 0;
        deleteAllBtn.disabled = !hasFiles;
      }
    }
  }

  async fetchOrphanedHistory() {
    const refreshBtn = document.getElementById("history-refresh-btn");
    if (refreshBtn) refreshBtn.loading = true;
    try {
      const resp = await fetch("/api/history/orphaned");
      if (!resp.ok) throw new Error("Failed to fetch");
      const data = await resp.json();
      this._orphanedHistory = data;
      this.renderOrphanedHistory(data);
    } catch (err) {
      console.error("Failed to fetch orphaned history:", err);
      this._orphanedHistory = [];
      this.renderOrphanedHistory(null); // null signals error vs empty
    } finally {
      if (refreshBtn) refreshBtn.loading = false;
    }
  }

  renderOrphanedHistory(entries) {
    const emptyEl = document.getElementById("history-empty");
    const tableWrapper = document.getElementById("history-table-wrapper");
    const deleteAllBtn = document.getElementById("history-delete-all-btn");

    if (entries === null || (Array.isArray(entries) && entries.length === 0)) {
      if (emptyEl) {
        emptyEl.style.display = "";
        const msg = emptyEl.querySelector("p");
        if (msg) msg.textContent = entries === null
          ? "Failed to load orphaned history. Try refreshing."
          : "No orphaned history entries found.";
      }
      if (tableWrapper) tableWrapper.style.display = "none";
      if (deleteAllBtn) deleteAllBtn.disabled = true;
      return;
    }

    if (emptyEl) emptyEl.style.display = "none";
    if (tableWrapper) tableWrapper.style.display = "";
    if (deleteAllBtn) deleteAllBtn.disabled = false;

    const table = document.getElementById("history-table");
    table.querySelectorAll(".history-row").forEach((row) => row.remove());

    for (const entry of entries) {
      const row = document.createElement("div");
      row.className = "history-row";
      const vid = this.app.escapeHtml(entry.videoId);
      const vidStr = `<a class="history-vid" href="https://www.youtube.com/watch?v=${vid}" target="_blank" rel="noopener" title="${vid}">${vid}</a>`;
      const addedStr = `<span data-timestamp="${this.app.escapeHtml(entry.addedAt)}" title="${new Date(entry.addedAt).toLocaleString()}">${this.app.escapeHtml(formatRelativeTime(entry.addedAt))}</span>`;
      const deleteBtn = `<sl-icon-button name="trash" label="Remove" class="history-delete-btn" data-video-id="${vid}"></sl-icon-button>`;
      row.innerHTML = vidStr + addedStr + deleteBtn;
      table.appendChild(row);
    }

    // Event delegation for delete buttons — attach once.
    if (!table._historyDelegated) {
      table._historyDelegated = true;
      table.addEventListener("click", (e) => {
        const btn = e.target.closest(".history-delete-btn");
        if (btn) this.deleteOrphanedHistory(btn.dataset.videoId);
      });
    }
  }

  async deleteOrphanedHistory(videoId) {
    if (!await this.app.showConfirm(`Remove this history entry so the video can be re-discovered?\n\n${videoId}`, { okLabel: "Remove", okVariant: "danger" })) return;

    try {
      const resp = await fetch("/api/history/orphaned", {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ videoIds: [videoId] }),
      });
      if (!resp.ok) throw new Error("Failed to delete");
      const result = await resp.json();
      if (result.deleted && result.deleted.length > 0) {
        this.app.showToast("History entry removed", "success");
      }
      await this.fetchOrphanedHistory();
    } catch (err) {
      this.app.showToast("Failed to remove history entry", "danger");
    }
  }

  async deleteAllOrphanedHistory() {
    if (!this._orphanedHistory || this._orphanedHistory.length === 0) return;
    const count = this._orphanedHistory.length;
    if (!await this.app.showConfirm(`Remove ${count === 1 ? "this" : `all ${count}`} orphaned history entr${count === 1 ? "y" : "ies"}?`, { okLabel: "Remove All", okVariant: "danger" })) return;

    const deleteAllBtn = document.getElementById("history-delete-all-btn");
    if (deleteAllBtn) { deleteAllBtn.loading = true; deleteAllBtn.disabled = true; }

    const videoIds = this._orphanedHistory.map((e) => e.videoId);
    try {
      const resp = await fetch("/api/history/orphaned", {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ videoIds }),
      });
      if (!resp.ok) throw new Error("Failed to delete");
      const result = await resp.json();
      const n = result.deleted ? result.deleted.length : 0;
      this.app.showToast(`Removed ${n} entr${n === 1 ? "y" : "ies"}`, "success");
      await this.fetchOrphanedHistory();
    } catch (err) {
      this.app.showToast("Failed to remove history entries", "danger");
    } finally {
      if (deleteAllBtn) {
        deleteAllBtn.loading = false;
        const has = this._orphanedHistory && this._orphanedHistory.length > 0;
        deleteAllBtn.disabled = !has;
      }
    }
  }
}
