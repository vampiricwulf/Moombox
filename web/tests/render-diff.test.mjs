// The details dialog and the status bar sit on the job_update path, which runs
// at ~60 Hz per active job. Re-assigning a textContent or className that
// already holds the same value still dirties layout, so a repeat update must
// perform ZERO DOM writes (sweep T2-21).
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

/**
 * Count textContent / className / style.display assignments across the whole
 * document by wrapping the prototype accessors. Restore before asserting, or a
 * failure message's own string work is counted too.
 */
function countDomWrites(window) {
  const textDesc = Object.getOwnPropertyDescriptor(window.Node.prototype, "textContent");
  const classDesc = Object.getOwnPropertyDescriptor(window.Element.prototype, "className");
  const displayDesc = Object.getOwnPropertyDescriptor(window.CSSStyleDeclaration.prototype, "display");
  const counts = { textContent: 0, className: 0, display: 0 };

  Object.defineProperty(window.Node.prototype, "textContent", {
    ...textDesc,
    set(v) { counts.textContent++; textDesc.set.call(this, v); },
  });
  Object.defineProperty(window.Element.prototype, "className", {
    ...classDesc,
    set(v) { counts.className++; classDesc.set.call(this, v); },
  });
  if (displayDesc && displayDesc.set) {
    Object.defineProperty(window.CSSStyleDeclaration.prototype, "display", {
      ...displayDesc,
      set(v) { counts.display++; displayDesc.set.call(this, v); },
    });
  }

  return {
    counts,
    restore() {
      Object.defineProperty(window.Node.prototype, "textContent", textDesc);
      Object.defineProperty(window.Element.prototype, "className", classDesc);
      if (displayDesc && displayDesc.set) {
        Object.defineProperty(window.CSSStyleDeclaration.prototype, "display", displayDesc);
      }
    },
  };
}

const downloadingJob = () => ({
  id: "job-1",
  videoId: "vid1",
  title: "A stream",
  channelName: "Chan",
  platform: "youtube",
  status: "Downloading",
  progress: "V:1234 A:1234 C:5678",
  percent: 37,
  lastVideoSeq: 1234,
  lastAudioSeq: 1234,
  chatStatus: "downloading",
  totalChatMessages: 5678,
  speed: "1.5x",
  updatedAt: harness ? harness.agoISO(30) : "",
});

test("updateJobDetails writes nothing when the job has not changed", { skip }, async () => {
  const h = await harness.makeApp();
  const job = downloadingJob();

  h.app.selectedJobId = job.id;
  h.app.jobs = [job];
  h.app.details.renderJobDetails(job);
  h.app.details.updateJobDetails(job); // settle any render-vs-update difference
  await h.flush();

  const spy = countDomWrites(h.window);
  h.app.details.updateJobDetails(job);
  spy.restore();

  assert.equal(spy.counts.textContent, 0,
    "an unchanged job must not re-assign any textContent — the unguarded version (the mutant) " +
    "writes ~8 of them 60 times a second for as long as the dialog is open");
  assert.equal(spy.counts.className, 0, "an unchanged status must not re-assign the badge class");
  assert.equal(spy.counts.display, 0,
    "updateDetailsButtons must not re-assign eight style.display values per tick");
});

test("updateJobDetails still writes when a value actually changes", { skip }, async () => {
  const h = await harness.makeApp();
  // isVod: true — renderJobDetails only emits the [data-field="progress"] row
  // for VOD jobs; a Live/Downloading job renders segment counts instead
  // (deviation from the brief's plain downloadingJob(), recorded in the report).
  const job = { ...downloadingJob(), isVod: true };

  h.app.selectedJobId = job.id;
  h.app.jobs = [job];
  h.app.details.renderJobDetails(job);
  h.app.details.updateJobDetails(job);
  await h.flush();

  h.app.details.updateJobDetails({ ...job, progress: "V:9999 A:9999 C:9999" });

  const progress = h.el("job-details-content").querySelector('[data-field="progress"]');
  assert.match(progress.textContent, /9999/,
    "guarding the write must not SKIP it — a `return` instead of an `if` (the mutant) freezes the " +
    "details panel on its first value");
});

