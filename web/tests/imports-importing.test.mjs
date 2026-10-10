// Once an import's body was all sent, the server was extracting it — and the
// panel still read "Uploading... 100%" with Cancel offered. Cancel aborted the
// XHR and toasted "Upload cancelled" while the server went on to create the
// job, so the retry met "job already exists". From xhr.upload's "load" until
// the response the panel now says "Importing…" and offers no way to give the
// import up: Cancel is hidden and inert, Clear and another file wait.
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

/** An XHR the test drives: the body going out, then the response. */
class DrivenXHR {
  static all = [];
  constructor() {
    this.listeners = {};
    this.uploadListeners = {};
    this.upload = { addEventListener: (t, fn) => { (this.uploadListeners[t] ||= []).push(fn); } };
    this.aborted = false;
    this.status = 0;
    this.responseText = "";
    DrivenXHR.all.push(this);
  }
  open() {}
  setRequestHeader() {}
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  send() {}
  abort() {
    this.aborted = true;
    for (const fn of this.listeners.abort || []) fn();
  }
  /** The whole body has gone out: progress at 100%, then upload "load". */
  bodySent(total = 3) {
    for (const fn of this.uploadListeners.progress || []) fn({ lengthComputable: true, loaded: total, total });
    for (const fn of this.uploadListeners.load || []) fn({});
  }
  respond(status, body) {
    this.status = status;
    this.responseText = JSON.stringify(body);
    for (const fn of this.listeners.load || []) fn();
  }
}

async function startUpload(h, name = "a.zip") {
  globalThis.XMLHttpRequest = DrivenXHR;
  if (!h.app.imports.importInitialized) h.app.imports.initImports();
  h.app.imports.setImportFile(new h.window.File(["zip"], name));
  h.app.imports.uploadImport();
  return DrivenXHR.all.at(-1);
}

const cancelShown = (h) => {
  const btn = h.el("import-cancel-btn");
  return !!btn && btn.style.display !== "none";
};
const toastTexts = (h) => h.toasts().map((t) => t.textContent.trim());

// Mutant: dropping _hideCancelButton() from the upload "load" listener (Cancel
// is still offered), dropping its status line (still "Uploading... 100%"), or
// dropping the !this._importing() guard in cancelUpload (the hidden Cancel,
// clicked, aborts and toasts "Upload cancelled").
test("once the body is sent, Cancel is gone and the panel says Importing…", { skip }, async () => {
  const h = await harness.makeApp();
  const xhr = await startUpload(h);
  assert.equal(cancelShown(h), true, "precondition: Cancel offered while uploading");

  xhr.bodySent();
  assert.equal(h.el("import-status-text").textContent, "Importing…");
  assert.equal(cancelShown(h), false, "Cancel is still offered while the server imports");

  h.el("import-cancel-btn").click();
  assert.equal(xhr.aborted, false, "a Cancel after the body went out aborted the import");
  assert.ok(!toastTexts(h).some((t) => /cancelled/i.test(t)), `toasts: ${JSON.stringify(toastTexts(h))}`);
  assert.equal(h.app.imports.importUploading, true);
});

// Mutant: dropping the _importing() guard in clearImportFile (Clear aborts
// the import and toasts "Upload cancelled"), or setImportFile always naming
// Cancel (which is not there).
test("Clear and another file wait for the import instead of giving it up", { skip }, async () => {
  const h = await harness.makeApp();
  const xhr = await startUpload(h);
  xhr.bodySent();
  const file = h.app.imports.importFile;

  h.el("import-clear-btn").click();
  assert.equal(xhr.aborted, false, "Clear aborted an import the server was already making");
  assert.equal(h.app.imports.importFile, file, "Clear dropped the file being imported");
  assert.ok(toastTexts(h).some((t) => /being imported — wait for it to finish/.test(t)), `toasts: ${JSON.stringify(toastTexts(h))}`);
  assert.ok(!toastTexts(h).some((t) => /cancelled/i.test(t)));

  h.app.imports.setImportFile(new h.window.File(["zip"], "b.zip"));
  assert.equal(h.app.imports.importFile, file);
  assert.ok(toastTexts(h).some((t) => /being imported — wait for it to finish before choosing another file/.test(t)));
  assert.ok(!toastTexts(h).some((t) => /cancel it before/.test(t)), "the refusal names a Cancel that is not there");
});

// Mutant: _importing() reading _bodySent alone (Clear is still refused after
// the response), or uploadImport not resetting _bodySent (the next upload's
// Cancel is inert from its first byte).
test("the response ends the import: Clear works, and the next upload can be cancelled", { skip }, async () => {
  const h = await harness.makeApp();
  const first = await startUpload(h);
  first.bodySent();
  first.respond(409, { error: "job already exists for video ID: dQw4w9WgXcQ" });
  assert.equal(h.el("import-status-text").textContent, "job already exists for video ID: dQw4w9WgXcQ");

  h.el("import-clear-btn").click();
  assert.equal(h.app.imports.importFile, null, "Clear was refused with no import running");

  const second = await startUpload(h, "b.zip");
  assert.equal(cancelShown(h), true, "Cancel not offered for the next upload");
  h.el("import-cancel-btn").click();
  assert.equal(second.aborted, true, "the next upload's Cancel did nothing");
});

// Mutant: dropping the `this._activeXhr !== xhr` check in the upload "load"
// listener — a cancelled upload's late event hides the running upload's
// Cancel and calls it Importing.
test("a cancelled upload's late upload event leaves the running upload alone", { skip }, async () => {
  const h = await harness.makeApp();
  const stale = await startUpload(h);
  h.el("import-cancel-btn").click();
  assert.equal(stale.aborted, true, "precondition: the first upload was cancelled");

  await startUpload(h, "b.zip");
  stale.bodySent();
  assert.equal(cancelShown(h), true, "a stale event hid the running upload's Cancel");
  assert.notEqual(h.el("import-status-text").textContent, "Importing…");
});

// The upload "progress" listener had no such check: a cancelled upload's late
// progress event wrote its own percentage over the running upload's bar and
// status line.
//
// Mutant: dropping the `this._activeXhr !== xhr` check in the upload
// "progress" listener — the stale 90% replaces the running upload's 10%.
test("a cancelled upload's late progress event leaves the running upload's progress alone", { skip }, async () => {
  const h = await harness.makeApp();
  const stale = await startUpload(h);
  h.el("import-cancel-btn").click();
  assert.equal(stale.aborted, true, "precondition: the first upload was cancelled");

  const running = await startUpload(h, "b.zip");
  for (const fn of running.uploadListeners.progress || []) fn({ lengthComputable: true, loaded: 1, total: 10 });
  const shown = h.el("import-status-text").textContent;
  assert.match(shown, /^Uploading\.\.\. 10% /, "precondition: the running upload's progress is shown");

  for (const fn of stale.uploadListeners.progress || []) fn({ lengthComputable: true, loaded: 9, total: 10 });
  assert.equal(h.el("import-status-text").textContent, shown, "a stale progress event rewrote the running upload's status line");
  assert.equal(h.el("import-progress-bar").value, 10, "a stale progress event moved the running upload's bar");
});
