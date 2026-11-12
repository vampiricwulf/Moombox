// Derived job sub-states shown in the dashboard (audit rule 5, Arc D): the
// automatic action an Error row waits on (owner ruling R-D2). The pure
// function runs against the table cmd/moombox's Go parity test runs
// internal/jobstate against; the DOM cases run under jsdom.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import {
  pendingAutoAction, pendingAutoActionText, reinitializeDeletesText, sumReinitializePreviews, isWaitLine, isAwaitingProcessing,
  queuedHold, queuedHoldText, diskGaugeLevel, cookieRetry, cookieRetryText,
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
  assert.match(pendingAutoActionText("auto_mux"), /Muxes automatically once Twitch confirms the broadcast over, while the channel is monitored/);
  assert.match(pendingAutoActionText("auto_retry"), /Retries automatically if the broadcast is live again, while the channel is monitored/);
});

// The Go twin is worker TestReinitializePreviewSummary's cancels cases.
//
// Mutants: the cancels clause dropped; the pending counts left out of the sum.
test("the Reinitialize clause names the automatic action it cancels", () => {
  assert.equal(reinitializeDeletesText({ stagedFiles: 1, stagedBytes: 10, pendingAutoMux: 1 }),
    "cancels the pending automatic mux, and deletes 1 staged file (10B)");
  assert.equal(reinitializeDeletesText({ pendingAutoRetry: 1 }),
    "cancels the pending automatic retry, and deletes nothing recorded");
  assert.equal(reinitializeDeletesText({ pendingAutoMux: 2, pendingAutoRetry: 3 }),
    "cancels 2 pending automatic muxes and 3 pending automatic retries, and deletes nothing recorded");
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
  assert.match(cell("tw_m").querySelector(".job-auto-action").title, /Muxes automatically once Twitch confirms the broadcast over, while the channel is monitored/);
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
  assert.match(block().textContent, /Muxes automatically once Twitch confirms the broadcast over, while the channel is monitored/);

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
  assert.match(asked ?? "", /This cancels the pending automatic mux, and deletes 2 staged files \(2\.0KB\)\./);
});

// Mutant: isWaitLine reading the marker alone (the closing paren unchecked),
// or a different marker.
test("isWaitLine answers the shared table as jobstate.IsWaitLine does", () => {
  assert.ok(PARITY.waitLines.length >= 8);
  for (const c of PARITY.waitLines) assert.equal(isWaitLine(c.line), c.want, JSON.stringify(c.line));
  assert.equal(isWaitLine(undefined), false);
});

// The Go twin is jobstate.AwaitingProcessing (TestAwaitingProcessingParity),
// which the TUI's details pane reads.
//
// Mutants: the channel check dropped (priority 1 alone); isVod ignored.
test("isAwaitingProcessing answers the shared table as jobstate does", () => {
  assert.ok(PARITY.awaitingProcessing.length >= 6);
  for (const c of PARITY.awaitingProcessing) {
    assert.equal(isAwaitingProcessing(c.job), c.want, c.name);
  }
  assert.equal(isAwaitingProcessing(null), false);
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

// The open job dialog of a Queued row follows the holds too, as the TUI's
// details pane does (JobDetailsModel.SetAdmissionHolds): an admission_holds
// frame rewrites its progress line without waiting for the job's next
// job_update.
//
// Mutant: setAdmissionHolds redrawing only the job-list cells.
test("an open Queued job's dialog follows the admission holds", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": () => ({ ...queued({ isVod: true }), hasStaging: false, hasSegments: false, asides: [], keptChatSidecar: false,
        unmuxedParts: false, muxGaps: [] }),
      "GET /api/jobs/:id/logs": () => [],
    },
  });
  const dlg = h.el("details-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [queued({ isVod: true })] } });
  await h.flush();
  h.app.details.showJobDetails(h.app.jobs[0]);
  await h.flush(); await h.flush();
  const progress = () => h.document.querySelector('#job-details-content [data-field="progress"]');
  assert.ok(progress(), "premise: the dialog has a progress row");
  assert.equal(progress().textContent, "Waiting in queue...");

  h.app.handleMessage({ type: "admission_holds", payload: { offline: false, diskHeld: true } });
  assert.equal(progress().textContent, "Held: disk full");
});

// A reconnect's initial_state carries the holds snapshot; the disk indicator
// redraws from it, as it does from an admission_holds frame or /api/status.
//
// Mutant: initial_state assigning admissionHolds without the redraw — the
// indicator keeps "holding" from before the disconnect.
test("a reconnect's initial_state redraws the disk indicator from its holds", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], admissionHolds: { offline: false, diskHeld: true } } });
  await h.flush();
  h.app.stats.updateDiskIndicator({ free: 12 * 2 ** 30, total: 200 * 2 ** 30, usedPct: 94, warnLevel: "warn" });
  const ind = h.document.getElementById("disk-indicator");
  assert.match(ind.textContent, /holding/);

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], admissionHolds: { offline: false, diskHeld: false } } });
  await h.flush();
  assert.doesNotMatch(ind.textContent, /holding/, "the snapshot's open gate redraws the indicator");
});

