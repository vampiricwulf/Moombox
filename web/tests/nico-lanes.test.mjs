// Tests for web/public/modules/nico-lanes.js
import { test } from "node:test";
import assert from "node:assert/strict";
import { LaneAllocator, seedCursorIndex } from "../public/modules/nico-lanes.js";
import { indexAfter } from "../public/modules/chat-timeline.js";

const W = 1000, D = 8000;
const alloc = (la, nowMs, widthPx, extra = {}) =>
  la.allocate({ nowMs, widthPx, stageWidthPx: W, durationMs: D, lanesNeeded: 1, gapMs: 0, ...extra });

test("empty lanes: first allocation takes lane 0, a simultaneous one takes lane 1", () => {
  const la = new LaneAllocator(3);
  assert.equal(alloc(la, 0, 100), 0);
  assert.equal(alloc(la, 0, 100), 1);
});

test("wide follower must wait for the two-edge bound: D·wj/(W+wj)", () => {
  // leader 100 px at t=0; follower 600 px: bound = 8000*600/1600 = 3000 ms
  const la = new LaneAllocator(1);
  assert.equal(alloc(la, 0, 100), 0);
  assert.equal(alloc(la, 1000, 600), -1);   // right edge cleared (930 ms) but the follower would overtake
  assert.equal(alloc(la, 2999, 600), -1);
  assert.equal(alloc(la, 3000, 600), 0);
});

test("narrow follower behind a wide leader waits for the leader's tail: D·wi/(W+wi)", () => {
  const la = new LaneAllocator(1);
  assert.equal(alloc(la, 0, 600), 0);          // bound for any follower ≤ 600 px = 3000 ms
  assert.equal(alloc(la, 2999, 100), -1);
  assert.equal(alloc(la, 3000, 100), 0);
});

test("gapMs adds a fixed buffer", () => {
  const la = new LaneAllocator(1);
  assert.equal(alloc(la, 0, 100), 0);
  assert.equal(alloc(la, 3000, 600, { gapMs: 150 }), -1);
  assert.equal(alloc(la, 3150, 600, { gapMs: 150 }), 0);
});

test("lanesNeeded requires consecutive free lanes", () => {
  const la = new LaneAllocator(3);
  assert.equal(alloc(la, 0, 100), 0);                            // lane 0 busy
  assert.equal(alloc(la, 0, 100, { lanesNeeded: 2 }), 1);       // lanes 1-2
  assert.equal(alloc(la, 0, 100, { lanesNeeded: 2 }), -1);
});

// T12-M3: a mutant that only records the FIRST of lanesNeeded lanes as
// occupied would still return 0 here (both calls target a 2-lane
// allocator) but would then wrongly free lane 1 for the follow-up 1-lane
// request below.
test("lanesNeeded occupies every requested lane, not just the first", () => {
  const la = new LaneAllocator(2);
  assert.equal(alloc(la, 0, 100, { lanesNeeded: 2 }), 0);
  assert.equal(alloc(la, 0, 100), -1);   // both lanes are occupied
});

test("media time: the same nowMs never frees a lane (no wall clock inside)", () => {
  const la = new LaneAllocator(1);
  assert.equal(alloc(la, 5000, 300), 0);
  assert.equal(alloc(la, 5000, 300), -1);
  assert.equal(alloc(la, 5000 + D, 300), 0);   // a full traverse later it is certainly free
});

test("reset clears occupancy and can change the lane count", () => {
  const la = new LaneAllocator(2);
  alloc(la, 0, 100); alloc(la, 0, 100);
  la.reset(4);
  assert.equal(la.laneCount, 4);
  assert.equal(alloc(la, 0, 100), 0);
});

