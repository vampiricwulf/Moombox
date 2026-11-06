// DOM tests for the player's chapters (owner decision CH-2): the strip of
// markers under the video and the collapsible chapter list, both fed by
// GET /api/jobs/{id}/chapters — see helpers/player-dom.mjs.
//
// jsdom is probed first and every test skips without it, as player.test.mjs
// does.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/player-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const finished = (id, extra = {}) => ({
  id, status: "Finished", filename: `${id}.mp4`, title: `Title ${id}`,
  channelName: "Chan", updatedAt: "2026-09-01T00:00:00Z", ...extra,
});

const THREE = {
  chapters: [
    { start: 0, end: 120, title: "Just Chatting" },
    { start: 120, end: 300, title: "Elden Ring" },
    { start: 300, end: 400, title: "Outro" },
  ],
};

const markers = (h) => [...h.el("player-chapter-strip").querySelectorAll(".chapter-strip-marker")];
const rows = (h) => [...h.el("player-chapter-list").querySelectorAll(".chapter-row")];
const lefts = (h) => markers(h).map((m) => m.style.left);

// Mutants: the strip laid out against a fixed duration instead of the
// element's (the 800 s re-layout keeps 30%/75%); the durationchange listener
// dropped (same); a marker per chapter END rather than start.
test("the strip draws one marker per chapter start, at its share of the timeline", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("v")], chaptersById: { v: THREE } });
  await h.selectJob("v");

  const strip = h.el("player-chapter-strip");
  assert.equal(strip.hidden, false);
  // Before metadata: laid out against the last chapter's end (400 s).
  assert.deepEqual(lefts(h), ["0%", "30%", "75%"]);
  // Hover text and accessible name carry the title and the start time.
  assert.equal(markers(h)[1].title, "Chapter 2: Elden Ring (2:00)");
  assert.equal(markers(h)[1].getAttribute("aria-label"), "Seek to Chapter 2: Elden Ring, at 2:00");
  assert.equal(markers(h)[1].type, "button");

  // The element's own duration, once known, is the timeline.
  h.video.duration = 800;
  h.video.dispatchEvent(new h.window.Event("durationchange"));
  assert.deepEqual(lefts(h), ["0%", "15%", "37.5%"]);
});

// Mutants: the strip laid out against the element's duration on a multi-part
// job (one part's 60 s, so 90 s lands off the end); the segment indicator
// appended at the end of the column (after the chapter list).
test("a multi-part job's strip is laid out on the global timeline, beside the segment indicator", { skip }, async () => {
  const job = finished("m", {
    segments: [
      { segmentIndex: 0, durationSeconds: 60, quality: "720p" },
      { segmentIndex: 1, durationSeconds: 60, quality: "1080p" },
    ],
  });
  const h = harness.makePlayer({
    jobs: [job],
    chaptersById: { m: { chapters: [{ start: 0, end: 30, title: "A" }, { start: 30, end: 90, title: "B" }, { start: 90, end: 120, title: "C" }] } },
  });
  await h.selectJob("m");
  h.video.duration = 60; // one part's own length — not the timeline
  h.video.dispatchEvent(new h.window.Event("durationchange"));

  assert.deepEqual(lefts(h), ["0%", "25%", "75%"]);
  const column = h.el("player-video-column");
  const order = [...column.children].map((c) => c.id);
  assert.deepEqual(order.slice(1), ["player-chapter-strip", "player-segment-indicator", "player-chapters"],
    `column order: ${order.join(", ")}`);
});

// Mutants: the marker or row click handler dropped; the click seeking to the
// chapter's end; a multi-part click writing currentTime directly (no part
// switch).
test("clicking a marker or a list row seeks to that chapter's start", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("v")], chaptersById: { v: THREE } });
  await h.selectJob("v");

  markers(h)[2].click();
  assert.equal(h.video.currentTime, 300);
  rows(h)[1].click();
  assert.equal(h.video.currentTime, 120);

  // Multi-part: a chapter in part 2 loads part 2 and seeks inside it.
  const job = finished("m", {
    segments: [
      { segmentIndex: 0, durationSeconds: 60 },
      { segmentIndex: 1, durationSeconds: 60 },
    ],
  });
  const m = harness.makePlayer({
    jobs: [job],
    chaptersById: { m: { chapters: [{ start: 0, end: 90, title: "A" }, { start: 90, end: 120, title: "B" }] } },
  });
  await m.selectJob("m");
  rows(m)[1].click();
  assert.ok(m.video.src.endsWith("/api/jobs/m/segments/1/video"), `video.src = ${m.video.src}`);
  m.video.dispatchEvent(new m.window.Event("loadeddata"));
  assert.equal(m.video.currentTime, 30);
});

