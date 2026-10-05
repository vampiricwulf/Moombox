// The Tasks list's selection and the actions that read it: what a batch or a
// single action may touch is what the operator can see and confirm.
import { test, after } from "node:test";
import assert from "node:assert/strict";

import * as harness from "./helpers/app-dom.mjs";
import * as inputs from "./fixtures/app-render-inputs.mjs";

after(() => harness.teardownAll());

const fin = (over = {}) => ({ ...inputs.JOBS.Finished, id: "fin-1", videoId: "fin1", title: "Old finished",
  watched: false, updatedAt: harness.agoISO(60), ...over });
const live = (over = {}) => ({ ...inputs.JOBS.Finished, id: "live-1", videoId: "live1", title: "Live now",
  status: "Live", updatedAt: harness.agoISO(10), ...over });

async function boot(routes = {}) {
  const h = await harness.makeApp({ routes });
  h.document.querySelector('sl-tab-panel[name="tasks"]').setAttribute("active", "");
  h.app._activePanel = "tasks";
  return h;
}

// A job ticked before the filter changed stayed selected while the filter hid
// it: the bar kept "1 selected", offered Delete, and the batch deleted a row
// that was not on screen. The filter now drops what it hides.
//
// Mutant: renderJobs' visibleIds prune removed — DELETE /api/jobs/fin-1 goes out.
test("a filter drops the selection it hides, so no batch reaches a hidden job", async () => {
  const h = await boot({ "DELETE /api/jobs/:id": () => ({ success: true }) });
  h.app.jobs = [fin(), live()];
  h.app.renderJobs();
  h.el("jobs-container").querySelector('.video-item[data-job-id="fin-1"] .job-checkbox').click();
  assert.equal(h.app._selectedTaskJobs.size, 1);

  h.app.filterBar.tasksFilterTokens = [{ type: "status", value: "active", negate: false }];
  h.app.renderJobs();
  assert.equal(h.app._selectedTaskJobs.size, 0, "the hidden job is still selected");

  h.app.showConfirm = async () => true;
  await h.app.batchAction("delete");
  await h.flush();
  assert.deepEqual(h.fetchLog.filter((c) => c.method === "DELETE").map((c) => c.url), []);
});

// The Archived panel keeps its own selection and prunes it the same way.
//
// Mutant: renderArchivedJobs' visibleArchivedIds prune removed.
test("an Archived filter drops the selection it hides", async () => {
  const h = await harness.makeApp({});
  h.app._activePanel = "archived";
  h.app.archivedJobs = [fin({ id: "old-a", videoId: "oa", title: "Alpha" }), fin({ id: "old-b", videoId: "ob", title: "Bravo" })];
  h.app._selectedArchivedJobs.add("old-a");
  h.app.filterBar.archivedFilterTokens = [{ type: "text", value: "bravo", negate: false }];
  h.app.renderArchivedJobs();
  assert.equal(h.app._selectedArchivedJobs.size, 0, "the hidden archived job is still selected");
});

// The confirm waits on the operator, and a row can move meanwhile: an Error
// job the monitor retries is Downloading by the time OK is clicked, and the
// DELETE route cancels a running download before removing it. Every action
// re-reads the row after its confirm and acts only on what still qualifies.
//
// Mutants: batchAction's post-confirm re-filter removed; deleteJob's or
// cancelJob's post-confirm status check removed.
test("a batch delete skips a job that started downloading while the confirm was open", async () => {
  const h = await boot({ "DELETE /api/jobs/:id": () => ({ success: true }) });
  const err = fin({ id: "err-1", videoId: "e1", status: "Error" });
  h.app.jobs = [err, live()];
  h.app.renderJobs();
  h.app._selectedTaskJobs.add("err-1");
  h.app.showConfirm = async () => {
    h.app.handleMessage({ type: "job_update", payload: { ...err, status: "Downloading" } });
    return true;
  };
  await h.app.batchAction("delete");
  await h.flush();
  assert.deepEqual(h.fetchLog.filter((c) => c.method === "DELETE").map((c) => c.url), []);
});

test("a single delete does not reach a job that started downloading while the confirm was open", async () => {
  const h = await boot({ "DELETE /api/jobs/:id": () => ({ success: true }) });
  const err = fin({ id: "err-1", videoId: "e1", status: "Error" });
  h.app.jobs = [err];
  h.app.renderJobs();
  h.app.showConfirm = async () => {
    h.app.handleMessage({ type: "job_update", payload: { ...err, status: "Downloading" } });
    return true;
  };
  await h.app.deleteJob("err-1");
  await h.flush();
  assert.deepEqual(h.fetchLog.filter((c) => c.method === "DELETE").map((c) => c.url), []);
});

test("a single cancel does not reach a job that finished while the confirm was open", async () => {
  const h = await boot({ "POST /api/jobs/:id/cancel": () => ({ success: true }) });
  const dl = live({ id: "dl-1", videoId: "d1", status: "Downloading" });
  h.app.jobs = [dl];
  h.app.renderJobs();
  h.app.showConfirm = async () => {
    h.app.handleMessage({ type: "job_update", payload: { ...dl, status: "Finished" } });
    return true;
  };
  await h.app.cancelJob("dl-1");
  await h.flush();
  assert.deepEqual(h.fetchLog.filter((c) => c.url.endsWith("/cancel")).map((c) => c.url), []);
});
