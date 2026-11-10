// The cookie-setup dialog the first-run wizard and Settings share (K33),
// owned by one CookieSetupSession (modules/cookie-setup-session.js):
//
//   - its handlers are bound once, so the wizard's and Settings' never both
//     fire (the Done button used to POST /finish twice after an in-page
//     wizard, on the status-driven reopen that did not re-clone);
//   - the platform is stored, not parsed from the label — after a reload
//     during a Twitch setup the dialog reopened as "Cookie Login" and the
//     finish was read as YouTube's;
//   - one phase field, and the unload beacon fires only for a setup this page
//     started.
//
// jsdom is optional, as in app.test.mjs: every test skips without it.
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
const utils = jsdomMissing ? null : await import("../public/modules/utils.js");
const session = jsdomMissing ? null : await import("../public/modules/cookie-setup-session.js");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

// Twitch's and YouTube's verdicts are made to differ, so the copy on screen
// says whose verdict was read.
const rejectedFinish = {
  success: true,
  authenticated: false,
  twitchAuthenticated: false,
  youtubeVerification: "failed",
  twitchVerification: "unknown",
};

async function app(routes = {}) {
  const h = await harness.makeApp({
    routes: {
      "POST /api/cookies/auto-setup/start": () => ({ success: true }),
      "POST /api/cookies/auto-setup/finish": () => rejectedFinish,
      "POST /api/cookies/auto-setup/cancel": () => ({ success: true }),
      ...routes,
    },
  });
  const beacons = [];
  h.window.navigator.sendBeacon = (url) => { beacons.push(url); return true; };
  h.beacons = beacons;
  return h;
}

const finishes = (h) => h.http.matching("/api/cookies/auto-setup/finish", "POST").length;
const result = (h) => h.el("auto-cookie-setup-result").textContent;
const twitchRejected = () => utils.cookieSetupRejectedMessage("unknown");
const youtubeRejected = () => utils.cookieSetupRejectedMessage("failed");

test("a Twitch setup started from Settings is finished as Twitch's", { skip }, async () => {
  const h = await app();
  await h.app.settings.startAutoCookieSetup("twitch");
  const s = h.app.cookieSetup;
  assert.equal(s.phase, session.PHASE.ACTIVE);
  assert.equal(s.platform, "twitch");
  assert.equal(h.el("auto-cookie-setup-dialog").label, "Setup: Twitch");
  assert.ok(h.el("auto-cookie-setup-dialog")._open);
  assert.ok(s.holdsSlot());

  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(finishes(h), 1);
  assert.equal(result(h), twitchRejected());
  assert.equal(s.phase, session.PHASE.REJECTED);
  assert.equal(s.holdsSlot(), false, "an answered finish released the slot");
});

// MUTANT: adopt() leaving the platform to a label parse — the reopened
// dialog's "Cookie Login" reads as YouTube and the YouTube copy is shown.
test("a setup adopted after a reload keeps the server's platform", { skip }, async () => {
  const h = await app({
    "GET /api/cookies/auto-status": () => ({
      setupInProgress: true, setupPlatform: "twitch", availableBrowsers: [],
      lastRefresh: null, lastError: null, needsManualRelogin: {},
    }),
  });
  await h.app.settings.loadAutoCookieStatus();
  const s = h.app.cookieSetup;
  assert.equal(s.phase, session.PHASE.ADOPTED);
  assert.equal(s.platform, "twitch");
  assert.equal(h.el("auto-cookie-setup-dialog").label, "Setup: Twitch");
  assert.ok(h.el("auto-cookie-setup-dialog")._open);
  assert.equal(s.holdsSlot(), false, "a setup this page did not start is not its to abandon");

  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(result(h), twitchRejected(), "the Twitch setup was reported as YouTube's");
  assert.notEqual(twitchRejected(), youtubeRejected());
});

test("an adopted setup the server names no platform for says neither platform", { skip }, async () => {
  const h = await app({
    "GET /api/cookies/auto-status": () => ({ setupInProgress: true, availableBrowsers: [], needsManualRelogin: {} }),
  });
  await h.app.settings.loadAutoCookieStatus();
  assert.equal(h.app.cookieSetup.platform, null);
  assert.equal(h.el("auto-cookie-setup-dialog").label, "Cookie Login");
});

// The handlers are bound once. After an in-page wizard ran a setup, the
// Done button carried the wizard's handler and Settings added its own, so a
// status-driven reopen POSTed /finish twice.
//
// MUTANT: SettingsController.setupListeners binding its own Done handler
// again — two finishes.
test("Done after the wizard and Settings both bound sends one finish", { skip }, async () => {
  const h = await app({
    "GET /api/cookies/auto-status": () => ({ setupInProgress: true, setupPlatform: "youtube", availableBrowsers: [], needsManualRelogin: {} }),
  });
  h.app.setup.setupListeners();
  await h.app.setup.startCookieSetup("youtube");
  h.app.settings.setupListeners?.();
  await h.app.settings.loadAutoCookieStatus();
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(finishes(h), 1);
});

