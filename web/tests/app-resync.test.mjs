// A mid-session `initial_state` must REPLACE the dashboard's job list.
//
// The server sends a full snapshot as the first frame after it drops one for a
// lagging client (internal/web/websocket.go, sweep T3-26). That fix rests
// entirely on app.js treating `initial_state` as a full-state replace rather
// than a first-connect hydrate — true today, pinned here.
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

const job = (id, title) => ({
  id, title, videoId: id, channelName: "Chan", platform: "youtube", status: "Downloading",
});

test("a mid-session initial_state replaces the job list", { skip }, async () => {
  const h = await harness.makeApp();

  // The state a lagging tab is left in: a row the dropped job_deleted retired.
  h.app.jobs = [job("ghost", "Ghost row"), job("keep", "Still here")];
  h.app.renderJobs();
  assert.ok(
    h.el("jobs-container").querySelector('.video-item[data-job-id="ghost"]'),
    "fixture: the ghost row must be on screen before the resync",
  );

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [job("keep", "Still here")] } });
  await h.flush();

  assert.deepEqual(h.app.jobs.map((j) => j.id), ["keep"],
    "initial_state must REPLACE this.jobs — an upsert-style merge (the mutant) keeps the ghost");
  assert.equal(
    h.el("jobs-container").querySelector('.video-item[data-job-id="ghost"]'), null,
    "the ghost row must be gone from the DOM, not just from the array",
  );
  assert.ok(
    h.el("jobs-container").querySelector('.video-item[data-job-id="keep"]'),
    "the surviving row must still be drawn",
  );
});

test("a mid-session initial_state replaces the log buffer", { skip }, async () => {
  const h = await harness.makeApp();

  h.app.logPanel.logs = ["INFO stale line"];
  h.app.logPanel.renderLogs();

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], logs: ["INFO fresh line"] } });
  h.flushRaf();
  await h.flush();

  assert.deepEqual(h.app.logPanel.logs, ["INFO fresh line"],
    "the snapshot's logs must replace the buffer — appending them (the mutant) doubles the history " +
    "of every re-synced tab");
  assert.match(h.el("logs-viewer").textContent, /fresh line/);
  assert.doesNotMatch(h.el("logs-viewer").textContent, /stale line/);
});

// The snapshot rows are raw GetAllJobs rows; hasStaging/hasSegments/asides/
// keptChatSidecar come only from GET /api/jobs/:id. jobs_update and job_update
// carry them across a replace; initial_state did not, so an open details
// dialog refreshed from the snapshot hid its Resume and Mux buttons — and the
// hub sends this snapshot mid-session in place of a dropped frame.
//
// Mutant: drop the _preserveStagingFields call from initial_state.
test("a mid-session initial_state keeps the details dialog's staging fields", { skip }, async () => {
  const row = (id) => ({
    id, title: "T " + id, videoId: id, channelName: "Chan", platform: "youtube",
    status: "Error", createdAt: "2026-09-05T11:00:00Z", updatedAt: "2026-09-05T11:00:00Z",
  });
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": ({ params }) => ({ ...row(params.id), hasStaging: true, hasSegments: true, asides: [], keptChatSidecar: false }),
      "GET /api/jobs/:id/logs": () => [],
    },
  });
  const dlg = h.el("details-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [row("A")] } });
  await h.flush();
  h.app.details.showJobDetails(h.app.jobs[0]);
  await h.flush(); await h.flush();
  assert.equal(h.el("details-resume-btn").style.display, "", "premise: Resume shown after the enrich fetch");

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [row("A")] } });
  await h.flush();
  assert.equal(h.app.jobs[0].hasStaging, true, "the snapshot dropped hasStaging");
  assert.equal(h.el("details-resume-btn").style.display, "", "Resume hidden after the snapshot");
  assert.equal(h.el("details-mux-btn").style.display, "", "Mux hidden after the snapshot");
});

