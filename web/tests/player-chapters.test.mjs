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
  // Hover text and accessible name carry the title and the start time — the
  // hover text in the marker's sl-tooltip (FE-37).
  assert.equal(markers(h)[1].closest("sl-tooltip")?.getAttribute("content"), "Chapter 2: Elden Ring (2:00)");
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
// cleared from the previous row; a strip re-layout dropping the highlight.
test("the current chapter is highlighted as playback moves", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("v")], chaptersById: { v: THREE } });
  await h.selectJob("v");

  const current = () => rows(h).map((r) => r.getAttribute("aria-current") === "true");
  assert.deepEqual(current(), [true, false, false]);

  h.tick(130_000);
  assert.deepEqual(current(), [false, true, false]);
  assert.ok(rows(h)[1].classList.contains("active"));
  assert.deepEqual(markers(h).map((m) => m.classList.contains("active")), [false, true, false]);
  // A re-layout of the strip (metadata landing) keeps the highlight.
  h.video.duration = 800;
  h.video.dispatchEvent(new h.window.Event("durationchange"));
  assert.deepEqual(markers(h).map((m) => m.classList.contains("active")), [false, true, false]);

  h.tick(300_000); // exactly on a start: that chapter
  assert.deepEqual(current(), [false, false, true]);
  h.seek(10_000);
  assert.deepEqual(current(), [true, false, false]);
});

// Mutants: the strip or the list rendered with no chapters (an empty strip
// shown, the list not hidden); the previous job's chapters left in place
// until the next job's answer (the clear at selection dropped); the list's
// collapse not reset with it (job v's list reopens expanded).
test("a job with no chapters draws nothing, and a switch clears the previous job's", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("v"), finished("none")], chaptersById: { v: THREE } });
  await h.selectJob("v");
  assert.equal(markers(h).length, 3);
  assert.equal(h.el("player-chapters").hidden, false);
  assert.equal(h.el("player-chapters").getAttribute("summary"), "Chapters (3)");
  // The viewer expands job v's list; the next job's list starts collapsed.
  h.el("player-chapters").open = true;

  // The previous job's chapters go the moment the next is opened, not when
  // its own (here parked) chapters answer.
  const parked = h.http.deferBody("GET /api/jobs/none/chapters");
  await h.selectJob("none");
  assert.equal(markers(h).length, 0, "job v's markers still drawn while job none's chapters are in flight");
  assert.equal(h.el("player-chapters").hidden, true);
  parked.resolve({ chapters: [] });
  await h.flush();
  assert.equal(h.el("player-chapter-strip").hidden, true);
  assert.equal(markers(h).length, 0);
  assert.equal(h.el("player-chapters").hidden, true);
  assert.equal(rows(h).length, 0);

  // Closing the player (the picker cleared) drops them too.
  await h.selectJob("v");
  assert.equal(markers(h).length, 3);
  assert.equal(h.el("player-chapters").open, false, "the list must come back collapsed, as its default is");
  h.player.clearPlayer();
  assert.equal(markers(h).length, 0);
  assert.equal(h.el("player-chapter-strip").hidden, true);
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

// Owner decision FE-37: a marker's name is in an sl-tooltip, not a title
// attribute — shown on hover AND on keyboard focus (the title showed on hover
// only, and never to a keyboard) — and the aria-label stays. The tooltip
// wraps the marker, so the strip still lays the buttons out, and the current
// chapter's highlight still reaches them.
//
// Mutants: the title attribute kept (or the tooltip dropped); the tooltip's
// trigger without focus; the highlight still walking the strip's children
// (now the tooltips, so no marker is highlighted).
test("chapter markers name themselves in an sl-tooltip, on hover and on focus", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("v")], chaptersById: { v: THREE } });
  await h.selectJob("v");
  const tips = [...h.el("player-chapter-strip").children];
  assert.equal(tips.length, 3);
  for (const [i, tip] of tips.entries()) {
    assert.equal(tip.tagName, "SL-TOOLTIP");
    const marker = tip.querySelector(".chapter-strip-marker");
    assert.ok(marker, `tooltip ${i} wraps its marker`);
    assert.equal(marker.hasAttribute("title"), false, "no title attribute beside the tooltip");
    assert.ok(/\bfocus\b/.test(tip.getAttribute("trigger") || ""), "the tooltip opens on keyboard focus");
    assert.ok(/\bhover\b/.test(tip.getAttribute("trigger") || ""), "and on hover");
  }
  assert.equal(tips[2].getAttribute("content"), "Chapter 3: Outro (5:00)");
  assert.equal(markers(h)[2].getAttribute("aria-label"), "Seek to Chapter 3: Outro, at 5:00");

  h.video.currentTime = 130;
  h.video.dispatchEvent(new h.window.Event("timeupdate"));
  assert.deepEqual(markers(h).map((m) => m.classList.contains("active")), [false, true, false]);
});

