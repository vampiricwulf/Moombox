// The trim dialog's failure states and keyboard reach: a preview that cannot
// load said nothing, a refused time was left in its box while the timeline
// kept the old value, and the timeline handles were mouse-only divs.
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
const response = (spec) => harness.response(spec);
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

async function openTrim() {
  const h = await harness.makeApp();
  h.app.trimmer.open({ id: "j1", title: "t", lengthSeconds: 100 });
  return h;
}
const said = (h) => h.toasts().map((t) => t.textContent).join(" | ");

// Mutant: drop the video error listener.
test("a trim preview that cannot load says so", { skip }, async () => {
  const h = await openTrim();
  h.el("trim-video").dispatchEvent(new h.window.Event("error"));
  assert.match(said(h), /trim preview could not load/);
});

// Mutant: drop the refuse() call from the start input's sl-change.
test("a refused trim time is put back and explained", { skip }, async () => {
  const h = await openTrim();
  const input = h.el("trim-start-input");
  input.value = "5:00"; // past the 1:40 recording
  input.dispatchEvent(new h.window.Event("sl-change"));
  assert.equal(input.value, "0:00", "the box shows the marker it still holds");
  assert.equal(h.app.trimmer.startMarker, 0);
  assert.match(said(h), /Enter a time between 0:00 and 1:40/);
});

// Mutants: drop the focused-handle arm from _onKeyDown (the arrow seeks the
// video instead); drop the aria-value updates from _updateTimeline.
test("a focused timeline handle is a keyboard slider", { skip }, async () => {
  const h = await openTrim();
  const start = h.el("trim-handle-start");
  assert.equal(start.getAttribute("role"), "slider");
  assert.equal(start.getAttribute("tabindex"), "0");

  const press = (target, key, init = {}) => {
    const ev = new h.window.KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init });
    target.dispatchEvent(ev); // a target, so composedPath() is real
    h.app.trimmer._onKeyDown(ev);
  };
  press(start, "ArrowRight", { shiftKey: true });
  press(start, "ArrowRight");
  assert.equal(h.app.trimmer.startMarker, 6);
  assert.equal(start.getAttribute("aria-valuenow"), "6");
  assert.equal(start.getAttribute("aria-valuetext"), "0:06");
  assert.equal(h.el("trim-start-input").value, "0:06");

  const end = h.el("trim-handle-end");
  press(end, "ArrowRight");
  assert.equal(h.app.trimmer.endMarker, 100, "the end marker stops at the recording's end");
  press(end, "ArrowLeft");
  assert.equal(h.app.trimmer.endMarker, 99);
});

// ── A trim answer belongs to the dialog that asked ────────────────────────
//
// The dialog can close while its request is out (Cancel, Escape, a click
// outside), and the one TrimController then serves the next trim dialog. The
// answer used to be applied to whatever `this` held by then.

const finishedJob = (id) => ({
  id, title: `title ${id}`, status: "Finished", lengthSeconds: 100, videoId: `v${id}`,
  platform: "youtube", channelName: "ch", filename: `ch/${id}.mp4`,
  createdAt: "2026-09-05T11:00:00Z", updatedAt: "2026-09-05T11:00:00Z", trims: [],
});

// Shoelace's dialogs as app.js meets them: hide() flips `open` and fires
// sl-after-hide once the (async) animation is done — which is what runs
// trimmer.destroy() and clears the details selection.
function shoelaceDialog(h, dlg) {
  dlg.open = false;
  dlg._calls = [];
  dlg.show = function () { this._calls.push("show"); this.open = true; return Promise.resolve(); };
  dlg.hide = function () {
    this._calls.push("hide");
    if (!this.open) return Promise.resolve();
    this.open = false;
    return Promise.resolve().then(() => this.dispatchEvent(new h.window.Event("sl-after-hide")));
  };
}

/**
 * The real dashboard paths — a job card's click, the details dialog's Trim
 * button, the trim dialog's Create and Cancel — with the trim POST held open
 * until the test answers it. `answer(id, spec)` releases the request job `id`
 * made with a response spec.
 */
