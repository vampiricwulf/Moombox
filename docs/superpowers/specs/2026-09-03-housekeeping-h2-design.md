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

## R9 — The profile-mode boot and post-flight lines say what happened (12c arc-close F1, F2)

Two operator-facing lines describe a mechanism that did not run when `cookies.acquisition = "profile"`.
Neither is a decision; both are sentences, and both are wrong in the configuration the README recipe
prescribes. **This is the item that crosses the Non-goals' "No REST/web change" line, by controller
ruling (2026-09-03): the Arc 12c arc-close homed F2 in H2, and F2's second surface is the dashboard.
The exception is exactly one additive payload key, one exported JS helper and the toast that reads
them — no route, no status code, no config key, no schema.**

**F1 — the boot line.** `NewAutoCookieService` (`internal/cookies/autocookies.go:548-551`) computes
`validateBrowserProfileDirForLaunch`'s verdict and, when it is non-nil, logs
`auto-cookie profile dir rejected at construction` at **ERROR**, on every boot whose
`cookies.browser_profile_dir` is a real browser profile tree — in `profile` mode too. The constructor
cannot do better: `AcquisitionMode` is wired at `cmd/moombox/services.go:970`, AFTER construction at
`:922`, so the level is chosen before the mode is knowable. The verdict is right and does not move —
the launch guard refuses that directory in every mode, which is the whole of `security.md`'s launch
boundary — so what changes is only WHERE the sentence is said and at what level. The constructor
computes silently; one new method, `AutoCookieService.LogProfileDirVerdict()`, says it once, called
from the wiring site immediately after the `AcquisitionMode` closure and before
`s.autoCookieSvc = autoCookieSvc`. Under `auto` it is the existing ERROR — message and `err` argument
verbatim, because "refusing to launch a headless session against it" is exactly the operator's cue
that a refresh they expect to happen will not. Under `profile` it is one INFO stating both halves:
no headless browser will be launched against this directory, and the read-only import is what runs.
**Unchanged:** `validateBrowserProfileDirForLaunch`'s single call site, `profileDirErr`'s semantics,
and the four subprocess sites' direct `s.profileDirErr` reads in every mode — every arc since 12c
pins those (`autocookies_launchguard_test.go`), and `readOnlyProfileDirErr` keeps its own wording.
**Lock preconditions:** `LogProfileDirVerdict` takes NO lock and MUST NOT be called with `s.mu` held;
it calls `resolvedAcquisition()`, which reaches the config store's own RWMutex through the injected
callback — the same rule the four launch sites and `readOnlyProfileDirErr` already follow. It reads
`profileDirErr`, `profileDir`, `logger` and `AcquisitionMode`, each written once before the service
is handed to any goroutine.

**F2 — the post-flight sentences.** `internal/tui/app_update.go:420-434` and `web/public/app.js:847-909`
both open with `Browser cookie refresh …` after a pass that launched nothing. Pre-existing — every
browser-free import has rendered it since the import path landed, long before `cookies.acquisition`
existed — because both surfaces had only the MODE to go on, and the mode is not the answer: a host
with no browser installed imports in `auto` mode too. So the fix is at the source. `RefreshResult`
gains `Mechanism`, one of `RefreshMechanismBrowser` (`"browser"`) or `RefreshMechanismProfileImport`
(`"profile-import"`), stamped at the one place the path is chosen — the `importedFromProfile`
decision (`autocookies.go:2092-2095`) — and empty on every exit that stops above it. It reaches
every one of `refreshCookiesDetailed`'s eighteen returns (eight aborts, seven declines, three
verdicts) through a named result and a single `defer`, not through eighteen edited return literals,
so the nineteenth return site carries it too. One decline sits BELOW the decision — the browser
branch's empty-jar gate, `len(s.refreshPlatforms()) == 0` (`:2137`) — and carries `"browser"`: that
branch was chosen and then declined, which is what the sentence should say, and it is the sentence
the mode fallback produces for it anyway (the gate is reachable only in `auto` with a browser).
It is **wording only**, the same rule `YouTubeStored` / `TwitchStored` carry: nothing branches a
decision on it. On the wire it is one additive key, `mechanism`, on `cookieRefreshOutcome`
(`internal/web/routes/cookies.go:55`) — additive exactly as `ran` and `verdict` were, so an older
frontend ignores it and a newer frontend against an older binary reads `undefined`.

