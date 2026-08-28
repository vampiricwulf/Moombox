/**
 * Shared utility functions for the Moombox dashboard.
 */

/**
 * Format seconds into H:MM:SS or M:SS timestamp.
 */
export function formatTimestamp(seconds) {
  if (seconds == null || !isFinite(seconds)) return "0:00";
  if (seconds < 0) seconds = 0;
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  return `${m}:${String(s).padStart(2, "0")}`;
}

/**
 * Format bytes into human-readable size (e.g. 1.5MB).
 */
export function formatBytes(bytes) {
  if (bytes == null || !isFinite(bytes) || bytes < 0) bytes = 0;
  if (bytes < 1024) return `${Math.round(bytes)}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  if (bytes < 1024 * 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)}GB`;
  return `${(bytes / (1024 * 1024 * 1024 * 1024)).toFixed(1)}TB`;
}

/**
 * Format total seconds into human-readable duration (e.g. 2h 15m 30s).
 */
export function formatDurationSeconds(totalSeconds) {
  if (totalSeconds == null || !isFinite(totalSeconds) || totalSeconds < 0) totalSeconds = 0;
  const total = Math.floor(totalSeconds);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours > 0) return `${hours}h ${minutes}m ${seconds}s`;
  if (minutes > 0) return `${minutes}m ${seconds}s`;
  return `${seconds}s`;
}

/**
 * Format an ISO date string as relative time (e.g. "5m ago").
 */
export function formatRelativeTime(isoDate) {
  const date = new Date(isoDate);
  if (isNaN(date.getTime())) return "unknown";
  const now = new Date();
  const diffMs = now - date;
  const diffSecs = Math.floor(diffMs / 1000);

  if (diffSecs <= 0) return "just now";
  if (diffSecs < 60) return `${diffSecs}s ago`;
  const diffMins = Math.floor(diffSecs / 60);
  if (diffMins < 60) return `${diffMins}m ago`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  const diffDays = Math.floor(diffHours / 24);
  return `${diffDays}d ago`;
}

/**
 * Check if a keyboard event originates from inside an input-like element.
 * Uses composedPath() to traverse shadow DOM boundaries (Shoelace components
 * render native <input> elements inside their shadow roots).
 */
export function isTypingInInput(e) {
  for (const el of e.composedPath()) {
    if (!(el instanceof HTMLElement)) continue;
    const tag = el.tagName;
    if (tag === "INPUT" || tag === "TEXTAREA" ||
        tag === "SL-INPUT" || tag === "SL-TEXTAREA" || tag === "SL-SELECT") return true;
    if (el.contentEditable === "true") return true;
  }
  return false;
}

/**
 * Format milliseconds to H:MM:SS or M:SS (used in player chat timestamps).
 * Supports negative offsets.
 */
