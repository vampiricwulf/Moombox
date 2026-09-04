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