// A YouTube backlog VOD is created Queued without isVod (the backlog creator
// never sets it; stream processing writes it once it starts), so the dialog
// gates its progress row on the row's state, not on isVod alone: a Queued
// row and an Awaiting-processing row show it, and neither says "Type: Live".
//
// Mutant: the progress row gated on job.isVod alone.
test("a Queued backlog row's dialog shows its hold without isVod", { skip }, async () => {
  const row = queued();
  assert.equal(row.isVod, undefined, "premise: the backlog creator's row has no isVod");
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": () => ({ ...queued(), hasStaging: false, hasSegments: false, asides: [], keptChatSidecar: false,
        unmuxedParts: false, muxGaps: [] }),
      "GET /api/jobs/:id/logs": () => [],
    },
  });
  const dlg = h.el("details-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [row], admissionHolds: { offline: false, diskHeld: true } } });
  await h.flush();
  h.app.details.showJobDetails(h.app.jobs[0]);
  await h.flush(); await h.flush();
  const content = h.document.getElementById("job-details-content");
  const progress = () => content.querySelector('[data-field="progress"]');
  assert.ok(progress(), "the dialog has a progress row");
  assert.equal(progress().textContent, "Held: disk full");
  assert.doesNotMatch(content.textContent, /Type:\s*Live/);

  // Admitted: Upcoming, still no isVod — the dialog says Awaiting processing.
  h.app.handleMessage({ type: "job_update", payload: { ...row, status: "Upcoming" } });
  await h.flush(); await h.flush();
  assert.equal(progress()?.textContent, "Awaiting processing");
  assert.doesNotMatch(content.textContent, /Type:\s*Live/);
});

// A row's own hold (its retry backoff, a rate-limit requeue) runs out with no
// frame to say so while the slots stay full: the dashboard arms a one-shot
// timer for the earliest hold end and redraws the Queued cells then.
//
// Mutant: setAdmissionHolds without the expiry timer — the cell keeps
// "Retry 1 of 3 at HH:MM" after that time has passed.
test("a Queued row's own hold running out redraws its line", { skip }, async () => {
  const h = await harness.makeApp();
  const until = new Date(Date.now() + 5 * 60000).toISOString();
  h.app.jobs = [queued()];
  h.app.renderJobs();
  h.app.handleMessage({ type: "admission_holds", payload: { offline: false, diskHeld: false,
    jobs: { q1: { attempt: 1, of: 3, until } } } });
  const cell = () => h.document.querySelector('.video-item[data-job-id="q1"] .job-progress-text');
  assert.match(cell().textContent, /^Retry 1 of 3 at \d\d:\d\d$/);

  const frozenNow = Date.now;
  try {
    Date.now = () => frozenNow() + 4 * 60000;
    h.advance(4 * 60000);
    assert.match(cell().textContent, /^Retry 1 of 3/, "not yet");
    Date.now = () => frozenNow() + 5 * 60000 + 1000;
    h.advance(60000 + 1000);
    assert.equal(cell().textContent, "Waiting in queue...");
  } finally {
    Date.now = frozenNow;
  }
});

// Owner ruling R2-C1: the automatic cookie retries a parked job has used.
// Mutants: the status or membership check dropped; the window compared the
// wrong way round or changed on this side only.
test("cookieRetry answers the shared table as jobstate does", () => {
  assert.ok(PARITY.cookieRetries.length >= 8);
  for (const c of PARITY.cookieRetries) {
    assert.deepEqual(cookieRetry(c.holds, c.job, Date.parse(c.now)), c.want, c.name);
  }
  assert.deepEqual(cookieRetry(null, { id: "a", status: "COOKIES?" }), { count: 0 });
});

test("cookieRetryText words the count as the TUI does", () => {
  assert.equal(cookieRetryText({ count: 0 }), "");
  assert.equal(cookieRetryText({ count: 1, of: 3 }), "Cookie retry 1 of 3 used");
  assert.equal(cookieRetryText({ count: 3, of: 3 }), "Cookie retries used up (3 of 3): no more automatic retries");
});

const parked = (over = {}) => ({
  id: "c1", videoId: "c1", title: "Members", channelName: "C", platform: "youtube", status: "COOKIES?",
  parkReason: "auth", error: "Login required", createdAt: "2026-11-12T11:00:00Z", updatedAt: "2026-11-12T11:00:00Z", ...over,
});

// A COOKIES? row says how many automatic retries it has used, from the holds
// snapshot, and follows each admission_holds frame; a membership park keeps
// its own line.
//
// Mutants: formatProgress not reading the count; _redrawQueuedHolds skipping
// COOKIES? rows — the cell keeps "Needs cookie refresh" after the frame.
test("a COOKIES? row shows its cookie retries and follows the holds frame", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [parked(), parked({ id: "c2", videoId: "c2", parkReason: "membership" })] } });
  await h.flush();
  const cell = (id) => h.document.querySelector(`.video-item[data-job-id="${id}"] .job-progress-text`);
  assert.equal(cell("c1").textContent, "Needs cookie refresh");

  const last = new Date(Date.now() - 60_000).toISOString();
  h.app.handleMessage({ type: "admission_holds", payload: { cookieRetries: {
    c1: { count: 2, of: 3, last }, c2: { count: 2, of: 3, last } } } });
  await h.flush();
  assert.equal(cell("c1").textContent, "Cookie retry 2 of 3 used");
  assert.equal(cell("c2").textContent, "Not a member — needs member account");

  h.app.handleMessage({ type: "admission_holds", payload: { cookieRetries: { c1: { count: 3, of: 3, last } } } });
  await h.flush();
  assert.equal(cell("c1").textContent, "Cookie retries used up (3 of 3): no more automatic retries");
});
