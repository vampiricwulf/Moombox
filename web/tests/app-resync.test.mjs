// A mid-session `initial_state` must REPLACE the dashboard's job list.
//
// The server sends a full snapshot as the first frame after it drops one for a
// lagging client (internal/web/websocket.go, sweep T3-26). That fix rests
// entirely on app.js treating `initial_state` as a full-state replace rather
// than a first-connect hydrate — true today, pinned here.
//
// Like the other DOM suites this needs jsdom; the import is probed first so
// `node --test web/tests/*.test.mjs` stays green without it.
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
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const job = (id, title) => ({
  id, title, videoId: id, channelName: "Chan", platform: "youtube", status: "Downloading",
});

test("a mid-session initial_state replaces the job list", { skip }, async () => {
  const h = await harness.makeApp();

  // The state a lagging tab is left in: a row the dropped job_deleted retired.
  h.app.jobs = [job("ghost", "Ghost row"), job("keep", "Still here")];
  h.app.renderJobs();
  assert.ok(
    h.el("jobs-container").querySelector('.video-item[data-job-id="ghost"]'),
    "fixture: the ghost row must be on screen before the resync",
  );

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [job("keep", "Still here")] } });
  await h.flush();

  assert.deepEqual(h.app.jobs.map((j) => j.id), ["keep"],
    "initial_state must REPLACE this.jobs — an upsert-style merge (the mutant) keeps the ghost");
  assert.equal(
    h.el("jobs-container").querySelector('.video-item[data-job-id="ghost"]'), null,
    "the ghost row must be gone from the DOM, not just from the array",
  );
  assert.ok(
    h.el("jobs-container").querySelector('.video-item[data-job-id="keep"]'),
    "the surviving row must still be drawn",
  );
});

test("a mid-session initial_state replaces the log buffer", { skip }, async () => {
  const h = await harness.makeApp();

  h.app.logPanel.logs = ["INFO stale line"];
  h.app.logPanel.renderLogs();

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], logs: ["INFO fresh line"] } });
  h.flushRaf();
  await h.flush();

  assert.deepEqual(h.app.logPanel.logs, ["INFO fresh line"],
    "the snapshot's logs must replace the buffer — appending them (the mutant) doubles the history " +
    "of every re-synced tab");
  assert.match(h.el("logs-viewer").textContent, /fresh line/);
  assert.doesNotMatch(h.el("logs-viewer").textContent, /stale line/);
});
