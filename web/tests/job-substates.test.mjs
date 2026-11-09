// Derived job sub-states shown in the dashboard (audit rule 5, Arc D): the
// automatic action an Error row waits on (owner ruling R-D2). The pure
// function runs against the table cmd/moombox's Go parity test runs
// internal/jobstate against; the DOM cases run under jsdom.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import {
  pendingAutoAction, pendingAutoActionText, reinitializeDeletesText, sumReinitializePreviews, isWaitLine,
  queuedHold, queuedHoldText, diskGaugeLevel,
} from "../public/modules/utils.js";

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

const PARITY = JSON.parse(readFileSync(new URL("./fixtures/job-state-parity.json", import.meta.url), "utf8"));

// Mutant: any arm of pendingAutoAction dropped or reordered (the end-
// unconfirmed row with the flap's text reads auto_retry).
test("pendingAutoAction answers the shared table as jobstate does", () => {
  assert.ok(PARITY.pendingAutoAction.length >= 10);
  for (const c of PARITY.pendingAutoAction) {
    assert.equal(pendingAutoAction(c.job), c.want, c.name);
  }
  assert.equal(pendingAutoAction(null), "");
  assert.equal(pendingAutoActionText(""), "");
  assert.match(pendingAutoActionText("auto_mux"), /Will mux automatically/);
  assert.match(pendingAutoActionText("auto_retry"), /Will retry automatically/);
});

// The Go twin is worker TestReinitializePreviewSummary's cancels cases.
//
// Mutants: the cancels clause dropped; the pending counts left out of the sum.
test("the Reinitialize clause names the automatic action it cancels", () => {
  assert.equal(reinitializeDeletesText({ stagedFiles: 1, stagedBytes: 10, pendingAutoMux: 1 }),
    "deletes 1 staged file (10B); it cancels the pending automatic mux");
  assert.equal(reinitializeDeletesText({ pendingAutoRetry: 1 }),
    "deletes nothing recorded; it cancels the pending automatic retry");
  assert.equal(reinitializeDeletesText({ pendingAutoMux: 2, pendingAutoRetry: 3 }),
    "deletes nothing recorded; it cancels 2 pending automatic muxes and 3 pending automatic retries");
  const sum = sumReinitializePreviews([{ pendingAutoMux: 1 }, { pendingAutoRetry: 1, pendingAutoMux: 1 }]);
  assert.equal(sum.pendingAutoMux, 2);
  assert.equal(sum.pendingAutoRetry, 1);
});

const muxRow = (over = {}) => ({
  id: "tw_m", videoId: "m", title: "Latched", channelName: "C", platform: "twitch", status: "Error",
  error: "twitch HLS: 500", parkReason: "twitch_end_unconfirmed",
  createdAt: "2026-11-09T11:00:00Z", updatedAt: "2026-11-09T11:00:00Z", ...over,
});

// Mutant: formatProgressHtml not adding the tag (the row shows the error alone).
test("an Error row the monitor will mux says so in the job list", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [muxRow(), muxRow({ id: "plain", parkReason: undefined, error: "boom" })];
  h.app.renderJobs();
  const cell = (id) => h.document.querySelector(`.video-item[data-job-id="${id}"] .job-progress-text`);
  assert.match(cell("tw_m").textContent, /Auto-mux pending/);
  assert.match(cell("tw_m").querySelector(".job-auto-action").title, /Will mux automatically/);
  assert.equal(cell("plain").querySelector(".job-auto-action"), null);
});

// Mutants: the details block not rendered; the update path not hiding it
// once the row leaves Error.
test("the job details name the pending automatic action and drop it when the row moves on", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": () => ({ ...muxRow(), hasStaging: true, hasSegments: true, asides: [], keptChatSidecar: false,
        unmuxedParts: false, muxGaps: [], pendingAutoAction: "auto_mux" }),
      "GET /api/jobs/:id/logs": () => [],
    },
  });
  const dlg = h.el("details-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [muxRow()] } });
  await h.flush();
  h.app.details.showJobDetails(h.app.jobs[0]);
  await h.flush(); await h.flush();
  const block = () => h.document.querySelector(".details-auto-action");
  assert.ok(block() && !block().hidden, "the pending mux is shown");
  assert.match(block().textContent, /Will mux automatically/);

  h.app.handleMessage({ type: "job_update", payload: muxRow({ status: "Muxing", parkReason: undefined, error: undefined }) });
  await h.flush(); await h.flush();
  assert.ok(!block() || block().hidden, "a row that left Error shows no pending action");
});

