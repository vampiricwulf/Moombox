// The Archived panel: one list held in memory, the newest 200 cards drawn,
// a fetch that cannot wipe the list, and the same Arrow/Enter keys as Tasks.
//
// Needs jsdom, like the other DOM suites; skipped without it.
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

// Row n is n minutes old, so row 0 is the newest and sorts first.
const row = (n, over = {}) => ({
  id: `a${n}`, videoId: `a${n}`, title: `Archived ${n}`, channelName: n % 2 ? "Odd" : "Even",
  platform: "youtube", status: "Finished", updatedAt: harness.agoISO(86400 * 30 + n * 60), ...over,
});
const rows = (count) => Array.from({ length: count }, (_, n) => row(n));

const cards = (h) => [...h.el("archived-container").querySelectorAll(".video-item")];
const shown = (el) => el.style.display !== "none";
const key = (h, k, target) => (target || h.document.body).dispatchEvent(
  new h.window.KeyboardEvent("keydown", { key: k, bubbles: true, cancelable: true, composed: true }));

async function open(routes = {}) {
  const h = await harness.makeApp({ routes });
  h.app._activePanel = "archived";
  return h;
}

// A gate a route can wait on, so a test decides when an answer lands.
function gate() {
  let release;
  const p = new Promise((r) => { release = r; });
  return { p, release };
}

// Mutant: the failure branch assigns [] again.
test("a failed fetch keeps the held list and the selection, and offers Retry", { skip }, async () => {
  let fail = false;
  const h = await open({
    "GET /api/jobs/archived": () => (fail ? harness.response({ status: 500 }) : rows(3)),
  });
  await h.app.fetchArchivedJobs();
  assert.equal(cards(h).length, 3);
  cards(h)[1].querySelector(".job-checkbox").click();
  assert.deepEqual([...h.app._selectedArchivedJobs], ["a1"]);

  fail = true;
  await h.app.fetchArchivedJobs();
  assert.equal(h.app.archivedJobs.length, 3, "a 500 emptied the held list");
  assert.equal(cards(h).length, 3);
  assert.deepEqual([...h.app._selectedArchivedJobs], ["a1"], "a 500 dropped the selection");
  assert.ok(shown(h.el("archived-fetch-error")), "no error notice");
  assert.ok(!shown(h.el("archived-empty-state")), "a failed fetch reads as an empty archive");

  fail = false;
  const before = h.http.matching("/api/jobs/archived").length;
  h.el("archived-retry").click();
  await h.flush(); await h.flush();
  assert.equal(h.http.matching("/api/jobs/archived").length, before + 1, "Retry did not refetch");
  assert.ok(!shown(h.el("archived-fetch-error")), "the notice outlived a good answer");
});

test("a first fetch that fails shows the error, not 'No archived jobs'", { skip }, async () => {
  const h = await open({ "GET /api/jobs/archived": () => { throw new Error("offline"); } });
  const realError = console.error;
  console.error = () => {};
  try { await h.app.fetchArchivedJobs(); } finally { console.error = realError; }
  assert.ok(shown(h.el("archived-fetch-error")));
  assert.ok(!shown(h.el("archived-empty-state")));
  assert.ok(!shown(h.el("archived-loading")));
});

test("the first open shows a loading state until the answer lands", { skip }, async () => {
  const g = gate();
  const h = await open({ "GET /api/jobs/archived": () => g.p.then(() => []) });
  const done = h.app.fetchArchivedJobs();
  assert.ok(shown(h.el("archived-loading")), "no loading state while the first fetch is out");
  assert.ok(!shown(h.el("archived-empty-state")), "the empty state shows before anything was loaded");
  g.release();
  await done;
  assert.ok(!shown(h.el("archived-loading")));
  assert.ok(shown(h.el("archived-empty-state")), "an empty archive must say so once loaded");
});

