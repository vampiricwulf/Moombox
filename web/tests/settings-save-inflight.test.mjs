// K32: a Settings save is a state of its own. An edit typed while the save's
// PUT (or the custom-browser validation ahead of it) is in flight is not part
// of the payload, so the save's success must not mark the page clean: it
// used to, and the next non-dirty loadConfig (a WebSocket reconnect) then
// refilled the form over the unsent edit. And the banner's Save, unlike the
// page's, was never disabled during a save, so a second PUT could start.
//
// jsdom probe as in the other settings suites: an absent module skips.
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

const CONFIG = {
  network: { port: 774, network_access: "localhost", public_url: "" },
  logs: { log_level: "INFO", log_max_file_size: 10485760, log_max_files: 5 },
  downloader: { num_parallel_downloads: 3, output_template: "${title}" },
  notifications: [],
};

/** A page whose PUT /api/config waits until the test calls release(). */
async function openWithPendingPut() {
  const gate = {};
  const h = await harness.makeApp({
    initialState: { config: structuredClone(CONFIG) },
    routes: {
      "GET /api/config": () => structuredClone(CONFIG),
      "PUT /api/config": () => new Promise((resolve) => {
        gate.release = () => resolve({ success: true, changed: true });
      }),
    },
  });
  h.app.config = structuredClone(CONFIG);
  h.app.settings.populateConfigForm();
  return { h, gate };
}

const typeInto = (h, id, value) => {
  h.el(id).value = value;
  h.el(id).dispatchEvent(new h.window.CustomEvent("sl-input", { bubbles: true, composed: true }));
};

// MUTANT: settled() clearing the dirty flag unconditionally — the port edit
// made during the PUT reads clean, and loadConfig puts 774 back.
test("an edit made while a save is in flight stays dirty and survives loadConfig", { skip }, async () => {
  const { h, gate } = await openWithPendingPut();
  typeInto(h, "cfg-log-level", "DEBUG");
  const saving = h.app.settings.saveConfig();
  await h.flush();
  assert.equal(h.http.matching("/api/config", "PUT").length, 1);

  typeInto(h, "cfg-port", "775");
  gate.release();
  await saving;
  await h.flush();

  assert.equal(h.app.settings._dirty, true, "the edit made during the save was marked clean");
  assert.notEqual(h.el("settings-unsaved-banner").style.display, "none", "the unsaved banner was hidden over an unsent edit");

  await h.app.loadConfig();
  await h.flush();
  assert.equal(String(h.el("cfg-port").value), "775", "loadConfig overwrote the unsent edit");
});

// MUTANT: the _saving gate dropped — the banner's Save starts a second PUT.
test("a second Save during a save is refused, from either button", { skip }, async () => {
  const { h, gate } = await openWithPendingPut();
  typeInto(h, "cfg-log-level", "DEBUG");
  const saving = h.app.settings.saveConfig();
  await h.flush();

  assert.equal(h.el("settings-banner-save").disabled, true, "the banner Save stayed enabled during a save");
  assert.equal(h.el("save-config-btn").disabled, true);
  h.el("settings-banner-save").click();
  await h.app.settings.saveConfig();
  await h.flush();
  assert.equal(h.http.matching("/api/config", "PUT").length, 1, "a second PUT started during the first");

  gate.release();
  await saving;
  await h.flush();
  assert.equal(h.app.settings._dirty, false, "a save with no edit since it began leaves the page clean");
  assert.equal(h.el("settings-banner-save").disabled, false);
  assert.equal(h.el("save-config-btn").disabled, false);
});