async function trimRace() {
  const pending = {};
  const h = await harness.makeApp({
    routes: {
      "POST /api/jobs/:id/trims": ({ params }) => new Promise((resolve) => { pending[params.id] = resolve; }),
      "GET /api/jobs/:id": ({ params }) => finishedJob(params.id),
    },
  });
  h.app.jobs = [finishedJob("A"), finishedJob("B"), finishedJob("C")];
  h.app.renderJobs();
  shoelaceDialog(h, h.el("trim-dialog"));
  shoelaceDialog(h, h.el("details-dialog"));
  const settle = async () => { for (let i = 0; i < 6; i++) await h.flush(); };
  const click = async (target) => {
    target.dispatchEvent(new h.window.MouseEvent("click", { bubbles: true }));
    await settle();
  };
  const openDetails = (id) => click(h.document.querySelector(`.video-item[data-job-id="${id}"]`));
  const openTrim = async (id) => {
    await openDetails(id);
    await click(h.el("details-trim-btn"));
    h.advance(100); // trimmer.open shows its dialog on a 100 ms timer
    await settle();
    assert.equal(h.app.trimmer.job?.id, id);
  };
  const submit = async (start, end) => {
    h.app.trimmer.startMarker = start;
    h.app.trimmer.endMarker = end;
    await click(h.el("trim-submit-btn"));
  };
  const answer = async (id, spec) => {
    pending[id](spec);
    await settle();
    h.advance(100);
    await settle();
  };
  return { h, click, openDetails, openTrim, submit, answer };
}

// Mutant: act on the answer whatever the dialog (drop `!session.signal.aborted`).
test("a late trim answer leaves another job's trim dialog alone", { skip }, async () => {
  const r = await trimRace();
  await r.openTrim("A");
  await r.submit(10, 20);
  await r.click(r.h.el("trim-cancel-btn"));
  assert.equal(r.h.app.trimmer.job, null, "closing the dialog destroyed it");
  await r.openTrim("B");
  r.h.app.trimmer.startMarker = 30;
  await r.answer("A", { trim: { id: "tA", jobId: "A" } });
  assert.equal(r.h.el("trim-dialog").open, true, "job A's answer closed job B's trim dialog");
  assert.equal(r.h.app.trimmer.job?.id, "B");
  assert.equal(r.h.app.trimmer.startMarker, 30, "job B's markers were lost");
});

// Mutants: reset Create in the finally whatever the dialog (drop the
// `!session.signal.aborted` guard there) — B's in-flight Create is offered
// again; drop open()'s reset — B's dialog opens with A's Create still loading.
test("a late trim answer leaves the next dialog's Create button alone", { skip }, async () => {
  const r = await trimRace();
  const create = r.h.el("trim-submit-btn");
  await r.openTrim("A");
  await r.submit(10, 20);
  await r.click(r.h.el("trim-cancel-btn"));
  await r.openTrim("B");
  assert.equal(create.disabled, false, "job B's dialog opened with job A's Create still busy");
  assert.equal(create.loading, false);
  await r.submit(30, 40);
  await r.answer("A", { trim: { id: "tA", jobId: "A" } });
  assert.equal(create.disabled, true, "job A's answer re-enabled Create while job B's request is out");
  assert.equal(create.loading, true);
});

const failed = () => response({ status: 500, body: { error: "Failed to create trim" } });

// Mutant: clear the selection whatever it is (drop `this.app.selectedJobId === job.id`).
test("a late failed trim keeps another job's selection", { skip }, async () => {
  const r = await trimRace();
  await r.openTrim("A");
  await r.submit(10, 20);
  await r.click(r.h.el("trim-cancel-btn"));
  // Job C's details are open when A's answer lands…
  await r.openDetails("C");
  await r.answer("A", failed());
  assert.equal(r.h.app.selectedJobId, "C", "job A's failed trim cleared job C's selection");
  assert.equal(r.h.el("details-dialog").open, true);

  // …or job B's own trim is out, holding B selected so that its answer can
  // refresh B's details.
  const r2 = await trimRace();
  await r2.openTrim("A");
  await r2.submit(10, 20);
  await r2.click(r2.h.el("trim-cancel-btn"));
  await r2.openTrim("B");
  await r2.submit(30, 40);
  await r2.answer("A", failed());
  assert.equal(r2.h.app.selectedJobId, "B", "job A's failed trim cleared the selection job B's trim holds");
});

// Mutant: clear the selection while its details are open (drop `!el.details?.open`).
test("a late failed trim keeps its own job's reopened details selected", { skip }, async () => {
  const r = await trimRace();
  await r.openTrim("A");
  await r.submit(10, 20);
  await r.click(r.h.el("trim-cancel-btn"));
  await r.openDetails("A");
  await r.answer("A", failed());
  assert.equal(r.h.app.selectedJobId, "A", "the failed trim cleared the selection under job A's open details");
});

// The undisturbed path, which the guards must keep: the answer closes its own
// dialog and reopens its job's details.
test("a trim answer closes its own dialog and reopens its job's details", { skip }, async () => {
  const r = await trimRace();
  await r.openTrim("A");
  await r.submit(10, 20);
  await r.answer("A", { trim: { id: "tA", jobId: "A" } });
  assert.equal(r.h.el("trim-dialog").open, false);
  assert.equal(r.h.el("details-dialog").open, true);
  assert.equal(r.h.app.selectedJobId, "A");
});
