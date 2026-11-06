// Import chapters from a pasted timestamp comment (owner decisions CI-3,
// CI-4, CI-5): the player's "Import chapters" dialog.
//
// There is no parser here. The live preview asks the server
// (POST /api/jobs/{id}/chapters/preview), which runs the one parser
// (utils.ParseChapterComment) and the one offset reader
// (utils.ParseChapterOffset) — the offset field's text goes up as typed — so
// what the table shows is exactly what Save writes. Save is
// POST /api/jobs/{id}/chapters, Restore original
// POST /api/jobs/{id}/chapters/restore; after either the player reloads its
// chapters from GET /api/jobs/{id}/chapters.
import { formatTimestamp, serverErrorMessage } from "./utils.js";

/**
 * How long the preview waits after the last keystroke before it asks the
 * server: a paste is one input event, typing is many, and each preview probes
 * the archive's length.
 */
export const CHAPTER_PREVIEW_DEBOUNCE_MS = 300;

export class ChapterImportDialog {
  /**
   * @param {object} player the PlayerController: its open job, its toast
   *   (player.app.showToast) and the chapter reload (_loadChapters).
   */
  constructor(player) {
    this.player = player;
    this.jobId = null;
    this._timer = null;
    this._seq = 0;
    this._previewOk = false;
    this._busy = false;
  }

  _el(id) {
    return document.getElementById(id);
  }

  /** Wire the dialog's controls once (PlayerController.initPlayer). */
  init() {
    this._el("player-chapters-import")?.addEventListener("click", () => {
      if (this.player.playerJob) this.open(this.player.playerJob.id);
    });
    const schedule = () => this._schedulePreview();
    this._el("chapter-import-text")?.addEventListener("sl-input", schedule);
    this._el("chapter-import-text")?.addEventListener("input", schedule);
    this._el("chapter-import-offset")?.addEventListener("sl-input", schedule);
    this._el("chapter-import-offset")?.addEventListener("input", schedule);
    this._el("chapter-import-save")?.addEventListener("click", () => this.save());
    this._el("chapter-import-restore")?.addEventListener("click", () => this.restore());
    this._el("chapter-import-cancel")?.addEventListener("click", () => this.close());
  }

  /**
   * Open the dialog for a job: an empty paste, and a first preview that
   * brings the job's current chapters, whether an original can be restored,
   * and the offset the field starts at.
   */
  async open(jobId) {
    clearTimeout(this._timer);
    this._timer = null;
    this.jobId = jobId;
    this._previewOk = false;
    this._el("chapter-import-text").value = "";
    this._el("chapter-import-offset").value = "";
    this._renderPreview(null, "");
    this._setSaveEnabled();
    const dialog = this._el("chapter-import-dialog");
    if (dialog) dialog.open = true;
    const view = await this._preview({ initial: true });
    if (view && this._el("chapter-import-offset").value === "") {
      const def = Number(view.defaultOffsetSeconds) || 0;
      if (def !== 0) this._el("chapter-import-offset").value = String(def);
    }
  }

  close() {
    clearTimeout(this._timer);
    this._timer = null;
    this._seq++;
    const dialog = this._el("chapter-import-dialog");
    if (dialog) dialog.open = false;
  }

  _schedulePreview() {
    clearTimeout(this._timer);
    this._timer = setTimeout(() => {
      this._timer = null;
      this._preview();
    }, CHAPTER_PREVIEW_DEBOUNCE_MS);
  }

  _body() {
    return JSON.stringify({
      text: this._el("chapter-import-text").value || "",
      offsetSeconds: this._el("chapter-import-offset").value || "",
    });
  }

