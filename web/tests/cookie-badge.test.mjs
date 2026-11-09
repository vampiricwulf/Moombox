// The dashboard's cookie badge (cookieIndicatorState) had no test. Its
// precedence is the TUI's (owner ruling R-D4: Relogin > FileUnreadable >
// Parked/Rejected > Unknown > None > OK), the list cmd/moombox's parity test
// compares with tui.CookieBadgePrecedence; these cases mirror
// internal/tui TestCookieBadgePrecedence and cmd/moombox cookie_badge_test.go.
import { test } from "node:test";
import assert from "node:assert/strict";

import { cookieIndicatorState, cookieCheckState, cookieBadgeState, COOKIE_BADGE_PRECEDENCE } from "../public/modules/utils.js";

const yt = (status, relogin = false, parked = false) => cookieIndicatorState("youtube", status, relogin, parked);

test("the precedence is the TUI's", () => {
  assert.deepEqual(COOKIE_BADGE_PRECEDENCE, ["relogin", "fileUnreadable", "parked", "rejected", "unknown", "none", "ok"]);
});

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
