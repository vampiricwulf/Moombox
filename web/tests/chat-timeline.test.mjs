// Tests for web/public/modules/chat-timeline.js
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  normalizeOffsetMs, computeChatBiasMs, parseTimeMs, partitionChatByVideo, indexAfter, mergePartChats,
  formatChatHeader, dividerLabelFor, deriveMissingOffsets, correctLegacyTwitchEmotes,
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

// Owner ruling RV-19, with the two real captures of the chat-alignment
// measurement (2026-11-27). Each row is a chat message's stored offsetMs and
// where its moment sits in the muxed file ("frame at": its wall time minus the
// first segment's Ingestion-Walltime, which is the file's 0:00).
//
// Capture A: Lofi Girl, a 24/7 stream recorded from the live edge (HLS,
// marathon), 100 days into the broadcast. Its file covers 1805 s from
// 22:56:50.408; the ten messages were sent about an hour after its media, so
// their frames lie past its end — but at 3603.757 s, not 8,698,391 s.
const CAPTURE_A = {
  chatEpoch: "2026-08-19T07:43:43Z",                // chat header streamStartTime = job streamStartTime
  mediaStartTime: "2026-11-27T22:56:50.408302Z",    // sq 1738862's ingest wall clock
  joinOffsetSeconds: 8694310,                       // seq x 5, what the finalize stored
  durationMs: 1805000,
  rows: [
    [8698391165, 3603757], [8698402895, 3615487], [8698409367, 3621959], [8698415077, 3627669],
    [8698422597, 3635189], [8698431236, 3643829], [8698446510, 3659103], [8698465089, 3677681],
    [8698474612, 3687204], [8698487991, 3700583],
  ],
};
// Capture B: recorded from sq=0 (DASH). Today chat shows 6.236 s early.
const CAPTURE_B = {
  chatEpoch: "2026-11-27T23:29:11Z",
  mediaStartTime: "2026-11-27T23:29:04.76416Z",
  rows: [
    [1844146, 1850382], [1848075, 1854311], [1857488, 1863725], [1883529, 1889765], [1907570, 1913806],
    [1917906, 1924143], [1931169, 1937405], [1940514, 1946750], [1951398, 1957634], [1978496, 1984733],
  ],
};
const placed = (offsetMs, bias) => offsetMs - bias;

test("parseTimeMs: RFC3339Nano is read to the millisecond", () => {
  assert.equal(parseTimeMs("2026-11-27T22:56:50.408302Z"), Date.UTC(2026, 10, 27, 22, 56, 50, 408));
  assert.equal(parseTimeMs("2026-11-27T22:56:50.408302123Z"), Date.UTC(2026, 10, 27, 22, 56, 50, 408));
  assert.equal(parseTimeMs("2026-11-27T22:56:50Z"), Date.UTC(2026, 10, 27, 22, 56, 50));
  assert.ok(Number.isNaN(parseTimeMs("")));
  assert.ok(Number.isNaN(parseTimeMs(undefined)));
});

// ECMAScript only promises three fractional digits; V8 happens to read more,
// so the case above passes in node whatever the trim does. This one checks
// what Date.parse is handed. Mutant: the trim's pattern unescaped (`.d{3}`,
// which matches nothing in a time, so all nine digits reach Date.parse).
test("parseTimeMs hands Date.parse at most three fractional digits", () => {
  const seen = [];
  const real = Date.parse;
  Date.parse = (s) => { seen.push(s); return real(s); };
  try {
    parseTimeMs("2026-11-27T22:56:50.408302123Z");
    parseTimeMs("2026-11-27T22:56:50.408302Z");
    parseTimeMs("2026-11-27T22:56:50Z");
  } finally {
    Date.parse = real;
  }
  assert.deepEqual(seen, ["2026-11-27T22:56:50.408Z", "2026-11-27T22:56:50.408Z", "2026-11-27T22:56:50Z"]);
});

