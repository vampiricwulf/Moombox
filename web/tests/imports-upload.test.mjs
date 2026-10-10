// The import panel's Clear (x) stayed clickable during an upload: it hid the
// progress bar and its Cancel button while the upload kept running, and the
// result toast later arrived from nowhere. Choosing another file mid-upload
// did the same. Clear now cancels the upload, and a new file is refused
// until the running upload is cancelled or done.
//
// Same jsdom probe as app.test.mjs: an absent jsdom skips, anything else fails.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const skip = jsdomMissing || false;

const realXHR = globalThis.XMLHttpRequest;
after(() => {
  globalThis.XMLHttpRequest = realXHR;
  harness?.teardownAll();
});

/** An XHR that never answers on its own: an upload in flight. */
class PendingXHR {
  static last = null;
  constructor() {
    this.listeners = {};
    this.upload = { addEventListener() {} };
    this.aborted = false;
    PendingXHR.last = this;
  }
  open() {}
  setRequestHeader() {}
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  send() {}
  abort() {
    this.aborted = true;
    for (const fn of this.listeners.abort || []) fn();
  }
}

async function uploading(h) {
  globalThis.XMLHttpRequest = PendingXHR;
  h.app.imports.initImports();
  h.app.imports.setImportFile(new h.window.File(["zip"], "a.zip"));
  h.app.imports.uploadImport();
  assert.equal(h.app.imports.importUploading, true, "precondition: uploading");
  assert.equal(h.el("import-progress").style.display, "", "precondition: progress shown");
}

// Mutant: drop the cancelUpload call in clearImportFile — the XHR is never
// aborted and importUploading stays true.
test("clearing the file cancels the upload in flight", { skip }, async () => {
  const h = await harness.makeApp();
  await uploading(h);
  h.el("import-clear-btn").click();

  assert.equal(PendingXHR.last.aborted, true, "the upload was left running");
  assert.equal(h.app.imports.importUploading, false);
  assert.equal(h.app.imports._activeXhr, null);
  assert.equal(h.app.imports.importFile, null);
});

// Mutant: drop the importUploading guard in setImportFile — the progress
// (and its Cancel button) is hidden mid-upload.
test("choosing another file mid-upload is refused, not swapped in", { skip }, async () => {
  const h = await harness.makeApp();
  await uploading(h);
  const before = h.app.imports.importFile;
  h.app.imports.setImportFile(new h.window.File(["zip"], "b.zip"));

  assert.equal(h.app.imports.importFile, before, "the file being uploaded was replaced");
  assert.equal(h.el("import-progress").style.display, "", "the progress and its Cancel button were hidden");
  assert.equal(PendingXHR.last.aborted, false);
  assert.ok(h.toasts().some((t) => /cancel it before choosing another file/.test(t.textContent)));
});

/** An XHR the server answers as soon as it is sent. */
function answering(status, body) {
  return class AnsweringXHR extends PendingXHR {
    send() {
      this.status = status;
      this.responseText = JSON.stringify(body);
      for (const fn of this.listeners.load || []) fn();
    }
  };
}

async function importAnswered(h, status, body) {
  globalThis.XMLHttpRequest = answering(status, body);
  h.app.imports.initImports();
  h.app.imports.setImportFile(new h.window.File(["zip"], "a.zip"));
  h.app.imports.uploadImport();
}

// W25-01: a re-import that re-adopted the identical files a deleted row left
// in imports/, or took " (2)" beside a different file, says so — in a toast
// and in the status line, which stays until the next file instead of going
// with the 1.5 s reset.
//
// Mutants: ignoring `import.note` (the plain "Import complete!" and its
// reset); keeping the 1.5 s reset for a note (the status line is cleared);
// a success toast for a rename; leaving the submit button shown.
test("an import's outcome note is shown and kept", { skip }, async () => {
  const h = await harness.makeApp();
  const note = 'imports/ already held a different "Stream [dQw4w9WgXcQ].mp4"; imported as "Stream [dQw4w9WgXcQ] (2).mp4"';
  await importAnswered(h, 201, {
    id: "dQw4w9WgXcQ",
    title: "Stream",
    import: { renamed: [{ from: "Stream [dQw4w9WgXcQ].mp4", to: "Stream [dQw4w9WgXcQ] (2).mp4" }], note },
  });

  assert.ok(h.toasts().some((t) => t.textContent.includes(note) && t.variant === "warning"), "no warning toast carries the note");
  h.advance(5000);
  assert.equal(h.el("import-progress").style.display, "", "the status line was reset away");
  assert.ok(h.el("import-status-text").textContent.includes(note), h.el("import-status-text").textContent);
  assert.equal(h.el("import-submit-btn").style.display, "none", "the same archive can be sent again");
});

// A plain import keeps its old behaviour: a success toast and the reset.
test("an import with nothing to report resets as before", { skip }, async () => {
  const h = await harness.makeApp();
  await importAnswered(h, 201, { id: "dQw4w9WgXcQ", title: "Stream", import: {} });

  assert.ok(h.toasts().some((t) => /Archive imported successfully/.test(t.textContent)));
  h.advance(1500);
  assert.equal(h.el("import-progress").style.display, "none", "the form was not reset");
});

// W25-04: a refusal names its reason — the zip's videos, for one holding
// more than one recording — in the status line and the toast.
test("a refused import shows the server's reason", { skip }, async () => {
  const h = await harness.makeApp();
  const reason = "the zip holds more than one recording: A.mp4, B.mp4 — import one recording per zip";
  await importAnswered(h, 400, { error: reason });

  assert.equal(h.el("import-status-text").textContent, reason);
  assert.ok(h.toasts().some((t) => t.textContent.includes(reason) && t.variant === "danger"));
});
