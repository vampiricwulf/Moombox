# Cookies Subsystem Audit

## Overview

The `internal/cookies` package (~2,739 lines across 9 files) is Moombox's authentication bridge for YouTube and Twitch. It has three responsibilities:

1. **Cookie jar** (`jar.go`) — parses Netscape-format cookie files, extracts essential YouTube/Google/Twitch cookies, and generates SAPISIDHASH `Authorization` headers for YouTube API requests.
2. **Auto-cookie extraction** (`autocookies*.go`) — launches a real browser (Chrome/Edge/Brave/Opera/Firefox/Waterfox) for the user to log in, then extracts cookies via **Chrome DevTools Protocol (CDP)** for Chromium-family browsers or by reading `cookies.sqlite` directly for Firefox-family browsers.
3. **Refresh service** (`refresh.go`) — every 30 min by default, reloads cookies, pings `youtubei/v1/guide` + `id.twitch.tv/oauth2/validate` to validate auth, and merges `Set-Cookie` headers back into the Netscape file. Triggers `OnRecoveryNeeded` when auth transitions from authenticated→not-authenticated.

A key architectural distinction from yt-dlp: **Moombox does NOT decrypt cookies directly**. There is no DPAPI / AES-GCM / keyring code — Chromium cookies are fetched from a running browser via CDP, and Firefox's `cookies.sqlite` is read with the app's own writable profile directory. This is a clean and safe choice (no v10/v11 cipher churn, no CNG dance) but it bears on several of the findings below (discovery scope, browser-lock conflicts, UX).

Most logic is Windows-specific even though many files compile cross-platform. Per `CLAUDE.md`, Windows is the only supported target.

## Implementation Status (as of 2026-04-23)