test("the wizard's accepted setup lights its badges; Settings reloads status", { skip }, async () => {
  const accepted = { success: true, authenticated: false, twitchAuthenticated: true, twitchVerification: "ok", youtubeVerification: "failed" };
  const h = await app({ "POST /api/cookies/auto-setup/finish": () => accepted });
  await h.app.setup.startCookieSetup("twitch");
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(h.app.setup.cookieTWDone, true);
  assert.equal(h.el("setup-tw-badge").variant, "success");
  assert.equal(h.app.cookieSetup.phase, session.PHASE.IDLE);
  assert.equal(h.el("auto-cookie-setup-dialog")._open, false);

  const statusLoads = h.http.matching("/api/status", "GET").length;
  await h.app.settings.startAutoCookieSetup("twitch");
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(h.app.setup.cookieTWDone, true);
  assert.ok(h.http.matching("/api/status", "GET").length > statusLoads, "Settings' accepted setup did not reload the status");
});

// Escape (sl-request-close) and Cancel both go through the server's cancel.
test("Escape cancels the setup on the server and hides the dialog", { skip }, async () => {
  const h = await app();
  await h.app.settings.startAutoCookieSetup("youtube");
  const dlg = h.el("auto-cookie-setup-dialog");
  const ev = new h.window.CustomEvent("sl-request-close", { cancelable: true });
  dlg.dispatchEvent(ev);
  await h.flush();
  assert.equal(ev.defaultPrevented, true);
  assert.equal(h.http.matching("/api/cookies/auto-setup/cancel", "POST").length, 1);
  assert.equal(dlg._open, false);
  assert.equal(h.app.cookieSetup.phase, session.PHASE.IDLE);
});

// The beacon: /abandon (never /cancel), only for a setup this page started,
// never for a page going into the back/forward cache, and once.
test("pagehide abandons only a setup this page started", { skip }, async () => {
  const h = await app({
    "GET /api/cookies/auto-status": () => ({ setupInProgress: true, setupPlatform: "twitch", availableBrowsers: [], needsManualRelogin: {} }),
  });
  const pagehide = (persisted) => {
    const e = new h.window.Event("pagehide");
    e.persisted = persisted;
    h.window.dispatchEvent(e);
  };

  await h.app.settings.loadAutoCookieStatus(); // adopted
  pagehide(false);
  assert.deepEqual(h.beacons, [], "an adopted setup was abandoned");

  await h.app.cookieSetup.cancel();
  await h.app.settings.startAutoCookieSetup("twitch");
  pagehide(true);
  assert.deepEqual(h.beacons, [], "a bfcache pagehide abandoned a setup that is coming back");
  pagehide(false);
  pagehide(false);
  assert.deepEqual(h.beacons, ["/api/cookies/auto-setup/abandon"]);
});

// ── a Cancel while the finish is out (fix round) ───────────────────────
//
// Cancel (or Escape) during a finish set the phase idle, and the finish's
// late answer then moved it out again — to rejected, or to aborted with the
// page still "holding" the slot — so a tab close later abandoned a setup the
// user had cancelled, and adopt() took another tab's setup for this one and
// kept the cancelled setup's platform. A finish now acts only for the setup
// it was started in.

const youtubeStatus = () => ({
  setupInProgress: true, setupPlatform: "youtube", availableBrowsers: [],
  lastRefresh: null, lastError: null, needsManualRelogin: {},
});

async function cancelledDuringFinish(settle) {
  const gate = {};
  let status = { setupInProgress: false, availableBrowsers: [], needsManualRelogin: {} };
  const h = await app({
    "POST /api/cookies/auto-setup/finish": () => new Promise((resolve, reject) => { gate.resolve = resolve; gate.reject = reject; }),
    "GET /api/cookies/auto-status": () => status,
  });
  await h.app.settings.startAutoCookieSetup("twitch");
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(h.app.cookieSetup.phase, session.PHASE.FINISHING);
  h.el("btn-auto-cookie-cancel").click();
  await h.flush();
  settle(gate);
  await h.flush();
  await h.flush();
  h.setStatus = (s) => { status = s; };
  return h;
}

