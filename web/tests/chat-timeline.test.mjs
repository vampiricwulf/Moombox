// Tests for web/public/modules/chat-timeline.js
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  normalizeOffsetMs, computeChatBiasMs, partitionChatByVideo, indexAfter, mergePartChats,
  formatChatHeader, dividerLabelFor, deriveMissingOffsets,
} from "../public/modules/chat-timeline.js";

test("normalizeOffsetMs: numbers, json.Number strings, garbage", () => {
  assert.equal(normalizeOffsetMs(1500), 1500);
  assert.equal(normalizeOffsetMs(-90000), -90000);
  assert.equal(normalizeOffsetMs("123"), 123);   // imported chat: json.Number as string
  assert.equal(normalizeOffsetMs(undefined), 0);
  assert.equal(normalizeOffsetMs(null), 0);
  assert.equal(normalizeOffsetMs("abc"), 0);
  assert.equal(normalizeOffsetMs(NaN), 0);
  // phase-2-review.md §4 mutant MU4: the guard is Number.isFinite, not
  // !Number.isNaN — an infinity has to become 0 too. Every case above passes
  // under `Number.isNaN(n) ? 0 : n`; these are the ones that do not, and an
  // offset that reached the overlay as Infinity would make its lane busy
  // forever.
  assert.equal(normalizeOffsetMs(Infinity), 0);
  assert.equal(normalizeOffsetMs(-Infinity), 0);
  assert.equal(normalizeOffsetMs("Infinity"), 0);
});

test("computeChatBiasMs: twitch offsets are already video-relative → 0", () => {
  assert.equal(computeChatBiasMs({
    platform: "twitch",
    chatStreamStartTime: "2026-06-11T10:00:00Z",   // Twitch startedAt
    jobStreamStartTime: "2026-06-11T10:12:00Z",    // differs — pins the platform branch, not a same-epoch shortcut
  }), 0);
});

test("computeChatBiasMs: youtube = actual start − scheduled start (late start)", () => {
  assert.equal(computeChatBiasMs({
    platform: undefined,                              // YouTube files carry no platform field
    chatStreamStartTime: "2026-06-11T10:00:00Z",      // epoch the offsets count from
    jobStreamStartTime: "2026-06-11T10:12:00Z",       // actual start = video t=0
  }), 12 * 60 * 1000);
});

test("computeChatBiasMs: same epoch, missing or unparsable → 0", () => {
  assert.equal(computeChatBiasMs({ chatStreamStartTime: "2026-06-11T10:12:00Z", jobStreamStartTime: "2026-06-11T10:12:00Z" }), 0);
  assert.equal(computeChatBiasMs({ chatStreamStartTime: "", jobStreamStartTime: "2026-06-11T10:12:00Z" }), 0);
  assert.equal(computeChatBiasMs({ chatStreamStartTime: "2026-06-11T10:00:00Z", jobStreamStartTime: undefined }), 0);
  assert.equal(computeChatBiasMs({ chatStreamStartTime: "garbage", jobStreamStartTime: "2026-06-11T10:12:00Z" }), 0);
});

const msgs = (...offsets) => offsets.map((o, i) => ({ id: String(i), offsetMs: o }));

test("partitionChatByVideo: pre-show, in-video and post-end counts", () => {
  const p = partitionChatByVideo(msgs(-90000, -5000, 0, 1000, 5000, 61000, 65000), 60000);
  assert.deepEqual(p, { preCount: 2, firstLiveIndex: 2, postCount: 2, firstPostIndex: 5 });
});

test("partitionChatByVideo: a message exactly at totalDurationMs is still in-video", () => {
  const p = partitionChatByVideo(msgs(0, 30000, 60000, 60001), 60000);
  assert.deepEqual(p, { preCount: 0, firstLiveIndex: 0, postCount: 1, firstPostIndex: 3 });
});

