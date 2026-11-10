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
  const next = (n) => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return (seed >>> 8) % n; };
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

// ── One card out, one card moved ────────────────────────────────────────────

/** An app whose single-card paths all decline: every event takes the full rebuild. */
async function rebuildOnlyApp() {
  const h = await harness.makeApp();
  h.app._insertJobCard = () => false;
  h.app._moveJobCard = () => false;
  h.app._removeJobCard = () => false;
  return h;
}

/**
 * Everything the operator can see or act on in the Tasks list. `data-progress`
 * is left out of the markup: it is updateJobCard's per-card write cache, set
 * on a card's first tick and absent from a freshly drawn one, never content.
 * So is a checkbox's `checked` ATTRIBUTE: a card drawn while selected carries
 * it, a card ticked after it was drawn holds only the property. The property
 * is what shows, and `ticked` compares it.
 */
function tasksSnapshot(h) {
  const container = h.el("jobs-container");
  const clone = container.cloneNode(true);
  for (const el of clone.querySelectorAll("[data-progress]")) el.removeAttribute("data-progress");
  // updateJobCard notes on the thumbnail whether the row had a local file.
  for (const el of clone.querySelectorAll("[data-had-thumb-file]")) el.removeAttribute("data-had-thumb-file");
  for (const el of clone.querySelectorAll(".job-checkbox[checked]")) el.removeAttribute("checked");
  const cards = [...container.querySelectorAll(".video-item")];
  const bar = h.el("batch-action-bar");
  return {
    html: clone.innerHTML,
    order: cards.map((c) => c.dataset.jobId),
    focusedJobIndex: h.app.focusedJobIndex,
    focusedCards: cards.filter((c) => c.hasAttribute("data-focused")).map((c) => c.dataset.jobId),
    selected: [...h.app._selectedTaskJobs].sort(),
    ticked: cards.filter((c) => c.querySelector(".job-checkbox").checked).map((c) => c.dataset.jobId),
    highlighted: cards.filter((c) => c.classList.contains("selected")).map((c) => c.dataset.jobId),
    batchBar: bar ? `${bar.className}|${bar.textContent.replace(/\s+/g, " ").trim()}` : "",
    batchButtons: bar ? [...bar.querySelectorAll("sl-button")].map((b) => `${b.id}:${b.style.display}:${b.disabled ? "off" : "on"}`) : [],
    activeIndicator: `${h.el("active-indicator").style.display}|${h.el("active-indicator").textContent}`,
    emptyState: h.el("empty-state").style.display,
    filterCount: h.el("tasks-filter-count")?.style.display,
    channels: h.app.filterBar.channels("jobs"),
    held: h.app.jobs.map((j) => `${j.id}:${j.status}`).sort(),
  };
}

const STATUSES = ["Queued", "Upcoming", "Live", "Downloading", "Muxing", "Finished", "Error", "Cancelled", "COOKIES?"];

/**
 * A reproducible stream of the events the Tasks list takes: discoveries,
 * status changes (through job_update and through a status-carrying
 * job_progress), renames, ticks, shape-only changes, deletes, and the
 * operator moving the focus and ticking rows in between.
 */
