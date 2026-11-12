// The details dialog's "Set Aside by Reinitialize" section: the archives a
// Reinitialize of the job renamed aside, from GET /api/jobs/{id}'s
// setAsideArchives (worker.ScanSetAsideArchives, owner ruling R2-W1b).
// Moombox never replaces or deletes them and Files does not offer them while
// the job exists, so the dialog is where the operator learns their names.
//
// Like the other DOM suites this needs jsdom; the import is probed first so
// `node --test web/tests/*.test.mjs` stays green without it.
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

const row = (id) => ({
  id, title: "T " + id, videoId: id, channelName: "Chan", platform: "youtube",
  status: "Upcoming",
  createdAt: "2026-09-05T11:00:00Z", updatedAt: "2026-09-05T11:00:00Z",
});

const archive = { name: "Show.restart-1700000000.mp4", path: String.raw`D:\out\Chan\Show.restart-1700000000.mp4`, size: 3 * 1024 * 1024 * 1024 };

async function openDetails(setAsideArchives) {
  let answer = setAsideArchives;
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": ({ params }) => ({
        ...row(params.id), hasStaging: false, hasSegments: false, asides: [], keptChatSidecar: false, unmuxedParts: false, muxGaps: [],
        setAsideArchives: answer,
      }),
      "GET /api/jobs/:id/logs": () => [],
    },
  });
  h.setAnswer = (v) => { answer = v; };
  const dlg = h.el("details-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [row("A")] } });
  await h.flush();
  h.app.details.showJobDetails(h.app.jobs[0]);
  await h.flush(); await h.flush();
  return h;
}

// The line is the TUI's setAsideArchiveLine, pinned on the same file and size
// (TestDetailsPanelListsTheArchivesAReinitializeSetAside).
// MUTANT: drop the section, or setAsideArchives from the rebuild test in
// _fetchStagingFields — a job that is recording again says nothing about the
// archive its Reinitialize set aside.
test("the details dialog lists the archives a Reinitialize set aside", { skip }, async () => {
  const h = await openDetails([archive]);
  const section = h.el("details-set-aside-archives-section");
  assert.ok(section, "no #details-set-aside-archives-section in the details dialog");
  const line = section.querySelector('[data-field="set-aside-archive"]');
  assert.equal(line.textContent, "Show.restart-1700000000.mp4 (3.0GB, kept, never replaced)");
  assert.equal(line.getAttribute("title"), archive.path, "the path is the line's tooltip");
  assert.equal(section.querySelector("sl-button"), null, "the section offers no action");
});

// MUTANT: render the section unconditionally.
test("a job with no set-aside archive renders no section", { skip }, async () => {
  const h = await openDetails([]);
  assert.equal(h.el("details-set-aside-archives-section"), null);
});

// MUTANT: drop setAsideArchives from _preserveStagingFields — the raw row a
// job_update carries has no such field, and the section vanishes on the next
// rebuild.
test("a job_update keeps the set-aside archives on the open details dialog", { skip }, async () => {
  const h = await openDetails([archive]);
  h.app.handleMessage({ type: "job_update", payload: { ...row("A"), title: "renamed" } });
  await h.flush();
  h.app.details.renderJobDetails(h.app.jobs[0]);
  assert.ok(h.el("details-set-aside-archives-section"), "the section vanished after a job_update");
});

// MUTANT: leave the section out of `shown` in _fetchStagingFields — a file
// the operator has since removed stays listed on the open dialog.
test("an answer without the archive removes the section", { skip }, async () => {
  const h = await openDetails([archive]);
  h.setAnswer([]);
  await h.app.details._fetchStagingFields("A");
  await h.flush();
  assert.equal(h.el("details-set-aside-archives-section"), null, "the section outlived the file");
});
