// The status_changed frame (owner ruling R2-D1). Cookie badges, the re-login
// warning and the "sidecar down" alert were drawn only from the status read
// at page load or on a WebSocket reconnect, so a tab left open for days kept
// showing them as they were then. The server now sends a frame with no
// payload when any of them changes, and the tab reads GET /api/status again.
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

const warnings = (h) => h.el("status-warnings").textContent;
const reads = (h) => h.http.matching("/api/status", "GET").filter((c) => c.url === "/api/status").length;
const nudge = (h) => h.app.handleMessage({ type: "status_changed", payload: null });
const settle = async (h) => { for (let i = 0; i < 4; i++) await h.flush(); };

// Mutant: no status_changed case in handleMessage — the alert stays up after
// the sidecar recovered, and the re-login warning never appears.
test("status_changed re-reads the status: the sidecar alert clears and a re-login warning appears", { skip }, async () => {
  let status = {
    activePlatforms: { youtube: true, twitch: true },
    botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 },
  };
  const h = await harness.makeApp({ routes: { "GET /api/status": () => status } });
  assert.match(warnings(h), /PO tokens: sidecar down/);

  status = {
    botguardSidecar: { healthy: true, reason: "", restarts: 1 },
    activePlatforms: { youtube: true, twitch: true }, // the re-login warning is drawn for an active platform only
    autoCookieReloginRequired: { youtube: true, twitch: false },
  };
  nudge(h);
  await settle(h);
  assert.doesNotMatch(warnings(h), /sidecar down/, "the alert outlived the sidecar's recovery");
  assert.match(warnings(h), /YT: Re-login/, "the re-login flag raised after the page loaded is not shown");
});

/** A status route whose answers the test releases by hand, in any order. */
function heldStatus(first) {
  const pending = [];
  let booted = false;
  return {
    pending,
    route: () => {
      if (!booted) { booted = true; return first; }
      return new Promise((resolve) => pending.push(resolve));
    },
  };
}

// Mutant: one fetch per frame — a burst of auth changes becomes a burst of
// GET /api/status.
test("a burst of status_changed frames costs one read in flight and one more after it", { skip }, async () => {
  const held = heldStatus({});
  const h = await harness.makeApp({ routes: { "GET /api/status": held.route } });
  const before = reads(h);

  for (let i = 0; i < 5; i++) nudge(h);
  await settle(h);
  assert.equal(reads(h) - before, 1, "frames that arrive during a read must not start another");

  held.pending.shift()({});
  await settle(h);
  assert.equal(reads(h) - before, 2, "the frames that arrived during the read are owed one read after it");

  held.pending.shift()({});
  await settle(h);
  assert.equal(reads(h) - before, 2, "nothing arrived during the second read: no third");
  assert.equal(held.pending.length, 0);
});

// Two status reads can overlap (a frame's and a settings save's), and the
// one asked for first can answer last. Mutant: no generation check in
// loadStatus — the older answer is drawn over the newer.
test("a status answer that is no longer the newest read's is dropped", { skip }, async () => {
  const held = heldStatus({});
  const h = await harness.makeApp({ routes: { "GET /api/status": held.route } });

  const older = h.app.loadStatus();
  const newer = h.app.loadStatus();
  await settle(h);
  const [answerOlder, answerNewer] = held.pending;
  answerNewer({ botguardSidecar: { healthy: true, reason: "", restarts: 1 } });
  await newer;
  answerOlder({ botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 } });
  await older;
  assert.doesNotMatch(warnings(h), /sidecar down/, "the late answer of the older read was drawn");
});

// The frame's read takes only what the frame is about. Mutant: the frame
// running loadStatus — its snapshot, fetched before an admission_holds frame
// arrived, is written over that frame's newer holds.
test("a status_changed read does not write over state that has frames of its own", { skip }, async () => {
  const held = heldStatus({});
  const h = await harness.makeApp({ routes: { "GET /api/status": held.route } });
  nudge(h);
  await settle(h);
  const newer = { diskCritical: true };
  h.app.handleMessage({ type: "admission_holds", payload: newer });
  held.pending.shift()({ admissionHolds: { diskCritical: false }, botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 }, activePlatforms: { youtube: true } });
  await settle(h);
  assert.match(warnings(h), /sidecar down/, "the frame's read was not applied at all");
  assert.equal(h.app.admissionHolds, newer, "the read's older snapshot replaced the frame's holds");
});