function eventStream(steps, seedValue) {
  let seed = seedValue;
  const next = (n) => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return (seed >>> 8) % n; };
  const rows = new Map();
  let made = 0;
  let clock = 5000;
  const stamp = () => harness.agoISO(clock--); // every write is newer than the last
  const fresh = () => {
    const id = `j${made++}`;
    const row = job(id, `Title ${String.fromCharCode(65 + next(26))}${next(50)}`, STATUSES[next(STATUSES.length)], {
      channelName: `Chan ${next(4)}`, platform: next(3) === 0 ? "twitch" : "youtube", updatedAt: stamp(),
    });
    rows.set(id, row);
    return row;
  };
  const initial = [];
  for (let i = 0; i < 14; i++) initial.push({ ...fresh() });
  const pick = () => { const ids = [...rows.keys()]; return rows.get(ids[next(ids.length)]); };

  const events = [];
  for (let i = 0; i < steps; i++) {
    const kind = next(100);
    if (rows.size < 4 || kind < 12) {
      events.push({ frame: { type: "job_update", payload: { ...fresh() } } });
    } else if (kind < 40) {
      const row = pick();
      row.status = STATUSES[next(STATUSES.length)];
      row.progress = row.status === "Downloading" ? "V:1 A:1 C:1" : "";
      row.updatedAt = stamp();
      if (row.status === "Finished") row.incompleteTail = next(3) === 0;
      events.push({ frame: { type: "job_update", payload: { ...row } } });
    } else if (kind < 48) {
      const row = pick();
      row.status = STATUSES[next(STATUSES.length)];
      row.updatedAt = stamp();
      events.push({ frame: { type: "job_progress", payload: { id: row.id, status: row.status, progress: row.progress, updatedAt: row.updatedAt } } });
    } else if (kind < 56) {
      const row = pick();
      row.title = `Renamed ${String.fromCharCode(65 + next(26))}${next(50)}`;
      row.updatedAt = stamp();
      events.push({ frame: { type: "job_update", payload: { ...row } } });
    } else if (kind < 70) {
      const row = pick();
      row.progress = `V:${i} A:${i} C:${i}`;
      events.push({ frame: { type: "job_progress", payload: { id: row.id, status: row.status, progress: row.progress } } });
    } else if (kind < 76) {
      // Shape only: same status, title and updatedAt, a new hasStaging.
      const row = pick();
      row.hasStaging = !row.hasStaging;
      events.push({ frame: { type: "job_update", payload: { ...row } } });
    } else if (kind < 88) {
      const row = pick();
      rows.delete(row.id);
      events.push({ frame: { type: "job_deleted", payload: { id: row.id } } });
    } else if (kind < 94) {
      events.push({ focus: next(2) ? 1 : -1 });
    } else {
      events.push({ tick: pick().id });
    }
  }
  return { initial, events };
}

function applyEvent(h, ev) {
  if (ev.frame) {
    // A fresh copy each: the handler keeps the payload object as the row.
    h.app.handleMessage({ type: ev.frame.type, payload: { ...ev.frame.payload } });
  } else if (ev.focus) {
    h.app.navigateJobList(ev.focus);
  } else if (ev.tick) {
    h.document.querySelector(`.video-item[data-job-id="${ev.tick}"] .job-checkbox`)?.click();
  }
}

// THE pin for web-2 (b) and (c): for the same events, the single-card paths
// leave exactly what a full rebuild leaves, after every event.
//
// MUTANT: _detachJobCard drops the text run on the wrong side, or none — the
// markup differs from the rebuild by an indentation run.
// MUTANT: _moveJobCard does not put data-focused back — the highlight is lost
// when the focused row changes status.
// MUTANT: _removeJobCard keeps the deleted id in _selectedTaskJobs — the batch
// bar counts a row that is gone.
// MUTANT: _moveJobCard skips updateBatchActionBar — the bar keeps offering
// Cancel for a selected row that just finished.
// MUTANT: the status-change branch patches the card in place — the row keeps
// its old badge, actions and place.
// MUTANT: the same-status branch ignores _cardShape — a row keeps Reinitialize
// where a rebuild draws Resume.
test("the single-card paths leave what a full rebuild leaves, event by event", { skip }, async () => {
  for (const seedValue of [7, 20261110, 424242]) {
    const { initial, events } = eventStream(260, seedValue);
    // One app at a time: the harness publishes the newest window as the
    // globals app.js reads, so two apps cannot be driven side by side.
    const run = async (h) => {
      h.app.jobs = initial.map((j) => ({ ...j }));
      h.app.renderJobs();
      let rebuilds = 0;
      const realRender = h.app.renderJobs.bind(h.app);
      h.app.renderJobs = (...a) => { rebuilds++; return realRender(...a); };
      const shots = events.map((ev) => { applyEvent(h, ev); return tasksSnapshot(h); });
      return { shots, rebuilds };
    };
    const fast = await run(await harness.makeApp());
    const full = await run(await rebuildOnlyApp());
    const rebuilds = fast.rebuilds;
    assert.ok(full.rebuilds > 100, "the reference run must be the one that rebuilds");

    events.forEach((ev, i) => {
      assert.deepEqual(fast.shots[i], full.shots[i],
        `seed ${seedValue}, event ${i} (${JSON.stringify(ev)}): the list must be what a rebuild draws`);
    });
    assert.equal(rebuilds, 0,
      `seed ${seedValue}: with no filter and a list that never empties, no event needs the full rebuild ` +
      "— a fallback here means the comparison above proved nothing about the fast path");
  }
});

