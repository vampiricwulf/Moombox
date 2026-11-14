// The dashboard's cookie badge (cookieIndicatorState) had no test. Its
// precedence is the TUI's (owner ruling R-D4: Relogin > FileUnreadable >
// Parked/Rejected > Unknown > None > OK), the list cmd/moombox's parity test
// compares with tui.CookieBadgePrecedence; these cases mirror
// internal/tui TestCookieBadgePrecedence and cmd/moombox cookie_badge_test.go.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { cookieIndicatorState, cookieIndicatorText, cookieCheckState, cookieBadgeState, cookieCheckPending, sessionHealth, renewalLine, cookieFlickering, formatClockTime, formatAttemptTime, sessionHealthLines } from "../public/modules/utils.js";

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

// Owner ruling K4: the session health (ok | flickering | relogin) is one
// predicate in two languages. The Go half of this fixture is
// internal/cookies TestSessionHealthParity.
const HEALTH_PARITY = JSON.parse(readFileSync(new URL("./fixtures/session-health-parity.json", import.meta.url), "utf8"));

test("sessionHealth matches Go over the fixture", () => {
  assert.ok(HEALTH_PARITY.cases.length >= 10);
  for (const c of HEALTH_PARITY.cases) {
    assert.equal(sessionHealth(c.status, Date.parse(c.now)), c.health, c.name);
  }
});

// The flickering state is text on the pill, not only a colour or a tooltip,
// and it never replaces a badge that already asks for something.
// Mutants: the flicker arm ahead of relogin/parked; the pill text left "YT".
test("a flickering session is shown on a healthy badge only", () => {
  const now = Date.parse("2026-11-13T12:00:00Z");
  const next = "2026-11-13T13:20:00Z";
  const flick = { checked: true, found: true, authenticated: true, verification: "ok", pageSignedOutAt: "2026-11-13T11:50:00Z", nextRecoveryAt: next };
  const s = cookieIndicatorState("youtube", flick, false, false, now);
  assert.equal(s.className, "indicator-warn");
  assert.equal(s.title, `YouTube: Signed in, but pages are intermittently signed out; no automatic attempt before ${formatAttemptTime(Date.parse(next), now)}`);
  assert.equal(cookieIndicatorText("youtube", flick, cookieFlickering(flick, false, false, now)), "YT~");
  assert.equal(cookieIndicatorText("twitch", flick, cookieFlickering(flick, false, false, now)), "TW~");

  // The window is over: the next signed-out page asks for the attempt.
  const due = { ...flick, nextRecoveryAt: "2026-11-13T11:55:00Z" };
  assert.match(cookieIndicatorState("youtube", due, false, false, now).title, /next automatic attempt when a page next renders signed out$/);

  // Relogin and parked keep their own text and the plain code.
  assert.match(cookieIndicatorState("youtube", flick, true, false, now).title, /Re-login required/);
  assert.match(cookieIndicatorState("youtube", flick, false, true, now).title, /A download stopped/);
  assert.equal(cookieFlickering(flick, true, false, now), false);
  assert.equal(cookieFlickering(flick, false, true, now), false);
  // A quiet day later the badge is plain green again with no new payload.
  const later = now + 24 * 60 * 60 * 1000;
  assert.deepEqual(cookieIndicatorState("youtube", flick, false, false, later), { className: "indicator-ok", title: "YouTube: Authenticated" });
  assert.equal(cookieFlickering(flick, false, false, later), false);
});

// The words and the time are visible text in Settings, not only the pill's
// tooltip: one line for each platform whose pill reads "YT~" / "TW~".
// Mutants: the line built for a platform that is not active, or whose badge
// asks for something; the time left out of the line.
test("a flickering session is spelled out as visible text with the next attempt", () => {
  const now = Date.parse("2026-11-13T12:00:00Z");
  const next = Date.parse("2026-11-13T13:20:00Z");
  const flick = { checked: true, found: true, authenticated: true, verification: "ok", pageSignedOutAt: "2026-11-13T11:50:00Z", nextRecoveryAt: "2026-11-13T13:20:00Z" };
  const ok = { checked: true, found: true, authenticated: true, verification: "ok" };
  const row = (platform, status, extra = {}) => ({ platform, status, relogin: false, parked: false, active: true, ...extra });

  assert.deepEqual(sessionHealthLines([row("youtube", flick), row("twitch", ok)], now), [
    `YouTube: Signed in, but pages are intermittently signed out; no automatic attempt before ${formatAttemptTime(next, now)}`,
  ]);
  assert.equal(sessionHealthLines([row("youtube", flick), row("twitch", flick)], now).length, 2);
  assert.match(sessionHealthLines([row("twitch", flick)], now)[0], /^Twitch: /);
  // Nothing for a platform that is off, re-login, parked, or past the quiet day.
  assert.deepEqual(sessionHealthLines([row("youtube", flick, { active: false })], now), []);
  assert.deepEqual(sessionHealthLines([row("youtube", flick, { relogin: true })], now), []);
  assert.deepEqual(sessionHealthLines([row("youtube", flick, { parked: true })], now), []);
  assert.deepEqual(sessionHealthLines([row("youtube", flick)], now + 24 * 60 * 60 * 1000), []);

  // The page has the element app.js fills, and app.js fills it from this helper.
  assert.match(readFileSync(new URL("../public/index.html", import.meta.url), "utf8"), /id="cookie-session-health"/);
  const app = readFileSync(new URL("../public/app.js", import.meta.url), "utf8");
  assert.match(app, /getElementById\("cookie-session-health"\)[\s\S]{0,400}sessionHealthLines\(/);
});

test("formatAttemptTime names the day when it is not today", () => {
  const now = new Date(2026, 10, 13, 12, 0).getTime();
  assert.equal(formatAttemptTime(new Date(2026, 10, 13, 14, 5).getTime(), now), "14:05");
  assert.equal(formatAttemptTime(new Date(2026, 10, 14, 9, 5).getTime(), now), "Sat 09:05");
});

test("formatClockTime is zero-padded local time", () => {
  const d = new Date(2026, 10, 13, 4, 5);
  assert.equal(formatClockTime(d.getTime()), "04:05");
});

// Owner ruling K5: the renewal line is one sentence in two languages. The Go
// half of this fixture is internal/cookies TestRenewalLineParity.
const RENEWAL_PARITY = JSON.parse(readFileSync(new URL("./fixtures/renewal-line-parity.json", import.meta.url), "utf8"));

test("renewalLine matches Go over the fixture", () => {
  assert.ok(RENEWAL_PARITY.cases.length >= 10);
  for (const c of RENEWAL_PARITY.cases) {
    assert.equal(renewalLine(c.status, Date.parse(c.now)), c.line, c.name);
  }
});
