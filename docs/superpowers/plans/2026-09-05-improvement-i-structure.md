# Arc I — Structure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split the two 4k-line cookie files by responsibility, extract the niconico overlay's pure geometry and scheduling logic from `player.js`, and thin `app.js` by controller extraction — with NO behaviour change, every move pinned by tests that pass identically before and after.

**Architecture:** Go: pure same-package file moves (no renames, no signature changes) pinned by an exported-surface snapshot (`go doc -all`), the package tests and the docs citation test. JS: two new pure modules (`nico-geometry.js`, `nico-scheduler.js`) with node tests, `player.js` keeping the DOM/WAAPI half; `app.js` thinned into five controller modules in the existing `SettingsController`/`StatsController` pattern behind a new jsdom harness that pins the rendered output first.

**Tech Stack:** Go 1.27 (`GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command), staticcheck 2026.2.1 (hard gate), Node 24 `node --test`, jsdom (web/tests devDependency).

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §9 I. Design notes + rulings I1–I9: `.superpowers/sdd/2026-09-05-improvement-i-structure/design-notes.md`; inventories: `survey.md` there.

## Global Constraints

- No behaviour change anywhere in this arc. A Go move keeps every identifier, signature, visibility, comment and blank line; a JS extraction keeps every rendered string byte-identical.
- Same package for every Go move (`package cookies`). Exported surface pinned: `go doc -all ./internal/cookies` before == after.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils`; `internal/jobfilter`, `internal/stats`, `internal/ytdlpplugin` stay neutral (unchanged here).
- Docs citation test: `internal/docs/citations_test.go` `TestSpecDocCitationsResolve` pairs each backticked symbol with the `.go` file cited beside it and requires that FILE to declare or mention the symbol. Moved functions must have their citing docs re-pointed at the new file. Never rename a cited symbol.
- LF line endings only; `perl -0777 -ne 'print tr/\r//' <file>` → 0 on every touched file.
- Every commit ends with the two trailers `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp`.
- Never `go test ./...` inside a task (the controller runs the one full gate). Gates per task are listed in the task.
- Git is read-only beyond `git add`/`git mv`/`git commit` (no stash, checkout, reset, rebase).

---

### Task 1: split `internal/cookies/autocookies.go` by responsibility (pure moves)

**Files:**
- Modify: `internal/cookies/autocookies.go` (3973 lines → ≈700)
- Create: `internal/cookies/autocookies_setup_slot.go`, `autocookies_status.go`, `autocookies_browser_resolve.go`, `autocookies_setup.go`, `autocookies_verdict.go`, `autocookies_refresh.go`, `autocookies_messages.go`, `autocookies_periodic.go`, `autocookies_process.go`, `cookie_files.go`
- Modify (citations only): `docs/spec/data-and-storage.md`, `docs/spec/operations.md`, `docs/spec/security.md`, `docs/spec/platform-services.md` — whichever lines the citation test names

**Interfaces:**
- Consumes: nothing new.
- Produces: the identical exported surface (pinned by Step 1's snapshot). Later tasks depend on nothing from this task.

**The move map** (function start lines as of main 60944a4; each function moves WITH its preceding doc comment and any `// ---` section header that introduces only it; types move with their methods; a `var`/`const` block moves with the first function that uses it if nothing else in the remaining file does — otherwise it stays):

| New file | Moves (from `autocookies.go`) |
|---|---|
| `autocookies_setup_slot.go` | the `// --- setup slot lifecycle ---` header (:640) and everything under it to :857: `realSetupBrowserGone` (:713), `browserGoneFrom` (:737), `setupBrowserLiveLocked` (:763), `setupRetainedLocked` (:786), `setupInProgressLocked` (:806), `reapAbandonedSetupLocked` (:831), plus the slot-state vars/consts declared between :640 and :713 |
| `autocookies_status.go` | `AutoCookieStatus` (:230), `GetStatus` (:858), `ReloginStatus` (:916), `LogProfileDirVerdict` (:1091) |
| `autocookies_browser_resolve.go` | `validateBrowserProfileDirForLaunch` (:192), `browserOverrideConfigured` (:935), `resolvedBrowser` (:943), `browserLaunchBlocked` (:1009), `resolvedAcquisition` (:1021), `readOnlyProfileDirErr` (:1046) |
| `autocookies_setup.go` | `StartSetup` (:1154), `SetupResult` (:1271), `FinishSetup` (:1313), `FinishSetupDetailed` (:1320), `CancelSetup` (:1638), `AbandonSetup` (:1698) |
| `autocookies_verdict.go` | `RefreshVerdict.String` (:1746) and the `RefreshVerdict` type/consts it belongs to (grep `type RefreshVerdict`), `RecheckedPlatform` (:1759), `RecheckReport` (:1784), `RefreshResult` (:1825) with `Verdict` (:1908), `HasCredentials` (:1925), `Overall` (:1952), `AnyVerified` (:1970), `refreshDeclined` (:2010), `refreshAborted` (:2016), `verdictOf` (:2021) |
| `autocookies_refresh.go` | `RefreshCookies` (:2049), `refreshCookies` (:2053), `RefreshCookiesDetailed` (:2087), `refreshCookiesDetailed` (:2091, ends :2980) |
| `autocookies_messages.go` | `platformDisplayName` (:2981), `inconclusiveHedge` (:2995), `combinedInconclusiveHedge` (:3028), `cookiesLostMessage` (:3064) |
| `autocookies_periodic.go` | `shouldSkipPeriodicRefresh` (:3109), `periodicRefreshHasSource` (:3142), `notePassCompleted` (:3152), `StartProfileSeed` (:3180), `StartPeriodicRefresh` (:3281) |
| `autocookies_process.go` | the `// --- helpers ---` header (:3374) and: `killProcessTreeUnix` (:3431), `killSetupProcess` (:3494), `killRefreshProcess` (:3524), `cleanup` (:3575), `cleanupLocked` (:3603), `trackedSetupJob` (:3654), `adoptSetupJobLocked` (:3687), `setError` (:3700), `isWindows` (:3707), plus the helper vars/consts between :3374 and :3431 |
| `cookie_files.go` | `writeFileAtomic` (:3762), `sweepCookieTempFilesOnce` (:3829), `sweepStaleCookieTempFiles` (:3851), the DACL memo (`applyUserOnlyDACL` var :3885, `dirTightenState` + consts :3895, `tightenedCookieDirsMu`/`tightenedCookieDirs` :3928) and `tightenCookieDirOnce` (:3932) |
| stays in `autocookies.go` | package doc + imports, `AutoCookieService` (:242) and its field docs, `NewAutoCookieService` (:532), `refreshPlatforms` (:629), `refreshBrowser` (:1114), `FlagManualRelogin` (:1140), `Stop` (:3078) |

Each new file starts with `package cookies`, its imports (only what it needs — `goimports` is not available; add/remove imports by hand until `go build` is clean), and ONE header comment line naming the responsibility, e.g. `// autocookies_verdict.go — the refresh-outcome types and the verdict algebra over them.`

- [ ] **Step 1: snapshot the pin**

```bash
cd D:/Git/Moombox/.worktrees/improvement-i-structure
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
mkdir -p .superpowers/pins
go doc -all ./internal/cookies > .superpowers/pins/cookies-godoc-before.txt
go test -count=1 ./internal/cookies/ 2>&1 | tail -n 1     # expect: ok
```
(`.superpowers/` is gitignored — the pin files are never committed.)

- [ ] **Step 2: create the ten files by moving text**

Use a small Python script (not shell heredocs) that reads `autocookies.go`, cuts each listed function (from the first line of its doc comment to its closing `}`) in the table's order, and writes each group to its new file under a `package cookies` header, leaving the remainder in place. Hand-fix the imports of every file until `go build ./internal/cookies/` is clean. Do NOT edit a single line inside any moved function.

- [ ] **Step 3: verify the moves are pure**

```bash
gofmt -l ./internal/cookies                                   # silent
go build ./... && go vet ./internal/cookies/ && staticcheck ./internal/cookies/ && echo STATIC-OK
go doc -all ./internal/cookies > .superpowers/pins/cookies-godoc-after.txt
diff .superpowers/pins/cookies-godoc-before.txt .superpowers/pins/cookies-godoc-after.txt && echo GODOC-IDENTICAL
git add -A internal/cookies && git diff --cached -M50% --stat | tail -n 15   # every new file should show as a rename/copy from autocookies.go
go test -count=1 ./internal/cookies/
```
Expected: silent gofmt; `STATIC-OK`; `GODOC-IDENTICAL`; `ok`.

- [ ] **Step 4: re-point the doc citations**

```bash
go test -count=1 ./internal/docs/ 2>&1 | grep 'cites' 
```
Every line is of the form `docs/spec/<doc>.md:<line> cites `<symbol>` (`internal/cookies/autocookies.go`), but that file neither declares nor mentions it`. For each, change ONLY the file half of that citation to the new file that now declares the symbol (the table above tells you which). Re-run until the package is `ok`. Do not touch any other doc text.

- [ ] **Step 5: LF check and commit**

```bash
for f in $(git diff --cached --name-only; git diff --name-only); do printf '%s ' "$f"; perl -0777 -ne 'print tr/\r//, "\n"' "$f"; done   # all 0
git add -A internal/cookies docs/spec
git commit -F - <<'MSG'
refactor(cookies): autocookies.go split by responsibility — pure moves

The 3973-line file becomes eleven: the service struct and constructor stay,
and the setup slot lifecycle, status reads, browser resolution, the
interactive setup flow, the verdict types, the headless refresh pass, the
user-facing messages, the periodic scheduler, process cleanup and the cookie
file hygiene each get a file. No identifier, signature or line inside a
function changed: go doc -all is byte-identical and the spec citations that
named the old file now name the file that declares each symbol.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
MSG
```
(If `git commit -F -` misbehaves on this host, write the message to a file under `.superpowers/` and pass that path to `-F`.)

---

### Task 2: split `internal/cookies/refresh.go` by responsibility (pure moves)

**Files:**
- Modify: `internal/cookies/refresh.go` (3638 lines → ≈900)
- Create: `internal/cookies/refresh_update_types.go`, `refresh_auth_status.go`, `refresh_liveness.go`, `refresh_pass.go`, `refresh_youtube.go`, `refresh_cookiefile.go`, `refresh_twitch.go`
- Modify (citations only): the docs the citation test names (`data-and-storage.md` carries most of the `refresh.go` pairs; also `user-interfaces.md`, `platform-services.md`, `operations.md`, `SPEC.md` — SPEC.md is NOT in the checked set, but re-point it too for truth)

**Interfaces:** consumes nothing from Task 1 beyond a clean tree; produces the identical exported surface (Task 1's `go doc` pin recipe is reused; take the "before" snapshot AFTER Task 1's commit).

**The move map** (function start lines as of 60944a4; same move rules as Task 1):

| New file | Moves (from `refresh.go`) |
|---|---|
| `refresh_update_types.go` | `cookieUpdateKey` (:159), `cookieUpdate` (:168), `cookieOrigin` with `covers` (:211) and `platform` (:219), `cookiePlatformOf` (:225) — and the type declarations between :159 and :277 they belong to |
| `refresh_auth_status.go` | `AuthStatus` (:278), `verdictFromCheck` (:337), `twitchAuthLossMessage` (:389), `twitchAuthMark` (:430), `authStatusChanged` (:472) |
| `refresh_liveness.go` | `livenessRecordOf` (:759), `ObserveLiveness` (:1025), `recordLiveness` (:1091), `livenessRefireWindowFor` (:1146), `escalateLivenessRefire` (:1160), `resetLivenessRefire` (:1187), `recordInconclusiveLiveness` (:1224), `noteRecoveryDecided` (:1253), `livenessObservedRecently` (:1265) |
| `refresh_pass.go` | `CheckYouTubeAuth` (:1275), `CheckTwitchAuth` (:1281), `NoteTwitchAuthLoss` (:1322), `doRefresh` (:1400), `refresh` (:1410, ends :1927), `shouldFireRecovery` (:1928), `shouldObserveCredentials` (:1972), `advanceIdentityBaseline` (:2000) |
| `refresh_youtube.go` | `setYouTubeHeaders` (:2008), `youtubeGuideRequestBody` (:2023), `youtubeGuideAuthVerdict` (:2207), `youtubeGuideAuthVerdictFallback` (:2321), `authResponseIsOurs` (:2405), `youtubeGuideExchange` (:2476), `checkYouTubeAuth` (:2551), `checkAndRefreshYouTube` (:2561), `processYouTubeSetCookies` (:2586) |
| `refresh_cookiefile.go` | `hasRowBreakingChar` (:2679), `trackedCookieName` (:2707), `admitSetCookie` (:2885), `updateCookieFile` (:3087), `hasScopedSibling` (:3430), `resolveRowUpdate` (:3466), `sameCookiePlatform` (:3517), `sameCookieScope` (:3528), `isGoogleOnlyAuthName` (:3546) |
| `refresh_twitch.go` | `checkTwitchAuth` (:3554) |
| stays in `refresh.go` | package doc + imports, the consts/vars at the top (:1-:158), `RefreshService` (:482, with its field docs to ~:758), `NewRefreshService` (:848), `SetExpectedPlatforms` (:879), `Start` (:901), `Stop` (:966), `GetStatus` (:977), `CheckNow` (:1006) |

- [ ] **Step 1: snapshot** — as Task 1 Step 1 (`cookies-godoc-before-2.txt`).
- [ ] **Step 2: move** — as Task 1 Step 2, with this table.
- [ ] **Step 3: verify** — as Task 1 Step 3 (`GODOC-IDENTICAL`, `-M50% --stat` renames, `ok`).
- [ ] **Step 4: re-point citations** — as Task 1 Step 4; additionally `grep -n 'refresh.go' SPEC.md` and re-point by hand (not test-checked).
- [ ] **Step 5: LF check and commit**

```
refactor(cookies): refresh.go split by responsibility — pure moves

The 3638-line file becomes eight: the service, its constructor and lifecycle
stay; the cookie-update types, the auth-status verdicts, the liveness
recovery pilot, the refresh pass, the YouTube guide exchange, the Netscape
file rewrite rules and the Twitch check each get a file. go doc -all is
byte-identical; the spec citations follow the symbols.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
```

---

### Task 3: `nico-geometry.js` — the letterbox/rows math leaves `player.js`

**Files:**
- Create: `web/public/modules/nico-geometry.js`, `web/tests/nico-geometry.test.mjs`
- Modify: `web/public/modules/player.js` — `_updateNicoGeometry` (grep `_updateNicoGeometry({ immediate`) and `_commitNicoGeometry` (grep `_commitNicoGeometry(w, h, rows) {`)

**Interfaces:**
- Produces (Task 4 and player.js consume):
```js
// nico-geometry.js
/** Centred-fit stage of a video inside its element box. Before `loadedmetadata`
 *  (videoW/videoH 0) the box itself is the stage. Returns null for a zero-sized
 *  result — callers keep the last good geometry (player.js R11/R12). */
export function letterboxStage({ boxW, boxH, offsetLeft, offsetTop, videoW, videoH })
  // → { left, top, w, h } | null
export function rowsFor(h, rowH)                 // → Math.max(1, Math.floor(h / rowH))
export function sameStage(geo, w, h, rows)       // → geo && geo.width === w && geo.height === h && geo.rows === rows
export function nextGeometry(prev, w, h, rows)   // → { width: w, height: h, laneHeight: h / rows, rows, version: (prev?.version || 0) + 1 }
export const NICO_GEO_SETTLE_MS                  // moved verbatim from player.js (the R23 settle debounce); player.js imports it
```

- [ ] **Step 1: write the failing tests** — `web/tests/nico-geometry.test.mjs`:

```js
import { test } from "node:test";
import assert from "node:assert/strict";
import { letterboxStage, rowsFor, sameStage, nextGeometry } from "../public/modules/nico-geometry.js";

test("a 16:9 video in a square box is letterboxed top and bottom", () => {
  const s = letterboxStage({ boxW: 400, boxH: 400, offsetLeft: 10, offsetTop: 20, videoW: 1920, videoH: 1080 });
  assert.deepEqual(s, { left: 10, top: 20 + Math.round((400 - 225) / 2), w: 400, h: 225 });
});

test("a portrait video in a landscape box is pillarboxed left and right", () => {
  const s = letterboxStage({ boxW: 800, boxH: 450, offsetLeft: 0, offsetTop: 0, videoW: 1080, videoH: 1920 });
  const w = Math.round(1080 * (450 / 1920));
  assert.deepEqual(s, { left: Math.round((800 - w) / 2), top: 0, w, h: 450 });
});

test("before loadedmetadata the element box is the stage", () => {
  assert.deepEqual(letterboxStage({ boxW: 640, boxH: 360, offsetLeft: 5, offsetTop: 6, videoW: 0, videoH: 0 }),
    { left: 5, top: 6, w: 640, h: 360 });
});

test("a zero-sized result is null, never a zero box", () => {
  assert.equal(letterboxStage({ boxW: 0, boxH: 360, offsetLeft: 0, offsetTop: 0, videoW: 1920, videoH: 1080 }), null);
  assert.equal(letterboxStage({ boxW: 0, boxH: 0, offsetLeft: 0, offsetTop: 0, videoW: 0, videoH: 0 }), null);
});

test("rows floor the stage height by the row height and never drop below one", () => {
  assert.equal(rowsFor(450, 24), 18);
  assert.equal(rowsFor(10, 24), 1);
});

test("sameStage compares width, height and rows; nextGeometry bumps the version", () => {
  const g1 = nextGeometry(null, 800, 450, 18);
  assert.deepEqual(g1, { width: 800, height: 450, laneHeight: 25, rows: 18, version: 1 });
  assert.equal(sameStage(g1, 800, 450, 18), true);
  assert.equal(sameStage(g1, 800, 450, 17), false);
  assert.equal(sameStage(null, 800, 450, 18), false);
  assert.equal(nextGeometry(g1, 640, 360, 15).version, 2);
});
```
Run: `node --test web/tests/nico-geometry.test.mjs` → fails (module not found).

- [ ] **Step 2: write the module** — `web/public/modules/nico-geometry.js`:

```js
/**
 * Pure geometry for the niconico overlay: where the picture actually is inside
 * the <video> box, and how many text rows fit. No DOM here — player.js reads
 * the element sizes and writes the styles; this file only does the arithmetic,
 * so it can be pinned without jsdom.
 */

/**
 * Centred-fit stage. The <video> paints its content centred inside its box at
 * the largest scale that fits, so a portrait video in a landscape box gets
 * pillarbox bars (and vice versa). Before `loadedmetadata` the intrinsic size
 * is unknown (0) and the element box is the best available stage.
 * @returns {{left:number, top:number, w:number, h:number}|null} null when the
 *   result would be zero-sized — the caller keeps the last good geometry.
 */
export function letterboxStage({ boxW, boxH, offsetLeft, offsetTop, videoW, videoH }) {
  let w = boxW, h = boxH, left = offsetLeft, top = offsetTop;
  if (videoW > 0 && videoH > 0 && boxW > 0 && boxH > 0) {
    const scale = Math.min(boxW / videoW, boxH / videoH);
    w = Math.round(videoW * scale);
    h = Math.round(videoH * scale);
    left += Math.round((boxW - w) / 2);
    top += Math.round((boxH - h) / 2);
  }
  if (w <= 0 || h <= 0) return null;
  return { left, top, w, h };
}

/** Text rows that fit a stage of height `h` at one line box of `rowH`. */
export function rowsFor(h, rowH) {
  return Math.max(1, Math.floor(h / rowH));
}

/** True when `geo` already describes a stage of exactly this size and row count. */
export function sameStage(geo, w, h, rows) {
  return !!geo && geo.width === w && geo.height === h && geo.rows === rows;
}

/** The geometry record player.js installs; `version` counts installs. */
export function nextGeometry(prev, w, h, rows) {
  return { width: w, height: h, laneHeight: h / rows, rows, version: (prev?.version || 0) + 1 };
}
```
Run the test → 6 pass.

- [ ] **Step 3: make `player.js` call it (no behaviour change)**

Add `import { letterboxStage, rowsFor, sameStage, nextGeometry } from "./nico-geometry.js";` beside the existing `nico-lanes.js` import. In `_updateNicoGeometry`, replace the block from `const bw = video.clientWidth, bh = video.clientHeight;` through `if (w <= 0 || h <= 0) return;` with:
```js
    const stage = letterboxStage({
      boxW: video.clientWidth, boxH: video.clientHeight,
      offsetLeft: video.offsetLeft, offsetTop: video.offsetTop,
      videoW: video.videoWidth, videoH: video.videoHeight,
    });
    // Never write a zero-sized box: the guard above would then refuse every
    // later measurement, wedging the overlay shut. Keep the last good geometry
    // and wait for the next resize instead.
    if (!stage) return;
    const { left, top, w, h } = stage;
```
(keep the letterbox explanatory comment above it, moving the prose to the module doc is fine). Replace `const rows = Math.max(1, Math.floor(h / rowH));` with `const rows = rowsFor(h, rowH);` and the two `geo && geo.width === w && geo.height === h && geo.rows === rows` conditions (one in `_updateNicoGeometry`, one in `_commitNicoGeometry`) with `sameStage(geo, w, h, rows)`. Replace `this._nicoGeo = { width: w, height: h, laneHeight: h / rows, rows, version: (geo?.version || 0) + 1 };` with `this._nicoGeo = nextGeometry(geo, w, h, rows);`.

- [ ] **Step 4: gates**
```bash
cd D:/Git/Moombox/.worktrees/improvement-i-structure && node --test web/tests/*.test.mjs 2>&1 | grep -E '^ℹ (tests|pass|fail)'
git diff --stat web/public/modules/player.js      # removals ≥ additions
perl -0777 -ne 'print tr/\r//' web/public/modules/nico-geometry.js web/tests/nico-geometry.test.mjs web/public/modules/player.js
```
Expected: `tests` = previous count + 6, `fail 0`; CR 0.

- [ ] **Step 5: commit** — `refactor(player): the overlay's letterbox and row math live in nico-geometry.js` + one body sentence + trailers.

---

### Task 4: `NicoScheduler` — the cursor/pending/drop state machine leaves `player.js`

**Files:**
- Create: `web/public/modules/nico-scheduler.js`, `web/tests/nico-scheduler.test.mjs`
- Modify: `web/public/modules/player.js` — the `NICO_*` constants at the top (grep `const NICO_`), the constructor's `nicoCursor`/`nicoDropped`/`_nicoDroppedShown`/`_nicoPending`/`_nicoAnchorMs` fields, `_resetNicoCursor`, `_resetNicoDropCount`, `spawnNicoMessages`, `_countNicoDrop`, `clearNicoOverlay`, `_updateNicoDropPill`, and every other reader of those fields (`grep -n 'nicoCursor\|nicoDropped\|_nicoPending\|_nicoAnchorMs' player.js`)
- Modify (the ONE permitted edit): `web/tests/player.test.mjs` and `web/tests/helpers/player-dom.mjs` — reads of `player.nicoCursor`/`nicoDropped` become `player.nico.cursor`/`player.nico.dropped` (grep first; list every changed line in the report)

**Interfaces:**
```js
// nico-scheduler.js
export const NICO_DURATION_MS, NICO_LANE_GAP_MS, NICO_MAX_LATENESS_MS, NICO_LEAD_MS, NICO_TICK_AHEAD_MS,
             NICO_MAX_PER_TICK, NICO_SEED_MAX_FALLBACK;   // moved verbatim from player.js, same values (NICO_GEO_SETTLE_MS moved to nico-geometry.js in Task 3)
export class NicoScheduler {
  constructor({ lanes, indexAfter, seedCursorIndex,
                leadMs = NICO_LEAD_MS, maxLatenessMs = NICO_MAX_LATENESS_MS, tickAheadMs = NICO_TICK_AHEAD_MS,
                maxPerTick = NICO_MAX_PER_TICK, seedMaxFallback = NICO_SEED_MAX_FALLBACK })
  cursor      // number; -1 = un-anchored
  anchorMs    // number
  pending     // Array<entry>; entries are opaque to the scheduler except `entry.msg.offsetMs`
  dropped     // number
  anchor(messages, effectiveMs)   // the _resetNicoCursor rule; clears pending; lanes.reset()
  unanchor()                      // cursor = -1; pending = []   (the hidden-panel path — caller clears the overlay first)
  resetDropCount()                // dropped = 0
  countDrop(msg)                  // R2: if (msg.offsetMs - leadMs > anchorMs) dropped++
  tick(messages, effectiveMs, { prepare, place, discard })  // steps 1–2 of spawnNicoMessages; returns { placed, deferred, skipped }
}
```
`player.js` constructs `this.nico = new NicoScheduler({ lanes: this._lanes, indexAfter, seedCursorIndex })` in the constructor and imports the constants from the new module.

- [ ] **Step 1: write the failing tests** — `web/tests/nico-scheduler.test.mjs`, pure (no DOM), at production constants. A fake allocator `{ laneCount: 17, reset(rows) { this.resets++; }, allocate() { return this.refuse ? -1 : 0; } }`, `indexAfter` and `seedCursorIndex` imported from the real modules (`chat-timeline.js`, `nico-lanes.js`), messages `[{offsetMs}]`, hooks recording calls (`prepare` returns `{ msg }` unless the message is `{ system: true }` → null; `place` returns `!refuse`; `discard` records).

  1. **anchor seeds within the lateness window and at most 2×rows** — fixture A: messages every 100 ms from 0 to 60 000 (601 messages), lanes.laneCount = 17; `anchor(msgsA, 30000)` → `cursor === indexAfter(msgsA, 29000)` (the window 31000 − 2000 holds 20 messages, under the cap of 34). Fixture B: 200 messages all at offset 29 500 plus one at 40 000; `anchor(msgsB, 30000)` → `cursor === indexAfter(msgsB, 31000) − 34` (the count cap wins). Both expected values are computed in the test from the real `indexAfter`, not hard-coded.
  2. **the tick consumes up to tick-ahead early and places at first sight** — `anchor(msgs, 0)`; `tick(msgs, 250)` places every message with `offsetMs - 1000 <= 550`; `placed` equals that count; the cursor advanced exactly that far.
  3. **a too-late message is skipped and counted only past the anchor, and skips are free** — anchor at 0 with 500 messages all at offset 0 (backlog): tick at 5000 → cursor === 500 after ONE tick (skips do not consume `work`), `dropped === 0` because `offsetMs - lead (= -1000) <= anchorMs (0)`; then one more message at 3000: tick at 6000 → dropped === 1.
  4. **the per-tick cap leaves the cursor put** — 40 placeable messages at the current time; `tick` places `NICO_MAX_PER_TICK`, `cursor` advanced by exactly that many; a second tick places the rest.
  5. **an unplaceable entry is deferred, retried oldest-first, and dropped when too late** — allocator refuses: tick → `deferred === n`, `pending.length === n`, `discard` NOT called yet; allocator accepts: next tick places the pending in order (hook call order asserted) BEFORE any new message; then with refusal again advance time past `maxLatenessMs` → pending dropped, counted, `discard` called for each.
  6. **unanchor + re-anchor after a seek counts nothing from the gap (the Phase 3+4 F1 scenario)** — anchor at 0, tick at 1000; `unanchor()`; `anchor(msgs, 30000)`; tick at 30250 → `dropped === 0` and every placed message has `offsetMs ≥ 30000 + 1000 - 2000`.
  7. **system-only messages consume the cursor but place nothing** — `prepare` returns null → not placed, not deferred, cursor advanced.

  Run: `node --test web/tests/nico-scheduler.test.mjs` → fails (module not found).

- [ ] **Step 2: write the module** — move the seven remaining `NICO_*` constants verbatim (with their comments) from `player.js` into `nico-scheduler.js` as exports, then:

```js
export class NicoScheduler {
  constructor({ lanes, indexAfter, seedCursorIndex, leadMs = NICO_LEAD_MS, maxLatenessMs = NICO_MAX_LATENESS_MS,
                tickAheadMs = NICO_TICK_AHEAD_MS, maxPerTick = NICO_MAX_PER_TICK, seedMaxFallback = NICO_SEED_MAX_FALLBACK }) {
    this.lanes = lanes; this.indexAfter = indexAfter; this.seedCursorIndex = seedCursorIndex;
    this.leadMs = leadMs; this.maxLatenessMs = maxLatenessMs; this.tickAheadMs = tickAheadMs;
    this.maxPerTick = maxPerTick; this.seedMaxFallback = seedMaxFallback;
    this.cursor = -1; this.anchorMs = 0; this.pending = []; this.dropped = 0;
  }
  /** (doc comment moved verbatim from player.js _resetNicoCursor) */
  anchor(messages, effectiveMs) {
    const rows = this.lanes.laneCount || this.seedMaxFallback / 2;
    this.cursor = this.seedCursorIndex(messages, effectiveMs + this.leadMs, this.maxLatenessMs, 2 * rows, this.indexAfter);
    this.pending = [];
    this.anchorMs = effectiveMs;
    this.lanes.reset();
  }
  unanchor() { this.cursor = -1; this.pending = []; }
  resetDropCount() { this.dropped = 0; }
  /** (doc comment moved verbatim from _countNicoDrop) */
  countDrop(msg) { if (msg.offsetMs - this.leadMs > this.anchorMs) this.dropped++; }
  entryMs(msg) { return msg.offsetMs - this.leadMs; }
  tooLate(msg, effectiveMs) { return effectiveMs - this.entryMs(msg) > this.maxLatenessMs; }
  /** (the two numbered comment blocks from spawnNicoMessages move here verbatim) */
  tick(messages, effectiveMs, { prepare, place, discard }) {
    let placed = 0, deferred = 0, skipped = 0;
    const stillPending = [];
    for (const entry of this.pending) {
      if (this.tooLate(entry.msg, effectiveMs)) { this.countDrop(entry.msg); discard(entry); skipped++; continue; }
      if (place(entry, effectiveMs, true)) placed++; else stillPending.push(entry);
    }
    this.pending = stillPending;
    let work = 0;
    while (this.cursor < messages.length && this.entryMs(messages[this.cursor]) <= effectiveMs + this.tickAheadMs) {
      const msg = messages[this.cursor];
      if (this.tooLate(msg, effectiveMs)) { this.cursor++; this.countDrop(msg); skipped++; continue; }
      if (work++ >= this.maxPerTick) break;
      this.cursor++;
      const entry = prepare(msg);
      if (!entry) continue;
      if (place(entry, effectiveMs, false)) placed++; else { this.pending.push(entry); deferred++; }
    }
    return { placed, deferred, skipped };
  }
}
```
NOTE for the implementer: in player.js today a too-late PENDING entry is simply dropped (its element is already detached) — `discard(entry)` exists so the caller can keep that exact behaviour (a no-op hook in player.js) and so the test can observe it. Run the tests → 7 pass.

- [ ] **Step 3: `player.js` uses the scheduler** — constructor: `this.nico = new NicoScheduler({ lanes: this._lanes, indexAfter, seedCursorIndex });` and DELETE the fields `nicoCursor`, `nicoDropped`, `_nicoPending`, `_nicoAnchorMs` (keep `_nicoDroppedShown` — pill state is DOM-side). `_resetNicoCursor(effectiveMs)` body → `this.nico.anchor(this.playerChatMessages, effectiveMs);`. `_resetNicoDropCount` → `this.nico.resetDropCount();` + the existing pill lines. `_countNicoDrop` → delete (callers use `this.nico.countDrop`). `clearNicoOverlay`: the `this._nicoPending = [];` line → `this.nico.pending = [];` (keep the comment). `spawnNicoMessages`: keep the four guards and the geo check exactly; the hidden-panel branch's `this.nicoCursor = -1;` → `this.nico.unanchor();`; the lazy anchor `if (this.nicoCursor < 0)` → `if (this.nico.cursor < 0)`; replace steps 1–2 (from `const stillPending = [];` to the end of the `while`) with:
```js
    this.nico.tick(messages, effectiveMs, {
      prepare: (msg) => this._prepareNico(msg, ctx),
      place: (entry, at, retry) => this._placeEntry(entry, at, ctx, retry),
      discard: () => {}, // the entry's element is already detached (see _placeEntry)
    });
```
`_updateNicoDropPill`: `this.nicoDropped` → `this.nico.dropped` (both reads). Any other `nicoCursor`/`nicoDropped` reader (grep — `onPlayerJobSelect` sets `nicoCursor = -1` per R27; the `seeking` listener per Phase 3+4 F1; the toggle-OFF handler) → `this.nico.unanchor()`. Import the constants from `./nico-scheduler.js` (delete the local `const NICO_*` block).

- [ ] **Step 4: the permitted test edit** — `grep -n 'nicoCursor\|nicoDropped\|_nicoPending' web/tests/*.mjs web/tests/helpers/*.mjs`; change each read to the `nico.` form; nothing else in those files changes. List every changed line in the report.

- [ ] **Step 5: gates** — `node --test web/tests/*.test.mjs` (count = previous + 7, fail 0 — the jsdom suite must run, not skip: `npm ci` in web/tests first if needed); `git diff --stat web/public/modules/player.js` (removals ≥ additions); `grep -c 'NICO_' web/public/modules/player.js` still > 0 (uses) and `grep -c '^const NICO_' web/public/modules/player.js` = 0; CR 0 on every touched file.

- [ ] **Step 6: commit** — `refactor(player): NicoScheduler owns the overlay cursor, pending list and drop count` + body: "The state machine that spawnNicoMessages, _resetNicoCursor and _countNicoDrop implemented (rulings R2/R7/R9/R10/R21/R29 of the player ledger) is now a pure class with prepare/place/discard hooks, pinned by seven node tests at production constants — including the seek scenario the 2026-09-03 review found by hand. player.js keeps the DOM guards, elements, WAAPI and the pill; behaviour is unchanged." + trailers.

---

### Task 5: a jsdom harness for `app.js` that pins what the extractions must keep

**Files:**
- Create: `web/tests/helpers/app-dom.mjs`, `web/tests/app.test.mjs`
- Modify: `web/public/app.js` — ONE production edit: `class MoomboxApp {` → `export class MoomboxApp {` (the file is loaded as an ES module; an export changes nothing in the browser). Nothing else.

**Interfaces (Tasks 6–10 consume):**
```js
// app-dom.mjs (mirrors helpers/player-dom.mjs — read it first and reuse its fake HTTP, manual timers and window bookkeeping; import or copy, do not rewrite the design)
export async function makeApp({ routes = {}, initialState = {} } = {})
  // → { app, window, document, http, el(id), advance(ms), flush(), teardown() }
  // Builds a JSDOM window from the WHOLE <body> of web/public/index.html (the app touches every panel),
  // installs window/document/localStorage/matchMedia/WebSocket(fake)/fetch(fake) on globalThis,
  // answers GET /api/setup/status → { isFirstRun: false, ffmpegValid: true }, GET /api/config → initialState.config || {},
  // GET /api/status → initialState.status || {}, then imports "../../public/app.js" DYNAMICALLY (after the globals exist)
  // and constructs `new MoomboxApp()`; awaits `flush()` so init() has run.
export function teardownAll()
```
The fake WebSocket never connects (`readyState = 3`, `send` records); `connectWebSocket` must not throw. Every stub carries a one-line reason like player-dom.mjs.

- [ ] **Step 1: write the failing tests** — `web/tests/app.test.mjs`, jsdom-probed and skippable exactly like player.test.mjs's first 25 lines. Pins (each reads the RENDERED result, never the implementation):
  1. `renderJobItem(job, containerKey)` — for one fixture job in each of `Upcoming`, `Live`, `Downloading` (with progress `"V:1234 A:1234 C:5678"`), `Muxing`, `Finished`, `Error`, `Cancelled`, `COOKIES?`, `Queued`: the returned/inserted element's `outerHTML` (after `escapeHtml`) equals a snapshot string stored in `web/tests/fixtures/app-job-items.json` (write the fixture by running the current code ONCE and pasting; the test compares strictly). This is the byte-identical pin for Task 10 (job details) and any later job-list move.
  2. `renderOrphanedFiles(files)` and `renderOrphanedHistory(rows)` with two-item fixtures → `#orphaned-files-list` / `#orphaned-history-list` (grep the real ids) `innerHTML` snapshots in the same fixture file.
  3. `updateVersionIndicator()` with `_updateAvailable = { version: "9.9.9" }` and with null → the indicator element's text/hidden state.
  4. `addLog({level:"warn",…})` ×3 with `logFilter = "warn"` then `renderLogs()` → the log container's child count and text.
  5. the unified filter bar: `app.tasksFilterTokens = parseFilterQuery("status:live -platform:twitch")` then whatever method renders the chips (grep `_setupUnifiedFilter` for the render call) → chip labels via `_filterTokenLabel`.
  6. one delegation smoke test: `app.showToast("x", "primary")` creates an `sl-alert` (Shoelace is not loaded — assert on the element name only).
  Run: `node --test web/tests/app.test.mjs` → fails (harness missing / MoomboxApp not exported).

- [ ] **Step 2: the export and the harness** — add `export` to the class; write `app-dom.mjs`; iterate until the six tests pass. Where the app calls a browser API jsdom lacks (e.g. `HTMLElement.prototype.scrollIntoView`, `navigator.clipboard`), stub it in the harness with a reason comment — never change app.js for the harness's sake.

- [ ] **Step 3: gates** — `node --test web/tests/*.test.mjs` (count = previous + 6, fail 0, and the six ran — not skipped); `git diff web/public/app.js` shows exactly one changed line; CR 0 on every new file.

- [ ] **Step 4: commit** — `test(web): a jsdom harness pins app.js rendering before the controller extractions` + trailers.

---

### Tasks 6–10: one controller extraction per task (same recipe; the table is the spec)

**Recipe (applies to each of Tasks 6–10):**
1. Create `web/public/modules/<file>` with `export class <Controller> { constructor(app) { this.app = app; … } … }` in the `StatsController` shape; move every method in the task's list VERBATIM (body unchanged; `this.<field>` that belongs to the feature becomes the controller's own field, initialised in its constructor with the same initial value the `MoomboxApp` constructor used; `this.<app-wide thing>` becomes `this.app.<thing>`).
2. In `MoomboxApp`: construct it beside the other controllers (`this.files = new FilesController(this);`), delete the moved methods and the moved fields, and move the feature's listener block out of `setupEventListeners` into the controller's `bind()` method (called from `initializeApp` where `setupEventListeners` runs today — keep the call order).
3. Keep a one-line delegation on `MoomboxApp` ONLY for methods that another file calls through `app.` (the call map: `showToast`, `setInputValue`, `getInputNumber`, `escapeHtml`, `getInputValue`, `loadConfig`, `showConfirm`, `loadStatus`, `parseTimeInput`, `copyTextToClipboard`, `createTrim`, `checkMonitorsNow`, `applyAuthStatus`, `updateVersionIndicator`, `_updateJobResumePosition`, `initializeApp`, `autoCookieRefresh`; properties `config`, `js`, `selectedJobId`, `player`, `backfillStatus`, `activePlatforms`, `_uptimeSeconds`, `_uptimeCapturedAt`, `setup`, `_updateAvailable`) — `grep -rn "app\.<name>" web/public/modules/ web/public/index.html` before deciding; if nothing outside app.js calls it, no shim.
4. Every `this.<moved method>(…)` call left in app.js becomes `this.<controller>.<method>(…)`; grep for each moved name until `node --test` is green and `grep -n "this\.<method>(" web/public/app.js` finds only the intended shim.
5. Gates: `node --test web/tests/*.test.mjs` (count unchanged, fail 0 — the pins from Task 5 are the proof of byte-identical output); `git diff --stat web/public/app.js` (removals ≥ additions); `grep -c "^  [a-zA-Z_]*(.*) {$" web/public/app.js` decreased by the number of moved methods (minus shims); CR 0.
6. Commit: `refactor(web): <Controller> owns <feature>; app.js delegates` + one body sentence naming the moved methods + trailers.

| Task | New file / class | Methods that move (app.js line ranges as of 60944a4) | Fields that move | Listener block |
|---|---|---|---|---|
| 6 | `files.js` / `FilesController` (field `this.files`) | `fetchOrphanedFiles` (:4524), `renderOrphanedFiles` (:4542), `deleteOrphanedFile` (:4604), `deleteAllOrphanedFiles` (:4626), `fetchOrphanedHistory` (:4665), `renderOrphanedHistory` (:4683), `deleteOrphanedHistory` (:4729), `deleteAllOrphanedHistory` (:4749) | none app-wide (grep the bodies for `this._orphan*`) | the Files-tab buttons and the `sl-tab-show` branch that triggers the fetch (grep `fetchOrphaned` in `setupEventListeners` and the tab handler) |
| 7 | `log-panel.js` / `LogPanelController` (field `this.logPanel`) | `addLog` (:3587), `_createLogLine` (:3625), `getFilteredLogs` (:3660), `renderLogs` (:3675), `clearLogs` (:3708) | `logs`, `logFilter`, `_logAutoScroll`, `_logSearchQuery` | log filter select, clear button, auto-scroll toggle, search input (grep `logFilter`, `clearLogs`, `_logAutoScroll`, `_logSearchQuery` in `setupEventListeners`) — `addLog` keeps a shim (called from modules) |
| 8 | `update-indicator.js` / `UpdateController` (field `this.updates`) | `updateVersionIndicator` (:1067), `showUpdateDialog` (:1117), `applyUpdate` (:1140), `dismissUpdate` (:1174) | `_updateAvailable` (settings.js reads `app._updateAvailable` → keep a getter shim `get _updateAvailable() { return this.updates.available; }`) | the version-indicator click and dialog buttons; `updateVersionIndicator` keeps a shim (settings.js calls it) |
| 9 | `filter-bar.js` / `FilterBarController` (field `this.filterBar`) | `_setupUnifiedFilter` (:3858), `_filterTokenLabel` (:4126), `getFilteredJobs` (:3826), `getFilteredArchivedJobs` (:3830) | `tasksFilterTokens`, `archivedFilterTokens`, `_tasksChannels`, `_archivedChannels` — app.js readers (`renderJobs`, `renderArchivedJobs`, keyboard `/`) go through `this.filterBar.tokens("jobs"|"archived")` | the whole `_setupUnifiedFilter` IS the listener block |
| 10 | `job-details.js` / `JobDetailsController` (field `this.details`) | `showJobDetails` (:2350), `_fetchStagingFields` (:2366), `updateJobDetails` (:2386), `updateDetailsButtons` (:2540), `renderJobDetails` (:2580), `loadJobLogs` (:2936), `_refreshJobDetails` (:3547), `_preserveStagingFields` (:4952) | `selectedJobId` STAYS on app (modules read it); details-only caches (grep `_details*`, `_staging*`) move | the details-panel buttons (grep `showJobDetails`, `loadJobLogs` in `setupEventListeners`) |

Order: 6 → 7 → 8 → 9 → 10 (smallest blast radius first; Task 10 is last because `renderJobDetails` is 350 lines and touches the most app state — dispatch it on opus, the others on sonnet).

---

### Task 11: docs for the new structure

**Files:** `docs/spec/architecture.md` (the package/module inventory rows: the ~17 new cookies files summarised as one row each per responsibility group, not one per file; the two new JS modules; the five controllers), `docs/spec/user-interfaces.md` (the Web modules table if it lists modules), `README.md` only if it lists frontend modules (grep `modules/`), `web/tests/README*` if present.
- Gate: `go test -count=1 ./internal/docs/`; `node --test web/tests/*.test.mjs`. Commit `docs: the cookies package, player overlay and web app after the Arc I split`.
- Line counts are Arc K's `appendix-metrics.md` regen — do not touch it here.