test("computeChatBiasMs: an anchored live-edge capture puts each message on its frame (capture A)", () => {
  const bias = computeChatBiasMs({
    chatStreamStartTime: CAPTURE_A.chatEpoch, jobStreamStartTime: CAPTURE_A.chatEpoch,
    mediaStartTime: CAPTURE_A.mediaStartTime, joinOffsetSeconds: CAPTURE_A.joinOffsetSeconds,
  });
  assert.equal(bias, 8694787408);
  for (const [offsetMs, frameMs] of CAPTURE_A.rows) {
    // The measured "frame at" is to the millisecond; the stored offset is
    // whole ms, so a row may round either way.
    assert.ok(Math.abs(placed(offsetMs, bias) - frameMs) <= 1, `offset ${offsetMs} placed at ${placed(offsetMs, bias)}, frame at ${frameMs}`);
  }
});

test("computeChatBiasMs: capture A's chat lands inside the file only with the anchor", () => {
  // A message sent at 23:10:00, a moment the file holds (it covers 22:56:50.408
  // to 23:26:55.4): its offset counts from the chat epoch.
  const offsetMs = Date.UTC(2026, 10, 27, 23, 10, 0) - Date.parse(CAPTURE_A.chatEpoch);
  const anchored = computeChatBiasMs({ chatStreamStartTime: CAPTURE_A.chatEpoch, jobStreamStartTime: CAPTURE_A.chatEpoch, mediaStartTime: CAPTURE_A.mediaStartTime });
  assert.equal(placed(offsetMs, anchored), 789592);
  const one = [{ id: "m", offsetMs: placed(offsetMs, anchored) }];
  assert.deepEqual(partitionChatByVideo(one, CAPTURE_A.durationMs), { preCount: 0, firstLiveIndex: 0, postCount: 0, firstPostIndex: -1 });
  // Without anchor or join point (the bug): 100 days past the end.
  const before = computeChatBiasMs({ chatStreamStartTime: CAPTURE_A.chatEpoch, jobStreamStartTime: CAPTURE_A.chatEpoch });
  assert.equal(before, 0);
  assert.equal(partitionChatByVideo([{ id: "m", offsetMs: placed(offsetMs, before) }], CAPTURE_A.durationMs).postCount, 1);
});

test("computeChatBiasMs: an unanchored late join adds its join point (capture A archived before the anchor)", () => {
  const bias = computeChatBiasMs({
    chatStreamStartTime: CAPTURE_A.chatEpoch, jobStreamStartTime: CAPTURE_A.chatEpoch,
    joinOffsetSeconds: CAPTURE_A.joinOffsetSeconds,
  });
  assert.equal(bias, 8694310000);
  // Off by the stored join point's drift (477.408 s on 100 days), not by 100 days.
  const [offsetMs, frameMs] = CAPTURE_A.rows[0];
  assert.equal(placed(offsetMs, bias) - frameMs, 477408);
  // Added to today's bias (a late actual start), not in place of it.
  assert.equal(computeChatBiasMs({
    chatStreamStartTime: "2026-06-11T10:00:00Z", jobStreamStartTime: "2026-06-11T10:12:00Z", joinOffsetSeconds: 90.5,
  }), 12 * 60 * 1000 + 90500);
});

test("computeChatBiasMs: an anchored from-start capture loses its 6.236 s early shift (capture B)", () => {
  const today = computeChatBiasMs({ chatStreamStartTime: CAPTURE_B.chatEpoch, jobStreamStartTime: CAPTURE_B.chatEpoch, joinOffsetSeconds: 0 });
  assert.equal(today, 0);
  const [o0, f0] = CAPTURE_B.rows[0];
  assert.equal(placed(o0, today) - f0, -6236);
  const bias = computeChatBiasMs({
    chatStreamStartTime: CAPTURE_B.chatEpoch, jobStreamStartTime: CAPTURE_B.chatEpoch,
    mediaStartTime: CAPTURE_B.mediaStartTime, joinOffsetSeconds: 0,
  });
  assert.equal(bias, -6236);
  for (const [offsetMs, frameMs] of CAPTURE_B.rows) {
    assert.ok(Math.abs(placed(offsetMs, bias) - frameMs) <= 1, `offset ${offsetMs} placed at ${placed(offsetMs, bias)}, frame at ${frameMs}`);
  }
});

