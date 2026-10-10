/**
 * Import Controller — ZIP archive import UI
 */
import { formatBytes } from "./utils.js";

export class ImportController {
  constructor(app) {
    this.app = app;
    this.importFile = null;
    this.importUploading = false;
    this.importInitialized = false;
    this._activeXhr = null;
    this._clearTimeout = null;
    this._bodySent = false;
  }

  // The whole archive has gone out and the server is extracting it: the
  // stretch between xhr.upload's "load" and the response. Nothing can be
  // cancelled any more — see uploadImport.
  _importing() {
    return this._activeXhr !== null && this._bodySent;
  }

  initImports() {
    this.importInitialized = true;

    const dropzone = document.getElementById("import-dropzone");
    const fileInput = document.getElementById("import-file-input");
    const submitBtn = document.getElementById("import-submit-btn");
    const clearBtn = document.getElementById("import-clear-btn");

    // Click, Enter or Space on the dropzone browses — one `browse` for both
    // input paths. The dropzone is a div (role="button" in the markup), so
    // the key half is ours to provide.
    const browse = () => fileInput.click();
    dropzone.addEventListener("click", browse);
    dropzone.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); browse(); }
    });

    // File input change
    fileInput.addEventListener("change", () => {
      if (fileInput.files.length > 0) {
        if (fileInput.files[0].name.toLowerCase().endsWith(".zip")) {
          this.setImportFile(fileInput.files[0]);
        } else {
          this.app.showToast("Please select a .zip file", "warning");
          fileInput.value = "";
        }
      }
    });

    // Drag & drop
    dropzone.addEventListener("dragover", (e) => {
      e.preventDefault();
      dropzone.classList.add("drag-over");
    });

    dropzone.addEventListener("dragleave", () => {
      dropzone.classList.remove("drag-over");
    });

    dropzone.addEventListener("drop", (e) => {
      e.preventDefault();
      dropzone.classList.remove("drag-over");
      const files = e.dataTransfer.files;
      if (files.length === 0) return; // Non-file drop (text, image, etc.)
      if (files[0].name.toLowerCase().endsWith(".zip")) {
        this.setImportFile(files[0]);
      } else {
        this.app.showToast("Please drop a .zip file", "warning");
      }
    });

    // Clear button
    clearBtn.addEventListener("click", () => this.clearImportFile());

    // Submit
    submitBtn.addEventListener("click", () => this.uploadImport());
  }

  cancelUpload() {
    if (this._activeXhr && !this._importing()) {
      this._activeXhr.abort();
      this._activeXhr = null;
      this.importUploading = false;
      const submitBtn = document.getElementById("import-submit-btn");
      if (submitBtn) { submitBtn.disabled = false; submitBtn.loading = false; }
      const statusText = document.getElementById("import-status-text");
      if (statusText) statusText.textContent = "Upload cancelled";
      this._hideCancelButton();
      this.app.showToast("Upload cancelled", "primary");
    }
  }

  _showCancelButton() {
    let cancelBtn = document.getElementById("import-cancel-btn");
    if (!cancelBtn) {
      cancelBtn = document.createElement("sl-button");
      cancelBtn.id = "import-cancel-btn";
      cancelBtn.variant = "danger";
      cancelBtn.size = "small";
      cancelBtn.textContent = "Cancel";
      cancelBtn.style.marginTop = "var(--sl-spacing-x-small)";
      cancelBtn.addEventListener("click", () => this.cancelUpload());
      const progress = document.getElementById("import-progress");
      if (progress) progress.appendChild(cancelBtn);
    }
    cancelBtn.style.display = "";
  }

  _hideCancelButton() {
    const cancelBtn = document.getElementById("import-cancel-btn");
    if (cancelBtn) cancelBtn.style.display = "none";
  }

  setImportFile(file) {
    // Swapping the file mid-upload hid the progress bar and its Cancel button
    // while the old file kept uploading.
    if (this.importUploading) {
      this.app.showToast(this._importing()
        ? "The archive is being imported — wait for it to finish before choosing another file"
        : "An import is uploading — cancel it before choosing another file", "warning");
      const fileInput = document.getElementById("import-file-input");
      if (fileInput) fileInput.value = "";
      return;
    }
    this.importFile = file;

    // Cancel any pending clear timeout from a previous completed upload
    // to prevent it from wiping this new selection
    if (this._clearTimeout) {
      clearTimeout(this._clearTimeout);
      this._clearTimeout = null;
    }

    const dropzone = document.getElementById("import-dropzone");
    const fileInfo = document.getElementById("import-file-info");
    const fileName = document.getElementById("import-file-name");
    const options = document.getElementById("import-options");
    const submitBtn = document.getElementById("import-submit-btn");
    const progress = document.getElementById("import-progress");

    dropzone.classList.add("has-file");
    fileInfo.style.display = "";
    fileName.textContent = `${file.name} (${formatBytes(file.size)})`;
    options.style.display = "";
    submitBtn.style.display = "";
    // Hide stale progress from a previous upload
    if (progress) progress.style.display = "none";
  }

  clearImportFile() {
    // Once the body is all sent there is no giving up on it from here — the
    // server is importing it — so Clear, like the hidden Cancel, waits.
    if (this._importing()) {
      this.app.showToast("The archive is being imported — wait for it to finish", "warning");
      return;
    }
    // Clearing the file is giving up on it: an upload still running would
    // otherwise go on with its progress and Cancel button hidden, then toast
    // a result out of nowhere.
    if (this._activeXhr) this.cancelUpload();
    this.importFile = null;

    // Cancel any pending auto-clear timeout (e.g. from completed upload)
    if (this._clearTimeout) {
      clearTimeout(this._clearTimeout);
      this._clearTimeout = null;
    }

    const dropzone = document.getElementById("import-dropzone");
    const fileInput = document.getElementById("import-file-input");
    const fileInfo = document.getElementById("import-file-info");
    const options = document.getElementById("import-options");
    const submitBtn = document.getElementById("import-submit-btn");
    const progress = document.getElementById("import-progress");

    dropzone.classList.remove("has-file");
    fileInfo.style.display = "none";
    options.style.display = "none";
    submitBtn.style.display = "none";
    progress.style.display = "none";
    fileInput.value = "";

    document.getElementById("import-title").value = "";
    document.getElementById("import-channel").value = "";
  }

  uploadImport() {
    if (!this.importFile || this.importUploading) return;

    this.importUploading = true;
    this._bodySent = false;

    const submitBtn = document.getElementById("import-submit-btn");
    const progress = document.getElementById("import-progress");
    const progressBar = document.getElementById("import-progress-bar");
    const statusText = document.getElementById("import-status-text");

    submitBtn.disabled = true;
    submitBtn.loading = true;
    progress.style.display = "";
    progressBar.value = 0;
    statusText.textContent = "Uploading...";
    this._showCancelButton();

    const title = document.getElementById("import-title").value.trim();
    const channel = document.getElementById("import-channel").value.trim();

    const xhr = new XMLHttpRequest();
    try {
      xhr.open("POST", "/api/import");
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      // HTTP headers are Latin-1 only — setRequestHeader throws synchronously
      // for CJK/emoji titles and mangles é-style chars. Percent-encode; the
      // server decodes (url.PathUnescape in import_routes.go — PathUnescape,
      // not QueryUnescape, so a literal '+' survives).
      if (title) xhr.setRequestHeader("X-Import-Title", encodeURIComponent(title));
      if (channel) xhr.setRequestHeader("X-Import-Channel", encodeURIComponent(channel));
    } catch (e) {
      // Reset upload state — a throw here would otherwise wedge the tab
      // (button stuck loading, cancel a no-op) until reload.
      this.importUploading = false;
      submitBtn.disabled = false;
      submitBtn.loading = false;
      this._hideCancelButton();
      statusText.textContent = "Upload failed";
      this.app.showToast("Upload failed: " + e.message, "danger");
      return;
    }

    // A cancelled upload's late event must not write over the running one's
    // bar and status line — the same check as the upload "load" below.
    xhr.upload.addEventListener("progress", (e) => {
      if (this._activeXhr !== xhr) return;
      if (e.lengthComputable) {
        const pct = Math.round((e.loaded / e.total) * 100);
        progressBar.value = pct;
        statusText.textContent = `Uploading... ${pct}% (${formatBytes(e.loaded)} / ${formatBytes(e.total)})`;
      }
    });

    // The body is all sent and the server is extracting the archive, which
    // for a large one takes a while. Cancel stayed offered here and toasted
    // "Upload cancelled" while the server went on to create the job, so a
    // retry then met "job already exists". Hide it and say what is happening
    // until the response arrives. (A client that goes anyway — a closed tab —
    // is caught by the server, which then removes what it extracted.)
    xhr.upload.addEventListener("load", () => {
      if (this._activeXhr !== xhr) return;
      this._bodySent = true;
      this._hideCancelButton();
      statusText.textContent = "Importing…";
    });

    this._activeXhr = xhr;

    xhr.addEventListener("load", () => {
      this._activeXhr = null;
      this.importUploading = false;
      submitBtn.disabled = false;
      submitBtn.loading = false;
      this._hideCancelButton();

      if (xhr.status === 201) {
        progressBar.value = 100;
        // `import` says what became of a name already taken in imports/
        // (import_routes.go importOutcome): a byte-identical file re-adopted,
        // or a different one left alone while this archive took " (2)" — and
        // any chat left out for matching no video's name.
        let outcome = null;
        try { outcome = JSON.parse(xhr.responseText).import || null; } catch {}
        const note = outcome && outcome.note ? outcome.note : "";
        if (note) {
          // The note stays where it can be read — under the bar, with the
          // submit hidden so the same archive is not sent again — until the
          // next file or Clear, rather than going with the 1.5 s reset.
          statusText.textContent = `Import complete — ${note}`;
          submitBtn.style.display = "none";
          // A rename, or a chat left out for matching no video's name, is a
          // warning; files re-adopted as they were are a success.
          const listed = (k) => Array.isArray(outcome[k]) && outcome[k].length > 0;
          const warn = listed("renamed") || listed("unpairedChats");
          this.app.showToast(`Archive imported — ${note}`, warn ? "warning" : "success");
        } else {
          statusText.textContent = "Import complete!";
          this.app.showToast("Archive imported successfully", "success");

          // Reset form after delay
          this._clearTimeout = setTimeout(() => {
            this._clearTimeout = null;
            this.clearImportFile();
          }, 1500);
        }

        // Refresh player job list if initialized
        if (this.app.player.playerInitialized) {
          this.app.player.loadPlayerJobList();
        }
      } else {
        let errorMsg = "Import failed";
        try {
          const data = JSON.parse(xhr.responseText);
          errorMsg = data.error || errorMsg;
        } catch {}
        statusText.textContent = errorMsg;
        this.app.showToast(errorMsg, "danger");
      }
    });

    xhr.addEventListener("error", () => {
      this._activeXhr = null;
      this.importUploading = false;
      submitBtn.disabled = false;
      submitBtn.loading = false;
      this._hideCancelButton();
      statusText.textContent = "Upload failed (network error)";
      this.app.showToast("Upload failed: network error", "danger");
    });

    // Ensure state resets on abort (triggered by cancelUpload)
    xhr.addEventListener("abort", () => {
      this._activeXhr = null;
      this.importUploading = false;
    });

    xhr.send(this.importFile);
  }
}
