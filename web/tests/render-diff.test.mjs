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
