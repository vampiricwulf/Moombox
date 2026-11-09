// K03: the details dialog's staging facts (hasSegments, unmuxedParts, asides,
// ...) come from one GET /api/jobs/{id} and a WS row never carries them, so
// _preserveStagingFields carried them across every status change: a job
// opened Upcoming (no segments) that then failed with segments staged kept
// Mux hidden, and a Reinitialize (which deletes staging) kept a stale Mux.
// The facts are now stamped with the status they were fetched under
// (factsStatus), and a status change of the open job asks again, once.
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

const row = (status) => ({
  id: "A", title: "T A", videoId: "A", channelName: "Chan", platform: "youtube",
  status, createdAt: "2026-09-05T11:00:00Z", updatedAt: "2026-09-05T11:00:00Z",
});

/** The server: `state.status` and `state.hasSegments` are what GET answers. */
async function openDetails(state) {
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": () => ({
        ...row(state.status), hasStaging: state.hasSegments, hasSegments: state.hasSegments,
        asides: [], keptChatSidecar: false, unmuxedParts: false,
      }),
      "GET /api/jobs/:id/logs": () => [],
    },
  });
  const dlg = h.el("details-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [row(state.status)] } });
  await h.flush();
  h.app.details.showJobDetails(h.app.jobs[0]);
  await h.flush(); await h.flush();
  return h;
}

const gets = (h) => h.http.matching("/api/jobs/A", "GET").filter((c) => c.url === "/api/jobs/A").length;
const mux = (h) => h.el("details-mux-btn").style.display;

// MUTANT: no refetch on a status change — the carried hasSegments=false
// keeps Mux hidden on a failed job with segments.
test("a job opened Upcoming that fails with segments staged offers Mux", { skip }, async () => {
  const state = { status: "Upcoming", hasSegments: false };
  const h = await openDetails(state);
  assert.equal(mux(h), "none");
  assert.equal(gets(h), 1);

  state.status = "Error"; state.hasSegments = true;
  h.app.handleMessage({ type: "job_update", payload: row("Error") });
  await h.flush(); await h.flush();
  assert.equal(gets(h), 2, "the status change did not ask for the facts again");
  assert.equal(mux(h), "", "Mux hidden on a failed job with segments");

  // Further updates under the same status do not ask again.
  h.app.handleMessage({ type: "job_update", payload: { ...row("Error"), title: "renamed" } });
  await h.flush(); await h.flush();
  assert.equal(gets(h), 2, "a same-status update refetched");
});

// MUTANT: Mux judged on facts fetched under another status — the stale
// hasSegments=true shows Mux on the reinitialized job until the GET answers.
test("after a Reinitialize, Mux disappears", { skip }, async () => {
  const state = { status: "Error", hasSegments: true };
  const h = await openDetails(state);
  assert.equal(mux(h), "");

  state.status = "Upcoming"; state.hasSegments = false;
  h.app.handleMessage({ type: "job_update", payload: row("Upcoming") });
  assert.equal(mux(h), "none", "a Mux judged on the Error-time facts showed after the reset");
  await h.flush(); await h.flush();
  assert.equal(gets(h), 2);
  assert.equal(h.app.jobs[0].factsStatus, "Upcoming");

  // And when the reset job fails again before any segment, Mux stays hidden.
  state.status = "Error";
  h.app.handleMessage({ type: "job_update", payload: row("Error") });
  await h.flush(); await h.flush();
  assert.equal(mux(h), "none", "a stale Mux on a job whose staging the reset deleted");
});
