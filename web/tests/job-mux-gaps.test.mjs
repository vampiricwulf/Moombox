// The details dialog's "Kept for a Mux Gap" section: a Finished job whose
// staging is kept because a mux could not carry a staged recording whole — a
// damaged fragment's unreadable bytes left out of a repaired copy, or a copy
// nothing could check — says why, from GET /api/jobs/{id}'s muxGaps. The job's
// own log said so first, but that log lives in memory and is gone after a
// restart, while the shield has no age rule.
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
  status: "Finished", filename: "out.mp4",
  createdAt: "2026-09-05T11:00:00Z", updatedAt: "2026-09-05T11:00:00Z",
});

const gap = {
  path: "D:\\staging\\A\\video_stream",
  reason: "1 damaged stretch(es) (9775 bytes, 2.0s) were left out of D:\\out\\a.mp4: bytes 10-20 (10 bytes): video 0:01:40.0 to 0:01:42.0 (2.0s)",
};

async function openDetails(muxGaps) {
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": ({ params }) => ({
        ...row(params.id), hasStaging: true, hasSegments: true, asides: [], keptChatSidecar: false, unmuxedParts: false, muxGaps,
      }),
      "GET /api/jobs/:id/logs": () => [],
    },
  });
  const dlg = h.el("details-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [row("A")] } });
  await h.flush();
  h.app.details.showJobDetails(h.app.jobs[0]);
  await h.flush(); await h.flush();
  return h;
}

// MUTANT: drop the section, or the rebuild _fetchStagingFields makes for a
// job with mux gaps — the dialog of a job whose staging is kept for good says
// nothing about why.
test("the details dialog says which staged recording a mux could not carry whole, and why", { skip }, async () => {
  const h = await openDetails([gap]);
  const section = h.el("details-mux-gaps-section");
  assert.ok(section, "no #details-mux-gaps-section in the details dialog");
  const text = section.textContent;
  assert.match(text, /Kept for a Mux Gap \(1\)/, "the heading must name how many recordings are kept");
  assert.match(text, /video_stream — 1 damaged stretch\(es\)/, "the recording's name and its reason are not shown");
  assert.match(text, /0:01:40\.0 to 0:01:42\.0/, "the reason's time range is not shown");
  assert.equal(
    section.querySelector(".details-value[title]").getAttribute("title"),
    "D:\\staging\\A\\video_stream",
    "the row must carry the recording's real on-disk path as its tooltip",
  );
});

// MUTANT: render the section container unconditionally — every job grows an
// empty block, and app.test.mjs's jobDetails snapshot goes red with it.
test("a job with no mux gap renders no section", { skip }, async () => {
  const h = await openDetails([]);
  assert.equal(h.el("details-mux-gaps-section"), null, "a section was rendered for muxGaps: []");
  h.app.details.renderJobDetails(row("B"));
  assert.equal(h.el("details-mux-gaps-section"), null, "a section was rendered for a row with no muxGaps");
});

// MUTANT: drop muxGaps from _preserveStagingFields — the raw row a job_update
// carries has no such field, and the section vanishes on the next rebuild.
test("a job_update keeps the mux gaps on the open details dialog", { skip }, async () => {
  const h = await openDetails([gap]);
  h.app.handleMessage({ type: "job_update", payload: { ...row("A"), title: "renamed" } });
  await h.flush();
  assert.deepEqual(h.app.jobs[0].muxGaps, [gap], "muxGaps was not carried across the job_update");
  h.app.details.renderJobDetails(h.app.jobs[0]);
  assert.ok(h.el("details-mux-gaps-section"), "the section vanished after a job_update");
});