  /**
   * Ask the server to parse the paste. Only the newest answer is drawn: an
   * older one landing late (a slow probe) must not replace it.
   */
  async _preview({ initial = false } = {}) {
    const jobId = this.jobId;
    const seq = ++this._seq;
    let res;
    try {
      res = await fetch(`/api/jobs/${jobId}/chapters/preview`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: this._body(),
      });
    } catch {
      if (seq === this._seq) this._status("Could not reach the server");
      return null;
    }
    if (seq !== this._seq) return null;
    if (!res.ok) {
      this._previewOk = false;
      this._setSaveEnabled();
      this._status(await serverErrorMessage(res));
      return null;
    }
    const view = await res.json();
    if (seq !== this._seq) return null;
    const empty = !this._el("chapter-import-text").value;
    // The opening preview has no paste yet: its "No timestamps found" is not
    // news, so it shows the prompt instead.
    this._renderPreview(view, initial || empty ? "Paste a timestamp comment to preview its chapters" : view.error || "");
    this._previewOk = !view.error && Array.isArray(view.chapters) && view.chapters.length > 0;
    this._setSaveEnabled();
    return view;
  }

  _status(text) {
    const el = this._el("chapter-import-status");
    if (el) el.textContent = text;
  }

  _setSaveEnabled() {
    const save = this._el("chapter-import-save");
    if (save) save.disabled = this._busy || !this._previewOk;
  }

  /** Draw the preview table, the current chapters and the restore button. */
  _renderPreview(view, status) {
    this._status(status);
    const table = this._el("chapter-import-preview");
    const body = table?.querySelector("tbody");
    const list = Array.isArray(view?.chapters) ? view.chapters : [];
    if (body) {
      body.replaceChildren(...list.map((c) => {
        const tr = document.createElement("tr");
        const at = document.createElement("td");
        at.className = "chapter-import-start";
        at.textContent = formatTimestamp(c.start);
        const title = document.createElement("td");
        title.textContent = c.title || "";
        tr.append(at, title);
        return tr;
      }));
    }
    if (table) table.hidden = list.length === 0;

    const current = Array.isArray(view?.current) ? view.current : [];
    const details = this._el("chapter-import-current");
    const ol = this._el("chapter-import-current-list");
    if (ol) {
      ol.replaceChildren(...current.map((c) => {
        const li = document.createElement("li");
        li.textContent = `${formatTimestamp(c.start)} ${c.title || ""}`.trim();
        return li;
      }));
    }
    if (details) {
      details.hidden = current.length === 0;
      details.setAttribute("summary", `Current chapters (${current.length}) — replaced on save`);
      details.open = false;
    }
    const restore = this._el("chapter-import-restore");
    if (restore) restore.hidden = !view?.hasOriginal;
  }

  async save() {
    if (!this._previewOk) return;
    await this._commit(`/api/jobs/${this.jobId}/chapters`, this._body(), "Chapters imported");
  }

  async restore() {
    await this._commit(`/api/jobs/${this.jobId}/chapters/restore`, null, "Original chapters restored");
  }

  /**
   * Save or restore, then reload the player's chapters. A re-mux that could
   * not embed the list (embedError) is still a save — the list is kept beside
   * the archive, which the player reads — and the toast says so.
   *
   * The player's own source for the job is let go for the save and assigned
   * again after it, at the same position and play state: a re-mux replaces
   * the archive behind the same /video URL with a different byte layout (the
   * chapter track resizes the moov, and every sample offset moves), so an
   * element that kept the old file's sample table would read the new file's
   * bytes at the old offsets; and on Windows a file the player is streaming
   * cannot be replaced, so a save that left the source in place would fall
   * back to the sidecar.
   */
  async _commit(url, body, done) {
    const jobId = this.jobId;
    this._busy = true;
    this._setSaveEnabled();
    this._status("Saving…");
    const held = this._releaseSource(jobId);
    try {
      await this._send(url, body, done, jobId);
    } finally {
      this._reattachSource(jobId, held);
    }
  }

  /**
   * Let go of the player's single-file source for jobId: its position and
   * whether it played, or null when the player is not playing that job's
   * archive as one file (another job, or a split archive's parts, which an
   * import never rewrites).
   */
  _releaseSource(jobId) {
    const video = this._el("player-video");
    if (!video || this.player.playerJob?.id !== jobId || this.player._seg?.active || !video.getAttribute("src")) return null;
    const held = { time: video.currentTime, paused: video.paused };
    video.pause();
    video.removeAttribute("src");
    video.load();
    return held;
  }

  /**
   * Assign the source released by _releaseSource again — the archive as it
   * now is — and put back the position and play state once its metadata is
   * in. Not when the player has since moved on (another job selected, or
   * this one reopened: it assigned a source of its own).
   */
  _reattachSource(jobId, held) {
    const video = this._el("player-video");
    if (!held || !video || this.player.playerJob?.id !== jobId || video.getAttribute("src")) return;
    video.addEventListener("loadedmetadata", () => {
      video.currentTime = held.time;
      if (!held.paused) video.play()?.catch?.(() => {});
    }, { once: true });
    video.src = `/api/jobs/${jobId}/video`;
  }

  async _send(url, body, done, jobId) {
    let res;
    try {
      res = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: body ?? "{}",
      });
    } catch {
      this._busy = false;
      this._setSaveEnabled();
      this._status("Could not reach the server");
      return;
    }
    this._busy = false;
    if (!res.ok) {
      this._setSaveEnabled();
      this._status(await serverErrorMessage(res));
      return;
    }
    const view = await res.json();
    this.close();
    const app = this.player.app;
    if (view.embedError) {
      app?.showToast?.(`${done}; the file could not be rewritten, so they are saved beside it: ${view.embedError}`, "warning");
    } else {
      app?.showToast?.(done, "success");
    }
    if (this.player.playerJob?.id === jobId) {
      this.player._loadChapters(jobId, this.player._selectionSeq);
    }
  }
}