test("partitionChatByVideo: no negatives, unknown duration", () => {
  assert.deepEqual(partitionChatByVideo(msgs(0, 1000), 0), { preCount: 0, firstLiveIndex: 0, postCount: 0, firstPostIndex: -1 });
  assert.deepEqual(partitionChatByVideo([], 60000), { preCount: 0, firstLiveIndex: -1, postCount: 0, firstPostIndex: -1 });
  assert.deepEqual(partitionChatByVideo(msgs(-3, -2), 60000), { preCount: 2, firstLiveIndex: -1, postCount: 0, firstPostIndex: -1 });
});

test("formatChatHeader: a region with no messages contributes no clause", () => {
  assert.equal(formatChatHeader(0, 0, 0), "0 messages");
  assert.equal(formatChatHeader(120, 0, 0), "120 messages");
  assert.equal(formatChatHeader(120, 8, 0), "120 messages · 8 pre-show");
  assert.equal(formatChatHeader(120, 0, 3), "120 messages · 3 after end");
  assert.equal(formatChatHeader(120, 8, 3), "120 messages · 8 pre-show · 3 after end");
});

test("dividerLabelFor: one label per region boundary, null everywhere else", () => {
  const p = partitionChatByVideo(msgs(-90000, -5000, 0, 1000, 61000), 60000);
  assert.deepEqual(p, { preCount: 2, firstLiveIndex: 2, postCount: 1, firstPostIndex: 4 });
  assert.equal(dividerLabelFor(p, 2), "Waiting room — 2 messages before the stream");
  assert.equal(dividerLabelFor(p, 4), "Recording ended — 1 messages after it");
  assert.equal(dividerLabelFor(p, 0), null);   // inside the pre-show region
  assert.equal(dividerLabelFor(p, 3), null);   // inside the video
  assert.equal(dividerLabelFor(null, 2), null);
});

test("dividerLabelFor: no pre-show region means no waiting-room label on row 0", () => {
  const p = partitionChatByVideo(msgs(0, 1000), 60000);
  assert.equal(p.firstLiveIndex, 0);           // exists, but preCount is 0
  assert.equal(dividerLabelFor(p, 0), null);
});

// Both regions can start on the SAME row: a recording so short that every
// non-negative message lands past its end. The waiting-room label wins —
// it explains that row's own position, and player.js stamps one divider per
// row, so the two call sites (build-time and _applyDividers) must agree.
test("dividerLabelFor: waiting-room wins when both regions start on one row", () => {
  const p = partitionChatByVideo(msgs(-5000, 61000, 62000), 60000);
  assert.equal(p.firstLiveIndex, 1);
  assert.equal(p.firstPostIndex, 1);
  assert.equal(dividerLabelFor(p, 1), "Waiting room — 1 messages before the stream");
});

test("indexAfter: first index whose offset is strictly greater", () => {
  const m = msgs(0, 0, 1000, 1000, 5000);
  assert.equal(indexAfter(m, -1), 0);
  assert.equal(indexAfter(m, 0), 2);      // equal offsets are NOT after
  assert.equal(indexAfter(m, 999), 2);
  assert.equal(indexAfter(m, 1000), 4);
  assert.equal(indexAfter(m, 5000), 5);
  assert.equal(indexAfter([], 0), 0);
});

test("mergePartChats: shifts each part by its start offset and keeps first header", () => {
  const merged = mergePartChats([
    { startOffsetSec: 0,    data: { platform: "twitch", streamStartTime: "S", emotes: { bttv: [] }, messages: [{ id: "a", offsetMs: 5000 }] } },
    { startOffsetSec: 3600.5, data: { platform: "twitch", messages: [{ id: "b", offsetMs: "1000" }, { id: "c", offsetMs: -2000 }] } },
  ]);
  assert.equal(merged.platform, "twitch");
  assert.equal(merged.streamStartTime, "S");
  assert.deepEqual(merged.emotes, { bttv: [] });
  assert.deepEqual(merged.messages.map((m) => [m.id, m.offsetMs]), [["a", 5000], ["b", 3601500], ["c", 3598500]]);
});

