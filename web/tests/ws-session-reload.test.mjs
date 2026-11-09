// K31, end to end through app.js's own fetch wrapper: a socket refused before
// it opened sends its session probe through the wrapped fetch, and the
// probe's 401 is what reloads the page to the login screen. app.js wraps
// window.fetch once per process, on the FIRST makeApp's window (see
// helpers/app-dom.mjs), so this file holds exactly one test: a second one
// would reach the plain fake and prove nothing about the reload.
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

// MUTANTS: _probeSession calling an unwrapped fetch, or the wrapper exempting
// /api/status — either shows "Session expired" and never reloads.
test("the probe's 401 goes through the fetch wrapper and reloads to the login page", { skip }, async () => {
  let expired = false;
  const h = await harness.makeApp({
    routes: {
      "GET /api/status": () => (expired ? harness.response({ status: 401, body: { error: "unauthorized" } }) : {}),
    },
  });
  assert.equal(h.navigations.length, 0, "the boot reloaded");

  expired = true;
  h.sockets[h.sockets.length - 1].onclose();
  await h.flush();
  await h.flush();

  assert.equal(h.app._connState, "unauthorized");
  assert.equal(h.navigations.length, 1, "the probe's 401 did not reload the page");

  // A reload the operator cancelled (Stay on the unsaved-settings prompt)
  // must not leave the tab stuck: the slow re-probe keeps going, reloads no
  // second time, and a later 200 (a login made in another tab) reconnects.
  const sockets = h.sockets.length;
  h.advance(30000);
  await h.flush();
  await h.flush();
  assert.equal(h.app._connState, "unauthorized");
  assert.equal(h.navigations.length, 1, "a re-probe reloaded again");
  assert.equal(h.sockets.length, sockets);

  expired = false;
  h.advance(30000);
  await h.flush();
  await h.flush();
  assert.equal(h.sockets.length, sockets + 1, "a restored session did not reconnect");
  assert.equal(h.app._connState, "connecting");
});
