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
