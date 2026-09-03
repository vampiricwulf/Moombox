# Housekeeping H2 — the cookie-side items: design

**Status:** owner-ruled 2026-08-29 (Q1 back-off; Q5/Q7 horizons + login expiry; Q13 A2 narrowed) and
the Arc 10/12a reviews' carried items; drafted 2026-09-03 from `h2-file-map.md`. Runs on branch
`cookie-housekeeping-h2` from `main` AFTER Arc 12c merges (shared files: `autocookies.go`,
`services.go`). The liveness pilot stays disarmed.
**Scope:** `internal/cookies` (`refresh.go`, `autocookies.go`, `autocookies_profile.go`,
`autocookies_merge.go`, `jar.go` read-only), `cmd/moombox` (the startup line; an AST test),
`internal/tui` (the feedback fold; the wizard's accepted verdict), the spec sentences each item flips.

## R1 — The re-alarm backs off (Q1)
Replace the flat `livenessRefireWindow` re-fire in `recordLiveness` (`refresh.go`) with a per-platform
back-off: base 30 min, ×2 per re-fire, cap 24 h, RESET on a conclusive positive (auth returns). State
lives beside `lastLivenessKnown` (a per-platform `refireAfter time.Duration` or the plan's shape);
the pilot's gate (`livenessRecoveryArmed`) is unchanged — the back-off shapes WHEN a re-fire would
happen, and stays inert until arming. Tests mutate the base, the factor, the cap and the reset
(`refresh_liveness_test.go` gains five: four schedule tests that use the constants SYMBOLICALLY, plus
one literal pin of the three numbers — without it a 15-minute base or a 20-hour cap passes every
schedule test; `TestRecordLivenessRefiresAfterTheWindow` becomes the base case). The reset trigger is
the tier-2 conclusive positive (`recordLiveness(loggedIn=true)`) only; a tier-1 `OnAuthRecovered` does
not reset, and a tier-1 fire stamps the dedupe without escalating. Docs: `data-and-storage.md:843`
("until that lands…" → built), `:845` ("Three maps" → four, with the new row), `:867`, the remediation
plan's F2 note.

## R2 — The startup line carries the horizons and the login expiry (Q5, Q7)
The `Cookies loaded` Info line (`cmd/moombox/services.go`, find by `log.Info("Cookies loaded"`) and
the refresh-completion line `"cookie refresh succeeded"` in `refreshCookiesDetailed` (`autocookies.go`)
gain `youtubeAuthHorizon` / `twitchAuthHorizon` (ISO-8601 from `AuthCookieHorizonFor`, `"none"` when
zero) and `twitchLoginExpiry` (a new accessor `CookieJar.TwitchLoginExpiry` reading the `login` row's
expiry — `authCookieNamesFor` is NOT split), through ONE producer (`CookieJar.HorizonLogFields`) so the
two lines carry identical keys and can be read against each other. NOT the per-launch
`autocookies_firefox.go:288` line: it fires inside `refreshFirefox`'s platform loop BEFORE
`readFirefoxCookies`, the merge, `writeCookieFile` and the jar reload, so a horizon there is the one
the pass STARTED with (the boot line already has it), and `refreshChromium` has no Info completion
line to twin. Values only ever timestamps — never a cookie value. ONE Warn at `refreshCookiesDetailed`'s
merge site when `mergeCookieFiles`' expiry prune drops an expired `login` while an `auth-token`
remains (the Twitch half-credential the Arc 10 routes mark). Tests reuse `argRecordingLogger`
(`cookie_import_service_test.go`), which captures args; the startup line's field list is extracted
into a pure `cookiesLoadedFields` so `cmd/moombox` can assert it without driving `initServices`.
`AuthCookieHorizonFor`'s existing callers are NOT swept (owner). Docs: the three "no UI or log carries
a horizon" sentences (`data-and-storage.md:705`, `user-interfaces.md:724`, `platform-services.md:495`)
and the settling observation ("read the two log lines" replaces the test-only reading).

## R3 — The browser path rolls back on a regression only (Q13, A2 narrowed)
A twin of the import gate (`autocookies.go` pre-check `pre := map[string]platformAuth{}` … `platformsToRestore`)
on the browser path, taking ONLY the regression arm of `platformsToRestore` (`autocookies_profile.go:716`:
`before.ok() && after.state == verifyFailed`); the `verifyUnknown` arm (`:718`) must NOT restore on the
browser path (a browser refresh writes cookies just re-fetched from the live site — an inconclusive
verify is not evidence of regression). Clone `TestRefreshCookiesRestoresOnlyTheRegressedPlatform` for
the browser path; mutate both arms (restore on unknown → fails; skip the regression restore → fails).
The browser-path test drives a Firefox that cannot launch (`Path: "moombox-no-such-browser"`, the
established fixture): `refreshFirefox` swallows the launch failure and reads the managed profile, which
reaches the merge/write/verify sequence with no real browser. The `pre` snapshot's
`importedFromProfile &&` guard is dropped (the snapshot runs on both paths whenever a `cookies.txt`
exists); the policy is chosen at the restore site. Two operator strings that name the import path
("imported profile cookies did not hold up…"; "…the mounted browser profile did not verify") become
false on the browser path and are made path-conditional; the two `errMsg` sentences that say "the
browser profile did not verify" stay, being true of both paths. The now-false scoping comment
(`autocookies.go` "Only the import path does this…") and `data-and-storage.md:907` gain the
browser-path sentence; the remediation plan's `:2015` (2) closes and the A2 bullet (`:2009`) gets its
BUILT note. Rebased on Arc 12c's `resolvedAcquisition()` at the same decision site
(`importedFromProfile := browser == nil`, forced true under `AcquisitionProfile`).

## R4 — `authStatusChanged`'s contract is pinned, not widened
`refresh.go:447-454` keeps its body; its CONTRACT paragraph (`:423-435`) is restated as the doc of
record and the boundary the existing tests leave implicit is pinned. The missing rows are the two
`Authenticated` booleans moving ALONE: `TestAuthStatusChangedGateCoversEverySurfaceInput`'s base has
both verdicts `RefreshFailed` and every row that moves an `Authenticated` boolean also moves that
platform's verdict, and `TestOnAuthChangeOnlyFiresWhenAuthFlagsChange` moves boolean and verdict
together off a zero `AuthStatus` — so a gate with either boolean comparison DELETED passes today
(verified by mutation at `8558f5f`). Two rows in the existing table test close it. The reason-only
case ("a `TwitchError` change fires no push") is already FULLY pinned — the table's "only the error
wording changed" row and `TestTwitchMarkFiresAuthChangeOnAVerdictTransitionOnly`'s second mark
(`refresh_twitch_mark_test.go:392-437`) — and is not the gap. No widening.

## R5 — The TUI feedback triple is one struct
`App.feedbackMsg` / `feedbackSev` / `feedbackTimer` (`internal/tui/app.go:355-369`) fold into one
`feedback` struct written by the three setters (`app_update.go:924-942`) and read by `app_layout.go`;
the invariant "a message never outlives its severity" becomes structural. Every existing test that
reads the fields is updated; behaviour byte-identical (the layout tests pin it).

## R6 — ARMING row 12 is asserted now, where it can be
The Twitch mark's fire path stamps `noteRecoveryDecided("twitch", …)` (`NoteTwitchAuthLoss`, `refresh.go:1235`)
— assert the stamp itself while DISARMED (`lastRecoveryDecided["twitch"]` set by the mark); the
double-fire suppression under arming stays for the arming commit (`progress-arc8.md` § ARMING row 12
records which half is now pinned).

## R7 — The deferred re-check sites are pinned by an AST test
A `cmd/moombox` test (the `internal/web/routes/cookies_import_callsite_test.go` precedent) walks
`monitor_callbacks.go`, `services.go`, `tui_wiring.go` and asserts each `recheckAfterCookieWrite` /
`checkNowFn` call site the Arc 10 reload-site table names sits inside a `*ast.DeferStmt` — except
`OnPassCompleted`'s plain closure (`services.go:1058-1060`), which the test pins by its own shape
(invoked through `postRefreshRecheckHook`). Mutation: hoist one call out of its `defer` → fails.

## R8 — The wizard shows its accepted verdict (12a arc-close F4)
`app_update.go:720-734`'s accepted arm renders INSIDE the wizard (a `successMsg` twin of `errorMsg`,
`setup_wizard.go:195`, rendered where `errorMsg` is in the two cookie views — `viewSimpleCookies`,
which `OpenCookieLogin` reaches, and `viewAdvancedCookies`) while the overlay is visible, instead of
the App feedback line the overlay hides; the feedback line ALSO still gets it (decided: setting both
costs nothing, holding the message until close needs a pending field and a drain hook). One goja-free
test mirroring `TestSetupCookieFinishFeedback`: the accepted text is reachable through
`setupWiz.View()` while `IsVisible()`, rendered with a `SuccessStyle` (lipgloss v2 emits ANSI in
tests, so the style assertion has teeth).

## Non-goals
No arming. No `AuthCookieHorizonFor` caller sweep. No change to `shouldFireRecovery`. No REST/web change.

## Invariants
`livenessRecoveryArmed` stays false; `main.go:276-278` untouched; no cookie value/token in any log
(timestamps only); no goroutine; anonymous logger; every assertion mutation-checked; byte-wise LF.
