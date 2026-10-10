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

// W27 review: Settings' restart redirect followed the configured port even
// when the port was not changed, so on a page served from a fallback port
// (774 taken, the server on 775 — and on 775 again after the restart,
// internal/web/server.go) turning HTTPS on sent the tab to
// https://localhost:774/, whatever held it. An unchanged port now redirects
// to the page's own port, the setup wizard's W26-12 rule; a changed port
// still goes to the new value.
//
// Mutants killed: the configured port whatever changed (:774 for the HTTPS
// toggle); the page's port whatever changed (:775 for the port change).
test("the restart redirect keeps the serving port unless the port changed", { skip }, async () => {
  const redirectAfter = async (saved) => {
    const h = await harness.makeApp({
      url: "http://localhost:775/",
      routes: { "POST /api/restart": () => ({ success: true }) },
    });
    h.app.showConfirm = async () => true;
    h.app.settings._originalRestartValues = { "network.port": 774, "network.https_enabled": false };
    await h.app.settings._checkRestartRequired({ network: saved });
    await h.flush();
    const toast = h.toasts().map((t) => t.textContent.trim()).find((t) => t.startsWith("Redirecting to"));
    assert.ok(toast, "no redirect toast");
    return toast;
  };
  assert.match(await redirectAfter({ port: 774, https_enabled: true }), /Redirecting to https:\/\/localhost:775\/ /);
  assert.match(await redirectAfter({ port: 8080, https_enabled: false }), /Redirecting to http:\/\/localhost:8080\/ /);
});

// The channel card's monitoring switch had only a title, which is not an
// accessible name for the switch's inner input: a screen reader announced an
// unnamed switch per channel. Its label slot now carries screen-reader-only
// text naming the channel.
// Mutant: drop the visually-hidden span — the label is empty.
test("each channel's monitoring switch is named for its channel", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.config = { ...(h.app.config || {}), channels: [{ id: "UCabc", name: "Miko <Ch>", platform: "youtube", enabled: true }] };
  h.app.settings.renderChannelsList();
  const sw = h.el("channels-list").querySelector('sl-switch[data-action="toggle"]');
  assert.ok(sw, "no monitoring switch rendered");
  const label = sw.querySelector(".visually-hidden");
  assert.ok(label, "the switch has no screen-reader label");
  assert.equal(label.textContent, "Monitor Miko <Ch>");
});

// The three restart-required cookie fields carried a Restart badge but help
// text that never said so, unlike every other restart-required field.
test("restart-required cookie fields say so in their help text", { skip }, async () => {
  const h = await harness.makeApp();
  for (const id of ["cfg-cookie-file", "cfg-cookie-refresh-interval", "cfg-auto-cookies-profile-dir"]) {
    assert.match(h.el(id).getAttribute("help-text"), /Requires restart\.$/, id);
  }
});
