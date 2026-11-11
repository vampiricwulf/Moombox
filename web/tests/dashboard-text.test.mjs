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

// Owner ruling R-D1 of 2026-11-10: a Muxing row's progress line is the worker's mux readout
// ("Muxing 42%", then "Muxing: finalizing"), and the dashboard shows the line
// as it is, on the card and in the details dialog (both call formatProgress;
// the dialog's row is the next test).
// The percent bar is not the readout's. Mutant: formatProgress answering the
// fixed "Muxing..." for every Muxing row.
test("a Muxing row shows the mux readout the worker wrote", { skip }, async () => {
  const h = await harness.makeApp();
  assert.equal(h.app.formatProgress({ status: "Muxing", progress: "Muxing 42%", percent: 100 }), "Muxing 42%");
  assert.equal(h.app.formatProgress({ status: "Muxing", progress: "Muxing: finalizing", percent: 100 }), "Muxing: finalizing");
  assert.equal(h.app.formatProgress({ status: "Muxing", progress: "" }), "Muxing...");
});

// The dialog half of R-D1. A live capture (not a VOD) takes the dialog's
// "Type: Live" arm, which had no Progress row, so the readout showed on the
// card only while the TUI's details pane showed it. Mutant: the Progress row
// drawn for VOD rows alone.
test("the details dialog of a live capture being muxed shows the mux readout", { skip }, async () => {
  const row = (status, progress) => ({
    id: "A", title: "T A", videoId: "A", channelName: "Chan", platform: "youtube", isVod: false,
    status, progress, percent: 100, lastVideoSeq: 1300, lastAudioSeq: 1300,
    createdAt: "2026-09-05T11:00:00Z", updatedAt: "2026-09-05T11:00:00Z",
  });
  let status = "Muxing";
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": () => ({ ...row(status, ""), hasStaging: true, hasSegments: true, asides: [], keptChatSidecar: false, unmuxedParts: false }),
      "GET /api/jobs/:id/logs": () => [],
    },
  });
  const dlg = h.el("details-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [row("Muxing", "Muxing 42%")] } });
  await h.flush();
  h.app.details.showJobDetails(h.app.jobs[0]);
  await h.flush(); await h.flush();
  const field = () => h.el("job-details-content").querySelector('[data-field="progress"]');
  assert.equal(field()?.textContent, "Muxing 42%");
  assert.ok(h.el("job-details-content").querySelector('[data-field="segments"]'), "the Segments row is kept beside it");

  h.app.details.updateJobDetails(row("Muxing", "Muxing: finalizing"));
  assert.equal(field()?.textContent, "Muxing: finalizing");

  // Outside Muxing a live capture has no Progress row, as before.
  status = "Finished";
  h.app.details.updateJobDetails(row("Finished", ""));
  await h.flush();
  assert.equal(field(), null);
});
