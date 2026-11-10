// The dashboard's two shared dialogs have one owner at a time (K34).
//
// showConfirm: a second confirm opened while one is up — after an await, as
// reinitializeJob's preview fetch and the restart prompt after a save do —
// used to replace the open one's text and buttons under the operator's
// cursor and resolve the first caller false, silently. It now waits its
// turn and is drawn once the first is answered and hidden (sl-after-hide).
//
// The update dialog: Settings > View Release Notes answering while the
// update prompt is open used to turn the prompt into the viewer under its
// reader. The viewer now waits for the prompt to close, and the prompt for
// the viewer.
//
// jsdom is optional, as in app.test.mjs: every test skips without it.
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

after(() => harness?.teardownAll());

const shownMessage = (h) => h.el("confirm-dialog-message").textContent;
const isOpen = (h, id) => h.el(id)._open === true;

// Records each confirm's answer as it resolves.
function ask(h, message, opts) {
  const rec = { answer: undefined };
  rec.promise = h.app.showConfirm(message, opts).then((v) => { rec.answer = v; return v; });
  return rec;
}

// MUTANT: drawing every call at once (the old replace) — the first resolves
// false when the dialog hides, and the second's text is on screen first.
test("a second confirm waits for the first and is drawn after it hides", { skip }, async () => {
  const h = await harness.makeApp();
  const first = ask(h, "Delete this job?", { okLabel: "Delete", okVariant: "danger" });
  await h.flush();
  const second = ask(h, "Reinitialize this job?", { okLabel: "Reinitialize", title: "Reinitialize" });
  await h.flush();

  assert.equal(shownMessage(h), "Delete this job?", "the open confirm was replaced under the cursor");
  assert.equal(h.el("confirm-dialog-ok").textContent, "Delete");
  assert.equal(first.answer, undefined, "the first confirm was answered without the operator");

  h.el("confirm-dialog-ok").click();
  await first.promise;
  assert.equal(first.answer, true);
  assert.equal(second.answer, undefined);

  await h.flush();
  assert.ok(isOpen(h, "confirm-dialog"), "the waiting confirm was not drawn");
  assert.equal(shownMessage(h), "Reinitialize this job?");
  assert.equal(h.el("confirm-dialog-ok").textContent, "Reinitialize");
  assert.equal(h.el("confirm-dialog").label, "Reinitialize");

  h.el("confirm-dialog-cancel").click();
  await second.promise;
  assert.equal(second.answer, false);
  await h.flush();
  assert.equal(isOpen(h, "confirm-dialog"), false);
});

// Closed any other way (Esc, the close button): false, and the next one up.
test("a confirm closed without an answer resolves false and draws the next", { skip }, async () => {
  const h = await harness.makeApp();
  const first = ask(h, "first");
  const second = ask(h, "second");
  await h.flush();
  h.el("confirm-dialog").hide();
  await first.promise;
  assert.equal(first.answer, false);
  await h.flush();
  assert.equal(shownMessage(h), "second");
  assert.equal(second.answer, undefined);
  h.el("confirm-dialog-ok").click();
  assert.equal(await second.promise, true);
});

// The buttons are bound once: a double click answers the open confirm once,
// and never answers the one waiting behind it.
//
// MUTANT: answering whichever entry is the head without the answered check
// and shifting it at once — the second click answers the waiting confirm.
test("a double click answers one confirm, not the next", { skip }, async () => {
  const h = await harness.makeApp();
  const first = ask(h, "first", { okLabel: "Go" });
  const second = ask(h, "second", { okLabel: "Again" });
  await h.flush();
  h.el("confirm-dialog-ok").click();
  h.el("confirm-dialog-ok").click();
  assert.equal(await first.promise, true);
  await h.flush();
  assert.equal(shownMessage(h), "second");
  assert.equal(second.answer, undefined, "the double click answered the waiting confirm");
  assert.equal(h.el("confirm-dialog-ok").textContent, "Again");
  h.el("confirm-dialog-cancel").click();
  assert.equal(await second.promise, false);
});

// A confirm asked from a caller's continuation after OK (the restart prompt
// after a confirmed save) waits for the hide too, and is shown.
test("a confirm asked as the first resolves is shown after the hide", { skip }, async () => {
  const h = await harness.makeApp();
  let chained;
  const first = h.app.showConfirm("save?").then((ok) => {
    chained = ask(h, "restart now?");
    return ok;
  });
  await h.flush();
  h.el("confirm-dialog-ok").click();
  assert.equal(await first, true);
  await h.flush();
  assert.equal(shownMessage(h), "restart now?");
  assert.ok(isOpen(h, "confirm-dialog"));
  h.el("confirm-dialog-ok").click();
  assert.equal(await chained.promise, true);
});

// ── the update dialog ──────────────────────────────────────────────────

const release = (v) => ({
  version: v, tagName: `v${v}`, releaseNotes: `notes ${v}`,
  releaseNotesHtml: `<p>notes ${v}</p>`, publishedAt: "2026-09-01T00:00:00Z",
});
const currentNotes = { tagName: "v2.8.10", releaseNotes: "current", releaseNotesHtml: "<p>current</p>" };

