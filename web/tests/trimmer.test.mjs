// The trim dialog's failure states and keyboard reach: a preview that cannot
// load said nothing, a refused time was left in its box while the timeline
// kept the old value, and the timeline handles were mouse-only divs.
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

async function openTrim() {
  const h = await harness.makeApp();
  h.app.trimmer.open({ id: "j1", title: "t", lengthSeconds: 100 });
  return h;
}
const said = (h) => h.toasts().map((t) => t.textContent).join(" | ");

// Mutant: drop the video error listener.
test("a trim preview that cannot load says so", { skip }, async () => {
  const h = await openTrim();
  h.el("trim-video").dispatchEvent(new h.window.Event("error"));
  assert.match(said(h), /trim preview could not load/);
});

// Mutant: drop the refuse() call from the start input's sl-change.
test("a refused trim time is put back and explained", { skip }, async () => {
  const h = await openTrim();
  const input = h.el("trim-start-input");
  input.value = "5:00"; // past the 1:40 recording
  input.dispatchEvent(new h.window.Event("sl-change"));
  assert.equal(input.value, "0:00", "the box shows the marker it still holds");
  assert.equal(h.app.trimmer.startMarker, 0);
  assert.match(said(h), /Enter a time between 0:00 and 1:40/);
});

// Mutants: drop the focused-handle arm from _onKeyDown (the arrow seeks the
// video instead); drop the aria-value updates from _updateTimeline.
test("a focused timeline handle is a keyboard slider", { skip }, async () => {
  const h = await openTrim();
  const start = h.el("trim-handle-start");
  assert.equal(start.getAttribute("role"), "slider");
  assert.equal(start.getAttribute("tabindex"), "0");

  const press = (target, key, init = {}) => {
    const ev = new h.window.KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init });
    target.dispatchEvent(ev); // a target, so composedPath() is real
    h.app.trimmer._onKeyDown(ev);
  };
  press(start, "ArrowRight", { shiftKey: true });
  press(start, "ArrowRight");
  assert.equal(h.app.trimmer.startMarker, 6);
  assert.equal(start.getAttribute("aria-valuenow"), "6");
  assert.equal(start.getAttribute("aria-valuetext"), "0:06");
  assert.equal(h.el("trim-start-input").value, "0:06");

  const end = h.el("trim-handle-end");
  press(end, "ArrowRight");
  assert.equal(h.app.trimmer.endMarker, 100, "the end marker stops at the recording's end");
  press(end, "ArrowLeft");
  assert.equal(h.app.trimmer.endMarker, 99);
});
