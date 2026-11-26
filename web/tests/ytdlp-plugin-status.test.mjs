// The Settings → Integrations yt-dlp plugin card: the badge, the warnings and
// the install button drawn from GET /api/ytdlp-plugin/status
// (ytdlpplugin.Info). An outdated file (ytdlpplugin.Info.Outdated: written by
// an older Moombox) is offered as an Update, like a port or scheme mismatch.
//
// Like app.test.mjs this suite needs jsdom, and is skipped, not failed, when
// it is absent.
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

const base = {
  installed: true,
  pluginDir: "/plug",
  currentPort: 774,
  httpsEnabled: true,
  installedPort: 774,
  installedScheme: "https",
  portMismatch: false,
  extractedPath: "/plug/moombox",
  unparseable: false,
  outdated: false,
};

async function card(status) {
  const h = await harness.makeApp({
    routes: { "GET /api/ytdlp-plugin/status": () => structuredClone(status) },
  });
  await h.flush();
  await h.app.settings.loadYtdlpPluginStatus();
  const shown = (id) => h.el(id).style.display !== "none";
  return {
    badge: h.el("ytdlp-plugin-status-badge").textContent,
    outdated: shown("ytdlp-outdated-warning"),
    mismatch: shown("ytdlp-port-mismatch-warning"),
    button: h.el("ytdlp-install-btn").textContent.trim(),
    force: h.app.settings._ytdlpReinstall,
  };
}

// Mutants: the outdated arm dropped (reads "Installed"); its warning never
// shown; the button left on "Reinstall" with force.
test("an outdated plugin file reads Outdated and offers Update", { skip }, async () => {
  const got = await card({ ...base, outdated: true });
  assert.deepEqual(got, { badge: "Outdated", outdated: true, mismatch: false, button: "Update", force: false });
});

// Mutant: the outdated warning shown whatever the flag.
test("a current plugin file reads Installed with no warning", { skip }, async () => {
  const got = await card(base);
  assert.deepEqual(got, { badge: "Installed", outdated: false, mismatch: false, button: "Reinstall", force: true });
});
