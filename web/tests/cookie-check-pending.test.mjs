// Owner ruling R-H2: the dashboard is served before the server's first cookie
// auth check answers. Until `checked` turns true the badges read "checking",
// the re-login warnings are held back, and the dashboard asks /api/status
// again by itself, since nothing pushes the answer.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = "jsdom not installed - run npm ci in web/tests (" + e.code + ")";
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const warningLabels = (document) =>
  [...document.getElementById("status-warnings").children].map((el) => el.textContent);

const pending = { checked: false, found: false, authenticated: false, verification: "unknown" };
const statusWith = (cookie) => ({
  version: "test",
  activePlatforms: { youtube: true, twitch: true },
  autoCookieReloginRequired: { youtube: true },
  cookieStatus: cookie,
  twitchAuthStatus: cookie,
});

test("an unchecked status shows checking, holds the re-login warning back and polls until it is checked", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "GET /api/status": statusWith(pending) } });
  await h.app.loadStatus();

  const yt = h.document.getElementById("yt-indicator");
  const tw = h.document.getElementById("tw-indicator");
  // Mutant: cookieCheckState without its pending arm -> "indicator-warn".
  assert.equal(yt.className, "indicator-checking");
  assert.equal(yt.title, "YouTube: Checking cookies…");
  assert.equal(tw.className, "indicator-checking");
  // Mutant: the !checkPending guards removed -> ["YT: Re-login"].
  assert.deepEqual(warningLabels(h.document), []);
  // Mutant: the _pollCookieCheck call removed -> no timer, stuck on checking.
  assert.ok(h.app._cookieCheckTimer, "no poll is armed while the check is pending");

  // A second redraw does not stack a second timer.
  const timer = h.app._cookieCheckTimer;
  h.app.updateStatusBar();
  assert.equal(h.app._cookieCheckTimer, timer);

  // The check answers: the next poll picks it up with no reload or gesture.
  h.http.on("GET /api/status", statusWith({ checked: true, found: true, authenticated: true, verification: "ok" }));
  h.advance(1999);
  await h.flush();
  assert.equal(yt.className, "indicator-checking", "the poll fired before its 2 s");
  h.advance(1);
  await h.flush();
  assert.equal(yt.className, "indicator-error");
  assert.equal(yt.title, "YouTube: Re-login required");
  assert.equal(tw.className, "indicator-ok");
  assert.deepEqual(warningLabels(h.document), ["YT: Re-login"]);
  assert.equal(h.app._cookieCheckTimer, null, "the poll kept running after the status was checked");
});

test("a checked status, and an older binary's payload with no `checked` key, arm no poll", { skip }, async () => {
  for (const cookie of [{ checked: true, found: false, authenticated: false, verification: "failed" }, { found: false, authenticated: false }]) {
    const h = await harness.makeApp({ routes: { "GET /api/status": statusWith(cookie) } });
    await h.app.loadStatus();
    assert.equal(h.document.getElementById("yt-indicator").className, "indicator-error");
    assert.equal(h.document.getElementById("tw-indicator").className, "indicator-off");
    assert.deepEqual(warningLabels(h.document), ["YT: Re-login"]);
    assert.ok(!h.app._cookieCheckTimer);
  }
});
