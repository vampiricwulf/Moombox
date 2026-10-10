// Settings' "Check for updates" answered "Up to date" by dropping the badge,
// whatever it showed. The server withdraws only the release that was pending
// when the check STARTED (routes.ClearPendingUpdate), and keeps one another
// check found during the round trip — which reached this page as
// update_available and which every other page still offers. An up-to-date
// answer now drops the badge only while it shows the release it showed when
// the button was pressed. (The server also answers with a release it still
// holds; that half is pinned by TestUpdateCheckUpToDateKeepsAReleaseFoundDuringIt.)
//
// Like app.test.mjs this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every test is
// skipped (not failed) when jsdom is absent. Only an absent module is a skip;
// any other import failure must fail loudly.
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

const release = (v) => ({
  version: v,
  tagName: `v${v}`,
  releaseNotes: `notes ${v}`,
  releaseNotesHtml: `<p>notes ${v}</p>`,
  publishedAt: "2026-09-01T00:00:00Z",
});

async function clickCheckNow(h) {
  h.el("btn-check-update-now").click();
  await h.flush();
  await h.flush();
  return h.el("update-check-result");
}

// MUTANT: the up-to-date branch clearing the badge whatever it shows — the
// release found during the round trip goes, and the result reads "Up to date".
for (const before of [null, "9.9.1"]) {
  test(`an up-to-date answer keeps a release found during the check (badge ${before ?? "none"} before)`, { skip }, async () => {
    let h;
    h = await harness.makeApp({
      routes: {
        "POST /api/update/check": () => {
          // Another check, over this one's round trip.
          h.app.handleMessage({ type: "update_available", payload: release("9.9.2") });
          return { currentVersion: "2.8.10", available: false };
        },
      },
      initialState: { status: { version: "2.8.10", ...(before ? { updateAvailable: release(before) } : {}) } },
    });

    const result = await clickCheckNow(h);
    assert.equal(h.app.updates.available?.tagName, "v9.9.2", "the badge dropped a release the server still holds");
    assert.equal(result.textContent, "v9.9.2 available!");
  });
}

// MUTANT: the up-to-date branch never clearing — a pulled release stays
// offered, and Update Now would fetch a dead asset.
test("an up-to-date answer still drops a pulled release", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "POST /api/update/check": () => ({ currentVersion: "2.8.10", available: false }) },
    initialState: { status: { version: "2.8.10", updateAvailable: release("9.9.1") } },
  });

  const result = await clickCheckNow(h);
  assert.equal(h.app.updates.available, null);
  assert.equal(result.textContent, "Up to date");
});
