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
  }

  initImports() {
    this.importInitialized = true;

    const dropzone = document.getElementById("import-dropzone");
    const fileInput = document.getElementById("import-file-input");
    const submitBtn = document.getElementById("import-submit-btn");
    const clearBtn = document.getElementById("import-clear-btn");

    // Click dropzone to browse
    dropzone.addEventListener("click", () => fileInput.click());

    // File input change
    fileInput.addEventListener("change", () => {
      if (fileInput.files.length > 0) {
        this.setImportFile(fileInput.files[0]);
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
      if (files.length > 0 && files[0].name.toLowerCase().endsWith(".zip")) {
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
    if (this._activeXhr) {
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
    this.importFile = file;

    const dropzone = document.getElementById("import-dropzone");
    const fileInfo = document.getElementById("import-file-info");
    const fileName = document.getElementById("import-file-name");
    const options = document.getElementById("import-options");
    const submitBtn = document.getElementById("import-submit-btn");

    dropzone.classList.add("has-file");
    fileInfo.style.display = "";
    fileName.textContent = `${file.name} (${formatBytes(file.size)})`;
    options.style.display = "";
    submitBtn.style.display = "";
  }

  clearImportFile() {
    this.importFile = null;

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
    xhr.open("POST", "/api/import");
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    if (title) xhr.setRequestHeader("X-Import-Title", title);
    if (channel) xhr.setRequestHeader("X-Import-Channel", channel);

    xhr.upload.addEventListener("progress", (e) => {
      if (e.lengthComputable) {
        const pct = Math.round((e.loaded / e.total) * 100);
        progressBar.value = pct;
        statusText.textContent = `Uploading... ${pct}% (${formatBytes(e.loaded)} / ${formatBytes(e.total)})`;
      }
    });

    this._activeXhr = xhr;

    xhr.addEventListener("load", () => {
      this._activeXhr = null;
      this.importUploading = false;
      submitBtn.disabled = false;
      submitBtn.loading = false;
      this._hideCancelButton();

      if (xhr.status === 201) {
        statusText.textContent = "Import complete!";
        progressBar.value = 100;
        this.app.showToast("Archive imported successfully", "success");

        // Reset form after delay
        setTimeout(() => this.clearImportFile(), 1500);

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