Then ONE producer per surface names the sentence's subject from it:
`cookieRefreshMechanismLabel(mechanism, mode)` in `internal/tui/app_actions.go` beside
`cookieRefreshFeedback`, and `cookieRefreshMechanismLabel(mechanism, acquisition)` in
`web/public/modules/utils.js` beside `cookieRefreshPreflightToast`. Result first, mode as the
fallback: an empty mechanism means the pass declined before choosing, and the mode is then the best
answer available and the one the PRE-flight sentence already gave, so the two lines agree instead of
contradicting each other. `TestRefreshPostflightMechanismAgreesAcrossSurfaces` pins the two by exact
equality over every mechanism × mode combination, driven through the goja harness `utilsModuleVM`
already built for `TestRefreshPreflightSentenceAgreesAcrossSurfaces`
(`internal/tui/settings_acquisition_test.go:107-170`).

**What deliberately does NOT move, and why.** The per-arm PREDICATES stay per surface and unchanged
("…declined to run (…) — nothing was learned about these cookies" against "…declined to run — …
Nothing was learned about these cookies.", "…ran and auth verification failed" against
"…completed — auth verification failed"). Only the SUBJECT is shared. Unifying the predicates would
break pins that exist for other reasons — `TestAppJSReadsTheFieldsTheHandlerEmits` needs
`data.ran === false` and `data.verdict === "failed"` to stay in `app.js` itself, so the arms cannot
move into a shared helper; and pulling the TUI's wording toward the dashboard's would break
`TestCookieForceRefreshFeedback`'s case-sensitive `wantSaid` substrings
(`internal/tui/cookie_refresh_feedback_test.go`: `"nothing was learned"` against the dashboard's
`Nothing was learned about these cookies.`) and stale the two rows `TestFeedbackColorWarningMessages`
(`internal/tui/app_layout_test.go:100-102`) pins as "real strings emitted by the TUI today" — and
none of that is what F2 is about. Rung 3 is untouched and out of scope on its own account: it names
an affordance, not a mechanism, and `TestRungThreeSentencesDivergeByDesign` pins that pair APART.
The `!Renewed` arm on both surfaces keeps its browser wording because an import can never reach it:
`renewed := importedFromProfile || browserActed` (`autocookies.go:2455`) forces `Renewed` true on
every import, and `browserActed` starts true and is cleared only inside the browser branch (`:2110`),
so each guard holds the arm shut on its own — dropping either alone changes nothing (verified by
mutation), and the mechanism test pins the property by driving a real import to a `Ran` result. `cookieRefreshReportFor`'s worker log (`cmd/moombox/services.go:54`) needs
nothing: it already says "automatic cookie refresh", which is true of both mechanisms.

Docs: `data-and-storage.md:897` (the pre-flight paragraph gains its post-flight half),
`user-interfaces.md:622-629` ("the four `cookieRefreshOutcome` keys" → five, with the row),
`security.md:461` (the launch-boundary paragraph gains the level-per-mode clause), and the Arc 12c
plan's final-state rows `:2327` / `:2328` plus field-test row 23's reading rule, which currently
tells the tester to expect both wrong lines.

## R9 invariants

`profileDirErr` is still computed exactly once, in the constructor, and still read directly by all
four subprocess sites in both modes. `Mechanism` never gates a branch. No new goroutine, no lock, no
config key, no route, no status code, no database column. The anonymous logger, in place.


---

## Non-goals
No arming. No `AuthCookieHorizonFor` caller sweep. No change to `shouldFireRecovery`. No REST/web change — R9 is the ruled exception (2026-09-03): one additive `cookieRefreshOutcome` key and one `utils.js` export; no route, no status code.

## Invariants
`livenessRecoveryArmed` stays false; `main.go:276-278` untouched; no cookie value/token in any log
(timestamps only); no goroutine; anonymous logger; every assertion mutation-checked; byte-wise LF.
