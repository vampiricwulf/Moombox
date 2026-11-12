// Reinitialize confirms first and names what it deletes (owner decision
// PR-M1): the dashboard's row icon, its details button and the batch bar all
// read GET /api/jobs/{id}/reinitialize/preview before POST /reinitialize, and
// the row icon is Resume where the job can continue its recording.
//
// Same jsdom probe as app.test.mjs for the DOM suites; the pure clause runs
// everywhere.
import { test, after } from "node:test";
import assert from "node:assert/strict";

import { reinitializeDeletesText, sumReinitializePreviews } from "../public/modules/utils.js";

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

// The Go twin is worker.ReinitializePreview.Summary
// (TestReinitializePreviewSummary): the same cases, the same words.
//
// Mutants: the singular dropped ("1 set-aside recordings"); the empty case
// dropped; the last clause joined by a comma.
test("reinitializeDeletesText words the preview as the TUI does", () => {
  assert.equal(reinitializeDeletesText({}), "deletes nothing recorded");
  assert.equal(reinitializeDeletesText({ setAsideRecordings: 1 }), "deletes 1 set-aside recording");
  assert.equal(
    reinitializeDeletesText({ setAsideRecordings: 2, recordedParts: 3, recordedPartBytes: 1536, stagedFiles: 1, stagedBytes: 100, importedChapters: 5 }),
    "deletes 2 set-aside recordings, 3 recorded parts (1.5KB), 1 staged file (100B) and 5 imported chapters",
  );
  assert.equal(reinitializeDeletesText({ stagedFiles: 4, stagedBytes: 2048, importedChapters: 1 }),
    "deletes 4 staged files (2.0KB) and 1 imported chapter");
  assert.deepEqual(
    sumReinitializePreviews([{ setAsideRecordings: 1, stagedBytes: 10 }, { setAsideRecordings: 2, importedChapters: 3, allowed: true }]),
    { setAsideRecordings: 3, recordedParts: 0, recordedPartBytes: 0, stagedFiles: 0, stagedBytes: 10, importedChapters: 3,
      pendingAutoMux: 0, pendingAutoRetry: 0, existingArchives: 0, existingArchiveBytes: 0 },
  );
  // The archive the reset keeps (owner ruling R2-W1), after what it cancels:
  // the same two cases as TestReinitializePreviewSummary.
  assert.equal(reinitializeDeletesText({ existingArchives: 1, existingArchiveBytes: 2048 }),
    "sets the existing archive (2.0KB) aside, and deletes nothing recorded");
  assert.equal(reinitializeDeletesText({ existingArchives: 1 }), "sets the existing archive aside, and deletes nothing recorded");
  assert.equal(
    reinitializeDeletesText({ existingArchives: 2,existingArchiveBytes: 4096, stagedFiles: 1, stagedBytes: 10, pendingAutoMux: 1 }),
    "cancels the pending automatic mux, sets 2 existing archives (4.0KB) aside, and deletes 1 staged file (10B)",
  );
});

const errJob = (over = {}) => ({ id: "e1", videoId: "e1", title: "Broken stream", channelName: "C",
  platform: "youtube", status: "Error", updatedAt: harness.agoISO(30), ...over });

async function boot(previews) {
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id/reinitialize/preview": ({ params }) => ({ allowed: true, ...previews[params.id] }),
      "POST /api/jobs/:id/reinitialize": () => ({ success: true }),
    },
  });
  h.document.querySelector('sl-tab-panel[name="tasks"]').setAttribute("active", "");
  h.app._activePanel = "tasks";
  return h;
}

// A single Reinitialize — the row icon's or the details button's
// (reinitializeJob) — asks first, naming the preflight's counts, and a
// declined confirm sends nothing.
//
// Mutants: reinitializeJob not confirming (the POST goes out on decline);
// the confirm not naming the preview (no "deletes 2 set-aside recordings").
test("a single Reinitialize confirms first and names what it deletes", { skip }, async () => {
  const h = await boot({ e1: { setAsideRecordings: 2, stagedFiles: 3, stagedBytes: 2048 } });
  h.app.jobs = [errJob()];
  let asked = null;
  h.app.showConfirm = async (msg) => { asked = msg; return false; };
  await h.app.reinitializeJob("e1");
  await h.flush();
  assert.match(asked ?? "", /Reinitialize "Broken stream".*This deletes 2 set-aside recordings and 3 staged files \(2\.0KB\)\. This cannot be undone\./);
  assert.deepEqual(h.fetchLog.filter((c) => c.method === "POST").map((c) => c.url), []);

  h.app.showConfirm = async () => true;
  await h.app.reinitializeJob("e1");
  await h.flush();
  assert.deepEqual(h.fetchLog.filter((c) => c.method === "POST").map((c) => c.url), ["/api/jobs/e1/reinitialize"]);
});