// phase-2-review.md §4 mutants (MU1/MU2/MU3/MU7), one case:
// - a whole-null part payload (a chat file whose JSON is literally `null`)
//   must be skipped, not crash the loop (MU2, `if (!data) continue` removed)
// - a part whose `messages` field is `null` (Go's nil-slice encoding) must
//   count as zero messages, not throw (MU1, `data.messages || []` dropped)
// - a later part's differing platform must not overwrite an earlier part's
//   value — header fields come from the FIRST part that has them (MU3,
//   `??=` mutated to unconditional `=`). Deviation from the brief's literal
//   "platform present only on the second part": with only ONE part ever
//   carrying a platform value, first-wins and last-wins produce the same
//   result (proven empirically — both implementations converge), so this
//   uses two parts with DIFFERING platform values instead, which is what
//   actually distinguishes the two behaviours.
// - a fractional startOffsetSec (301.00000000000006, the float-sum shape
//   the review's example calls out) must still shift onto an integer
//   offsetMs (MU7, `Math.round` dropped from the shift).
test("mergePartChats: null part, null messages, first-wins platform and a fractional shift", () => {
  const merged = mergePartChats([
    { startOffsetSec: 0, data: null },
    { startOffsetSec: 100.1, data: { platform: "youtube", messages: null } },
    { startOffsetSec: 301.00000000000006, data: { platform: "twitch", messages: [{ id: "a", offsetMs: 0 }] } },
  ]);
  assert.equal(merged.platform, "youtube");
  assert.deepEqual(merged.messages.map((m) => [m.id, m.offsetMs]), [["a", 301000]]);
  assert.equal(Number.isInteger(merged.messages[0].offsetMs), true);
});

// --- deriveMissingOffsets (T-F12 remainder) ------------------------------
//
// A message with no offset of its own — offsetMs 0 and no hasOffset, the
// producer's pre-2026-04-22 sentinel (internal/chat/types.go: "offsetMs=0 was
// the unset sentinel") — piles at t=0 and the sidebar/overlay show it all at
// once (R10 caps the flood, it does not fix it). The offset is recoverable
// whenever the file header has an epoch: the Go producer computes
// offsetMs = timestampUsec/1000 − streamStartMs (internal/chat/downloader.go,
// integer division), so the player can do the same arithmetic on load.
//
// A NON-ZERO offsetMs is authoritative even with no hasOffset — a
// pre-2026-04-22 file has real offsets on every message and the flag on none —
// so the skip tests the sentinel, not just the flag (F1).
//
// EPOCH_MS below is the file header's `streamStartTime`, i.e. the file's own
// (first run's) epoch — the same "one file, one epoch" value everything else
// on the timeline is measured against.
const EPOCH = "2026-06-11T10:00:00Z";
const EPOCH_MS = Date.parse(EPOCH);
const usecAt = (ms) => String((EPOCH_MS + ms) * 1000);

test("deriveMissingOffsets: legacy messages get the offset the producer would have written", () => {
  const messages = [
    { id: "a", offsetMs: 0, timestampUsec: usecAt(1500) },              // hasOffset absent (legacy)
    { id: "b", offsetMs: 0, hasOffset: false, timestampUsec: usecAt(2500) }, // explicit false
  ];
  const same = deriveMissingOffsets(messages, EPOCH);
  assert.equal(same, messages, "mutates in place and returns the same array");
  assert.deepEqual(messages.map((m) => [m.offsetMs, m.hasOffset]), [[1500, true], [2500, true]]);
});

test("deriveMissingOffsets: a message that already has an offset is untouched", () => {
  const messages = [{ id: "a", offsetMs: 4242, hasOffset: true, timestampUsec: usecAt(9999) }];
  deriveMissingOffsets(messages, EPOCH);
  assert.equal(messages[0].offsetMs, 4242, "an authoritative offset must not be recomputed");
});