// Owner decision FE-36: the chapter list remembers its open or collapsed
// state per browser, in localStorage — collapsed the first time, then as the
// viewer last left it, job after job. A job with no chapters collapses and
// hides the list without forgetting the state, and a storage that throws on
// every access costs nothing: the list opens collapsed and still toggles.
//
// Mutants: the state not read on render (the second job opens collapsed);
// not written on sl-show/sl-hide (same); the hidden guard dropped (the
// no-chapters job's collapse is saved and the next job opens collapsed);
// either storage access unguarded (the throwing storage breaks the render or
// the toggle); the e.target guard dropped (a child component's bubbling
// sl-show is saved as the list opening).
test("the chapter list remembers its open state per browser, and survives a storage that throws", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("a"), finished("b"), finished("none"), finished("c")],
    chaptersById: { a: THREE, b: THREE, c: THREE },
  });
  const details = h.el("player-chapters");
  const toggle = (type) => details.dispatchEvent(new h.window.CustomEvent(type, { bubbles: true }));

  await h.selectJob("a");
  assert.equal(details.open, false, "collapsed the first time");
  details.open = true;
  toggle("sl-show"); // the viewer opens it
  assert.equal(h.window.localStorage.getItem("player-chapters-open"), "true");

  await h.selectJob("b");
  assert.equal(details.open, true, "the next job's list opens as it was left");

  await h.selectJob("none");
  assert.equal(details.hidden, true);
  toggle("sl-hide"); // Shoelace's event for the collapse a chapterless job makes
  assert.equal(h.window.localStorage.getItem("player-chapters-open"), "true", "a hidden list's collapse is not the viewer's");

  await h.selectJob("c");
  assert.equal(details.open, true);
  toggle("sl-hide");
  assert.equal(h.window.localStorage.getItem("player-chapters-open"), "false");
  // A Shoelace component inside the list (a marker's tooltip, say) fires its
  // own sl-show, which bubbles to the details: that is not the list opening.
  const child = details.firstElementChild;
  assert.ok(child, "the list has a child to fire from");
  child.dispatchEvent(new h.window.CustomEvent("sl-show", { bubbles: true }));
  assert.equal(h.window.localStorage.getItem("player-chapters-open"), "false", "a child's bubbling sl-show is not saved");
  await h.selectJob("a");
  assert.equal(details.open, false);

  // Storage that throws on every access.
  const throwing = {
    getItem() { throw new Error("SecurityError"); },
    setItem() { throw new Error("QuotaExceededError"); },
  };
  const real = globalThis.localStorage;
  Object.defineProperty(globalThis, "localStorage", { configurable: true, writable: true, value: throwing });
  try {
    await h.selectJob("b");
    assert.equal(details.hidden, false);
    assert.equal(details.open, false, "nothing readable: collapsed");
    assert.equal(rows(h).length, 3, "the list is still drawn");
    // A listener's exception does not reach dispatchEvent's caller; jsdom
    // reports it on the window instead.
    const errors = [];
    const onError = (e) => { errors.push(e.error?.message || e.message); e.preventDefault(); };
    h.window.addEventListener("error", onError);
    toggle("sl-show");
    h.window.removeEventListener("error", onError);
    assert.deepEqual(errors, [], "the toggle's save threw");
  } finally {
    Object.defineProperty(globalThis, "localStorage", { configurable: true, writable: true, value: real });
  }
});
