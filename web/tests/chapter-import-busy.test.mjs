// K35: the chapter-import dialog's committing state. Restore was neither
// disabled nor checked during a Save, so a second commit could start and its
// 409 re-enabled Save mid-save; and Escape hid the dialog without close(),
// so a failure of the commit it interrupted went to the hidden dialog's
// status line and was never seen. Owner ruling R-B1: Esc during a save still
// closes the dialog, and the save's failure is reported as a toast.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/player-dom.mjs");
const { CHAPTER_PREVIEW_DEBOUNCE_MS } = await import("../public/modules/chapter-import.js");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const finished = (id) => ({
  id, status: "Finished", filename: `${id}.mp4`, title: `Title ${id}`,
  channelName: "Chan", updatedAt: "2026-09-01T00:00:00Z",
});

const view = (extra = {}) => ({
  chapters: [], current: [], hasOriginal: false, offsetSeconds: 0,
  defaultOffsetSeconds: 0, durationSeconds: 600, split: false, embedded: false, ...extra,
});

/**
 * The dialog open on job "v" with a saveable paste, an original to restore,
 * and a Save whose answer waits for gate.release(answer).
 */
async function readyToSave() {
  const gate = {};
  const h = harness.makePlayer({ jobs: [finished("v")] });
  h.http.on("GET /api/jobs/:id/chapters", () => ({ chapters: [] }));
  h.http.on("POST /api/jobs/:id/chapters/preview", ({ body }) => view({
    hasOriginal: true, originalCount: 2,
    chapters: body.text ? [{ start: 0, end: 600, title: "new" }] : [],
    error: body.text ? "" : "No timestamps found",
  }));
  h.http.on("POST /api/jobs/:id/chapters", () => new Promise((resolve) => { gate.release = resolve; }));
  await h.selectJob("v");
  h.el("player-chapters-import").click();
  await h.flush();
  h.el("chapter-import-text").value = "0:00 new";
  h.el("chapter-import-text").dispatchEvent(new h.window.Event("sl-input", { bubbles: true }));
  h.advance(CHAPTER_PREVIEW_DEBOUNCE_MS);
  await h.flush();
  assert.equal(h.el("chapter-import-save").disabled, false);
  h.app.confirmAnswer = true;
  return { h, gate };
}

// MUTANT: restore() without the _busy check, or restore.disabled not
// following it — a second commit starts during the Save.
test("Restore is disabled and refused while a Save is committing", { skip }, async () => {
  const { h, gate } = await readyToSave();
  let restores = 0;
  h.http.on("POST /api/jobs/:id/chapters/restore", () => { restores++; return { __response: true, status: 409, body: { error: "busy" } }; });

  h.el("chapter-import-save").click();
  await h.flush();
  assert.equal(h.el("chapter-import-save").disabled, true);
  assert.equal(h.el("chapter-import-restore").disabled, true, "Restore stayed enabled during a Save");

  h.el("chapter-import-restore").click();
  await h.flush(); await h.flush();
  assert.equal(restores, 0, "a restore was sent during the Save");
  assert.equal(h.el("chapter-import-save").disabled, true, "Save re-enabled mid-save");
  assert.equal(h.el("chapter-import-status").textContent, "Saving…");

  gate.release(view({ chapters: [{ start: 0, end: 600, title: "new" }], embedded: true, hasOriginal: true }));
  await h.flush(); await h.flush();
  assert.equal(h.el("chapter-import-restore").disabled, false);
});

// MUTANT: no sl-request-close listener — Escape hides the dialog, the debounce
// timer still fires a preview into it.
test("Escape goes through close(): no preview fires into the hidden dialog", { skip }, async () => {
  const { h } = await readyToSave();
  h.el("chapter-import-text").value = "0:00 newer";
  h.el("chapter-import-text").dispatchEvent(new h.window.Event("sl-input", { bubbles: true }));
  const before = h.http.matching("/chapters/preview", "POST").length;
  h.el("chapter-import-dialog").dispatchEvent(new h.window.CustomEvent("sl-request-close", { detail: { source: "keyboard" } }));
  assert.equal(h.el("chapter-import-dialog").open, false);
  h.advance(CHAPTER_PREVIEW_DEBOUNCE_MS);
  await h.flush();
  assert.equal(h.http.matching("/chapters/preview", "POST").length, before, "a preview fired after Escape");
});

// R-B1. MUTANT: the failure written only to the hidden dialog's status line.
test("Escape during a Save closes the dialog, and the Save's failure is a toast", { skip }, async () => {
  const { h, gate } = await readyToSave();
  h.el("chapter-import-save").click();
  await h.flush();
  h.el("chapter-import-dialog").dispatchEvent(new h.window.CustomEvent("sl-request-close", { detail: { source: "keyboard" } }));
  assert.equal(h.el("chapter-import-dialog").open, false, "Escape during the save did not close the dialog");

  gate.release({ __response: true, status: 500, body: { error: "re-mux failed" } });
  await h.flush(); await h.flush();
  const last = h.app.toasts.at(-1);
  assert.equal(last?.variant, "danger", `no failure toast: ${JSON.stringify(h.app.toasts)}`);
  assert.match(last.message, /re-mux failed/);
});

// A dialog reopened while the dismissed save was still running is not closed
// by that save's success.
test("a save that ends after the dialog was reopened leaves the new dialog open", { skip }, async () => {
  const { h, gate } = await readyToSave();
  h.el("chapter-import-save").click();
  await h.flush();
  h.el("chapter-import-dialog").dispatchEvent(new h.window.CustomEvent("sl-request-close", { detail: { source: "keyboard" } }));
  h.el("player-chapters-import").click();
  await h.flush();
  assert.equal(h.el("chapter-import-dialog").open, true);

  gate.release(view({ chapters: [{ start: 0, end: 600, title: "new" }], embedded: true, hasOriginal: true }));
  await h.flush(); await h.flush();
  assert.equal(h.el("chapter-import-dialog").open, true, "the old save closed the reopened dialog");
  assert.deepEqual(h.app.toasts.at(-1), { message: "Chapters imported", variant: "success" });
});

// A Save clicked while Restore's confirm is up: the guard ahead of the confirm
// passed before the Save began, so the re-check after it is what keeps the
// restore out. MUTANT: no re-check after the confirm — a restore is sent
// during the Save.
test("a Restore confirmed after a Save began is not sent", { skip }, async () => {
  const { h, gate } = await readyToSave();
  let restores = 0;
  h.http.on("POST /api/jobs/:id/chapters/restore", () => { restores++; return view({ hasOriginal: false }); });
  let answer;
  h.app.showConfirm = () => new Promise((resolve) => { answer = resolve; });

  h.el("chapter-import-restore").click();
  await h.flush();
  assert.ok(answer, "the Restore confirm did not open");
  h.el("chapter-import-save").click();
  await h.flush();
  answer(true);
  await h.flush(); await h.flush();
  assert.equal(restores, 0, "a restore was sent during the Save");
  assert.equal(h.el("chapter-import-status").textContent, "Saving…");

  gate.release(view({ chapters: [{ start: 0, end: 600, title: "new" }], embedded: true, hasOriginal: true }));
  await h.flush(); await h.flush();
});