// Mutant: Recheck's answer not taking a number — the frame's read, asked for
// before the recheck and answered after it, draws its older verdict over the
// recheck's. The recheck says nothing about the sidecar, so that half of the
// read is still applied. Mutant: one number for both halves — the sidecar
// alert the frame was sent for never appears, and no second frame follows.
test("a status read in flight does not draw over a recheck that answered meanwhile, and keeps its sidecar half", { skip }, async () => {
  const held = heldStatus({});
  const h = await harness.makeApp({
    routes: {
      "GET /api/status": held.route,
      "POST /api/cookies/recheck": () => ({
        success: true,
        activePlatforms: { youtube: true },
        cookieStatus: { checked: true, isAuthenticated: true },
      }),
    },
  });
  nudge(h);
  await settle(h);
  await h.app.recheckCookies();
  const fromRecheck = h.app.cookieStatus;
  held.pending.shift()({
    activePlatforms: { youtube: true },
    cookieStatus: { checked: true, isAuthenticated: false },
    botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 },
  });
  await settle(h);
  assert.equal(h.app.cookieStatus, fromRecheck);
  assert.match(warnings(h), /sidecar down/, "the read's sidecar half was dropped with its cookie half");
});

// Mutant: the sidecar half applied with no number at all — a frame's read,
// asked for before a full status load and answered after it, draws its older
// health over the newer.
test("a frame's read that answers after a newer status load does not draw its older sidecar health", { skip }, async () => {
  const held = heldStatus({});
  const h = await harness.makeApp({ routes: { "GET /api/status": held.route } });
  nudge(h);
  await settle(h);
  const newer = h.app.loadStatus();
  await settle(h);
  const [answerFrame, answerLoad] = held.pending;
  answerLoad({ botguardSidecar: { healthy: true, reason: "", restarts: 1 } });
  await newer;
  answerFrame({ botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 } });
  await settle(h);
  assert.doesNotMatch(warnings(h), /sidecar down/);
});

// Mutant: no deadline — a read that never settles holds the busy latch, and
// every later frame is only queued behind it for the life of the tab.
test("a status read that never answers is given up, and later frames read again", { skip }, async () => {
  const aborted = [];
  const h = await harness.makeApp({ routes: { "GET /api/status": () => ({}) } });
  const realFetch = h.window.fetch;
  // The harness's fetch ignores an abort signal; a browser's rejects on it.
  const hang = (url, opts) => new Promise((_, reject) => {
    opts.signal.addEventListener("abort", () => { aborted.push(url); reject(new DOMException("aborted", "AbortError")); });
  });
  globalThis.fetch = h.window.fetch = hang;
  nudge(h);
  await settle(h);
  h.advance(15000);
  await settle(h);
  assert.equal(aborted.length, 1, "the hung read was not given up");

  globalThis.fetch = h.window.fetch = realFetch;
  const before = reads(h);
  nudge(h);
  await settle(h);
  assert.equal(reads(h) - before, 1, "the frame after a given-up read did not read");
});

// Mutant: the toast on every status load — each cookie or reconnect read
// while Settings holds unsaved edits stacks another copy.
test("the reload-after-saving toast is shown once per version", { skip }, async () => {
  let version = "1.0.0";
  const h = await harness.makeApp({ routes: { "GET /api/status": () => ({ version }) } });
  h.app.settings.isDirty = () => true;
  version = "1.0.1";
  const toasts = () => [...h.document.querySelectorAll("sl-alert")].filter((a) => /was updated to v1.0.1/.test(a.textContent)).length;
  await h.app.loadStatus();
  await h.app.loadStatus();
  await settle(h);
  assert.equal(toasts(), 1);
});
