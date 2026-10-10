// The details dialog's Mux button on a Finished job whose finalize left a
// split part unmuxed. The server keeps that job's staging and names Mux as the
// way back; POST /api/jobs/{id}/mux now accepts a Finished row exactly while
// GET /api/jobs/{id} reports unmuxedParts, and the button follows that flag.
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

async function openDetails(unmuxedParts) {
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": ({ params }) => ({
        ...row(params.id), hasStaging: true, hasSegments: true, asides: [], keptChatSidecar: false, unmuxedParts,
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

// MUTANT: leave canMux at MUX_STATUSES && hasSegments — the button stays hidden
// and the recovery the server names has no way in from the dashboard.
test("Mux is offered on a Finished job that still holds an unmuxed part", { skip }, async () => {
  const h = await openDetails(true);
  assert.equal(h.el("details-mux-btn").style.display, "", "Mux hidden on a Finished job with an unmuxed part");
});

// MUTANT: gate the Finished case on hasSegments instead of unmuxedParts — every
// Finished job whose staging was kept for another reason (an incomplete tail,
// a chat capture) offers a Mux the server refuses.
test("Mux stays hidden on a Finished job with nothing left to mux", { skip }, async () => {
  const h = await openDetails(false);
  assert.equal(h.el("details-mux-btn").style.display, "none", "Mux offered on a fully-muxed Finished job");
});

// MUTANT: drop unmuxedParts from _preserveStagingFields — the raw row a
// job_update carries has no such field, and the button vanishes on the next
// frame for the job.
test("a job_update keeps the Finished job's Mux button", { skip }, async () => {
  const h = await openDetails(true);
  h.app.handleMessage({ type: "job_update", payload: { ...row("A"), title: "renamed" } });
  await h.flush();
  assert.equal(h.app.jobs[0].unmuxedParts, true, "unmuxedParts was not carried across the job_update");
  assert.equal(h.el("details-mux-btn").style.display, "", "Mux hidden after a job_update");
});
