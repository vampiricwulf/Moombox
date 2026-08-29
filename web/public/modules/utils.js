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
 * What a browser-path validation attempt means for the save in progress.
 *
 * THREE outcomes, and the middle one is the whole point. The save used to have
 * two: `await resp.json()` with no `ok` check, then `if (!validateResult.valid)`
 * — so a 429 off the heavy rate limiter, a 400, or a proxy's HTML error page all
 * produced an object with no `valid` key, rendered as "Invalid browser:
 * undefined", and RETURNED. Every unrelated setting the user had just edited in
 * the same form went with it. "The server could not check the path" is not
 * "the path is wrong", and it must not cost the user their other edits.
 *
 * `block` is reserved for the one answer that earns it: the server ran the check
 * and said no. Everything else — unreachable, non-200, unparseable, or a 200
 * carrying no verdict — warns and lets the save through.
 *
 * Letting it through is not unguarded, but it is not equivalent either, and the
 * difference is worth stating exactly. PATCH /api/config runs
 * ValidateBrowserPathQuick on browser_path and rejects the save with a field
 * error, so a path that is missing, relative, not a regular file, not
 * executable, or of an unknown browser type still cannot be stored. What the
 * backstop does NOT run is the `--version` probe this endpoint adds — so a real
 * executable that is not actually that browser CAN now be stored when the
 * pre-check could not run. That failure surfaces later as a legible refresh
 * error; the alternative cost the user every other edit in the form.
 *
 * Written as a pure function over `{reached, body, detail}` so it can be RUN in
 * a test — saveConfig is DOM-coupled and cannot be. The comparison against
 * `valid` is POSITIVE in both directions (`=== false` blocks, `=== true`
 * passes); a `!body.valid` shorthand would fold the missing-key case back into
 * the blocking arm, which is the defect this exists to remove.
 */