// The chat count's `toLocaleString` is an Intl format, and it ran on EVERY
// job_update tick — ~60 Hz per active job — even though the count moves once
// per chat flush. Gate it on the source value the way the updated-row title is
// gated on data-timestamp (sweep R5/item 4).
test("updateJobDetails re-formats the chat count only when it moves", { skip }, async () => {
  const h = await harness.makeApp();
  const job = downloadingJob();

  h.app.selectedJobId = job.id;
  h.app.jobs = [job];
  h.app.details.renderJobDetails(job);
  h.app.details.updateJobDetails(job); // settle the render-vs-update whitespace difference
  await h.flush();

  // The harness already replaced Number.prototype.toLocaleString with a
  // locale-pinned wrapper; count calls through it and put it back afterwards.
  const pinned = Number.prototype.toLocaleString;
  let formats = 0;
  Number.prototype.toLocaleString = function (...args) { formats++; return pinned.apply(this, args); };
  try {
    h.app.details.updateJobDetails(job);
    assert.equal(formats, 0,
      "an unchanged count must not be re-formatted — the ungated version (the mutant) runs an Intl " +
      "number format on every tick for as long as the dialog is open");

    h.app.details.updateJobDetails({ ...job, totalChatMessages: 9999 });
    assert.ok(formats >= 1, "a moved count must still be re-formatted — gating must not FREEZE it");
  } finally {
    Number.prototype.toLocaleString = pinned;
  }

  const chat = h.el("job-details-content").querySelector('[data-field="chat"]');
  assert.match(chat.textContent, /9,999 messages/,
    "the new count must reach the DOM — a `return` instead of an `if` (the mutant) leaves 5,678 there");
});

test("updateActiveIndicator writes nothing when the count has not changed", { skip }, async () => {
  const h = await harness.makeApp();
  const jobs = [downloadingJob(), { ...downloadingJob(), id: "job-2", status: "Live" }];

  h.app.stats.updateActiveIndicator(jobs);
  const spy = countDomWrites(h.window);
  h.app.stats.updateActiveIndicator(jobs);
  spy.restore();

  assert.equal(spy.counts.textContent, 0, "an unchanged active count must not re-write the label");
  assert.equal(spy.counts.className, 0, "an unchanged active count must not re-write the class");
  assert.equal(spy.counts.display, 0, "an unchanged active count must not re-write style.display");

  h.app.stats.updateActiveIndicator([]);
  assert.equal(h.el("active-indicator").style.display, "none",
    "dropping to zero active jobs must still hide the indicator");
});

// chat_status "incomplete" is written when a capture stopped short — a Twitch
// VOD whose cursor paging stalled, an IRC session that exhausted its reconnect
// budget. The badge text is the raw value in both code paths; only the variant
// is mapped, and an unmapped value silently falls through to "neutral", which
// reads as "nothing to see here" for the one status that asks for action.
//
// Mutants this kills:
//   - no `incomplete` entry in either chatVariantMap: the variant is "neutral".
//   - adding it to only ONE of the two maps: the render path and the update
//     path disagree, so the badge changes colour on the next 60 Hz tick — each
//     half of this test covers one map.
test("an incomplete chat capture renders a warning badge in both paths", { skip }, async () => {
  const h = await harness.makeApp();
  const job = { ...downloadingJob(), status: "Finished", chatStatus: "incomplete" };

  h.app.selectedJobId = job.id;
  h.app.jobs = [job];
  h.app.details.renderJobDetails(job);
  await h.flush();

  const badge = h.el("job-details-content").querySelector('[data-field="chat"] sl-badge');
  assert.ok(badge, "the details panel rendered no chat badge for an incomplete capture");
  assert.equal(badge.getAttribute("variant"), "warning",
    "renderJobDetails mapped `incomplete` to the wrong variant — unmapped values fall through " +
    "to `neutral`, which reads as nothing to act on");
  assert.equal(badge.textContent.trim(), "incomplete",
    "the badge text is the raw machine value; no display string restates it");

  // The update path owns its own copy of the map. Drive it with a changed
  // count so the block is entered, then read the PROPERTY it assigns (jsdom
  // treats sl-badge as an unknown element, so `variant` lands as a property,
  // not as the attribute the render path wrote).
  h.app.details.updateJobDetails({ ...job, totalChatMessages: 4211 });
  assert.equal(badge.variant, "warning",
    "updateJobDetails mapped `incomplete` to the wrong variant — the two maps must agree, or " +
    "the badge changes colour on the next job_update tick");
  assert.equal(badge.textContent.trim(), "incomplete",
    "updateJobDetails rewrote the badge text");
});

