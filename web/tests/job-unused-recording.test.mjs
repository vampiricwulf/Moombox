// The details dialog's "Kept Recording" section: a Finished job whose staging
// root still holds a recording the archive was not made from names the file,
// its size and its path, from GET /api/jobs/{id}'s unusedRootRecording (owner
// ruling R-E1). That recording keeps the job's staging with no age limit and
// can be the longer copy; the only other account is one line in the job's
// in-memory log, gone after a restart.
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

const kept = { name: "video.mp4", path: String.raw`D:\staging\A\video.mp4`, size: 3 * 1024 * 1024 * 1024 };

async function openDetails(unusedRootRecording) {
  let answer = unusedRootRecording;
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": ({ params }) => ({
        ...row(params.id), hasStaging: true, hasSegments: true, asides: [], keptChatSidecar: false, unmuxedParts: false, muxGaps: [],
        ...(answer ? { unusedRootRecording: answer } : {}),
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

// MUTANT: drop the section, or the rebuild _fetchStagingFields makes for a
// job with a kept recording — the dialog says nothing about what holds the
// staging.
test("the details dialog names the kept recording the archive was not made from", { skip }, async () => {
  const h = await openDetails(kept);
  const section = h.el("details-unused-recording-section");
  assert.ok(section, "no #details-unused-recording-section in the details dialog");
  assert.equal(
    section.querySelector('[data-field="unused-recording"]').textContent,
    "Kept recording not in the archive: video.mp4 (3.0GB)",
  );
  assert.equal(
    section.querySelector('[data-field="unused-recording-path"]').textContent,
    kept.path,
    "the path must be shown beneath the line",
  );
  assert.equal(section.querySelector("sl-button"), null, "the section offers no action (owner ruling R-E1)");
});

// MUTANT: render the section unconditionally — every job grows an empty block.
test("a job with no kept recording renders no section", { skip }, async () => {
  const h = await openDetails(null);
  assert.equal(h.el("details-unused-recording-section"), null, "a section was rendered for a job with none");
  assert.equal(h.app.jobs[0].unusedRootRecording, null, "an absent field must be stored as null, not left undefined");
});

// MUTANT: drop unusedRootRecording from _preserveStagingFields — the raw row
// a job_update carries has no such field, and the section vanishes on the
// next rebuild.
test("a job_update keeps the kept recording on the open details dialog", { skip }, async () => {
  const h = await openDetails(kept);
  h.app.handleMessage({ type: "job_update", payload: { ...row("A"), title: "renamed" } });
  await h.flush();
  assert.deepEqual(h.app.jobs[0].unusedRootRecording, kept, "unusedRootRecording was not carried across the job_update");
  h.app.details.renderJobDetails(h.app.jobs[0]);
  assert.ok(h.el("details-unused-recording-section"), "the section vanished after a job_update");
});

// MUTANT: leave `shown` out of the rebuild test, or store an absent answer as
// undefined — a section for a recording that is gone (the job was
// reinitialized, or the file taken) stays on the open dialog.
test("an answer without the recording removes the section", { skip }, async () => {
  const h = await openDetails(kept);
  h.setAnswer(null);
  await h.app.details._fetchStagingFields("A");
  await h.flush();
  assert.equal(h.el("details-unused-recording-section"), null, "the section outlived the recording");
});