export function browserPathValidationOutcome({ reached = false, body = null, detail = "" } = {}) {
  if (!reached) {
    return {
      block: false,
      variant: "warning",
      message: `Could not check the browser path (${detail || "no answer from the server"}). Saving anyway — the server runs its own check on the path when it stores it.`,
    };
  }
  if (body && body.valid === false) {
    return {
      block: true,
      variant: "danger",
      message: `Invalid browser: ${body.error || "the server did not say why"}`,
    };
  }
  if (body && body.valid === true) {
    return { block: false, variant: "success", message: "Path validated." };
  }
  return {
    block: false,
    variant: "warning",
    message: "Could not check the browser path (the server answered without a verdict). Saving anyway — the server runs its own check on the path when it stores it.",
  };
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
 *   lastRefresh — the server-side RFC3339 stamp a successful finish writes
 *                 (autocookies.go:1055, immediately before its final cleanup;
 *                 every FinishSetup failure path returns before that line).
 *
 * lastRefresh is NOT written only by a finish, and it would be wrong to reason
 * about the comparison as though it were. Three writers exist: the successful
 * finish; a refresh pass that actually renewed (:1910); and the sidecar restore
 * at construction (:368). What makes the comparison safe is not exclusivity of
 * the field but MUTUAL EXCLUSION OF THE WRITERS — StartSetup refuses to start
 * while `refreshCmd != nil` (:695) and RefreshCookiesDetailed declines while
 * `setupInProgressLocked()` (:1469) — so no refresh can stamp between the
 * baseline and the probe for as long as the setup holds the slot. The residual
 * window is between the finish's own cleanup and this probe, which is the same
 * window the lastError arm hedges its attribution for.
 *
 * Never throws, so a caller may hold the promise unawaited. That licence is
 * load-bearing (the baseline probe is held unawaited) and is pinned by a
 * behaviour test, not by the shape of this catch.
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
 * FIVE arms, because a freed setup slot is not one outcome. In descending
 * order of what the server actually told us:
 *
 *   - the probe could not answer → say only that.
 *   - the slot is still held → the finish is still running.
 *   - it recorded an error → say what it said. This is the 422 that refused to
 *     overwrite an unreadable cookies.txt, and the empty profile that found no
 *     login, both of which free the slot exactly like a success does.
 *   - it stamped a new lastRefresh → the finish completed and committed.
 *   - neither → the slot is free and nothing was recorded. Reached by the
 *     MkdirAll, atomic-write and jar-reload returns (the three that cleanup
 *     without a setError), by both early returns — no setup in progress, and
 *     cancelled — and by the SERVER-SIDE REAP of an abandoned setup, which on
 *     this path is not exotic: the grace window and the client cap are both
 *     60 s. None of them saved anything, so none may claim a save.
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

/**
 * How each platform words the state "this install was never configured".
 *
 * The asymmetry is deliberate and predates this helper: YouTube without cookies
 * is a warning, because almost everything Moombox does with YouTube wants them;
 * Twitch without cookies is the ordinary anonymous mode and gets the neutral
 * "off" dot. Keeping it as data is what lets one function serve both badges.
 */
const COOKIE_INDICATOR_PLATFORMS = {
  youtube: {
    name: "YouTube",
    absent: { className: "indicator-warn", title: "YouTube: No cookies" },
    // Which key of the platform's status payload carries WHY an inconclusive
    // check could not conclude. See CookieStatusPayload / TwitchAuthStatus-
    // Payload in internal/web/routes/cookies.go — the two halves land on
    // differently-named keys because they come off one AuthStatus.
    errorKey: "youtubeError",
  },
  twitch: {
    name: "Twitch",
    absent: { className: "indicator-off", title: "Twitch: Anonymous" },
    errorKey: "twitchError",
  },
};

/**
 * The job status a park lands on. database.StatusCookies in Go
 * (internal/database/types.go); app.js carries its own copies for the action
 * sets. Pinned against the Go constant by TestWebParkedStatusMatchesGo.
 */
const PARKED_JOB_STATUS = "COOKIES?";

/**
 * Which platforms have at least one job parked in COOKIES?, as
 * {youtube, twitch}.
 *
 * THE WEB HALF of the TUI's parkedCookieJobs (internal/tui/status_bar.go),
 * ported deliberately rather than re-derived, so the two dashboards attribute
 * the same alarm to the same platform:
 *
 *   - PER PLATFORM. A single unfiltered flag is what the TUI shipped first, and
 *     it reddened YouTube for a parked TWITCH job — an alarm pointed at the
 *     platform that was fine, which sends the operator to re-export credentials
 *     that were never the problem.
 *   - An ABSENT platform counts as YouTube, matching the TUI and matching every
 *     other platform test in app.js (`job.platform === "twitch"`, everything
 *     else is YouTube). Pre-Twitch rows really do carry "", and the importer
 *     backfills exactly "youtube" when it meets one.
 *   - NO ParkReason FILTER, also matching the TUI. Membership, dead
 *     credentials and the pre-v18 zero value all escalate, because in all three
 *     the remedy is credentials of some kind. What the red badge means is "a
 *     download stopped for want of usable credentials", not "your cookies
 *     expired"; the job row's own progress text carries the difference.
 *
 * Pure over the jobs already in memory — no fetch — and it stops as soon as
 * both platforms are seen, because it runs on every job event.
 */
export function parkedCookiePlatforms(jobs) {
  const parked = { youtube: false, twitch: false };
  for (const job of jobs || []) {
    if (job?.status !== PARKED_JOB_STATUS) continue;
    if (job.platform === "twitch") parked.twitch = true;
    else parked.youtube = true;
    if (parked.youtube && parked.twitch) break;
  }
  return parked;
}

/**
 * Decide the dashboard indicator for one platform: {className, title}.
 *
 * FOUR states where there were three, and the new one is the whole point. The
 * old chain ended `else -> "Not verified"`, in red, which is what a transient
 * network fault rendered as: the server reports `authenticated: false` for a
 * check that could not reach the site, exactly as it does for one the site
 * rejected, and the badge could not tell them apart. `verification` is the
 * field that can — "ok", "failed" or "unknown" — and only "failed" is a
 * conclusion about the credentials.
 *
 * TWO PROPERTIES CARRY THE ADDITIVE CONTRACT, and both are about a NEWER
 * FRONTEND AGAINST AN OLDER BINARY, which emits neither `verification` nor (on
 * the Twitch side) `found`:
 *
 *   - the comparison is POSITIVE (`=== "unknown"`). Written as `!== "ok"` a
 *     missing field would hedge about every platform on every older build.
 *   - `authenticated` is tested BEFORE `found`. Twitch's payload carried no
 *     `found` key at all until this arc, so an older binary leaves it
 *     undefined; ordering the absence test first would render a working,
 *     authenticated Twitch session as "Anonymous".
 *
 * The relogin arm stays first, and it is the caller's flag to compute because it
 * arrives on a different key of the status payload than the per-platform check
 * result the rest of this function reads — NOT because the caller filters it.
 * It used to be conjoined with the auto_enabled config flag at the call site,
 * and that gate is gone: "a human must sign in again" is exactly as true, and
 * exactly as actionable, for an install that maintains cookies.txt by hand. Do
 * not reintroduce it here or there — the TUI has never had one, and the two
 * surfaces are supposed to agree. See updateStatusBar in app.js.
 *
 * `parked` is the fourth state and the Web half of a divergence: a job stopped
 * in COOKIES? for want of usable credentials. It ranks SECOND, immediately
 * under re-login and above `authenticated`, which is exactly where the TUI
 * ranks it (renderCookieStatus, internal/tui/status_bar.go) and for the TUI's
 * stated reason — a park is evidence from a real download attempt that
 * something tried these credentials and could not proceed, and it therefore
 * outranks a check that merely asked. Ranking it below `authenticated` would
 * hide it behind a healthy check, which is the state it exists to contradict.
 * It comes from the caller because it is computed from the job list rather than
 * from the status payload; see parkedCookiePlatforms.
 *
 * The reason line is `youtubeError` / `twitchError` off the same payload, and
 * it is appended ONLY to the inconclusive arm. That is the one verdict a reason
 * explains: a conclusive "ok" or "failed" has no cause to give, and the
 * producers leave the field empty there. An older binary sends no such key at
 * all and the title degrades to exactly today's sentence.
 */
export function cookieIndicatorState(platform, status, reloginRequired, parked) {
  const meta = COOKIE_INDICATOR_PLATFORMS[platform];
  if (reloginRequired) {
    return { className: "indicator-error", title: `${meta.name}: Re-login required` };
  }
  if (parked) {
    return {
      className: "indicator-error",
      title: `${meta.name}: A download stopped for want of usable credentials`,
    };
  }
  if (status?.authenticated) {
    return { className: "indicator-ok", title: `${meta.name}: Authenticated` };
  }
  if (!status?.found) {
    return meta.absent;
  }
  if (status?.verification === "unknown") {
    const reason = status?.[meta.errorKey];
    return {
      className: "indicator-warn",
      title: `${meta.name}: Cookies saved — Moombox could not establish whether they work`
        + (reason ? ` (${reason})` : ""),
    };
  }
  return { className: "indicator-error", title: `${meta.name}: Not authenticated` };
}

/**
 * Word the toast for a manual cookie recheck: {message, variant}.
 *
 * THE SAME GESTURE AS THE TUI'S R C CHORD, and it was answering it
 * differently. This toast said "Cookies refreshed successfully" or "Cookie
 * check completed", keyed on `data.success` — which is
 * `youtubeAuthenticated || twitchAuthenticated`, and therefore false for a
 * check that never reached the site. So the one gesture whose entire purpose
 * is "tell me what my credentials are doing" answered with a sentence that
 * named neither the platform nor the finding, in the arc that taught every
 * other surface to say exactly that.
 *
 * `message` is deliberately NOT worded here. It is rendered by
 * cookies.RecheckReport in Go and reproduced below character for character,
 * pinned by a test that runs this function and compares against the Go output
 * — the same discipline RefreshDeclinedCauses is held to, because "the two
 * UIs answer the same question the same way" is a property that decays the
 * moment one side is edited alone.
 *
 * `variant` is web-only (Shoelace has no counterpart in the TUI) and ranks
 * the outcomes: a conclusive failure is the thing to act on, so it outranks a
 * check that concluded nothing, which in turn outranks a clean pass.
 *
 * The `default` arm is the additive contract, and it degrades to the
 * UNQUALIFIED legacy copy rather than to the hedged one — same rule
 * cookieIndicatorState follows. An older binary emits no `verification` at
 * all, and hedging about every recheck on every older build would be a new
 * wrong answer in place of the old one.
 */
export function cookieRecheckToast(activePlatforms, cookieStatus, twitchAuthStatus, success) {
  // The legacy two-arm copy, reached only where this function cannot word a
  // truthful answer. Written once so the fallback cannot drift from itself.
  const legacy = () => ({
    message: success ? "Cookies refreshed successfully" : "Cookie check completed",
    variant: success ? "success" : "primary",
  });

  // MISSING is not EMPTY. An absent activePlatforms means we do not know which
  // platforms are configured; an empty one means none are, and only the second
  // may be reported as such.
  if (!activePlatforms) return legacy();

  const rows = [
    ["youtube", "YouTube", cookieStatus],
    ["twitch", "Twitch", twitchAuthStatus],
  ].filter(([key]) => activePlatforms[key] === true);

  const parts = [];
  let failed = false;
  let unestablished = false;
  for (const [, label, status] of rows) {
    switch (status?.verification) {
      case "ok":
        parts.push(`${label} OK`);
        break;
      case "failed":
        parts.push(`${label} not authenticated`);
        failed = true;
        break;
      case "unknown":
        parts.push(`${label} — could not establish`);
        unestablished = true;
        break;
      default:
        return legacy();
    }
  }

  if (parts.length === 0) {
    return { message: "Cookies: no platforms configured", variant: "neutral" };
  }
  return {
    message: "Cookies: " + parts.join(", "),
    variant: failed ? "danger" : unestablished ? "warning" : "success",
  };
}