// The snapshot carries active rows only. With the Archived panel open, a
// job_deleted missed while the socket was down (or evicted from the queue and
// "replaced" by the snapshot) left a clickable ghost there until the operator
// left the tab. An open Archived panel refetches on every snapshot now.
//
// Mutant: drop the fetchArchivedJobs call from initial_state.
test("a snapshot with the Archived panel open refetches it", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "GET /api/jobs/archived": () => [] } });
  h.app._activePanel = "archived";
  h.app.archivedJobs = [{ id: "GHOST", title: "Ghost", videoId: "GHOST", channelName: "Chan", platform: "youtube", status: "Finished" }];
  h.app.renderArchivedJobs();
  const before = h.http.matching("/api/jobs/archived").length;
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [] } });
  await h.flush(); await h.flush();
  assert.equal(h.http.matching("/api/jobs/archived").length, before + 1, "no archived refetch on the snapshot");
  assert.ok(!h.app.archivedJobs.some((j) => j.id === "GHOST"), "the deleted row is still in the archived list");
});

// The Player's picker lists Finished recordings, but it was rebuilt only on
// tab activation, Open in Player and a ZIP import: with the Player tab open,
// a recording that finished was not offered and a deleted one stayed listed,
// playing from a source that now 404s. A status change to/from Finished and a
// job_deleted rebuild it while that tab is open — and only then.
//
// Mutants: drop either _refreshPlayerPicker call; drop its open-tab gate (a
// hidden tab rebuilds too).
test("the Player picker follows recordings finishing and being deleted", { skip }, async () => {
  const h = await harness.makeApp();
  let rebuilds = 0;
  h.app.player.loadPlayerJobList = async () => { rebuilds++; };
  h.app.player.playerInitialized = true;
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [job("A", "Alpha"), job("B", "Beta")] } });
  await h.flush();

  h.app._activePanel = "tasks";
  h.app.handleMessage({ type: "job_update", payload: { ...job("A", "Alpha"), status: "Finished", filename: "a.mp4" } });
  assert.equal(rebuilds, 0, "a hidden Player tab rebuilt its picker");

  h.app._activePanel = "player";
  h.app.handleMessage({ type: "job_update", payload: { ...job("B", "Beta"), status: "Finished", filename: "b.mp4" } });
  assert.equal(rebuilds, 1, "a recording that finished was not offered");
  h.app.handleMessage({ type: "job_deleted", payload: { id: "B" } });
  assert.equal(rebuilds, 2, "a deleted recording stayed in the picker");
});

// batchAction emptied the selection set but left the boxes ticked and the
// .selected highlight on rows no job_update would redraw: a target the
// server refused (a 400 from /cancel) provokes none, and a selected row that
// was not a target of the verb at all is redrawn only by chance.
//
// Mutant: clear only the set again (the previous two lines of batchAction).
test("a batch action clears the ticks on rows it did not redraw", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "POST /api/jobs/:id/cancel": () => harness.response({ status: 400, body: { error: "Job cannot be cancelled" } }) },
  });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [job("A", "Alpha"), { ...job("B", "Beta"), status: "Finished" }] } });
  await h.flush();
  h.app._selectedTaskJobs.add("A").add("B");
  h.app.renderJobs();
  const box = (id) => h.el("jobs-container").querySelector(`.video-item[data-job-id="${id}"] .job-checkbox`);
  assert.equal(box("A").checked, true, "precondition: A ticked");
  h.app.showConfirm = async () => true;

  await h.app.batchAction("cancel"); // A refused by the server; B is not a cancel target
  await h.flush();

  assert.equal(h.app._selectedTaskJobs.size, 0);
  for (const id of ["A", "B"]) {
    assert.equal(box(id).checked, false, `${id} still ticked after the batch`);
    assert.ok(!h.el("jobs-container").querySelector(`.video-item.selected[data-job-id="${id}"]`), `${id} still highlighted`);
  }
});

// A filter that hides every row returned early, before the stale-selection
// prune, so deleting a selected job left the bar counting it.
//
// Mutant: prune after the filtered-empty return again — the bar says
// "2 selected" with one job left.
test("a delete under a filter that hides every row drops the job from the selection", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [job("A", "Alpha"), job("B", "Beta")] } });
  await h.flush();
  h.app._selectedTaskJobs.add("A").add("B");
  h.app.filterBar.tasksFilterTokens = [{ type: "text", value: "zzz-no-match" }];
  h.app.renderJobs();
  assert.equal(h.el("batch-count").textContent, "2 selected", "precondition");

  h.app.handleMessage({ type: "job_deleted", payload: { id: "B" } });
  await h.flush();

  assert.deepEqual([...h.app._selectedTaskJobs], ["A"]);
  assert.equal(h.el("batch-count").textContent, "1 selected");
});
