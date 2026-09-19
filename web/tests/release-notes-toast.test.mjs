// The View Release Notes button was the last alert() in web/public (WEB-15).
// A blocking alert() is a browser dialog: it freezes the tab, carries the
// origin in its title bar and looks like a page fault rather than a Moombox
// message, while every other failure in this dashboard toasts.
//
// This drives the REAL button through the REAL listener, so it also pins that
// SettingsController.setupListeners still wires it.
//
// Like app.test.mjs this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every test is
// skipped (not failed) when jsdom is absent. Only an absent module is a skip;
// any other import failure must fail loudly.
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

// MUTANT: put `alert("Failed to fetch release notes: " + err)` back in
// settings.js — the spy below fires and no toast is drawn, so both assertions
// fail. (The Go twin, TestNoAlertInTheWebUI in internal/web/routes, finds a
// restored alert anywhere under web/public; this one pins what replaced it.)
test("a failed release-notes fetch toasts instead of blocking the tab", { skip }, async () => {
  const h = await harness.makeApp();

  // settings.js resolves bare globals off globalThis, the same way app.js
  // does; publishing a spy here means a restored alert() records rather than
  // throwing ReferenceError, so the assertion below names the real defect.
  const alerted = [];
  const priorAlert = Object.getOwnPropertyDescriptor(globalThis, "alert");
  Object.defineProperty(globalThis, "alert", {
    configurable: true, writable: true, value: (msg) => alerted.push(String(msg)),
  });
  try {
    // No route is registered for GET /api/update/release-notes, so the
    // harness answers 404 — the !resp.ok branch under test.
    h.el("btn-view-release-notes").click();
    await h.flush();
  } finally {
    if (priorAlert) Object.defineProperty(globalThis, "alert", priorAlert);
    else delete globalThis.alert;
  }

  assert.deepEqual(alerted, [], "the release-notes failure must not open a browser alert");

  const texts = h.toasts().map((t) => t.textContent);
  assert.ok(
    texts.some((t) => t.includes("Failed to fetch release notes")),
    `the failure must toast; toasts were ${JSON.stringify(texts)}`,
  );
});
