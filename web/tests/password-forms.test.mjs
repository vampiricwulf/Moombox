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
const SPACE_WARNING = "This password starts or ends with a space. It is kept as typed.";

// Type `value` into the sl-input `id`, as the browser reports typing.
function type(h, id, value) {
  const input = h.el(id);
  input.value = value;
  input.dispatchEvent(new h.window.Event("sl-input"));
  return input;
}
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

// W26-09: the wizard read the password through its trimming val(), so it
// posted "correct horse battery" for " correct horse battery " while the login
// posts the field as typed — the operator could not log in with what they
// typed. No surface trims a password now.
//
// Mutant killed: reading the password through val() (trimmed).
test("the wizard posts the external password as typed", { skip }, async () => {
  const h = await wizard();
  const typed = " correct horse battery ";
  h.el("setup-network-access").value = "external";
  h.el("setup-external-password").value = typed;
  await h.app.setup.finishAdvancedSetup();
  const call = h.http.matching("/api/setup/complete", "POST")[0];
  assert.ok(call, "setup/complete was not posted");
  assert.equal(call.body.password, typed);
});

// Both forms warn — never refuse — while the password typed starts or ends
// with a space, and drop the warning once it does not; Settings keeps its own
// hint beside it.
//
// Mutants killed: the wizard's field not bound to the warning; Settings' new
// password not bound; passwordHasOuterSpace always false; the warning
// replacing Settings' own hint.
test("both forms warn while a password starts or ends with a space", { skip }, async () => {
  const h = await wizard();
  let input = type(h, "setup-external-password", "padded secret ");
  assert.equal(input.getAttribute("help-text"), SPACE_WARNING);
  input = type(h, "setup-external-password", "plain secret");
  assert.equal(input.getAttribute("help-text"), null);

  const s = await settingsForm();
  input = type(s, "security-new-password", " padded secret");
  assert.equal(input.getAttribute("help-text"), `Minimum 8 characters ${SPACE_WARNING}`);
  input = type(s, "security-new-password", "plain secret");
  assert.equal(input.getAttribute("help-text"), "Minimum 8 characters");
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