| ID | Status | Notes |
|----|--------|-------|
| 1 | Done | sort.Strings (jar.go:221); test.6 |
| 2 | Done | sanitizer strips `;` `,` (jar.go:198); test.6 |
| 3 | Done | cdpEnsurePageTarget reuses any existing page target (navigates it to platform URL) or spawns a new tab via Target.createTarget on the browser-level WS; called from extractChromiumCookies before cdpGetCookiesAsNetscape |
| 4 | Done | every matching row updated (refresh.go:617-628); test in refresh_test.go |
| 5 | Done | subdomain flag derived from leading dot (refresh.go:657-660); test |
| 6 | Done | per-platform error message (autocookies.go:446-469) |
| 7 | Done | test.16 — added LibreWolf, Zen, Vivaldi, Thorium |
| 8 | Done | test.16 — Job Object wired into Chromium setup + refresh |
| 9 | Done | removeStaleLock skips locks with mtime < 5s (autocookies_chromium.go, autocookies_firefox.go) |
| 10 | Dismissed | refreshCmd field is dual-purpose (slot claim + actual cmd ref for kill-tree); the slot-claim path operates under s.mu.Lock() so it's race-free under the mutex. The audit's earlier "still racy under specific Stop() timing" concern was addressed by killRefreshProcess polling for the sentinel→real-cmd transition (audit #22). Refactoring to atomic.Bool would split the dual role across two fields with no net safety gain. |
| 11 | Done | cleanup() called on waitForCDP failure (autocookies_chromium.go:90-91) |
| 12 | Done | allowlist of YouTube origins (jar.go:272-278); test |
| 13 | Done | refresh launch mirrors anti-automation flags + window-size (autocookies_chromium.go:121-137) |
| 14 | Done | wait goroutine guards browserExited write with proc-pointer match (chromium + firefox setup) |
| 15 | Done | test.16 — `FlexDuration.AsDuration` helper, fixed cmd/moombox/main.go:1145 |
| 16 | Done | parts[6:] joined with tab; malformed-line Debug log (jar.go:107-114, 124-127); tests |
| 17 | Done | full fallback chain + explicit error if all empty (autocookies_chromium.go:347-427) |
| 18 | Done | navCtx.Err() distinguishes timeout vs error (autocookies_chromium.go:293-307) |
| 19 | Dismissed | cdpSendCommand has only one caller (cdpCloseBrowser → Browser.close) which intentionally doesn't care about the response. The post-write conn.Read briefly drains the ack so the WebSocket flushes before defer Close races the server. Existing godoc at lines 524-530 documents this design. Plumbing a logger would only help when Browser.close fails — which is harmless because we kill the process tree anyway. |
| 20 | Done | `_busy_timeout=2000` (autocookies_firefox.go:191) |
| 21 | Done | nil callback Warns and falls back to presence-only result (autocookies.go:270, 279) |
| 22 | Done | killRefreshProcess polls up to 2s for sentinel-to-real-cmd transition (autocookies.go) |
| 23 | ✅ Done (active-job half) | `AutoCookieService` gains an optional `HasActiveJobs func() bool` callback (`autocookies.go:82-89`); `StartPeriodicRefresh` checks `shouldSkipPeriodicRefresh()` per tick (`autocookies.go:521-523`) and skips the headless-Chrome launch when no Live/Downloading jobs exist. Wired in `cmd/moombox/services.go:340-353` to `db.GetJobStats().ActiveCount > 0` (uses the cached stats query so the per-tick check stays cheap; nil-stats fallback returns true so a transient DB hiccup doesn't drop refreshes). 2 unit tests in `autocookies_periodic_test.go`. mtime half (b) of the audit recommendation is deferred — the active-job half captures the headline savings for idle sessions. |
| 24 | Done | fallback string capped at authBodyFallbackLimit=16KB at both call sites (refresh.go) |
| 25 | Done | utils.ApplyUserOnlyDACL applied at autocookies.go:277 (commit d0df3c9). |
| 26 | Done | validateBrowserProfileDir + dangerousProfilePathSubstrings (autocookies.go:42-95). Refuses paths under known browser-profile trees. 18-pattern dangerous-substring list + 21 unit-test cases. Commit 1d7cff1. |
| 27 | Done | telemetry uploads + reports disabled (autocookies_firefox.go:25-26) |
| 28 | Done | maps swapped only after read succeeds (jar.go:78-90, 164-168); test |
| 29 | Done | timestamp captured once via `now` (jar.go:290) — finding noted no-action; locked in |
| 30 | Done | test.16 — Vivaldi + Thorium added to knownBrowsers |
| 31 | Done | test.16 — LibreWolf + Zen added; isFirefoxBased covers them (autocookies_detect.go:38-44) |
| 32 | Dismissed | audit marked as document-as-non-goal |
| 33 | Dismissed | audit marked Safari out-of-scope |
| 34 | Done | helpers `isYouTubeDomain`/`isGoogleDomain`/`isTwitchDomain`/`isRelevantDomain` (autocookies_merge.go:30-42) |
| 35 | Done | youtubeGuideRequestBody helper used at both call sites (refresh.go:317, 341, 428) |
| 36 | Dismissed | The two functions serve different goals: closeFirefoxGracefully sends a graceful signal and waits for browserExited (Firefox-specific cleanup path); killProcessTree forces taskkill /T /F on a process tree as a fallback. Unifying them would conflate "graceful" with "forceful" semantics — the diff is by design. |
| 37 | Dismissed | audit marked doc-only |
| 38 | Done | cdpExtractTimeout / cdpRefreshTimeout / cdpNavigateTimeout consts added (autocookies_chromium.go) |
| 39 | Done | suffix-anchored matchers via domainMatches (autocookies_merge.go:24-37); tests |
| 40 | Done | Set-Cookie Domain= captured authoritatively (refresh.go:533-536, 545-557) |
| 41 | Done | fmt.Fprintf error checked (refresh.go:666-669) |
| 42 | Dismissed | The two discard-reads (Page.enable response in cdpNavigateAndWait, post-write ack in cdpSendCommand) are both intentional. The navigate-loop filters by message type so an unconsumed response is harmless; the Browser.close ack lets the WebSocket flush before Close races the server. Both godoc'd at the call site. |
| 43 | Done | browserDetectCache TTL + uncached helper (autocookies_detect.go:75-87, 91); tests use seam |
| 44 | Deferred | Migrating AutoCookieReloginRequired{YouTube, Twitch bool} → map[string]bool would change the JSON wire shape consumed by the Web UI + TUI. The audit's concern (adding a third platform requires editing the struct) is moot for current scope — there are only YouTube and Twitch supported, and DECISIONS #33 dismisses multi-account. Re-open if a third platform is ever added. |
| 45 | Done | killProcessTreePollDelay (autocookies.go:34) and firefoxLaunchSpacing (autocookies_firefox.go:42) replace the last two inline literals (50ms and 5s respectively). All cookie-package time.Sleep calls now reference named consts with godoc rationale. |
| 46 | Done | glob `Singleton*` / `*lockfile*` (autocookies_chromium.go:571-579) |
| 47 | Deferred | ~80% logic overlap is real but the two flows diverge meaningfully: FinishSetup runs after a USER-initiated browser sign-in (interactive, blocking on user action); RefreshCookies runs UN-attended in headless mode with anti-automation flags. Extracting shared helpers requires careful boundary work to avoid leaking the "user is watching" assumption. Substantive refactor — defer to a focused session. |
| 48 | Deferred | Persisting LastRefresh across restarts is a feature add, not a bug fix. Without it, periodic refresh fires immediately on each restart — slightly wasteful but not incorrect. Re-open if telemetry shows the wasted refreshes are a real cost. |
| 49 | Dismissed | DECISION #33 — multi-account not planned |
| 50 | Deferred | cleanChromiumLockFiles and cleanFirefoxLockFiles already proactively scrub the lock files before launch. Reactive detection of "ProfileInUse" requires inspecting browser stderr or polling for specific exit codes per browser-family — substantial effort with low marginal value over the proactive cleanup. Defer until a real user incident shows the proactive path missing a case. |
| 51 | Done | `setupCmd` field removed (test.7 batch) |
| 52 | Done | test.16 — anonymous Job Objects, jobCounter dropped |
| 53 | Dismissed | audit marked doc-only |
| 54 | Done | merge_test.go covers isRelevantDomain, isEssentialCookie, deduplicateAndFormat, mergeCookieFiles |
| 55 | Done | refresh_transitions_test.go covers SetExpectedPlatforms seeding, GetStatus snapshot, OnAuthChange callback diff (auth-flag changes only), and the OnRecoveryNeeded vs OnAuthRecovered transition contract via direct state manipulation (avoids needing network seams). |
| 56 | Done | refresh_test.go covers updateCookieFile (duplicate rows, subdomain flag, fallback) |
| 57 | Done | test.16 — parseDefaultBrowserProgID extracted with 15 fixture cases |
| 58 | Done | TestMakeSidAuthorizationKnownVector locks down hash (jar_test.go:188-197) |
| 59 | Done | test.16 — TestCookieHeaderDeterministicOrder (jar_test.go:96-130) |
| 60 | Dismissed | Audit explicitly marked acceptable — Job Object code is Windows-syscall-bound and unit testing would need a mock CreateJobObject syscall. Functional verification via end-to-end smoke runs is the documented strategy. |

Cross-references: Q1 owner question → DECISIONS #6 (DPAPI fallback) is **Deferred**. Q4 owner question → DECISIONS #23 (`COOKIES?` auto-retry) is **Done** — `OnAuthRecovered` sweeps parked jobs (refresh.go:83-86, 278-287). Q6 → DECISIONS #33 multi-account **Dismissed** (already reflected in finding #49).

## Critical Issues

### 1. [High] CookieJar.GetCookieHeader iterates over a map — non-deterministic Cookie header order
- **File:** `internal/cookies/jar.go:153-162`
- **What:** `GetCookieHeader()` builds pairs by ranging over `j.cookies` (a `map[string]string`). Go deliberately randomizes map iteration order.
- **Why:** YouTube and Twitch do not strictly require a particular cookie order, but three concrete problems arise from this:
  1. Reproducibility of the SAPISIDHASH flow is harder to reason about across consecutive requests because `Cookie:` header changes identity every time the request is retried, even when cookie content is stable.
  2. Some YouTube endpoints have been observed to reject requests when certain `__Secure-*` cookies appear before their non-`__Secure-` counterparts (yt-dlp serializes in a canonical order).
  3. A Cookie header whose content changes between attempts defeats HTTP-level debugging and cache tooling.
- **Fix:** Sort names before joining (e.g. `sort.Strings(names)`), preferring a `__Secure-*` last, `auth-token`/`SAPISID` first order consistent with what most browsers emit.
- **Effort:** 10 min.

### 2. [High] CookieJar.sanitizeCookieValue does not strip cookie separators `;` or `,`
- **File:** `internal/cookies/jar.go:144-150`
- **What:** `cookieValueSanitizer` only replaces `\r` and `\n`. A cookie value that legitimately (or maliciously, if someone tampers with the file) contains `;` will break the Cookie header — everything after the `;` is parsed as the next name/value by intermediaries.
- **Why:** The YouTube CONSENT cookie and some Google cookies commonly carry `:`, but a stray `;` in the raw extracted value would split the Cookie header silently. This also weakens header-injection defense if a malicious cookie file is placed in the profile.
- **Fix:** Also strip `;` and `,` (Netscape spec forbids them in values anyway), or reject the cookie entirely and log a Warn. Same fix should be mirrored in the `value` handling in `updateCookieFile` (refresh.go:559-561).
- **Effort:** 10 min.

### 3. [High] extractChromiumCookies never ensures a YouTube / Twitch page was opened before extraction
- **File:** `internal/cookies/autocookies_chromium.go:74-87`
- **What:** `extractChromiumCookies()` issues `Storage.getCookies` at the browser-level immediately when `FinishSetup` is called, relying on the user having navigated to the platform in the visible browser. If the user logs in, closes the tab, and only has about:newtab open when clicking Finish, the browser-level `Storage.getCookies` may still have them — but the Network.getCookies fallback uses a hardcoded URL list that misses youtube.com sub-origins used by member-only checkout flows.
- **Why:** `Storage.getCookies` was deprecated years ago in newer Chrome versions; the fallback path (`autocookies_chromium.go:286-337`) relies on there being a `page` target, which won't exist if the user closed all tabs.
- **Fix:** Before extraction, call `Target.createTarget` via CDP with `https://www.youtube.com/` (or twitch.tv based on `s.targetPlatform`), then wait for load, then extract. This also removes the dependency on the user leaving the login page open.
- **Effort:** 1 hr.

### 4. [High] RefreshService.updateCookieFile can insert duplicate cookies on legacy files
- **File:** `internal/cookies/refresh.go:577-596`
- **What:** When a cookie name exists multiple times in the existing file (e.g. once on `.youtube.com` and once on `.google.com`), the scanner updates the first occurrence it encounters in the order of the `Set-Cookie` headers and then stops because `updated[name]=true`. Subsequent duplicate rows for that same name in the file are left stale with the old value. The "add new cookies" step then does nothing because `updated[name]` is already true.
- **Why:** This creates silent cookie drift — a stale `SAPISID` on `.google.com` can linger and override the fresh `.youtube.com` version depending on which is parsed first by `CookieJar.Load`, because jar load uses the `youtube.com`-preferring logic but only for deduplication (first write wins based on domain preference, not on recency).
- **Fix:** Either (a) update every matching row, (b) delete all matching rows during the scan and append a single fresh row at the end, or (c) parse, mutate via the map, and re-serialize canonically as `mergeCookieFiles` does.
- **Effort:** 45 min.

### 5. [High] updateCookieFile writes WithoutSubdomain flag as "TRUE" regardless of domain dot prefix
- **File:** `internal/cookies/refresh.go:591-593`
- **What:** Netscape format's second field is "include subdomains". The code hard-codes `TRUE` for every new cookie. For YouTube session cookies that's fine (leading `.`), but for cookies like `login` (no leading `.`) it writes an incorrect row, then the jar re-load filters them out anyway because it only cares about domain substring. However, when `mergeCookieFiles` later re-parses, a cookie whose domain lacks the dot but whose subdomain flag is TRUE is mildly malformed per spec.
- **Fix:** Set subdomain flag based on `strings.HasPrefix(domain, ".")`.
- **Effort:** 5 min.

### 6. [High] RefreshCookies does not clear `s.lastError` on a successful-but-partial run
- **File:** `internal/cookies/autocookies.go:428-450`
- **What:** If `yt OR tw` verification succeeds, `lastError` is cleared (line 432). But if neither succeeds, `setError(errMsg)` is called with a generic message — and any *prior* successful refresh's `lastError: nil` is overwritten even though the refresh itself completed without technical errors. The UI shows "manual re-login required" even when the cookie file was actually written correctly; the user is told nothing about which platform verified vs. failed.
- **Why:** Affects UX of the `COOKIES?` recovery path — users see a scary error toast instead of a targeted "YouTube auth expired" message.
- **Fix:** Track per-platform success and report a structured error (e.g. `"YouTube ok, Twitch needs re-login"`), or only set error when no platform has verifiable cookies.
- **Effort:** 30 min.

### 7. [High] DetectBrowser promotes default browser but inspects HKCU only — misses Windows default app changes
- **File:** `internal/cookies/autocookies_detect.go:140-176`
- **What:** Runs `reg query HKCU\...\https\UserChoice`. This is the correct registry path for the user's current UrlAssociations, but when a user has never set a default https handler, or has a default set via GPO, the query returns empty and detection falls back to "Firefox > Waterfox > Chrome > Brave > Opera > Edge." Opera GX and Zen are missed entirely.
- **Why:** Users who installed Zen, Vivaldi, LibreWolf, Arc, or Firefox Developer Edition get no browser detected. Opera GX has its own `opera.exe` but `knownBrowsers` maps it to type "opera" only with the Opera GX path first — fine — but Vivaldi (`vivaldi.exe` / `Vivaldi\Application\vivaldi.exe`), Arc, LibreWolf, and Thorium are completely absent.
- **Fix:** Add at minimum: Vivaldi, Zen, LibreWolf, and Arc. yt-dlp's `CHROMIUM_BASED_BROWSERS = {'brave','chrome','chromium','edge','opera','vivaldi','whale'}` is the right baseline.
- **Effort:** 1 hr.

### 8. [Medium] Job Object termination is wired for Firefox refresh only — Chromium refresh leaks on crash
- **File:** `internal/cookies/autocookies_firefox.go:226-237` vs `internal/cookies/autocookies_chromium.go:108-122`
- **What:** `runWithTimeout` (Firefox path) creates a Windows Job Object with `KILL_ON_JOB_CLOSE` so all child processes die when Moombox exits. The Chromium refresh path (`refreshChromium`) does NOT use the Job Object — it only calls `killProcessTree(cmd.Process)` on defer. If Moombox crashes before defer runs, the headless Chromium stays alive indefinitely.
- **Fix:** Wire `newProcessJob` into `refreshChromium` and `startChromiumSetup` the same way Firefox does. Even better, create one job at service start and assign all launched browsers to it.
- **Effort:** 1 hr.

### 9. [Medium] cleanChromiumLockFiles / cleanFirefoxLockFiles unconditionally deletes files — no check that the browser isn't running
- **File:** `internal/cookies/autocookies_chromium.go:479-483` and `autocookies_firefox.go:213-217`
- **What:** Before launching, Moombox deletes `lockfile`, `SingletonLock`, `SingletonSocket`, `SingletonCookie` (Chromium) and `parent.lock`, `.parentlock` (Firefox). If a user has the browser-profile open in *another* instance (e.g. debugging, another headless run that didn't register with Moombox), the lock is deleted under them, leading to profile corruption warnings on next launch.
- **Why:** Under periodic refresh, two refreshes racing (one already running but mutex didn't catch it because it's a headless-without-the-mutex case during cold start) would delete each other's locks.
- **Fix:** Check that no other process has the profile open via `taskkill /FI "IMAGENAME eq firefox.exe"` or simply skip the delete if the file is younger than a threshold.
- **Effort:** 30 min.

### 10. [Medium] refreshCmd sentinel pattern leaks when RefreshCookies returns an early error
- **File:** `internal/cookies/autocookies.go:316-345`
- **What:** `s.refreshCmd = &exec.Cmd{}` is assigned at line 327 as a sentinel to claim the slot. The deferred cleanup at 329-333 always sets it back to nil, so this is fine in the steady state. However `refreshChromium` itself *reassigns* `s.refreshCmd = cmd` at line 112 (replacing the sentinel) and its own deferred `s.refreshCmd = nil` at line 119 overlaps with the outer defer. This race isn't harmful today (mutex ordering saves it), but the doubly-nested defers are fragile.
- **Fix:** Remove the sentinel from the outer layer and delegate slot-claiming to the per-browser function, or use a single atomic `refreshInProgress bool` flag.
- **Effort:** 30 min.

### 11. [Medium] waitForCDP prints timeout error but leaves the sentinel slot held
- **File:** `internal/cookies/autocookies_chromium.go:63-71`
- **What:** When `waitForCDP` fails, `startChromiumSetup` calls `killSetupProcess()` and returns err. But `setupProcess`, `setupCmd`, `cdpPort`, and `setupBrowser` may have been set on the struct in the interim. They are only cleared by `cleanup()`, which is not called here on the failure path.
- **Why:** Subsequent `StartSetup` calls fail with "setup already in progress" because `s.setupProcess != nil` even though the process is dead.
- **Fix:** Call `s.cleanup()` before returning the error.
- **Effort:** 10 min.

### 12. [Medium] SAPISIDHASH uses SHA-1 — matches Google's scheme but no defense for origin tampering
- **File:** `internal/cookies/jar.go:224-229`
- **What:** `makeSidAuthorization` hashes `timestamp + " " + sid + " " + origin`. Origin is always `https://www.youtube.com` in our codebase, but if origin is ever passed from external user input (settings wizard, future API), there is no validation that it's a known domain.
- **Why:** Accidental supply of a malformed origin (`"https://evil"`) would generate a valid SAPISIDHASH for an attacker-controlled origin.
- **Fix:** Either hardcode valid origins or validate that `origin` is in an allowlist (`youtube.com`, `youtubekids.com`, `youtubestudio`). The call sites already only pass youtube.com so this is defense-in-depth.
- **Effort:** 15 min.

### 13. [Medium] Chromium headless refresh uses `--headless=new` but does NOT pass --disable-blink-features
- **File:** `internal/cookies/autocookies_chromium.go:97-106`
- **What:** During setup (visible browser, line 35) `--disable-blink-features=AutomationControlled` is passed to hide the automation flag. During refresh (line 97) it's not passed, meaning youtube.com sees an automated browser and may rate-limit or increase fraud scores, invalidating the session cookies we're trying to refresh.
- **Fix:** Mirror the flags used in setup for refresh. Also consider `--window-size=1280,720` to mimic a real window.
- **Effort:** 15 min.

### 14. [Medium] BrowserExited status flag is stored but never reset across multiple setup attempts
- **File:** `internal/cookies/autocookies.go:534-544` and `autocookies_chromium.go:58-60`
- **What:** `s.browserExited = true` is set by the wait goroutine when the browser exits. `cleanup()` resets it to false. But if `StartSetup` is called after a successful `FinishSetup`, there's a narrow window between `cleanup()` clearing the flag and a new goroutine being spawned — during which `closeFirefoxGracefully()` of a *new* setup reads `browserExited` and thinks the previous browser exited. Not reachable in practice because mutexes serialize setup, but bug-prone.
- **Fix:** Scope the wait-goroutine state to a per-setup struct, or pair with a generation counter.
- **Effort:** 1 hr.

### 15. [Medium] Periodic refresh interval uses `Minutes()` inside `Duration` constructor — truncation
- **File:** `cmd/moombox/main.go:1028` (consuming), `internal/cookies/autocookies.go:465`
- **What:** `time.Duration(cfg.Cookies.RefreshInterval.Minutes()) * time.Minute`. `Minutes()` returns a float64; the cast to `Duration` truncates to int64 nanoseconds before the `* time.Minute` multiplication, so a user-configured `90s` becomes `1.5` → `1` → `1 minute`.
- **Why:** Any user who set the refresh interval via config sub-minute granularity silently loses fidelity. Admittedly 30-min default is fine, but the bug exists in the consumer of the cookies package.
- **Fix:** Use `cfg.Cookies.RefreshInterval` directly (it's already a Duration).
- **Effort:** 2 min (out-of-scope but surfaced for owner).

### 16. [Medium] GetCookieHeader silently drops cookies whose value contains tab — not handled by jar.Load either
- **File:** `internal/cookies/jar.go:75-120`
- **What:** `strings.Split(line, "\t")` treats a tab in the cookie value as a column separator. Netscape spec says values cannot contain tabs — but if `Set-Cookie` returns a base64 value that somehow has a tab due to non-printable chars, or if a user hand-edits the file, parsing silently produces wrong fields or skips the line. No warning is logged.
- **Fix:** When `len(parts) > 7`, the value is `strings.Join(parts[6:], "\t")`. When `< 7`, log `Debug` before skipping.
- **Effort:** 15 min.

### 17. [Medium] cdpGetCookiesAsNetscape reads 200 OK but doesn't verify Storage.getCookies response shape
- **File:** `internal/cookies/autocookies_chromium.go:286-344`
- **What:** If `Storage.getCookies` returns an empty `result` (permissions refused, no cookies granted), `cookieResult.Cookies` is a zero-length slice and the function returns a Netscape file with just headers and no cookies. No error, no warning. The downstream `s.jar.Load()` then fails silently to authenticate.
- **Fix:** If `len(cookieResult.Cookies) == 0`, try the `Network.getAllCookies` fallback chain anyway; if still empty, return an explicit error.
- **Effort:** 15 min.

### 18. [Medium] cdpNavigateAndWait silently eats errors from reading CDP events
- **File:** `internal/cookies/autocookies_chromium.go:234-248`
- **What:** The `for { ... conn.Read() }` loop returns `nil` on any error, claiming "page likely loaded." A network timeout here is indistinguishable from success.
- **Fix:** Distinguish context-done (return nil, page probably loaded) from read-error before any navigate response was received (return the error). At minimum log at Debug when an unexpected error is swallowed.
- **Effort:** 20 min.

### 19. [Medium] cdpSendCommand fire-and-forget — no deadline on failure and no per-call mutex means concurrent sends corrupt
- **File:** `internal/cookies/autocookies_chromium.go:391-414`
- **What:** `cdpSendCommand` and `cdpSendCommandWithResult` each open a fresh WebSocket connection. That is safe. But the 5-second read timeout on `cdpSendCommand` means if CDP is slow, the Browser.close message fires and forgets, and the browser may not actually close before we assume it did.
- **Fix:** Log at Debug whether the close ack was received. Not critical since we force-kill afterward.
- **Effort:** 10 min.

### 20. [Medium] Firefox cookies.sqlite read holds zero retry for "database is locked" specifically
- **File:** `internal/cookies/autocookies_firefox.go:146-165`
- **What:** The retry loop runs 5 attempts with 500ms backoff for *any* error. But WAL-mode SQLite returns the locked error after a timeout (default busy_timeout=0), which modernc's sqlite returns as text error "database is locked". The pre-query `?mode=ro` opens read-only, but WAL locks can still briefly occur if Firefox is mid-flush.
- **Fix:** Use `?mode=ro&_busy_timeout=2000` or `?_pragma=busy_timeout=2000` to let SQLite handle the wait. 5*500ms=2.5s total timeout is aggressive for a browser with a large cookie store.
- **Effort:** 10 min.

### 21. [Medium] FinishSetup does API verification but if callbacks are nil, returns true based on cookie presence alone
- **File:** `internal/cookies/autocookies.go:245-265`
- **What:** `ytAuth = s.jar.HasYouTubeAuthCookies()` — that check requires SAPISID AND LOGIN_INFO, but mere cookie presence doesn't mean the cookies are valid. If `VerifyYouTubeAuth` callback is nil, the user is told "verified" when all we know is the cookies exist.
- **Why:** In cmd/moombox the callback is always wired so this is moot. But `NewAutoCookieService` in tests / alternate wiring would silently succeed.
- **Fix:** If the verification callback is nil, log a warning and still report success, or expose an explicit "unverified" return value.
- **Effort:** 15 min.

### 22. [Medium] `Stop()` calls `killRefreshProcess` which uses `s.refreshCmd` — but sentinel has no Process
- **File:** `internal/cookies/autocookies.go:522-532`
- **What:** `killRefreshProcess` bails when `cmd.Process == nil`. During the narrow window between `RefreshCookies` setting `refreshCmd = &exec.Cmd{}` sentinel and the actual cmd being assigned (line 112), a `Stop()` call would see the sentinel with nil Process and return silently. Meanwhile the goroutine is running a Chromium launch that will not be killed.
- **Fix:** Use a `context.CancelFunc` slot instead of an exec.Cmd slot, so Stop() can cancel unconditionally.
- **Effort:** 30 min.

### 23. [Medium] Periodic refresh does not check if cookies file was touched externally (no mtime cache)
- **File:** `internal/cookies/autocookies.go:462-492`
- **What:** Refreshing cookies via a headless Chrome launch is expensive (1-5s, GPU init, memory). The ticker fires unconditionally. If the user hasn't actively used Moombox in hours and no job is pending auth, we still spawn a headless Chrome every 30 min.
- **Fix:** Skip refresh when (a) no downloading/live jobs and (b) cookie file mtime is newer than last refresh minus `interval/2`. Matches "lazy" cache invalidation per the audit brief.
- **Effort:** 45 min.

## Security Concerns

### 24. [High] Cookie values may appear in error messages / logs indirectly
- **File:** `internal/cookies/refresh.go:340`, `internal/cookies/autocookies.go:217`
- **What:** `io.ReadAll(io.LimitReader(resp.Body, 5<<20))` reads up to 5MB of the YouTube guide response. If JSON unmarshal fails (line 344), the fallback `strings.Contains(respStr, ...)` runs on the full body. If a downstream log ever Warns with the actual body string (currently they don't in this file), cookies/session tokens from Set-Cookie in the response body could leak. It's a latent hazard.
- **Fix:** Limit to first N KB when doing string fallback, redact `Set-Cookie:` and `Authorization:` patterns before any log.
- **Effort:** 30 min.

### 25. [Medium] Cookie file written with mode 0600 but tmp file created with WriteFile default then renamed
- **File:** `internal/cookies/autocookies.go:559-569`, `internal/cookies/refresh.go:600-606`
- **What:** `os.WriteFile(tmpPath, data, 0o600)` creates tmp with 0600 — that is correct. `os.Rename` preserves permissions. Good. However, on Windows, mode bits are largely advisory. For defense in depth, the parent directory (`./browser-profile`) is created with 0o755 (line 167). On multi-user Windows systems the profile directory (containing the Firefox/Chromium cookies.sqlite) is world-readable.
- **Fix:** On Windows, set ACL on the profile dir to the current user only. `golang.org/x/sys/windows` has helpers.
- **Effort:** 2 hr.

### 26. [Medium] Setup browser launched with user data dir controllable from config — path traversal risk if future UI lets user supply it
- **File:** `internal/cookies/autocookies.go:87-91`, `autocookies_chromium.go:32-39`
- **What:** `profileDir` comes from `cfg.Cookies.BrowserProfileDir`. Today it's config-file controlled. If a future feature exposes this via the web UI (setup wizard), a malicious origin with CSRF bypass could set it to `C:\Users\...\AppData\Local\Google\Chrome\User Data\Default`, making Moombox launch against the real Chrome profile and steal session cookies.
- **Fix:** Whitelist-validate that `profileDir` is under Moombox's data dir. If it's under a known browser profile path, refuse with a clear error.
- **Effort:** 45 min.

### 27. [Medium] firefoxUserJS silently disables telemetry opt-out notice — privacy-adjacent
- **File:** `internal/cookies/autocookies_firefox.go:17-23`
- **What:** The written `user.js` sets `datareporting.policy.dataSubmissionPolicyBypassNotification=true`, which suppresses Firefox's data-submission policy first-run dialog. This doesn't *send* telemetry that wasn't going to be sent, but it hides the notice from the user. Users operating under privacy regulations might object.
- **Fix:** Also set `datareporting.healthreport.uploadEnabled=false` and `toolkit.telemetry.enabled=false` to be safe.
- **Effort:** 5 min.

### 28. [Low] CookieJar.Load does not clear cookies on read error — partial state retained
- **File:** `internal/cookies/jar.go:48-63`
- **What:** On `os.ReadFile` error (other than not-exist), `Load` returns before resetting maps — actually no, the reset happens *first* (lines 53-54), so the jar is emptied BEFORE the read fails. This is the opposite problem: a read error empties the jar, potentially wiping valid auth mid-run.
- **Fix:** Read file first, then only swap maps if read succeeded. Atomicity.
- **Effort:** 15 min.

### 29. [Low] CookieJar.GenerateAuthorizationHeader: timestamp is re-generated on every call
- **File:** `internal/cookies/jar.go:200-222`
- **What:** The timestamp is `time.Now().Unix()` per call. For a single request that's fine. But if a retry happens and a second header is generated with a different timestamp, YouTube could flag it as replay. The current behavior (capture `now` once inside the function) is correct; just worth noting no clock-skew defense exists.
- **Fix:** None needed for correctness; consider a jitter window if clock goes backward (monotonic vs wall).
- **Effort:** n/a.

## Browser Coverage / Compatibility

### 30. [Medium] Missing Chromium forks: Vivaldi, Arc, Thorium, Yandex, UC Browser
- **File:** `internal/cookies/autocookies_detect.go:50-57`
- **What:** `knownBrowsers` lists 6 browsers. yt-dlp supports at minimum `{brave, chrome, chromium, edge, opera, vivaldi, whale}`. Users on Vivaldi (moderately popular) get no auto-detection.
- **Fix:** Add Vivaldi, Arc, Thorium. Test that `--user-data-dir` works for each.
- **Effort:** 30 min + manual QA.

### 31. [Medium] Missing Firefox forks: LibreWolf, Zen Browser, Firefox Developer Edition
- **File:** `internal/cookies/autocookies_detect.go:50-57`, `autocookies_firefox.go`
- **What:** Only Firefox and Waterfox are supported. LibreWolf is increasingly popular among privacy-conscious users. Zen Browser is a rising Firefox fork.
- **Fix:** Add LibreWolf (`librewolf`) and Zen (`zen`). `isFirefoxBased()` in `autocookies_detect.go:38` is the single branch point.
- **Effort:** 20 min.

### 32. [Low] Opera Mini / Opera Crypto Browser absent; Opera GX mapped to both "opera" type names
- **File:** `internal/cookies/autocookies_detect.go:55`
- **What:** Regular Opera and Opera GX share type `"opera"`. Opera Crypto Browser has a separate binary. Users on Opera One may see Opera GX preferred even if they run only Opera One.
- **Fix:** Consider `opera-gx` vs `opera` as distinct types, though UX-wise this might be pedantic.
- **Effort:** n/a (document as non-goal).

### 33. [Low] Safari not mentioned — intentional because Windows-only
- **File:** N/A
- **What:** yt-dlp supports Safari; Moombox does not. Consistent with Windows-only focus.
- **Fix:** None.
- **Effort:** n/a.

## Dedup Opportunities

### 34. [Low] YouTube/Google domain filter repeated in 3 places
- **File:** `jar.go:94-97`, `autocookies_merge.go:20-25`, `refresh.go:465-468`
- **What:** Three slightly different domain-relevance checks: `jar.go` uses substring match on youtube/google/twitch, `autocookies_merge.go` has a helper `isRelevantDomain`, `refresh.go:465` inlines its own `strings.Contains(scLower, "youtube.com") || ...google.com`. Easy to drift.
- **Fix:** Extract one `isRelevantDomain(domain string) bool` and one `isYouTubeDomain(domain string) bool`, use everywhere.
- **Effort:** 20 min.

### 35. [Low] "YouTube session body JSON" string is re-built in 2 places
- **File:** `refresh.go:303` and `refresh.go:388`
- **What:** Two nearly-identical Innertube request bodies and headers. Could share a single `buildYouTubeGuideRequest` helper.
- **Fix:** Extract helper; already have `setYouTubeHeaders`.
- **Effort:** 20 min.

### 36. [Low] "Process killed on Windows with taskkill" wrapped in both `killProcessTree` and `closeFirefoxGracefully`
- **File:** `autocookies.go:498-507` and `autocookies_firefox.go:76-80`
- **What:** `closeFirefoxGracefully` uses `taskkill /T` (no /F) to send graceful. `killProcessTree` uses `/F /T`. These are both valid but the wrapping is scattered. Similarly `refreshFirefox` spawns `cmd := exec.Command(...)` and then calls `runWithTimeout` — but `runWithTimeout` internally starts the process, not the caller. The caller hasn't called `cmd.Start()` yet, so `runWithTimeout(cmd, ...)` does start it — good. But that double-responsibility is confusing.
- **Fix:** Single `browserLauncher` abstraction with both graceful and forceful kill; `runWithTimeout` always owns the lifecycle.
- **Effort:** 2 hr (optional cleanup).

### 37. [Low] `setError / lastError` and `needsRelogin` both communicate "auth is broken" via different channels
- **File:** `autocookies.go:35-42, 546-550`
- **What:** `LastError` is free-form text; `NeedsManualRelogin` is a typed per-platform bool. Call sites overlap — a Twitch refresh failure sets `lastError = "..."` AND `needsRelogin.Twitch = true`. UI has to inspect both.
- **Fix:** Consolidate — `lastError` reflects exception/IO failure, `needsRelogin` reflects auth state; document the separation.
- **Effort:** n/a (doc-only).

## Quality Improvements

### 38. [Low] Magic numbers scattered: 30s timeout, 15s, 5MB, 5 retries, 500ms, 8s, 200ms
- **File:** `autocookies.go:19`, `refresh.go:23`, etc.
- **What:** Some constants are named (`processTimeout`, `authCheckTimeout`, `defaultRefreshInterval`), others are inline. E.g. `cdpPollTimeout = 15s` vs `extractChromiumCookies ctx = 30s` vs `cdpNavigateAndWait = 30s` vs `cdpSendCommand = 5s`.
- **Fix:** Group timeouts into one block; name them `cdpNavTimeout`, `cdpReadTimeout`, etc.
- **Effort:** 20 min.

### 39. [Low] `strings.Contains(domain, "google.com")` would also match "fakegoogle.com.evil.tld"
- **File:** `autocookies_merge.go:21-24`, `jar.go:95-96`
- **What:** The substring check is lax. For cookies coming from a browser via CDP this is safe in practice (CDP returns the exact cookie domain), but if a hand-edited cookie file contains `.evilgoogle.com`, we'd happily treat its cookies as auth.
- **Fix:** Use suffix match with leading dot or exact-equals: `strings.HasSuffix(domain, ".google.com") || domain == "google.com"`.
- **Effort:** 15 min.

### 40. [Low] `strings.ToUpper(name), "GOOGLE"` comparison in refresh.go:584 is heuristic
- **File:** `refresh.go:584`
- **What:** `if strings.Contains(strings.ToUpper(name), "GOOGLE")` attempts to decide domain for new cookies. This is brittle — most YouTube cookies don't have "GOOGLE" in the name but are Google cookies. `PREF`, `NID`, `SID` don't contain "GOOGLE" but are google.com.
- **Fix:** Track original `Set-Cookie` `Domain=` attribute when parsing; use that as authoritative domain.
- **Effort:** 30 min.

### 41. [Low] `fmt.Fprintf(&result, ...)` ignores errors
- **File:** `refresh.go:592`
- **What:** `fmt.Fprintf` returns (int, error). Error is never checked. For a `strings.Builder` that's fine (never errors), but the pattern sets a precedent.
- **Fix:** Use `result.WriteString(fmt.Sprintf(...))`.
- **Effort:** 5 min.

### 42. [Low] `conn.Read(navCtx)` return-value `_, _ = conn.Read(...)` pattern used 3 times
- **File:** `autocookies_chromium.go:222`, `:412`
- **What:** Discarding CDP response reads silently. At least a Debug log would help diagnose CDP hangs.
- **Fix:** Add `rs.logger.Debug("CDP read result", "err", err)` or similar.
- **Effort:** 10 min.

### 43. [Low] `browserDetectCache` is a package-level singleton — tests can't reset it
- **File:** `autocookies_detect.go:20-27`
- **What:** No way to reset or bypass the cache. Tests can't inject a fake detector without exposing internals.
- **Fix:** Promote to a `Detector` struct with a `Reset()` method, or use a `runtimeGOOS()`-style seam.
- **Effort:** 30 min.

### 44. [Low] `AutoCookieReloginRequired` has hardcoded JSON field names; adding a third platform requires schema changes at 4+ sites
- **File:** `autocookies.go:29-32`
- **What:** YouTube and Twitch are spelled out as Go fields, JSON tags, and switch cases. Adding YouTube Music, Kick, or a third platform requires edits in at least `autocookies.go`, `refresh.go`, `cookies.go` routes, and the frontend.
- **Fix:** Use `map[string]bool` with canonical platform string keys.
- **Effort:** 1 hr.

### 45. [Low] `time.Sleep(300 * time.Millisecond)` and `time.Sleep(500 * time.Millisecond)` are sprinkled
- **File:** `autocookies.go:519`, `autocookies_chromium.go:144`, `autocookies_firefox.go:69`, `:91`, `:99`
- **What:** Hardcoded short sleeps compensate for OS-level handle release timing. Tolerable but fragile.
- **Fix:** Poll-with-timeout instead.
- **Effort:** 1 hr.

### 46. [Low] `chromiumLockFiles` list hardcoded — misses `SingletonLock-` variants newer Chrome uses
- **File:** `autocookies_chromium.go:22`
- **What:** Different Chrome versions create different lock files (`SingletonLock`, `SingletonLock.lock`, `SingletonSocket`, `SingletonCookie`). Brave adds `Last Version`. Full coverage requires scanning the profile dir for files matching pattern.
- **Fix:** Glob for `Singleton*` and `*lockfile*` before delete. Respect caveat in finding #9.
- **Effort:** 20 min.

## Tech Debt / Future Refactors

### 47. [Medium] Two similar "extract cookies and merge" code paths — FinishSetup vs RefreshCookies
- **File:** `autocookies.go:194-301` and `autocookies.go:315-450`
- **What:** Both paths: extract-via-browser → optionally merge with existing → atomic write → reload jar → verify via API → set re-login flags. About 80% of the logic is identical. Diverge on: kill browser vs leave browser running, verification callback timeouts.
- **Fix:** Extract `extractAndPersist(ctx, browser, closeBrowserAfter bool) (yt, tw bool, err error)` helper.
- **Effort:** 2 hr.

### 48. [Medium] No mechanism to roll forward cookie-file schema (e.g. JSON sidecar for metadata)
- **File:** N/A
- **What:** Everything lives in a Netscape text file. "Platforms verified" is tracked in config. "Last refresh" is in memory. On crash we lose the last-refresh timestamp, so the TUI status bar shows "never refreshed" after a restart even if cookies are fresh.
- **Fix:** Persist `LastRefresh` and `LastError` to a sidecar (`cookies.meta.json`) on the same atomic-write path.
- **Effort:** 2 hr.

### 49. [Low] No structured support for multi-account cookie profiles
- **File:** `autocookies.go:47-76`
- **What:** One profile dir, one cookies.txt. A user with two YouTube accounts (work + personal) cannot switch. Not a bug; a feature gap.
- **Fix:** Future: `cfg.Cookies.Profiles[name] = { cookieFile, browserProfileDir }`.
- **Effort:** Significant (design needed).

### 50. [Low] No telemetry on "refresh failed because browser was running"
- **File:** N/A
- **What:** If a user has Chrome open on the same profile dir that Moombox wants to use (shouldn't happen in the default config, but could happen if user pointed Moombox at their real Chrome profile), the launch fails silently with lockfile errors. A specific error message would help.
- **Fix:** Detect "ProfileInUse" or lockfile-holder errors and surface: "Close your browser and try again."
- **Effort:** 1 hr.

## Dead / Unused Code

### 51. [Low] `s.setupCmd *exec.Cmd` stored but only used once
- **File:** `autocookies.go:51`, `autocookies_chromium.go:47`, `autocookies_firefox.go:42`
- **What:** `setupCmd` is set but read only inside `cleanup()` where it's set to nil — never dereferenced for any operation (all kill paths use `setupProcess`).
- **Fix:** Remove the field.
- **Effort:** 5 min.

### 52. [Low] `JobObject` field `jobCounter` incremented on create but never used to identify leaks
- **File:** `job_windows.go:11, 66`
- **What:** Counter is included in the Job Object name but nothing outside this file inspects it.
- **Fix:** Either log it (Debug) when creating, or drop to simple naming.
- **Effort:** 5 min.

### 53. [Low] `job_other.go` implements a cross-platform stub but the package is Windows-only by spec
- **File:** `job_other.go`
- **What:** Provides no-op implementations for non-Windows builds. Per CLAUDE.md "Windows-only — Linux/Mac only if requested. Always assume Windows." Keeping the stub makes `go build ./...` on Linux work for developers but is otherwise unused. Not harmful, but inconsistent with the "Windows-only" rule.
- **Fix:** None needed, but document why.
- **Effort:** n/a.

## Test Coverage Gaps

### 54. [High] No tests for `mergeCookieFiles`, `deduplicateAndFormat`, `isEssentialCookie`, `isRelevantDomain`
- **File:** `autocookies_merge.go`
- **What:** This is the data-path correctness hotspot. Bug surface includes domain dedup, `#HttpOnly_` handling, ordering, tab handling, empty strings. Zero unit tests.
- **Fix:** Add table-driven tests covering each filter + order path.
- **Effort:** 2 hr.

### 55. [High] No tests for RefreshService auth-loss transition logic
- **File:** `refresh.go:220-265`
- **What:** The `prevYouTubeAuth → current not-auth → OnRecoveryNeeded` transition logic is non-trivial (network-error case vs genuine-auth-loss case, first-check vs subsequent-check, hasCheckedOnce seeding). Zero tests.
- **Fix:** Mock HTTP client, drive through sequences: (auth) → (auth), (auth) → (nil, netErr), (auth) → (false, nil), etc.
- **Effort:** 2 hr.

### 56. [High] No tests for `updateCookieFile` (silent cookie drift, duplicate rows, new-cookie insertion)
- **File:** `refresh.go:533-613`
- **What:** The update logic is the most complex file-mutation function in the package. Test coverage = zero.
- **Fix:** Round-trip test: (initial file, set of updates) → updated file — verify every update applied, preserves HttpOnly prefix, preserves comments.
- **Effort:** 2 hr.

### 57. [Medium] No tests for `DetectBrowser` (can't, because filesystem I/O) — but `detectDefaultBrowserWindows` regex parsing is test-worthy
- **File:** `autocookies_detect.go:141-176`
- **What:** The parsing of `reg query` output is untested. Sample outputs across Windows versions could be mocked.
- **Fix:** Extract pure parsing function; test with fixture strings.
- **Effort:** 30 min.

### 58. [Medium] No tests for SAPISIDHASH correctness vs a known vector
- **File:** `jar.go:224-229`
- **What:** `TestGenerateAuthorizationHeader` checks the string contains `SAPISIDHASH` but doesn't verify the hash is correct against a known (timestamp, SID, origin, hash) vector.
- **Fix:** Add a test that hardcodes `unixTime=1234567890` and verifies the exact hash.
- **Effort:** 10 min.

### 59. [Medium] No tests for cookie-header ordering / stability (related to finding #1)
- **File:** `jar.go:153-162`
- **What:** Once ordering is deterministic, a test should lock it down.
- **Effort:** 10 min (after fix).

### 60. [Low] Job Object (`job_windows.go`) has no test — acceptable because Windows-syscall-heavy
- **File:** `job_windows.go`
- **What:** Integration test would need a real subprocess.
- **Fix:** Optional — integration test using `powershell.exe -Command "Start-Sleep 10"`.
- **Effort:** 1 hr.

## Questions for Owner

1. **CDP vs SQLite decryption.** yt-dlp directly decrypts Chrome's `Cookies` DB with DPAPI. Moombox instead launches a browser and uses CDP. Was this chosen because (a) CNG/DPAPI complexity, (b) to always get "fresh" cookies, or (c) to avoid triggering Windows Defender on cookie-file access? Answer shapes whether adding DPAPI support for read-only extraction from the user's real Chrome is worth it (would unblock #31 indirectly and let users keep using their own browser).

2. **Browser-profile location.** `browserProfileDir` defaults to `./browser-profile`. On Windows this lands in the working directory — which if Moombox is launched from a random folder could pollute that folder. Should this default to a stable path like `%LOCALAPPDATA%\Moombox\browser-profile`?

3. **Refresh frequency.** 30-min default was inherited from TS. Is there data on how often YouTube actually rotates session cookies? If it's hourly or less, we could slash refresh overhead (and the #23 laziness optimization) without changing auth reliability.

4. **COOKIES? UX.** When auth fails, the user sees `COOKIES?` job status. Does the SPA/TUI trigger a setup-wizard deep link, or does the user need to find Settings? This package returns `NeedsManualRelogin{YouTube:true}` — is that surfaced to the user with an explicit "Click here to re-login" CTA?

5. **Concurrent setups.** `mu.Lock` serializes `StartSetup`. If a user accidentally clicks "Start" twice, the second fails with "setup already in progress." Is that the right UX, or should the second click cancel the first and start a new one?

6. **Multi-account.** Is there any plan to support multiple YouTube accounts or a Twitch+YouTube co-signed single browser session? Affects whether #44 and #49 become real work.

7. **Telemetry for stale state.** #14 and #48 both concern "is the jar's state fresh?" — would you welcome a `lastRefreshLatency` metric / "refreshing now..." indicator in the status bar?

## Summary Counts
- Critical: 0
- High: 12 (findings #1–7, #24, #54–56)
- Medium: 23 (findings #8–23, #25–27, #30–31, #47–48, #57–58)
- Low: 22 (findings #28–29, #32–46, #49–53, #59–60)
- Total: 60 (findings numbered 1–60; ownership questions separate)
