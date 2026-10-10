/**
 * Update Controller — the version indicator (header) and the update-available dialog
 */

// Moombox GitHub repository page — opened by double-clicking the version
// indicator. Mirrors constants.ProjectRepoURL on the Go side (keep in sync).
const GITHUB_REPO_URL = "https://github.com/vampiricwulf/Moombox";

export class UpdateController {
  constructor(app) {
    this.app = app;
  }

  /** Wire up the update dialog's buttons. */
  bind() {
    // Update dialog buttons
    const updateNowBtn = document.getElementById("update-now-btn");
    if (updateNowBtn) updateNowBtn.addEventListener("click", () => this.applyUpdate());
    const updateDismissBtn = document.getElementById("update-dismiss-btn");
    if (updateDismissBtn) updateDismissBtn.addEventListener("click", () => this.dismissUpdate());
  }

  // ===== Version / Update Indicator =====

  updateVersionIndicator() {
    const el = document.getElementById("version-indicator");
    if (!el) return;
    if (!this.app._version) { el.style.display = "none"; return; }

    el.style.display = "";
    // Bind a single stable click handler once; toggle what it does via
    // the _updateAvailable flag. Previously we cloneNode(false)'d the
    // element to drop the prior listener, which also dropped any nested
    // icon children that might be added later and any unrelated listeners.
    if (!this._versionClickHandler) {
      this._versionClickHandler = () => {
        if (this.available) {
          this.showUpdateDialog();
          return;
        }
        // No update pending: click-twice-to-open. The first click arms and
        // pops the manual tooltip ("Click again to open the GitHub page");
        // a second click inside the window opens the repo. Mirrors the
        // TUI's O G chord (and its confirm-chord arming pattern).
        const tooltip = document.getElementById("version-open-tooltip");
        if (this._versionOpenArmed) {
          clearTimeout(this._versionOpenArmTimer);
          this._versionOpenArmed = false;
          tooltip?.hide();
          window.open(GITHUB_REPO_URL, "_blank", "noopener");
          return;
        }
        this._versionOpenArmed = true;
        tooltip?.show();
        this._versionOpenArmTimer = setTimeout(() => {
          this._versionOpenArmed = false;
          tooltip?.hide();
        }, 3000);
      };
      el.addEventListener("click", this._versionClickHandler);
      // The keyboard half of the same role="button": the SAME handler, so
      // the click-twice-to-open arming is shared across both input paths and
      // a mouse-then-Enter gesture completes. preventDefault() because Space
      // is the page-scroll key.
      el.addEventListener("keydown", (e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          this._versionClickHandler();
        }
      });
    }
    if (this.available) {
      el.textContent = `v${this.app._version} ⬆`;
      el.className = "version-indicator has-update";
      el.title = `Update available: v${this.available.version}`;
      el.style.cursor = "pointer";
    } else {
      el.textContent = `v${this.app._version}`;
      el.className = "version-indicator";
      el.title = `Moombox v${this.app._version} — click to open the GitHub page`;
      el.style.cursor = "";
    }
    // Its text is a bare version string ("v2.8.8 ⬆"), which names the control
    // but not what pressing it does; the title already says that, so it is the
    // accessible name too.
    el.setAttribute("aria-label", el.title);
  }

  showUpdateDialog() {
    const dlg = document.getElementById("update-dialog");
    const notes = document.getElementById("update-release-notes");
    if (!dlg || !this.available) return;
    // The release whose notes go on screen. Update Now and Skip name it, and
    // the server refuses them (409) once a check has replaced it — an
    // update_available landing while the dialog is open changes
    // this.available, never the notes the operator is reading.
    this.shown = this.available;
    dlg.label = `Update to v${this.available.version}`;
    // SECURITY CONTRACT: this is the app's ONLY innerHTML sink for external
    // content (GitHub release markdown), deliberately unescaped because the
    // server renders AND sanitizes it via bluemonday.UGCPolicy()
    // (internal/updater/updater.go, pinned by
    // TestRenderReleaseNotesHtmlSanitizesScripts). If this field ever gets a
    // different source or the server policy loosens, this becomes stored XSS
    // — keep the sanitizer, or switch to textContent. Fall back to the raw
    // stripped markdown as TEXT if an older server didn't send the html
    // field, and finally to a generic message.
    const html = this.available.releaseNotesHtml || "";
    if (html) {
      notes.innerHTML = html;
    } else {
      notes.textContent = this.available.releaseNotes || "No release notes available.";
    }
    dlg.show();
  }

  // The body of an apply or a skip: the tag of the release whose notes are on
  // screen. None when the dialog never showed one; the server then acts on
  // whatever is pending.
  _shownRequest() {
    return {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ tagName: this.shown?.tagName }),
    };
  }

  // A 409 from apply or dismiss: a check found another release while the
  // dialog was open, and the server refused to act on a release whose notes
  // were never on screen. Its notes replace the ones the operator read, so
  // they decide again; nothing was installed or skipped.
  _showPendingInstead(data, verb) {
    this.available = data.pending;
    this.updateVersionIndicator();
    this.showUpdateDialog();
    this.app.showToast(`Not ${verb}: ${data.error}`, "warning");
  }

  async applyUpdate() {
    // Updating restarts the whole process: active recordings are
    // interrupted and resume on the new binary, but live segments broadcast
    // during the ~30s gap can be lost (Twitch expires them fastest). Make
    // that a deliberate choice, not a surprise.
    const active = (this.app.jobs || []).filter(
      (j) => j.status === "Downloading" || j.status === "Live" || j.status === "Muxing",
    ).length;
    if (active > 0) {
      const noun = active === 1 ? "download is" : "downloads are";
      const ok = await this.app.showConfirm(
        `${active} ${noun} active — the update restart interrupts them, and live segments during the ~30s gap may be lost (Twitch especially). Update anyway?`,
        { okLabel: "Update Anyway", okVariant: "warning" },
      );
      if (!ok) return;
    }
    const btn = document.getElementById("update-now-btn");
    if (btn) { btn.loading = true; btn.disabled = true; }
    try {
      const resp = await fetch("/api/update/apply", this._shownRequest());
      if (resp.ok) {
        this.app.showToast("Update applied. Restarting...", "success");
        document.getElementById("update-dialog")?.hide();
      } else {
        const data = await resp.json().catch(() => ({ error: resp.statusText }));
        if (resp.status === 409 && data.pending) {
          this._showPendingInstead(data, "updated");
        } else {
          this.app.showToast("Update failed: " + (data.error || "Unknown error"), "danger");
        }
      }
    } catch (e) {
      // No answer is not a failure: the server keeps downloading after the
      // browser stops waiting (a slow link outlives its response timeout),
      // and restarts on its own when the update lands.
      this.app.showToast(`Lost contact with the server (${e.message}) — the update may still be downloading; the dashboard reconnects when it restarts.`, "warning");
    } finally {
      if (btn) { btn.loading = false; btn.disabled = false; }
    }
  }

  async dismissUpdate() {
    // Viewer mode: the shared update dialog is being reused by Settings >
    // View Release Notes, with this same button relabeled "Close" (see
    // settings.js). In that mode there may be no pending update at all, and
    // even if there is, "Close" must NOT skip it — just hide the dialog.
    const dlg = document.getElementById("update-dialog");
    if (dlg?.dataset.viewerMode === "true") {
      dlg.hide();
      return;
    }
    try {
      const resp = await fetch("/api/update/dismiss", this._shownRequest());
      if (!resp.ok) {
        const data = await resp.json().catch(() => ({ error: resp.statusText }));
        if (resp.status === 409 && data.pending) {
          this._showPendingInstead(data, "skipped");
          return;
        }
        this.app.showToast("Failed to dismiss: " + (data.error || "Unknown error"), "danger");
        return;
      }
      // The release the server skipped, which is the one on screen; the badge
      // drops only if it still shows that one, by update_cleared's rule.
      const data = await resp.json().catch(() => ({}));
      const skipped = data.skipped || this.shown?.tagName || "this version";
      if (this.available?.tagName === skipped) this.available = null;
      this.updateVersionIndicator();
      document.getElementById("update-dialog")?.hide();
      // Version-scoped skip (the old behavior disabled ALL update checks —
      // that lives in Settings > Updates now); the next release notifies.
      this.app.showToast(`Skipped ${skipped} — you'll be notified about the next release.`, "primary");
    } catch (e) {
      this.app.showToast("Failed to dismiss: " + e.message, "danger");
    }
  }
}