// Mutant: reinitializeJob not passing the preview's pending counts through
// (sumReinitializePreviews dropping them).
test("the Reinitialize confirm warns that it cancels the pending mux", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id/reinitialize/preview": () => ({ allowed: true, stagedFiles: 2, stagedBytes: 2048, pendingAutoMux: 1 }),
      "POST /api/jobs/:id/reinitialize": () => ({ success: true }),
    },
  });
  h.app.jobs = [muxRow()];
  let asked = null;
  h.app.showConfirm = async (msg) => { asked = msg; return false; };
  await h.app.reinitializeJob("tw_m");
  await h.flush();
  assert.match(asked ?? "", /This deletes 2 staged files \(2\.0KB\); it cancels the pending automatic mux\./);
});

// Mutant: isWaitLine reading the marker alone (the closing paren unchecked),
// or a different marker.
test("isWaitLine answers the shared table as jobstate.IsWaitLine does", () => {
  assert.ok(PARITY.waitLines.length >= 8);
  for (const c of PARITY.waitLines) assert.equal(isWaitLine(c.line), c.want, JSON.stringify(c.line));
  assert.equal(isWaitLine(undefined), false);
});

const upcoming = (over = {}) => ({
  id: "u1", videoId: "u1", title: "Up", channelName: "C", platform: "youtube", status: "Upcoming",
  createdAt: "2026-11-09T11:00:00Z", updatedAt: "2026-11-09T11:00:00Z", ...over,
});

// Owner ruling R-D1: an Upcoming row shows the wait it is in, an admitted
// backlog VOD or a classified VOD reads "Awaiting processing", and Twitch's
// "Waiting for stream..." keeps the last-check time.
//
// Mutants: formatProgress never reading job.progress for Upcoming (the
// slot wait reads "Last check"); the awaiting-processing arm dropped (the
// admitted backlog VOD reads "Waiting for stream..."); job.progress shown
// for any non-empty text (Twitch's line hides its last check).
test("an Upcoming row shows its wait line, a VOD awaits processing, Twitch keeps its last check", { skip }, async () => {
  const h = await harness.makeApp();
  const recheck = harness.agoISO(30);
  const fp = (job) => h.app.formatProgress(job);
  assert.equal(fp(upcoming({ progress: "Waiting for a download slot... (3m)", isVod: true, lastRecheckAt: recheck })),
    "Waiting for a download slot... (3m)");
  assert.equal(fp(upcoming({ progress: "Rate-limited by YouTube; fetching again... (24m 0s left)", lastRecheckAt: recheck })),
    "Rate-limited by YouTube; fetching again... (24m 0s left)");
  assert.equal(fp(upcoming({ progress: "Waiting for network connectivity... (10s)" })), "Waiting for network connectivity... (10s)");
  assert.equal(fp(upcoming({ queuePriority: 1, channelId: "UCx" })), "Awaiting processing");
  assert.equal(fp(upcoming({ isVod: true, lastRecheckAt: recheck })), "Awaiting processing");
  assert.match(fp(upcoming({ platform: "twitch", progress: "Waiting for stream...", lastRecheckAt: recheck })), /^Last check: /);
  assert.equal(fp(upcoming({ platform: "twitch", progress: "Waiting for stream..." })), "Waiting for stream...");
  assert.equal(fp(upcoming()), "Waiting for stream...");
});

// The relative-time refresher rewrites a cell stamped with data-timestamp; a
// wait line's cell must not be stamped, or the next tick writes "Last check"
// over it — on the first render and on the no-rebuild update path alike.
//
// Mutant: the card template or updateJobCard stamping on lastRecheckAt alone.
test("a wait line's cell carries no last-check timestamp to overwrite it", { skip }, async () => {
  const h = await harness.makeApp();
  const recheck = harness.agoISO(30);
  h.app.jobs = [upcoming({ progress: "Waiting for a download slot... (<1m)", lastRecheckAt: recheck }), upcoming({ id: "u2", lastRecheckAt: recheck })];
  h.app.renderJobs();
  const cell = (id) => h.document.querySelector(`.video-item[data-job-id="${id}"] .job-progress-text`);
  assert.equal(cell("u1").dataset.timestamp, undefined);
  assert.equal(cell("u1").textContent, "Waiting for a download slot... (<1m)");
  assert.equal(cell("u2").dataset.timestamp, recheck);

  h.app.handleMessage({ type: "job_update", payload: { ...h.app.jobs[1], progress: "Waiting for a download slot... (1m)" } });
  await h.flush();
  assert.equal(cell("u2").dataset.timestamp, undefined);
  assert.equal(cell("u2").textContent, "Waiting for a download slot... (1m)");
});