// MUTANT: the delete or status-change branch calls renderJobs() again — one
// renderJobItem per row in the list instead of none, or one.
test("a delete draws no card and a status change draws one", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [job("a", "AAA"), job("b", "BBB"), job("c", "CCC"), job("d", "DDD", "Queued")];
  h.app.renderJobs();
  const untouched = h.document.querySelector('.video-item[data-job-id="c"]');

  let items = 0;
  const realItem = h.app.renderJobItem.bind(h.app);
  h.app.renderJobItem = (...a) => { items++; return realItem(...a); };

  h.app.handleMessage({ type: "job_deleted", payload: { id: "b" } });
  assert.equal(items, 0, "a deleted row costs no card render");

  h.app.handleMessage({ type: "job_update", payload: job("d", "DDD", "Downloading") });
  assert.equal(items, 1, "a status change renders the one card that moved");
  assert.deepEqual([...h.document.querySelectorAll(".video-item")].map((e) => e.dataset.jobId), ["a", "c", "d"],
    "the moved card lands where _sortJobs puts it");
  assert.equal(h.document.querySelector('.video-item[data-job-id="c"]'), untouched,
    "a row that did not change keeps its element: its shadow roots, scroll and open menu survive");
  assert.equal(h.el("active-indicator").textContent, "▶ 3", "the active count follows");
});

// The fast paths are only safe while the rendered list and this.jobs are the
// same set. Each of these must take the rebuild.
test("removal and move fall back to a full render when they cannot be proven", { skip }, async () => {
  const start = () => [job("a", "AAA"), job("b", "BBB"), job("c", "CCC")];
  const counted = async () => {
    const h = await harness.makeApp();
    h.app.jobs = start();
    h.app.renderJobs();
    const c = { renders: 0 };
    const real = h.app.renderJobs.bind(h.app);
    h.app.renderJobs = (...a) => { c.renders++; return real(...a); };
    return { h, c };
  };

  // A filter is active: the match set and the "N of M" count change.
  {
    const { h, c } = await counted();
    h.app.filterBar.tasksFilterTokens = [{ type: "status", value: "Downloading", negated: false }];
    h.app.renderJobs();
    c.renders = 0;
    h.app.handleMessage({ type: "job_deleted", payload: { id: "b" } });
    assert.equal(c.renders, 1, "a delete under a filter rebuilds");
    assert.equal(h.el("tasks-filter-count").textContent, "2 of 2", "and the count is recomputed");
    h.app.handleMessage({ type: "job_update", payload: job("a", "AAA", "Muxing") });
    assert.equal(c.renders, 2, "a status change under a filter rebuilds");
    assert.deepEqual([...h.document.querySelectorAll(".video-item")].map((e) => e.dataset.jobId), ["c"],
      "the row that left the filter is gone from the list");
  }

  // The list empties: the empty state is the rebuild's to draw.
  {
    const { h, c } = await counted();
    h.app.handleMessage({ type: "job_deleted", payload: { id: "a" } });
    h.app.handleMessage({ type: "job_deleted", payload: { id: "b" } });
    assert.equal(c.renders, 0);
    h.app.handleMessage({ type: "job_deleted", payload: { id: "c" } });
    assert.equal(c.renders, 1, "the last delete rebuilds");
    assert.equal(h.el("empty-state").style.display, "flex", "and the empty state shows");
    assert.equal(h.el("jobs-container").innerHTML, "");
  }

  // The rendered list has drifted from this.jobs.
  {
    const { h, c } = await counted();
    h.document.querySelector('.video-item[data-job-id="c"]').remove();
    h.app.handleMessage({ type: "job_update", payload: job("a", "AAA", "Muxing") });
    assert.equal(c.renders, 1, "a drifted list rebuilds on a move");
    assert.deepEqual([...h.document.querySelectorAll(".video-item")].map((e) => e.dataset.jobId), ["b", "c", "a"],
      "and the rebuild reconciles it");
  }
  {
    const { h, c } = await counted();
    h.document.querySelector('.video-item[data-job-id="b"]').remove();
    h.app.handleMessage({ type: "job_deleted", payload: { id: "b" } }); // its card is already gone
    assert.equal(c.renders, 1, "a delete whose card is missing rebuilds");
  }
});

