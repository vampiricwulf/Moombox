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
