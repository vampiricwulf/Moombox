// Ctrl/Cmd/Alt combinations belong to the browser. The dashboard's and the
// trim dialog's shortcut handlers read e.key alone, so Ctrl+1..8 switched the
// dashboard's tab (and preventDefault() kept the browser from switching its
// own), Ctrl+A opened the Add dialog, Ctrl+F focused the filter instead of
// opening Find, and Ctrl+O in the trim dialog set the end marker.
// (The player's handler has its own test in player.test.mjs.)
//
// Same jsdom probe as app.test.mjs: an absent jsdom skips, anything else fails.
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

function press(h, key, init = {}) {
  const ev = new h.window.KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init });
  h.document.body.dispatchEvent(ev);
  return ev;
}

// Mutant: drop the guard in setupKeyboardShortcuts — every row acts.
test("a modified key never reaches a dashboard shortcut", { skip }, async () => {
  const h = await harness.makeApp();
  const shown = [];
  h.app.showTab = (panel) => shown.push(panel);
  let addClicks = 0;
  h.el("add-video-btn").addEventListener("click", () => { addClicks++; });

  for (const [key, mod] of [["2", "ctrlKey"], ["3", "metaKey"], ["4", "altKey"], ["a", "ctrlKey"], ["f", "ctrlKey"], ["?", "ctrlKey"]]) {
    const ev = press(h, key, { [mod]: true });
    assert.equal(ev.defaultPrevented, false, `${mod}+${key} was swallowed`);
  }
  assert.deepEqual(shown, [], "a modified digit switched the dashboard's tab");
  assert.equal(addClicks, 0, "Ctrl+A opened the Add dialog");

  // The bare keys still work.
  press(h, "2");
  assert.deepEqual(shown, ["archived"]);
});

// Mutant: drop the guard in TrimController._onKeyDown — Ctrl+O sets the end
// marker and Ctrl+I the start.
test("a modified key never reaches a trim shortcut", { skip }, async () => {
  const h = await harness.makeApp();
  const trimmer = h.app.trimmer;
  const acted = [];
  trimmer._setStartMarker = () => acted.push("start");
  trimmer._setEndMarker = () => acted.push("end");
  trimmer._togglePlay = () => acted.push("play");
  trimmer._frameStep = (d) => acted.push(`step${d}`);

  for (const [key, mod] of [["o", "ctrlKey"], ["i", "metaKey"], [" ", "altKey"], [",", "ctrlKey"], [".", "metaKey"]]) {
    const ev = new h.window.KeyboardEvent("keydown", { key, [mod]: true, bubbles: true, cancelable: true });
    h.document.body.dispatchEvent(ev); // a target, so composedPath() is real
    trimmer._onKeyDown(ev);
    assert.equal(ev.defaultPrevented, false, `${mod}+${JSON.stringify(key)} was swallowed`);
  }
  assert.deepEqual(acted, []);

  const bare = new h.window.KeyboardEvent("keydown", { key: "o", bubbles: true, cancelable: true });
  h.document.body.dispatchEvent(bare);
  trimmer._onKeyDown(bare);
  assert.deepEqual(acted, ["end"]);
});