test("computeChatBiasMs: a from-start row without an anchor, Twitch, and a bad anchor keep today's rule", () => {
  const base = { chatStreamStartTime: "2026-06-11T10:00:00Z", jobStreamStartTime: "2026-06-11T10:12:00Z" };
  for (const join of [0, null, undefined, -5, "garbage"]) {
    assert.equal(computeChatBiasMs({ ...base, joinOffsetSeconds: join }), 12 * 60 * 1000, `join ${join}`);
  }
  assert.equal(computeChatBiasMs({ ...base, mediaStartTime: "garbage" }), 12 * 60 * 1000);
  assert.equal(computeChatBiasMs({ ...base, platform: "twitch", mediaStartTime: CAPTURE_A.mediaStartTime, joinOffsetSeconds: 90 }), 0);
  // No chat epoch: no wall-clock offsets to move.
  assert.equal(computeChatBiasMs({ chatStreamStartTime: "", jobStreamStartTime: base.jobStreamStartTime, mediaStartTime: CAPTURE_A.mediaStartTime }), 0);
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
  assert.equal(formatChatHeader(1, 0, 0), "1 message");
});

test("dividerLabelFor: one label per region boundary, null everywhere else", () => {
  const p = partitionChatByVideo(msgs(-90000, -5000, 0, 1000, 61000), 60000);
  assert.deepEqual(p, { preCount: 2, firstLiveIndex: 2, postCount: 1, firstPostIndex: 4 });
  assert.equal(dividerLabelFor(p, 2), "Waiting room — 2 messages before the stream");
  assert.equal(dividerLabelFor(p, 4), "Recording ended — 1 message after it");
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
  assert.equal(dividerLabelFor(p, 1), "Waiting room — 1 message before the stream");
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

// ── Legacy Twitch emote-offset correction ───────────────────────────────────

const SOH = "\u0001";

/**
 * The message text every fixture below shares: 8 code points, 9 UTF-16 units.
 * "Kappa" sits at code points 2..6 and at UTF-16 units 3..7.
 *
 * The trailing "!" is LOAD-BEARING. Without it an already-correct span (3..7)
 * would end at the LAST code point, and the corrector's bounds guard
 * (`en >= cps.length`) would skip the emote before any gate was consulted —
 * so every "left untouched" assertion here would pass with the gates deleted.
 */
const TEXT = "🎉 Kappa!";

/** One live-IRC message: `raw` is what marks it IRC-recorded rather than VOD. */
const ircMsg = (message, emotes) => ({
  offsetMs: 0, authorName: "u", message, emotes,
  raw: `@emotes=x :u!u@u.tmi.twitch.tv PRIVMSG #c :${message}`,
});

test("correctLegacyTwitchEmotes: a marked file is left exactly as written", () => {
  // Mutant: deleting the `emoteOffsets === "utf16"` gate and correcting on the
  // strength of `raw` alone. Every IRC message has `raw`, so that mutant
  // re-shifts every span in every NEW file — this one to
  // { name: "appa!", start: 4, end: 8 }.
  const data = {
    platform: "twitch", emoteOffsets: "utf16",
    messages: [ircMsg(TEXT, [{ id: "25", name: "Kappa", start: 3, end: 7 }])],
  };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].emotes, [{ id: "25", name: "Kappa", start: 3, end: 7 }]);
  assert.equal(data.messages[0].message, TEXT);
});

test("correctLegacyTwitchEmotes: an unmarked IRC message is mapped code point → UTF-16", () => {
  // The legacy producer stored the WIRE (code-point) offsets and a name sliced
  // out of them in UTF-16 space, i.e. " Kapp". Both have to be repaired.
  // Mutant: mapping start but not end (end = cpToUnit[end]) clips the span to
  // "Kapp"; mutant: leaving `name` alone renders the garbled " Kapp" as the
  // emote's alt text.
  const data = {
    platform: "twitch",
    messages: [ircMsg(TEXT, [{ id: "25", name: " Kapp", start: 2, end: 6 }])],
  };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].emotes, [{ id: "25", name: "Kappa", start: 3, end: 7 }]);
  assert.equal(data.messages[0].message.substring(3, 8), "Kappa");
});