test("deriveMissingOffsets: a pre-2026-04-22 REAL offset (no hasOffset) is untouched (F1)", () => {
  // The class this pass exists for is "offsetMs 0 AND no hasOffset". Every
  // message in a file written before 068465ed lacks hasOffset while carrying
  // a real offset — on a replay/VOD file, YouTube's own video-relative
  // videoOffsetTimeMsec. Deriving from wall-clock there would shift the whole
  // archive by the ingest latency. Mutant: `if (m.hasOffset) continue;`.
  const messages = [{ id: "legacy-real", offsetMs: 4242, timestampUsec: usecAt(9999) }];
  deriveMissingOffsets(messages, EPOCH);
  assert.deepEqual([messages[0].offsetMs, messages[0].hasOffset], [4242, undefined],
    "a legacy message's own offset is authoritative — the sentinel is offsetMs 0, not the flag");
});

test("deriveMissingOffsets: the division truncates, matching the Go int64 divide", () => {
  // The producer writes `usec/1000` on int64 — truncation, not rounding.
  // Mutant: Math.round → 1501 − EPOCH_MS. usec is an absolute microsecond
  // clock, so build it from the epoch and add a sub-millisecond remainder.
  const messages = [{ id: "a", offsetMs: 0, timestampUsec: String(EPOCH_MS * 1000 + 1_500_999) }];
  deriveMissingOffsets(messages, EPOCH);
  assert.equal(messages[0].offsetMs, 1500, "1_500_999 µs past the epoch is 1500 ms, not 1501");
});

test("deriveMissingOffsets: no usable header epoch → nothing changes", () => {
  const rows = () => [{ id: "a", offsetMs: 0, timestampUsec: usecAt(1500) }];
  for (const header of [undefined, "", null, "not a date"]) {
    const messages = rows();
    deriveMissingOffsets(messages, header);
    assert.deepEqual(messages.map((m) => [m.offsetMs, m.hasOffset]), [[0, undefined]],
      `header ${JSON.stringify(header)} must leave the pile at 0 — R10 bounds it`);
  }
});

test("deriveMissingOffsets: pre-show chat derives a NEGATIVE offset (N-F2)", () => {
  // Waiting-room messages precede the epoch. A clamp to 0 here would put the
  // whole waiting room back on the pile it was just rescued from.
  const messages = [{ id: "a", offsetMs: 0, timestampUsec: usecAt(-90_000) }];
  deriveMissingOffsets(messages, EPOCH);
  assert.deepEqual([messages[0].offsetMs, messages[0].hasOffset], [-90_000, true]);
});

test("deriveMissingOffsets: no derivable timestamp → left alone", () => {
  // Twitch chat files (internal/twitch/types.go) carry offsetMs with no
  // timestampUsec at all and their offsets are already video-relative — they
  // must fall through untouched, as must a missing/garbage/zero usec.
  const messages = [
    { id: "twitch", offsetMs: 7000 },
    { id: "garbage", offsetMs: 0, timestampUsec: "not-a-number" },
    { id: "empty", offsetMs: 0, timestampUsec: "" },
    { id: "zero", offsetMs: 0, timestampUsec: "0" },
  ];
  deriveMissingOffsets(messages, EPOCH);
  assert.deepEqual(messages.map((m) => [m.offsetMs, m.hasOffset]),
    [[7000, undefined], [0, undefined], [0, undefined], [0, undefined]]);
});

test("deriveMissingOffsets: a numeric timestampUsec works too", () => {
  // The Go producer writes a string, but an imported/hand-edited file may
  // carry a JSON number.
  const messages = [{ id: "a", offsetMs: 0, timestampUsec: Number(usecAt(3000)) }];
  deriveMissingOffsets(messages, EPOCH);
  assert.equal(messages[0].offsetMs, 3000);
});
