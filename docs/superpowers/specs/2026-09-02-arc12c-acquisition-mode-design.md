# Arc 12c — an explicit cookie acquisition mode (G4), and the launch guard leaves the read-only import alone (G3): design

**Status:** owner-scheduled 2026-08-29 (Q10; the audit's G4 asked "is the knob wanted?" and the
owner scheduled it). Drafted 2026-09-02 from `arc12-file-map.md` and the audit findings G3/G4
(`reports/cookies-observations.md:317-350`, gitignored). A settings change: the `moombox-settings`
skill checklist (config struct → default → validation → API validation → Web UI → TUI → hot-reload →
docs) is the shape of the plan.
**Scope:** `internal/config` (one field + migration-safe default + validation), `internal/cookies`
(`AcquisitionMode` callback; the refresh decision; the guard split), `cmd/moombox` (wiring),
`internal/web/routes` + `web/public` (settings panel row), `internal/tui/settings.go` (row), docs.

## 1. The problem, as the code stands

`refreshCookiesDetailed` decides launch-vs-read by `importedFromProfile := browser == nil`
(`internal/cookies/autocookies.go:1958`): a host with any detectable browser always takes the launch
path; a Windows desktop user who wants browserless import from their real signed-in profile has no
way to ask for it. `auto_enabled` (`BrowserLaunchAllowed`, `:437`) gates only the launch. And the
launch-path security guard `validateBrowserProfileDir` (`:162`) — whose purpose is to stop a config
from driving the user's real browser headlessly — also fast-fails the two READ-ONLY paths
(`importProfileCookies`, `autocookies_profile.go:502`; the startup seed decision,
`autocookies_profile.go:872`), although the read-only path copies `cookies.sqlite` + `-wal` into a
0700 temp dir and opens the copy `mode=ro` (the snapshot in `autocookies_profile.go`), which is the
line `dpapi/dpapi.go` already draws. G3 is inert without G4: with a browser installed the read-only
branch is never taken.

## 2. Required behaviour

**R1 — One setting, TWO values (ruling 2026-09-02).** `cookies.acquisition`: `"auto"` (default; empty and
absent normalise to it; = today's behaviour exactly — launch when a browser resolves, otherwise the
profile import) and `"profile"` (never launch for a REFRESH: read the configured profile read-only;
the interactive wizard `StartSetup` is unaffected — it is the user's explicit click). The audit's
third value `"browser"` is NOT added: it would be observationally identical to `"auto"` at every
site (the decision is `profile`-vs-rest everywhere, and the non-goal forbids `"browser"` from
overriding `auto_enabled`), and a value that behaves like another is a trap — the doc says so; a
later semantics can add it additively. Normalisation replaces any other value with `"auto"` through
`validateOrNormalize`'s `fail()` (`internal/config/config.go:427` — it has no logger and deliberately
none; the recorded error surfaces through `config.Validate` to `Save` and `Store.Update` and the
settings UIs, exactly as `network.network_access` does), and the API rejects it with a field error
(`validateConfigUpdates`, `internal/web/routes/config_routes.go:106` — not `jobs.go`; the
`moombox-settings` skill is stale on that one path).

**R2 — Hot-reload through a callback, like the browser override.** `AutoCookieService` gains
`AcquisitionMode func() string` beside `ConfiguredBrowserOverride` (`autocookies.go:445`), wired in
`cmd/moombox` to the live config; `refreshCookiesDetailed` consults it at the `importedFromProfile`
decision (`autocookies.go:1958`): `"profile"` → import branch regardless of `resolvedBrowser()`;
`"auto"` → today's rule. `resolvedBrowser()` is untouched. A nil callback reads as `"auto"`.

**R3 — The guard is split (G3), tied to the opt-in.** `validateBrowserProfileDir` becomes
`validateBrowserProfileDirForLaunch` and stays on every subprocess site (the four launch sites from
the A1 file map). The two read-only sites consult it only when `AcquisitionMode()` is NOT
`"profile"`; in `"profile"` mode a read-only import of a real profile proceeds, and any remaining
read-only refusal has its own message that does not say "refusing to launch". `decideStartupSeed`
(`autocookies_profile.go:873`) has a SECOND short-circuit — it stands down whenever
`resolvedBrowser() != nil` — which in `"profile"` mode must not fire, or the boot seed never runs on a
desktop with a browser (the audit's G4 names this); the stale reasoning in `StartProfileSeed`'s
`gateApplies` comment changes with it. The periodic tick (`autocookies.go:3000`) is the same
defect at the other automatic caller: it decides "is this tick a browser-free import?" by
`refreshBrowser(gateExempt) == nil`, so in `"profile"` mode with `auto_enabled = true` it would run
an import every tick over a live `cookies.txt` without `automaticImportGuard` — it must treat
`"profile"` as browser-free too (review finding 2026-09-02). No forward-slash
variants are added to `dangerousProfilePathSubstrings` (a Linux desktop user pointing at a real
profile must keep working — audit G3).

**R4 — Settings surface, both UIs.** The web settings panel's cookie section gains a select
(`auto` / `profile`) with one-line help per value; the TUI settings overlay
(`internal/tui/settings.go:145-156`) gains the row; the REST `PUT` parses and validates it; the
existing `R F` ladder and the "Refresh cookies from browser profile" button respect the mode (in
`"profile"` mode the ladder's browser rungs are skipped and the status line says so).

**R5 — Docs.** `docs/spec/data-and-storage.md` § Cookies config (the field table) and § Auto-Cookie
Service (the "four ways" list `:872`, the `auto_enabled` table `:876-884`, the `R F` ladder
`:891-896`, the automatic-import rule `:898`, the guard sentence `:948`); `docs/spec/operations.md`
§ Cookies in a container (`:108` — the launch guard is not described anywhere in that file today, so
this is an addition, not a correction); `docs/spec/security.md` (the guard's purpose and its new
boundary — a new section; its Scope sentence enumerates coverage and gains the guard);
`docs/spec/user-interfaces.md` (the `auto-refresh` route row `:579` and the 422 sentinel table
`:640`, which enumerates every 422 sentinel and so must list the new one); `SPEC.md` § Cookies; the
remediation plan's G3/G4 paragraph (`:2009`); README's cookie-setup section (it lists the settings;
its "Three options:" already sits over four headings).

## 3. Non-goals

Not needed for Docker (G1+G2 did that; Arc 11 added the import). No change to `auto_enabled`'s
meaning (`BrowserLaunchAllowed` still gates launches; `"auto"` with `auto_enabled = false` launches
nothing and says so, as today — the two settings compose, they do not replace each other). No change
to the interactive wizard. No `dpapi_fallback` change.

## 4. Invariants

Config migration is non-destructive (`migrateOldFormat` untouched; absent = `"auto"`); the guard
never leaves a launch site; every AUTOMATIC browser-free import stays behind `automaticImportGuard`
(`"profile"` makes a pass browser-free by setting, and both automatic callers must recognise that);
no cookie value in any log or UI string; every assertion mutation-checked (both modes at the
decision point, plus the nil-callback default, the guard on a launch site vs a read-only site, the
validation replacement, the API rejection, the hot-reload callback, the periodic tick in profile
mode, the cross-surface sentence).
