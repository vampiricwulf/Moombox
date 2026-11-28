// The latency-mode backfill in Settings (owner ruling RV-20): its switch,
// monitors.latency_backfill, and the one status line beside it, worded by
// latencyBackfillLine in utils.js, the twin of Go's worker.LatencyBackfillLine
// over fixtures/latency-backfill-line-parity.json (the Go half is
// TestLatencyBackfillLineParity in internal/worker).
//
// The parity test runs everywhere; the DOM tests need jsdom and are skipped,
// not failed, when it is absent.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { latencyBackfillLine } from "../public/modules/utils.js";

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

const PARITY = JSON.parse(readFileSync(new URL("./fixtures/latency-backfill-line-parity.json", import.meta.url), "utf8"));

// Mutants: any wording drifting from Go's; the rate-limit end worded in the
// process's zone instead of the one asked for (the UTC+1 and UTC-5 rows).
test("latencyBackfillLine matches Go over the fixture", () => {
  assert.ok(PARITY.cases.length >= 15);
  for (const c of PARITY.cases) {
    assert.equal(latencyBackfillLine(c.status, c.utcOffsetMinutes), c.line, c.name);
  }
});

const CONFIG = {
  logs: { log_level: "INFO" },
  monitors: { membership_discovery: true, latency_backfill: true },
};

async function openSettings(config = CONFIG, routes = {}) {
  const puts = [];
  const h = await harness.makeApp({
    initialState: { config: structuredClone(config) },
    routes: {
      "PUT /api/config": ({ body }) => {
        puts.push(body);
        return { success: true, changed: true };
      },
      ...routes,
    },
  });
  h.app.config = structuredClone(config);
  h.app.settings.populateConfigForm();
  return { h, puts };
}

const line = (h) => h.el("latency-backfill-status");

// Mutants: the initial_state field not read; the latency_backfill frame not
// handled; the line not hidden for a status it cannot word.
test("initial_state draws the line beside the switch and each frame redraws it", { skip }, async () => {
  const { h } = await openSettings();
  assert.equal(line(h).style.display, "none", "a line is shown before any status");

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], latencyBackfill: { state: "running", done: 212, total: 540 } } });
  assert.equal(line(h).textContent, "212 of 540 recordings done");
  assert.notEqual(line(h).style.display, "none");

  h.app.handleMessage({ type: "latency_backfill", payload: { state: "caught_up", done: 540, total: 540 } });
  assert.equal(line(h).textContent, "caught up (540 recordings)");

  h.app.handleMessage({ type: "latency_backfill", payload: { state: "sideways", done: 1, total: 2 } });
  assert.equal(line(h).style.display, "none", "a state this build does not know is hidden, not worded");
});

// Mutants: the Settings tab not reading the status when it opens; the
// answer not drawn.
test("opening Settings reads the status afresh", { skip }, async () => {
  const { h } = await openSettings(CONFIG, {
    "GET /api/latency-backfill": () => ({ state: "paused", pause: "offline", done: 3, total: 9 }),
  });
  const tabs = h.document.querySelector("sl-tab-group");
  tabs.dispatchEvent(new h.window.CustomEvent("sl-tab-show", { detail: { name: "settings" } }));
  await h.flush();
  await h.flush();
  assert.equal(h.http.matching("/api/latency-backfill", "GET").length, 1);
  assert.equal(line(h).textContent, "paused: offline (3 of 9 recordings done)");
});

// Mutants: the switch not filled from the config (an explicit false shows
// on); its value not gathered (the save sends nothing for it); an absent key
// read as off.
test("the switch follows the config and its edit is saved", { skip }, async () => {
  const off = await openSettings({ ...CONFIG, monitors: { latency_backfill: false } });
  assert.equal(off.h.el("cfg-latency-backfill").checked, false);

  const absent = await openSettings({ ...CONFIG, monitors: {} });
  assert.equal(absent.h.el("cfg-latency-backfill").checked, true, "an absent key is on");

  const { h, puts } = await openSettings();
  h.el("cfg-latency-backfill").checked = false;
  await h.app.settings.saveConfig();
  await h.flush();
  assert.deepEqual(puts, [{ monitors: { latency_backfill: false } }]);
});