// MUTANT: _moveJobCard forgets the focus — Arrow and Enter then act on
// nothing, or (with a stale index) on the wrong row.
test("focus and selection follow a row that moves, and leave with one that is deleted", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [job("a", "AAA"), job("b", "BBB"), job("c", "CCC"), job("q", "QQQ", "Queued")];
  h.app.renderJobs();
  h.app.navigateJobList(1);
  h.app.navigateJobList(1); // focus on "b"
  h.document.querySelector('.video-item[data-job-id="b"] .job-checkbox').click();
  h.document.querySelector('.video-item[data-job-id="c"] .job-checkbox').click();
  assert.equal(h.app.focusedJobIndex, 1);

  h.app.handleMessage({ type: "job_update", payload: job("b", "BBB", "Queued") });
  const moved = h.document.querySelector('.video-item[data-job-id="b"]');
  assert.deepEqual([...h.document.querySelectorAll(".video-item")].map((e) => e.dataset.jobId), ["a", "c", "b", "q"]);
  assert.ok(moved.hasAttribute("data-focused"), "the highlight moves with the row");
  assert.equal(h.app.focusedJobIndex, 2, "and so does the index Arrow and Enter read");
  assert.ok(moved.classList.contains("selected") && moved.querySelector(".job-checkbox").checked, "the row stays ticked");

  h.app.handleMessage({ type: "job_deleted", payload: { id: "b" } });
  assert.equal(h.app.focusedJobIndex, -1, "a deleted focused row leaves no focus");
  assert.deepEqual([...h.app._selectedTaskJobs], ["c"], "and leaves the selection");

  h.app.navigateJobList(1); // "a"
  h.app.navigateJobList(1); // "c"
  h.app.handleMessage({ type: "job_deleted", payload: { id: "a" } });
  assert.equal(h.app.focusedJobIndex, 0, "a delete above the focused row shifts its index");
  assert.ok(h.document.querySelector('.video-item[data-job-id="c"]').hasAttribute("data-focused"));
});

// A part of the card updateJobCard does not patch used to be put right by the
// next rebuild of the whole list, which any row's status change caused. With
// those rebuilds gone, the one card is drawn again when that part changes.
//
// MUTANT: drop the _cardShape branch — the row keeps Reinitialize until the
// list is next rebuilt.
test("a same-status update that changes the row's actions draws that one card again", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [job("a", "AAA"), job("e", "EEE", "Error", { hasStaging: false })];
  h.app.renderJobs();
  const actions = () => [...h.document.querySelectorAll('.video-item[data-job-id="e"] .job-quick-actions sl-icon-button')]
    .map((b) => b.dataset.quickAction);
  assert.deepEqual(actions(), ["reinitialize", "delete"]);

  let items = 0;
  const realItem = h.app.renderJobItem.bind(h.app);
  h.app.renderJobItem = (...a) => { items++; return realItem(...a); };
  h.app.handleMessage({ type: "job_update", payload: job("e", "EEE", "Error", { hasStaging: true }) });

  assert.deepEqual(actions(), ["resume", "delete"], "the row now offers Resume");
  assert.equal(items, 1, "for the cost of one card");
});

// MUTANT: drop _reshapeJobCard from _fetchStagingFields — opening the details
// corrects the dialog's buttons and leaves the row's icon as it was.
test("the details fetch redraws the row when its answer changes the row's actions", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "GET /api/jobs/:id": ({ params }) => ({ ...job(params.id, "EEE", "Error"), hasStaging: true }) },
  });
  h.app.jobs = [job("a", "AAA"), job("e", "EEE", "Error", { hasStaging: false })];
  h.app.renderJobs();

  await h.app.details._fetchStagingFields("e");

  assert.deepEqual([...h.document.querySelectorAll('.video-item[data-job-id="e"] .job-quick-actions sl-icon-button')]
    .map((b) => b.dataset.quickAction), ["resume", "delete"]);
});

// ── The hidden tab (web-4) ──────────────────────────────────────────────────

/** Make this window's document report hidden or visible. */
function setHidden(h, hidden) {
  Object.defineProperty(h.document, "hidden", { configurable: true, get: () => hidden });
}

/** Count every node added, removed or rewritten under `root`, and attribute writes. */
function watchDom(h, root) {
  const records = [];
  const obs = new h.window.MutationObserver((list) => records.push(...list));
  obs.observe(root, { subtree: true, childList: true, characterData: true, attributes: true });
  return { stop() { records.push(...obs.takeRecords()); obs.disconnect(); return records.length; } };
}

const tickFrame = (id, n, extra = {}) => ({
  type: "job_progress",
  payload: { id, status: "Downloading", progress: `V:${n} A:${n} C:${n}`, percent: n, lastVideoSeq: n, ...extra },
});

