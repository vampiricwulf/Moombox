// The first-run wizard's Finish (modules/setup.js finishAdvancedSetup): the
// address it sends the tab to after the restart.
//
// Like app.test.mjs, this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every test is
// skipped (not failed) when jsdom is absent. Only an absent module is a skip.
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

const firstRun = { setup: { isFirstRun: true, ffmpegValid: true } };

// The wizard at `url`, its setup/complete answering `complete` (a refusal by
// default, so a test stops at the request instead of polling for a restart).
async function wizardAt(url, complete = () => harness.response({ status: 400, body: { error: "stop here" } })) {
  return harness.makeApp({
    url,
    initialState: firstRun,
    routes: { "POST /api/setup/complete": complete },
  });
}

// W26-12: the server serves from a nearby port when 774 is taken
// (internal/web/server.go) and binds the same one after the restart. A blank
// port field keeps the server's port, yet the wizard compared 774 with the
// page's port and sent the tab to http://localhost:774 — whatever held it.
//
// Mutant killed: `port || 774` as the new port (redirects to :774).
test("a blank port on a page served from a fallback port does not redirect", { skip }, async () => {
  const h = await wizardAt("http://localhost:775/");
  await h.app.setup.finishAdvancedSetup();
  assert.equal(h.app.setup._redirectUrl, null);
});

// Toggling HTTPS with a blank port still moves the address — to the port the
// page is served from, under the new scheme.
//
// Mutant killed: `port || 774` as the new port (https://localhost:774).
test("HTTPS toggled with a blank port redirects to the serving port", { skip }, async () => {
  const h = await wizardAt("http://localhost:775/");
  h.el("setup-https-enabled").checked = true;
  await h.app.setup.finishAdvancedSetup();
  assert.equal(h.app.setup._redirectUrl, "https://localhost:775");
});

// A typed port that differs from the page's still redirects there.
//
// Mutant killed: ignoring the typed port (`newPort = currentPort`, no
// redirect).
test("a typed port other than the page's redirects to it", { skip }, async () => {
  const h = await wizardAt("http://localhost:775/");
  h.el("setup-port").value = "8080";
  await h.app.setup.finishAdvancedSetup();
  assert.equal(h.app.setup._redirectUrl, "http://localhost:8080");
});