export function formatMsToTime(ms) {
  if (ms == null || !isFinite(ms)) return "0:00";
  const negative = ms < 0;
  const absTotalSec = Math.floor(Math.abs(ms) / 1000);
  const h = Math.floor(absTotalSec / 3600);
  const m = Math.floor((absTotalSec % 3600) / 60);
  const s = absTotalSec % 60;

  const prefix = negative ? "-" : "";
  if (h > 0) {
    return `${prefix}${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  }
  return `${prefix}${m}:${String(s).padStart(2, "0")}`;
}

/**
 * Read the server's own message off a failed response.
 *
 * Every /api handler emits {"error": "..."} through jsonError, and `data.error`
 * is the established render idiom across this frontend — but four cookie-setup
 * call sites threw the body away and substituted `HTTP ${status}`, so a 422
 * naming the exact file that could not be read arrived as "HTTP 422" and a 424
 * meaning "no supported browser installed" arrived as "HTTP 424". The server
 * had simply never been asked.
 *
 * Falls back to the status line when the body is absent, empty, not JSON, or
 * carries no `error` — a status code is a poor message, but an empty one is
 * worse.
 */
export async function serverErrorMessage(response) {
  try {
    const data = await response.json();
    if (data && typeof data.error === "string" && data.error !== "") return data.error;
  } catch {
    // Not JSON, or an empty body. Fall through to the status line.
  }
  return `HTTP ${response.status}`;
}

/**
 * Word the toast for a platform an interactive cookie setup ACCEPTED.
 *
 * Accepted is not verified. FinishSetup takes a sign-in the user just
 * completed even when the site could not be reached to confirm it — a 429 or a
 * DNS blip is not evidence against a login that happened thirty seconds ago —
 * and the response says so in `youtubeVerification` / `twitchVerification`.
 * Reporting that middle state as "configured" claims a check that never
 * happened.
 *
 * The comparison is POSITIVE — `=== "unknown"`, never `!== "ok"` — and that is
 * load-bearing. Against an older binary that emits neither field the value is
 * undefined, which matches neither arm, so this degrades to the unqualified
 * success copy those users already see. Written the other way round, a missing
 * field would hedge about every setup against every older build.
 *
 * The "failed" arm is unreachable while acceptance requires ok-or-attempted-
 * unknown; it is spelled out so a future change to that predicate shows up as
 * wrong copy instead of a conclusive failure worded as an inconclusive one.
 */
export function cookieSetupAcceptedToast(platformLabel, verification) {
  if (verification === "unknown") {
    return {
      message: `${platformLabel} cookies saved, but Moombox could not establish whether they work — nothing has been concluded about them`,
      variant: "warning",
    };
  }
  if (verification === "failed") {
    return { message: `${platformLabel} cookies saved, but auth verification failed`, variant: "danger" };
  }
  return { message: `${platformLabel} cookies configured`, variant: "success" };
}

/**
 * Word the inline result for a finish that accepted NEITHER platform.
 *
 * This branch used to render `data.error || "No login detected. Try again."`,
 * and `data.error` never exists on a 200 — the fallback was doing all the work,
 * for two outcomes that need different advice.
 *
 * "unknown" here means credentials WERE extracted and the check never left the
 * process: the jar could not produce a cookie header or a SAPISIDHASH. A login
 * was detected; it just did not yield anything that can sign a request, so
 * "no login detected" sends the user looking for the wrong thing. An empty
 * profile reports "failed" for both platforms and still gets the original line.
 */
export function cookieSetupRejectedMessage(verification) {
  if (verification === "unknown") {
    return "Cookies were saved, but Moombox could not establish whether they authenticate — " +
      "they cannot form a signed-in request. Try signing in again.";
  }
  return "No login detected. Try again.";
}

/**
 * How long the abort-path probe may take before it gives up.
 *
 * BOUNDED ON PURPOSE, and the bound is the point. The branch that calls this
 * exists precisely because the server did not answer within 60 s — so an
 * unbounded fetch there would leave the user with no alert, no recovery
 * buttons and a countdown still on screen, all stuck behind the await, in
 * exactly the situation where the server is the thing that is wedged.
 *
 * Unrelated to the 60 s finish cap, which must not move: the server-side setup
 * grace window is priced against that constant.
 */
const COOKIE_SETUP_PROBE_TIMEOUT_MS = 5000;

/**
 * Ask the server what became of the interactive setup.
 *
 *   ok          — false when the probe itself could not answer. "We do not
 *                 know" is the one thing the abort path must not round to a
 *                 conclusion, so it is a field rather than a missing value.
 *   inProgress  — is the setup slot still held?
 *   lastError   — the message the finish recorded, or null.
 *   lastRefresh — the server-side RFC3339 stamp that ONLY a successful finish
 *                 writes (FinishSetupDetailed sets it immediately before its
 *                 final cleanup; every failure path returns before it).
 *
 * Never throws, so a caller may hold the promise unawaited.
 *
 * The last two fields are what make the "already finished" verdict honest.
 * `setupInProgress` alone cannot say WHICH conclusion was reached: cleanup()
 * runs on every failure path too, including the 422 that deliberately refuses
 * to write cookies.txt — so a report built on it announces "your cookies were
 * saved" in exactly the case where the code guaranteed they were not.
 */
export async function cookieSetupProbe() {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), COOKIE_SETUP_PROBE_TIMEOUT_MS);
  const unknown = { ok: false, inProgress: false, lastError: null, lastRefresh: null };
  try {
    const response = await fetch("/api/cookies/auto-status", { signal: controller.signal });
    if (!response.ok) return unknown;
    const status = await response.json();
    return {
      ok: true,
      inProgress: !!status.setupInProgress,
      lastError: typeof status.lastError === "string" && status.lastError !== "" ? status.lastError : null,
      lastRefresh: typeof status.lastRefresh === "string" ? status.lastRefresh : null,
    };
  } catch {
    return unknown;
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Did a successful finish land between the two probes?
 *
 * Compared server stamp against server stamp — the baseline is read from the
 * same endpoint before the finish starts — so no browser/server clock skew can
 * turn an old stamp into a fresh one.
 *
 * Both "no stamp now" and "no trustworthy baseline" answer false. Without a
 * baseline the server may have carried that stamp all along, and the cost of
 * the two directions is not symmetric: falsely claiming a save sends the user
 * away believing they are signed in.
 */
function cookieSetupRefreshAdvanced(baseline, probe) {
  if (!probe.lastRefresh) return false;
  if (!baseline || !baseline.ok) return false;
  return probe.lastRefresh !== baseline.lastRefresh;
}

/**
 * Word the alert shown when the client stops waiting for a finish.
 *
 * The old copy asserted "Cookie extraction timed out" — a claim this side
 * cannot make. FinishSetup writes the merged cookies.txt and reloads the jar
 * BEFORE it verifies, so the 60 s abort can fire over work that has already
 * committed, and telling the user it timed out invites them to redo a sign-in
 * that succeeded.
 *
 * Four arms, because a freed setup slot is not one outcome. In descending
 * order of what the server actually told us:
 *
 *   - it recorded an error → say what it said. This is the 422 that refused to
 *     overwrite an unreadable cookies.txt, and the empty profile that found no
 *     login, both of which free the slot exactly like a success does.
 *   - it stamped a new lastRefresh → the finish completed and committed.
 *   - neither → the slot is free and nothing was recorded. Three failure paths
 *     look like this (the MkdirAll, atomic-write and jar-reload returns), and
 *     none of them saved anything, so this must not claim a save.
 *   - the probe could not answer → say only that.
 *
 * `wizard` adds the one thing the setup wizard cannot do on this path: it
 * cannot mark a platform configured, because nothing here says WHICH platform
 * the completed finish accepted, and cookieYTDone/cookieTWDone are the sole
 * source of the active_platforms the wizard is about to write. Inferring it
 * from /api/status would read the answer off RefreshService's own check — a
 * different mechanism — so the alert says so instead of guessing.
 *
 * The 60 s cap is NOT the thing to change here: the server-side setup grace
 * window is priced against that exact constant, so raising it to buy a slow
 * finish more time would silently break a cross-language relationship. Report
 * what the server says instead.
 */
export function cookieSetupAbortReport(probe, baseline, { wizard = false } = {}) {
  if (!probe.ok) {
    return {
      message: "Moombox stopped waiting after 60 seconds and could not reach the server to find out whether " +
        "the setup finished. The browser window may still be open.",
      variant: "warning",
      icon: "question-circle",
    };
  }
  if (probe.inProgress) {
    return {
      message: "Moombox stopped waiting after 60 seconds. The server is still working on this setup, " +
        "and the browser window may still be open.",
      variant: "warning",
      icon: "clock",
    };
  }
  if (probe.lastError) {
    // Deliberately not attributed to "this setup". The slot is free by the
    // time we ask, so in principle the periodic refresh could have recorded
    // the message in between — reporting it as the last thing recorded is true
    // either way, and is the sentence that actually helps.
    return {
      message: "Moombox stopped waiting after 60 seconds. The setup is no longer running, and the last " +
        `thing the server recorded was: ${probe.lastError}`,
      variant: "danger",
      icon: "exclamation-octagon",
    };
  }
  if (cookieSetupRefreshAdvanced(baseline, probe)) {
    return {
      message: "Moombox stopped waiting after 60 seconds, but the server had already finished this setup — " +
        "the cookies it extracted were saved." +
        (wizard
          ? " This wizard cannot tell which platform it accepted, so it will not mark one as configured " +
            "here — press Try Again, or set cookies up from Settings once setup is done."
          : " Check the cookie status, and try again only if it still shows no sign-in."),
      variant: "primary",
      icon: "info-circle",
    };
  }
  return {
    message: "Moombox stopped waiting after 60 seconds. The setup is no longer running on the server, but it " +
      "recorded neither a result nor an error — nothing may have been saved. Check the cookie status " +
      "before signing in again.",
    variant: "warning",
    icon: "question-circle",
  };
}