// F2 (phase 3+4 review, mutation N4): the BARE reset() is what player.js calls
// from clearNicoOverlay and _resetNicoCursor, and a mutant that defaulted
// `laneCount` to 0 — silently emptying the stage's rows on every clear —
// survived all 13 cases here. Both halves of the default matter: the current
// count is kept AND every lane is freed.
test("reset() with no argument keeps the lane count and frees every lane", () => {
  const la = new LaneAllocator(3);
  assert.equal(alloc(la, 0, 100), 0);
  assert.equal(alloc(la, 0, 100), 1);
  assert.equal(alloc(la, 0, 100), 2);
  assert.equal(alloc(la, 0, 100), -1, "all three lanes are busy");

  la.reset();

  assert.equal(la.laneCount, 3, "the lane count survives a bare reset");
  assert.equal(alloc(la, 0, 100), 0, "lane 0 is free again");
  assert.equal(alloc(la, 0, 100), 1);
  assert.equal(alloc(la, 0, 100), 2, "and so are the other two");
});

// N6: every other fixture in this file runs at D = 8000 with no gap. This is
// the only case at the constants the player actually ships — NICO_DURATION_MS
// 4000, NICO_LANE_GAP_MS 150, a 1280 px stage and the 17 rows a 408 px overlay
// yields at a 24 px line box.
test("production constants: a full 17-row stage frees a lane at D·w/(W+w) + gap", () => {
  const PD = 4000, PW = 1280, GAP = 150, ROWS = 17, WIDTH = 300;
  const la = new LaneAllocator(ROWS);
  const req = (nowMs) => la.allocate({
    nowMs, widthPx: WIDTH, stageWidthPx: PW, durationMs: PD, lanesNeeded: 1, gapMs: GAP,
  });

  for (let i = 0; i < ROWS; i++) assert.equal(req(0), i, `lane ${i} taken at t=0`);
  assert.equal(req(0), -1, "the stage is full");

  // Leader and follower are the same width, so max(wi, wj) = WIDTH.
  const bound = (PD * WIDTH) / (PW + WIDTH) + GAP;
  assert.ok(Math.abs(bound - 909.4937) < 1e-3, `bound = ${bound}`);
  assert.equal(req(bound - 1), -1, "a millisecond early every lane is still busy");
  assert.equal(req(bound), 0, "at the bound lane 0 accepts the follower");
});

const msgsAt = (...offsets) => offsets.map((o, i) => ({ id: String(i), offsetMs: o }));

test("seedCursorIndex: byTime wins when the count window is far larger (5 msgs, seedMax 30)", () => {
  const messages = msgsAt(0, 500, 1000, 1500, 2000);
  // effectiveMs=2000, latenessMs=2000 -> byTime = indexAfter(messages, 0) = 1
  // byCount = indexAfter(messages, 2000) - 30 = 5 - 30 = -25
  assert.equal(seedCursorIndex(messages, 2000, 2000, 30, indexAfter), 1);
});

test("seedCursorIndex: byCount wins for a legacy pile all at one offset", () => {
  const messages = msgsAt(...Array(100).fill(0));
  // effectiveMs=0, latenessMs=2000 -> byTime = indexAfter(messages, -2000) = 0
  // byCount = indexAfter(messages, 0) - 30 = 100 - 30 = 70
  assert.equal(seedCursorIndex(messages, 0, 2000, 30, indexAfter), 70);
});

test("seedCursorIndex: empty messages -> 0", () => {
  assert.equal(seedCursorIndex([], 5000, 2000, 30, indexAfter), 0);
});

test("seedCursorIndex: seedMax 0 behaves as 1", () => {
  const messages = msgsAt(0, 500, 1000, 1500, 2000);
  // byTime = indexAfter(messages, 0) = 1
  // byCount = indexAfter(messages, 2000) - max(1, 0) = 5 - 1 = 4 (NOT 5 - 0 = 5)
  assert.equal(seedCursorIndex(messages, 2000, 2000, 0, indexAfter), 4);
});

// T12-M6: seedCursorIndex's own Math.max(byTime, byCount, 0) floor is part
// of the injected-dependency contract — a broken/stubbed indexAfter that
// returns negative indices must never produce a negative cursor.
test("seedCursorIndex: floors at 0 even when indexAfter returns negative", () => {
  const stubIndexAfter = () => -50;
  assert.equal(seedCursorIndex([1, 2, 3], 1000, 500, 30, stubIndexAfter), 0);
});
