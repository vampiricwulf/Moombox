// The dashboard half of YOUTUBE-5's user-visible signal: when /api/status says
// the BotGuard sidecar is down, the header warning list says so. It rides the
// EXISTING #status-warnings list (the same one the re-login prompts use), so
// there is no new markup — only a new item and the action-less shape that item
// introduces.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = "jsdom not installed - run npm ci in web/tests (" + e.code + ")";
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

function warningLabels(document) {
  return [...document.getElementById("status-warnings").children].map((el) => el.textContent);
}

test("a dead BotGuard sidecar raises a header warning", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "GET /api/status": { version: "test", botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 } },
    },
  });

  await h.app.loadStatus();

  // Mutant: the warningItems push removed -> the list is empty.
  assert.deepEqual(warningLabels(h.document), ["PO tokens: sidecar down"]);

  // Mutant: span.dataset.action = w.action left unguarded -> "undefined".
  const span = h.document.getElementById("status-warnings").firstElementChild;
  assert.equal(span.dataset.action, undefined, "a sidecar warning is not clickable and must carry no data-action");
  assert.match(span.title, /BotGuard sidecar/);

  // Mutant: the icon's dataset.action left unguarded -> "undefined" there too.
  const icon = h.document.getElementById("status-warnings-icon");
  assert.ok(icon.classList.contains("active"), "the mobile warning icon stayed inactive");
  assert.equal(icon.dataset.action, undefined);
});

test("a healthy sidecar raises nothing, and so does a payload without the key", { skip }, async () => {
  const healthy = await harness.makeApp({
    routes: { "GET /api/status": { version: "test", botguardSidecar: { healthy: true, reason: "", restarts: 1 } } },
  });
  await healthy.app.loadStatus();
  // Mutant: treating any present botguardSidecar as an alarm -> one item.
  assert.deepEqual(warningLabels(healthy.document), []);

  const absent = await harness.makeApp({ routes: { "GET /api/status": { version: "test" } } });
  await absent.app.loadStatus();
  // Mutant: !status.botguardSidecar?.healthy (true when the key is absent)
  // -> a sidecar-disabled install shows a permanent warning.
  assert.deepEqual(warningLabels(absent.document), []);
});

test("the warning clears on recovery, on the same dashboard", { skip }, async () => {
  // One app instance on purpose. The two tests above each build their own, so
  // between them they never exercise a TRANSITION — and a sticky
  // implementation (`if (this.sidecarHealthy !== false) this.sidecarHealthy = …`)
  // passes both while leaving a recovered install showing a permanent alert.
  // Recovery is the half the spec names explicitly: the supervisor's restart
  // is only visible as the warning going away.
  const h = await harness.makeApp({
    routes: {
      "GET /api/status": { version: "test", botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 } },
    },
  });

  await h.app.loadStatus();
  assert.deepEqual(warningLabels(h.document), ["PO tokens: sidecar down"]);

  // The supervisor got it back: same dashboard, same route, new payload.
  h.http.on("GET /api/status", { version: "test", botguardSidecar: { healthy: true, reason: "", restarts: 1 } });
  await h.app.loadStatus();

  // Mutant: sidecarHealthy assigned only while it is not already false
  // -> the label is still there after the restart succeeded.
  assert.deepEqual(warningLabels(h.document), []);

  // And the mobile icon goes with it — its own branch, its own mutant
  // (the `else` that clears title/dataset dropped).
  const icon = h.document.getElementById("status-warnings-icon");
  assert.ok(!icon.classList.contains("active"), "the mobile warning icon stayed active after recovery");
  assert.equal(icon.title, "");
});