test("correctLegacyTwitchEmotes: an unmarked VOD comment is untouched", () => {
  // No `raw` = it came from the GQL VOD path, which emitted UTF-16 all along.
  // Mutant: deleting the per-message `raw` gate corrects every message in an
  // unmarked file, shifting this already-UTF-16 span to
  // { name: "appa!", start: 4, end: 8 } — every VOD archive's emotes move.
  const vod = { offsetMs: 0, authorName: "u", message: TEXT,
    emotes: [{ id: "25", name: "Kappa", start: 3, end: 7 }] };
  const data = { platform: "twitch", messages: [vod] };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].emotes, [{ id: "25", name: "Kappa", start: 3, end: 7 }]);
});

test("correctLegacyTwitchEmotes: an unmarked /me is unwrapped and re-indexed", () => {
  // Legacy stored the wrapped text with offsets that index the STRIPPED text.
  // Mutant: building the code-point table from the WRAPPED text (re-indexing
  // before the unwrap) reads 2..6 out of "\x01ACTION …" and yields
  // { name: "CTION", start: 2, end: 6 }.
  const data = {
    platform: "twitch",
    messages: [ircMsg(`${SOH}ACTION ${TEXT}${SOH}`, [{ id: "25", name: "x", start: 2, end: 6 }])],
  };
  correctLegacyTwitchEmotes(data);
  assert.equal(data.messages[0].message, TEXT);
  assert.equal(data.messages[0].isAction, true);
  assert.deepEqual(data.messages[0].emotes, [{ id: "25", name: "Kappa", start: 3, end: 7 }]);
});

test("correctLegacyTwitchEmotes: out-of-range and inverted spans are left alone", () => {
  // The Go producer passes a malformed wire range through untouched; the
  // corrector must not invent a span for it. TEXT (not an ASCII string) is the
  // fixture on purpose: over ASCII the code-point map is the identity, so a
  // dropped guard would move nothing and this would pin nothing.
  // Mutant: dropping `en >= cps.length` gives the first emote
  // { name: "🎉 Kappa!", start: 0, end: NaN } — cpToUnit[100] is undefined — and
  // _appendTwitchMessage then swallows the rest of the line. Mutant: dropping
  // `s > en` moves the inverted span to { start: 5, end: 3 }.
  const data = {
    platform: "twitch",
    messages: [ircMsg(TEXT, [
      { id: "25", name: "", start: 0, end: 99 },
      { id: "26", name: "", start: 4, end: 2 },
    ])],
  };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].emotes, [
    { id: "25", name: "", start: 0, end: 99 },
    { id: "26", name: "", start: 4, end: 2 },
  ]);
});

test("correctLegacyTwitchEmotes: a non-Twitch file is not its business", () => {
  // Gate 1 (the FILE's platform) is read before the messages are walked at all.
  // The message is deliberately given a Twitch IRC shape — `raw` plus a legacy
  // code-point span — so that the platform gate is the only thing stopping it.
  // Mutant: deleting `data.platform !== "twitch"` → { name: "Kappa", start: 3,
  // end: 7 }.
  const data = {
    platform: "youtube",
    messages: [ircMsg(TEXT, [{ id: "25", name: " Kapp", start: 2, end: 6 }])],
  };
  correctLegacyTwitchEmotes(data);
  assert.deepEqual(data.messages[0].emotes, [{ id: "25", name: " Kapp", start: 2, end: 6 }]);

  // A realistically shaped YouTube message — `message` is an array of runs and
  // only TwitchChatMessage carries `raw` — is stopped by BOTH gates, which is
  // why the synthetic fixture above is what pins the platform one.
  const yt = { messages: [{ offsetMs: 0, message: [{ text: "hi" }] }] };
  correctLegacyTwitchEmotes(yt);
  assert.deepEqual(yt.messages[0].message, [{ text: "hi" }]);
});