// MUTANT: finish() writing REJECTED after its await without checking the
// setup it was started for — the phase leaves idle and adopt() keeps Twitch.
test("a finish answering after a Cancel leaves the session idle", { skip }, async () => {
  const h = await cancelledDuringFinish((g) => g.resolve(rejectedFinish));
  const s = h.app.cookieSetup;
  assert.equal(s.phase, session.PHASE.IDLE, "the late finish moved a cancelled session out of idle");
  assert.equal(s.holdsSlot(), false);
  assert.equal(result(h), "", "the cancelled setup's rejection was drawn");
  assert.equal(h.el("auto-cookie-setup-dialog")._open, false);

  h.setStatus(youtubeStatus());
  await h.app.settings.loadAutoCookieStatus();
  assert.equal(s.phase, session.PHASE.ADOPTED);
  assert.equal(s.platform, "youtube", "another tab's setup took the cancelled setup's platform");
  assert.equal(h.el("auto-cookie-setup-dialog").label, "Setup: YouTube");
});

// MUTANT: the abort branch writing ABORTED without the check — holdsSlot()
// turns true again and a tab close abandons the cancelled setup.
test("a finish aborting after a Cancel neither holds the slot nor draws Try Again", { skip }, async () => {
  const h = await cancelledDuringFinish((g) => g.reject(Object.assign(new Error("aborted"), { name: "AbortError" })));
  const s = h.app.cookieSetup;
  assert.equal(s.phase, session.PHASE.IDLE);
  assert.equal(s.holdsSlot(), false, "a cancelled setup holds the slot again");
  assert.equal(h.el("cookie-retry-btn"), null, "the cancelled setup's abort report was drawn");
  const e = new h.window.Event("pagehide");
  e.persisted = false;
  h.window.dispatchEvent(e);
  assert.deepEqual(h.beacons, [], "a tab close abandoned a setup the user cancelled");
});

// REJECTED frees the slot, so a setup the server reports later is someone
// else's (another tab, the TUI) and is adopted with its own platform.
//
// MUTANT: adopt() treating REJECTED as "ours already".
test("a rejected session adopts the next setup with the server's platform", { skip }, async () => {
  let status = { setupInProgress: false, availableBrowsers: [], needsManualRelogin: {} };
  const h = await app({ "GET /api/cookies/auto-status": () => status });
  await h.app.settings.startAutoCookieSetup("youtube");
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  const s = h.app.cookieSetup;
  assert.equal(s.phase, session.PHASE.REJECTED);

  status = { ...youtubeStatus(), setupPlatform: "twitch" };
  await h.app.settings.loadAutoCookieStatus();
  assert.equal(s.phase, session.PHASE.ADOPTED);
  assert.equal(s.platform, "twitch");
  assert.equal(h.el("auto-cookie-setup-dialog").label, "Setup: Twitch");
  assert.equal(s.holdsSlot(), false);
  assert.equal(result(h), "", "the earlier setup's rejection stayed on the adopted setup");
});

// ── close round ────────────────────────────────────────────────────────
//
// A finish cancelled while out, then a new setup whose own finish is running:
// the old finish's finally reset the Done button and blanked the countdown
// under the new FINISHING, so Done could be clicked again mid-finish.
//
// MUTANT: finally writing the button and countdown without the stale check.
test("a stale finish settling leaves the next setup's finish UI alone", { skip }, async () => {
  const gates = [];
  const h = await app({
    "POST /api/cookies/auto-setup/finish": () => new Promise((resolve) => { gates.push(resolve); }),
  });
  const s = h.app.cookieSetup;
  await h.app.settings.startAutoCookieSetup("twitch");
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  h.el("btn-auto-cookie-cancel").click();
  await h.flush();
  assert.equal(s.phase, session.PHASE.IDLE);

  await h.app.settings.startAutoCookieSetup("youtube");
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(s.phase, session.PHASE.FINISHING);
  assert.equal(gates.length, 2);
  const countdown = h.el("auto-cookie-countdown").textContent;
  assert.notEqual(countdown, "");

  gates[0](rejectedFinish);
  await h.flush();
  await h.flush();
  assert.equal(s.phase, session.PHASE.FINISHING);
  assert.equal(h.el("btn-auto-cookie-done").loading, true, "the stale finish reset the running finish's Done button");
  assert.equal(h.el("btn-auto-cookie-done").disabled, true);
  assert.equal(h.el("auto-cookie-countdown").textContent, countdown, "the stale finish blanked the running finish's countdown");

  gates[1](rejectedFinish);
  await h.flush();
  await h.flush();
  assert.equal(s.phase, session.PHASE.REJECTED);
  assert.equal(h.el("btn-auto-cookie-done").loading, false);
  assert.equal(h.el("auto-cookie-countdown").textContent, "");
});

