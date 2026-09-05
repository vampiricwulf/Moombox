// Tests for web/public/modules/nico-scheduler.js — the overlay's cursor /
// anchor / pending-list / drop-count state machine, pure (no DOM), at the
// constants the player actually ships (D = 4000, lead 1000, lateness 2000,
// ahead 300, cap 20, seed fallback 30).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  NicoScheduler,
  NICO_LEAD_MS,
  NICO_MAX_LATENESS_MS,
  NICO_TICK_AHEAD_MS,
  NICO_MAX_PER_TICK,
} from "../public/modules/nico-scheduler.js";
import { indexAfter } from "../public/modules/chat-timeline.js";
import { seedCursorIndex } from "../public/modules/nico-lanes.js";

/** A stand-in for LaneAllocator: 17 rows, and it either takes everything or nothing. */
function fakeLanes(laneCount = 17) {
  return {
    laneCount,
    resets: 0,
    refuse: false,
    reset() { this.resets++; },
    allocate() { return this.refuse ? -1 : 0; },
  };
}

/** Hooks that record every call; `place` mirrors the allocator's answer. */
function fakeHooks(lanes) {
  const rec = { prepared: [], placed: [], discarded: [] };
  rec.hooks = {
    prepare(msg) {
      rec.prepared.push(msg);
      return msg.system ? null : { msg };
    },
    place(entry, at, retry) {
      rec.placed.push({ offsetMs: entry.msg.offsetMs, at, retry });
      return !lanes.refuse;
    },
    discard(entry) { rec.discarded.push(entry.msg.offsetMs); },
  };
  return rec;
}

const make = (lanes) => new NicoScheduler({ lanes, indexAfter, seedCursorIndex });

/** `count` messages spaced `stepMs` apart starting at `startMs`. */
const ladder = (count, stepMs, startMs = 0) =>
  Array.from({ length: count }, (_, i) => ({ offsetMs: startMs + i * stepMs }));

test("anchor seeds inside the lateness window, and the count cap wins on a pile", () => {
  const lanes = fakeLanes();
  const s = make(lanes);

  // Every 100 ms from 0 to 60 000: the window (31 000 − 2 000) holds 20
  // messages, well under the cap of 2 × 17 rows = 34, so time wins.
  const msgsA = ladder(601, 100);
  s.anchor(msgsA, 30000);
  assert.equal(s.cursor, indexAfter(msgsA, 29000));
  assert.equal(s.anchorMs, 30000);
  assert.deepEqual(s.pending, []);
  assert.equal(lanes.resets, 1, "anchoring frees every lane");

  // 200 messages piled at one offset: the count cap keeps the seed at 34.
  const msgsB = [...Array.from({ length: 200 }, () => ({ offsetMs: 29500 })), { offsetMs: 40000 }];
  s.anchor(msgsB, 30000);
  assert.equal(s.cursor, indexAfter(msgsB, 31000) - 34);
});

test("the tick consumes up to the lookahead early and places at first sight", () => {
  const lanes = fakeLanes();
  const s = make(lanes);
  const rec = fakeHooks(lanes);
  const msgs = ladder(51, 100); // 0 … 5000

  s.anchor(msgs, 0);
  assert.equal(s.cursor, 0);
  const r = s.tick(msgs, 250, rec.hooks);

  // Taken while `offsetMs − NICO_LEAD_MS <= 250 + NICO_TICK_AHEAD_MS`.
  const want = msgs.filter((m) => m.offsetMs - NICO_LEAD_MS <= 250 + NICO_TICK_AHEAD_MS).length;
  assert.equal(want, 16);
  assert.deepEqual(r, { placed: want, deferred: 0, skipped: 0 });
  assert.equal(s.cursor, want);
  assert.deepEqual(rec.placed.map((p) => p.retry), Array(want).fill(false));
});

test("a too-late message is skipped and counted only past the anchor, and skips are free", () => {
  const lanes = fakeLanes();
  const s = make(lanes);
  const rec = fakeHooks(lanes);
  const msgs = Array.from({ length: 500 }, () => ({ offsetMs: 0 }));

  s.anchor(msgs, 0);
  assert.equal(s.cursor, 500 - 34, "the seed cap already skipped the bulk of the pile");

  // 34 skips in ONE tick, well past NICO_MAX_PER_TICK: a skip builds nothing,
  // so it must not consume a work slot.
  const r = s.tick(msgs, 5000, rec.hooks);
  assert.equal(s.cursor, 500, "a backlog clears in one tick");
  assert.equal(r.skipped, 34);
  assert.ok(r.skipped > NICO_MAX_PER_TICK);
  assert.equal(rec.prepared.length, 0);
  assert.equal(s.dropped, 0, "already flying at the anchor (entry −1000 ≤ anchor 0) is not a drop");

  msgs.push({ offsetMs: 3000 });
  s.tick(msgs, 6000, rec.hooks);
  assert.equal(s.dropped, 1, "a message that entered after the anchor and never showed IS a drop");

  s.resetDropCount();
  assert.equal(s.dropped, 0);
});

