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
