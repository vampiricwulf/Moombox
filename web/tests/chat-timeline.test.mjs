// Tests for web/public/modules/chat-timeline.js
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  normalizeOffsetMs, computeChatBiasMs, partitionChatByVideo, indexAfter, mergePartChats,
  formatChatHeader, dividerLabelFor,
} from "../public/modules/chat-timeline.js";

test("normalizeOffsetMs: numbers, json.Number strings, garbage", () => {
  assert.equal(normalizeOffsetMs(1500), 1500);
  assert.equal(normalizeOffsetMs(-90000), -90000);
  assert.equal(normalizeOffsetMs("123"), 123);   // imported chat: json.Number as string
  assert.equal(normalizeOffsetMs(undefined), 0);
  assert.equal(normalizeOffsetMs(null), 0);
  assert.equal(normalizeOffsetMs("abc"), 0);
  assert.equal(normalizeOffsetMs(NaN), 0);
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
