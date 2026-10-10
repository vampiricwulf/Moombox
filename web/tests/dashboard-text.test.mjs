// Small user-facing text that read wrong: "Chat: 1 messages" in a job's
// progress tooltip, "0B used of 0B · 0B free" on the Stats tab before any
// disk reading, and a single-file recording's load error blamed on a missing
// segment. (The chat header and divider counts are pinned in
// chat-timeline.test.mjs.)
//
// Same jsdom probe as app.test.mjs: an absent jsdom skips, anything else fails.
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
const playerHarness = jsdomMissing ? null : await import("./helpers/player-dom.mjs");
const skip = jsdomMissing || false;

after(() => {
  harness?.teardownAll();
  playerHarness?.teardownAll();
});

// The Imports tab's help is the only in-app guidance, and it described the
// old rules: any video, and "optionally a chat JSON file" — a chat.json is
// now left out unless it is named after the video, and a second video is a
// 400. Mutant: the old sentence.
test("the Imports tab's help states the one-recording, name-paired rule", { skip }, async () => {
  const h = await harness.makeApp();
  const help = h.window.document.querySelector("#imports-container .settings-help").textContent;
  assert.match(help, /one recording/);
  assert.match(help, /<name> - part1/);
  assert.match(help, /<name>\.chat\.json/);
});

// Mutant: restore the fixed "messages" suffix.
test("a progress tooltip counts one chat message in the singular", { skip }, async () => {
  const h = await harness.makeApp();
  assert.equal(h.app.formatProgressTooltip({ progress: "Seq: 42 C: 1" }), "Segments: 42, Chat: 1 message");
  assert.equal(h.app.formatProgressTooltip({ progress: "V:95.3% C: 2" }), "Video: 95.3% downloaded, Chat: 2 messages");
  assert.equal(h.app.formatProgressTooltip({ progress: "(V: 7/9 A: 7/9 C: 1)" }),
    "Video: 7/9 segments, Audio: 7/9 segments, Chat: 1 message");
});

// Mutant: drop the total > 0 check — the tab reads "0B used of 0B".
test("the Stats tab says when there is no disk reading", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.stats.renderDisk({ free: 0, total: 0, usedPct: 0, warnLevel: "ok" });
  assert.equal(h.el("stats-disk").textContent, "No disk reading available");

  h.app.stats.renderDisk({ free: 50, total: 100, usedPct: 50, warnLevel: "ok" });
  assert.match(h.el("stats-disk").textContent, /used of/);
});

// Mutant: always mention a segment again.
test("a single-file recording's load error does not blame a segment", { skip }, async () => {
  const job = { id: "j1", status: "Finished", filename: "j1.mp4", title: "T", channelName: "C", updatedAt: "2026-09-01T00:00:00Z" };
  const h = playerHarness.makePlayer({ jobs: [job], watchState: {} });
  await h.selectJob("j1");
  Object.defineProperty(h.video, "error", { configurable: true, value: { message: "404" } });
  h.video.dispatchEvent(new h.window.Event("error"));
  assert.deepEqual(h.app.toasts.map((t) => t.message), ["Video failed to load"]);
});
