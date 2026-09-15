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
