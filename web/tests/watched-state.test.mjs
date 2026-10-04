// Mark Watched / Mark Unwatched, from the details dialog and from the batch
// bar, against the rows the hub never restates.
//
// Both watched routes answer with the updated row, and the dialog threw that
// answer away and waited for a job_update instead — which never comes for an
// archived Finished row (cmd/moombox/monitor_callbacks.go gates the broadcast
// on jobfilter.IsArchivedAt), so with hide_finished_age_days = 0 the pill, the
// buttons and the card's eye stayed stale until the dialog was reopened. The
// batch bar had the same gap one level up: it counted res.ok and relied on a
// jobs_update that restates the active list only.
//
// The harness's WebSocket never connects, so every test here is the
// "nothing arrives" case by construction: whatever the dashboard shows after
// the click, it learned from the route's answer alone.
//
// Like app.test.mjs, this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every test is
// skipped (not failed) when jsdom is absent. Only an absent module is a skip.
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
const inputs = jsdomMissing ? null : await import("./fixtures/app-render-inputs.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

/** A Finished job old enough to have aged into archivedJobs, not yet watched. */
const archived = (over = {}) => ({
  ...inputs.JOBS.Finished,
  id: "old-1",
  videoId: "old1vid",
  watched: false,
  incompleteTail: false,
  updatedAt: harness.agoISO(30 * 86400),
  // The enriched fields the dialog's own GET /api/jobs/{id} seeds; a route's
  // answer is a raw row without them, so they must be carried forward.
  hasStaging: false,
  hasSegments: true,
  ...over,
});

/** The row the watched route answers with: the same job, watched flipped, resume cleared. */
const answer = (job, watched) => ({ ...job, watched, resumePosition: null, updatedAt: new Date().toISOString() });

const card = (h, id) => h.el("archived-container").querySelector(`.video-item[data-job-id="${id}"]`);
const pill = (h) => h.el("job-details-content").querySelector(".watch-pill");

/** Boot with the archived panel showing `jobs`, and the dialog open on the first. */
async function openArchived(jobs, routes = {}) {
  const h = await harness.makeApp({ routes: { "GET /api/jobs/:id/logs": () => [], ...routes } });
  h.document.querySelector('sl-tab-panel[name="archived"]').setAttribute("active", "");
  h.app._activePanel = "archived";
  h.app.archivedJobs = jobs;
  h.app.renderArchivedJobs();
  h.app.selectedJobId = jobs[0].id;
  h.app.details.renderJobDetails(jobs[0]);
  return h;
}

// MUTANT: ignore the response body again (the shipped handler) — the dialog
// still shows Mark Watched, no pill, and the card has no eye, because the
// job_update it was waiting on is exactly the one the hub withholds.
test("Mark Watched applies the row the route answers with: dialog, list and card", { skip }, async () => {
  const job = archived();
  const h = await openArchived([job], {
    "POST /api/jobs/:id/watched": () => answer(job, true),
  });
  assert.ok(h.el("details-mark-watched"), "precondition: the dialog offers Mark Watched");
  assert.equal(pill(h), null, "precondition: no pill before the click");
  assert.equal(card(h, job.id).querySelector(".watch-indicator"), null, "precondition: no eye on the card");

  h.el("details-mark-watched").click();
  await h.flush();

  assert.ok(h.fetchLog.some((c) => c.method === "POST" && c.url === "/api/jobs/old-1/watched"));
  assert.equal(h.el("details-mark-watched"), null, "Mark Watched must give way once the job is watched");
  assert.ok(h.el("details-mark-unwatched"), "Mark Unwatched must take its place");
  assert.ok(pill(h)?.classList.contains("watched"), "the dialog's pill must say Watched");
  assert.ok(card(h, job.id).querySelector(".watch-indicator.watched"), "the card's eye must appear");

  const held = h.app.archivedJobs.find((j) => j.id === job.id);
  assert.equal(held.watched, true, "archivedJobs must hold the answered row");
  assert.equal(held.hasSegments, true,
    "the raw row the route answers with must not drop the enriched fields the dialog fetched");
  assert.ok(
    !h.fetchLog.some((c) => c.method === "GET" && c.url === "/api/jobs/old-1"),
    "the answer is the refresh — no second fetch of the job",
  );
});

// MUTANT: apply `watched` but not the cleared resume position — the pill keeps
// reading "Paused at" for a job the server no longer holds a position for.
test("Mark Unwatched clears the pill and the resume position it carried", { skip }, async () => {
  const job = archived({ watched: true, resumePosition: 125 });
  const h = await openArchived([job], {
    "DELETE /api/jobs/:id/watched": () => answer(job, false),
  });
  assert.match(pill(h).textContent, /Watched · Paused at/, "precondition: watched with a position");

  h.el("details-mark-unwatched").click();
  await h.flush();

  assert.ok(h.fetchLog.some((c) => c.method === "DELETE" && c.url === "/api/jobs/old-1/watched"));
  assert.equal(pill(h), null, "neither Watched nor Paused at may remain");
  assert.ok(h.el("details-mark-watched"), "Mark Watched must be offered again");
  assert.equal(h.el("details-mark-unwatched"), null);
  assert.equal(card(h, job.id).querySelector(".watch-indicator"), null, "the card's eye must go");
  const held = h.app.archivedJobs.find((j) => j.id === job.id);
  assert.equal(held.watched, false);
  assert.equal(held.resumePosition, null);
});

// MUTANT: apply the flip optimistically before the answer — a refused write
// shows a state the server does not hold.
test("a refused answer toasts and changes nothing", { skip }, async () => {
  const job = archived();
  const h = await openArchived([job], {
    "POST /api/jobs/:id/watched": () => harness.response({ status: 404, body: { error: "job not found" } }),
  });

  h.el("details-mark-watched").click();
  await h.flush();

  assert.ok(h.toasts().some((t) => t.textContent.includes("Failed to mark watched")));
  assert.ok(h.el("details-mark-watched"), "the button must stay");
  assert.equal(pill(h), null);
  assert.equal(h.app.archivedJobs[0].watched, false);
  assert.strictEqual(h.app.archivedJobs[0], job, "the held row must not have been replaced");
});

// The other way the dialog can be stale: a job_update DOES arrive, but for a
// job this tab holds only in archivedJobs (a deep link, or an archived row
// whose updated_at moved back inside the active window). That takes the
// upsert branch, which only the found-index branch's dialog refresh covered.
//
// MUTANT: put the dialog refresh back inside the found-index branch — the
// pill stays unwatched. MUTANT: drop the archived copy's _preserveStagingFields
// — the refresh rebuilds on the raw row and the Set-aside Recordings section
// vanishes, the same failure job-asides.test.mjs pins for the other branch.
test("a job_update for a job held only in archivedJobs refreshes the open dialog", { skip }, async () => {
  const job = archived({
    asides: [{ path: "D:\\staging\\old-1\\video.mp4.restart-1700000000", size: 1024, timestamp: "2023-11-14T22:13:20Z", hasResumeSidecar: true }],
    keptChatSidecar: false,
  });
  const h = await openArchived([job]);
  assert.ok(h.el("details-asides-section"), "precondition: the section renders from the enriched row");
  assert.equal(pill(h), null);

  // The raw row the hub broadcasts — none of the enriched fields.
  const raw = { ...job, watched: true, updatedAt: new Date().toISOString() };
  for (const k of ["asides", "keptChatSidecar", "hasStaging", "hasSegments"]) delete raw[k];
  h.app.handleMessage({ type: "job_update", payload: raw });
  await h.flush();

  assert.ok(pill(h)?.classList.contains("watched"), "the open dialog must follow the update");
  assert.ok(h.el("details-mark-unwatched"));
  assert.ok(h.el("details-asides-section"), "the asides must survive the move out of archivedJobs");
  assert.equal(h.app.jobs.find((j) => j.id === job.id)?.hasSegments, true);
  assert.equal(h.app.archivedJobs.length, 0, "the archived copy must be pruned, not shown twice");
});

// MUTANT: count res.ok and stop (the shipped batch action) — both archived
// cards keep their empty thumbnails and their checked boxes, with a toast
// saying "Marked watched: 2 jobs" above them.
test("the batch Watched action patches archived rows and redraws the panel", { skip }, async () => {
  const a = archived({ id: "old-1" });
  const b = archived({ id: "old-2", videoId: "old2vid" });
  const h = await openArchived([a, b], {
    "POST /api/jobs/batch/watched": () => ({ success: true }),
  });
  h.el("details-dialog").hide();
  h.app.selectedJobId = null;
  h.app._selectedArchivedJobs.add(a.id).add(b.id);
  h.app.renderArchivedJobs(); // re-applies the selection to the cards, as a click would
  assert.equal(card(h, a.id).querySelector(".job-checkbox").checked, true, "precondition: selected");
  h.app.showConfirm = async () => true;

  await h.app.batchAction("watched");
  await h.flush();

  const call = h.fetchLog.find((c) => c.method === "POST" && c.url === "/api/jobs/batch/watched");
  assert.deepEqual(call?.body, { jobIds: ["old-1", "old-2"] });
  for (const j of [a, b]) {
    assert.equal(j.watched, true, `${j.id} must be patched locally`);
    assert.ok(card(h, j.id).querySelector(".watch-indicator.watched"), `${j.id}'s card must show the eye`);
    assert.equal(card(h, j.id).querySelector(".job-checkbox").checked, false,
      `${j.id}'s box must be unchecked once the selection is cleared`);
  }
  assert.equal(h.app._selectedArchivedJobs.size, 0);
  assert.ok(h.toasts().some((t) => t.textContent.includes("Marked watched: 2 jobs")));
});

// The Tasks panel takes the other redraw, and Unwatched clears the resume
// position too. MUTANT: redraw the archived panel unconditionally — the Tasks
// cards keep their eyes.
test("the batch Unwatched action patches active rows and redraws the Tasks panel", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "DELETE /api/jobs/batch/watched": () => ({ success: true }) },
  });
  h.document.querySelector('sl-tab-panel[name="tasks"]').setAttribute("active", "");
  const a = { ...inputs.JOBS.Finished, id: "fin-1", videoId: "fin1", watched: true, updatedAt: harness.agoISO(60) };
  const b = { ...inputs.JOBS.Finished, id: "fin-2", videoId: "fin2", watched: false, resumePosition: 42, updatedAt: harness.agoISO(60) };
  h.app.jobs = [a, b];
  h.app.renderJobs();
  h.app._selectedTaskJobs.add(a.id).add(b.id);
  h.app.showConfirm = async () => true;
  const cardOf = (id) => h.el("jobs-container").querySelector(`.video-item[data-job-id="${id}"]`);
  assert.ok(cardOf(a.id).querySelector(".watch-indicator"), "precondition: an eye to remove");

  await h.app.batchAction("unwatched");
  await h.flush();

  const call = h.fetchLog.find((c) => c.method === "DELETE" && c.url === "/api/jobs/batch/watched");
  assert.deepEqual(call?.body, { jobIds: ["fin-1", "fin-2"] });
  assert.deepEqual([a.watched, a.resumePosition, b.watched, b.resumePosition], [false, null, false, null]);
  assert.equal(cardOf(a.id).querySelector(".watch-indicator"), null, "the eye must go");
  assert.equal(cardOf(b.id).querySelector(".watch-indicator"), null, "so must the in-progress mark");
  assert.equal(h.app._selectedTaskJobs.size, 0);
});

// MUTANT: patch the rows before the answer, or regardless of it — a refused
// batch leaves the panel claiming a state the server refused to write.
test("a refused batch leaves the rows alone", { skip }, async () => {
  const a = archived({ id: "old-1" });
  const h = await openArchived([a], {
    "POST /api/jobs/batch/watched": () => harness.response({ status: 500, body: { error: "failed to update jobs" } }),
  });
  h.el("details-dialog").hide();
  h.app.selectedJobId = null;
  h.app._selectedArchivedJobs.add(a.id);
  h.app.showConfirm = async () => true;

  await h.app.batchAction("watched");
  await h.flush();

  assert.equal(a.watched, false);
  assert.equal(card(h, a.id).querySelector(".watch-indicator"), null);
  assert.ok(h.toasts().some((t) => t.textContent.includes("1 failed")));
});