// A dashboard left open in a background tab takes ~60 frames a second per
// active download, and WebSocket messages are not throttled there. The data
// path must not change: every frame is still merged into the row.
//
// MUTANT: no document.hidden branch — the card and the dialog are rewritten
// for nobody on every frame.
// MUTANT: `if (document.hidden) break;` ahead of the merge — the rows freeze
// while hidden, and everything that reads this.jobs goes stale.
// MUTANT: _repaintAfterHidden never called, or the flag never set — the tab
// comes back showing the progress it had when it was hidden.
test("progress frames in a hidden tab update the rows and draw nothing until it shows", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [job("a", "AAA", "Downloading", { isVod: true }), job("b", "BBB")];
  h.app.renderJobs();
  h.app.selectedJobId = "a";
  h.app.details.renderJobDetails(h.app.jobs[0]);
  h.app.handleMessage(tickFrame("a", 1));
  h.app.handleMessage(tickFrame("b", 1));
  await h.flush();
  const cell = (id) => h.document.querySelector(`.video-item[data-job-id="${id}"] .job-progress-text`).textContent;
  const dialog = () => h.el("job-details-content").querySelector('[data-field="progress"]').textContent;
  assert.equal(cell("a"), "V:1 A:1 C:1", "visible: a frame reaches the card as it arrives");

  setHidden(h, true);
  let renders = 0;
  const realRender = h.app.renderJobs.bind(h.app);
  h.app.renderJobs = (...a) => { renders++; return realRender(...a); };
  const watch = watchDom(h, h.document.body);
  for (let n = 2; n <= 40; n++) {
    h.app.handleMessage(tickFrame("a", n));
    h.app.handleMessage(tickFrame("b", n));
  }
  assert.equal(watch.stop(), 0, "hidden: 78 frames write nothing to the document");
  assert.equal(renders, 0);
  assert.equal(h.app.jobs.find((j) => j.id === "a").progress, "V:40 A:40 C:40", "the row took every frame");
  assert.equal(h.app.jobs.find((j) => j.id === "b").lastVideoSeq, 40);
  assert.equal(cell("a"), "V:1 A:1 C:1", "the card waits");

  setHidden(h, false);
  h.app._repaintAfterHidden();
  assert.equal(renders, 1, "one repaint when the tab shows");
  assert.equal(cell("a"), "V:40 A:40 C:40");
  assert.equal(cell("b"), "V:40 A:40 C:40");
  assert.match(dialog(), /V:40/, "the open dialog catches up too");

  h.app._repaintAfterHidden();
  assert.equal(renders, 1, "and only one: nothing was skipped since");

  h.app.handleMessage(tickFrame("a", 41));
  assert.equal(cell("a"), "V:41 A:41 C:41", "visible again: frames reach the card as they arrive");
  assert.equal(renders, 1);
});

// The repaint must leave exactly what the frames would have drawn had the tab
// been visible throughout.
test("the list after a hidden spell is the list a visible tab would show", { skip }, async () => {
  const frames = [];
  for (let n = 2; n <= 12; n++) frames.push(tickFrame("a", n), tickFrame("b", n * 2));
  const run = async (hide) => {
    const h = await harness.makeApp();
    h.app.jobs = [job("a", "AAA"), job("b", "BBB"), job("q", "QQQ", "Queued")];
    h.app.renderJobs();
    h.app.navigateJobList(1);
    h.document.querySelector('.video-item[data-job-id="b"] .job-checkbox').click();
    if (hide) setHidden(h, true);
    for (const f of frames) h.app.handleMessage({ type: f.type, payload: { ...f.payload } });
    if (hide) { setHidden(h, false); h.app._repaintAfterHidden(); }
    const snap = tasksSnapshot(h);
    // The progress bar is compared by its number: updateJobCard creates it
    // and sets the value property, a render writes it as markup with the
    // template indentation around it.
    snap.bars = [...h.document.querySelectorAll(".video-item")].map((c) => {
      const bar = c.querySelector(".job-progress-bar");
      return bar ? String(bar.value ?? bar.getAttribute("value")) : "";
    });
    snap.html = snap.html.replace(/<sl-progress-bar[^>]*><[/]sl-progress-bar>/g, "");
    return snap;
  };
  assert.deepEqual(await run(true), await run(false));
});

// MUTANT: the hidden check ahead of the status comparison — a job that
// finishes while the tab is hidden keeps its old place, and the parked badge
// and archive boundary wait for the operator to come back.
test("a status-changing progress frame is handled at once in a hidden tab", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [job("a", "AAA"), job("b", "BBB")];
  h.app.renderJobs();
  setHidden(h, true);

  h.app.handleMessage({ type: "job_progress", payload: { id: "a", status: "Muxing", progress: "" } });

  assert.deepEqual([...h.document.querySelectorAll(".video-item")].map((e) => e.dataset.jobId), ["b", "a"],
    "the row moves to its new group");
  assert.equal(h.document.querySelector('.video-item[data-job-id="a"]').dataset.status, "muxing");
});
