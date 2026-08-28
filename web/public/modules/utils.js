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
 * Ask the server whether it is still holding the interactive setup slot.
 *
 * Returns true, false, or null when the probe itself could not answer — the
 * third value matters, because "we do not know" is the one thing the abort
 * path must not round to a conclusion.
 */
export async function cookieSetupStillRunning() {
  try {
    const response = await fetch("/api/cookies/auto-status");
    if (!response.ok) return null;
    const status = await response.json();
    return !!status.setupInProgress;
  } catch {
    return null;
  }
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
 * The 60 s cap is NOT the thing to change here: the server-side setup grace
 * window is priced against that exact constant, so raising it to buy a slow
 * finish more time would silently break a cross-language relationship. Report
 * what the server says instead.
 */
export function cookieSetupAbortReport(stillRunning) {
  if (stillRunning === false) {
    return {
      message: "Moombox stopped waiting after 60 seconds, but the server had already finished this setup — " +
        "anything it extracted was saved. Check the cookie status, and try again only if it still shows no sign-in.",
      variant: "primary",
      icon: "info-circle",
    };
  }
  if (stillRunning === true) {
    return {
      message: "Moombox stopped waiting after 60 seconds. The server is still working on this setup, " +
        "and the browser window may still be open.",
      variant: "warning",
      icon: "clock",
    };
  }
  return {
    message: "Moombox stopped waiting after 60 seconds and could not reach the server to find out whether " +
      "the setup finished. The browser window may still be open.",
    variant: "warning",
    icon: "question-circle",
  };
}
