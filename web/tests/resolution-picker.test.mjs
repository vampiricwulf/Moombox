// The Max Resolution picker, both places it appears. The wire value is still a
// plain integer (owner ruling "R1 format") — the select just writes into the
// numeric input, which is what saveConfig and the wizard already read. So the
// tests below assert BOTH halves: the preset mapping, and that the number the
// form would send is unchanged in shape.
//
// The pure mapping tests need no DOM. The two picker tests do, and
// `node --test web/tests/*.test.mjs` must stay green without jsdom, so the
// import is probed first and those tests skip (not fail) when it is absent.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { RESOLUTION_PRESETS, resolutionPresetFor } from "../public/modules/settings.js";

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

// MUTANT: dropping the "custom" branch so an off-ladder value maps to "" — the
// select would go blank and the operator's 1234 would look deleted.
test("resolutionPresetFor maps a stored integer onto a preset or Custom", () => {
  assert.equal(resolutionPresetFor(0), "0");
  assert.equal(resolutionPresetFor(480), "480");
  assert.equal(resolutionPresetFor(1080), "1080");
  assert.equal(resolutionPresetFor(2160), "2160");
  assert.equal(resolutionPresetFor(4320), "4320");
  assert.equal(resolutionPresetFor("2160"), "2160", "a string from the DOM maps the same as a number");
  assert.equal(resolutionPresetFor(1234), "custom");
  assert.equal(resolutionPresetFor(undefined), "");
  assert.equal(resolutionPresetFor(null), "");
  assert.equal(resolutionPresetFor(""), "");
  assert.equal(resolutionPresetFor(-1), "");
});

// MUTANT: reordering or trimming the ladder — the TUI twin
// (internal/tui/settings.go resolutionPresets) would no longer offer the same
// seven values, and the two UIs would disagree about what "Unbounded" is.
test("the preset ladder is the seven values both UIs offer", () => {
  assert.deepEqual(RESOLUTION_PRESETS, ["0", "480", "720", "1080", "1440", "2160", "4320"]);
});

// MUTANT: populateConfigForm not calling _wireResolutionPreset — the select
// stays blank on a loaded config and the custom input is visible for a preset.
test("the settings picker shows the stored cap as a preset", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.config = { downloader: { max_video_resolution: 1440 } };
  h.app.settings.populateConfigForm();

  assert.equal(h.el("cfg-max-resolution-preset").value, "1440");
  assert.equal(h.el("cfg-max-resolution").value, "1440");
  assert.equal(h.el("cfg-max-resolution").style.display, "none", "a preset hides the custom box");
});

// MUTANT: the Custom branch dropped from _wireResolutionPreset — the custom box
// stays hidden and an off-ladder cap becomes uneditable.
test("an off-ladder cap shows as Custom with its number", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.config = { downloader: { max_video_resolution: 1234 } };
  h.app.settings.populateConfigForm();

  assert.equal(h.el("cfg-max-resolution-preset").value, "custom");
  assert.equal(h.el("cfg-max-resolution").value, "1234");
  assert.notEqual(h.el("cfg-max-resolution").style.display, "none", "Custom reveals the number box");
});

// MUTANT: the sl-change handler not writing the preset into the numeric input —
// saveConfig would send the PREVIOUS cap, silently ignoring the pick.
test("picking a preset writes the integer the form will send", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.config = { downloader: { max_video_resolution: 2160 } };
  h.app.settings.populateConfigForm();

  const select = h.el("cfg-max-resolution-preset");
  select.value = "720";
  select.dispatchEvent(new h.window.CustomEvent("sl-change"));
  assert.equal(h.el("cfg-max-resolution").value, "720");
  assert.equal(h.app.getInputNumber("cfg-max-resolution"), 720, "the wire value stays an integer");

  select.value = "0";
  select.dispatchEvent(new h.window.CustomEvent("sl-change"));
  assert.equal(h.app.getInputNumber("cfg-max-resolution"), 0, "Unbounded is the integer 0, not a string");

  select.value = "custom";
  select.dispatchEvent(new h.window.CustomEvent("sl-change"));
  assert.equal(h.el("cfg-max-resolution").value, "0", "switching to Custom keeps the number to edit");
  assert.notEqual(h.el("cfg-max-resolution").style.display, "none");
});

// MUTANT: setupListeners not wiring the wizard picker — the first-run wizard
// would send whatever the hidden input happened to hold.
test("the setup wizard picker writes the same integer", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.setup.setupListeners();

  const select = h.el("setup-max-resolution-preset");
  select.value = "1080";
  select.dispatchEvent(new h.window.CustomEvent("sl-change"));
  assert.equal(h.el("setup-max-resolution").value, "1080");
  assert.equal(h.el("setup-max-resolution").style.display, "none");

  select.value = "custom";
  select.dispatchEvent(new h.window.CustomEvent("sl-change"));
  assert.notEqual(h.el("setup-max-resolution").style.display, "none");
});
