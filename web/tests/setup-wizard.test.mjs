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

// The wizard at `url`, its setup/complete answering `complete` (applied by
// default; the restart that follows is never reached, the clock being manual).
async function wizardAt(url, complete = () => ({ success: true })) {
  return harness.makeApp({
    url,
    initialState: firstRun,
    routes: { "POST /api/setup/complete": complete },
  });
}

// The address the tab is being sent to after an applied setup, or null when
// it waits for the restart at the address it is on (pollForRestart).
function redirectShown(h) {
  const waiting = h.document.getElementById("restart-waiting");
  assert.ok(waiting, "no restart screen: the setup was not applied");
  if (!waiting.textContent.includes("Dashboard address has changed")) return null;
  return waiting.querySelector("p[style*='monospace']")?.textContent ?? waiting.textContent;
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
  assert.equal(redirectShown(h), null);
});

// Toggling HTTPS with a blank port still moves the address — to the port the
// page is served from, under the new scheme.
//
// Mutant killed: `port || 774` as the new port (https://localhost:774).
test("HTTPS toggled with a blank port redirects to the serving port", { skip }, async () => {
  const h = await wizardAt("http://localhost:775/");
  h.el("setup-https-enabled").checked = true;
  await h.app.setup.finishAdvancedSetup();
  assert.equal(redirectShown(h), "https://localhost:775");
});

// A typed port that differs from the page's still redirects there.
//
// Mutant killed: ignoring the typed port (`newPort = currentPort`, no
// redirect).
test("a typed port other than the page's redirects to it", { skip }, async () => {
  const h = await wizardAt("http://localhost:775/");
  h.el("setup-port").value = "8080";
  await h.app.setup.finishAdvancedSetup();
  assert.equal(redirectShown(h), "http://localhost:8080");
});

// W27 review of W26-12: the redirect is kept only once the server applied the
// setup. An Advanced Finish with port 8080 that the server refused (a 400 for
// an output directory that cannot be created) left it set, and a Use Defaults
// afterwards — which keeps the port — sent the tab to :8080 while the server
// restarted on the port it was on. Quick setup is the same path.
//
// Mutants killed: the code before — the redirect set before the request and
// kept by submitSetup whatever the answer (the refused Finish's :8080 reaches
// Use Defaults' restart screen); dropping it from submitSetup's success (the
// applied Advanced Finishes above do not redirect). Setting it before the
// request alone is benign: the success overwrites it either way.
test("a refused Advanced Finish leaves no redirect for Use Defaults or Quick setup", { skip }, async () => {
  for (const finish of ["finishWithDefaults", "finishSimpleSetup"]) {
    let calls = 0;
    const h = await wizardAt("http://localhost:774/", () => {
      calls++;
      return calls === 1
        ? harness.response({ status: 400, body: { error: "Validation failed", details: { "paths.output_directory": "could not create" } } })
        : { success: true };
    });
    h.el("setup-port").value = "8080";
    await h.app.setup.finishAdvancedSetup();
    assert.equal(h.document.getElementById("restart-waiting"), null, "the refused Finish restarted");
    await h.app.setup[finish]();
    assert.equal(calls, 2);
    assert.equal(redirectShown(h), null, `${finish} after a refused Advanced Finish redirects`);
  }
});