test("the per-tick cap leaves the cursor put", () => {
  const lanes = fakeLanes();
  const s = make(lanes);
  const rec = fakeHooks(lanes);
  const msgs = Array.from({ length: 40 }, () => ({ offsetMs: 1100 }));

  s.anchor(msgs, 0);
  assert.equal(s.cursor, 0);

  const r1 = s.tick(msgs, 100, rec.hooks);
  assert.equal(r1.placed, NICO_MAX_PER_TICK);
  assert.equal(s.cursor, NICO_MAX_PER_TICK, "the capped message is NOT consumed");

  const r2 = s.tick(msgs, 100, rec.hooks);
  assert.equal(r2.placed, 40 - NICO_MAX_PER_TICK);
  assert.equal(s.cursor, 40);
  assert.equal(s.dropped, 0);
});

test("an unplaceable entry is deferred, retried oldest-first, and dropped when too late", () => {
  const lanes = fakeLanes();
  const s = make(lanes);
  const rec = fakeHooks(lanes);
  const msgs = [1000, 1100, 1200, 2000, 3000, 3100].map((offsetMs) => ({ offsetMs }));

  s.anchor(msgs, 0);
  assert.equal(s.cursor, 0);

  lanes.refuse = true;
  const r1 = s.tick(msgs, 200, rec.hooks);
  assert.equal(r1.deferred, 3);
  assert.equal(r1.placed, 0);
  assert.equal(s.pending.length, 3);
  assert.deepEqual(rec.discarded, [], "a deferred entry is not discarded");
  assert.equal(s.dropped, 0);

  // The retries come first, oldest first, and only then the new message.
  lanes.refuse = false;
  rec.placed.length = 0;
  const r2 = s.tick(msgs, 800, rec.hooks);
  assert.equal(r2.placed, 4);
  assert.equal(s.pending.length, 0);
  assert.deepEqual(rec.placed, [
    { offsetMs: 1000, at: 800, retry: true },
    { offsetMs: 1100, at: 800, retry: true },
    { offsetMs: 1200, at: 800, retry: true },
    { offsetMs: 2000, at: 800, retry: false },
  ]);

  // Refuse again so the last two defer, then run the clock past the bound.
  lanes.refuse = true;
  const r3 = s.tick(msgs, 1900, rec.hooks);
  assert.equal(r3.deferred, 2);
  assert.equal(s.pending.length, 2);

  const r4 = s.tick(msgs, 4200, rec.hooks);
  assert.equal(r4.skipped, 2);
  assert.equal(s.pending.length, 0);
  assert.deepEqual(rec.discarded, [3000, 3100], "the caller is told to release each entry");
  assert.equal(s.dropped, 2);
});

test("unanchor + re-anchor after a seek counts nothing from the gap", () => {
  const lanes = fakeLanes();
  const s = make(lanes);
  const rec = fakeHooks(lanes);
  const msgs = ladder(601, 100); // 0 … 60 000

  s.anchor(msgs, 0);
  s.tick(msgs, 1000, rec.hooks);
  assert.ok(s.cursor > 0);

  // The `seeking` listener un-anchors before the seek's own timeupdate lands.
  s.unanchor();
  assert.equal(s.cursor, -1);
  assert.deepEqual(s.pending, []);

  s.anchor(msgs, 30000);
  rec.placed.length = 0;
  s.tick(msgs, 30250, rec.hooks);
  assert.equal(s.dropped, 0, "the 29 s of chat jumped over is not a drop");
  assert.ok(rec.placed.length > 0);
  for (const p of rec.placed) {
    assert.ok(p.offsetMs >= 30000 + NICO_LEAD_MS - NICO_MAX_LATENESS_MS,
      `placed ${p.offsetMs} is inside the seed window`);
  }
});

test("a message with nothing renderable consumes the cursor and places nothing", () => {
  const lanes = fakeLanes();
  const s = make(lanes);
  const rec = fakeHooks(lanes);
  const msgs = [{ offsetMs: 1000, system: true }, { offsetMs: 1100, system: true }];

  s.anchor(msgs, 0);
  const r = s.tick(msgs, 200, rec.hooks);
  assert.deepEqual(r, { placed: 0, deferred: 0, skipped: 0 });
  assert.equal(s.cursor, 2);
  assert.equal(rec.prepared.length, 2);
  assert.deepEqual(rec.placed, []);
  assert.deepEqual(s.pending, []);
});