// Mutants: the timeupdate listener dropped (no highlight); the highlight
// taking the first chapter whose start is AFTER the time; aria-current not
// cleared from the previous row.
test("the current chapter is highlighted as playback moves", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("v")], chaptersById: { v: THREE } });
  await h.selectJob("v");

  const current = () => rows(h).map((r) => r.getAttribute("aria-current") === "true");
  assert.deepEqual(current(), [true, false, false]);

  h.tick(130_000);
  assert.deepEqual(current(), [false, true, false]);
  assert.ok(rows(h)[1].classList.contains("active"));
  assert.deepEqual(markers(h).map((m) => m.classList.contains("active")), [false, true, false]);

  h.tick(300_000); // exactly on a start: that chapter
  assert.deepEqual(current(), [false, false, true]);
  h.seek(10_000);
  assert.deepEqual(current(), [true, false, false]);
});

// Mutants: the strip or the list rendered with no chapters (an empty strip
// shown, the list not hidden); the previous job's chapters left in place
// when the next job has none.
test("a job with no chapters draws nothing, and a switch clears the previous job's", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("v"), finished("none")], chaptersById: { v: THREE } });
  await h.selectJob("v");
  assert.equal(markers(h).length, 3);
  assert.equal(h.el("player-chapters").hidden, false);
  assert.equal(h.el("player-chapters").getAttribute("summary"), "Chapters (3)");

  await h.selectJob("none");
  assert.equal(h.el("player-chapter-strip").hidden, true);
  assert.equal(markers(h).length, 0);
  assert.equal(h.el("player-chapters").hidden, true);
  assert.equal(rows(h).length, 0);

  h.player.clearPlayer();
  assert.equal(h.el("player-chapters").hidden, true);
});

// Mutants: onPlayerJobSelect awaiting the chapters fetch (the selection does
// not settle while it is parked); the stale-selection check dropped (job A's
// late chapters drawn over job B).
test("a slow chapters fetch never delays playback, and a late one for an old job is dropped", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("a"), finished("b")],
    chat: { messages: [{ offsetMs: 1000, author: "x", message: "hi" }] },
  });
  const slowA = h.http.deferBody("GET /api/jobs/a/chapters");
  let settled = false;
  const p = h.player.onPlayerJobSelect("a").then(() => { settled = true; });
  await h.flush();
  await h.flush();
  assert.ok(settled, "the selection settled while the chapters fetch was still parked");
  assert.ok(h.video.src.endsWith("/api/jobs/a/video"), `video.src = ${h.video.src}`);
  await p;

  await h.selectJob("b");
  slowA.resolve(THREE);
  await h.flush();
  assert.equal(h.player.playerJob.id, "b");
  assert.equal(markers(h).length, 0, "job a's chapters must not be drawn over job b");
  assert.equal(h.el("player-chapters").hidden, true);
});

// Mutants: a failed fetch toasting or throwing out of onPlayerJobSelect; the
// non-2xx arm reading the body as chapters.
test("a failed chapters fetch leaves playback working and draws nothing", { skip }, async () => {
  for (const failing of [
    () => harness.response({ status: 500, body: { chapters: [{ start: 5, end: 9, title: "bogus" }] } }),
    () => Promise.reject(new TypeError("Failed to fetch")),
  ]) {
    const h = harness.makePlayer({ jobs: [finished("v")] });
    h.http.on("GET /api/jobs/v/chapters", failing);
    await h.selectJob("v");
    assert.equal(h.player.playerJob.id, "v");
    assert.ok(h.video.src.endsWith("/api/jobs/v/video"), `video.src = ${h.video.src}`);
    assert.deepEqual(h.app.toasts, []);
    assert.equal(h.el("player-chapter-strip").hidden, true);
    assert.equal(markers(h).length, 0);
    // Playback ticks on unharmed.
    h.tick(5000);
    assert.equal(h.el("player-chapters").hidden, true);
  }
});