// Mutant: the generation check removed — the list ends at one row.
test("a late answer is dropped", { skip }, async () => {
  const first = gate();
  let n = 0;
  const h = await open({
    "GET /api/jobs/archived": () => (++n === 1 ? first.p.then(() => rows(1)) : rows(3)),
  });
  const slow = h.app.fetchArchivedJobs();
  await h.app.fetchArchivedJobs();
  assert.equal(h.app.archivedJobs.length, 3);
  first.release();
  await slow;
  assert.equal(h.app.archivedJobs.length, 3, "the older answer overwrote the newer one");
  assert.equal(cards(h).length, 3);
});

test("a row deleted while a fetch is out does not come back with the answer", { skip }, async () => {
  const g = gate();
  const h = await open({ "GET /api/jobs/archived": () => g.p.then(() => rows(3)) });
  h.app.archivedJobs = rows(3);
  h.app.renderArchivedJobs();
  const done = h.app.fetchArchivedJobs();
  h.app.handleMessage({ type: "job_deleted", payload: { id: "a1" } });
  g.release();
  await done;
  assert.deepEqual(h.app.archivedJobs.map((j) => j.id), ["a0", "a2"], "the deleted row is back as a ghost");
});

test("the newest 200 cards are drawn and Show more draws the next 200", { skip }, async () => {
  const h = await open({ "GET /api/jobs/archived": () => rows(450) });
  await h.app.fetchArchivedJobs();
  assert.equal(cards(h).length, 200);
  assert.equal(cards(h)[0].dataset.jobId, "a0", "the newest row is not first");
  assert.equal(cards(h)[199].dataset.jobId, "a199");
  assert.ok(shown(h.el("archived-more")));
  assert.match(h.el("archived-more-count").textContent, /200 of 450/);

  h.el("archived-show-more").click();
  assert.equal(cards(h).length, 400);
  h.el("archived-show-more").click();
  assert.equal(cards(h).length, 450);
  assert.ok(!shown(h.el("archived-more")), "Show more stays once everything is drawn");
});

test("a short list has no Show more", { skip }, async () => {
  const h = await open({ "GET /api/jobs/archived": () => rows(5) });
  await h.app.fetchArchivedJobs();
  assert.ok(!shown(h.el("archived-more")));
});

test("the filter and its count act on every row, drawn or not", { skip }, async () => {
  const h = await open({ "GET /api/jobs/archived": () => [...rows(300), row(999, { title: "Needle in the tail" })] });
  await h.app.fetchArchivedJobs();
  assert.equal(h.el("archived-container").querySelector('[data-job-id="a999"]'), null, "precondition: the row is past the drawn 200");
  h.app.filterBar.archivedFilterTokens = [{ type: "text", value: "needle" }];
  h.app.renderArchivedJobs();
  assert.deepEqual(cards(h).map((c) => c.dataset.jobId), ["a999"]);
  assert.equal(h.el("archived-filter-count").textContent, "1 of 301");
});

test("Select All selects every matching row and the bar says how many are not drawn", { skip }, async () => {
  const h = await open({ "GET /api/jobs/archived": () => rows(450) });
  await h.app.fetchArchivedJobs();
  cards(h)[0].querySelector(".job-checkbox").click();
  assert.equal(h.el("batch-count").textContent, "1 selected");
  assert.equal(h.el("batch-select-all").textContent, "Select All (450)");

  h.el("batch-select-all").click();
  assert.equal(h.app._selectedArchivedJobs.size, 450);
  assert.equal(h.app._getSelectedJobs().length, 450);
  assert.equal(h.el("batch-count").textContent, "450 selected (250 not shown yet)");

  // A card drawn later arrives ticked.
  h.el("archived-show-more").click();
  assert.ok(cards(h)[300].querySelector(".job-checkbox").checked);
  assert.ok(cards(h)[300].classList.contains("selected"));
  assert.equal(h.el("batch-count").textContent, "450 selected (50 not shown yet)");

  // And a redraw keeps the selection of rows it does not draw.
  h.app.renderArchivedJobs();
  assert.equal(h.app._selectedArchivedJobs.size, 450);
});

