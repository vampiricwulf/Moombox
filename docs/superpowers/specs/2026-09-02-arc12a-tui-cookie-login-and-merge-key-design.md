# Arc 12a — the TUI reaches the cookie login, and the merge key sees the path: design

**Status:** owner-scheduled 2026-08-29 (Q10: V9 + T3 key audit → Arc 12); drafted 2026-09-02 from
`arc12-file-map.md`; plan reviewed 2026-09-02 (`arc12a-plan-review.md` — R1, R4 and R5 below were
amended by that review). Two small items that share nothing but their arc; one plan.
**Scope:** `internal/tui` (one chord + one wizard entry), `cmd/moombox/tui_wiring.go` (nothing new
expected — the callbacks exist), `internal/cookies/autocookies_merge.go` (the key), the docs that
say the TUI has no cookie-setup entry point and the plan's T3 paragraph. No REST change, no config.

## 1. The problems, as the code stands

**V9.** The cookie login wizard exists in the TUI (`internal/tui/setup_wizard.go`: `OnStartAutoCookie`,
`OnFinishAutoCookie`, `OnCancelAutoCookie`, the 300 s countdown) and is wired to the real service
(`cmd/moombox/tui_wiring.go:475-528`), but it opens at exactly one site — `internal/tui/app.go:674-677`,
`if a.IsFirstRun { a.setupWiz.Open() }`. After first run the TUI can re-check (`R C`) and force a
browser refresh (`R F`) but cannot start the interactive login; the status bar can show `Relogin`
with no chord that answers it. The web dashboard has had this since Arc 3.

**T3.** `mergeCookieFiles` (`internal/cookies/autocookies_merge.go:142`) keys rows by `{name, domain}`
(`:143-146`, built at `:166`). RFC 6265 identity is name + domain + path. Two rows that differ only in
path collide on one map entry and the later-parsed line silently wins (`:179-189`). Three writers
merge through it: `FinishSetup`, the browser refresh, and Arc 11's import. Secure and HttpOnly are
attributes of a cookie, not its identity, and stay out of the key.

## 2. Required behaviour

**R1 — A chord opens the cookie login from anywhere in the TUI.** One new `R` chord (the plan picks the
letter from what `buildMenuItems()` leaves free and records why) added in `buildMenuItems()` and
`dispatchAction()` (`internal/tui/app_actions.go`) — the single source of truth for chords, menu, hints
and help (CLAUDE.md). It opens the setup wizard directly at its cookie step for the platform the
user picks — the wizard's cookie step IS the YouTube/Twitch picker already (`setup_wizard.go:673-713`,
hand-rolled on `cookieFocus`), so no second picker and no second state machine: a new entry
(`OpenCookieLogin(platform)`) opens at that step and a `cookieOnly` flag branches only the two exits;
it must NOT reuse `Open()` (which clears config values and shells out to `OnCheckFFmpeg`) and must
NOT arm the countdown itself (the countdown arms when a browser exists, as today). `StartSetup` is
not gated on `auto_enabled` and the three callbacks are bound unconditionally
(`cmd/moombox/tui_wiring.go:461`), so the chord has no refusal of its own to render: it is gated on
the callback exactly as `R F` is, and a nil callback DELETES the chord rather than making it speak —
`processSecondKey` (`internal/tui/app_keys.go`) resolves the second key against `buildMenuItems()`,
an unregistered pair reports `Invalid Chord: R L`, and the menu and help omit it. The
`dispatchAction` nil-guard is defensive (a direct caller) and unreachable from the keyboard. Every
service-side refusal (`ErrSetupInProgress`, `ErrRefreshInProgress`, `ErrNoBrowserFound`,
`ErrServiceStopped`) already renders inline in the wizard, on the operator's Enter.

**R2 — The `Relogin` badge names the chord.** The status-bar hint for `CookieStatusRelogin`
(`internal/tui/status_bar.go`) tells the user the chord, the way the header warning in the web UI
names its click.

**R3 — Behaviour parity with the web wizard, not a new flow.** Start → countdown → Finish/Cancel
map onto `StartSetup` / `FinishSetupDetailed` (60 s ctx as today) / `CancelSetup` through the
existing `On*AutoCookie` callbacks; the verdict renders the three-state vocabulary the web UI uses;
abandoning the overlay calls `OnCancelAutoCookie` → `CancelSetup` (the TUI's one cancel binding,
`tui_wiring.go:527`; `AbandonSetup` is the dashboard's unload beacon and has no TUI binding) so the
acquisition slot is released — pinned by a test.

**R4 — The merge key includes the path.** `cookieKey` becomes `{name, domain, path}`; `parseCookies`
fills `path` from `fields[2]`. One collision test: two rows same name + domain, different paths, both
survive; the mutant that drops `path` from the key fails it. The three callers are untouched. The
FILE stops losing a row; `CookieJar` stays name-keyed and last-wins for equal domains, so the loaded
entry is unchanged in every ordering — the code comment and the doc say so. Every in-tree comment
that states the old key as fact changes with it — the plan lists them: twelve mentions across seven
files, `cookie_import.go` and `cookie_import_test.go` included (the Arc 11 plan and spec are dated
records and stay).

**R5 — Docs state today's truth.** `docs/spec/user-interfaces.md` — the TUI chord table, the file
table's `setup_wizard.go` row and the overlay table's `First run` trigger cell (both say the wizard
is first-run only), the in-process sentence in §What the TUI renders (its callback list), a new
`R L` paragraph, and the two cookie-parity rows; grep finds NO standalone "no cookie-setup entry
point" sentence in `docs/spec/` — the only absence claim is the remediation plan's V9 bullet
(`:2010`), which the plan closes. Also CLAUDE.md's chord list (the `R` chords line), the
remediation plan's T3 (`:2012`) paragraph, and `docs/spec/data-and-storage.md:872` where the merge
key is described.

## 3. Non-goals

No REST or web change. No new config. No change to the wizard's first-run flow or to the 300 s
countdown. No change to `deduplicateAndFormat`'s bare-name key (deliberate, `autocookies_merge.go:94`).

## 4. Invariants

Chord table single source of truth; `huh` components over custom widgets (Charm-first); no goroutine
added without the inline recover; no cookie value in any TUI string or test; every assertion
mutation-checked (the chord dispatch, the platform pick at the cookie step, the cancel-on-abandon, the key).