// ── updateJobCard's progress cell, and the single-card insert (WEB-4 / WEB-9) ─

const jobAt = (id, title) => ({ ...downloadingJob(), id, title });

/** The list container's markup after `newcomer` arrives over the socket. */
async function splicedMarkup(existing, newcomer) {
  const h = await harness.makeApp();
  h.app.jobs = existing.map((j) => ({ ...j }));
  h.app.renderJobs();

  let renders = 0;
  const realRender = h.app.renderJobs.bind(h.app);
  h.app.renderJobs = (...a) => { renders++; return realRender(...a); };

  h.app.handleMessage({ type: "job_update", payload: { ...newcomer } });
  return { html: h.el("jobs-container").innerHTML, renders };
}

/** The same list container, rendered from scratch with every job present. */
async function rebuiltMarkup(all) {
  const h = await harness.makeApp();
  h.app.jobs = all.map((j) => ({ ...j }));
  h.app.renderJobs();
  return h.el("jobs-container").innerHTML;
}

// updateJobCard's progress cell is the highest-frequency DOM write in the app:
// ~60 Hz per active job, and both assignments were unconditional. Measured
// before the fix: the text node was destroyed and recreated 60/60 ticks.
// (First-sweep item 21 covered job-details.js and stats.js, not this.)
test("updateJobCard leaves the progress cell alone when the string has not moved", { skip }, async () => {
  const h = await harness.makeApp();
  const job = downloadingJob();

  h.app.jobs = [job];
  h.app.renderJobs();
  h.app.updateJobCard(job); // settle the render-vs-update difference

  const cell = h.document.querySelector('.video-item[data-job-id="job-1"] .job-progress-text');
  const node = cell.firstChild;
  const title = cell.title;

  for (let i = 0; i < 10; i++) h.app.updateJobCard(job);

  assert.equal(cell.firstChild, node,
    "an unchanged progress string must not replace the text node — the unguarded version (the mutant) " +
    "destroys and recreates it 60 times a second for as long as the download runs");
  assert.equal(cell.title, title, "an unchanged tooltip must not be re-assigned");
});

// MUTANT: `return` instead of `if` around the guarded write — the progress cell
// freezes on its first value for the whole download.
test("updateJobCard still writes the progress cell when the string moves", { skip }, async () => {
  const h = await harness.makeApp();
  const job = downloadingJob();

  h.app.jobs = [job];
  h.app.renderJobs();
  h.app.updateJobCard(job);

  h.app.updateJobCard({ ...job, progress: "V:9999 A:9999 C:9999" });

  const cell = h.document.querySelector('.video-item[data-job-id="job-1"] .job-progress-text');
  assert.match(cell.textContent, /9999/,
    "guarding the write must not SKIP it — the card would freeze on its first progress string");
});