// An adopted setup follows the server: if the setup the server reports now
// runs on another platform (it ended and another tab or the TUI started a new
// one between two status loads), the session re-adopts it rather than
// finishing it as the old platform's.
//
// MUTANT: adopt() keeping "ours already" for an ADOPTED session.
test("an adopted setup takes the platform the server changed under it", { skip }, async () => {
  let status = youtubeStatus();
  const h = await app({ "GET /api/cookies/auto-status": () => status });
  await h.app.settings.loadAutoCookieStatus();
  const s = h.app.cookieSetup;
  assert.equal(s.phase, session.PHASE.ADOPTED);
  assert.equal(s.platform, "youtube");
  h.el("auto-cookie-setup-result").textContent = "left over";

  status = { ...youtubeStatus(), setupPlatform: "twitch" };
  await h.app.settings.loadAutoCookieStatus();
  assert.equal(s.phase, session.PHASE.ADOPTED);
  assert.equal(s.platform, "twitch", "the adopted setup kept the platform the server changed");
  assert.equal(h.el("auto-cookie-setup-dialog").label, "Setup: Twitch");
  assert.equal(result(h), "", "the old setup's messages stayed");

  // An unnamed platform is no change.
  status = { setupInProgress: true, availableBrowsers: [], needsManualRelogin: {} };
  await h.app.settings.loadAutoCookieStatus();
  assert.equal(s.platform, "twitch");
});

// Only an ADOPTED session re-adopts: one this page started keeps its own
// platform whatever the status says.
test("a setup this page started keeps its platform on a status load", { skip }, async () => {
  let status = { setupInProgress: false, availableBrowsers: [], needsManualRelogin: {} };
  const h = await app({ "GET /api/cookies/auto-status": () => status });
  await h.app.settings.startAutoCookieSetup("youtube");
  status = { ...youtubeStatus(), setupPlatform: "twitch" };
  await h.app.settings.loadAutoCookieStatus();
  const s = h.app.cookieSetup;
  assert.equal(s.phase, session.PHASE.ACTIVE);
  assert.equal(s.platform, "youtube");
  assert.ok(s.holdsSlot());
});

// ── close round 2 ──────────────────────────────────────────────────────
//
// A finish cancelled while out leaves the Done button loading and disabled;
// the stale check above keeps that finish's finally off it, so whatever ends
// a setup and starts the next (begin, adopt, cancel — each through
// _clearMessages) resets it. Without that, the next setup opened with a dead
// "I'm Logged In" until a reload, whichever of the two came first: the old
// finish settling or the next setup beginning.
//
// MUTANT: _clearMessages leaving the Done button alone.
async function finishCancelledWhileOut() {
  const gates = [];
  const h = await app({
    "POST /api/cookies/auto-setup/finish": () => new Promise((resolve) => { gates.push(resolve); }),
  });
  await h.app.settings.startAutoCookieSetup("twitch");
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(h.app.cookieSetup.phase, session.PHASE.FINISHING);
  h.el("btn-auto-cookie-cancel").click();
  await h.flush();
  return { h, gates };
}

function assertDoneUsable(h) {
  const done = h.el("btn-auto-cookie-done");
  assert.equal(done.loading, false, "Done still loading from the cancelled finish");
  assert.equal(done.disabled, false, "Done still disabled from the cancelled finish");
}

test("the next setup's Done works when the cancelled finish settles first", { skip }, async () => {
  const { h, gates } = await finishCancelledWhileOut();
  gates[0](rejectedFinish);
  await h.flush();
  await h.flush();
  await h.app.settings.startAutoCookieSetup("youtube");
  assert.equal(h.app.cookieSetup.phase, session.PHASE.ACTIVE);
  assertDoneUsable(h);
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(finishes(h), 2, "Done on the next setup sent no finish");
});

test("the next setup's Done works when it begins before the cancelled finish settles", { skip }, async () => {
  const { h, gates } = await finishCancelledWhileOut();
  await h.app.settings.startAutoCookieSetup("youtube");
  gates[0](rejectedFinish);
  await h.flush();
  await h.flush();
  assert.equal(h.app.cookieSetup.phase, session.PHASE.ACTIVE);
  assertDoneUsable(h);
  h.el("btn-auto-cookie-done").click();
  await h.flush();
  assert.equal(finishes(h), 2, "Done on the next setup sent no finish");
});

// The cancelled finish's countdown interval keeps running until that finish
// settles; its ticks must not draw over the next setup's dialog.
//
// MUTANT: the interval callback writing the countdown without the stale check.
test("a cancelled finish's countdown does not tick into the next setup", { skip }, async () => {
  const { h } = await finishCancelledWhileOut();
  await h.app.settings.startAutoCookieSetup("youtube");
  assert.equal(h.app.cookieSetup.phase, session.PHASE.ACTIVE);
  assert.equal(h.el("auto-cookie-countdown").textContent, "");
  h.advance(1000);
  assert.equal(h.el("auto-cookie-countdown").textContent, "", "the cancelled finish's countdown drew on the next setup");
});
