// Settings → Updates → Verify Signature checks the running binary's own
// signature and, when its release publishes a signed manifest, that manifest
// too. The server says which ran (manifest); the signature alone says only
// that the key signed these bytes, so the button must not call a
// signature-only check "valid" in the full check's words or colour.
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

async function verify(answer) {
  const h = await harness.makeApp({
    routes: { "POST /api/update/verify": () => harness.response(answer) },
  });
  h.el("btn-verify-signature").click();
  await h.flush();
  return h.el("verify-signature-result");
}

// Mutant: drop the manifest branch — the full check reads "Signature valid".
test("a release whose manifest was checked reads as the full verification", { skip }, async () => {
  const result = await verify({ body: { verified: true, manifest: true } });
  assert.equal(result.textContent, "Signature and release manifest valid");
  assert.equal(result.style.color, "var(--text-success)");
});

// Mutant: restore the single "Signature valid" in green for every success.
test("a release with no manifest says only the signature was checked", { skip }, async () => {
  const result = await verify({ body: { verified: true, manifest: false } });
  assert.match(result.textContent, /no signed manifest, so only the signature was checked/);
  assert.equal(result.style.color, "var(--text-warning)");
});

test("a failed check shows the server's reason", { skip }, async () => {
  const result = await verify({ status: 422, body: { error: "signature verification failed: running binary: SHA-256 mismatch" } });
  assert.match(result.textContent, /SHA-256 mismatch/);
  assert.equal(result.style.color, "var(--text-danger)");
});
