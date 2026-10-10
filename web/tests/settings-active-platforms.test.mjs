// The two "active platform" toggles (cookies.active_platforms) show either the
// operator's explicit override or, without one, the server's INFERRED answer
// (verified cookie platforms, then enabled channels). An empty list is an
// override too: both indicators off. So a save must (a) send [] when the
// operator turns both off — the server stores it rather than reading it as "no
// override" — and (b) send nothing while the toggles still show an unedited
// inferred answer, which saving back would freeze.
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
  const config = { downloader: { max_video_resolution: 1080 }, cookies };
  const h = await harness.makeApp({
    initialState: { config: structuredClone(config) },
    routes: { "PUT /api/config": () => ({ success: true }) },
  });
  h.app.config = structuredClone(config);
  h.app.activePlatforms = inferred;
  h.app.settings.populateConfigForm();
  return h;
}

async function savedCookies(h) {
  await h.app.settings.saveConfig();
  await h.flush();
  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.ok(put, "saveConfig issued no PUT /api/config");
  return put.body.cookies;
}

// MUTANT: always send the toggles' state — the inferred answer is frozen into
// an explicit override by a save that never touched them.
test("an unedited inferred answer is not written back", { skip }, async () => {
  const h = await openSettings({ active_platforms: null }, { youtube: true, twitch: false });
  assert.equal(h.el("cfg-active-youtube").checked, true);
  const cookies = await savedCookies(h);
  assert.equal("active_platforms" in cookies, false,
    `active_platforms was sent (${JSON.stringify(cookies.active_platforms)}) though no toggle changed`);
});

// MUTANT: drop the "toggle changed" arm — turning both off from an inferred
// answer is then never sent, and the indicators come straight back.
test("turning both toggles off sends an empty override", { skip }, async () => {
  const h = await openSettings({ active_platforms: null }, { youtube: true, twitch: true });
  h.el("cfg-active-youtube").checked = false;
  h.el("cfg-active-twitch").checked = false;
  const cookies = await savedCookies(h);
  assert.deepEqual(cookies.active_platforms, []);
});

// MUTANT: drop the "explicit" arm — an existing override stops being re-sent,
// harmless today but the baseline must not silently forget it.
test("an existing override, even an empty one, is sent on every save", { skip }, async () => {
  const h = await openSettings({ active_platforms: [] }, { youtube: true, twitch: true });
  assert.equal(h.el("cfg-active-youtube").checked, false, "an empty override shows both off");
  assert.equal(h.el("cfg-active-twitch").checked, false);
  const cookies = await savedCookies(h);
  assert.deepEqual(cookies.active_platforms, []);
});
