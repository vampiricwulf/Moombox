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