// Feed/DECAPI/backfill discovery adds ONE row per broadcast, and each one ran a
// full renderJobs(): an innerHTML rebuild of every card and every Shoelace
// shadow root in the list. A backfill page that finds K jobs did that K times.
//
// MUTANT: keep the unconditional this.renderJobs() in the unknown-id branch —
// renderJobItem is called once per job in the list instead of once.
test("a newly discovered job is spliced in, not re-rendered over the whole list", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [jobAt("a", "AAA"), jobAt("b", "BBB"), jobAt("c", "CCC")];
  h.app.renderJobs();

  let items = 0;
  const realItem = h.app.renderJobItem.bind(h.app);
  h.app.renderJobItem = (...a) => { items++; return realItem(...a); };

  h.app.handleMessage({ type: "job_update", payload: jobAt("bb", "BBZ") });

  assert.equal(items, 1,
    "one new job must cost one renderJobItem — rebuilding the list (the mutant) costs one per row, " +
    "and a backfill page does that once per job it finds");

  const order = [...h.document.querySelectorAll(".video-item")].map((el) => el.dataset.jobId);
  assert.deepEqual(order, ["a", "b", "bb", "c"],
    "the spliced card must land at the position _sortJobs gives it — same-status rows sort by title");
});

// R1: the fast path is an OPTIMISATION, so its DOM must be what renderJobs()
// would have produced — byte for byte, at the head, in the middle and at the
// tail (the two insertion branches).
//
// MUTANT: insert at the wrong position (`cards[index + 1]`, or always
// "beforeend") — the markup no longer matches the rebuild.
// MUTANT: drop the leading-indentation shuffle — the cards are right but the
// serialized list differs from a rebuild by one indentation run.
test("the spliced list is byte-identical to a full renderJobs() rebuild", { skip }, async () => {
  const existing = [jobAt("a", "AAA"), jobAt("b", "BBB"), jobAt("c", "CCC")];
  const cases = [
    ["head", jobAt("aa", "AA0")],
    ["middle", jobAt("bb", "BBZ")],
    ["tail", jobAt("dd", "DDD")],
  ];

  for (const [where, newcomer] of cases) {
    const { html, renders } = await splicedMarkup(existing, newcomer);
    const full = await rebuiltMarkup([...existing, newcomer]);
    assert.equal(renders, 0, `${where}: the fast path must have handled this, not renderJobs()`);
    assert.equal(html, full,
      `${where}: the spliced list must be byte-identical to the rebuild — anything else means the ` +
      "optimisation changed what the user sees");
  }
});

// The fast path is only safe when the rendered list and this.jobs are the same
// set. MUTANT: splice regardless of the filter — the new card appears in a
// filtered list it does not match, and the "N of M" count goes stale.
test("a newly discovered job falls back to a full render while a filter is active", { skip }, async () => {
  const h = await harness.makeApp();
  // The filter matches BOTH held jobs on purpose: a filter that hid one would
  // leave the rendered list one card short, and the "rendered cards are
  // this.jobs minus the newcomer" check would decline for that reason instead
  // — the filter guard itself would never be the thing under test.
  h.app.jobs = [jobAt("a", "AAA"), jobAt("b", "AAB")];
  h.app.renderJobs();

  // FilterBarController exposes no setter — setTokens is a closure handed to
  // _setupUnifiedFilter — so the filter is armed the way a user arms it.
  const input = h.document.querySelector("#tasks-filter .unified-filter-input");
  input.value = "AA";
  input.dispatchEvent(new h.window.Event("input", { bubbles: true }));
  h.advance(400); // the filter input debounces at 200 ms, then renders
  assert.ok(h.app.filterBar.tokens("jobs").length > 0, "the filter never armed — this test would be vacuous");
  assert.equal(h.document.querySelectorAll(".video-item").length, 2,
    "both held jobs must match, or the guard under test is not the one that declines");

  let renders = 0;
  const realRender = h.app.renderJobs.bind(h.app);
  h.app.renderJobs = (...a) => { renders++; return realRender(...a); };

  h.app.handleMessage({ type: "job_update", payload: jobAt("c", "CCC") });

  assert.equal(renders, 1,
    "with a filter active the match set and the 'N of M' count both have to be recomputed — the fast " +
    "path must decline");
  assert.equal(h.document.querySelector('.video-item[data-job-id="c"]'), null,
    "a job the filter excludes must not be spliced into a filtered list");
  assert.equal(h.el("tasks-filter-count").textContent, "2 of 3",
    "the 'N of M' count must count the new job — splicing past the filter (the mutant) leaves it stale");
});

