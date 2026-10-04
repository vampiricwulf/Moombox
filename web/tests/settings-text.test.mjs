// Settings and setup text that sent the operator the wrong way or dropped the
// server's reason: a "Settings → Security" that does not exist (the password
// lives under Network), a restart prompt that never named HTTPS/TLS, fixed
// failure toasts over the server's reason, wizard validation errors without
// their field, and a hide-finished field with no upper bound though the
// server refuses anything over 365.
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

const said = (h) => h.toasts().map((t) => t.textContent).join(" | ");

test("the security banner points at the section the password lives in", { skip }, async () => {
  const h = await harness.makeApp();
  assert.match(h.el("security-banner").textContent, /Settings\s*→\s*Network\s*→\s*Password/);
  assert.doesNotMatch(h.el("security-banner").textContent, /→\s*Security/);
});

test("the hide-finished fields carry the server's 365-day bound", { skip }, async () => {
  const h = await harness.makeApp();
  assert.equal(h.el("cfg-hide-finished-days").getAttribute("max"), "365");
  assert.equal(h.el("setup-hide-age").getAttribute("max"), "365");
});

// Mutant: restore the fixed "Failed to fetch release notes".
test("a refused release-notes fetch names the server's reason", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "GET /api/update/release-notes": () => harness.response({ status: 503, body: { error: "updater not configured" } }) },
  });
  h.el("btn-view-release-notes").click();
  await h.flush();
  assert.match(said(h), /Failed to fetch release notes: updater not configured/);
});

// Mutant: restore the fixed "Re-scan failed".
test("a refused re-scan names the server's reason", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "POST /api/backfill/rescan": () => harness.response({ status: 503, body: { error: "backfill unavailable" } }) },
  });
  await h.app.settings.rescanFeedHistory();
  await h.flush();
  assert.match(said(h), /Re-scan failed: backfill unavailable/);
});

// Mutant: join Object.values again — the field name is gone.
test("a refused setup names the field each error belongs to", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "POST /api/setup/complete": () => harness.response({
        status: 400,
        body: { error: "validation failed", details: { "paths.staging_directory": "Path cannot contain a .. segment" } },
      }),
    },
  });
  await h.app.setup.submitSetup({}, null);
  await h.flush();
  assert.match(said(h), /paths\.staging_directory: Path cannot contain a \.\. segment/);
});

// Mutant: drop "HTTPS/TLS" from the prompt.
test("the restart prompt names HTTPS/TLS among the settings it covers", { skip }, async () => {
  const h = await harness.makeApp();
  const asked = [];
  h.app.showConfirm = async (message) => { asked.push(message); return false; };
  h.app.settings._originalRestartValues = { "network.https_enabled": false };
  await h.app.settings._checkRestartRequired({ network: { https_enabled: true } });
  assert.equal(asked.length, 1, "toggling HTTPS did not prompt");
  assert.match(asked[0], /HTTPS\/TLS/);
});
