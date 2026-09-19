// POST /api/update/check spends one of GitHub's 60 unauthenticated requests
// per hour, so the server now debounces it to one accepted check per 30 s and
// answers 200 {success:false, debounced:true, retryAfterMs:N} — the same shape
// /api/monitors/check-now and /api/backfill/rescan already return (WEB-12).
// Without a branch for it the Settings button read that body as "no update"
// and said "Up to date", which is a lie: nothing was checked.
//
// Like app.test.mjs, this suite needs jsdom, and `node --test web/tests/*.test.mjs`
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

// The Tasks panel's force-check (app.checkMonitorsNow) says exactly this for
// the same server answer; one debounce deserves one sentence.
const WORDING = /^Just checked — try again in \d+s$/;

async function clickCheckNow(h) {
  h.el("btn-check-update-now").click();
  await h.flush();
  await h.flush();
  return h.el("update-check-result");
}

// MUTANT: drop the `if (data.debounced)` branch — the debounced body falls
// through to the `available === undefined` else and the button reports
// "Up to date" for a check that never happened.
test("a debounced check reports the wait, not 'Up to date'", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "POST /api/update/check": () => ({ success: false, debounced: true, retryAfterMs: 24500 }),
    },
  });

  const result = await clickCheckNow(h);
  assert.match(result.textContent, WORDING, "the debounced answer must say how long to wait");
  assert.ok(
    !/up to date/i.test(result.textContent),
    `a debounced check checked nothing; got ${JSON.stringify(result.textContent)}`,
  );
  // Ceil, not floor: 24500 ms must not read as "try again in 24s" and invite
  // a second refused call.
  assert.equal(result.textContent, "Just checked — try again in 25s");
});

// MUTANT: render the debounce branch unconditionally (or test `data.retryAfterMs`
// instead of `data.debounced`) — a real "no update" answer stops saying so.
test("a real up-to-date answer is unchanged", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "POST /api/update/check": () => ({ currentVersion: "2.8.8", available: false }) },
  });

  const result = await clickCheckNow(h);
  assert.equal(result.textContent, "Up to date");
});

// MUTANT: put the debounce branch AFTER `data.available` — a body that
// carried both would hide the update. Pins that the available branch still
// wins on a genuine answer and still updates the app's pending-update state.
test("an available update is still announced", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "POST /api/update/check": () => ({ currentVersion: "2.8.8", available: true, version: "2.9.0" }),
    },
  });

  const result = await clickCheckNow(h);
  assert.equal(result.textContent, "v2.9.0 available!");
  assert.equal(h.app._updateAvailable?.version, "2.9.0");
});

// MUTANT: read retryAfterMs without the `|| 0` fallback — a body missing the
// field renders "try again in NaNs".
test("a debounced body with no wait still renders a number", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "POST /api/update/check": () => ({ success: false, debounced: true }) },
  });

  const result = await clickCheckNow(h);
  assert.equal(result.textContent, "Just checked — try again in 0s");
});
