// How long a toast stays (owner ruling R2-D3): toastDuration in
// web/public/modules/utils.js decides it, and showToast in web/public/app.js
// — the one place a toast is made — applies it to the alert and to the
// countdown bar. Every toast used to close after 3 s, so an error or a
// several-sentence cookie result was gone before it could be read, most of
// all on a phone, where there is no hover to hold it.
//
// The helper's tests are pure. The showToast tests need jsdom, and `node
// --test web/tests/*.test.mjs` must stay green without it, so the import is
// probed first and those tests skip (not fail) when it is absent.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { toastDuration } from "../public/modules/utils.js";

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

// The refresh-declined warning the cookie button shows, about 190 characters.
const LONG = "Browser cookie refresh declined to run — a setup or another refresh is already in flight, or no platform has cookies worth refreshing. Nothing was learned about these cookies.";

// MUTANT: danger given the scaled time (it closes by itself); the warning
// floor dropped (a short warning is back to 3 s); the per-character time
// dropped (a long message is back to its floor); the cap dropped (a page of
// text stays for minutes); the scaling applied to short successes (a
// "Settings saved" lingers).
test("a toast's time follows its variant and its length", () => {
  assert.equal(toastDuration("Failed to add video", "danger"), Infinity, "an error stays until closed");
  assert.equal(toastDuration(LONG, "danger"), Infinity);

  assert.equal(toastDuration("Video added successfully", "success"), 3000);
  assert.equal(toastDuration("Cookies: still checking…", "primary"), 3000);
  assert.equal(toastDuration("Cookies: still checking…"), 3000, "no variant is the neutral one");

  assert.equal(toastDuration("Invalid YouTube/Twitch URL or ID", "warning"), 6000, "a short warning still gets 6 s");

  assert.equal(toastDuration(LONG, "warning"), LONG.length * 60);
  assert.equal(toastDuration(LONG, "success"), LONG.length * 60, "a long success is scaled too");
  assert.ok(LONG.length * 60 > 6000 && LONG.length * 60 < 15000, "the sample must land between the floor and the cap");

  assert.equal(toastDuration("x".repeat(400), "warning"), 15000, "no timed toast outlasts the cap");
  assert.equal(toastDuration("x".repeat(400), "primary"), 15000);
});

// MUTANT: showToast back on a fixed alert.duration (the long warning closes
// at 3 s); the --toast-duration property not set (the bar empties in 3 s
// while the toast stays 10).
test("showToast gives the alert and its countdown bar the helper's time", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.showToast(LONG, "warning");
  const [alert] = h.toasts();
  const want = toastDuration(LONG, "warning");
  assert.equal(alert.duration, want);
  assert.equal(alert.style.getPropertyValue("--toast-duration"), `${want}ms`);
  assert.ok(alert.querySelector(".toast-countdown"), "a timed toast shows its countdown");
});

// MUTANT: a duration on the danger alert; the countdown bar still drawn on a
// toast that never times out (it would empty and the toast would stay).
test("a danger toast stays until closed and draws no countdown", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.showToast("Failed to add video: network error", "danger");
  const [alert] = h.toasts();
  assert.equal(alert.duration, Infinity);
  assert.equal(alert.closable, true, "it can only go by being closed");
  assert.equal(alert.querySelector(".toast-countdown"), null);
  assert.equal(alert.style.getPropertyValue("--toast-duration"), "");
});