// Mutants: a reordered precedence (disk ahead of offline); the platform
// check on the rate limit dropped; a run-out hold still reported.
test("queuedHold answers the shared table as jobstate does", () => {
  assert.ok(PARITY.queuedHolds.length >= 10);
  for (const c of PARITY.queuedHolds) {
    const got = queuedHold(c.holds, c.job, Date.parse(c.now));
    const want = { reason: c.want.reason };
    if (c.want.until) want.until = c.want.until;
    if (c.want.attempt) { want.attempt = c.want.attempt; want.of = c.want.of; }
    assert.deepEqual(got, want, c.name);
  }
  assert.deepEqual(queuedHold(null, { status: "Queued" }), { reason: "" });
});

test("queuedHoldText words a hold as the TUI does", () => {
  const at = new Date(2026, 10, 9, 14, 5).toISOString(); // 14:05 on this machine's clock
  assert.equal(queuedHoldText({ reason: "" }), "Waiting in queue...");
  assert.equal(queuedHoldText({ reason: "offline" }), "Held: offline");
  assert.equal(queuedHoldText({ reason: "disk" }), "Held: disk full");
  assert.equal(queuedHoldText({ reason: "rate_limit", until: at }), "Held: YouTube rate limit until 14:05");
  assert.equal(queuedHoldText({ reason: "retry", until: at, attempt: 2, of: 3 }), "Retry 2 of 3 at 14:05");
});

// Mutant: diskGaugeLevel ignoring the gate (warn while it holds), or
// reporting holding over critical.
test("the disk gauge says holding while the gate holds below critical", () => {
  assert.equal(diskGaugeLevel({ warnLevel: "warn" }, { diskHeld: true }), "holding");
  assert.equal(diskGaugeLevel({ warnLevel: "ok" }, { diskHeld: true }), "holding");
  assert.equal(diskGaugeLevel({ warnLevel: "critical" }, { diskHeld: true }), "critical");
  assert.equal(diskGaugeLevel({ warnLevel: "warn" }, { diskHeld: false }), "warn");
  assert.equal(diskGaugeLevel({ warnLevel: "warn" }, null), "warn");
});

const queued = (over = {}) => ({
  id: "q1", videoId: "q1", title: "Backlog", channelName: "C", platform: "youtube", status: "Queued",
  queuePriority: 1, channelId: "UCx", createdAt: "2026-11-09T11:00:00Z", updatedAt: "2026-11-09T11:00:00Z", ...over,
});

// Owner ruling R-D3 on the dashboard: Queued rows say why they are held, from
// /api/status at boot and from each admission_holds frame after, and the
// disk indicator says "holding" while the disk gate holds.
//
// Mutants: formatProgress keeping the constant "Waiting in queue..."; the
// admission_holds frame not handled; updateDiskIndicator ignoring the holds.
test("Queued rows and the disk indicator follow the admission holds", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.jobs = [queued()];
  h.app.renderJobs();
  const cell = () => h.document.querySelector('.video-item[data-job-id="q1"] .job-progress-text');
  assert.equal(cell().textContent, "Waiting in queue...");

  h.app.handleMessage({ type: "admission_holds", payload: { offline: false, diskHeld: true } });
  assert.equal(cell().textContent, "Held: disk full");

  h.app.stats.updateDiskIndicator({ free: 12 * 2 ** 30, total: 200 * 2 ** 30, usedPct: 94, warnLevel: "warn" });
  const ind = h.document.getElementById("disk-indicator");
  assert.match(ind.textContent, /holding/);
  assert.match(ind.className, /disk-indicator-holding/);

  const until = new Date(Date.now() + 20 * 60000).toISOString();
  h.app.handleMessage({ type: "admission_holds", payload: { offline: false, diskHeld: false,
    jobs: { q1: { attempt: 2, of: 3, until } } } });
  assert.match(cell().textContent, /^Retry 2 of 3 at \d\d:\d\d$/);
  assert.doesNotMatch(ind.textContent, /holding/, "the reopened gate redraws the indicator");
});