test("opening the tab again starts from the newest 200", { skip }, async () => {
  const h = await open({ "GET /api/jobs/archived": () => rows(450) });
  await h.app.fetchArchivedJobs();
  h.el("archived-show-more").click();
  assert.equal(cards(h).length, 400);
  const tabs = h.document.querySelector("sl-tab-group");
  tabs.dispatchEvent(new h.window.CustomEvent("sl-tab-show", { detail: { name: "tasks" } }));
  tabs.dispatchEvent(new h.window.CustomEvent("sl-tab-show", { detail: { name: "archived" } }));
  await h.flush(); await h.flush();
  assert.equal(cards(h).length, 200);
});

// ── Keyboard: the same Arrow and Enter as the Tasks rows ────────────────────

// Mutant: the key handler's panel check back to Tasks only.
test("Arrow keys walk the Archived rows and Enter opens the focused one", { skip }, async () => {
  const h = await open({ "GET /api/jobs/archived": () => rows(3) });
  await h.app.fetchArchivedJobs();
  const opened = [];
  h.app.details.showJobDetails = (job) => opened.push(job.id);

  key(h, "ArrowDown");
  key(h, "ArrowDown");
  assert.equal(h.app.focusedArchivedIndex, 1);
  assert.deepEqual(cards(h).filter((c) => c.hasAttribute("data-focused")).map((c) => c.dataset.jobId), ["a1"]);
  assert.equal(h.app.focusedJobIndex, -1, "the Tasks focus moved with the Archived one");

  key(h, "Enter");
  assert.deepEqual(opened, ["a1"]);

  key(h, "ArrowUp");
  key(h, "ArrowUp");
  assert.equal(h.app.focusedArchivedIndex, 2, "ArrowUp from the first row wraps to the last");

  // Enter on a control belongs to the control.
  key(h, "Enter", h.el("add-video-btn"));
  assert.deepEqual(opened, ["a1"]);
});

test("the focused Archived row survives a redraw and follows its job", { skip }, async () => {
  const h = await open({ "GET /api/jobs/archived": () => rows(3) });
  await h.app.fetchArchivedJobs();
  key(h, "ArrowDown");
  key(h, "ArrowDown"); // a1
  h.app.handleMessage({ type: "job_deleted", payload: { id: "a0" } });
  assert.equal(h.app.focusedArchivedIndex, 0);
  assert.ok(h.el("archived-container").querySelector('[data-job-id="a1"]').hasAttribute("data-focused"));
  h.app.handleMessage({ type: "job_deleted", payload: { id: "a1" } });
  assert.equal(h.app.focusedArchivedIndex, -1, "a deleted focused row leaves no focus");
});

test("Arrow navigation stays inside the drawn rows", { skip }, async () => {
  const h = await open({ "GET /api/jobs/archived": () => rows(450) });
  await h.app.fetchArchivedJobs();
  key(h, "ArrowUp");
  assert.equal(h.app.focusedArchivedIndex, 199);
  key(h, "ArrowDown");
  assert.equal(h.app.focusedArchivedIndex, 0);
});

test("Arrow keys on Archived leave the Tasks focus alone", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "GET /api/jobs/archived": () => rows(2) } });
  h.app.jobs = [row(50, { id: "t1", status: "Downloading" }), row(51, { id: "t2", status: "Downloading" })];
  h.app.renderJobs();
  key(h, "ArrowDown");
  assert.equal(h.app.focusedJobIndex, 0);
  h.app._activePanel = "archived";
  await h.app.fetchArchivedJobs();
  key(h, "ArrowDown");
  assert.equal(h.app.focusedJobIndex, 0);
  assert.ok(h.el("jobs-container").querySelector(".video-item[data-focused]"), "the Tasks highlight was cleared");
});
