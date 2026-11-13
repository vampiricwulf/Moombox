// The dashboard's cookie badge (cookieIndicatorState) had no test. Its
// precedence is the TUI's (owner ruling R-D4: Relogin > FileUnreadable >
// Parked/Rejected > Unknown > None > OK), the list cmd/moombox's parity test
// compares with tui.CookieBadgePrecedence; these cases mirror
// internal/tui TestCookieBadgePrecedence and cmd/moombox cookie_badge_test.go.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { cookieIndicatorState, cookieIndicatorText, cookieCheckState, cookieBadgeState, cookieCheckPending } from "../public/modules/utils.js";

const yt = (status, relogin = false, parked = false) => cookieIndicatorState("youtube", status, relogin, parked);

// The contested state: cookies.txt unreadable, not authenticated, a job
// parked. The TUI shows "YT?" (the file); the dashboard showed red "A
// download stopped". Mutant: parked ranked above fileUnreadable again.
test("an unreadable cookies.txt outranks a parked job", () => {
  const s = yt({ authenticated: false, found: false, fileError: "/data/cookies.txt: permission denied" }, false, true);
  assert.equal(s.className, "indicator-error");
  assert.match(s.title, /cookies\.txt could not be read \(\/data\/cookies\.txt: permission denied\)/);
});

test("every arm by precedence", () => {
  assert.match(yt({ authenticated: true }, true, true).title, /Re-login required/);
  // Parked outranks a healthy check, and every check below it.
  assert.match(yt({ authenticated: true, found: true }, false, true).title, /A download stopped/);
  assert.match(yt({ found: true, verification: "unknown" }, false, true).title, /A download stopped/);
  // A jar that still authenticates stays green beside a file error.
  assert.equal(yt({ authenticated: true, found: true, fileError: "x" }).className, "indicator-ok");
  assert.match(yt({ found: true, verification: "failed", youtubeError: "401" }).title, /Not authenticated \(401\)/);
  assert.equal(yt({ found: true, verification: "unknown" }).className, "indicator-warn");
  assert.deepEqual(yt({ found: false }), { className: "indicator-warn", title: "YouTube: No cookies" });
  assert.deepEqual(cookieIndicatorState("twitch", { found: false }, false, false), { className: "indicator-off", title: "Twitch: Anonymous" });
  assert.equal(yt({ authenticated: true, found: true }).className, "indicator-ok");
  // An older binary: no `found` on Twitch, authenticated still green.
  assert.equal(cookieIndicatorState("twitch", { authenticated: true }, false, false).className, "indicator-ok");
});

test("cookieCheckState and cookieBadgeState are the two halves", () => {
  assert.equal(cookieCheckState({ authenticated: true, fileError: "x" }), "ok");
  assert.equal(cookieCheckState({ fileError: "x" }), "fileUnreadable");
  assert.equal(cookieCheckState({}), "none");
  assert.equal(cookieCheckState({ found: true, verification: "unknown" }), "unknown");
  assert.equal(cookieCheckState({ found: true, verification: "failed" }), "rejected");
  assert.equal(cookieBadgeState("ok", false, true), "parked");
  assert.equal(cookieBadgeState("fileUnreadable", false, true), "fileUnreadable");
  assert.equal(cookieBadgeState("unknown", true, true), "relogin");
});

// Owner ruling R-H2: the dashboard is served before the server's first cookie
// auth check answers. One predicate, two languages; the Go half of this
// fixture is cmd/moombox TestCookieCheckPendingParity.
const CHECK_PARITY = JSON.parse(readFileSync(new URL("./fixtures/cookie-check-parity.json", import.meta.url), "utf8"));

test("cookieCheckPending and the check state match Go over the fixture", () => {
  assert.ok(CHECK_PARITY.cases.length >= 6);
  for (const c of CHECK_PARITY.cases) {
    assert.equal(cookieCheckPending(c.status), c.pending, c.name);
    assert.equal(cookieCheckState(c.status), c.badge, c.name);
  }
});

test("cookieCheckPending is positive on checked === false", () => {
  assert.equal(cookieCheckPending(), false);
  assert.equal(cookieCheckPending(null, undefined), false);
  assert.equal(cookieCheckPending({}, { checked: true }), false);
  assert.equal(cookieCheckPending({ checked: true }, { checked: false }), true);
});

// Mutant: "checking" moved below "relogin" in COOKIE_BADGE_PRECEDENCE, or the
// pending arm dropped from cookieCheckState (the badge reads "No cookies").
test("an unchecked status shows checking, never no-cookies or re-login", () => {
  const unchecked = { checked: false, found: false, authenticated: false, verification: "unknown" };
  assert.deepEqual(yt(unchecked), { className: "indicator-checking", title: "YouTube: Checking cookies…" });
  assert.deepEqual(cookieIndicatorState("twitch", unchecked, false, false), { className: "indicator-checking", title: "Twitch: Checking cookies…" });
  // The pill's own text says so too, and reads as the bare code otherwise.
  assert.equal(cookieIndicatorText("youtube", unchecked), "YT…");
  assert.equal(cookieIndicatorText("twitch", unchecked), "TW…");
  assert.equal(cookieIndicatorText("youtube", { ...unchecked, checked: true }), "YT");
  assert.equal(cookieIndicatorText("twitch", undefined), "TW");
  // Whatever else holds: a relogin flag, a parked job.
  assert.equal(yt(unchecked, true, true).className, "indicator-checking");
  // Once checked, the same payload reads as it always did.
  assert.deepEqual(yt({ ...unchecked, checked: true, verification: "failed" }), { className: "indicator-warn", title: "YouTube: No cookies" });
  assert.match(yt({ ...unchecked, checked: true }, true).title, /Re-login required/);
});
