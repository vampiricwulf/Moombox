// Tests for web/public/modules/utils.js

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  formatTimestamp,
  formatBytes,
  formatDurationSeconds,
  formatMsToTime,
  safePlay,
} from "../public/modules/utils.js";

test("formatTimestamp: zero and invalid inputs", () => {
  assert.equal(formatTimestamp(0), "0:00");
  assert.equal(formatTimestamp(null), "0:00");
  assert.equal(formatTimestamp(undefined), "0:00");
  assert.equal(formatTimestamp(NaN), "0:00");
  assert.equal(formatTimestamp(Infinity), "0:00");
  assert.equal(formatTimestamp(-5), "0:00");
});

test("formatTimestamp: minutes:seconds", () => {
  assert.equal(formatTimestamp(30), "0:30");
  assert.equal(formatTimestamp(59), "0:59");
  assert.equal(formatTimestamp(60), "1:00");
  assert.equal(formatTimestamp(125), "2:05");
});

test("formatTimestamp: hours:minutes:seconds", () => {
  assert.equal(formatTimestamp(3600), "1:00:00");
  assert.equal(formatTimestamp(3665), "1:01:05");
  assert.equal(formatTimestamp(36000), "10:00:00");
});

test("formatBytes: each unit boundary", () => {
  assert.equal(formatBytes(0), "0B");
  assert.equal(formatBytes(512), "512B");
  assert.equal(formatBytes(1024), "1.0KB");
  assert.equal(formatBytes(1536), "1.5KB");
  assert.equal(formatBytes(1024 * 1024), "1.0MB");
  assert.equal(formatBytes(1024 * 1024 * 1024), "1.0GB");
  assert.equal(formatBytes(1024 * 1024 * 1024 * 1024), "1.0TB");
});

test("formatBytes: invalid inputs coerce to 0B", () => {
  assert.equal(formatBytes(null), "0B");
  assert.equal(formatBytes(NaN), "0B");
  assert.equal(formatBytes(-100), "0B");
});

test("formatDurationSeconds", () => {
  assert.equal(formatDurationSeconds(0), "0s");
  assert.equal(formatDurationSeconds(45), "45s");
  assert.equal(formatDurationSeconds(90), "1m 30s");
  assert.equal(formatDurationSeconds(3661), "1h 1m 1s");
  assert.equal(formatDurationSeconds(null), "0s");
});

test("formatMsToTime: positive values", () => {
  assert.equal(formatMsToTime(0), "0:00");
  assert.equal(formatMsToTime(30_000), "0:30");
  assert.equal(formatMsToTime(90_500), "1:30");
  assert.equal(formatMsToTime(3_600_000), "1:00:00");
});

test("formatMsToTime: negative (pre-stream waiting-room chat)", () => {
  assert.equal(formatMsToTime(-30_000), "-0:30");
  assert.equal(formatMsToTime(-125_000), "-2:05");
  assert.equal(formatMsToTime(-3_600_000), "-1:00:00");
});

test("formatMsToTime: invalid inputs return 0:00", () => {
  assert.equal(formatMsToTime(null), "0:00");
  assert.equal(formatMsToTime(NaN), "0:00");
  assert.equal(formatMsToTime(Infinity), "0:00");
});

test("safePlay swallows a rejected play() promise and tolerates a void return", async () => {
  let rejections = 0;
  const onUnhandled = () => { rejections++; };
  process.on("unhandledRejection", onUnhandled);
  try {
    safePlay({ play: () => Promise.reject(new Error("AbortError")) });
    safePlay({ play: () => undefined });
    // phase-2-review.md §4 mutant MU6: the `media && media.play` guard is the
    // documented tolerance, so pin it — a bare `media.play()` throws on both.
    safePlay(null);
    safePlay({});
    await new Promise((r) => setImmediate(r));
    assert.equal(rejections, 0);
  } finally {
    process.off("unhandledRejection", onUnhandled);
  }
});
