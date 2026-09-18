// job_progress is the ~60 Hz frame (sweep 2 row #40 / O-O): only the columns a
// progress tick writes, merged onto the row the tab already holds. Everything
// the frame does NOT carry — title, channel, thumbnail, description, output
// paths, gaps, trims, the staging flags — has to survive the merge, because
// job_update is now the only frame that restates them.
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

const heldJob = () => ({
  id: "job-1",
  videoId: "dQw4w9WgXcQ",
  title: "A stream",
  channelName: "Chan",
  platform: "youtube",
  status: "Downloading",
  progress: "V:100 A:100 C:100",
  percent: 10,
  speed: "1.0 MB/s",
  eta: "02:00:00",
  lastVideoSeq: 100,
  lastAudioSeq: 100,
  totalVideoSeq: 1000,
  totalAudioSeq: 1000,
  totalChatMessages: 100,
  description: "the 5 KB description the frame no longer carries",
  thumbnailUrl: "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg",
  filename: "Chan/2026-09-17 A stream [dQw4w9WgXcQ].mp4",
  hasStaging: true,
  updatedAt: harness ? harness.agoISO(30) : "",
});

const progressFrame = () => ({
  id: "job-1",
  status: "Downloading",
  progress: "V:200 A:200 C:250",
  percent: 20,
  speed: "2.0 MB/s",
  eta: "01:00:00",
  lastVideoSeq: 200,
  lastAudioSeq: 200,
  totalVideoSeq: 1000,
  totalAudioSeq: 1000,
  totalChatMessages: 250,
  updatedAt: harness ? harness.agoISO(1) : "",
});

// MUTANT: `this.jobs[i] = patch` instead of `{...old, ...patch}` — the title,
// channel, thumbnail, filename and staging flags all vanish and the card
// renders an untitled row on the next full render.
test("job_progress merges onto the held row instead of replacing it", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [heldJob()];
  h.app.renderJobs();

  h.app.handleMessage({ type: "job_progress", payload: progressFrame() });

  const job = h.app.jobs[0];
  assert.equal(job.progress, "V:200 A:200 C:250", "the frame's progress must win");
  assert.equal(job.percent, 20, "the frame's percent must win");
  assert.equal(job.totalChatMessages, 250, "the frame's chat count must win");
  assert.equal(job.title, "A stream",
    "the title is not in the frame and must survive — replacing the object (the mutant) loses it");
  assert.equal(job.description, "the 5 KB description the frame no longer carries",
    "the description is exactly what the slim frame drops; it must survive the merge");
  assert.equal(job.filename, "Chan/2026-09-17 A stream [dQw4w9WgXcQ].mp4",
    "the output filename must survive the merge");
  assert.equal(job.hasStaging, true,
    "the client-computed staging flag must survive — the job_update path preserves it explicitly and " +
    "this path must not be the one that drops it");
});

// MUTANT: call renderJobs() from this case (the job_update status branch's
// behaviour) — every card and every Shoelace shadow root is rebuilt 60 times a
// second, which is worse than the bytes this change saved.
test("job_progress updates the one card and never re-renders the list", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [heldJob(), { ...heldJob(), id: "job-2", title: "Another" }];
  h.app.renderJobs();

  let renders = 0;
  const realRender = h.app.renderJobs.bind(h.app);
  h.app.renderJobs = (...a) => { renders++; return realRender(...a); };
  let cards = 0;
  const realCard = h.app.updateJobCard.bind(h.app);
  h.app.updateJobCard = (...a) => { cards++; return realCard(...a); };

  h.app.handleMessage({ type: "job_progress", payload: progressFrame() });

  assert.equal(renders, 0, "a progress tick must not rebuild the list");
  assert.equal(cards, 1, "a progress tick must update exactly the one card it names");

  const text = h.document
    .querySelector('.video-item[data-job-id="job-1"] .job-progress-text')
    .textContent;
  assert.match(text, /200/, "the card did not pick up the new progress string");
});

// MUTANT: drop the `idx === -1` guard — the frame is pushed as a NEW job, and
// a row with no title, no channel and no thumbnail appears in the list.
test("a job_progress frame for an unknown id is ignored", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [heldJob()];
  h.app.renderJobs();

  h.app.handleMessage({ type: "job_progress", payload: { ...progressFrame(), id: "never-seen" } });

  assert.equal(h.app.jobs.length, 1,
    "an unknown id must be dropped — the next job_update or jobs_update carries the whole row");
});

// The server never sends a status-changing progress frame (isProgressOnlyChange
// excludes "status"), and `status` is in the frame so the client can CHECK that
// rather than assume it. MUTANT: merge and call updateJobCard anyway — the row
// keeps its old badge and its old sort position after a transition.
test("a status-changing job_progress frame falls back to a full render", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [heldJob()];
  h.app.renderJobs();

  let renders = 0;
  const realRender = h.app.renderJobs.bind(h.app);
  h.app.renderJobs = (...a) => { renders++; return realRender(...a); };

  h.app.handleMessage({
    type: "job_progress",
    payload: { ...progressFrame(), status: "Finished", percent: 100 },
  });

  assert.equal(h.app.jobs[0].status, "Finished", "the new status must reach the held row");
  assert.equal(renders, 1,
    "a status change re-sorts the list and can cross the archive boundary — the fast path must not " +
    "silently swallow one");
});

// The ~60 Hz cadence is PROTECTED: updates get cheaper, never rarer. MUTANT:
// coalesce the frames — a requestAnimationFrame/setTimeout batch, a "skip if
// the progress string moved by less than X" throttle, or a dedupe that folds
// consecutive ticks. Every one of those leaves `cards` short of the number of
// frames handled, and the card lags the download by however long the batch
// window is.
test("every job_progress frame reaches the card in the same turn", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [heldJob()];
  h.app.renderJobs();

  let cards = 0;
  const realCard = h.app.updateJobCard.bind(h.app);
  h.app.updateJobCard = (...a) => { cards++; return realCard(...a); };

  const ticks = [
    { progress: "V:210 A:210 C:255", percent: 21, lastVideoSeq: 210 },
    { progress: "V:220 A:220 C:260", percent: 22, lastVideoSeq: 220 },
    { progress: "V:230 A:230 C:265", percent: 23, lastVideoSeq: 230 },
  ];
  for (const tick of ticks) {
    h.app.handleMessage({ type: "job_progress", payload: { ...progressFrame(), ...tick } });
  }

  assert.equal(cards, 3,
    "each frame must drive its own card update — any batching or throttling (the mutant) drops ticks");
  assert.equal(h.app.jobs[0].percent, 23, "the last frame's value must be the one held");

  const text = h.document
    .querySelector('.video-item[data-job-id="job-1"] .job-progress-text')
    .textContent;
  assert.match(text, /V:230/,
    "the card must show the LAST tick synchronously — a deferred flush leaves an older string here");
});
