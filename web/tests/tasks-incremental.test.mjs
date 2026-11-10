// The Tasks list's cheap paths (improvement sweep web-2 and web-4): the sort's
// precomputed keys, the lazily computed channel list, single-card removal and
// re-placement, and the hidden-tab repaint. Each is an OPTIMISATION, so each is
// pinned against what the full renderJobs() rebuild draws for the same events.
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

const job = (id, title, status = "Downloading", extra = {}) => ({
  id,
  videoId: `vid-${id}`,
  title,
  channelName: "Chan",
  platform: "youtube",
  status,
  progress: status === "Downloading" ? "V:1 A:1 C:1" : "",
  percent: 0,
  updatedAt: harness ? harness.agoISO(30) : "",
  ...extra,
});

// ── The sort ────────────────────────────────────────────────────────────────

/** _sortJobs as it was before the keys were precomputed — the reference. */
function referenceSort(jobs) {
  const PRI = {
    "Error": 0, "COOKIES?": 1, "Downloading": 2, "Muxing": 3,
    "Live": 4, "Upcoming": 5, "Queued": 6, "Cancelled": 7, "Finished": 8,
  };
  return [...jobs].sort((a, b) => {
    const pa = PRI[a.status] ?? 99;
    const pb = PRI[b.status] ?? 99;
    if (pa !== pb) return pa - pb;
    if (pa >= 7) return new Date(b.updatedAt) - new Date(a.updatedAt);
    return (a.title || "").localeCompare(b.title || "", undefined, { sensitivity: "base" });
  });
}

// MUTANT: compare titles with `<` instead of the collator — "apple" sorts
// after "Zebra" and accented titles leave their base letter.
// MUTANT: sort completed rows oldest-first (`a.time - b.time`).
// MUTANT: key every row by title — the Finished group loses its newest-first
// order.
test("_sortJobs orders exactly as the per-comparison Date and localeCompare sort did", { skip }, async () => {
  const h = await harness.makeApp();
  const statuses = ["Error", "COOKIES?", "Downloading", "Muxing", "Live", "Upcoming", "Queued", "Cancelled", "Finished", "Odd"];
  const titles = ["apple", "Zebra", "Äpfel", "apple", "APPLE 2", "éclair", "eclair", "", undefined, "10 things", "9 things", "ゲーム", "zebra"];
  // A fixed pseudo-random walk, so a failure reproduces.
  let seed = 12345;
  const next = (n) => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed % n; };
  const jobs = [];
  for (let i = 0; i < 400; i++) {
    jobs.push({
      id: `j${i}`,
      status: statuses[next(statuses.length)],
      title: titles[next(titles.length)],
      updatedAt: harness.agoISO(next(40) * 3600),
    });
  }

  const got = h.app._sortJobs(jobs).map((j) => j.id);
  assert.deepEqual(got, referenceSort(jobs).map((j) => j.id),
    "the precomputed keys must give the order the old comparator gave, ties and all");
  assert.equal(jobs[0].id, "j0", "_sortJobs must not reorder the array it was handed");
});

// ── The channel list ────────────────────────────────────────────────────────

// MUTANT: keep a list cached at render time — a job that arrived through the
// single-card paths (no full render) is missing from the dropdown.
test("the filter dropdown's channels come from the jobs held when it is asked", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [job("a", "AAA", "Downloading", { channelName: "beta" }), job("b", "BBB", "Downloading", { channelName: "Alpha" })];
  h.app.renderJobs();
  h.app.jobs.push(job("c", "CCC", "Downloading", { channelName: "alpha 2" }), job("d", "DDD", "Downloading", { channelName: "beta" }),
    job("e", "EEE", "Downloading", { channelName: "" }));
  assert.deepEqual(h.app.filterBar.channels("jobs"), ["Alpha", "alpha 2", "beta"],
    "deduplicated, empty names dropped, sorted without regard to case, and current without a render");

  h.app.archivedJobs = [job("z", "ZZZ", "Finished", { channelName: "Old Chan" })];
  assert.deepEqual(h.app.filterBar.channels("archived"), ["Old Chan"], "the Archived bar lists the archived jobs' channels");
});

// ── The active indicator off the tick ───────────────────────────────────────

// MUTANT: put `this.stats.updateActiveIndicator(this.jobs)` back in the
// unchanged-status branches — an O(n) recount per progress frame.
test("a same-status frame does not recount the active jobs, a status change does", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [job("a", "AAA"), job("b", "BBB", "Upcoming")];
  h.app.renderJobs();
  assert.equal(h.el("active-indicator").textContent, "▶ 1");

  let recounts = 0;
  const real = h.app.stats.updateActiveIndicator.bind(h.app.stats);
  h.app.stats.updateActiveIndicator = (...a) => { recounts++; return real(...a); };

  h.app.handleMessage({ type: "job_progress", payload: { id: "a", status: "Downloading", progress: "V:2 A:2 C:2" } });
  h.app.handleMessage({ type: "job_update", payload: { ...job("a", "AAA"), progress: "V:3 A:3 C:3" } });
  assert.equal(recounts, 0, "no status moved, so the count cannot have");
  assert.equal(h.document.querySelector('.video-item[data-job-id="a"] .job-progress-text').textContent, "V:3 A:3 C:3",
    "the card itself still takes every frame");

  h.app.handleMessage({ type: "job_update", payload: job("b", "BBB", "Downloading") });
  assert.equal(h.el("active-indicator").textContent, "▶ 2", "a status change still reaches the status bar");
});

// ── Queued hold lines ───────────────────────────────────────────────────────

// MUTANT: one document.querySelector per Queued job again — O(Q × N) on a
// backlog of thousands.
test("a holds change redraws every Queued row in one pass over the cards", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [job("a", "AAA"), job("q1", "Q one", "Queued"), job("q2", "Q two", "Queued"), job("q3", "Q three", "Queued")];
  h.app.renderJobs();
  const before = h.document.querySelector('.video-item[data-job-id="q2"] .job-progress-text').textContent;

  let single = 0;
  const realQS = h.document.querySelector;
  h.document.querySelector = function (...a) { single++; return realQS.apply(this, a); };
  h.app.setAdmissionHolds({ offline: true });
  delete h.document.querySelector;

  assert.equal(single, 0, "no per-job document query");
  for (const id of ["q1", "q2", "q3"]) {
    const cell = h.document.querySelector(`.video-item[data-job-id="${id}"] .job-progress-text`);
    assert.notEqual(cell.textContent, before, `Queued row ${id} must show the hold`);
    assert.equal(cell.title, h.app.formatProgress(h.app.jobs.find((j) => j.id === id)), `Queued row ${id}'s tooltip follows`);
  }
  assert.equal(h.document.querySelector('.video-item[data-job-id="a"] .job-progress-text').textContent, "V:1 A:1 C:1",
    "a row that is not Queued is left alone");
});
