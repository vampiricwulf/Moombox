// The two "active platform" toggles (cookies.active_platforms) show either the
// operator's explicit override or, without one, the server's INFERRED answer
// (verified cookie platforms, then enabled channels). An empty list is an
// override too: both indicators off. So a save must (a) send [] when the
// operator turns both off — the server stores it rather than reading it as "no
// override" — and (b) send nothing while the toggles still show an unedited
// inferred answer, which saving back would freeze. A save sends only the
// fields the operator changed, so an untouched override is left to the server
// as well.
//
// Like app.test.mjs this suite needs jsdom and skips (not fails) without it.
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

async function openSettings(cookies, inferred) {
  const config = { network: { port: 774 }, downloader: { max_video_resolution: 1080 }, cookies };
  const h = await harness.makeApp({
    initialState: { config: structuredClone(config) },
    routes: { "PUT /api/config": () => ({ success: true }) },
  });
  h.app.config = structuredClone(config);
  h.app.activePlatforms = inferred;
  h.app.settings.populateConfigForm();
  return h;
}

/** The body of the save, with the port edited so there is always one. */
async function savedBody(h) {
  h.el("cfg-port").value = "775";
  await h.app.settings.saveConfig();
  await h.flush();
  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.ok(put, "saveConfig issued no PUT /api/config");
  return put.body;
}

// MUTANT: always send the toggles' state — the inferred answer is frozen into
// an explicit override by a save that never touched them.
test("an unedited inferred answer is not written back", { skip }, async () => {
  const h = await openSettings({ active_platforms: null }, { youtube: true, twitch: false });
  assert.equal(h.el("cfg-active-youtube").checked, true);
  const body = await savedBody(h);
  assert.equal(body.cookies?.active_platforms, undefined,
    `active_platforms was sent (${JSON.stringify(body.cookies)}) though no toggle changed`);
});

// MUTANT: compare the toggles with the config's own (absent) key instead of
// what they showed — turning both off from an inferred answer is then never
// sent, and the indicators come straight back.
test("turning both toggles off sends an empty override", { skip }, async () => {
  const h = await openSettings({ active_platforms: null }, { youtube: true, twitch: true });
  h.el("cfg-active-youtube").checked = false;
  h.el("cfg-active-twitch").checked = false;
  const body = await savedBody(h);
  assert.deepEqual(body.cookies.active_platforms, []);
});

// An existing override, even an empty one, is the server's to keep when the
// toggles were not touched: a TUI change of it while the page was open
// survives the save.
//
// MUTANT: send an explicit override on every save (the old rule) — the TUI's
// change is reverted.
test("an existing override is left alone by a save that did not touch it", { skip }, async () => {
  const h = await openSettings({ active_platforms: [] }, { youtube: true, twitch: true });
  assert.equal(h.el("cfg-active-youtube").checked, false, "an empty override shows both off");
  assert.equal(h.el("cfg-active-twitch").checked, false);
  const body = await savedBody(h);
  assert.equal(body.cookies?.active_platforms, undefined,
    `an untouched override was sent: ${JSON.stringify(body.cookies)}`);
});