// The very first job arrives at an EMPTY list, where the empty state is the
// thing on screen. MUTANT: drop the `cards.length === 0` guard — the card is
// appended and "No jobs yet" stays visible above it.
test("the first job goes through a full render, not a splice", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [];
  h.app.renderJobs();

  let renders = 0;
  const realRender = h.app.renderJobs.bind(h.app);
  h.app.renderJobs = (...a) => { renders++; return realRender(...a); };

  h.app.handleMessage({ type: "job_update", payload: jobAt("a", "AAA") });

  assert.equal(renders, 1, "there is nothing to splice into — the fast path must decline");
  assert.equal(h.document.querySelectorAll(".video-item").length, 1, "the first card must still appear");
  assert.equal(h.el("empty-state").style.display, "none",
    "the empty state must be dismissed — only renderJobs() does that");
});

// Counts agreeing is not lists agreeing. If the rendered cards have drifted
// from this.jobs (a job removed without a re-render), a splice would leave the
// stale card on screen for good.
//
// MUTANT: drop the id walk and trust `cards.length === sorted.length - 1` —
// the newcomer is spliced beside a card for a job that no longer exists.
test("a rendered list that has drifted from this.jobs falls back to a full render", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [jobAt("a", "AAA"), jobAt("b", "BBB")];
  h.app.renderJobs();
  // The list on screen still shows "b"; the model has moved on to "z". The
  // counts still line up once the newcomer is added, so only the id walk can
  // tell that the fast path would be wrong.
  h.app.jobs = [jobAt("a", "AAA"), jobAt("z", "ZZZ")];

  let renders = 0;
  const realRender = h.app.renderJobs.bind(h.app);
  h.app.renderJobs = (...a) => { renders++; return realRender(...a); };

  h.app.handleMessage({ type: "job_update", payload: jobAt("c", "CCC") });

  assert.equal(renders, 1, "the rendered list is not this.jobs minus the newcomer — the fast path must decline");
  const order = [...h.document.querySelectorAll(".video-item")].map((el) => el.dataset.jobId);
  assert.deepEqual(order, ["a", "c", "z"],
    "the full render must reconcile the list — splicing (the mutant) leaves the stale card on screen");
});

// _insertJobCard's contract is "the caller has already pushed the job onto
// this.jobs". Anything else declines — it never throws inside the socket
// handler, and it never guesses a position.
//
// MUTANT: drop `if (index < 0) return false` — the id walk runs past the last
// rendered card and throws a TypeError on `undefined.dataset`.
test("_insertJobCard declines a job the caller never pushed", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [jobAt("a", "AAA"), jobAt("b", "BBB"), jobAt("c", "CCC")];
  h.app.renderJobs();
  // Two cards against three held jobs: the "minus exactly one" count check
  // passes for a fourth job, so only the `index < 0` guard is left to notice
  // that this job was never pushed.
  h.document.querySelector('.video-item[data-job-id="c"]').remove();

  assert.equal(h.app._insertJobCard(jobAt("d", "DDD")), false,
    "a job that is not in this.jobs has no sort position — the helper must decline, not throw");
  assert.equal(h.document.querySelectorAll(".video-item").length, 2,
    "and must not insert a card it could not place");
});
