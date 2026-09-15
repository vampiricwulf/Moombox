// LogPanelController's append path must cost ONE DOM write per animation
// frame, not one per line: the old fast path read viewer.scrollHeight per line,
// forcing a synchronous layout each time (sweep T2-21).
//
// Needs jsdom (the controller queries the real markup); probed first so
// `node --test web/tests/*.test.mjs` stays green without it.
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

/** Count appendChild calls on one element, and the scrollTop writes it takes. */
function spyOnViewer(viewer) {
  const spy = { appends: 0, scrollWrites: 0 };
  const realAppend = viewer.appendChild.bind(viewer);
  viewer.appendChild = (node) => { spy.appends++; return realAppend(node); };
  let scrollTop = 0;
  Object.defineProperty(viewer, "scrollTop", {
    configurable: true,
    get() { return scrollTop; },
    set(v) { spy.scrollWrites++; scrollTop = v; },
  });
  return spy;
}

test("a burst of log lines becomes one fragment append per frame", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");
  const before = viewer.childElementCount;
  const spy = spyOnViewer(viewer);

  for (let i = 0; i < 100; i++) h.app.logPanel.addLog(`INFO line ${i}`);

  assert.equal(spy.appends, 0, "nothing may reach the DOM before the frame runs");
  h.flushRaf();

  assert.equal(spy.appends, 1,
    "100 lines in one tick must be ONE fragment append — per-line appendChild (the mutant) makes " +
    "this 100");
  assert.ok(spy.scrollWrites <= 1,
    `scrollTop written ${spy.scrollWrites} times; each write is preceded by a scrollHeight read, ` +
    "which forces a synchronous layout — one per frame is the budget");
  assert.equal(viewer.childElementCount, before + 100, "every line must still be drawn");
  assert.match(viewer.textContent, /line 99/);
  assert.equal(h.el("log-count").textContent, `${h.app.logPanel.logs.length} log entries`);
});

test("the batched path still trims to the 500-line window", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");

  for (let i = 0; i < 600; i++) h.app.logPanel.addLog(`INFO line ${i}`);
  h.flushRaf();

  assert.equal(h.app.logPanel.logs.length, 500, "the array keeps the newest 500");
  assert.equal(viewer.childElementCount, 500,
    "the DOM must keep the same 500 — dropping the excess trim (the mutant) grows the viewer " +
    "without bound on a busy install");
  assert.match(viewer.textContent, /line 599/);
  assert.doesNotMatch(viewer.textContent, /line 99\b/);
});

test("a full render supersedes a queued batch", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");

  h.app.logPanel.addLog("INFO queued line");
  h.app.logPanel.logs = ["INFO replaced line"];
  h.app.logPanel.renderLogs(); // e.g. the initial_state path
  h.flushRaf();

  assert.match(viewer.textContent, /replaced line/);
  assert.doesNotMatch(viewer.textContent, /queued line/,
    "a queued batch must be dropped by a full rebuild — flushing it afterwards (the mutant) " +
    "re-appends a line the rebuild already decided against");
});