async function promptOpen() {
  const h = await harness.makeApp({
    routes: { "GET /api/update/release-notes": () => currentNotes },
    initialState: { status: { version: "2.8.10", updateAvailable: release("9.9.1") } },
  });
  return h;
}

// MUTANT: showNotesViewer drawing at once — the prompt becomes the viewer.
test("the notes viewer waits for an open update prompt to close", { skip }, async () => {
  const h = await promptOpen();
  const dlg = h.el("update-dialog");
  // The fetch is out when the operator opens the update prompt.
  h.el("btn-view-release-notes").click();
  h.el("version-indicator").click();
  assert.equal(dlg.label, "Update to v9.9.1");
  await h.flush();
  await h.flush();

  assert.equal(dlg.label, "Update to v9.9.1", "the notes fetch turned the open prompt into the viewer");
  assert.equal(dlg.dataset.viewerMode, undefined);
  assert.notEqual(h.el("update-now-btn").style.display, "none", "Update Now hidden under the prompt's reader");

  dlg.hide();
  await h.flush();
  assert.equal(dlg.label, "Release Notes — v2.8.10", "the viewer was not drawn after the prompt closed");
  assert.equal(dlg.dataset.viewerMode, "true");
  assert.ok(isOpen(h, "update-dialog"));
  assert.equal(h.el("update-now-btn").style.display, "none");
  assert.equal(h.el("update-dismiss-btn").textContent, "Close");

  // Closing the viewer restores the prompt's controls.
  dlg.hide();
  await h.flush();
  assert.equal(dlg.dataset.viewerMode, undefined);
  assert.equal(h.el("update-now-btn").style.display, "");
  assert.equal(h.el("update-dismiss-btn").textContent, "Skip this version");
});

// The prompt waits for an open viewer too, and the prompt redraws in place
// (the 409 path) are untouched — covered in update-dialog.test.mjs.
test("an update prompt waits for an open notes viewer to close", { skip }, async () => {
  const h = await promptOpen();
  const dlg = h.el("update-dialog");
  h.el("btn-view-release-notes").click();
  await h.flush();
  await h.flush();
  assert.equal(dlg.dataset.viewerMode, "true");

  h.app.updates.showUpdateDialog();
  assert.equal(dlg.label, "Release Notes — v2.8.10", "the prompt replaced the open viewer");

  dlg.hide();
  await h.flush();
  assert.equal(dlg.label, "Update to v9.9.1");
  assert.equal(dlg.dataset.viewerMode, undefined);
  assert.equal(h.el("update-dismiss-btn").textContent, "Skip this version");
  assert.notEqual(h.el("update-now-btn").style.display, "none");
});

// ── repeats and the closing window (fix round) ─────────────────────────
//
// Update requests have no caller waiting on each one, so repeats of a mode
// merge into one waiting entry: two badge clicks under the viewer used to
// queue two prompts, and the prompt reopened after the operator closed it.

// MUTANT: _drawUpdateDialog pushing every request — the prompt reopens.
test("repeated prompt requests under the viewer draw one prompt", { skip }, async () => {
  const h = await promptOpen();
  const dlg = h.el("update-dialog");
  h.el("btn-view-release-notes").click();
  await h.flush();
  await h.flush();
  assert.equal(dlg.dataset.viewerMode, "true");

  h.app.updates.showUpdateDialog();
  h.app.updates.showUpdateDialog();
  dlg.hide();
  await h.flush();
  assert.equal(dlg.label, "Update to v9.9.1");
  assert.ok(isOpen(h, "update-dialog"));

  dlg.hide(); // Escape, the X or the overlay — not Skip
  await h.flush();
  await h.flush();
  assert.equal(isOpen(h, "update-dialog"), false, "a repeated request reopened the closed prompt");
});

// A request while the dialog is closing — even of the mode closing — waits
// for sl-after-hide: a show() in the hide animation cancels it and Shoelace
// fires a spurious sl-after-hide, which ended the owner under an open
// dialog and let the other mode draw straight over it.
//
// MUTANT: same-mode requests drawing at once while closing.
test("a request while the dialog is closing waits for the hide to end", { skip }, async () => {
  const h = await promptOpen();
  const dlg = h.el("update-dialog");
  h.el("version-indicator").click();
  assert.ok(isOpen(h, "update-dialog"));

  dlg.hide();
  h.el("version-indicator").click(); // within the hide animation
  assert.equal(isOpen(h, "update-dialog"), false, "the prompt was shown inside its own hide");
  await h.flush();
  assert.ok(isOpen(h, "update-dialog"), "the waiting prompt was not drawn after the hide");
  assert.equal(dlg.label, "Update to v9.9.1");

  // The prompt owns the dialog again: the viewer waits for it.
  h.app.updates.showNotesViewer(currentNotes);
  assert.equal(dlg.label, "Update to v9.9.1", "the viewer drew over the redrawn prompt");
  assert.equal(dlg.dataset.viewerMode, undefined);
  dlg.hide();
  await h.flush();
  assert.equal(dlg.label, "Release Notes — v2.8.10");
  assert.equal(h.el("update-dismiss-btn").textContent, "Close");
});