// A preview that cannot be read still confirms, and says what the verb
// deletes in general rather than nothing.
//
// Mutant: a failed preview skipping the confirm.
test("a Reinitialize whose preview fails still confirms", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "POST /api/jobs/:id/reinitialize": () => ({ success: true }) } });
  h.app.jobs = [errJob()];
  let asked = null;
  h.app.showConfirm = async (msg) => { asked = msg; return false; };
  await h.app.reinitializeJob("e1");
  await h.flush();
  assert.match(asked ?? "", /could not be read: it deletes the job's set-aside recordings/);
  assert.deepEqual(h.fetchLog.filter((c) => c.method === "POST").map((c) => c.url), []);
});

// A preview whose allowed is false — the POST's own status gate says no —
// gets no confirm: a warning names why and nothing is posted.
//
// Mutant: allowed ignored (the confirm opens).
test("a Reinitialize the server would refuse asks nothing", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id/reinitialize/preview": () => ({ allowed: false }),
      "POST /api/jobs/:id/reinitialize": () => ({ success: true }),
    },
  });
  h.app.jobs = [errJob()];
  let asked = false;
  h.app.showConfirm = async () => { asked = true; return true; };
  const toasts = [];
  h.app.showToast = (msg, variant) => toasts.push([msg, variant]);
  await h.app.reinitializeJob("e1");
  await h.flush();
  assert.equal(asked, false, "no confirm for a reset the server refuses");
  assert.deepEqual(toasts, [["Not reinitialized — the job is Error now", "warning"]]);
  assert.deepEqual(h.fetchLog.filter((c) => c.method === "POST").map((c) => c.url), []);
});

// A job a monitor restarted while the confirm was open is not reset under
// its run: the status is read again after the confirm.
//
// Mutant: the post-confirm re-check dropped (the POST goes out).
test("a Reinitialize re-checks the status after its confirm", { skip }, async () => {
  const h = await boot({ e1: { stagedFiles: 1, stagedBytes: 10 } });
  h.app.jobs = [errJob()];
  h.app.showConfirm = async () => { h.app.jobs = [errJob({ status: "Downloading" })]; return true; };
  const toasts = [];
  h.app.showToast = (msg, variant) => toasts.push([msg, variant]);
  await h.app.reinitializeJob("e1");
  await h.flush();
  assert.deepEqual(h.fetchLog.filter((c) => c.method === "POST").map((c) => c.url), []);
  assert.deepEqual(toasts, [["Not reinitialized — the job is Downloading now", "warning"]]);
});

// The batch confirm names the totals across the jobs it will act on.
//
// Mutant: the batch not asking the preflight (the old bare "Reinitialize 2
// jobs?").
test("the batch Reinitialize confirm names the totals", { skip }, async () => {
  const h = await boot({ e1: { recordedParts: 1, recordedPartBytes: 1024 }, e2: { recordedParts: 2, recordedPartBytes: 1024, importedChapters: 4 } });
  h.app.jobs = [errJob(), errJob({ id: "e2", videoId: "e2", status: "Cancelled" })];
  h.app.renderJobs();
  h.app._selectedTaskJobs.add("e1");
  h.app._selectedTaskJobs.add("e2");
  let asked = null;
  h.app.showConfirm = async (msg) => { asked = msg; return false; };
  await h.app.batchAction("reinitialize");
  await h.flush();
  assert.equal(asked, "Reinitialize 2 jobs? This deletes 3 recorded parts (2.0KB) and 4 imported chapters. This cannot be undone.");
});

// The row's restart icon is Resume where the job can continue, and
// Reinitialize only where it cannot; both stay in the overflow menu. Owner
// decision PE-5: "can continue" needs the server's hasStaging, which every
// list row Resume can be offered on carries — a resumable row with nothing staged, or one
// without the flag, gets Reinitialize.
//
// Mutants: the row keeping the arrow-clockwise Reinitialize icon for a
// resumable YouTube job; canResumeJob without requireKnownStaging (the row
// with no flag offers Resume).
test("the row icon is Resume where the job can resume", { skip }, async () => {
  const h = await boot({});
  const yt = h.app.renderJobItem(errJob({ hasStaging: true }));
  assert.match(yt, /<sl-icon-button name="play-fill" label="Resume" data-quick-action="resume"/);
  assert.doesNotMatch(yt, /data-quick-action="reinitialize"/);
  assert.match(yt, /<sl-menu-item value="reinitialize"/);

  const tw = h.app.renderJobItem(errJob({ platform: "twitch" }));
  assert.match(tw, /data-quick-action="reinitialize"/);
  assert.doesNotMatch(tw, /data-quick-action="resume"/);

  const noStaging = h.app.renderJobItem(errJob({ hasStaging: false }));
  assert.match(noStaging, /data-quick-action="reinitialize"/);
  assert.doesNotMatch(noStaging, /data-quick-action="resume"/);

  const unknown = h.app.renderJobItem(errJob());
  assert.match(unknown, /data-quick-action="reinitialize"/);
  assert.doesNotMatch(unknown, /data-quick-action="resume"/);
});
