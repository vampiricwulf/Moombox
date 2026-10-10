// The dashboard's two forms that set the dashboard password: the first-run
// wizard's External Access Password (modules/setup.js finishAdvancedSetup)
// and Settings → Network's Set / Change Password (modules/settings.js
// setPassword).
//
// Like app.test.mjs, this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every test is
// skipped (not failed) when jsdom is absent. Only an absent module is a skip.
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

const TOO_LONG = "Password too long (max 128 characters)";
const toastTexts = (h) => h.toasts().map((t) => t.textContent.trim());

// The first-run wizard, its setup/complete answering a refusal so a test stops
// at the request instead of polling for a restart.
async function wizard() {
  return harness.makeApp({
    initialState: { setup: { isFirstRun: true, ffmpegValid: true } },
    routes: { "POST /api/setup/complete": () => harness.response({ status: 400, body: { error: "stop here" } }) },
  });
}

// Settings with no password set: the Set Password form, posting to a
// set-password that succeeds.
async function settingsForm() {
  return harness.makeApp({
    initialState: { config: { cookies: {}, channels: [] } },
    routes: {
      "POST /api/auth/set-password": () => ({ success: true }),
      "GET /api/auth/status": () => ({ authRequired: false, authenticated: false, hasPassword: false }),
    },
  });
}

// W26-10: setup checked only the minimum, so a pasted passphrase over 128
// bytes was set although the login refuses it. The wizard refuses it before
// posting, with the server's words, and returns to the Network step.
//
// Mutant killed: the wizard checking only the minimum (posted).
test("the wizard refuses an external password the login would refuse", { skip }, async () => {
  const h = await wizard();
  h.el("setup-network-access").value = "external";
  h.el("setup-external-password").value = "a".repeat(129);
  await h.app.setup.finishAdvancedSetup();
  assert.equal(h.http.matching("/api/setup/complete", "POST").length, 0, "setup/complete was posted");
  assert.ok(toastTexts(h).includes(TOO_LONG), `toasts ${JSON.stringify(toastTexts(h))}`);
});

// Settings → Set Password refuses the same password before it posts; the
// server would refuse it too, but with the request already spent.
//
// Mutant killed: setPassword checking only the minimum (posted).
test("Set Password refuses a password the login would refuse", { skip }, async () => {
  const h = await settingsForm();
  const pw = "a".repeat(129);
  h.el("security-new-password").value = pw;
  h.el("security-confirm-password").value = pw;
  await h.app.settings.setPassword();
  assert.equal(h.http.matching("/api/auth/set-password", "POST").length, 0, "set-password was posted");
  assert.ok(toastTexts(h).includes(TOO_LONG), `toasts ${JSON.stringify(toastTexts(h))}`);
});
