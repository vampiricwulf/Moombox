# Cookie Subsystem Remediation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Moombox detect and truthfully report a dead YouTube session before a recording fails, then work through the remaining 47 cookie-subsystem findings in dependency order.

**Architecture:** The audit found 52 findings across seven areas that are not one subsystem. This document is a **roadmap covering all 52**, plus **Arc 1 in full executable detail**. Arcs 2-9 are scoped here and become their own plan documents when picked up — each arc produces working, testable software on its own. Arc 1 replaces presence-based cookie health checking with liveness checking: it fixes the two gates that currently silence the health check, harvests a liveness probe Moombox already makes and discards, adds a channel-independent fallback probe, and only then unblocks the notification — so the first alert an operator receives is both true and about the right thing.

**Tech Stack:** Go 1.26, modernc/sqlite, chi/v5, bubbletea (TUI), vanilla JS + Shoelace (Web UI). No CGo.

**Spec:** `reports/cookies-observations.md` (gitignored — regenerate or ask if absent). Finding IDs below refer to that document.

---

## EXECUTION STATUS — updated 2026-08-29 (Arc 9 merged; `main` @ `dd5dd18`)

> **CURRENT STATE, read this first.** `main` = **`dd5dd18`**. Merged in order: Arc 0 `5f0b69d` · Arc 1 `8ce558d` · follow-ups `1f90bd1` · F1 `5850ec2` · Arc 2 `f394cf9` · Arc 3 `2be52a5` · Arc 4+7 `f2b4e30` · Arc 6 `1a0f0ff` · Arc 5 `5eb5266` · Arc 8 `8389009` · **Arc 9 `dd5dd18`** (Arc 9 MERGED 2026-08-29, `--no-ff`, branch `cookie-arc9-docs-tests` deleted, post-merge gates build/vet/linux/gofmt + 27/0, the three arc-close edits landed as `7599ada`; Arc 8 MERGED 2026-08-29, `--no-ff`, branch deleted, post-merge gates 27/0; Arc 5 likewise merged 2026-08-29 —; post-merge gates 27/0; nine commits: `0c40e4d` · `72f4373` · `d5099a5` · `37eb62e` · `7e742b2` · `387d24a` · `2727c29` · `dccb5e5` · `db1993a`; arc-close review 2026-08-29 — see the Arc 5 paragraph and `progress-arc5.md`). **Arc 9 is MERGED (`dd5dd18`); arc-close reviewed 2026-08-29 (`arc9-arc-close-review.md`: MERGE AFTER NAMED EDITS — three one-clause doc edits, landed as `7599ada`) — MERGED as `dd5dd18`** — branch `cookie-arc9-docs-tests` (deleted) from `8389009` @ `7599ada` plus this plan edit (18 commits + 1; docs and tests only; see the Arc 9 paragraph and `progress-arc9.md`). The owner's `mux-wait-chat-idle` worktree is now merged too (`8437c86`, 2026-08-29, reviewed; was `376ff90`, one commit off `5850ec2`), which the owner ruled merges AFTER Arc 9 (Q2). **Nothing is pushed** — `origin/main` is still at `613ae12`; pushing, tagging and releasing are the owner's calls.
>
> **Remaining sequence (owner-ruled 2026-08-29, Q1-Q13 in `progress-arc9.md`; supersedes "arm the pilot → Arc 9 → final review"):** Arc 9 merge — DONE `dd5dd18` → `mux-wait-chat-idle` merge — DONE `8437c86` → Arc 10 (Twitch chat + HLS credential lifecycle) → Arc 11 (Docker re-auth ingest) → A1-Linux reap (own small arc) → Arc 12 planning (V9, G3/G4, Twitch tier-2 probe, T3 key audit — one or several plans by file map) → housekeeping (back-off; horizons + `login` expiry on the startup line WITH the doc absence-claim edits; A2 narrowed; `types.go` comment; gitattributes + renormalize; citation-rot test incl. absence claims and headings; `feedbackSev` fold; `refresh.go` `authStatusChanged` contract pin) → `RELEASE_NOTES.md` draft → final whole-plan review (deletes this plan) → owner: field-test plan, arming, bump, tag, push.
>
> **`auto_enabled`'s settled meaning (owner, 2026-08-28, ten corrections — full record in `progress-arc6.md`):** two independent liveness mechanisms on two independent timers, **not** a primary and a fallback. The in-process Go refresh always runs, with the monitors and its own timer. The headless-browser refresh is a **much slower** second timer that exists only when the flag is on. **The flag owns that timer, the one automatic recovery attempt, and — the exception the table must name — G5's `SetExpectedPlatforms` read at `main.go:276`.** `R F` / Web shift+click / the Settings-page twin are a three-rung ladder that the **flag** never causes to decline and that is never gated on the flag (it still declines nil-error on the running-service causes in `RefreshDeclinedCauses` — stopped / busy — unchanged and correct); `R C` is never gated. **The periodic timer is `gateExempt`:** flipping the flag OFF at runtime leaves it launching browsers until restart, by ruling (F2 → option (a)) — the restart-required label both UIs now carry is the honest cover; do not "fix" it. Every **automatic** browser-free import runs only when there is no `cookies.txt` to lose (`automaticImportGuard`: absent or zero rows; unreadable ABORTS); **manual** triggers import regardless; the **recovery** path is exempt because it fires only when the credentials are already known dead. The boot seed is `StartProfileSeed`, called unconditionally from `main.go`, once per boot, guard-gated (`shouldSeedFromProfileAtStartup` is deleted).
>
> **Every `internal/cookies/*.go` line number below is stale unless stamped `37eb62e`.** `autocookies.go` drifted +9 to +118 lines during Arc 4+7 and a further ~+36 to +280 during Arc 6; **`jar.go` was rewritten by Arc 5 (+419 lines, two per-platform jars — every citation into it that predates `72f4373` describes deleted code).** Arc 5 has NOT touched `autocookies.go`, `refresh.go`, `main.go` or the anchors below (its `services.go` change is the startup log only). Anchors re-derived at **`37eb62e`**: `GetStatus:642`, `StartSetup:785`, `FinishSetup:924`, `FinishSetupDetailed:931`, `CancelSetup:1234`, `AbandonSetup:1290`, `cleanup:2731`, `cleanupLocked:2757`, `trackedSetupJob:2800`, `tightenCookieDirOnce:2949`; `verdictFromCheck` at `refresh.go:236`; **`const livenessRecoveryArmed = false` at `refresh.go:496`** (`:606` at `8389009`; **`:632` at `97cfecf`**, still false); G5's gate at `main.go:276-278`. Re-derive anything not listed — three separate tasks have found citations off by 30-120 lines mid-execution.
>
> - **F1 — MERGED, the last hard precondition for arming is CLOSED** (`5850ec2`; `e7a61cf` + two fix rounds + one controller round, branch deleted). An unrecognisable 200 guide body is now **inconclusive**, not a conclusive "not authenticated"; the inconclusive error deliberately does not wrap `ErrAuthCheckNotAttempted`. Reviewed **PASS / PASS, no blocking findings**. Two measurement-driven design changes the plan did not anticipate: an anonymous reply carries **no `loggedIn` key at all** — it sends `"loggedOut":true`, so the old bool read false from *absence*, which a bool cannot distinguish from a real negative (both fields are now `*bool`); and the string fallback's `"logged_in":"1"` needle **could never have matched real wire data** (the params are objects). **The `refresh.go` dispatch bar is lifted — but every line number in that file must now be re-derived at `5850ec2`.**
> - **Arcs 2-9 were re-derived at `1f90bd1`** on 2026-08-27 — see the rewritten Arcs 2-9 section and `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/fable-arcs2-9-review.md`. Arcs 4 and 7 merge; S4 and A3's cdpNavigate half are already fixed and drop out; Arc 6 moves ahead of Arc 5 as an arming precondition.
> - **Arc 5 (2026-08-28) changed the ground under every other arc** — the cookie jar is now two in-memory jars (`youtube`/`twitch`, one `cookies.txt` file, no write path changed); `GetCookieHeader()` is YouTube-only; `GetCookie(name)` reads the TWITCH jar (sole consumer `twitch/auth.go:40`); expiry is captured (`cookieEntry.expiry`, `ExpiredAuthCookiesFor`/`AuthCookieHorizonFor`, `expiredYouTubeAuth`/`expiredTwitchAuth` on the startup log) but NEVER filtered — `rowExpired` in `autocookies_merge.go` remains the only pruner; `isEssentialCookie`'s first clause is domain-guarded; chat/IRC/VOD-chat credentials are `func() string` getters; and Twitch IRC had NEVER authenticated (unconditional `justinfan` NICK) — fixed at `7e742b2` + `dccb5e5` + `db1993a`: NICK from the `login` cookie through ONE paired accessor, with a one-shot anonymous fallback when Twitch refuses the handshake. The sections below were audited against `37eb62e` on 2026-08-28 and corrected inline where they said something the code no longer supports.
> - **The paragraph below is history from before those merges.** Where it says "unmerged", read the branch names as commit provenance, not as work outstanding.

**Arc 0 is COMPLETE and MERGED** (`main` @ `5f0b69d`, commits `613ae12..97e667b` plus the live-gate fix `237fc7b`). **Arc 1 is COMPLETE through Task 7** on branch `cookie-arc1-liveness` (**since merged as `8ce558d`**) — Tasks 0-7 at `ffea847` (19 commits), then the post-review fix round landed `9c715d8` (tier-1 guide checks gain the same answered-by-our-request provenance guard the page probes have) and `cc51b81` (pilot-gate rationale + two cost comments corrected), then the audit/B-fix round landed four more: `61f36bf` (B1 — the marker VALUE reader: LoggedOut only from an explicit false, unreadable → Unknown), `ad86767` (B4 — the startup cookie log names what it measures: `completeAuthSet`/`anyAuthCookie`), `ad7451e` (B2 — a declined recovery sends NOTHING and does not stamp the cooldown; B3 — an inconclusive fallback probe now logs once per change at Info; B5/B6 comment corrections; pre-colon whitespace tolerated in the marker KEY), and `4021a74` (four falsified-claim corrections; **`sessionAuthFromBytes` DELETED** — `livenessVerdict` is now the only byte-side reader, and the allocation pin merged into `TestLivenessVerdictDoesNotAllocate`), putting the branch at **`4021a74`, 25 commits** as of 2026-08-27. Acceptance **A1/A2/A3 PASSED end to end 2026-08-27** against a throwaway instance with owner-confirmed Discord delivery; **A4/A5 are still open** (see the acceptance section for what a 2026-08-27 source review established about each). The liveness pilot is landed **log-only** — `const livenessRecoveryArmed = false` in `internal/cookies/refresh.go`, re-confirmed at `4021a74` (then `refresh.go:355`; **`refresh.go:496` at `37eb62e`**, unflipped through every merge since). Arcs 2-9 remain unexecuted, but their scope was re-verified against `1f90bd1` on 2026-08-27 — see the note at the top of this section.

The Arc 0 and Arc 1 task sections below are now **execution history, not pending work**. The ledgers are the authority on what actually happened, including every ruling and overturn:
`.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress.md` (Arc 0) and `progress-arc1.md` (Arc 1).

Task → commit map (Arc 1): T0 `f16546a` · T1 `86544a9` · **T1b `bede4ef` (added in execution — Twitch validate mirror)** · T2 `70cd8cc`+`a439d4d` · **T2b `2198646`+`3067796`+`11fe29e` (added in execution — autocookies strict-presence sites)** · T3 `ce96411` · T4 `268fca9`+`f19b8a6` · T5 `ca1abba`+`e5e5c95`+`f702653` · T6 `9692561`+`6c89b9f` · T7 `4866f3a`+`9b11ac7`+`ffea847` · **whole-branch review round** `9c715d8`+`cc51b81` · **audit/B-fix round** `61f36bf`+`ad86767`+`ad7451e`+`4021a74`.

**Line-number citations in the Arc 0/Arc 1 sections predate 25 commits of churn and have NOT been re-verified except where a note stamps them at `cc51b81` or `4021a74`.** Anyone re-deriving from this document must re-check citations against source — three separate tasks found them off by 30-90 lines during execution, and the audit/B-fix round moved `internal/cookies/refresh.go` and `internal/youtube/watch_page.go` again after the `cc51b81` stamps.

---

## Global Constraints

- **Every goroutine MUST have an inline `defer func() { if r := recover(); ... }()`.** Non-negotiable project rule.
- **Logger is an anonymous interface repeated per struct** (`Debug/Info/Warn/Error(msg string, args ...any)`). Do NOT extract to a named interface.
- **Database writes go through `db.UpdateJobFields(id, map[string]any{...})`.**
- **API routes use the `/api/` prefix, no version.** Route registration and frontend fetch calls must stay in sync.
- **Web UI assets are `go:embed`-ed** — frontend changes require `go build` to take effect.
- **Never log a cookie value or session token.** The subsystem is currently clean on this; keep it that way. `YouTubeIdentity()` is hashed and must stay opaque.
- **Owner decision, 2026-08-25: liveness over presence.** *"We don't care if a cookie exists if it doesn't work."* Any health signal that answers "is a cookie there" rather than "does it work" is not the goal.
- **Build/test:** `go build ./...`, `go test ./...`, `go vet ./...`. Runtime needs FFmpeg on PATH (not needed for these tests).
- **Do NOT bump the version or tag.** Release timing is the owner's call.

---

## Coverage map — all 52 findings

Every finding is assigned. Nothing is dropped.

| Arc | Findings | Deliverable |
|---|---|---|
| **0. Make the browser refresh real** — **DONE, merged** | F1, F2, F3, F5 | The headless refresh either works or says it didn't — **field-confirmed broken, actively costing the owner sessions** |
| **1. Health checking that tells the truth** — **DONE, merged (`8ce558d`, follow-ups `1f90bd1`, F1 `5850ec2`); pilot log-only** | V2, V3, V11, V12*, G6, G5† | A default install detects a dead session and says so, truthfully — **A1/A2/A3 field-proven; A4 source-established; A5 must be re-run at arming** |
| **2. Write-path data loss** — **DONE, merged (`f394cf9`)** | S7, S9, S10, X2(partial) — ~~S4~~ fixed in Arc 0 | No path silently destroys working credentials — **goal met; all four writers verified by grep** |
| **3. Setup lifecycle** — **DONE, merged (`2be52a5`)** | S1, A1, S16, S5, S18 — S17 moved to 4+7 | Closing the setup browser can no longer wedge acquisition — **met on Windows, both browser families; NOT met on Linux/Docker (named residual, needs an owner)** |
| **4+7. Three-state, and what the UIs say** — **DONE, merged (`f2b4e30`)** | A3, H4(remainder), S12, S15, S17, V4, V5, V6, V10, S14 — **ten findings, not nine** | "Couldn't check" stops being reported as "failed", and both UIs say so — **goal met; all twelve verdict-rendering surfaces traced** |
| **5. Transport and identity** — **DONE, MERGED `5eb5266`** (nine commits from `cookie-arc5-transport-identity` @ `db1993a`, branch deleted; arc-close review 2026-08-29) | T2, T3, T1, V1 — **T2 CLOSED (expiry captured, reported per platform, never filtered); T3's cross-platform half + T1 tier 1 CLOSED by the jar partition (`72f4373`) and the write-path guard (`37eb62e`); T1 tier 2 REJECTED (live getter instead, `387d24a`); V1 CLOSED on all six credential sites (`d5099a5` + `387d24a`); +IRC had never authenticated — paired accessor + one-shot anonymous fallback (`7e742b2`/`dccb5e5`/`db1993a`); +Twitch live gate (`2727c29`)** | One cookie identity; no cross-site credential leak — **goal met on the jar and every downloader; three field gates open (badges, credentialed reconnect, end-to-end archive per platform); one NEW silent Twitch state found at the close, carried to Arc 8** |
| **6. Untangling `auto_enabled`** — **DONE, merged (`1a0f0ff`)** | G1, G2, S13, V7, V8, X1, X5 | One flag, one meaning, honestly labelled — **goal met; every flag read and browser-launch decision enumerated and checked** |
| **8. Performance and cleanup** — **DONE, MERGED `8389009`** (24 commits from `cookie-arc8-perf-cleanup` @ `8cc212b`, branch deleted; arc-close review 2026-08-29) | H5, S2, S3(a), H1, H2 (a PIN, + X4), S6, H3, H6 (corrected and hedged), H7 — **nine CLOSED; S8 OPEN as a field gate; S3(b) ruled out** — plus every carry-over listed there: the four Arc 2 ride-alongs, follow-ups 3/4/5/6/7(d)/7(e)-residual, all eight Arc 4+7 residuals, and Arc 5's (a)-(h) — **with 11(a) and 11(c) resolved AGAINST the controller's rulings on evidence, and 11(h) ruled NO** | Measured waste removed (status polls stop scanning the filesystem/registry; two refreshes cannot overlap; a boot panic cannot hang the process); stale facts corrected — **goal met; one finding needs a running Chromium; every carry-over homed in ONE block in its paragraph** |
| **9. Docs and tests** — **DONE, MERGED `dd5dd18`** | X2, X3, X4 | Spec matches code; the untested acquisition path gets tests — **must now also document Arcs 2-6's mechanisms and Arc 5's two-jar model (see its paragraph)** |
| **Deferred — needs an owner decision** | G3, G4, V9, A2, T3 (within-platform remainder only — the cross-platform half closed in Arc 5) | See "Deferred" at the end |

\* **V12** is new, added by owner requirement 2026-08-25: a channel-independent liveness probe for installs with no monitored channels. Recorded in `reports/cookies-observations.md` §8.5.

† **G5 was analysed and OVERTURNED in execution — the gate stays.** See the struck-out Task 7 Step 2 below for the source-verified derivation. Do not re-attempt it in a later arc.

### Arc ordering constraints (from the round-2 remedy audit)

> **Arc 0 executes first** — see its section below. It is field-confirmed broken and actively costing sessions.
>
> The full audited merge order for Arc 1's tasks, the four remedies that were rejected or rewritten (S7, A2, H4, A1), and the "do NOT combine" pairs live in `reports/cookies-observations.md` §"Suggested sequencing". That file is **gitignored** — if it is absent, the same constraints are restated inline in Arc 1's revision-2 section and in the Rollback section below.

> **Superseded in part, 2026-08-27.** The re-derived sequence is **Arc 2 → Arc 3 → Arc 4+7 → Arc 6 → Arc 5 → Arc 8 → arm → Arc 9** — see the Arcs 2-9 section for why Arc 6 moved ahead of Arc 5 and why 4 and 7 merged. The constraints below still hold *within* that order except where struck.

- **Arc 1 before everything.** It is the owner's stated priority and it makes every later fix observable.
- **Arc 3 before Arc 4** — A1 and H4 both touch `FinishSetup`; A1 changes whether it is *reachable*, H4 changes what it *returns*. Reviewing both at once hides H4's `PersistPlatforms(false,false)` regression. *(Still holds. Note the regression itself was already closed by Arc 1's Task 2b — what remains of H4 is surfacing, not the dangerous half.)*
- ~~**Within Arc 5: T3+T2 storage shape first, then T1.** T1 written against `j.domains[name]` is code T2 deletes.~~ *(Executed in that order 2026-08-28 — `0c40e4d` replaced the parallel maps with `cookieEntry`, then `72f4373` partitioned into two jars; neither `j.cookies` nor `j.domains` exists any more.)*
- **Never combine:** S1+A1 · T1+T2/T3 · S9+A2 · A1+H4. *(All history except S9+A2 — A2 remains deferred; S9 is done.)*
- **S7 and S10 are one change**, not two — S7's domain-scoped delete needs S10's canonical 7-field join.

---

## Arc 0: Make the browser refresh real — COMPLETE, MERGED (main @ 5f0b69d)

> **STATUS 2026-08-27: all four tasks executed, reviewed clean, whole-branch-reviewed, field-gated on two Firefox-family browsers (Waterfox + Mozilla Firefox), and merged.** The task text below is execution history. Two things below were **superseded during execution** and are flagged inline: A0.1's design (re-ruled from a warn-and-withhold gate to a Debug-level sanity note), and A0.3/H6's live-gate discriminator (the screenshot cannot pass on a fresh profile; the shipped gate counts `moz_cookies` rows). Per-task rulings: `progress.md`.

**Added 2026-08-25 after field evidence from the owner's install.** This arc did not exist in earlier revisions because the defect it fixes was invisible: the broken path logs `"cookie refresh succeeded"`. See `reports/cookies-observations.md` §9 for the full evidence.

**Why it precedes Arc 1.** Arc 1 improves *detection*. Arc 0 fixes the thing that is *actively costing sessions right now* — the managed profile has not been written to in five days while the refresh reports success on every run, including two the owner triggered by hand. Detection that works is less valuable than acquisition that works.

**Findings:** F1 (the refresh is a 170 ms no-op), F2 (success is measured against the wrong artifact), F3 (partial success masks a per-platform failure and suppresses the notification), F5 (the elapsed time that would have caught all of this is computed and discarded).

### Arc 0 — revision 2 (adversarial review + empirical validation, 2026-08-25)

**Order changed: A0.3 lands FIRST, not A0.1.** A0.3 is the fix; A0.1 is the detector for it. Landing the detector first flags 100% of refreshes as failures — correct, but noisy — and worse, calibrating the detector before the fix exists means calibrating against a broken baseline. **Separate commits, A0.3 then A0.1.**

**Revised order: A0.3 → A0.4 → A0.1 → A0.5 → (Arc 1 Task 7 / G6, rebased onto A0.5).**

| # | Problem | Status |
|---|---|---|
| **B3** | **A0.4's mtime gate breaks the browserless Docker import** — `importedFromProfile` (`autocookies.go:628`) never launches a browser, so the profile mtime can *never* advance. Gating success on it makes **every container import report failure, permanently** — the deployment this whole arc exists for. It would also break `TestRefreshCookiesImportsProfileWhenNoBrowser`, the rollback tests, and four `autocookies_emptyresult_test.go` cases. Two further defects: Chromium's DB is `Default/Network/Cookies`, not `cookies.sqlite`; and Firefox's WAL means a killed browser leaves writes in `-wal` with the main file untouched. | **fixed below** |
| **B4** | **A0.1 is not implementable as written** — `refreshFirefox` discards the launch outcome entirely (`autocookies_firefox.go:164-173`), so there is no channel to the caller. Returning an error would skip the whole merge/verify tail, 500 the Settings button, and fire *"Export a fresh cookies.txt"* — **false advice**, since the cookies are fine and the launcher is broken. | **fixed below** |
| **H2** | A0.1's "apply the same floor to `refreshChromium`" is wrong — that path has no `startTime`, no launcher handoff, and a different failure mode. **Bullet dropped.** Replaced by checking `cdpNavigate`'s discarded error (`autocookies_chromium.go:247`), which is the honest per-launch signal there and retires half of finding A3. | **fixed below** |
| **H4** | A0.3 never said what `runWithTimeout` **returns** on drain-timeout. "Today's behaviour" is `return nil` — so a hung browser would report `elapsed=30s` and *pass* A0.1's floor, resurrecting F1 invisibly. Must return a distinct error and **degrade, not abort**. | **fixed below** |
| **M2** | A0.3 widens finding S4's reaped-PID window from ~200 ms to up to 30 s — `refreshFirefox:161` publishes `s.refreshCmd = cmd` and never restores the sentinel, so `killRefreshProcess` can `taskkill /F /T` a recycled PID for the whole drain. **~150× more likely.** Fix inside A0.3: restore the sentinel when `cmd.Wait()` returns, exactly as `refreshChromium` already does (`autocookies_chromium.go:224-235`). | **fixed below** |
| **M3** | `runWithTimeout` takes no `ctx`, so the drain is up to 30 s of un-cancellable wait inside a caller that *does* cancel. Thread `ctx` in and select on `ctx.Done()`. | **fixed below** |
| **H3** | **Linux Firefox has the same root cause, gets no fix, and gains A0.1's alarm.** With no `KILL_ON_JOB_CLOSE` the browser is not killed — it runs detached while `readFirefoxCookies` reads a profile it is still writing, and holds `parent.lock` for the next launch to yank. `PR_SET_PDEATHSIG` fires on *Moombox's* death, not the launcher's. Accepted gap (Windows is primary), but A0.1's Linux message must say so rather than "the process exited before it could load anything". **⚠ 2026-08-27 audit: the "same root cause" premise is UNVERIFIED and probably wrong.** Firefox's launcher process is a **Windows-only** feature (Mozilla ships it only on Windows builds, Firefox 67+; there is no Linux counterpart), so on native Linux `cmd.Wait()` plausibly returns only after the headless `--screenshot` run completes — leaving `rendered` TRUE and `Renewed` behaving normally. Every "not acted ~always on Linux" / "permanent steady state" claim downstream (the ledger's A0.4 ruling cost, `RefreshResult.Renewed`'s doc, `routes/cookies.go`, `app.js`) inherits this premise. The shipped "could not confirm" *wording* is safe either way; the *frequency* claims are not. One run of the live gate's drained-launch subtest (which is NOT Windows-gated) on any Linux box settles it — see Follow-ups 7(f). | **documented; Linux premise unverified** |
| **H5 / M1** | A0.5 written against a bool re-creates the over-claiming it fixes: `RefreshCookies` returns `(false, nil)` from **five decline paths** (`services.go:641-655`), and once Arc 1 Task 1 lands every non-200 becomes `verifyUnknown`. A0.5 must be three-state from day one. Also **6 production callers, not 4** — the plan missed `autocookies.go:1100` and `:1127`. Use an additive `RefreshCookiesDetailed`. And **`services.go:634` has the identical F3 defect**: a Twitch success tells a *YouTube* job to retry into a guaranteed-identical failure. | **see A0.5** |
| **H6** | Nothing in Arc 0 can catch a regression of Arc 0. `shouldKeepWaiting` is a pure predicate that was *already* semantically correct — the defect was in the syscall wiring and placement. Add an env-gated live test (`MOOMBOX_LIVE_BROWSER_REFRESH=1`) asserting the screenshot exists and the cookie-DB fingerprint changed, following Arc 1 Task 0's precedent. | **see A0.3** |
| B1, B2 | Reported as blockers, but **already fixed** — the reviewer read Arc 0 before the validation pass. The code checks `r == 0` (not `err != nil`, which is the `syscall.Errno(0)` trap), and `startedAt` is captured before `cmd.Start()`. Worth keeping the explanatory comment about the errno trap, and adding a `j == nil \|\| j.handle == 0` guard. | already correct |

**Citation fix:** `runWithTimeout` spans `autocookies_firefox.go:516-581`, not `:516-566`. The clipped range omits the timeout branch.

**Do NOT combine:** **A0.5 + Arc 1 Task 7 (G6)** — both rewrite the same 50 lines of `monitor_callbacks.go:224-274` and both change when notifications fire. Land A0.5 first, rebase G6.

---

### Task A0.1: A refresh that finishes impossibly fast is a failure — DONE (32bcb01), **design superseded in execution**

> **AS EXECUTED — the steps below describe a design a ruling overrode; do not re-implement them.** After A0.4 landed, the screenshot became the DIRECT signal for "did the browser act", making an elapsed-time floor a second heuristic for the same question and a second way to manufacture a false failure. Ruling (reviewer M4, accepted): A0.1 shipped as a **Debug-level sanity note** for an implausibly fast launch that **does NOT set `browserActed = false`** and withholds nothing. The floor's exact value therefore stopped mattering. The bullets below (Warn-and-withhold, `browserActed = false` on a sub-floor launch) are the superseded design, kept for the record.

Cheapest possible fix, immediate value, and it turns F1 from invisible into loud — permanently, including for browsers nobody has tested yet.

**Files:** modify `internal/cookies/autocookies_firefox.go:163-168`; test `internal/cookies/autocookies_refresh_floor_test.go` (create).

- [ ] Add `minPlausibleBrowserRefresh = 1 * time.Second`.

**The threshold was measured, and 3 s (the value in the first draft) would have been wrong.** With A0.3's drain-wait in place, a *working* refresh against the real profile completes in **3.08 s** — so a 3 s floor sits 80 ms away from false-flagging every successful refresh on a fast machine. Observed values: no-op **160-211 ms**; working **3.08 s**. **1 s** sits ~5x above the failure mode and ~3x below the success case, with margin on both sides. Record both numbers in the comment so the next person does not "tighten" it back.

**Note the ordering consequence:** A0.1 alone, shipped before A0.3, will flag *every* refresh as a failure — because every refresh genuinely is one. That is correct and is the point: it makes the outage visible. But land A0.3 in the same batch, or expect a burst of warnings in between.
- [ ] **Warn and withhold success — do NOT return an error (B4).** `refreshFirefox` currently discards the launch outcome entirely (`autocookies_firefox.go:164-173`), so there is no channel to the caller and this task depends on A0.4's `browserActed` plumbing. Returning an error instead would skip the whole merge/verify tail, 500 the Settings button (`routes/cookies.go:92-94`), and fire *"Export a fresh Netscape cookies.txt"* (`monitor_callbacks.go:253`) — **false advice**, since the credentials are fine and the launcher is broken. That is precisely the over-claim the codebase argues against at `autocookies.go:829-832`.
- [ ] So: on a sub-floor launch, `Warn` naming the platform and elapsed, set `browserActed = false` for that platform, and let A0.4 withhold `lastRefresh` / `"cookie refresh succeeded"` / `SaveMeta`. Nothing else changes.
- [ ] Extract the decision as a pure predicate — `func refreshLooksLikeNoOp(elapsed time.Duration, err error) bool` — so it is table-testable without launching a browser. This matches the file's existing habit of pulling decisions out of unstubbable I/O.
- [ ] Table test: `(200ms, nil)` → true; `(8s, nil)` → false; `(200ms, someErr)` → false (a real error is already handled); `(1s, nil)` → false (exactly at the floor).
- [ ] **On Linux, say something true.** Firefox there has the same launcher handoff but no `KILL_ON_JOB_CLOSE`, so the browser survives detached rather than being killed — the floor still trips, but "the process exited before it could load anything" is wrong. Use a platform-appropriate message, and note the known gap: the detached browser keeps writing the profile while `readFirefoxCookies` reads it, and holds `parent.lock` into the next launch.
- [ ] **Do NOT apply this floor to `refreshChromium` (H2).** That path has no `startTime`, no launcher handoff, and a different failure mode — a 3 s floor there would be a new heuristic measuring a different mechanism, calibrated on no data, on a path §9 explicitly exonerates. The honest per-launch signal on Chromium is `cdpNavigate`'s **discarded return value** at `autocookies_chromium.go:247`; checking it retires half of finding A3 for free. Track that separately, not here. **2026-08-27 audit: still untracked and still discarded** (`autocookies_chromium.go:256` as of `cc51b81`) — and A0.4's Chromium branch meanwhile set `browserActed = err == nil` with a comment claiming that is "proof of the same order as the Firefox screenshot", which it is not: a nil error proves the browser answered CDP, not that any platform page loaded, so a Chromium pass whose navigations all failed still reports success/renewed. Recorded in Follow-ups 7(e); Arc 4's A3 entry does not name it, so without 7(e) it falls between arcs. **CLOSED at `ee1ce5f`/`bd6bc84` (the Arc 0/1 follow-ups round): `navigateAllPlatforms` folds per-platform navigation errors into `browserActed`. One residual, recorded not fixed: `cdpNavigateAndWait` returns `nil` on budget exhaustion (`autocookies_chromium.go:514-516` at `37eb62e`), so connect-then-stall still counts as navigated — missed-alarm direction; homed in Arc 8.**
- [ ] Commit: `fix(cookies): treat an impossibly fast browser refresh as the failure it is`

### Task A0.2: DONE — diagnosis complete, 2026-08-25

Resolved by controlled isolation before this plan was executed. A standalone Go program replicated `runWithTimeout` and ran Moombox's exact argv twice against two copies of the real profile, varying only the Job Object: **plain exec → screenshot written, profile mtime advanced. With the job → nothing.** Full evidence in `reports/cookies-observations.md` §9 F1.

**Cause: Waterfox uses the Firefox launcher-process model.** The launched `waterfox.exe` hands off and exits at ~166-207 ms; `cmd.Wait()` returns on that; `runWithTimeout`'s `defer job.close()` then fires `KILL_ON_JOB_CLOSE` and kills the real browser mid-load. Applies to **all** of `firefox`, `waterfox`, `librewolf`, `zen`. Chromium is unaffected (it does not use `runWithTimeout`).

Exonerated, do not re-investigate: the `--screenshot` flag, `--headless`, page weight, the argv, and the profile itself.

### Task A0.3: Wait for the job to drain, not for the launcher to exit — DONE (f6e5c59)

> **AS EXECUTED, one correction to the H6 bullet below:** the env-gated live test as specified (assert the screenshot exists and the cookie-DB fingerprint changed) **cannot pass against a freshly-created profile** — `--screenshot` silently produces no file there (reproduced on Waterfox AND Firefox, with and without the Job Object; cause unknown), and the killed-at-launcher-exit control still *creates* an empty `cookies.sqlite`, so the fingerprint stamp passes on exactly the launch the test must reject. The shipped gate (`237fc7b`) instead counts `SELECT host FROM moz_cookies` rows via the WAL-aware snapshot, with a killed-launch control subtest requiring zero rows. Certified on Waterfox and Mozilla Firefox (`MOOMBOX_LIVE_BROWSER_PATH` steers detection); **LibreWolf, Zen and all non-Windows platforms remain unverified.** The production `browserRendered` signal still keys off the screenshot — it discriminated correctly against real setup-created profiles — but the fresh-profile screenshot failure is an open item (see Follow-ups).
>
> **AS SHIPPED, two deltas from the bullets below (2026-08-27 audit):** (1) the inline drain sketch landed as an extracted `drainJob` (ctx-cancel select, a distinct `errBrowserDrainTimeout`, polls+elapsed logging, and a lap-zero "nothing was waited on" line that does not claim a finish) — do not re-derive from the sketch. (2) The M2 sentinel-restore bullet ("restore the moment `Wait()` returns, exactly as `refreshChromium`") was **refined by the whole-branch M3 ruling**: the restore ships as `onLauncherReaped`, fired only on exits that *observe* a reap, and deliberately NOT on the 5-second no-reap kill fallback — clearing the slot there would trade a recycled-PID kill for an orphaned browser holding the profile lock, the worse failure. Re-implementing the bullet as written would rebuild the rejected design. (3) The gate's "expected healthy numbers" band (2.7–3.6 s / 51–67 polls) is indicative, not diagnostic: a 2026-08-27 re-run on the certified machine drained **clean** in 13.96 s / 276 polls (cold first-run profile) with all six YouTube rows written — a slow clean drain is normal variance, not the leaves-a-process-alive shape.

**Files:** `internal/cookies/job_windows.go` (add the query), `internal/cookies/autocookies_firefox.go:516-566` (`runWithTimeout`), plus the `job_linux.go` / `job_other.go` stubs.

**This fix was built and validated in a standalone harness against three copies of the real profile before being written here.** Pass 2 (today's code) → no screenshot, profile untouched. Pass 3 (the code below) → screenshot written, profile mtime advanced, drain completed in **3.082 s over 59 polls**. The struct and call below are the ones that were actually executed, not a sketch.

- [ ] Add to `internal/cookies/job_windows.go`:

```go
var procQueryInformationJobOb = kernel32.NewProc("QueryInformationJobObject")

const jobObjectBasicAccountingInformation = 1 // JOBOBJECTINFOCLASS

// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION — 4x LARGE_INTEGER then 4x DWORD.
// No padding on amd64: 32 bytes of int64 followed by 16 bytes of uint32.
type jobobjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// activeProcesses reports how many processes are still alive in the job.
//
// This is what makes the Firefox-family refresh work at all. Firefox uses a
// launcher-process model: the exe we start hands off to the real browser and
// exits in ~170ms, so cmd.Wait() returning tells us nothing about whether the
// page loaded. Closing the job at that moment kills the browser mid-load —
// measured, and the reason every refresh silently did nothing.
func (j *processJob) activeProcesses() (int, error) {
	var info jobobjectBasicAccountingInformation
	var retLen uint32
	r, _, callErr := procQueryInformationJobOb.Call(
		uintptr(j.handle),
		uintptr(jobObjectBasicAccountingInformation),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
		uintptr(unsafe.Pointer(&retLen)),
	)
	if r == 0 {
		return 0, fmt.Errorf("QueryInformationJobObject: %w", callErr)
	}
	return int(info.ActiveProcesses), nil
}
```

- [ ] Add the stub to `job_linux.go` and `job_other.go`: `func (j *processJob) activeProcesses() (int, error) { return 0, nil }` — returning 0 makes the drain loop exit immediately, so those platforms keep today's behaviour exactly.
- [ ] In `runWithTimeout`, in the `case err := <-done:` branch, **do not return immediately** — drain the job first. Validated shape (59 polls / 3.082 s in the harness):

```go
	case err := <-done:
		// cmd.Wait() only tells us the LAUNCHER exited. Firefox hands off to
		// the real browser, so returning here closes the job and kills it
		// mid-load. Wait for the job itself to empty.
		if job != nil {
			for time.Since(startedAt) < timeout {
				n, qErr := job.activeProcesses()
				if qErr != nil {
					logger.Warn("could not query job process count; not waiting for drain", "err", qErr)
					break
				}
				if n == 0 {
					break
				}
				time.Sleep(killProcessTreePollDelay)
			}
		}
		return err
```

- [ ] Capture `startedAt := time.Now()` immediately before `cmd.Start()` so the drain shares the one `timeout` budget rather than starting a second one.
- [ ] A query error **breaks out and returns** rather than looping — losing the drain degrades to today's behaviour, which is bad but known; spinning on a failing syscall is worse.
- [ ] `killProcessTreePollDelay` (50 ms, `autocookies.go:41`) is the right cadence and already exists for this shape of wait. Do not add a new constant.
- [ ] The existing `case <-time.After(timeout)` branch is untouched — a genuinely hung browser still gets killed on the same budget.
- [ ] **Return a distinct error on drain-timeout (H4).** "Keep today's behaviour" would return `nil`, so a hung browser reports `elapsed=30s` and *passes* A0.1's floor — F1 resurrected, slower and now invisible to the detector built for it. Return e.g. `errBrowserDrainTimeout`, and have `refreshFirefox` **degrade rather than abort**: Warn, mark `browserActed = false` for that platform, continue to `readFirefoxCookies`. Same shape as the existing `ErrNoCookiesInProfile` handling (`autocookies.go:673-678`).
- [ ] **Restore the refresh sentinel when `cmd.Wait()` returns (M2).** `refreshFirefox:161` publishes `s.refreshCmd = cmd` and never restores it, so today `killRefreshProcess` can `taskkill /F /T` a reaped PID for ~200 ms. The drain stretches that window to **up to 30 s**, making finding S4 roughly 150× more likely. Restore `s.refreshCmd = &exec.Cmd{}` the moment `Wait()` returns, exactly as `refreshChromium` already does with its documented rationale (`autocookies_chromium.go:224-235`). Three lines, and A0.3 is what makes it matter.
- [ ] **Thread `ctx` into `runWithTimeout` and select on `ctx.Done()` in the drain loop (M3).** Today the un-cancellable window is ~200 ms; after this change it is up to 30 s per launch, inside a caller whose `refreshOverallBudget` *does* cancel. Cheap now, painful to retrofit.
- [ ] **Add the regression test Arc 0 otherwise lacks (H6).** `shouldKeepWaiting` is a pure predicate that was already semantically correct — it cannot fail for the bug it exists to fix, which lived in the syscall wiring and the placement. Add an env-gated live test following Arc 1 Task 0's precedent (`MOOMBOX_LIVE_BROWSER_REFRESH=1` + `t.Skip`): run one real `refreshFirefox` against a throwaway profile and assert the screenshot exists and the `cookieDBFingerprint` changed. ~40 lines, and it is the only thing that would have caught F1.
- [ ] Keep the `r == 0` check, not `err != nil` — `syscall.LazyProc.Call` always returns a non-nil `syscall.Errno`, and `Errno(0)` formats as *"The operation completed successfully."* The existing calls in this file get this right (`job_windows.go:71, 84`); say so in a comment so nobody "fixes" it. Guard `j == nil || j.handle == 0` too: `newProcessJob()` can fail and `runWithTimeout:523-525` continues with a nil job.
- [ ] On budget expiry, keep today's behaviour: log the timeout and let `close()` kill the tree.
- [ ] **Non-Windows must be a no-op**: `job_linux.go` relies on `PR_SET_PDEATHSIG` and has no job to drain, so `activeProcesses()` returning 0 makes the loop exit immediately.
- [ ] Test the predicate, not the syscall: extract `func shouldKeepWaiting(active int, elapsed, budget time.Duration) bool` and table-test it — `(2, 1s, 30s)` → true; `(0, 1s, 30s)` → false; `(2, 31s, 30s)` → false.

**Acceptance is objective:** after a refresh, `browser-profile/cookies.sqlite`'s mtime must have advanced. That is the only direct evidence the profile was actually written. Expect elapsed to go from ~200 ms to several seconds — which A0.1's floor will then stop flagging.

### Task A0.4: Success must reflect what the refresh did (F2) — REVISED per B3 — DONE (2d85a8c..b80898d)

> **AS EXECUTED:** shipped as specified, plus two accepted rulings recorded in `progress.md`: on native Linux "not acted" means "could not confirm" (no Job Object, so the detached browser may finish after the check) and is reported as such rather than suppressed; and the whole-branch review's I1 later carried `Renewed` into `RefreshResult` (7b7593c) so the Web/TUI "Refresh now" surfaces stopped toasting success for a refresh that renewed nothing. **Caveat (2026-08-27 audit):** the ruling's *frequency* claim — Linux reports not-acted "~always" — inherits H3's unverified launcher-handoff-on-Linux premise (see the flagged H3 row); if native Linux has no handoff, `rendered` is true there and the "permanent steady state" never occurs. The ruling's *wording* ("could not confirm", never "proven no-op") holds regardless.

Today `RefreshCookies` reads a stale profile, merges it, then verifies **`cookies.txt`** — which the independent 30-minute `RefreshService` keeps alive regardless. Verification passes and a completely failed refresh logs `"cookie refresh succeeded"`.

**The mtime gate from revision 1 is withdrawn.** It would have broken the browserless Docker import permanently (B3): that path never launches a browser, so the profile mtime can never advance, and every container import would report failure. Use the artifact the code *already produces* instead.

- [ ] **Primary signal: the screenshot.** `refreshFirefox:135` already passes `--screenshot <profile>/refresh-screenshot.png` to every launch, and §9's isolation experiment used exactly its presence/absence as the discriminator. `os.Remove` it *before* each launch and `os.Stat` it after — direct, per-launch, unambiguous proof the browser rendered. No WAL reasoning, no per-family DB paths, no import-path exposure.
- [ ] **Watch the existing defer.** `defer os.Remove(tempScreenshot)` (`:136`) is function-scoped, so the YouTube shot survives into the Twitch launch. Clear it per iteration or the second platform always reads as "acted".
- [ ] **Secondary signal: reuse `cookieDBFingerprint`, don't hand-roll mtime.** `autocookies_profile.go:186-224` already has `fileStamp{exists,size,mod}`, `stampFile` and `fingerprintsDiffer`, and its `equal` helper already documents the `time.Time` monotonic-clock trap a naive `==` would spring.
- [ ] **Scope every part of this task to `importedFromProfile == false`.** `RefreshCookies` serves three paths through one tail — browser refresh, browserless import, and the empty-profile fallback (`autocookies.go:673-678`). Only the first has a browser to have acted.
- [ ] Return a per-launch `browserActed bool` and aggregate as "all attempted launches acted". Withhold `s.lastRefresh` (`:887`), `"cookie refresh succeeded"` (`:921`) and `SaveMeta` when false.
- [ ] Make the success log distinguish *"the browser refreshed the profile"* from *"the existing cookies still verify"*. Only the first belongs in `"cookie refresh succeeded"`.

### Task A0.5: Per-platform reporting in the recovery path (F3) — DONE (c9140aa..2f3a93c)

> **AS EXECUTED, wider than the bullets below:** shipped as a three-state `RefreshResult` (`Ran`/`Renewed`/per-platform `RefreshOK|RefreshFailed|RefreshUnknown` + `YouTubeStored`/`TwitchStored` via `HasCredentials(platform)`), NOT a bool — the H5/M1 hardening row governed. Scope included fix site B: the worker path (`OnCookieRefreshNeeded` now carries `job.Platform`), and the worker's failure wording splits "holds no cookies — nothing was rejected" from "the stored cookies are dead". `RefreshUnknown` is the enum zero value by ruling.

Observed 2026-08-20 03:40:01 — YouTube conclusively failed, Twitch verified, `ok = true` carried the call, and `"auto-cookie recovery succeeded"` was logged for YouTube while no notification fired.

- [ ] `RefreshCookies` must report per-platform, or `OnRecoveryNeeded`'s callback must compare against the platform it was invoked for.
- [ ] `notifyAuthFailure` fires when *the triggering platform* did not recover, irrespective of its sibling.
- [ ] Test: recovery invoked for `youtube`, YouTube verify fails, Twitch verify succeeds → a failure notification for YouTube. This is the exact log line above, turned into a regression test.
- [ ] Note the interaction with Arc 1's G6: once both land, a real YouTube failure notifies whether or not `auto_enabled` is set and whether or not Twitch is healthy.

### Arc 0 acceptance

- [x] A refresh that does nothing is logged as a failure, not a success — met by A0.4's `browserActed` withholding (A0.1 itself was re-ruled to a Debug note; see its AS-EXECUTED banner). **Qualified (2026-08-27 audit): holds for the Firefox family and for a Chromium browser that fails outright. A Chromium pass whose per-platform navigations fail while CDP stays answerable is still credited** — `cdpNavigate`'s error is discarded (H2 bullet, Follow-ups 7(e)) — so F2 is closed on the Firefox path and half-open on the Chromium path. **Superseded: the Chromium half CLOSED at `ee1ce5f`/`bd6bc84` (navigation errors now fold into `browserActed`); the surviving residual is the `cdpNavigateAndWait` nil-on-budget-exhaustion case, homed in Arc 8.**
- [x] After a successful refresh the profile is genuinely being written — proven by the live drain gate on Waterfox AND Firefox (2.2-3.6 s drains, 6 youtube rows in `moz_cookies`; the killed-launch control writes 0). The mtime phrasing was the superseded discriminator.
- [ ] **STILL OPEN (passive field observation):** leave it running and confirm the profile stays signed in past the point it previously died (the owner's five-day divergence is the yardstick).
- [x] Force a YouTube-only failure with Twitch healthy and confirm a notification arrives (A0.5) — pinned by regression tests both directions; the notification path was then proven live by Arc 1's A1/A2 acceptance runs (2026-08-27, owner-confirmed delivery).

**RotateCookies — VERIFIED 2026-08-25, and the proposal is REJECTED.** Full report: `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/research-rotatecookies.md`.

The original note read: *"Research suggests Google's rotation is minted by a dedicated endpoint — `POST accounts.google.com/RotateCookies`, declaring a 600-second cadence — and that neither a page load nor the `youtubei/v1/guide` POST triggers it. If that holds, even a working page-load refresh was never the right mechanism, and a direct Go POST would keep the session alive with no browser at all, in Docker included."* Verdicts, claim by claim:

| Claim | Verdict |
|---|---|
| The endpoint exists | **CONFIRMED.** `POST accounts.google.com/RotateCookies`, `Content-Type: application/json`, body `[000,"-0000000000000000000"]`. gpt4free and HanaokaYuzu/Gemini-API ship byte-identical payloads. |
| ~600-second cadence | **PARTLY TRUE, single-source.** One issue thread reports `)]}' [["identity.hfcr",600],…]` and infers the interval. Both real implementations actually guard at **60s**, not 600. |
| Page loads don't rotate | **CONTRADICTED.** The proposal's own pitch concedes it turns *probabilistic* rotation into *guaranteed* rotation — ordinary traffic already rotates. yt-dlp's wiki: *"YouTube rotates account cookies frequently on open YouTube browser tabs."* |
| The `guide` POST doesn't rotate | **CONFIRMED empirically, small sample.** Owner's install, 2026-08-25 18:06–20:58, debug on: **10 guide checks, 10 × `no Set-Cookie headers`, 0 × `updating cookies`.** `RefreshService`'s rotation-merge has never fired in this window. It is a liveness check that happens to *have* a merge path, not a rotation mechanism. |
| A Go POST keeps YouTube alive | **CONTRADICTED.** The load-bearing premise fails below. |

**Why it fails:** the endpoint rotates only `__Secure-1PSIDTS` / `__Secure-3PSIDTS` — cookies YouTube Innertube auth does not use. yt-dlp has **zero** matches for `PSIDTS`, `SIDCC`, or `RotateCookies` across 1,128 files; its auth predicate is `LOGIN_INFO && (SAPISID || 1PSAPISID || 3PSAPISID)`, and `SAPISIDHASH` self-refreshes from a static cookie plus a timestamp. Moombox's `HasYouTubeAuthCookies` independently converged on the same set. The PSIDTS requirement is real, but it belongs to *first-party* Gemini/NotebookLM endpoints and does not generalise to YouTube.

**The premise is probably inverted.** Per yt-dlp, exported YouTube cookies die *because* a browser rotated them elsewhere — rotation is single-writer and YouTube clears `LOGIN_INFO` on the loser. The remedy is **isolation** (a dormant profile, sole rotator), not *more* rotation. Adding a rotation participant would make this worse, not better.

**DBSC is a different endpoint** — `/RotateBoundCookies`, TPM-signed, unforgeable by a Go client. `RotateCookies` is unsigned and works today, but it is precisely the replay pattern DBSC exists to kill, so it is a poor thing to build on.

**Residual uncertainty and the experiment that closes it.** The empirical result is one account over ~3 hours, and "PSIDTS is inert for YouTube" rests on yt-dlp's auth predicate — strong evidence about what yt-dlp *sends*, not proof the cookie is ignored. To settle it: copy a good `cookies.txt`, delete **only** the two `*PSIDTS` rows from the copy, run the existing `checkYouTubeAuth` against it. Still `logged_in=1` → inert, and the proposal is closed for good. Removals only, from a copy, against an endpoint Moombox already calls half-hourly.

**Consequence for Arc 0: none.** Arc 0 fixes honest reporting and the acquisition path, neither of which this touches.

---

## Arc 1 — revision 2 (adversarial review, 2026-08-25) — EXECUTED; tables below are history

> **STATUS 2026-08-27: Tasks 0-7 are complete on `cookie-arc1-liveness` @ `ffea847` (see EXECUTION STATUS at the top).** Two tasks the plan never had were added in execution — **Task 1b** (the Twitch mirror of Task 1) and **Task 2b** (the autocookies subsystem's strict-presence sites) — summarised inline below. The rows in this section still bind where they name standing constraints (N6 in particular), but their line-number citations are pre-execution.

**Arc 1 as first written would not have achieved its goal.** A review against the source found four issues that each independently defeat it. They are fixed inline below; this list exists so nothing changed silently.

| # | Problem | Where it is fixed |
|---|---|---|
| **B1** | Task 6 Step 5 was **impossible** — it said to schedule the fallback probe inside `RefreshService`'s goroutine, but that needs `internal/cookies` → `internal/youtube`, and `internal/youtube/auth.go:8` already imports `internal/cookies`. Import cycle. | Task 6, via callback inversion |
| **B2** | Task 4 named the wrong callers. There is **exactly one** call site, `cmd/moombox/monitor_callbacks.go:400`; `internal/monitor` has only doc comments and its `MembershipFetchFunc` keeps its 2-value shape. The old `git add internal/youtube/ internal/monitor/` would have committed a **non-compiling tree**. | Task 4 |
| **H1** | **The probe never runs in the state it exists for.** `cmd/moombox/monitor_callbacks.go:415` gates membership discovery on `HasAuthCookies()` — the *strict* SAPISID+LOGIN_INFO predicate. With LOGIN_INFO cleared, `FetchMembership` is never called and Task 4's verdict is dead code. **A third gate the review missed:** `cmd/moombox/services.go:467` does the same for the backfill sweep. | Task 4 |
| **H2** | **The alarm stays a presence test**, violating this plan's own liveness constraint. `checkAndRefreshYouTube` returns `(false, nil)` from four early returns *before touching the network* (`refresh.go:641-643, 645-648, 651-654`, plus the mirror at `:553-566`). Task 1 fixed one such non-observation (non-200) and left four. | Task 2 |

Also folded in: **H3** (Task 1 silently reroutes auto-cookie rollback), **H4** (Task 5 is blind to a login redirect), **M1** (Task 6's "30-minute window" does not exist in `refresh.go`), **M3/M4/N2/N5** (naming, nil-`Auth` panic, helper reuse, a test that tests nothing), **N6** (G5's real blast radius), **N7** (a third copy of the cookie-name list).

**One correction reversed — and the reversal's own mechanism was then ALSO corrected.** The hardening pass changed Task 3 to package-level `[]byte` vars, claiming `bytes.Index(b, []byte(const))` allocates per call. Measurement said otherwise (0 allocs/op either way), so Task 3 reverted to the inline form — but the explanation given here originally ("every marker is ≤ 32 bytes, the compiler's stack `tmpBuf` ceiling") was **also wrong**, in the opposite direction. The Task 4 review measured the full const/var × length matrix: **the 32-byte `tmpStringBufSize` ceiling is real but applies only to NON-constant sources; a compile-time constant converted in a non-escaping context gets an exact-size stack array with no ceiling at all** (const at 31/32/33/66/310 bytes → 0 allocs; a package var → 0 at ≤32, 1 at ≥33; anything escaping → 1). This claim was wrong twice in opposite directions on this branch because each experiment varied one factor and generalised to a conjunction it never visited — **do not restate it without the full matrix**. **[UPDATED at `4021a74`]** The audit/B-fix round changed which case applies: the marker KEYS now reach `bytes.Index` through a `key string` *parameter* (`sessionAuthMarkerInBytes`), so they are the ≤32-byte `tmpBuf` case at 11-12 bytes, not the exact-size-constant case; and the surviving pin is **`TestLivenessVerdictDoesNotAllocate`** (`TestSessionAuthFromBytesDoesNotAllocate` was merged into it when `sessionAuthFromBytes` was deleted). The matrix and `runtime/string.go` citations live in that test's comment in `internal/youtube/session_auth_test.go`.

### Cross-cutting corrections — read before Task 1

**Reuse the existing test helpers; do not create new ones (N2).**

- `internal/cookies` already has **`nopLogger`** (`refresh_test.go:11-16`) implementing the exact four-method interface. Use it. **Delete the `testLogger` block from Task 1** — the plan's "if it doesn't already exist" guard passes and you would create a third redundant logger.
- `jarWith` exists only as a *local closure* inside three funcs (`refresh_identity_test.go:15,79,101`), so `jarWithAuth` does not collide. Keep the plan's temp-file version rather than those closures' direct `j.cookies[k] = v`: `checkAndRefreshYouTube` calls `jar.Reload()`, which needs `filePath` set.
- `internal/youtube` has `noopLogger` (`service_test.go:10`). Use it.
- Verified no collisions for: `sessionAuthFromBytes`, `accountProbeURL`, `ProbeAccountLiveness`, `HasAnyYouTubeAuthCookie`, `HasAnyTwitchAuthCookie`, `HasAnyAuthCookie`, `ObserveLiveness`, `youtubeAuthCookieNames`, and the `sessionAuth*` marker consts.
- Both new tests mutate package-level vars, and nothing in `internal/cookies` uses `t.Parallel()` today. **Keep it that way**, and prefer `t.Cleanup` over manual save/restore so a panic cannot leak the override.

**Task 1 quietly changes the auto-cookie write path (H3).** `checkYouTubeAuth` is wired as `autoCookieSvc.VerifyYouTubeAuth` (`services.go:515`). In `autocookies_profile.go:560-568` an `err != nil` maps to `verifyUnknown`, and `platformsToRestore` (`:600-601`) restores on `before.hasCookies && after == verifyUnknown`. So after Task 1, a YouTube 429 during post-import verification turns a **commit into a rollback**, and changes the user-facing message from *"manual re-login required"* to *"the check did not complete (network?)"*. That is the correct behaviour and matches Arc 4's intent — but it is a cross-subsystem change with no test, because existing tests inject stub verify funcs. **State it in the Task 1 commit message and extend `autocookies_profile_rollback_test.go:173`.**

**Task 2's UI blast radius is real (M2).** `AuthStatus.HasYouTubeCookies` has three consumers and all change meaning for a half-cleared jar: `tui_wiring.go:571` moves it `CookieStatusNone` → `CookieStatusCookiesOnly`, which per `status_bar.go:414-419` (re-verified 2026-08-27) is the move from a **yellow "YT" that is dropped when the bar is unhealthy** (None) to a **red "YT" always shown** (CookiesOnly); the dashboard flips from the yellow `indicator-warn` *"YouTube: No cookies"* to the red `indicator-error` *"YouTube: Not verified"* (`app.js:885-895` — the earlier `:870-876` citation was the warnings block, not the YT indicator; settled in the Task 2 review). No Go test asserts on this field. Both UIs were confirmed after Task 2 — this row is settled history.

**Task 7 Step 2's G5 removal arms a population the plan did not name (N6).** `Cookies.Platforms` is **not** auto-cookie-only — `services.go:184-202` auto-detects and persists it from a plain `cookies.txt`. So seeding `SetExpectedPlatforms` for manual users sets `prev*Auth`/`*EverConcluded` true, which arms the **witnessed-transition** branch (`refresh.go:459-460`) *regardless of `cookiesPresent`*. Consequences: deleting `cookies.txt` while `platforms=["youtube"]` remains in config now alerts, and a stale `"twitch"` entry fires *"twitch auth lost"* on the first check. Both are defensible, neither is currently tested — **add an acceptance check, and expect it in the log-only phase.**

**Task 2 adds a third copy of the cookie-name list (N7).** `youtubeAuthCookieNames` overlaps `essentialYouTubeCookies` (`jar.go:35-43`) and `isGoogleOnlyAuthName` (`refresh.go:952-958`). Add a test pinning it as a subset of `essentialYouTubeCookies` so the three cannot drift. *(Shipped as `TestAuthCookieNameListsDoNotDrift`; at `37eb62e` the lists sit at `jar.go:81-89` / `:462-467` / `:593` and `refresh.go:2206`, and Arc 5's `37eb62e` added the Twitch half — `twitchAuthCookieNames ⊆ essentialTwitchCookies` — which the original pin lacked.)*

---

## Arc 1: Health checking that tells the truth

**Why this order inside the arc:** V2 and V3 are the two gates that make the health check unable to report anything. V11 is a hard dependency on V2 — `FetchMembershipVideos` returns early on the same presence gate, so the liveness probe would be suppressed by exactly the failure it exists to detect. G6 comes last because unblocking the notification before V3 turns it into a false-alarm generator.

**File structure for this arc:**

- `internal/cookies/jar.go` — add the "any auth cookie" predicate (V2)
- `internal/cookies/refresh.go` — non-200 becomes inconclusive (V3); accept externally-observed liveness verdicts (V11c)
- `internal/youtube/watch_page.go` — extract shared login markers, add the `[]byte` detector (V11a)
- `internal/youtube/account_probe.go` — **new**, the channel-independent fallback probe (V12)
- `internal/youtube/channel_membership.go` — return the login verdict instead of swallowing it (V11b)
- `cmd/moombox/monitor_callbacks.go` — route the verdict; ungate the notification (V11c, G6)
- ~~`cmd/moombox/main.go` — one-line gate removal (G5)~~ — **OVERTURNED in execution; the gate stays at `main.go:276-278` (no-touch zone, pinned in-comment, mutation-checked). See Task 7 Step 2.**

---

### Preconditions — do this before Task 0

- [ ] `git status --porcelain` is empty. If not, stop and ask.
- [ ] `go build ./... && go vet ./... && go test ./...` all pass. **Record the baseline** — if a test is already failing before you start, note which, or you will later blame your own change for it.
- [ ] You are on a branch, not `main`.

**Stop-and-ask triggers.** Halt and report rather than improvising if: a test helper the plan tells you to create already exists with a different signature; an existing test fails in a way the plan did not predict; Task 0 fails; or the Task 7 acceptance check does not fire. Every one of these means an assumption in the plan is wrong, and guessing past it produces a worse result than stopping.

---

### Task 0: Verify the login markers actually exist on these pages (BLOCKING) — DONE (f16546a); live run re-passed at execution

> **AS EXECUTED:** landed as written, then twice improved by later tasks: Task 3 re-pointed the pin to assert `livenessVerdict` as primary (production calls the strict variant), and Task 5's review added an **authenticated arm** behind `MOOMBOX_LIVE_YT_COOKIES=<path>` asserting `livenessVerdict == LoggedIn` — closing the risk that a formatting drift reads an authenticated page as dead while the anonymous-only live test stays green.

**The arc rests on an empirical premise: that `/channel/<id>/membership` and `/feed/subscriptions` stamp a login marker `sessionAuthFromBytes` can read.** If they do not, every verdict is `Unknown` and Arc 1 fails *safe* but does *nothing* — the worst outcome, because it looks shipped.

**PREMISE VERIFIED — both directions, 2026-08-25.**

| Page | Session | HTTP | `"LOGGED_IN"` | Verdict | Marker offset |
|---|---|---|---|---|---|
| `/feed/subscriptions` | anonymous | 200, 770 KB | **`false`** | `LoggedOut` | 56,905 |
| `/channel/UCBR8-60-B28hp2BmDPdntcQ/membership` | anonymous | 200, 1.97 MB | **`false`** | `LoggedOut` | 57,027 |
| `/feed/subscriptions` | **authenticated** | 200 | **`true`** | `LoggedIn` | — |

Anonymous fetches were run with curl from a Windows desktop; the authenticated row was confirmed by the owner in a signed-in browser (view-source, no credential handling). The detector therefore **discriminates in both directions on real pages** — it produces the `LoggedOut` the arc acts on, and it does *not* produce it for a healthy session. That was the false-positive risk, and it is closed.

**Tasks 3-6 are cleared to proceed.** (Incidentally the marker sits ~57 KB in, so a bounded-prefix scan would have worked; the plan still does a full scan, because that offset is an observation, not a contract.)

**Task 0 therefore becomes a regression pin.** Write the test anyway — the premise is external state YouTube can change under us, and this test is the only thing that would catch it.

Both pages were confirmed in **both** directions (anonymous `false`, authenticated `true`), so the marker is reliably present on every page this arc probes.

**The one residual risk, and why the measurements let us close it by construction.**

`watchPageSessionAuth`'s third branch returns `LoggedOut` for any page carrying `ytcfg.set` *without* a login key. Its safety rests on a comment's claim (`watch_page.go:246-254`) that consent interstitials carry no ytcfg at all — untested from an EU IP or a datacenter address, which is exactly the Docker deployment this feature targets. If a consent page *does* carry `ytcfg.set`, it reads as `LoggedOut`: a false alarm at an operator whose cookies are fine.

**Do not try to reproduce a consent wall to settle this. Delete the branch from the probe path instead.** All four measurements show these two pages *always* stamp the explicit `"LOGGED_IN":` key. The ytcfg fallback exists for watch pages, which may genuinely omit it; the probe pages demonstrably do not. So the probe can require an explicit marker and treat its absence as `Unknown` — which is the correct reading of an unrecognised page anyway, and makes the consent question moot regardless of what those pages contain. Implemented as `livenessVerdict` in Task 3.

Keep the redirect guard from Task 5 (H4) as defence in depth: it catches the *other* consent shape, where YouTube 302s to `consent.youtube.com` rather than interstitialing in place.

**Files:**
- Create: `internal/youtube/liveness_markers_live_test.go`

**Interfaces:**
- Consumes: `watchPageSessionAuth` (exists today — Task 0 runs *before* Task 3, so use the string version and `string(body)`; the copy does not matter in a manual test)
- Produces: a go/no-go answer for Tasks 3-6

- [ ] **Step 1: Write the live test**

Reuse the house convention from `internal/youtube/extraction_live_test.go` (env gate + `t.Skip`) and the existing `noopLogger` from `internal/youtube/service_test.go:10` — do **not** declare a new logger type.

```go
package youtube

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestLiveLoginMarkersPresent checks the premise the liveness arc is built
// on: that these two pages carry a marker watchPageSessionAuth can read.
//
// The anonymous half is the gate. LoggedOut is the ONLY verdict the arc
// acts on, so a page that answers Unknown to a signed-out request is
// useless for this purpose no matter what it does when authenticated.
//
// Enable with MOOMBOX_LIVE_YT_TEST=1 (matches extraction_live_test.go).
func TestLiveLoginMarkersPresent(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_YT_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_YT_TEST=1 to run the live login-marker check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
	}
	// A channel that offers memberships. Any will do — we are reading the
	// login marker, not the sponsorships tab.
	urls := map[string]string{
		"account feed":   constants.YouTubeURLs.Base + "/feed/subscriptions",
		"membership tab": constants.YouTubeURLs.Base + "/channel/UCBR8-60-B28hp2BmDPdntcQ/membership",
	}

	for name, u := range urls {
		body, err := utils.FetchBody(ctx, u, 20*time.Second, headers)
		if err != nil {
			t.Fatalf("%s: anonymous fetch failed: %v", name, err)
		}
		got := watchPageSessionAuth(string(body))
		if got != SessionAuthLoggedOut {
			t.Errorf("%s: anonymous verdict = %q, want LoggedOut.\n"+
				"The arc acts ONLY on LoggedOut. If this page cannot produce it, "+
				"pick a different page before building Tasks 3-6.", name, got)
		}
	}
}
```

- [ ] **Step 2: Run it**

```bash
MOOMBOX_LIVE_YT_TEST=1 go test ./internal/youtube/ -run TestLiveLoginMarkersPresent -v
```
PowerShell: `$env:MOOMBOX_LIVE_YT_TEST="1"; go test ./internal/youtube/ -run TestLiveLoginMarkersPresent -v`

- [ ] **Step 3: Act on the result**

Expected outcome is PASS — this was confirmed by hand on 2026-08-25 (table above). The test exists to catch YouTube changing it later.

- **Both LoggedOut → proceed to Task 1.** The premise holds.
- **Either one Unknown → STOP and report.** The page stopped carrying a readable marker. Do **not** "fix" it by loosening `sessionAuthFromBytes` to treat marker-absence as logged-out — that is precisely the over-claim `watch_page.go:246-254` was written to prevent, and it would alarm operators whose cookies are fine. The correct response is to pick a different probe page and re-run.
- **Either one LoggedIn on an anonymous fetch** → something is injecting credentials (a proxy, a shared cache). Investigate before trusting any verdict.
- **A fetch errors** → network or consent wall rather than a code problem. Retry; if it persists, report.

**Verifying the authenticated half — done 2026-08-25, and here is how to redo it safely.**

Use the browser, not a shell. Open `https://www.youtube.com/feed/subscriptions` in a browser already signed in to the archiving account, view source (Ctrl-U), and search for `"LOGGED_IN"`. Expect `true`.

**Prefer this over any scripted check.** The browser already holds the session, so nothing is copied out of `cookies.txt`, nothing is piped through a shell, and no credential material lands in a terminal history, a CI log, or an AI-assistant transcript. It takes ten seconds and carries none of the exposure a `Cookie:`-header one-liner does.

If you must script it (CI, a remote host with no browser), build the header from `cookies.txt` in a subshell, never echo it, and grep only curl's *response*. Treat any command that could print the header — a verbose flag, an error path, a shell trace — as a credential leak.

If it ever reports `false` for cookies you know work, **stop**: the arc would alarm on healthy credentials, and no amount of downstream plumbing fixes that.

- [ ] **Step 4: Commit the test**

```bash
git add internal/youtube/liveness_markers_live_test.go
git commit -m "test(youtube): live check that liveness probe pages carry a readable login marker"
```

---

### Task 1: Non-200 from the guide check is inconclusive (V3) — DONE (86544a9), plus Task 1b added

> **AS EXECUTED:** as written, with the H3 rollback consequence covered by `TestImportIsNotCommittedWhenTheRealCheckIsRateLimited`. The review then found **the Twitch mirror of this exact bug still live** (`checkTwitchAuth` returned `resp.StatusCode == http.StatusOK, nil`), executed immediately as **Task 1b (`bede4ef`)**: for `id.twitch.tv/oauth2/validate`, **200 = authenticated, 401 = conclusively dead (a 401 there genuinely means "sign in again" — do NOT blanket-copy the YouTube rule), everything else = inconclusive error naming only the status code** (never the body — a WAF echoing the Authorization header would otherwise render a credential in the UI, pinned by `TestTwitchValidateErrorNamesOnlyTheStatus`). The whole-branch round (`9c715d8`) later added a **request-provenance guard** (`authResponseIsOurs`) to all three tier-1 checks — a redirected/cookie-stripped exchange is inconclusive, never a verdict. **Known residual (2026-08-27 arc review, finding F1):** the YouTube guide BODY parse still treats an unrecognisable 200 body as a conclusive "not authenticated" — the last absence-of-marker detector in the subsystem; fix proposed (require the explicit `logged_in=0`, verified present on anonymous guide responses), not yet implemented.

Today `checkAndRefreshYouTube` returns `(false, nil)` on any non-200. `(false, nil)` means "conclusively not authenticated" to `shouldFireRecovery`, so a 429 or 503 is reported as dead credentials. It is currently masked by G6; fixing G6 first would surface false alarms.

**Files:**
- Modify: `internal/cookies/refresh.go:29-30` (URL consts → vars, for the test seam), `:586-589`, `:673-676`
- Test: `internal/cookies/refresh_status_test.go` (create)

**Interfaces:**
- Consumes: nothing
- Produces: `checkAndRefreshYouTube` and `checkYouTubeAuth` now return a non-nil `error` for any non-200; callers already treat `err != nil` as "network error, not auth loss" (`refresh.go:327-338`)

- [ ] **Step 1: Make the guide URLs test-seamable**

`internal/cookies/refresh.go` — change these two from `const` to `var` in their own block (leave the other consts alone):

```go
// Package vars, not consts, solely so tests can point them at an httptest
// server — these functions have no other seam (see refresh.go's note that
// the pure predicates were extracted for exactly this reason).
var (
	youtubeGuideURL        = "https://www.youtube.com/youtubei/v1/guide"
	youtubeGuideRefreshURL = "https://www.youtube.com/youtubei/v1/guide?prettyPrint=false"
)
```

- [ ] **Step 2: Write the failing test**

Create `internal/cookies/refresh_status_test.go`:

```go
package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// jarWithAuth writes a minimal Netscape file with the two cookies
// HasYouTubeAuthCookies requires, and loads it.
func jarWithAuth(t *testing.T) *CookieJar {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tsapisid-value\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-info-value\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestGuideNon200IsInconclusive: a 429 or 503 must NOT be reported as
// "conclusively not authenticated". shouldFireRecovery keys on checkErr ==
// nil, so returning (false, nil) here makes a rate-limit look like dead
// credentials — and once G6 unblocks the notification, an alarm.
func TestGuideNon200IsInconclusive(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		origRefresh, origPlain := youtubeGuideRefreshURL, youtubeGuideURL
		youtubeGuideRefreshURL, youtubeGuideURL = srv.URL, srv.URL
		rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

		auth, err := rs.checkAndRefreshYouTube(context.Background())

		youtubeGuideRefreshURL, youtubeGuideURL = origRefresh, origPlain
		srv.Close()

		if auth {
			t.Errorf("status %d: authenticated = true, want false", code)
		}
		if err == nil {
			t.Errorf("status %d: err = nil, want non-nil — a non-200 is not an auth verdict", code)
		}
	}
}

// TestGuide200LoggedOutIsConclusive: a real 200 that says logged_in=0 IS a
// conclusive auth failure and must keep returning a nil error.
func TestGuide200LoggedOutIsConclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"responseContext":{"mainAppWebResponseContext":{"loggedIn":false}}}`))
	}))
	defer srv.Close()
	origRefresh := youtubeGuideRefreshURL
	youtubeGuideRefreshURL = srv.URL
	defer func() { youtubeGuideRefreshURL = origRefresh }()

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	auth, err := rs.checkAndRefreshYouTube(context.Background())
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err != nil {
		t.Errorf("err = %v, want nil — a 200 saying logged-out is a real verdict", err)
	}
}
```

**Do not declare a logger type.** `internal/cookies` already has `nopLogger` (`refresh_test.go:11-16`) implementing the exact four-method interface — the tests above use it directly. (There is also `nopRefreshLogger` in `refresh_transitions_test.go:12-17`; either works, `nopLogger` is the shorter name.)

- [ ] **Step 3: Run the test and confirm it fails**

Run: `go test ./internal/cookies/ -run TestGuide -v`
Expected: `TestGuideNon200IsInconclusive` FAILS with "err = nil, want non-nil" for all three status codes. `TestGuide200LoggedOutIsConclusive` passes already.

- [ ] **Step 4: Make the non-200 path return an error**

`internal/cookies/refresh.go` — in **both** `checkYouTubeAuth` (~`:586`) and `checkAndRefreshYouTube` (~`:673`), replace:

```go
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}
```

with:

```go
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		// NOT (false, nil). That means "conclusively not authenticated" to
		// shouldFireRecovery, so a 429/503/edge block would be reported as
		// dead credentials. We learned nothing about the session here.
		return false, fmt.Errorf("youtube auth check: unexpected status %d", resp.StatusCode)
	}
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/cookies/ -run TestGuide -v`
Expected: both PASS.

- [ ] **Step 6: Run the full package and vet**

Run: `go test ./internal/cookies/... && go vet ./internal/cookies/...`
Expected: PASS, no vet output. Pay attention to `refresh_transitions_test.go` — it table-tests `shouldFireRecovery` directly and should be unaffected.

- [ ] **Step 7: Commit**

```bash
git add internal/cookies/refresh.go internal/cookies/refresh_status_test.go
git commit -m "fix(cookies): a non-200 guide response is an inconclusive check, not dead credentials"
```

---

### Task 2: The auth-loss gate stops conflating "never configured" with "expired" (V2) — DONE (70cd8cc, a439d4d)

> **AS EXECUTED — three deviations from the steps below, all reviewed and endorsed:**
> 1. **`checkTwitchAuth` KEEPS its conclusive `(false, nil)` on a missing token** — with no bearer token there is no credential to validate, so "not authenticated" is *true* rather than inferred, and probing with an empty `Authorization: OAuth ` header could manufacture the false failure. The "was Twitch ever configured" question moved to the doRefresh gate instead.
> 2. **`HasAnyTwitchAuthCookie` shipped broader than the snippet below**: `twitchAuthCookieNames = ["auth-token", "twilight-user"]`, because an expired auth-token can be pruned by `mergeCookieFiles` while `twilight-user` survives (an in-tree path) — so "any and all coincide" in the test comment below is **false as shipped**. `login`/`name` stay out, recorded as waiting-on-evidence.
> 3. An unrequested TOCTOU fix: `HasTwitchAuthCookies()` + `GetCookie("auth-token")` collapsed into one `GetTwitchAuthToken()` read (a `jar.Reload` between the two could send an empty header → 401 → false conclusive verdict).
>
> The doRefresh gate cited below as `refresh.go:303-304` sits at `refresh.go:637-638` as of `cc51b81` (the post-review fix round shifted `refresh.go` after `ffea847` — re-locate by the `hasYTCookies := rs.jar.HasAnyYouTubeAuthCookie()` line, not the number).
>
> **4. (Arc 5, `72f4373`) The `j.cookies` map every snippet below writes to NO LONGER EXISTS.** The jar is now two per-platform `map[string]cookieEntry` fields (`youtube`, `twitch`); `HasAnyYouTubeAuthCookie` / `HasYouTubeAuthCookies` read the YouTube jar and `HasAnyTwitchAuthCookie` / `GetTwitchAuthToken` the Twitch jar, so a `.twitch.tv` row named `SID` can no longer be read as YouTube evidence (the MINOR-2 hazard the Task 2 review recorded is closed structurally). The tests were migrated in the same commit; do not re-derive fixtures from the `j.cookies = ...` lines.

`shouldFireRecovery` ends `return cookiesPresent`, and the value passed is `jar.HasYouTubeAuthCookies()` — which requires SAPISID **and** LOGIN_INFO. A file with SAPISID but an empty LOGIN_INFO reads as "platform never configured", so the startup dead-auth case never fires. yt-dlp notes LOGIN_INFO is exactly what YouTube clears on rotation-invalidation, and finding S7 manufactures the same state.

**Files:**
- Modify: `internal/cookies/jar.go` (add predicate), `internal/cookies/refresh.go:303-304`
- Test: `internal/cookies/jar_test.go` (append)

**Interfaces:**
- Consumes: nothing
- Produces: `(*CookieJar).HasAnyYouTubeAuthCookie() bool` and `(*CookieJar).HasAnyTwitchAuthCookie() bool` — "was this platform ever configured", as opposed to `HasYouTubeAuthCookies`'s "is there a complete working set". Task 4 also uses `HasAnyYouTubeAuthCookie`.

- [ ] **Step 1: Write the failing test**

Append to `internal/cookies/jar_test.go`:

```go
// TestHasAnyYouTubeAuthCookie distinguishes "this platform was never
// configured" from "it was configured and the session has since been
// partially cleared". HasYouTubeAuthCookies cannot: it needs SAPISID AND
// LOGIN_INFO, and YouTube clears LOGIN_INFO on rotation-invalidation, so a
// half-dead file reads as never-configured and the auth-loss path stays
// silent forever.
func TestHasAnyYouTubeAuthCookie(t *testing.T) {
	tests := []struct {
		name    string
		cookies map[string]string
		wantAny bool
		wantAll bool
	}{
		{"empty jar", map[string]string{}, false, false},
		{"complete set", map[string]string{"SAPISID": "a", "LOGIN_INFO": "b"}, true, true},
		{"LOGIN_INFO cleared — configured but broken", map[string]string{"SAPISID": "a"}, true, false},
		{"SAPISID cleared — configured but broken", map[string]string{"LOGIN_INFO": "b"}, true, false},
		{"3PAPISID only", map[string]string{"__Secure-3PAPISID": "a"}, true, false},
		{"secure SID only", map[string]string{"__Secure-1PSID": "a"}, true, false},
		{"non-auth cookie only", map[string]string{"PREF": "x"}, false, false},
		{"empty values do not count", map[string]string{"SAPISID": "", "LOGIN_INFO": ""}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := NewCookieJar()
			j.cookies = tt.cookies
			if got := j.HasAnyYouTubeAuthCookie(); got != tt.wantAny {
				t.Errorf("HasAnyYouTubeAuthCookie() = %v, want %v", got, tt.wantAny)
			}
			if got := j.HasYouTubeAuthCookies(); got != tt.wantAll {
				t.Errorf("HasYouTubeAuthCookies() = %v, want %v", got, tt.wantAll)
			}
		})
	}
}

// TestHasAnyTwitchAuthCookie: Twitch auth is a single cookie, so "any" and
// "all" coincide. The predicate exists for symmetry at the call site.
func TestHasAnyTwitchAuthCookie(t *testing.T) {
	j := NewCookieJar()
	if j.HasAnyTwitchAuthCookie() {
		t.Error("empty jar reported a Twitch auth cookie")
	}
	j.cookies = map[string]string{"auth-token": "t"}
	if !j.HasAnyTwitchAuthCookie() {
		t.Error("jar with auth-token reported none")
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/cookies/ -run TestHasAny -v`
Expected: compile failure — `j.HasAnyYouTubeAuthCookie undefined`.

- [ ] **Step 3: Add the predicates**

Append to `internal/cookies/jar.go`, immediately after `HasYouTubeAuthCookies`:

```go
// youtubeAuthCookieNames are the cookies whose presence means "this install
// was configured for YouTube auth at some point". Deliberately broader than
// HasYouTubeAuthCookies' SAPISID+LOGIN_INFO pair: that pair answers "is
// there a complete working set right now", which is the wrong question for
// the auth-loss gate. A file holding SAPISID with LOGIN_INFO cleared is a
// CONFIGURED platform with BROKEN credentials — exactly the state worth
// reporting — and the narrower predicate reads it as never-configured.
var youtubeAuthCookieNames = []string{
	"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID",
	"SID", "HSID", "SSID", "APISID",
	"__Secure-1PSID", "__Secure-3PSID",
	"LOGIN_INFO",
}

// HasAnyYouTubeAuthCookie reports whether the jar holds ANY YouTube/Google
// auth cookie with a non-empty value — i.e. whether this install was ever
// configured for YouTube auth, regardless of whether the set is still
// complete. See youtubeAuthCookieNames for why this is not
// HasYouTubeAuthCookies.
func (j *CookieJar) HasAnyYouTubeAuthCookie() bool {
	if j == nil {
		return false
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	for _, name := range youtubeAuthCookieNames {
		if j.cookies[name] != "" {
			return true
		}
	}
	return false
}

// HasAnyTwitchAuthCookie is the Twitch counterpart. Twitch auth is a single
// cookie, so this coincides with HasTwitchAuthCookies; it exists so the
// auth-loss gate reads symmetrically for both platforms.
func (j *CookieJar) HasAnyTwitchAuthCookie() bool {
	if j == nil {
		return false
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.cookies["auth-token"] != ""
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/cookies/ -run TestHasAny -v`
Expected: PASS.

- [ ] **Step 4b: Broaden the four short-circuits that never reach the network (H2)**

This is what makes the arc meet its own liveness constraint, and it was missing.

`checkAndRefreshYouTube` (`refresh.go:641-654`) and `checkYouTubeAuth` (`:553-566`) each return `(false, nil)` from three early returns *before making any request*: strict `!HasYouTubeAuthCookies()`, empty `GetCookieHeader()`, empty `GenerateAuthorizationHeader()`. `(false, nil)` means "conclusively not authenticated", so for a SAPISID-present / LOGIN_INFO-cleared jar the alarm that fires is a **pure presence test** — exactly what the Global Constraints forbid.

Change the entry gate in **both** functions from `HasYouTubeAuthCookies()` to `HasAnyYouTubeAuthCookie()`:

```go
	if !rs.jar.HasAnyYouTubeAuthCookie() {
		// Nothing configured at all — no session to have an opinion about.
		return false, nil
	}
```

SAPISID alone is enough for both `GetCookieHeader` (`jar.go:215-229`) and `GenerateAuthorizationHeader` (`jar.go:348-373`) to produce output, so a half-cleared jar now **actually makes the request** and comes back with a real `logged_in=0` — genuine liveness instead of an inference from a missing cookie.

For the two remaining "cannot even form a request" returns, make them distinguishable rather than silently conclusive:

```go
	cookieHeader := rs.jar.GetCookieHeader()
	if cookieHeader == "" {
		return false, fmt.Errorf("youtube auth check: no cookie header could be built")
	}
	...
	authHeader := rs.jar.GenerateAuthorizationHeader(origin)
	if authHeader == "" {
		return false, fmt.Errorf("youtube auth check: no SAPISIDHASH could be generated")
	}
```

Add a test asserting a SAPISID-only jar reaches the server:

```go
// TestHalfClearedJarStillProbes: with LOGIN_INFO gone but SAPISID intact,
// the check must make a REQUEST and read YouTube's answer — not infer death
// from the missing cookie. Presence is not liveness.
func TestHalfClearedJarStillProbes(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"responseContext":{"mainAppWebResponseContext":{"loggedIn":false}}}`))
	}))
	defer srv.Close()
	orig := youtubeGuideRefreshURL
	youtubeGuideRefreshURL = srv.URL
	t.Cleanup(func() { youtubeGuideRefreshURL = orig })

	jar := jarWithAuth(t)
	jar.cookies["LOGIN_INFO"] = "" // the rotation-invalidation state

	rs := NewRefreshService(jar, 0, nopLogger{})
	auth, err := rs.checkAndRefreshYouTube(context.Background())
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1 — the check short-circuited instead of probing", hits)
	}
	if auth || err != nil {
		t.Errorf("auth=%v err=%v, want false/nil — a real logged-out verdict", auth, err)
	}
}
```

- [ ] **Step 5: Use the new predicate at the auth-loss gate**

`internal/cookies/refresh.go` — in `doRefresh`, replace lines ~303-304:

```go
	hasYTCookies := rs.jar.HasYouTubeAuthCookies()
	hasTWCookies := rs.jar.HasTwitchAuthCookies()
```

with:

```go
	// "Was this platform ever configured", NOT "is the set complete right
	// now". shouldFireRecovery's first-check branch returns this value, and
	// the complete-set predicate cannot tell a never-configured platform
	// from one whose LOGIN_INFO YouTube has cleared — which is the exact
	// state that must be reported, and was silent forever.
	hasYTCookies := rs.jar.HasAnyYouTubeAuthCookie()
	hasTWCookies := rs.jar.HasAnyTwitchAuthCookie()
```

Note `hasYTCookies` also feeds `rs.status.HasYouTubeCookies`. That is correct and is an improvement: the status now reports "cookies are configured" rather than "cookies are complete", which is what the UI label means.

- [ ] **Step 6: Run the full package**

Run: `go test ./internal/cookies/... -v -run "TestShouldFireRecovery|TestHasAny|TestPerPlatform"`
Expected: PASS. `shouldFireRecovery` itself is unchanged — only its input.

- [ ] **Step 7: Commit**

```bash
git add internal/cookies/jar.go internal/cookies/jar_test.go internal/cookies/refresh.go
git commit -m "fix(cookies): auth-loss gate no longer reads a half-cleared session as never-configured"
```

---

### Task 2b (ADDED IN EXECUTION): the autocookies subsystem still thought in strict presence — DONE (2198646, 3067796, 11fe29e)

Not in this plan as written; assembled from three review findings after Task 2 made `internal/cookies/refresh.go` and `internal/cookies/autocookies*.go` disagree about the same jar. Three sites, one theme:

1. **`refreshPlatforms`** (browser path) still asked the strict predicate, so Task 2's new alarms named a remedy that then *declined to run*. Broadened.
2. **`checkPlatformAuth`** (import path) mapped `hasCookies == false` straight to `verifyFailed` without calling the verify callback — telling a container operator "Moombox now holds no youtube cookies at all" for a `cookies.txt` that plainly held SAPISID, while the dashboard said the opposite at the same instant; and its `platformsToRestore` corollary let a destructive import commit over a half-cleared-but-working session. Fixed, with the rollback arm explicitly tested.
3. **`FinishSetup`** persisted presence-as-verified into `cfg.Cookies.Platforms` (durable state) on an inconclusive check, and its inconclusive branch was completely silent. Fixed via `ErrAuthCheckNotAttempted` wrapping exactly the four structural "could not even ask" gates plus an `attempted` field — genuine network failures keep `attempted=true` and are still accepted (refusing a login the user just completed is the worse, false-failure direction). The rollback message now distinguishes "the profile did not verify" from "the check did not complete".

Residual, deferred with a named owner-trigger: the "(network?)" hedge in one rollback message — whichever task next owns that switch should split it using `attempted`. See Follow-ups.

---

### Task 3: A `[]byte` login-verdict detector (V11a) — DONE (ce96411)

> **AS EXECUTED:** as written (markers inline as string consts, `livenessVerdict` strict variant, Task 0 pin re-pointed). The allocation commentary below was corrected AGAIN by the Task 4 review — see the "One correction reversed" paragraph above.
>
> **SUPERSEDED by the audit/B-fix round (`61f36bf`, `ad7451e`, `4021a74`) — the code below is a historical draft; do not re-derive from it.** As of `4021a74`:
> - **`sessionAuthFromBytes` no longer exists.** `livenessVerdict` is the only byte-side reader (the membership + account probes call it); `watchPageSessionAuth` is the only detector keeping the ytcfg fallback (watch pages only).
> - The marker keys are the quoted NAME only (`"LOGGED_IN"`, `"isLoggedIn"`) — **the colon left the literals** so whitespace on either side of it is tolerated (bounded, `sessionAuthMaxSpaceSkip = 8`); the scan resumes past non-marker occurrences of the key.
> - The VALUE is read by a shared generic `sessionAuthValue`: **LoggedIn only from an explicit `true` (bare or quoted), LoggedOut only from an explicit `false`, anything unreadable → Unknown** — an unreadable value returns immediately and never falls through to the camelCase key or the ytcfg bootstrap. This is the B1 fix: absence-of-`true` is no longer a dead session.
> - Tests renamed/merged: `TestSessionAuthFromBytesMatchesStringVersion` → `TestMarkerLookupTwinsAgree` (pins the string/bytes scan twins directly), `TestSessionAuthFromBytesDoesNotOverClaim` → `TestLivenessVerdictDoesNotOverClaim`, both allocation pins → `TestLivenessVerdictDoesNotAllocate`. New: `TestSessionAuthValueForms`, `TestUnreadableValueDoesNotFallThrough`, `TestSpacedKeyOnAWatchPageIsNotReadAsAnonymous`, `TestABareKeyOccurrenceDoesNotHideTheRealMarker`.

`watchPageSessionAuth` reads YouTube's own login verdict and correctly refuses to over-claim — it returns `Unknown` for consent walls and edge-error shells rather than asserting a dead session. It takes a `string`, but `parseMembershipTab` deliberately works on `[]byte` to avoid a `string(body)` copy of a ~1MB page (measured 98k → 32 allocs). Add a `bytes` twin sharing the markers.

**Files:**
- Modify: `internal/youtube/watch_page.go:260-282`
- Test: `internal/youtube/session_auth_test.go` (append)

**Interfaces:**
- Consumes: nothing
- Produces: `sessionAuthFromBytes(b []byte) SessionAuthState` — used by Tasks 4 and 5

- [ ] **Step 1: Write the failing test**

Append to `internal/youtube/session_auth_test.go`:

```go
// TestSessionAuthFromBytesMatchesStringVersion pins the two detectors to
// identical behaviour. They exist separately only because the membership
// path holds []byte and must not pay a ~1MB string copy — the SEMANTICS
// must never diverge, or a page read as logged-in on one path is read as
// dead on the other.
func TestSessionAuthFromBytesMatchesStringVersion(t *testing.T) {
	cases := []string{
		`<html>ytcfg.set({"LOGGED_IN":true});</html>`,
		`<html>ytcfg.set({"LOGGED_IN":false});</html>`,
		`<html>{"isLoggedIn":true}</html>`,
		`<html>{"isLoggedIn":false}</html>`,
		`<html>ytcfg.set({"OTHER":1});</html>`,
		`<html>consent interstitial, no ytcfg at all</html>`,
		``,
	}
	for _, html := range cases {
		want := watchPageSessionAuth(html)
		got := sessionAuthFromBytes([]byte(html))
		if got != want {
			t.Errorf("sessionAuthFromBytes(%q) = %q, watchPageSessionAuth = %q", html, got, want)
		}
	}
}

// TestSessionAuthFromBytesDoesNotOverClaim is the property the membership
// probe depends on: an unrecognisable page must be Unknown, never
// LoggedOut. Asserting death on a consent wall would alarm an operator
// whose cookies are fine.
func TestSessionAuthFromBytesDoesNotOverClaim(t *testing.T) {
	for _, html := range []string{
		"",
		"<html>502 Bad Gateway</html>",
		"<html>Before you continue to YouTube</html>",
	} {
		if got := sessionAuthFromBytes([]byte(html)); got != SessionAuthUnknown {
			t.Errorf("sessionAuthFromBytes(%q) = %q, want Unknown", html, got)
		}
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/youtube/ -run TestSessionAuthFromBytes -v`
Expected: compile failure — `undefined: sessionAuthFromBytes`.

- [ ] **Step 3: Extract the markers and add the twin**

`internal/youtube/watch_page.go` — add above `watchPageSessionAuth`:

```go
// Login-verdict markers, shared by the string and []byte detectors so the
// two can never drift. Two ytcfg spellings have been observed for the same
// flag; either counts.
const (
	sessionAuthKey       = `"LOGGED_IN":`
	sessionAuthCamelKey  = `"isLoggedIn":`
	sessionAuthTrue      = "true"
	sessionAuthYtcfgMark = "ytcfg.set"
)

```

**On allocation — measured, and then re-measured, because this paragraph was wrong as first written.** The `[]byte(const)` conversions written inline below cost **nothing**: `go build -gcflags=-m` reports "does not escape" for every marker, and `testing.AllocsPerRun(200, …)` over a 900 KB input gives **0 allocs/op**. TWO independent things keep them free: the markers are **compile-time constants** (for which the compiler emits an exact-size stack array — **no size ceiling applies**), and `bytes.Index`/`HasPrefix`/`Contains` do not retain their argument, so escape analysis keeps the conversion local. The 32-byte `tmpStringBufSize` ceiling is real but binds only the NON-constant case (a package var allocates from 33 bytes up; a const never does). So the failure modes to guard are "a converted marker escapes" or "a marker stops being a constant AND exceeds 32 bytes" — not marker length alone. Full measured matrix and runtime citations: `session_auth_test.go:201-229`. The inline form is kept because it reads closer to the string version it must stay in sync with.

Rewrite `watchPageSessionAuth` to use them (behaviour identical), and add the twin below it:

```go
// sessionAuthFromBytes is watchPageSessionAuth over raw response bytes.
//
// It exists because callers holding a ~1MB page as []byte must not pay a
// string copy just to read one flag — internal/youtube/channel_membership.go
// is explicit about that cost (98k → 32 allocs from lazy decoding). Three
// bytes.Index/Contains calls, no allocation.
//
// KEEP IN SYNC with watchPageSessionAuth. TestSessionAuthFromBytesMatchesStringVersion
// enforces it.
func sessionAuthFromBytes(b []byte) SessionAuthState {
	if i := bytes.Index(b, []byte(sessionAuthKey)); i >= 0 {
		if bytes.HasPrefix(b[i+len(sessionAuthKey):], []byte(sessionAuthTrue)) {
			return SessionAuthLoggedIn
		}
		return SessionAuthLoggedOut
	}
	if i := bytes.Index(b, []byte(sessionAuthCamelKey)); i >= 0 {
		if bytes.HasPrefix(b[i+len(sessionAuthCamelKey):], []byte(sessionAuthTrue)) {
			return SessionAuthLoggedIn
		}
		return SessionAuthLoggedOut
	}
	if bytes.Contains(b, []byte(sessionAuthYtcfgMark)) {
		return SessionAuthLoggedOut
	}
	return SessionAuthUnknown
}
```

Then add the probe-only strict variant, which is what Tasks 4-6 actually call:

```go
// livenessVerdict is sessionAuthFromBytes with the ytcfg fallback removed.
//
// That fallback ("a shell carrying ytcfg.set but no login key is anonymous")
// is sound for watch pages, which may legitimately omit the key. It is NOT
// safe for the liveness probe: a consent interstitial carrying ytcfg would
// read as a dead session and alarm an operator whose cookies are fine —
// from an EU or datacenter IP, i.e. the Docker deployment this targets.
//
// The probe does not need it. Measured 2026-08-25, both probe pages stamp
// the explicit key in both directions (anonymous false / authenticated
// true), on /feed/subscriptions and /channel/<id>/membership alike. So
// requiring the explicit marker costs nothing real and makes the consent
// question moot: an unrecognised page is Unknown, which is the truthful
// answer for a page we cannot read.
func livenessVerdict(b []byte) SessionAuthState {
	if !bytes.Contains(b, []byte(sessionAuthKey)) && !bytes.Contains(b, []byte(sessionAuthCamelKey)) {
		return SessionAuthUnknown
	}
	return sessionAuthFromBytes(b)
}
```

```go
// TestLivenessVerdictRefusesTheYtcfgFallback: the whole point. A page with a
// ytcfg bootstrap and no login key is Unknown to the probe, even though
// sessionAuthFromBytes calls it LoggedOut for watch-page purposes.
func TestLivenessVerdictRefusesTheYtcfgFallback(t *testing.T) {
	shell := []byte(`<html>ytcfg.set({"OTHER":1});</html>`)
	if got := sessionAuthFromBytes(shell); got != SessionAuthLoggedOut {
		t.Fatalf("precondition: sessionAuthFromBytes = %q, want LoggedOut", got)
	}
	if got := livenessVerdict(shell); got != SessionAuthUnknown {
		t.Errorf("livenessVerdict = %q, want Unknown — a consent shell must not read as a dead session", got)
	}
	// Explicit markers still pass through unchanged.
	if got := livenessVerdict([]byte(`{"LOGGED_IN":true}`)); got != SessionAuthLoggedIn {
		t.Errorf("livenessVerdict(logged-in) = %q, want LoggedIn", got)
	}
	if got := livenessVerdict([]byte(`{"LOGGED_IN":false}`)); got != SessionAuthLoggedOut {
		t.Errorf("livenessVerdict(logged-out) = %q, want LoggedOut", got)
	}
}
```

**Tasks 4 and 5 call `livenessVerdict`, never `sessionAuthFromBytes` directly.** (Task 6 calls neither — it consumes `SessionAuthState` values produced upstream.)

- [ ] **Re-point Task 0's regression pin at `livenessVerdict`**

Task 0 asserts on `watchPageSessionAuth` because it runs before this task exists. Now that `livenessVerdict` does exist, **update `TestLiveLoginMarkersPresent` to assert on it** — otherwise the pin tests a function production no longer uses. The failure it would miss is precise and severe: if YouTube drops the explicit `"LOGGED_IN":` key but keeps the ytcfg bootstrap, Task 0 still passes (the fallback returns `LoggedOut`) while `livenessVerdict` returns `Unknown` in production and the whole arc goes silent — exactly the "fails safe but does nothing, and looks shipped" outcome Task 0 was written to catch. Assert both functions if you want to keep the string-version coverage, but `livenessVerdict` is the one that must be pinned.

Pin the zero-allocation property so a future marker change cannot quietly break it:

```go
// TestSessionAuthFromBytesDoesNotAllocate guards the reason this function
// exists. (Comment as SHIPPED differs from this draft: the 32-byte ceiling
// binds only non-constant sources — a failure means a converted marker
// escaped, or stopped being a const while exceeding 32 bytes. See
// session_auth_test.go:201-229 for the authoritative measured-matrix text.)
func TestSessionAuthFromBytesDoesNotAllocate(t *testing.T) {
	page := append(bytes.Repeat([]byte("x"), 900<<10), []byte(`ytcfg.set({"LOGGED_IN":true});`)...)
	if n := testing.AllocsPerRun(200, func() { _ = sessionAuthFromBytes(page) }); n != 0 {
		t.Errorf("allocs/op = %v, want 0 — did a converted marker escape, or stop being a const while exceeding 32 bytes?", n)
	}
}
```

Add `"bytes"` to the import block.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/youtube/ -run TestSessionAuth -v`
Expected: PASS, including the pre-existing `TestWatchPageSessionAuth` (unchanged behaviour).

- [ ] **Step 5: Commit**

```bash
git add internal/youtube/watch_page.go internal/youtube/session_auth_test.go
git commit -m "feat(youtube): add a zero-copy []byte login-verdict detector"
```

---

### Task 4: The membership probe returns its login verdict (V11b) — DONE (268fca9, f19b8a6)

> **AS EXECUTED:** signature `FetchMembershipVideos(ctx, channelID) ([]MembershipVideo, SessionAuthState, error)` with a `membershipPageBase` test seam; the verdict comes from `livenessVerdict` (strict). Gate citations below have moved: the cmd gate is the `MembershipEnabled` closure (`monitor_callbacks.go:621-668` at `cc51b81`), the probe's own gate is `channel_membership.go:85`, and the backfill-sweep gate LEFT STRICT is now `services.go:591` (verified 2026-08-27 at `cc51b81`). Task 5 later replaced the raw fetch with the shared `fetchLivenessPage` guard — see Task 5's note.

`FetchMembershipVideos` already fetches an authed page with the real cookie header every monitor cycle and computes `hasAccess`, then collapses "cookies dead", "not a member", and "no members content" into `return nil, nil`. Return the verdict instead.

**Files:**
- Modify: `internal/youtube/channel_membership.go:64-98`
- Modify: callers in `internal/monitor/` (find with `grep -rn FetchMembershipVideos internal/ cmd/`)
- Test: `internal/youtube/channel_membership_test.go` (append or create)

**Interfaces:**
- Consumes: `sessionAuthFromBytes` (Task 3), `HasAnyYouTubeAuthCookie` (Task 2)
- Produces: `FetchMembershipVideos(ctx, channelID) ([]MembershipVideo, SessionAuthState, error)` — Task 6 consumes the verdict

- [ ] **Step 1: Write the failing test**

```go
// TestFetchMembershipVideosReportsLoginVerdict: the probe's value is the
// LOGIN verdict, not hasAccess. Most archived channels legitimately are not
// membered, so hasAccess == false carries no health information; a
// logged-out page does.
func TestMembershipVerdictFromBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want SessionAuthState
	}{
		{"logged in, member", `ytcfg.set({"LOGGED_IN":true}); "tabIdentifier":"TAB_ID_SPONSORSHIPS"`, SessionAuthLoggedIn},
		{"logged in, not a member", `ytcfg.set({"LOGGED_IN":true}); no sponsorships tab here`, SessionAuthLoggedIn},
		{"session is dead", `ytcfg.set({"LOGGED_IN":false});`, SessionAuthLoggedOut},
		{"consent wall", `Before you continue`, SessionAuthUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionAuthFromBytes([]byte(tt.body)); got != tt.want {
				t.Errorf("verdict = %q, want %q", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/youtube/ -run TestMembershipVerdict -v`
Expected: FAIL or compile error until Task 3 is merged; if Task 3 is already in, this passes — that is fine, it is a guard for the signature change below.

- [ ] **Step 3: Change the signature and stop swallowing the verdict**

`internal/youtube/channel_membership.go` — replace the body of `FetchMembershipVideos`:

```go
func (s *Service) FetchMembershipVideos(ctx context.Context, channelID string) ([]MembershipVideo, SessionAuthState, error) {
	// "Was YouTube auth ever configured", not "is the set complete". The
	// narrower predicate would skip the probe precisely when the session is
	// half-cleared — the state the probe exists to detect.
	if !s.Auth.HasAnyAuthCookie() {
		return nil, SessionAuthUnknown, nil
	}
	if err := s.Auth.SyncCookies(); err != nil {
		s.logger.Warn("[YouTube] SyncCookies failed before membership fetch", "error", err)
	}

	pageURL := fmt.Sprintf("%s/channel/%s/membership", constants.YouTubeURLs.Base, url.PathEscape(channelID))
	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
	}
	if ch := s.Auth.GetCookieHeader(); ch != "" {
		headers["Cookie"] = ch
	}

	body, err := utils.FetchBody(ctx, pageURL, 20*time.Second, headers)
	if err != nil {
		// A transport failure is not a login verdict.
		return nil, SessionAuthUnknown, fmt.Errorf("fetch membership tab: %w", err)
	}

	// The login verdict is the health signal. hasAccess is NOT: an archived
	// channel the account holds no membership for is the normal case.
	// livenessVerdict, NOT sessionAuthFromBytes — the strict variant refuses
	// the ytcfg fallback so a consent shell cannot read as a dead session.
	verdict := livenessVerdict(body)

	videos, hasAccess := parseMembershipTab(body)
	if !hasAccess {
		return nil, verdict, nil
	}
	return videos, verdict, nil
}
```

Add `HasAnyYouTubeAuthCookie` to `internal/youtube/auth.go`:

```go
// HasAnyAuthCookie reports whether YouTube auth was ever configured, as
// opposed to whether a complete working set is present. See
// cookies.CookieJar.HasAnyYouTubeAuthCookie.
//
// The "YouTube" qualifier is dropped here on purpose: Auth IS the YouTube
// auth type, matching the existing Auth.HasAuthCookies (auth.go:38).
func (a *Auth) HasAnyAuthCookie() bool {
	return a.jar.HasAnyYouTubeAuthCookie()
}
```

**Naming across the three layers — get this right or it will not compile:** `CookieJar.HasAnyYouTubeAuthCookie()` (platform-qualified, because the jar holds both platforms) → `Auth.HasAnyAuthCookie()` → `Service.HasAnyAuthCookie()`. Every `s.Auth.` call in Tasks 4 and 5 uses **`HasAnyAuthCookie`**; only `a.jar.`/`rs.jar.` calls use the qualified name.

- [ ] **Step 4: Update the one real caller (B2)**

There is **exactly one** call site — `cmd/moombox/monitor_callbacks.go:400`, inside the `s.feedMon.FetchMembership` closure. Everything else that mentions `FetchMembershipVideos` is a doc comment (`internal/monitor/feed.go:91,157`, `feed_test.go:47`, `internal/youtube/browse.go:209,225`). **`internal/monitor` needs no code change** — `MembershipFetchFunc` (`feed.go:92`) keeps its 2-value signature and the cmd closure adapts.

```go
	s.feedMon.FetchMembership = func(ctx context.Context, channelID string) ([]monitor.MembershipVideo, error) {
		vids, verdict, err := s.ytService.FetchMembershipVideos(ctx, channelID)
		if err != nil {
			return nil, err
		}
		_ = verdict // Task 6 routes this; keep the adapter's shape unchanged.
		out := make([]monitor.MembershipVideo, len(vids))
		for i, v := range vids {
			out[i] = monitor.MembershipVideo{VideoID: v.VideoID, Title: v.Title, Age: v.Age}
		}
		return out, nil
	}
```

- [ ] **Step 4b: Broaden the gates that decide whether the probe runs at all (H1) — CRITICAL**

Without this the whole arc is inert in its target case: three gates use the **strict** `HasAuthCookies()`, so with LOGIN_INFO cleared the probe is never called and the verdict above is dead code.

First add the delegate to `internal/youtube/service.go`, next to `HasAuthCookies` (`:283-285`):

```go
// HasAnyAuthCookie reports whether YouTube auth was ever configured, as
// opposed to whether a complete working set is present right now. Gates that
// decide "should we even try" must use this; a gate using HasAuthCookies
// skips exactly the half-cleared session worth detecting.
func (s *Service) HasAnyAuthCookie() bool {
	return s.Auth.HasAnyAuthCookie()
}
```

Then switch **both** gates:

- `cmd/moombox/monitor_callbacks.go:415` — `return enabled && s.ytService.HasAnyAuthCookie()`

**Exactly TWO gates, not three.** Revision 2 also broadened `cmd/moombox/services.go:467`. **That was wrong and is now reverted — do not touch it.** It gates the *backfill sweep*, which never calls `FetchMembershipVideos` (it goes through `FetchChannelTabPage`, `browse.go:217`, which returns no verdict) — so broadening it yields **zero liveness benefit**. What it does cause is durable data loss: flipping it re-queues a full catalog re-scan (`backfill.go:461-466`), and `completeScan` then persists `backfilled_with_membership = true` (`backfill.go:671`) even though the session was dead and the membership tab came back empty. Because the membership arm only re-fires on a recorded `false`, **the channel is permanently marked membership-complete and its members-only backlog is never re-scanned** once cookies are fixed. That is an Arc-2/Arc-5 change requiring `backfilled_with_membership` handling, not an Arc-1 liveness change.

Leave the other `HasAuthCookies()` uses alone — `internal/worker/stream_processor_youtube.go:267` and `internal/youtube/service.go:196` ask "do we have a usable set", which is the right question there; `service.go:303` (`GetAuthState`) and `internal/youtube/auth.go:94` (the `X-Youtube-Bootstrap-Logged-In` header, an assertion made *to* YouTube) also stay strict. Note the discovery grep `grep -rn "HasAuthCookies()"` does **not** match `HasYouTubeAuthCookies()` — it is a shortlist, not an exhaustive enumeration.

- [ ] **Step 4c: Fix the doc comment the signature change invalidates (M3)**

`internal/youtube/channel_membership.go:51-63` still says the function "returns `(nil, nil)` — no error — when…". Rewrite it for the three-value shape, and say plainly that the **login verdict is the health signal and `hasAccess` is not**.

- [ ] **Step 5: Add the test that actually tests this task (N5)**

`TestMembershipVerdictFromBody` only re-exercises `sessionAuthFromBytes`, which Task 3 already covers — it would pass without any of this task's changes. Add one that pins the new contract:

```go
// TestFetchMembershipVideosReturnsVerdict: a non-member page must still
// report LoggedIn. Most archived channels legitimately are not membered, so
// swallowing that as "nothing to do" is what hid the health signal.
func TestFetchMembershipVideosReturnsVerdict(t *testing.T) {
	// Serve a logged-in page with NO sponsorships tab.
	// Assert: videos empty, verdict == SessionAuthLoggedIn, err == nil.
	// Then serve a logged-out page and assert verdict == SessionAuthLoggedOut.
}
```

Point the fetch at an httptest server the same way Task 5 does with `accountProbeURL`; if `FetchMembershipVideos` has no URL seam, add one in the same style.

- [ ] **Step 6: Build, vet, commit — atomically**

Run: `go build ./... && go vet ./... && go test ./...`
(`go test ./internal/youtube/... ./internal/monitor/...` would **not** compile `cmd/moombox`, where the only caller lives.)

This commit must include every package or the tree does not build:

```bash
git add internal/youtube/ cmd/moombox/
git commit -m "feat(youtube): membership probe reports its login verdict, and the gates stop hiding it"
```

---

### Task 5: Channel-independent fallback probe (V12) — DONE (ca1abba, e5e5c95, f702653)

> **AS EXECUTED — the Step 3 implementation below is superseded.** The shipped probe does NOT use `utils.FetchBody`: review proved with a standalone repro that Go's redirect cookie-strip is **sticky** (`origin → wall → origin` lands back on the probe host with the Cookie permanently stripped, passes any terminal-host check, and delivers an anonymous body that reads as `LoggedOut` — the exact false alarm). Both probes now share `internal/youtube/liveness_fetch.go`'s `fetchLivenessPage`, which admits a body only when (1) we had cookies to send, (2) the final response came from the exact scheme+host asked for, and (3) **the request that finally answered still carried the Cookie header** — rule 3 subsumes the host check and rests on `utilsHTTPClient` having no `http.CookieJar`, pinned at both ends (`TestUtilsHTTPClientCarriesNoCookieJar`). The H4 paragraph's `isConsentRedirect` remedy was rejected as strictly weaker (it matches only `consent.*` and would miss `accounts.google.com`). The "Verified: utils.FetchBody errors on non-200 …" paragraph below described the superseded draft. Scope note: the guard gates membership DISCOVERY as well as liveness — a legitimate off-origin redirect on `/channel/<id>/membership` yields no videos, deliberately.

Membership discovery is per-channel, so an install with no YouTube channels gets no tier-2 probe. Add an account-page probe that runs **only when no membership observation has arrived recently**, so a normally-configured install pays nothing.

**Files:**
- Create: `internal/youtube/account_probe.go`
- Test: `internal/youtube/account_probe_test.go`

**Interfaces:**
- Consumes: `sessionAuthFromBytes` (Task 3), `HasAnyYouTubeAuthCookie` (Task 2)
- Produces: `(*Service).ProbeAccountLiveness(ctx) (SessionAuthState, error)` — Task 6 calls it

- [ ] **Step 1: Write the failing test**

Create `internal/youtube/account_probe_test.go`:

```go
package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProbeAccountLivenessVerdicts: the probe must return a three-state
// verdict and must never claim LoggedOut for a page it does not recognise.
func TestProbeAccountLivenessVerdicts(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   SessionAuthState
		wantEr bool
	}{
		{"logged in", 200, `ytcfg.set({"LOGGED_IN":true});`, SessionAuthLoggedIn, false},
		{"session dead", 200, `ytcfg.set({"LOGGED_IN":false});`, SessionAuthLoggedOut, false},
		{"consent wall", 200, `Before you continue`, SessionAuthUnknown, false},
		{"rate limited", 429, ``, SessionAuthUnknown, true},
		{"server error", 503, ``, SessionAuthUnknown, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			orig := accountProbeURL
			accountProbeURL = srv.URL
			defer func() { accountProbeURL = orig }()

			svc := newTestServiceWithAuthCookies(t)
			got, err := svc.ProbeAccountLiveness(context.Background())
			if got != tt.want {
				t.Errorf("verdict = %q, want %q", got, tt.want)
			}
			if (err != nil) != tt.wantEr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantEr)
			}
		})
	}
}
```

**Write `newTestServiceWithAuthCookies` carefully — the obvious shortcut nil-panics (M4).** Do **not** reuse `newTestService` (`internal/youtube/service_test.go:20`): it builds `&Service{logger: noopLogger{}}` with a **nil `Auth`**, and `ProbeAccountLiveness`'s first line calls `s.Auth.HasAnyAuthCookie()`. Go through `NewService(jar, noopLogger{})` (`service.go:70-92`). `jarWithAuth` from Task 1 is an unexported helper in package `cookies` and is **not reachable from package `youtube`** — reimplement it locally. The test file needs `os`, `path/filepath` and `internal/cookies` on top of the imports listed above.

**The probe is blind to a login redirect (H4).** `utils.FetchBody` returns bytes only — the final post-redirect URL is unreachable. If a logged-out `/feed/subscriptions` ever 302s to `accounts.google.com/ServiceLogin` or `consent.youtube.com`, the body may carry no `ytcfg.set` and the verdict degrades to `Unknown` — "learned nothing" for a genuinely dead session. Measured 2026-08-25: it does **not** redirect from a desktop IP (200, `"LOGGED_IN":false`, 6 × `ytcfg.set`), so this is insurance, not a present bug — but EU regions and datacenter IPs are untested and Docker is the target deployment. `FetchWatchPage` already solved exactly this with `utils.FetchWithTimeout` + `isConsentRedirect` (`watch_page.go:182-217`); reuse that instead of `FetchBody`, and map a consent/login redirect to `Unknown` explicitly rather than relying on marker absence.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/youtube/ -run TestProbeAccountLiveness -v`
Expected: compile failure — `undefined: accountProbeURL`, `undefined: ProbeAccountLiveness`.

- [ ] **Step 3: Implement the probe**

Create `internal/youtube/account_probe.go`:

```go
package youtube

import (
	"context"
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// accountProbeURL is a first-party page that renders a recognisably
// logged-in shell for an authenticated session and a logged-out one
// otherwise. /feed/subscriptions is stable, needs no configuration, and —
// unlike a members-only video probe — has no ID that ages out.
//
// A var, not a const, purely so tests can point it at an httptest server.
var accountProbeURL = constants.YouTubeURLs.Base + "/feed/subscriptions"

// ProbeAccountLiveness answers "do these cookies still work" without
// reference to any channel.
//
// The membership probe (FetchMembershipVideos) is the preferred liveness
// source: it is already being made every monitor cycle and it proves
// CAPABILITY, not just recognition. But it is per-channel, so an install
// with no YouTube channels — or with membership discovery off everywhere —
// gets no observation at all. This fills that gap and nothing else; the
// caller should skip it whenever a membership observation is recent.
//
// Returns SessionAuthUnknown plus an error for any transport failure or
// non-200. That asymmetry is the whole point: a rate limit is not a verdict
// on the session, and reporting it as one is how an operator with healthy
// cookies gets told they are dead.
func (s *Service) ProbeAccountLiveness(ctx context.Context) (SessionAuthState, error) {
	if !s.Auth.HasAnyAuthCookie() {
		// Nothing configured — there is no session to have an opinion about.
		return SessionAuthUnknown, nil
	}
	if err := s.Auth.SyncCookies(); err != nil {
		s.logger.Warn("[YouTube] SyncCookies failed before account liveness probe", "error", err)
	}

	headers := map[string]string{
		"User-Agent":      constants.UserAgents.Web,
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.5",
	}
	if ch := s.Auth.GetCookieHeader(); ch != "" {
		headers["Cookie"] = ch
	}

	body, err := utils.FetchBody(ctx, accountProbeURL, 20*time.Second, headers)
	if err != nil {
		return SessionAuthUnknown, fmt.Errorf("account liveness probe: %w", err)
	}
	// livenessVerdict, NOT sessionAuthFromBytes — see Task 3.
	return livenessVerdict(body), nil
}
```

**Verified:** `utils.FetchBody` already returns `nil, fmt.Errorf("HTTP %d from %s", ...)` for anything outside 2xx (`internal/utils/http.go:103-105`), so the non-200 path needs no extra check — the `err != nil` branch above covers it. It also caps the read at `MaxFetchBodySize` = 50 MB (`http.go:16,107`), far above a ~1 MB page, so no truncation risk.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/youtube/ -run TestProbeAccountLiveness -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/youtube/account_probe.go internal/youtube/account_probe_test.go
git commit -m "feat(youtube): channel-independent account liveness probe"
```

---

### Task 6: Route liveness verdicts into the health signal (V11c) — DONE (9692561, 6c89b9f), landed LOG-ONLY

> **AS EXECUTED:** shipped behind `const livenessRecoveryArmed = false` — verdicts are recorded, deduped and logged (Info only when signed-out / verdict changed / first observation of the process; Debug otherwise), and the `OnRecoveryNeeded` call is withheld. Since `ad7451e` an INCONCLUSIVE fallback probe also logs (`"liveness fallback probe learned nothing about this session"`, once per change via the shared `lastLivenessKnown` record), so a permanently-refused signal is distinguishable from a healthy quiet one. Naming drift: the plan's `lastRecoveryFired` map shipped as `lastRecoveryDecided` ("decided", because under the pilot the stamp records a decision, not a call). The freshness window is its own constant, `livenessFreshWindow = 25 * time.Minute`; the fallback is also skipped on `Start`'s synchronous initial check (not just `CheckNow`), so tier-2 coverage begins one cadence after boot and **there is no manual way to force a tier-2 observation** — an arming-checklist item.
>
> **The cost model stated below was FALSE and is corrected here (review I1).** `RefreshCookiesDetailed` **single-flights**, so N un-deduped verdicts never buy N headless browsers — calls 2..N are *declined*. **[CORRECTED AGAIN at `ad7451e`]** The declined-decline damage chain the first correction described (decline → spurious "Cookie Auto-Refresh Ineffective" → stamps `lastAuthFailNotify` → the real "Failed" suppressed) **was then FIXED in code, not merely bounded**: `runCookieRecovery`'s Unknown branch now splits on `RefreshResult.Ran`, and a declined pass logs one Info line, sends nothing, and stamps no cooldown (`TestDeclinedRecoveryDoesNotSpendTheCooldown` drives the two-platform sequence; both platforms now receive their accurate conclusive-failure notification). What the dedupe buys **as shipped** is therefore workload only: one real headless-browser refresh per 30 minutes instead of one per feed cycle, plus a goroutine per redundant fire spent being told no. The shipped guard for the tier-1/tier-2 same-pass pair is `noteRecoveryDecided` (tier-1 stamps the shared dedupe map before firing; one-directional on purpose).

**Files:**
- Modify: `internal/cookies/refresh.go` (accept external verdicts), `cmd/moombox/monitor_callbacks.go` (route them), `cmd/moombox/services.go` (schedule the fallback)
- Test: `internal/cookies/refresh_liveness_test.go` (create)

**Interfaces:**
- Consumes: Tasks 3-5
- Produces: `(*RefreshService).ObserveLiveness(platform string, loggedIn bool)` — records an external liveness observation and fires `OnRecoveryNeeded` on a logged-out verdict

- [ ] **Step 1: Write the failing test**

```go
// TestObserveLivenessFiresOnlyOnLoggedOut: LoggedIn is positive evidence and
// must be silent; Unknown must never move state (a consent wall is not a
// dead session); only LoggedOut fires recovery.
func TestObserveLivenessFiresOnlyOnLoggedOut(t *testing.T) {
	var fired []string
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	rs.OnRecoveryNeeded = func(p string) { fired = append(fired, p) }

	rs.ObserveLiveness("youtube", true)
	if len(fired) != 0 {
		t.Fatalf("logged-in observation fired recovery: %v", fired)
	}
	rs.ObserveLiveness("youtube", false)
	if len(fired) != 1 || fired[0] != "youtube" {
		t.Fatalf("logged-out observation did not fire recovery: %v", fired)
	}
}

// TestObserveLivenessDedupes: the membership probe runs once per channel per
// cycle, so a dead session produces N logged-out verdicts. Recovery must
// fire once, not N times.
func TestObserveLivenessDedupes(t *testing.T) {
	var fired int
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	rs.OnRecoveryNeeded = func(string) { fired++ }
	for range 5 {
		rs.ObserveLiveness("youtube", false)
	}
	if fired != 1 {
		t.Errorf("fired %d times, want 1 — N channels must not mean N alerts", fired)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/cookies/ -run TestObserveLiveness -v`
Expected: compile failure — `rs.ObserveLiveness undefined`.

- [ ] **Step 3: Implement `ObserveLiveness`**

Add to `internal/cookies/refresh.go`:

```go
// livenessRefireWindow bounds how often one platform's logged-out verdict
// re-fires recovery. The membership probe runs once per channel per cycle,
// so a dead session produces N verdicts.
//
// [CORRECTED — the draft said "without this, N channels means N alerts",
// which is FALSE: RefreshCookiesDetailed single-flights, so redundant fires
// are DECLINED. A decline USED to report as a spurious "Ineffective"
// notification that stamped the cooldown and suppressed the real verdict's
// message; as of ad7451e a declined pass reports NOTHING and stamps nothing
// (the Ran split), so the window's remaining value is workload — one real
// browser refresh per 30 min instead of per cycle. The shipped comment at
// refresh.go carries the current accounting.]
//
// Deliberately its own constant — this is NOT the notification cooldown in
// cmd/moombox's wireMonitorCallbacks, and it is only coincidentally equal
// to defaultRefreshInterval.
const livenessRefireWindow = 30 * time.Minute
```

**TWO maps, not one.** Revision 2 used a single map for two different questions, which made `TestFallbackSkippedWhenMembershipIsFresh` unpassable by construction:

```go
// lastLivenessObserved records the last CONCLUSIVE observation per platform,
// in BOTH directions. Consulted only by the fallback-probe skip: "did the
// membership probe already tell us something recently?"
lastLivenessObserved map[string]time.Time

// lastRecoveryFired records when a logged-out verdict actually fired
// OnRecoveryNeeded. Consulted only by the dedupe. A LoggedIn observation
// must never touch THIS map — otherwise a healthy verdict at t0 swallows a
// dead one at t0+ε.
lastRecoveryFired map[string]time.Time
```

Contract:

- `ObserveLiveness(platform, loggedIn)` writes `lastLivenessObserved[platform] = now` on **every** call (both directions) — that is what makes an install with channels skip the fallback.
- Only `loggedIn == false` reads and writes `lastRecoveryFired`, and only that path fires `OnRecoveryNeeded`.
- `Unknown` never reaches this method; the caller filters it, so reaching here always means a conclusive observation.
- Take `rs.mu` to read/update both maps, then **release it before invoking `OnRecoveryNeeded`** — `doRefresh` already establishes that convention (`refresh.go:341-352`).
- Use **two separate windows**. `livenessRefireWindow` (dedupe) wants to be ≥ the notification cooldown; the freshness window for the fallback wants to be ≈ the refresh cadence. Reusing one constant — which is also coincidentally `defaultRefreshInterval` — makes the freshness test race its own cadence.
- `doRefresh` does **not** consult the recorded verdict. `ObserveLiveness` fires recovery directly; `doRefresh` keeps owning the tier-1 guide check.

**Guard against double-firing in one pass.** Once Step 5 lands, a dead session triggers `OnRecoveryNeeded("youtube")` twice per `doRefresh` — once from `shouldFireRecovery` and once from the fallback's `ObserveLiveness`. [CORRECTED — the draft claimed "each call spawns its own RefreshCookies goroutine with a 2-minute timeout and a headless browser launch"; **false**: the auto-cookie single-flight declines the second call. The first correction then blamed the declined call's spurious "Ineffective" notification stamping `lastAuthFailNotify`; **that chain was itself fixed at `ad7451e`** — a declined pass now reports nothing and stamps nothing, so the double-fire's remaining cost is a redundant goroutine and its timeout.] **As shipped**, tier-1 stamps the shared dedupe map via `noteRecoveryDecided` before firing, so a liveness verdict landing in the same window is refused; the stamp is one-directional (tier-1 does not consult the map — that check has the longest field record and must not be suppressed by the new signal).

- [ ] **Step 4: Route the membership verdict**

In the monitor callback that calls `FetchMembershipVideos`, pass the verdict on:

```go
videos, verdict, err := s.ytService.FetchMembershipVideos(ctx, channelID)
if err == nil {
    switch verdict {
    case youtube.SessionAuthLoggedIn:
        s.cookieRefresh.ObserveLiveness("youtube", true)
    case youtube.SessionAuthLoggedOut:
        s.cookieRefresh.ObserveLiveness("youtube", false)
    // SessionAuthUnknown: learned nothing, do not move state.
    }
}
```

- [ ] **Step 5: Schedule the fallback probe — via callback inversion (B1)**

**Revision 1 said to put this inside `RefreshService`'s goroutine. That is impossible:** it would need `internal/cookies` → `internal/youtube`, and `internal/youtube/auth.go:8` already imports `internal/cookies`. Import cycle. (`cmd/moombox/services.go` has no refresh goroutine of its own either — `cookieRefresh` is built at `services.go:497` and started from `main.go:257`.)

Invert the dependency instead, matching the pattern `RefreshService` and `AutoCookieService` already use for exactly this (`VerifyYouTubeAuth`, `HasActiveJobs`, `ConfiguredBrowserOverride` are all injected callbacks):

Add to `RefreshService`:

```go
// FallbackLiveness is a channel-independent liveness probe, injected by
// cmd/moombox because internal/cookies cannot import internal/youtube
// (internal/youtube/auth.go already imports this package).
//
// Called at the END of doRefresh, and ONLY when no ObserveLiveness call has
// arrived within livenessRefireWindow — a normally-configured install gets
// its liveness from the membership probe for free and must not pay for a
// second request. conclusive == false means the probe learned nothing
// (consent wall, transport error) and MUST NOT move any state.
FallbackLiveness func(ctx context.Context) (loggedIn, conclusive bool)
```

Wire it in `cmd/moombox/services.go`, next to the existing `cookieRefresh` callbacks (~`:515`):

```go
	cookieRefresh.FallbackLiveness = func(ctx context.Context) (bool, bool) {
		verdict, err := ytService.ProbeAccountLiveness(ctx)
		if err != nil || verdict == youtube.SessionAuthUnknown {
			return false, false
		}
		return verdict == youtube.SessionAuthLoggedIn, true
	}
```

No new goroutine, so no new `defer recover()` obligation — it runs on `doRefresh`'s existing one.

**Write the consumer — revision 2 declared the field and wired the producer but never said what `doRefresh` does with it.** At the tail of `doRefresh`, after the existing callback section:

```go
	// Tier 2 fallback: only when the membership probe has told us nothing
	// recently. An install with channels gets liveness for free and must not
	// pay for a second request every cycle.
	if rs.FallbackLiveness != nil && !rs.livenessObservedRecently("youtube") {
		if loggedIn, conclusive := rs.FallbackLiveness(ctx); conclusive {
			rs.ObserveLiveness("youtube", loggedIn)
		}
	}
```

`livenessObservedRecently` reads `lastLivenessObserved` under `rs.mu` against the freshness window from Step 3.

**Also skip the fallback on the `CheckNow` path.** `POST /api/cookies/recheck` runs `doRefresh` synchronously on the HTTP handler (`routes/cookies.go:22`); adding up to 20 s of `FetchBody` on top of the existing 15 s guide check makes that button noticeably slower. `WriteTimeout` is 0 (`server.go:337`) so nothing 500s, but it is a bad interaction.

**Test the skip condition, not just the probe:**

```go
// TestFallbackSkippedWhenMembershipIsFresh: an install with channels gets
// liveness free from the membership probe. Firing the fallback anyway is a
// wasted request on every cycle.
func TestFallbackSkippedWhenMembershipIsFresh(t *testing.T) {
	// doRefresh makes a real guide request otherwise — point it at a stub so
	// the suite stays offline-safe and fast.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"responseContext":{"mainAppWebResponseContext":{"loggedIn":true}}}`))
	}))
	defer srv.Close()
	orig := youtubeGuideRefreshURL
	youtubeGuideRefreshURL = srv.URL
	t.Cleanup(func() { youtubeGuideRefreshURL = orig })

	var called int
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	rs.FallbackLiveness = func(context.Context) (bool, bool) { called++; return true, true }
	rs.ObserveLiveness("youtube", true) // a fresh membership observation
	rs.doRefresh(context.Background())
	if called != 0 {
		t.Errorf("fallback fired %d times despite a fresh observation, want 0", called)
	}
}
```

This test only passes with the **two-map** split from Step 3: a `LoggedIn` observation must write `lastLivenessObserved` (so the skip fires) while leaving `lastRecoveryFired` untouched.

- [ ] **Step 6: Test and build**

Run: `go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/cookies/ internal/youtube/ cmd/moombox/
git commit -m "feat(cookies): route liveness verdicts into the auth health signal"
```

---

### Task 7: Ungate the auth-loss notification (G6, G5) — DONE (4866f3a, 9b11ac7, ffea847); G5 OVERTURNED

> **AS EXECUTED:** built as a **fourth path**, not a restructure — deleting the early return would have launched a headless browser the user explicitly disabled. When `!autoEnabled`: Warn (not Debug) + a notification titled **"Cookie Re-Authentication Required"** (NOT the "YouTube Authentication Lost" title Step 1 below names) whose copy claims only what the code knows — no attempt, no finding, no failure, and it does not claim cookies are present (the witnessed-transition arm never consults `cookiesPresent`). "Nothing will attempt to restore it **on its own**" is scoped because `POST /api/cookies/auto-refresh` is not gated on `AutoEnabled`. Both paths share `notifyAuthFailure`, so the 30-minute per-platform cooldown covers them jointly. `livenessRecoveryArmed` stays false — Task 7 is the `auto_enabled` gate, not the pilot arming.

The notification is inside a gate added four months earlier to guard a *browser launch*. Gate the attempt, not the message.

**Files:**
- Modify: `cmd/moombox/monitor_callbacks.go:224-274`, `cmd/moombox/main.go:254`

- [ ] **Step 1: Restructure the callback**

When `!autoEnabled`: skip `RefreshCookies` and fire a notification written for the manual case. Do **not** reuse the existing copy — "Automatic cookie refresh for %s failed" is a false statement when nothing was attempted. New title: `"YouTube Authentication Lost"`; body leads with replacing `cookies.txt` at `s.cookieFilePath()` and notes the browser login is host-machine-only. Keep `Event: "auth"` and the 30-minute per-platform cooldown.

- [x] ~~**Step 2: Remove the G5 gate**~~ — **OVERTURNED 2026-08-27. Do NOT re-attempt. The gate STAYS.**

The proposal was to drop `cfg.Cookies.AutoEnabled &&` at `cmd/moombox/main.go:254`, keeping `len(cfg.Cookies.Platforms) > 0`, so manual-cookie installs get cross-restart auth-loss detection. Three source-verified reasons it was wrong, all re-verified in review:

1. **The seeding is unnecessary for the case it exists for.** `shouldFireRecovery`'s `everConcluded == false` arm already returns `cookiesPresent` (fed from `jar.HasAnyYouTubeAuthCookie`, the loose predicate), so a manual install with present-but-dead cookies **already fires on the first conclusive check of every start**. The premise — that manual users lack cross-restart detection — was false. The only real gap was the `auto_enabled` gate downstream, which Task 7 Step 1 closes.
2. **Seeding costs a recurring false alarm.** `checkYouTubeAuth` (`refresh.go:1026-1028` at `cc51b81`) and `checkTwitchAuth` (`refresh.go:1462-1487` at `cc51b81`) both return `(false, nil)` when nothing is configured, and nothing *automatic* prunes `Cookies.Platforms`. A stale entry takes the witnessed-transition arm and fires **once per restart, forever** — now as an operator-visible `TypeError`.
3. **The proposed mitigation was a no-op.** Seeding `prevAuth = true` while leaving `everConcluded = false` cannot change any answer: with `everConcluded == false`, `shouldFireRecovery` returns `cookiesPresent` **without ever reading `prevAuth`** (`refresh.go:828-836` at `cc51b81`). `SetExpectedPlatforms`' `hasCheckedOnce = true` (`:343-344` at `cc51b81`) gates only `OnAuthRecovered` and behaves identically seeded or not on a first check.

The one case seeding would add is `everConcluded && prevAuth && !cookiesPresent` — *the cookie file was emptied while the process was down*. At this seam that is indistinguishable from *platform never configured*, so declining to fire is correct under **a false failure is worse than a missed one**. That is the pre-existing I6 design, not a gap introduced here.

The derivation is pinned in code at `cmd/moombox/main.go:252-278` (the comment block ends at `:275`; the gate itself is `:276-278`, re-verified at `37eb62e` — every arc since has carried it as a no-touch zone) and by an acceptance test in `internal/cookies/refresh_transitions_test.go` (`TestSeedingIsUnnecessaryForStartupDeadAuthAndFiresFalselyWithoutCookies`, mutation-checked three ways). **The N6 row in "Cross-cutting corrections" above is the warning that produced this reversal — it was correct.**

- [ ] **Step 3: Commit**

```bash
git add cmd/moombox/
git commit -m "fix(cmd): notify on auth loss even when automatic refresh is disabled"
```

---

## Arc 1 acceptance

**Do not wait for the 30-minute ticker.** `POST /api/cookies/recheck` calls `refreshSvc.CheckNow(req.Context())` (`internal/web/routes/cookies.go:22`), which runs `doRefresh` synchronously — a deterministic, instant trigger for the whole health path.

- [x] **A1. Dead-session detection fires — PASSED 2026-08-27.** Garbage SAPISID+LOGIN_INFO, `auto_enabled = false`, throwaway instance on port 7741. Log: `youtube auth lost, triggering recovery` → `auth lost and automatic cookie refresh is disabled — manual re-authentication required platform=youtube`; the shipped notification title is **"Cookie Re-Authentication Required"** (the draft's "YouTube Authentication Lost" title was not what Task 7 shipped). Delivery confirmed by the owner in the real Discord channel. Fired at STARTUP via the startup-dead-auth arm, seconds after launch.

- [x] **A2. The V2 case specifically — PASSED 2026-08-27 (the important one).** LOGIN_INFO absent, SAPISID present. Log showed `Cookies loaded hasAuth=false` (the strict predicate) and the check **still probed and fired**. Identical copy to A1 is correct: the code cannot distinguish garbage values from a missing LOGIN_INFO, so claiming a distinction would be an unearned assertion. Owner confirmed delivery.

- [x] **A3. No false alarm on a transient error — PASSED 2026-08-27.** Guide URLs pointed at RFC 5737 TEST-NET (`192.0.2.1:9`, guaranteed unroutable) via a minimal build variant, immediately reverted. No auth-loss line, no notification line, owner confirmed nothing received.

> **A1-A3 method notes for A4/A5 (from the run):** use a throwaway instance, never the owner's install — the single-instance mutex name is a hardcoded const, so a second instance in the same Windows session needs a one-line build variant, built and immediately reverted; redirect `LOCALAPPDATA` so the cross-session lock does not collide. **The notification sender does not log send outcomes** — delivery can only be confirmed at the receiving channel, never from Moombox's logs.

- [ ] **A4. A healthy session stays silent.** Restore working cookies. `POST /api/cookies/recheck`. Expect no notification, and YouTube green in both the TUI status bar and the dashboard badge.
  > **Source-established 2026-08-27 (arc review), still worth one live run.** The full chain was traced at `4021a74`: healthy → `shouldFireRecovery` cannot fire; a LoggedIn liveness observation is structurally silent (one Info line, first observation only); dashboard `found && authenticated` → green "YouTube: Authenticated" (app.js), TUI `YouTubeAuthenticated` → `CookieStatusOK`. **Two run caveats:** (1) with jobs parked in `COOKIES?`, the first authenticated check of a process fires `OnCredentialsChanged` and may send the TypeInfo "Parked Jobs Re-evaluated" notification — correct resume behaviour, not an alarm; run A4 with no parked jobs or expect it. (2) The green badge requires the platform in `activePlatforms`.

- [ ] **A5. N channels do not mean N alerts.** With several YouTube channels configured and membership discovery on, break the cookies and let one monitor cycle run. Expect exactly **one** notification (the `ObserveLiveness` dedupe from Task 6).
  > **Scope caveat established 2026-08-27 (arc review): while `livenessRecoveryArmed = false`, A5 cannot exercise the ObserveLiveness dedupe it names** — verdicts are recorded and logged but the OnRecoveryNeeded call is withheld, so the single notification comes from tier-1 (once per process per loss) and the run passes trivially for the wrong mechanism. Running it pre-arming still has value (it is the multi-channel shape A1/A2 never had: expect N "liveness observation loggedIn=false" log lines with exactly ONE `wouldFireRecovery=true`, and ONE notification), but **A5 must be RE-RUN at arming** — that is the first moment the dedupe actually stands between N verdicts and N alerts. The armed path is unit-pinned (`TestRecordLivenessDedupesAcrossChannels`, `TestTierOneRecoveryStampsTheLivenessDedupe`) and source-verified (serial per-cycle probes; first verdict stamps `lastRecoveryDecided`, the rest are refused; `withAuthFailureCooldown` caps on top), but it has never run end to end.

**Retest caveat:** `lastAuthFailNotify` (`monitor_callbacks.go:201-212`) suppresses a repeat for 30 minutes per platform. Restart the process between acceptance runs, or you will read a working fix as a broken one.

## Staged rollout — do not enable notifications on day one

> **STATUS 2026-08-27: the log-only pilot is LANDED** — `const livenessRecoveryArmed = false` confirmed at `ffea847`; the pilot window has not yet run against real traffic. **Arming checklist, accumulated from execution rulings:**
> 1. The first tier-2 observation lands one cadence (~30 min) after boot, and **there is no manual way to force one** — `CheckNow` and `Start`'s initial check both skip the fallback probe deliberately. If arming shows this matters, the fix is a ticker-goroutine helper, NOT the synchronous startup path (ruled).
> 2. ~~**Inconclusive results are invisible at the default log level.**~~ **FIXED at `ad7451e` (audit item B3).** The fallback path now logs `"liveness fallback probe learned nothing about this session"` — Info on first occurrence or change of what is known, Debug on repeats, deduped through the same `lastLivenessKnown` record a verdict uses, and gated on the platform being configured (`TestInconclusiveFallbackIsReportedOncePerRun`, `TestInconclusiveFallbackIsSilentWhenNothingIsConfigured`). The probe's own error is logged at Debug by the cmd closure. **Pilot reading rule as shipped:** a healthy install emits at least one Info `liveness observation` line per process; an install whose probes are systematically refused emits the learned-nothing Info line instead; membership-path fetch errors remain Debug. "All Unknown" is therefore now readable from a default-level log. **Re-read at Arc 5 (`37eb62e`): these rules hold UNCHANGED.** Both probes gate on `s.Auth.HasAnyAuthCookie()` — the YouTube jar's loose predicate — before any fetch (`account_probe.go:53`, `channel_membership.go:85`), so a Twitch-only install produced no tier-2 observation before Arc 5 and produces none after it; `liveness_fetch.go:83`'s `no session cookies` decline is structurally unreachable on that path (a non-empty YouTube jar always yields a non-empty YouTube header). The `progress-arc5.md` claim that a Twitch-only install "now declines with `no session cookies` before probing" is therefore a misreading — the only thing Arc 5 changed on the probe path is the WIRE CONTENT of a dual-platform install's Cookie header (Twitch rows no longer ride along). **Twitch has no tier-2 signal at all** (`ObserveLiveness` has two producers, both YouTube); its only early warning is `expiredTwitchAuth` on the startup line.
> 3. The two comments whose justifications expire on arming were corrected by the post-review fix round (`cc51b81`, 2026-08-27) — re-read them at arming time anyway; they are the decision documents for flipping the constant.
> 4. Run the pilot with `auto_enabled = false` as well, belt and braces (below).
> 5. ~~**(added by the 2026-08-27 arc review) Fix finding F1 before arming**~~ — **DONE, merged at `5850ec2`.** The tier-1 guide body parse now requires an explicit negative marker; an unrecognisable 200 body is an inconclusive error. **What it does NOT fix, and what the arming decision must account for:** F1 silences the false *fire* and the false *notification*, not the false *badge*. `AuthStatus.YouTubeError`/`TwitchError` have no reader anywhere, and `doRefresh` sets `YouTubeAuthenticated: ytAuth` — false on an inconclusive check — so an install behind an intercepting intermediary still RENDERS "cookies found, not authenticated" exactly as before. That is Follow-up 1 / the merged Arc 4+7's job — **DONE at `f2b4e30`:** `AuthStatus` carries a real tri-state (`verification` = `ok` / `unknown` / `failed`, additive), both UIs render an inconclusive check as the hedged "could not establish" (never red), and `TestCookieIndicatorReadsTheHandlersOwnPayload` feeds the JS the Go handler's real map. The false badge is closed; `YouTubeError`/`TwitchError` still have zero readers by design. **A second cost of the same narrowing:** an install permanently behind such an intermediary now gets no tier-1 YouTube signal at all. Accepted — the same trade the provenance guard already made, and silence beats a standing false alarm about healthy cookies.
> 5b. **Not a blocker, but the first thing to measure if the pilot reads "learned nothing" forever.** F1's authenticated-side shape is **inferred, not measured** — no signed-in session was available and no cookie file was read. The failure direction is safe (an unrecognised positive becomes inconclusive, never a false alarm). One consequence, correctly bounded: an inconclusive verdict also skips `processYouTubeSetCookies`, so Set-Cookie merging stops. That halt is **mechanically real but consequentially unproven, and inherited rather than introduced** — pre-change `authenticated=false` took the identical skip, and this plan's own `research-rotatecookies.md` establishes that only `*PSIDTS`/`SIDCC` rotate while YouTube auth rides `LOGIN_INFO` + `*APISID` (corroborated in-tree at `jar.go:347-348`, which excludes exactly those two names from the fingerprint *because* they rotate constantly). Do **not** state the aging-out consequence as fact at the arming decision. Settling measurement if it ever matters: log the cookie **NAMES** — never values — that `processYouTubeSetCookies` updates on one authenticated cycle.
> 6. **(added by the 2026-08-27 arc review) Re-run A5 armed** — see the caveat on A5 above; the disarmed run cannot exercise the dedupe.
> 7. **(added by the 2026-08-27 arc review) Decide the persistent-loss cadence.** Armed, a genuinely dead session re-fires once per `livenessRefireWindow` (30 min) indefinitely — a notification every 30 minutes forever on `auto_enabled = false`, and a headless-browser attempt every 30 minutes on `auto_enabled = true`. Tier-1 alone notifies once per process. Whether the periodic re-alarm (vs. once-per-process or a back-off) is wanted is an owner decision to make AT arming, not a code accident to discover after it.
> 8. **(added by the 2026-08-27 delegation-readiness review) Land Arc 6 before arming — DONE, merged `1a0f0ff`.** V8/X1 restart-required labels are on both UIs; the settled meaning is in the EXECUTION STATUS block. Armed, `auto_enabled` governs a 30-minute headless-browser re-fire loop — and V8 is that the flag silently needs a restart with neither UI saying so. That makes V8 the first wall an operator hits while acting on the very notifications arming turns on. S13/V7 (live re-read) govern the same blast radius. This is why Arc 6 was pulled ahead of Arc 5 in the re-derived sequence.
> 9. **(same review) Landing the merged Arc 4+7 before arming is a positive, not a precondition — DONE, merged `f2b4e30`.** The pilot window is now readable from both UIs' badges (tri-state), not only the log. The ordering trap it named — follow-up 3 (`internal/twitch/auth.go:73-74` at `37eb62e` interpolates up to 1 MB of response body into its error) "must land first" — **was NOT landed and did not need to be**: Arc 4+7 renders the tri-state VERDICTS and never the error STRINGS (`YouTubeError`/`TwitchError` still have zero readers), and `twitch.Auth.ValidateToken` has zero production callers. The constraint is re-pointed in the Arcs 2-9 section as a live precondition on whatever first renders raw check errors; the XS fix itself is homed in Arc 8.
> 10. **(added by the 2026-08-28 Arc 5 audit; re-read at `db1993a`) Arc 5 adds NO arming interaction on the probe path** — see item 2's re-read note; the pilot reading rules are unchanged. Three Arc 5 facts that DO bear on the armed re-fire loop: (a) a Twitch session that dies mid-job downgrades IRC to anonymous on the next reconnect (live getter, `d5099a5`); a session Twitch REFUSES latches that job anonymous with ONE Warn (`dccb5e5`/`db1993a`) and a repaired cookie does not re-authenticate it until the next job; a job that started with NO credentials and gains them re-authenticates on its next reconnect (the latch never armed); none of these emits a notification (carried to Arc 8 as the Twitch anonymous-downgrade notification); (b) the recovery path's automatic profile import writes a `cookies.txt` that BOTH jars are re-parsed from, so a YouTube recovery cannot lose Twitch rows through the jar (one file, domain-partitioned on load) — Arc 2's per-platform rollback remains the guard on the write side; (c) every long-lived consumer now reads the jar at use time, so an armed recovery that rewrites `cookies.txt` mid-job reaches running YouTube chat, Twitch IRC/VOD chat and media segment downloads on their next request — before Arc 5 it reached none of them until the next job.
> 11. **(added by the 2026-08-29 Arc 8 close; read before arming) Three Arc 8 changes bear on the pilot's reading rules.** (a) **`CheckNow` now single-flights with the ticker (S2, `refresh.go` `refreshInFlight`):** a `POST /api/cookies/recheck` or `R C` landing during a ticker pass RUNS NO PASS — it answers with the in-flight pass's status, one snapshot behind at worst, and the only evidence is a Debug line (`cookie refresh skipped, another pass is already in flight`). The A4/A5 methodology must count passes from the log, never from clicks, and must expect an occasional click to produce no new `liveness observation` line; conversely a ticker tick that lands during a manual pass is DROPPED for a full interval, so a dashboard being clicked during the pilot lowers periodic coverage slightly. A caller that just rewrote `cookies.txt` (recovery-succeeded, `R F`) logs the skip at Info — `auth re-check after … was skipped` — because its badge then lags until the next tick; that line is expected, not a fault. (b) **First-run platform detection prefers the sidecar (H3/T7):** `cfg.Cookies.Platforms` — the list G5's `SetExpectedPlatforms` reads at `main.go:276` — is now seeded from `cookies.meta.json`'s verified `Platforms` when the config has none, and only otherwise from the LOOSE cookie-name predicates. On the config-reset-with-kept-data-dir path a sidecar that still records a platform the jar no longer holds seeds `prev*Auth=true` for it, and the first conclusive check fires "auth lost" for that platform — the same witnessed-transition behaviour a persisted config already produces on every restart, named here so a pilot log line of that shape is not read as a false verdict. (c) **Two new per-request surfaces render the reason strings and `lastError` (T12a/b):** the REST payload's `youtubeError`/`twitchError`, the web badge title's inconclusive arm, the `R C` line (reason + `Last cookie error:`), and the settings panel's `lastError` line. The push-driven TUI status bar renders NO reason by design, because `authStatusChanged` (`refresh.go:324-327`) excludes the strings from its change gate. Armed, a false LoggedOut therefore has a visible WHY beside it — the right direction — but a reason is server-authored prose with no lifecycle, so "reads stale on an always-on surface" is the failure to watch for if that gate is ever widened (carried to Arc 9).

Arc 1 makes Moombox send alerts it has never sent. If a verdict is systematically wrong, that is a notification every 30 minutes, forever, to a user whose cookies are fine — the exact failure `watch_page.go:246-254` was written to avoid, re-introduced at a higher level.

**Land Tasks 3-6 in log-only mode first.** In Task 6, have `ObserveLiveness` log the verdict at `Info`, and put the `OnRecoveryNeeded` call behind a constant `false`.

**"Just don't wire Task 7 yet" is NOT equivalent and must not be substituted.** `OnRecoveryNeeded` (`monitor_callbacks.go:223-273`) returns early only when `!autoEnabled`. On an `auto_enabled = true` install — anyone who used the setup wizard — `ObserveLiveness` firing it launches a headless browser (`RefreshCookies`, 2-minute timeout) and sends the existing "Cookie Auto-Refresh Failed"/"Ineffective" notifications, all without Task 7. The constant-`false` gate is the only thing that actually disarms the pilot. Run the pilot with `auto_enabled = false` as well, belt and braces.

Run against real traffic for a few days and read the logs:

- YouTube healthy → a steady stream of `LoggedIn`, no `LoggedOut`.
- Any `LoggedOut` while downloads are succeeding is a **false positive** — stop and diagnose before Task 7.
- All `Unknown` means Task 0's premise held in a test but not in production; re-open the page choice.

Only once the log is quiet and correct should Task 7 connect it to notifications. This costs a few days and removes the only way this arc can make things worse.

## Rollback

Every task ends in its own commit, so `git revert <sha>` backs out one task cleanly. Two exceptions:

- **Task 4** changes `FetchMembershipVideos`' signature across packages. It must be one atomic commit including every caller — reverting it alone is safe, but committing it half-done leaves the tree non-compiling.
- **Tasks 2 and 4 are coupled**: Task 4 calls `HasAnyYouTubeAuthCookie`. Reverting Task 2 without Task 4 breaks the build. Revert in reverse order.

**Correction (review round 3): "revert Task 7 alone" is false.** Earlier revisions claimed everything before Task 7 was inert plumbing. It is not:

- Tasks 1+2 change `AuthStatus.HasYouTubeCookies` for a half-cleared jar, which is visible in the TUI status bar and the dashboard badge (all four consumers listed in M2 above).
- Task 6's `ObserveLiveness` fires `OnRecoveryNeeded` directly, so on an `auto_enabled = true` install it launches browsers and sends notifications with Task 7 unreverted.
- Task 4's gate broadening changes monitor traffic.

**Back out in reverse order: 7 → 6 → 5 → 4 → 3 → 2b → 2 → 1b → 1** (as-executed order; 1b and 2b were added in execution — see the task→commit map at the top; reverting individual tasks means reverting their listed commits in reverse commit order).

Coupling beyond the Task 2↔4 pair already noted: **Task 6 depends on Task 4** (the three-value `FetchMembershipVideos`) and **on Task 5** (`ytService.ProbeAccountLiveness` in the wiring). Reverting 4 or 5 while 6 is still in place breaks the build. **Task 2b depends on Task 2's predicates** (`HasAnyYouTubeAuthCookie` et al.) and on Task 1's error classes; **Task 5's `fetchLivenessPage` is consumed by Task 4's membership probe** as shipped (the shared-guard refactor), so reverting 5 alone also touches 4's fetch path.

---

## Follow-ups carried out of Arcs 0-1 — NOW OWNED (dispositions below; items unchanged for provenance)

Accumulated in the ledgers during execution; recorded here so the plan, not the ledgers, is the one place a future arc has to look. **The four post-review items on `internal/cookies/refresh.go` and `cmd/moombox/monitor_callbacks.go` were fixed by the round that landed as `9c715d8` + `cc51b81` (2026-08-27) and are deliberately not repeated here.**

> **Dispositions, verified at `1f90bd1` by the 2026-08-27 delegation-readiness review** (derivations in `fable-arcs2-9-review.md` §Global 7). The numbered items below are kept verbatim for provenance; these rulings supersede them where they disagree.
>
> | # | Verdict at `1f90bd1` | Home |
> |---|---|---|
> | 1 | ~~**STILL REAL** — zero UI consumers~~ **CLOSED at `f2b4e30`** (Arc 4+7 Task 3, `835be7f`/`f6aa36f`): `AuthStatus` grew a tri-state, both badges render "could not establish" for an inconclusive check, the web recheck toast followed in `a6d4a6d`. The error STRINGS are still rendered nowhere — by ruling, not by omission (a tri-state, not error text). | none needed |
> | 2 | **FIXED** at `ad7451e` | none needed |
> | 3 | **STILL UNFIXED at `37eb62e`** — `internal/twitch/auth.go:73-74` interpolates up to 1 MB of body into the error. **The stated hazard is NOT live:** `twitch.Auth.ValidateToken` has ZERO production callers (grep at `37eb62e`; the tier-1 Twitch check is `refresh.go`'s `checkTwitchAuth`, status-code-only since Task 1b), so nothing can put that body into `AuthStatus.TwitchError` today. Arc 4+7 did not land it and did not need to. | **Arc 8**, XS — mirror `checkTwitchAuth`'s status-code-only errors. **Live precondition** on whatever first calls `ValidateToken` in production or first renders raw check errors (Arc 5's proposed Twitch live gate calls it from a TEST, which is fine). |
> | 4 | **STILL REAL at `37eb62e`** — two sites, now `autocookies.go:2301` and `:2306` (the `(network?)` hedge in the rollback message switch). Arc 4+7 did NOT take it: its Task 2 changed the setup FINISH response and its Task 3 `AuthStatus`, neither of which is this switch. | **Arc 8**, XS; `platformAuth.attempted` is the tool it needs. |
> | 5 | **RESIDUAL, NARROW** — invariant and both pins exist (`refresh.go:75-101` at `37eb62e`); the only production constructor passes 0 → the 30-minute default (`services.go:661` at `37eb62e`), and no config knob reaches the interval, so it is **unreachable in production today** | **Arc 8**, XS clamp-or-doc on `NewRefreshService`. Not worth its own task. |
> | 6 | **STILL REAL at `37eb62e`** — `services.go:328-336` (`jar.HasYouTubeAuthCookies()` at `:330`, `jar.HasTwitchAuthCookies()` at `:333`); Arc 5 rewrote the log line directly above it (`:321-326`) and left the detect block strict | **Arc 8, merged with H3's remedy — same lines, one ~15-line change closes both.** |
> | 7(a),(b) | open | no arc — a standalone investigation when a Firefox-family anomaly next appears; the drain gate runs when LibreWolf/Zen/non-Windows environments exist |
> | 7(c) | Q5 **PARTIALLY FIRED in Arc 4+7**: the setup abort report renders `lastError` on one branch, ruled safe there by setup/refresh mutual exclusion (`StartSetup` clears it; `RefreshCookiesDetailed` declines while a setup is live). The always-on cookies panel still renders no `lastError`, gated on the Q5 writer audit — **which has no owner**. The obvious lifecycle fix (`s.lastError = nil` in `cleanup()`) is CONFIRMED WRONG (three `setError` calls are each followed by `cleanup()`) and must never be retried. | **Arc 8** (writer audit, then render-or-don't), listed under the Arc 4+7 residuals |
> | 7(d) | open — `TestScreenshotIsClearedBeforeEachLaunch` (`autocookies_acted_test.go:449`) still sleeps the hard-coded `firefoxLaunchSpacing` (5 s, `autocookies_firefox.go:45`); no seam at `37eb62e` | **Arc 8**, XS (`firefoxLaunchSpacing` seam) |
> | 7(e) | **CLOSED** at `ee1ce5f`/`bd6bc84` — `navigateAllPlatforms` folds per-platform navigation errors into `browserActed`. One residual, recorded not fixed: `cdpNavigateAndWait` returns nil on budget exhaustion (`autocookies_chromium.go:514-516` at `37eb62e`), so connect-then-stall still counts as navigated (missed-alarm direction) | residual → **Arc 8**'s Chromium cleanup task, as a decide-don't-drift item |
> | 7(f) | unmeasured; no arc owns Linux | **precondition of Arc 9's X3 rewrite** and of any Docker-facing release note. One run of the drained-launch subtest on any Linux box. |
> | 8 | not an arc item | **standing controller practice** — see the namespace rule at the end of the Arcs 2-9 section |
>
> **Arc 8 close (2026-08-29):** items 3, 4, 5, 6, 7(c), 7(d) and 7(e)'s residual are **CLOSED** on `cookie-arc8-perf-cleanup` (see the Arc 8 paragraph for what shipped against each); 7(a), 7(b), 7(f) and 8 are unchanged. Item 1's "the error STRINGS are still rendered nowhere" is superseded in part: Arc 8 T12a gave `YouTubeError`/`TwitchError` readers on the PER-REQUEST paths only (REST payload, web badge title, `R C` line); the push-driven TUI bar still renders none, by design, and `LastCheck` is still unread.

1. ~~**`AuthStatus.YouTubeError` / `TwitchError` are rendered NOWHERE**~~ — **CLOSED at `f2b4e30`** (see the dispositions table): the inconclusive state is rendered as a tri-state verdict on every surface, which is what the constraint required; the strings themselves stay unread by ruling. Original text: no frontend, no API field consumer, no TUI reader. Three consecutive tasks landed in this hole; the Task 2 reviewer called it "the arc's biggest open surface: inconclusive that nobody can read is the second thing the constraints forbid." Promote to its own task (natural home: Arc 7 UI parity).
2. ~~**Dual-platform loss in one pass mis-reports the second platform**~~ — **FIXED at `ad7451e` (audit item B2).** `runCookieRecovery`'s Unknown branch splits on `RefreshResult.Ran`: a declined pass logs one Info line, sends nothing, and stamps no cooldown, so the second platform's next real attempt delivers its accurate "Cookie Auto-Refresh Failed". `TestDeclinedRecoveryDoesNotSpendTheCooldown` drives the exact two-platform sequence against the REAL cooldown wrapper (`withAuthFailureCooldown`, extracted for that test).
3. **`internal/twitch/auth.go:74` interpolates the raw response body into its error** — the same credential-exposure shape Task 1b's error was written to avoid (a WAF reflecting the `Authorization: OAuth <token>` header would render a credential wherever that error surfaces). Recorded in Task 1b as "worth its own task; do NOT let it be forgotten." *(Still unfixed at `37eb62e`, `:73-74`; the function has no production caller, so the surface is latent — Arc 8.)*
4. **FinishSetup's residual "(network?)" hedge** — the rollback-message switch still hedges one case; whichever task next owns that switch should split it using `platformAuth.attempted` (now available). Task 2b deferral, ruled acceptable because a hedge is not an assertion.
5. **`livenessFreshWindow`'s upper bound is pinned only against the `defaultRefreshInterval` constant** — a `RefreshService` constructed with an interval under ~25 min self-suppresses the fallback on alternate cycles in the field with every test green (Task 6, out of scope there).
6. **First-run auto-detect still uses the strict predicates** (`cmd/moombox/services.go:288-294` as of `ffea847`; `:328-336` at `37eb62e`, unchanged by Arcs 2-5): on an install with empty `Platforms` AND `ActivePlatforms`, a half-cleared jar registers no platform, so both UIs hide the YouTube indicator entirely (narrow — `GetActivePlatforms` falls back to enabled channels; Task 2 review caveat).
7. **Arc 0 open items:** (a) `--screenshot` silently produces no file against freshly-created profiles on both Waterfox and Firefox, no Job Object involved, cause unknown (only clue: Waterfox's `RenderCompositorSWGL` error); production's `browserRendered` still keys off it and discriminated correctly on real setup-created profiles — worth its own investigation; (b) LibreWolf, Zen, and all non-Windows platforms remain unverified by the live drain gate; (c) Q5: if any UI ever renders `lastError`, the stale-error retention risk becomes Important — re-open it then (and Q4: nothing renders it today, do not assume otherwise); (d) `TestScreenshotIsClearedBeforeEachLaunch` costs 5.07 s of the package's test time — fix is a `firefoxLaunchSpacing` injection seam mirroring `detectBrowser`; (e) **Chromium F2 half-gap (2026-08-27 audit):** `cdpNavigate`'s error is still discarded (`autocookies_chromium.go:256`), so a Chromium refresh whose platform navigations all fail while CDP stays answerable sets `browserActed = err == nil` → true, stamps `lastRefresh`/`SaveMeta` and logs "cookie refresh succeeded" off recycled cookies — and the comment beside it over-claims "proof of the same order as the Firefox screenshot". Fold the per-platform navigation errors into `browserActed` (retires half of finding A3 — Arc 4's A3 entry does NOT name this half, so it lives here until owned); (f) **Linux launcher-handoff premise unverified (2026-08-27 audit):** the "not acted ~always on Linux" claims in `RefreshResult.Renewed`'s doc, `job_linux.go`, `routes/cookies.go:72-74` and `app.js` rest on H3's assumption that the Firefox launcher handoff exists on Linux; Mozilla ships the launcher process on Windows only, so native Linux plausibly reports acted=true normally. Run `MOOMBOX_LIVE_BROWSER_REFRESH=1`'s drained-launch subtest (not Windows-gated) on any Linux box once, then rewrite those comments to the measured fact — whichever way it lands.
8. **Finding-label collision:** Arc 0/1 review findings reuse H1/H2-style labels that collide with the observation file's code-health finding IDs. An R1-R4 renumbering was offered, never applied. Any future arc citing "H2" must say which namespace it means.

## Arcs 2-9 — scope, re-derived at `1f90bd1` (2026-08-27 delegation-readiness review)

**Task-level detail lives in `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/fable-arcs2-9-review.md`** — a per-finding roll-call with a `file:line` read at `1f90bd1` behind every verdict, refreshed citations, per-arc task decompositions with their traps, sizing, and blocking premises. This section carries only what changes dispatch order. **Do not dispatch an arc from the paragraphs below alone** — the paragraphs are scope; the report is the brief.

**Three structural changes to the roadmap as originally written:**

1. **Arc 4 is half-done, and Arcs 4 and 7 MERGE into one plan** ("three-state, and what the UIs say"). H4's dangerous half — the `PersistPlatforms(false,false)` poisoning and the lenient-boolean flip — was closed by Arc 1's Task 2b (`autocookies.go:531-563`), and A3's cdpNavigate half by the follow-ups round. What remains of Arc 4 is API/UI surfacing that edits the same `AuthStatus` struct and the same three consumers as Arc 7's V5/V6. Dispatching Arc 4 from its old one-paragraph scope would re-implement finished work. **Pull S17 out of Arc 3 into the merged plan** — it reads the same setup response as the H4 remainder.
2. **Dropped from scope, verified fixed:** **S4** (closed in Arc 0) and the **cdpNavigate half of A3** (closed by the follow-ups round). Arc 2 shrinks to S7, S9, S10, X2-slice. The report carries a documented trap against "completing" either.
3. **Gated, not dropped:** **H1** (Arc 8) is the same two functions the in-flight F1 round is rewriting. Re-derive its residual scope after F1 merges.

**Recommended sequence:** **Arc 2 → Arc 3 → Arc 4+7 (merged) → Arc 6 → Arc 5 → Arc 8 → arm the pilot → Arc 9.**

- Arc 2 first: S7/S9 are the highest-severity still-real findings — silent destruction of working credentials.
- **Arc 6 moves ahead of Arc 5 because it is an arming precondition, not mid-list cleanup.** Once the pilot arms, `auto_enabled` governs a 30-minute headless-browser re-fire loop, and V8 — the flag silently needs a restart and neither UI says so — is the first thing an operator hits acting on the new notifications. Arc 5's tier-2 is calendar-gated on a members-only capture anyway. *(History: tier 2 was later REJECTED outright — see the Arc 5 paragraph — so the ordering rests on V8 alone. The order executed as written.)*
- Arc 8 is fully parallelisable and may interleave with Arcs 5/6, **except** its `internal/cookies/refresh.go` items (H1, S2, S3), which follow the F1 merge.
- Arc 9 stays last: the spec is rewritten once, after the code settles.

~~**Dispatch bar — nothing citing `internal/cookies/refresh.go` goes out until F1 merges.**~~ **LIFTED — F1 merged at `5850ec2`, Arc 2 at `f394cf9`.**

**Citations: assume stale, always.** The Fable Arcs 2-9 report read `refresh.go` at `1f90bd1`; F1 moved it ~+530/-90, Arc 2 moved it again (~+336/-56) along with `autocookies.go` (~+60) and `cmd/moombox/monitor_callbacks.go`, Arc 4+7 and Arc 6 moved `autocookies.go` by hundreds more, and **Arc 5 rewrote `jar.go` (+419) and left `refresh.go`/`autocookies.go` untouched** (`autocookies_merge.go:47-70` is its only edit in that family). Anchors re-derived at **`37eb62e`** (mid-Arc 5; `refresh.go` and `autocookies.go` are identical to `1a0f0ff` there AND at the Arc 5 head `db1993a` — the five later commits touch `jar.go`, `internal/twitch/`, `internal/engine/downloader.go` and `internal/worker/` only, so these anchors still hold):

| Anchor | At `37eb62e` |
|---|---|
| `processYouTubeSetCookies` head / sole call | `refresh.go:1750` / `:1742` |
| its substring pre-filter (B2's admission gap) | `refresh.go:1764-1767` |
| `updateCookieFile` head | `refresh.go:1918` |
| `resolveRowUpdate` (rule 2 at `:2143`) / `sameCookiePlatform` (`up := "google"` at `:2184`) | `refresh.go:2129` / `:2174` |
| `isGoogleOnlyAuthName` decl / its use | `refresh.go:2206` / `:2056` |
| `checkYouTubeAuth` / `checkAndRefreshYouTube` / `checkTwitchAuth` | `refresh.go:1603` / `:1671` / `:2214` |
| `Start` (sync initial `rs.refresh(ctx, false)` at `:562`) / `CheckNow` / `doRefresh` | `refresh.go:545` / `:612` / `:819` |
| `livenessRefireWindow` / `livenessFreshWindow` / `youtubeClientVersion` | `refresh.go:71` / `:101` / `:106` |
| S9 read sites (FinishSetup / RefreshCookies) | `autocookies.go:1023` / `:1867` |
| rollback gate (`platformsToRestore` call / decl) | `autocookies.go:1970` / `autocookies_profile.go:658` |
| `cleanup()` / `cleanupLocked()` | `autocookies.go:2731` / `:2757` |
| seams (`writeCookieFile` / `readCookieFile`) | `autocookies.go:2858` / `:2869` |
| `refreshPlatforms` / `resolvedBrowser` / `browserGatePolicy` block | `autocookies.go:441` / `:686` / `:709-770` |
| `ErrCookieFileUnreadable` | `internal/cookies/errors.go:155` |
| `automaticImportGuard` | `autocookies_profile.go:791` |
| `isEssentialCookie` / `deduplicateAndFormat` / `mergeCookieFiles` / `rowExpired` | `autocookies_merge.go:47` / `:88` / `:142` / `:217` |

**Every arc's first task re-derives the citations for the files it touches.** Three separate tasks have found them off by 30-90 lines mid-execution.

**Credential-order constraint — RE-POINTED (satisfied-for-now, not satisfied-forever):** follow-up 3 — `internal/twitch/auth.go:73-74` (at `37eb62e`) interpolates up to 1 MB of response body into its error — was written as "must land before the merged Arc 4+7 renders `TwitchError` anywhere". Arc 4+7 satisfied the constraint by **not rendering the error strings at all** (the tri-state carries verdicts; `YouTubeError`/`TwitchError` still have zero readers), and the fix itself never landed. It is still unfixed at `37eb62e` and its function has **zero production callers**, so the hazard is latent. The constraint transfers as a **live precondition** to whichever task first (a) renders `TwitchError`/`YouTubeError` or `lastError` from raw check errors, or (b) calls `twitch.Auth.ValidateToken` from production code. The XS fix is homed in **Arc 8**.

**Arc 2 — Write-path data loss — DONE, MERGED (`main` @ `f394cf9`).** S7 + S10 + S9 + the X2 slice, plus three defects review found and one the arc created for itself. Ledger: `progress-arc2.md`. Arc-close review: `fable-arc2-review.md` — it enumerated **every** writer of `cookies.txt` by grep (four sites, two owners) and confirmed each now merges, aborts, or rolls back.

Three corrections to what this paragraph used to say, all verified at source:

- **The deletion semantics are CPython's, not yt-dlp's.** `_cookie_from_cookie_tuple` does not exist in `references/yt-dlp/`; `YoutubeDLCookieJar` merely inherits `http.cookiejar`, and its own `clear()` override is pure `KeyError` suppression. CPython converts `Max-Age` to an absolute expiry, then calls `self.clear(domain, path, name)` and returns `None`. **The spec (`reports/cookies-observations.md`, S7) still carries the old attribution** — a future task citing "yt-dlp does X" will grep for code that is not there.
- **"Delete the row" shipped for the two unambiguous forms only** (a past `Expires`, `Max-Age<=0`). The bare `NAME=` form with no expiry attribute is **refused**, not deleted: this code runs only on a response YouTube has just confirmed authenticated, so a blanked credential there is self-contradictory, and the package cannot round-trip an empty-valued row anyway (`CookieJar.Load` TrimSpaces the line, the trailing tab vanishes, the row reads as 6 fields and is skipped). A forced divergence, not a chosen one.
- **The arming-interaction instruction is MOOT, and here is why so the loop closes.** It asked S7's brief to trace the tier-1 gates for an all-rows-deleted jar; the brief carried only the mid-process half and nothing traced the post-restart gates. It does not matter: pre-fix blanked rows were *already* invisible to `jar.Load`, so the post-restart jar is identical pre- and post-fix — both empty, `HasAnyYouTubeAuthCookie` false either way. The noisy→silent delta this feared never existed.

**Beyond the four findings, the arc also shipped:** a refusal for the blank-`Set-Cookie` form above; `Domain=` lowercased at the parse site (it was dot-normalized but not case-normalized while scope matching used `EqualFold`, so two map keys matched one row nondeterministically — one branch silently downgraded `secure` TRUE→FALSE); refreshes confined to one platform (a YouTube refresh could overwrite a `.twitch.tv` row); and `ErrCookieFileUnreadable`, because the new S9 abort's error landed in a notification telling the operator to **replace** the very file the abort had just refused to touch. *(These are review-finding labels from `review-arc2-task1.md`, deliberately not the spec's A1/A2 — Arc 3 has its own A1.)*

**Arc 3 — Setup lifecycle — DONE, MERGED (`main` @ `2be52a5`).** S1, S18, A1, S16, S5 — six commits, plus three defects the arc created for itself and closed before shipping. Ledger `progress-arc3.md`; arc-close review `fable-arc3-review.md`. S17 went to Arc 4+7 as planned. `RefreshDeclinedCauses` needed **no** change: the new gate is a strict subset of the old one (proved at source, not assumed), and all three pins are untouched.

The three self-inflicted defects are the arc's real lesson, and they share one shape — **a task granted teeth to an existing action, and every existing caller of that action was then unreviewed:**

- The reap keyed on `cmd.Wait()` returning. **Firefox hands off and exits in ~170 ms** — the same measured fact Arc 0 was built on — so a launcher exiting is not a browser closing. It now keys on a Job Object reporting zero live processes, with closed / empty / unqueryable held apart.
- A Job Object whose `assign` **failed** was still stored, and a live handle tracking nothing reads as an *empty* job — licensing a premature release (a lost finish, **not** a killed browser; that overstatement was made twice by the controller and corrected twice).
- S5 gave every *cancel* teeth on Firefox, while S16's beacon fired one with **zero grace** the instant the dashboard tab unloaded — the tab the flow tells the user to leave in order to sign in. A tab unload is not consent. The beacon became `POST /api/cookies/auto-setup/abandon`, which releases the slot **only where releasing cannot kill** — reusing `setupBrowserGone`'s existing `known`, no new mechanism. It is redundant on Windows (the reap owns it) and load-bearing on Linux/Docker (the only release there). The TUI countdown, aggressive at 60 s when it was the sole backstop, is now `cookieSetupCountdownSeconds = 300` (`internal/tui/setup_wizard.go:46` at `37eb62e`) because the reap is the backstop and the countdown now only bounds a human.

> **NAMED RESIDUAL — A1 IS NOT FIXED ON LINUX OR IN DOCKER. This needs an owner; it is not closed by anything in Arcs 4-9 as written.**
>
> `queryable()` returns `false` unconditionally on non-Windows (`job_linux.go:36`, `job_other.go:29`; only `job_windows.go:175` returns a real answer). **The reap therefore never fires on Linux or in Docker on *any* path, Chromium included** — S5 does not change this, because the gap is the absence of the Job Object primitive itself, not of a call to it. On those targets the `abandon` beacon is the only release mechanism, so a TUI-only session or a crashed tab still wedges acquisition until restart. The Linux setup path relies on `PR_SET_PDEATHSIG`; giving Linux a real liveness answer is a **design question**, not a port of the Windows code. Ruled during Arc 3 and — the failure worth recording — recorded in code, tests, commit messages and the ledger, but not here, which is the only document a later arc reads.

**Arc 4+7 (combined) — Three-state, and what the UIs say — DONE, MERGED (`main` @ `f2b4e30`).** The remaining H4 surface, S12 (which **is** follow-up 1), S15, S17, V4, V5, V6, V10, S14 — **ten findings**, six tasks plus a Task 1b found in review. Ledger `progress-arc47.md`; arc-close review `fable-arc47-review.md`, which traced **all twelve surfaces that render a credential verdict** and found none that can still say "failed" when the truth is "could not check". *(The paragraph this replaces was still written as pending work two merges after the arc closed, and the named-residuals block the arc review asked for had never been added — the exact "recorded everywhere but here" failure Arc 3 named.)*

**Delivered:** `AuthStatus` carries a real tri-state (`verification` = `ok` / `unknown` / `failed`, additive — `authenticated` keeps its meaning so an older frontend degrades identically); the setup finish response stopped guessing (S15/S17 — the abort report is built from server facts and its JS is executed under goja, `cookies_setup_utilsvm_test.go`); a parked `COOKIES?` job reddens its **own** platform's badge (V4; empty `Platform` escalates YouTube, matching three sibling sites); Twitch's `CookiesOnly` arm is live via the loose predicate (V5); the web re-login warning and badge are ungated on `auto_enabled` with "do not reintroduce" pins on both halves (V6); `Configured` deleted rather than recomputed (V10); a 429 on validate no longer aborts the settings save (S14); Chromium judges an empty profile by what Moombox would keep (Task 1b, which also fixed a between-tier gate that dropped `SAPISID` with a nil error); the web recheck toast speaks the same three-way vocabulary as `R C`; and `ErrAuthCheckNotAttempted` maps to `RefreshFailed`-class rendering via a sentinel split inside `verdictFromCheck` (`refresh.go:236` at `37eb62e`) — never tier promotion.

**Standing rulings later arcs must not relitigate:** the visibility/action split (hiding a TRUE alarm from `auto_enabled = false` users is rejected three surfaces deep — notification, web warning+badge, TUI); tri-state, not error strings (`YouTubeError`/`TwitchError`/`LastCheck` stay unread by design — a standing invitation to render stale strings, so delete or keep documented, Arc 8/9 — *superseded in part at the Arc 8 close: the two error strings are read on per-request paths only, never on a push-driven surface, because `authStatusChanged` excludes them; `LastCheck` is still unread — see the Arc 8 carry-over block*); `CookieStatusUnknown` sits at `tierEssential` on the two-route argument (`RefreshFailed → CookiesOnly` and `StatusCookies → cookiesRejected` both survive every tier, so promotion closes no hole); `s.lastError = nil` in `cleanup()` is CONFIRMED WRONG and must never be retried; `ErrNoCookiesInProfile` must NEVER be redefined as the auth-cookie predicate (it would discard `CONSENT`, which two other paths tell operators to supply).

> **NAMED RESIDUALS — each with a home (from `fable-arc47-review.md` §4/§5, verified at `37eb62e`). Arc 8 close (2026-08-29): every item homed in Arc 8 below is CLOSED there — see the Arc 8 paragraph, item 10; the two Arc 9 items and the retired V6 item are unchanged.**
> - **Web parked-badge parity — declined, knowingly divergent.** No jobs→`updateStatusBar` path exists (four call sites in `app.js`, none a job event, no fallback polling); the dashboard lists parked jobs with their remedy but its aggregate badge stays silent. Needs its own XS-S task → **Arc 8**.
> - **CDP-failure cause on the setup wire** — blocked-ladder / all-tiers-failed still flatten to the default 500 "failed to finish setup"; Task 1b's "until Task 2 adds the cause" promise was picked up by nobody. Sentinel-or-`cause`-field decision → **Arc 8**, XS-S.
> - **Refresh-side "rows came back, none were credentials" diagnosis** — unbuilt; a new flag fed from the loose predicates beside `fetchedRows`, consumed as one new `case` in the refresh switch. Constraint travels verbatim: **a new flag, never a sentinel redefinition** → **Arc 8**.
> - **`lastError` in the always-on cookies panel** — still unrendered, gated on the Q5 writer audit (refresh/import writers `setError`; `StartSetup` clears; `cleanup()` must not). Audit has no owner → **Arc 8**. Q5 partially fired here (the abort report renders `lastError` on one branch, safe by setup/refresh mutual exclusion).
> - **`FlagManualRelogin` (`autocookies.go:773` at `37eb62e`) has zero callers** — delete, or name the still-unbuilt re-auth ingest path (the Docker paste/upload endpoint) as its caller → **Arc 8** or the ingest brief.
> - **`/api/cookies/auto-status` no-service branch omits `availableBrowsers`** — unreachable in production (`services.go` constructs the service unconditionally), XS → **Arc 8** pass over that file.
> - **TUI feedback colorizer** renders `R C`'s conclusive "not authenticated" yellow and `R F`'s red — same fact, two severities, both truthful. One list-membership change → **Arc 8**.
> - **Two persistence policies for `active_platforms`**: both first-run wizards mark a platform configured on *accepted* (hedged verdicts included) while the backend's `PersistPlatforms` is gated on `verifyOK` only. Pre-existing, consistent between UIs; the same monotonic config union H4 worried about → **Arc 9** (document) unless an owner wants it unified.
> - **`segments.js` / `trimmer.js` are CRLF** despite `*.js text eol=lf`; the next edit to either produces a whole-file diff unless normalised first → **Arc 9**, mechanical.
> - **The V6 click target launches the browser wizard for manual users** — RETIRED, not deferred (it is a working remedy: setup routes are ungated, write to the same `cookies.txt`, and merge). Revisit only when the re-auth ingest path exists, in that brief.
> - **Anchors** at `37eb62e`: `GetStatus:642`, `StartSetup:785`, `FinishSetup:924`, `FinishSetupDetailed:931`, `CancelSetup:1234`, `AbandonSetup:1290`, `cleanup:2731`/`cleanupLocked:2757`, `trackedSetupJob:2800`.

**Arc 6 — Untangling `auto_enabled` — DONE, MERGED (`main` @ `1a0f0ff`).** G1, G2, S13, V7, V8, X1, X5. Ledger `progress-arc6.md`; arc-close review `fable-arc6-review.md`. **The settled meaning is in the EXECUTION STATUS block at the top of this file — read it there, not here.**

**Most of this arc was findings turning out to be misdiagnosed rather than unfixed**, and that is the part worth carrying:

- **S13** ("three consumers, three policies") did not need unifying. The consumers belong to **different subsystems**, correctly gated differently. When a finding says "N consumers, N policies", first ask which subsystem each belongs to.
- **V7** ("`R F` bypasses the live setting") described *correct* behaviour. The defect was one line up: a startup-only read left a nil callback, which **deleted the chord** from dispatch, the action menu and help — so on a disabled install the manual remedy did not exist in the TUI at all.
- **G1**'s implied remedy — ungating the periodic loop — was **rejected**. A mounted container profile is *unchanging*, so a timer would re-read identical bytes forever. G1's real fix is the on-demand ladder.
- **X5**'s commented-out flag was **correct**; only its explanation was missing.

**Delivered:** `R F` / Web shift+click / a new Settings-page twin (shift+click does not exist on a touch device, and the Docker workflow had no trigger there) are one three-rung ladder — browser, else profile import, else the in-process refresh, saying so. One `automaticImportGuard` owns every automatic browser-free import. The boot seed runs once regardless of the flag when there is no `cookies.txt`. G2's silent skip is now logged.

> **Residuals, each with a home:** `loadAutoCookieStatus` fires once per dashboard load (recorded, code unchanged — **and it calls `/auto-status` → `GetStatus()` → the UNCACHED `DetectBrowsers()`, so Arc 8's H5 now has one more caller than the review counted, on the most frequent path**); a browserless host with the flag **on** still imports on a timer via the resolver returning nil for reasons the flag has no say over (now guarded by the import rule, so it stands down when there is anything to lose); **U3 — flag-on browserless with a permanently-failing profile import warns `periodic auto-cookie refresh failed` every tick forever, no backoff** (misconfiguration-reachable only; named so it is not rediscovered); two assertions coupled to exact log strings; the exported/unexported entry-point split. New sentinels the later arcs must not re-wrap: `ErrProfileDirUnreadable` (the F1 fix) and the deliberately-narrow two-member `IsNoBrowserProfile` (rung 3 = "no profile at all, pre-work") — re-wrapping any profile-import sentinel through `ErrProfileNotFound`/`ErrNoBrowserFound` re-opens F1. **U1 was ruled, not left:** the recovery path's automatic import bypasses `automaticImportGuard` BY DESIGN (it fires only on a conclusive failure), the guard's doc says four callers and names the method-value one. None can destroy credentials — verified by execution at the arc close.

**Arc 5 — Transport and identity — DONE, MERGED to `main` as `5eb5266` (2026-08-29, `--no-ff`, branch deleted); the nine commits below came from `cookie-arc5-transport-identity` @ `db1993a` (nine commits; arc-close review 2026-08-29: delivered with named gaps).** *(Ledger: `progress-arc5.md` — the authority for every ruling below.)* Task → commit: T1 `0c40e4d` · T2 `72f4373` · T4+5 `d5099a5` · T6 `37eb62e` · T7 `7e742b2` · T8 `387d24a` · T9 `2727c29` · fix rounds `dccb5e5` + `db1993a`. **The design changed mid-arc on an owner correction, toward less machinery.** The original plan resolved cross-platform name collisions with a ranking; the owner pointed out the collision exists only because both platforms share one flat name-keyed map, and that separate jars make the question disappear. They were right, and this was the `check-the-simple-model` failure again. **Per finding: T2 CLOSED · T3 cross-platform half CLOSED, within-platform remainder still Deferred · T1 tier 1 CLOSED, tier 2 REJECTED by ruling · V1 CLOSED as filed (rotation staleness on every long-lived consumer; the restart half was retired by owner ruling, not deferred).** **As executed:**
- **T2 storage shape + T3-now — `0c40e4d`.** `cookieEntry{value, domain, expiry}` replaced the parallel maps; expiry captured and **never filtered** in `Load` (the autocookies loss-detection depends on jar-vs-merge *disagreement*); per-platform `ExpiredAuthCookiesFor`/`AuthCookieHorizonFor` and `expiredYouTubeAuth`/`expiredTwitchAuth` on the `services.go` startup line (both always printed — Twitch has no other early warning). The cross-platform comparator this commit added was **deleted one commit later by design**; what survives is the within-jar total order (youtube > google, fewer labels, dot-prefixed, lexical), pinned by exhaustive permutation tests on the whole partition snapshot.
- **T1 (all tiers) + T3's real fix — `72f4373`, the partition.** Two in-memory jars (`youtube`, `twitch`); **`cookies.txt` stays ONE file, no write path changed.** Domain-first admission replaced the unguarded `essentialYouTubeCookies[name]` clause, so a `.twitch.tv` row named `SID`/`SAPISID` is never admitted rather than admitted-and-outranked. Only youtube-over-google survives, *inside* the YouTube jar, where Google auth cookies legitimately live on both domains. `GetCookieHeader()` → YouTube jar; **`GetCookie(name)` → Twitch jar** (its sole consumer repo-wide is `twitch/auth.go:40`; routing it to YouTube de-auths Twitch *silently*, because IRC falls back to anonymous). **On the wire: authenticated youtube.com requests no longer carry the Twitch session's `auth-token`/`login`/`name`/`twilight-user`.** Reviewed clean, race-detector clean. **T1 tier 2 (host-gating cookies off googlevideo) was REJECTED, not deferred** — its premise is inference from yt-dlp's shape, never measured, and its failure lands on members-only captures; the segment path got the live-getter treatment instead (Task 8). **The members-only field gate is therefore no longer a gate for this arc.**
- **V1 — CLOSED: `d5099a5` (YouTube chat, both request paths; Twitch IRC; Twitch VOD chat) + `387d24a` (media segments — `engine/downloader.go:142` `CookieHeader func() string`, read in `setCommonHeaders` once per outbound request across all six callers, never host-scoped).** Every long-lived consumer that snapshotted a credential at construction now reads it at use time, mirroring the `generateAuth` field that already did it right. One idea across six sites and four implementers: a nil-guarded method value at the construction site, `nil ≡ "" ≡ anonymous` at the use site (two spellings — inline nil-check vs a `current*()` helper — reviewed as equivalent, not divergent). Construction sites at `db1993a`: `orchestrator_chat.go:72`, `stream_processor_youtube.go:455`, `stream_processor_twitch.go:161` (VOD, bearer-only) / `:334` (IRC, the paired getter), `strategy_youtube_dash.go:253`, `strategy_youtube_manifestless_dash.go:235`; the one-shot `FetchWatchPage` calls beside them keep a snapshot deliberately. **Those construction sites are observed by no test** (M15/T7/T8 survived, pre-existing gap, accepted: the type change blocks an *accidental* re-snapshot, not a deliberate `func() string { return snap }`; two end-to-end tests pin the exact method-value expression against a real jar rotated mid-flight). **Owner-settled scope: live getter only — no reason-reporting, no mid-job restart, no downgrade notification.** Chat's `ErrAuthRequired` still has ZERO references in `internal/worker` — a genuinely dead session still ends chat for that job and nothing restarts it; only rotation-induced 401s are fixed, which is the V1 defect as filed.
- **Write-path guard — `37eb62e`.** `isEssentialCookie` carried the same unguarded clause; on the write path it caused **eviction** (`deduplicateAndFormat` keys by bare name, its only skip rule protects a YouTube incumbent, so a Twitch `SID` overwrote a Google `SID` before the file was written — where the jar can never refuse it). Domain-guarded; the bare-name dedup key stays, deliberately. Plus the missing Twitch half of the name-list drift guard.
- **FOUND IN REVIEW, the arc's largest finding — Task 7, DONE `7e742b2` + fix rounds `dccb5e5`/`db1993a`: Twitch IRC had never authenticated.** The old `chat_irc.go` sent `NICK justinfan<random>` unconditionally, including after `PASS oauth:<token>`; Twitch binds the session to the token's user via NICK, so the session was refused or silently anonymous. **Subscriber-only messages and badges have never been captured on Twitch, regardless of cookie health** — which retracts the `d5099a5` note that a Twitch job "will now authenticate on its next reconnect": the token reached the wire, the nick did not. **As shipped:** `ircHandshakeLines(token, login)` renders PASS+NICK (+ an `authenticated` bool) from ONE decision — token-with-justinfan is unreachable; a login the protocol cannot carry (space/tab/CR/LF/NUL) goes fully anonymous, not on the wire. The two halves come from ONE paired accessor under ONE `RLock` — `CookieJar.GetTwitchCredentials()` → `twitch.Auth.GetCredentials()` → `ChatDownloaderOptions.Credentials func() (token, login string)` — because a two-lock read tore ~4,500 pairs per run in `TestGetTwitchCredentialsIsAtomicAcrossReload` (0 paired); the single-half accessors `GetTwitchLogin`/`Auth.GetLogin` from `7e742b2` were **deleted** in `dccb5e5`, deliberately, so the tear cannot return through a surviving half. Ruled over `ValidateToken`'s authoritative login (network round-trip on a path that must survive flaky network). **One-shot anonymous fallback — a FLOOR, not a mechanism:** a credentialed session that heard at least one inbound line and never received `001` latches that job anonymous (`authRefused`, per `ChatDownloader`) with ONE Warn naming the channel and neither credential; a session that heard NOTHING is a dropped socket, not a refusal (the outage-over `startChat()` relaunch at `orchestrator_twitch.go:764` is the real path that made this bit load-bearing); a session WE ended (`Stop`/`MarkStreamEnded`/caller cancel) is never a refusal; the reconnect budget is unchanged. "Authenticated if Twitch accepts it, anonymous if not, never nothing" is exactly what shipped before the nick existed. **Trade, stated and unmeasured:** a Twitch that refuses SILENTLY (no NOTICE, undocumented, never observed here) reads as a drop and burns the reconnect budget credentialed — the pre-fallback outcome for that one hypothetical. A new discriminator silently defanged four round-1 tests (`TestIRCShutdownIsNotARefusal` and three siblings coasted on the drop rule); all four were re-armed with a PING→PONG round-trip proof — **standing rule for every later arc: when a fix adds a discriminator, re-run the previous round's mutants.** *Nothing elsewhere in this plan assumed Twitch chat was authenticated — every Twitch-side claim here is about the cookie/token, and the Deferred "Twitch liveness" entry is about channel entitlement, a different question.*
- **Task 9 — DONE `2727c29`, and all three live gates RUN and PASSED at `387d24a`/`2727c29`.** `internal/twitch/auth_live_test.go` `TestLiveAuthenticatedTokenValidate` (`MOOMBOX_LIVE_TWITCH_COOKIES=<path>`) mirrors `TestLiveAuthenticatedAccountProbe` shape-for-shape and truncates `ValidateToken`'s error to `%.300s` (its unexpected-status branch echoes up to 1 MB of body). Results: cookieless `TestLivePublicExtraction` PASS (VOD 27 formats / live 14 — the empty YouTube jar still yields playable formats); `TestLiveAuthenticatedAccountProbe` PASS, verdict `LoggedIn` (file → `Load` → YouTube jar → `GetCookieHeader()` → a page YouTube stamps as signed in); Twitch validate PASS (file → `Load` → Twitch jar → `GetCookie("auth-token")` → `oauth2/validate` 200). Nothing printed a cookie value. A nonexistent path fails at `HasAuthToken`, not `Load` — the ENOENT behaviour carried to Arc 8, seen from the test side.
- **Field gates, THREE, all open and all the owner's:** (1) **badges on a real subscriber-only Twitch capture** — the positive half of Task 7; that tmi accepts `PASS oauth:<web-session token>` + `NICK <login>` rests on chatterino7's shape, and `oauth2/validate` accepting the token says nothing about it; (2) **a credentialed reconnect surviving a real outage** — the drop-vs-refusal discriminator, and the NOTICE-then-close premise behind it; (3) **a real archive on each platform end to end** — for YouTube, one that runs past at least one ~30-minute rotation on a members-only or age-gated stream, which is Task 8's own gate (the live getter changes the wire only for a rotated jar). What the live gates prove is exactly this and no more: each jar's credential reaches a service that accepts it. **Reading rule for gate (1): presence of chat no longer distinguishes success from failure on Twitch — the single `continuing anonymously` Warn does, and badges do.** Static layer: every worker/engine credential consumer enumerated at `37eb62e` and re-checked at `db1993a` — all read the right jar.
- **Twitch keepalive — owner requirement, RESEARCHED, no in-process path exists** (`research-twitch-keepalive.md`): yt-dlp never writes `auth-token` back; chatterino7 implements `refresh_token` for Kick and for Twitch only *detects* expiry and asks for re-login; the only issuer is `passport.twitch.tv/login` (password+2FA). **The browser refresh already navigates to `twitch.tv` every periodic pass** — whether that renews the cookie is unmeasured. **Settling observation: sample `AuthCookieHorizonFor(twitch)` before and after a browser refresh — a timestamp, never a value.** Moves forward → the keepalive already ships; unchanged → the answer is the re-auth ingest path (open, unbuilt).
**Standing rules unchanged:** `main.go:276-278` no-touch (G5, re-verified at `db1993a`); `updateCookieFile`/`resolveRowUpdate`/`sameCookiePlatform` untouched (grow-broadly/destroy-narrowly is deliberate; `refresh.go` and `autocookies.go` are byte-identical to `1a0f0ff` at `db1993a`); Arc 6's `countNetscapeCookieRows`/`automaticImportGuard` read the raw file, not the jar. **Arming interaction — CORRECTED 2026-08-28 and re-confirmed at the close: there is none on the probe path.** The earlier claim that a Twitch-only install's fallback probe "now declines with `no session cookies` before probing (`liveness_fetch.go:83`)" was a misreading: both probes gate on `HasAnyAuthCookie()` → `jar.HasAnyYouTubeAuthCookie()` before any fetch (`account_probe.go:53`, `channel_membership.go:85`, `monitor_callbacks.go:821` earlier still), so a Twitch-only install never probed before Arc 5 and never reaches `:83` after it; the only change on that path is that a dual-platform install's probe header no longer carries Twitch rows. The pilot reading rules (arming checklist item 2) hold unchanged. Pilot still disarmed (`refresh.go:496`). **Carried to Arc 8 (enumerated there as item 11 — nothing below is closed by anything in Arcs 8-9 as previously written; RESOLVED in Arc 8, 2026-08-29: (a) ruled KEEP with the rename window disproved, (b)/(d)/(e)/(f)/(g) closed, (c) closed by a Warn with `login` deliberately NOT added, (h) ruled NO — see the Arc 8 paragraph, item 11):** (a) `Load`'s `os.IsNotExist` branch (`jar.go:156-164`) clears BOTH jars' live auth (EIO/EPERM protected, ENOENT not); (b) IRC login-failure NOTICE surfacing beyond the single Warn, and a Twitch anonymous-downgrade notification (owner-scoped OUT of Arc 5); (c) **NEW at the arc close — a fourth silent Twitch auth state the fallback cannot see:** `auth-token` present, `login` absent → `ircHandshakeLines` goes fully anonymous WITHOUT ever attempting the login, so no refusal, no Warn, while `HasTwitchAuthCookies()`/`HasAnyTwitchAuthCookie()` read TRUE and both UIs show Twitch authenticated; `login` is outside `twitchAuthCookieNames`, so `expiredTwitchAuth` and the auth-loss gate are blind to it, and `mergeCookieFiles` can prune an expired `login` while `auth-token` survives (a minimal hand-made `cookies.txt` carrying only `auth-token` — the shape older guides describe — lands here on day one); `jar.go:589-598` still says adding `login` is "safe mechanically" and "would close the last silent Twitch state" — both wrong now (Task 6's drift guard exists because it is not; Task 7 made `login` a credential half). Fix shape: one Warn in the token-without-login branch (names nothing) plus a decision on `login` joining the marker list; (d) `refresh.go:1962` / `:2004` still test `essentialYouTubeCookies[name]` without a domain guard — log-severity selectors only, gate no mutation — so the "last surviving instance" claim at Task 2 was wrong twice; (e) `twitch/auth.go:94-96` interpolates up to 1 MB of body into `ValidateToken`'s error and `:110` logs the account `login` at Debug (line numbers at `db1993a`; the plan's `:73-74`/`:88` predate `GetCredentials`); (f) `gofmt -l internal/ cmd/` flags three pre-existing files in `internal/database/` (`migrations.go`, `migrations_v18_test.go`, `migrations_v19_test.go`) — untouched by this arc; `gofmt -w` them and widen the standing gate so it cannot recur; (g) **test-quality residual:** the caller-ended guard at `chat_irc.go:170` is `ctx.Err() != nil || !cd.IsRunning()`, and only the `ctx.Err()` half is pinned (`TestIRCShutdownIsNotARefusal` cancels the context; no Twitch test calls `Stop()`/`MarkStreamEnded()` on a credentialed session, and every Task 7 test's second session is kept clear by the drop rule) — a mutant deleting `!cd.IsRunning()` survives the whole tree. Load-bearing only in the window between the first inbound line and `001` on a `Stop()`, so low, but it is the exact shape fix round 2 named. **Not ruled in Arc 5, still wanting a ruling:** the two Arc 2 evaluation items below — Arc 5 changed no write path, so neither the cross-writer lost update (now three readers: `updateCookieFile`, the import merge, and `automaticImportGuard`'s unlocked read) nor the browser path's no-rollback overwrite was decided; carry both to Arc 8's H1 cluster, where `updateCookieFile` is already open. **Test-side notes, no home needed:** the Twitch tests reassign `constants.TwitchURLs.IRCWS`/`.GQL` under `t.Cleanup` (needs a seam only if that package's tests are ever parallelised); `internal/chat`'s test imports `internal/youtube` (no cycle today; that file breaks first if one appears); `TestGetTwitchCredentialsIsAtomicAcrossReload` is a timing-sensitive detector that loses its teeth if the iteration counts shrink or a `WriteFile` returns to the writer loop.

**Standing rules for every Arc 5 task:** mutate the doc's CLAIM for every assertion (bracketing to one function is not enough; name/substring checks are no guard) — assert on exact rendering, parsed structure, or executed behaviour, and execute shipped JS under goja (`cookies_setup_utilsvm_test.go` pattern); never place an assertion downstream of a junction several mechanisms satisfy (the plan's signature failure, **~17 occurrences** — the seventeenth was Arc 5's own `TestPartitionIsStructural`, satisfied by the very comparator the task deleted until its fixtures were re-chosen so a wrongly-admitted row would WIN under the old mechanism); never read, print, or quote a real cookie value or a real cookie store's contents (names are fine); gates `go build ./...` · `go vet ./...` · `GOOS=linux go build ./...` · `gofmt -l` · `go test -count=1 ./...` against the 27-package/0-failure baseline from ONE saved run (concurrent `go test` runs starve `TestPotProvider_BypassCache`); trust `go build` over editor/gopls diagnostics (~18 phantom batches); any non-Go text file keeps LF — a tool's CRLF default is a silent content mutation and a CRLF shebang breaks the container; never kill a process by image name, never launch a real browser from a test. **These rules bind Arc 8 and Arc 9 verbatim.**

> **Two evaluation items added by the Arc 2 close — deliberate rulings wanted, not prescribed fixes.** Standing owner doctrine is against building mechanism without a profile, so both may correctly end in "no".
> 1. **Cross-writer lost update.** `RefreshService.updateCookieFile` and `AutoCookieService`'s merge writes are two owners of one file with no shared lock, and the import path holds `previousCookies` across network round-trips. The window is seconds-wide only on that path, and what is lost is normally a seconds-old rotation the next cycle repairs. Cheapest containment if it is ever justified: re-read and re-merge immediately before the write, not a lock.
> 2. **The browser path's no-rollback overwrite** — already inside Arc 5's identity territory.

**Arc 8 — Performance and cleanup — DONE, MERGED to `main` as `8389009` (2026-08-29, `--no-ff`, branch deleted, post-merge gates 27/0); the 24 commits below came from `cookie-arc8-perf-cleanup` @ `8cc212b` (24 commits, 78 files, +11585/−734; arc-close review 2026-08-29: delivered with named gaps).** *(Ledger: `progress-arc8.md` — the authority for every ruling, overturn and incident below; it carries the citation re-derivation at `5eb5266`, the 12-task decomposition, the 22 brief-level rulings and the per-task review/fix log. The pre-execution paragraph this replaces — re-derived at `37eb62e` — is superseded in full; `fable-arcs2-9-review.md` §Arc 8 is history.)* Task → commit: T1 `1956423`+`ae29629` · T2 `005d397`+`736f10c` · T3 `98fd59b`+`3e98493`+`962660e` · T4 `eefcb5f`+`de148b2` · T5 `3d56813`+`3cec378` · T6 `983e721` · T7 `55064df` · T8 `2bebdbd`+`25a3370` · T9 `69891ff` · T10 `b83abc0`+`792e9ae` · T11 `b95c9db` · T12a `c9cdf5c`+`f2869c9` · T12b `2f414f8`+`0039708` · controller `8cc212b`. **Re-verified by the arc-close reviewer at `8cc212b`:** tree clean; `go build` · `go vet` · `gofmt -l internal/ cmd/` clean repo-wide · `go test -count=1 ./...` **27 / 0 from ONE run**; `livenessRecoveryArmed = false` at `refresh.go:606`; G5 gate untouched; `git stash list` holds only the owner's pre-existing entry.

**Per finding:**
- **H5 — CLOSED.** `ReloginStatus()` — which REAPS exactly as `GetStatus` does, mutation-pinned, because the status poll is the reap that actually fires — replaced `GetStatus()` at the four relogin-only callers (`/api/status`, `authStatusToTUI`, `/recheck`, `/auto-refresh`). `DetectBrowsers()` got the same 60 s cache slot and mutex as `DetectBrowser()`, so `/auto-status` → `loadAutoCookieStatus` (the Arc 6 caller) stops paying for the scan as well. `InvalidateBrowserDetection()` fires on `ValidateBrowserPath` success and on every TUI settings save (unconditionally — the settings model mutates the store's live pointer, so no pre-mutation value exists to diff). A web `PUT /api/config` hook was ruled NOT needed — CLOSED, not carried: detection reads the filesystem and registry, never config. The `(network?)` hedge (follow-up 4) is split per platform through `combinedInconclusiveHedge` — the first cut's AND tie-break was caught asserting a cause the code knew was false on a dual-platform install. The `/auto-status` no-service branch now emits `availableBrowsers: []`.
- **S2 — CLOSED.** `refreshInFlight` single-flights all three entry points; `CheckNow` returns `started bool`; a second caller is a Debug no-op and NEVER a `RefreshDeclinedCauses` member (that vocabulary belongs to the browser refresher and is pinned exhaustively in three consumers). Callers that have just rewritten `cookies.txt` (recovery-succeeded in `monitor_callbacks.go`, `R F`) log the skip at Info; `/recheck`, `/auto-refresh` and `R C` do not (the in-flight pass publishes through `OnAuthChange`). **The `/recheck` router move is HELD — closed, not deferred:** `heavy` wraps every route in one per-IP limiter, so recheck clicks would consume the browser endpoints' budget; with the guard, hammering is already harmless. **Accepted costs:** a ticker tick landing during a manual pass is dropped for a full interval (a deferred retry would be a mechanism to contain a mechanism); `RecheckReport` has no word for "already running" and none was invented.
- **S3 — (a) CLOSED; (b) RULED OUT** (it conflicts with A1's "fires seconds after launch" and arming item 1). `Start` wraps the synchronous call in the ticker's recover. **The first cut SELF-DEADLOCKED** (review finding): the guard-release defer took `rs.mu` while `refresh`'s ~80-line status section held it with a bare `Unlock()`, so a panic there would have parked the goroutine holding the lock — a loud boot crash turned into a silent hang with no dashboard, no TUI, no log line, and `Start`'s new recover unreachable. Fixed at `ae29629`: every `rs.mu` section in `refresh` is a scoped func literal with a deferred unlock (standing rule written at the release defer), plus a `refreshLockedHook` seam INSIDE the locked window so the panic test exercises the case that can fail — the first test fired outside it and proved only that the defer existed (the decoy inside the bracket). The proof-by-revert reverted a wrong line first and "proved" the point on an unrelated hang; caught by printing line numbers. `Start`/`Stop` still hold `rs.mu` without `defer` (three statements each, not panic-reachable) — accepted; the last two bare pairs if the rule is ever wanted package-wide.
- **H1 — CLOSED.** One `youtubeGuideExchange` returns `(verdict, *http.Response, error)` and never writes; `checkYouTubeAuth` discards the response, so the rollback verifier structurally cannot corrupt what it judges; `checkAndRefreshYouTube` is the ONLY caller of `processYouTubeSetCookies` (one production call site, grep-verified at the close). `youtubeGuideURL` and `youtubeGuideRefreshURL` were byte-identical twins — the brief's "different endpoints kept apart on purpose" was false; collapsed to one. Ride-alongs 1, 3, 4 and 11(d) folded in.
- **Ride-along 1 — CLOSED.** `cookieOrigin` (`youtube.com` / `google.com` / `twitch.tv`) names the response's SITE, not a platform — rule 2 needs host scope while `sameCookiePlatform` needs the credential grouping, and one enum would have widened the destroy scope. Declared by the caller to `updateCookieFile`; read at three decisions: rule 2, `sameCookiePlatform`'s Domain-less default, and the insertion loop. **Found in review: a DECLINED update was INSERTED, not dropped** — everything the matching rules refuse falls into the insertion loop, which invented a domain from the cookie name, so an undeclared or foreign origin's unscoped `SID` appended a brand-new `.google.com` row (WIDER than the hardcoded behaviour it replaced). The loop now refuses any row outside the declared PLATFORM and everything for the zero origin (`736f10c`, applied to explicit `Domain=` keys uniformly). Recorded boundary: it is a platform check, not a site check — an `originGoogle` ingest may insert a `.youtube.com` row, matching rule 3's grow-broadly half.
- **B2 — CLOSED** (`98fd59b` + two fix rounds; its own task, hostile inputs mandatory). The substring pre-filter is DELETED; `admitSetCookie` admits by what the header SAYS, after the whole attribute list is read: (1) row-breaking characters in name, value or domain are refused — the TAB is the only live vector (Go's `net/textproto` cannot deliver CR/LF/NUL; kept as belt); (2) scoped: `cookiePlatformOf(Domain=) == origin.platform()` (equivalence to the old `isYouTubeDomain || isGoogleDomain` proven total; the `p == ""` clause is load-bearing against an undeclared origin); (3) unscoped: `trackedCookieName` only. A 26-row hostile table, each row asserting a verdict. Three RFC 6265 items were TAKEN — a tab in a VALUE is REFUSED (the brief's "admitted verbatim" row flipped; cookie-octet excludes HTAB and yt-dlp's loader rejects the row), whitespace around `=` trimmed per §5.2, `Max-Age` clamped to 400 days per RFC 6265bis §5.5 (`now + maxAge` wrapped NEGATIVE and every `exp > 0 && exp < now` guard read it as never-expired — unprunable and invisible to the horizon accounting) — and one was **OVERTURNED against the controller by the reviewer:** quoted values are NOT de-quoted and a quoted `;` still truncates, because §5.2 never strips DQUOTEs and every browser behaves so; CPython does because it implements RFC 2109. **Found in review, reachable from an ordinary Google reply:** the `domain == ""` insertion fallback had been DEAD CODE for its whole life and went live — it invented `.google.com` for an unscoped `SID` host-scoped to `www.youtube.com` (the exact inverse of rule 2's confinement, and a different cookie); the domain is now the declared origin's own site and `isGoogleOnlyAuthName` is retired as a domain-inventor. Plus the duplicate row: an unscoped key is not INSERTED beside a scoped, non-deleting sibling of the same name (`Load` keys by bare name, last row wins) — `hasScopedSibling`, made `Delete`-aware in fix round 2 (a scoped delete plus an unscoped insert of one name is "replace"). **Pinned, not narrowed:** scoped headers are admitted under ANY name (now a table row); `trackedCookieName` has no Twitch set (no caller exists). **Pre-existing interop, out of scope, named:** a row already carrying a tab in its value loads here and is rejected by yt-dlp's `MozillaCookieJar` (exactly-seven-fields) — a shared `cookies.txt` silently loses that row on the yt-dlp side.
- **The three admission/insertion layers, read as one rule at the close:** the outer layer (`admitSetCookie` — per-header, pure, the only code that ever sees the raw header: row-breakers and the tracked-name gate live here and nowhere else) and the inner layer (`updateCookieFile` — the origin-derived insertion domain, the platform guard, and the sibling guard, which needs the batch view admission cannot have). They compose, and `admitSetCookie`'s doc states the composition. **One bullet in that doc overstates the confinement:** "an unscoped header can only land on the declared origin's own rows" is true of DELETIONS (rule 2 only) and of INSERTIONS (the origin's site), but a value REFRESH still fans out platform-wide through rule 3 — an unscoped youtube.com `SID=fresh` rewrites an existing `.google.com SID` row, the very cookie the fix-round argument called "a DIFFERENT cookie" when refusing to INSERT it there. That is the deliberate grow-broadly rule, pinned by `TestUnscopedRefreshCrossesDomainsOnlyInsideTheDeclaredPlatform`, and the two arguments disagree with each other; one has to yield → carry-over (iv) below.
- **H2 — CLOSED as a PIN, not a bump.** yt-dlp's `2.20260708.05.00` is the `mweb` client, which Moombox does not ship; every WEB-family client stays at `2.20260708.00.00`. `youtubeClientVersion` deleted from `refresh.go`; `youtubeGuideRequestBody` reads `constants.WebClient.ClientVersion`; three WEB-family consistency tests (the iOS test's UA-contains-version check deliberately omitted — browser UAs carry Chrome/Safari numbers unrelated to the Innertube version) plus an anchored text pin on `bgutil-sidecar/src/server.js` (goja execution genuinely infeasible — jsdom bootstraps at module level; the pin's limitation is stated in its own doc). **X4 DONE here** (`platform-services.md` WEB, WEB_CREATOR and TV_DOWNGRADED rows corrected from `constants.go`). 11(f): the three `internal/database/` files were never gofmt-clean because Go ≥1.19's doc-comment formatter rewrites `''` to U+201D — the SQL moved into indented code blocks inside the same doc comments, zero smart-quote code points introduced; **`gofmt -l internal/ cmd/` is clean repo-wide and is now the standing gate.**
- **S6 — CLOSED, both halves.** `sweepStaleCookieTempFiles`: `<base>.*.tmp` older than 1 h, once per process at service construction, matcher anchored on the real `filepath.Base(cookiePath)`; `tightenCookieDirOnce` memoises on SUCCESS with an in-flight marker (three states), panic-safe — the recover deletes the entry unless `succeeded`, the exact shape of T1's deadlock finding closed correctly. Linux: `ApplyUserOnlyDACL` really chmods 0700/0600 (the "no-op on non-Windows" doc was stale), so a permanently-failing host (read-only bind mount) pays one chmod per cookie write — pre-approved by the no-cap ruling and pinned.
- **H3 + follow-up 6 — CLOSED.** `detectCookiePlatforms(meta, jar)`: the sidecar's verified `Platforms` wins outright when non-empty; otherwise the LOOSE predicates; the source is logged. `youtubeAuthCookieNames` verified sign-in-only against `jar.go` (ten names; `VISITOR_INFO1_LIVE` is not among them); `twitchAuthCookieNames` already settled by its own doc.
- **H6 — CLOSED by CORRECTION, hedged.** "Brave kept v10 and still works" is likely false — per ONE third-party research repo (dated 2026-01-24) Brave registers its own IElevator service; `references/yt-dlp/cookies.py` @`81ecd58` has NO App-Bound handling at all (grepped: zero matches), so it can neither confirm nor deny. The comment now carries the source, the date, the hedge and no shared-IID framing; no code change (v20 detection is prefix-based, not browser-based); any future user-facing copy pairing "Brave" with "DPAPI fallback" must re-check the note. **Reviewers on this branch do not fetch external cookie/credential-decryption research** — one was terminated by an automated safeguard doing exactly that.
- **H7 — CLOSED.** ONE profile, deliberately: configured-browser filter → best `dpapiProfileScore` (loose/strict tiers per platform, summed) → tie keeps scan order with an Info line naming every tied profile; cookies are never merged across profiles. **Found in review, CRITICAL, fixed (`25a3370`):** the filter spoke per-browser (`"brave"`, `"edge"`) while the Web UI's ONLY Chromium option stores `"chrome"` for the whole family — every Brave/Edge/Vivaldi user configured through the Web UI lost every profile (hard error on a Brave-only machine; a silently signed-out Chrome profile otherwise). Now `"chrome"` ≡ unfiltered (`DpapiChromiumFamilyValue`, exported for the drift pin that reads the shipped `index.html` against it), a type with no dpapi layout (Opera, Thorium) falls back to unfiltered scoring at Debug, per-browser types narrow with channel siblings, `dpapi.KnownBrowserFamilies()` is derived from the authoritative layout list, and the override predicate is `browserOverrideConfigured` (`path != "" && type != ""`) — aligned with `resolvedBrowser()`, since the TUI's free-text type field is reachable without a path. Two decoy tests found and rewritten (a `"chrome"` fixture the family rule now intercepts before the logic under test; `"BETA-SAPISID"` containing `"A-SAPISID"`). **Known limitation, not built:** one score for both platforms, so YouTube-on-A / Twitch-on-B loses one platform — deterministically and logged now.
- **S8 — OPEN, honestly.** No Chromium-family browser was running, and the task may not launch one (the owner runs real profiles here). `dpapi_windows.go` now states the real question — does `modernc.org/sqlite`'s `mode=ro` read of a LIVE WAL file merge committed `-wal` frames, or return the last checkpoint (`busy_timeout` cannot see a stale-but-well-formed read) — and the exact check (`journal_mode` + a row count twice, seconds apart, while the signed-in browser is receiving Set-Cookie). If stale, the fix mirrors `snapshotFirefoxCookieDB` (copy `Cookies` + `Cookies-wal`, sweep via `sweepStaleCookieSnapshots`). Field gate (5) below.

**Ride-alongs 3-9:** 3 — `#HttpOnly_` DECIDED and documented: PRESERVED on rewrite (`parts[0]` verbatim; the file's own row is the authority), ADDED only on insertion from `cu.HTTPOnly`; the flag is a stale annotation at worst since nothing in the package treats it as a control. 4 — B3: the seven-field comment is a claim about the row READ, not the row WRITTEN. 5 — follow-up 3: `validateErrorDetail` renders status + parsed/clamped media type + at most 200 B of `text/plain`/`application/json` only, bounded drain; `login` and `client_id` are no longer decoded at all; the Debug line carries `userID` only. 6 — follow-up 4, under H5 above. 7 — follow-up 5: an interval ≤ `livenessFreshWindow` is refused and replaced with the default, with a Warn naming both. 8 — 7(d): `firefoxLaunchSpacing` seam on the service (zero → the const); `internal/cookies` −5 s. 9 — 7(e) DECIDED: `errNavigateBudgetExhausted` sentinel; folds into `browserActed=false` only when EVERY platform stalled ("do not loosen 'every' to 'any'" — one slow page beside a working one is still acted).

**Item 10 — all eight Arc 4+7 residuals:** **web parked-badge parity** CLOSED as the REAL escalation (the brief's stated fix was a no-op — `updateStatusBar` never read the job list): `parkedCookiePlatforms` in `utils.js` (per platform, absent platform = YouTube, no `ParkReason` filter — the TUI's rules, ported), a fourth `parked` argument on `cookieIndicatorState` ranked exactly where the TUI ranks it (relogin > parked > authenticated), wired on `job_update` / `jobs_update` / `initial_state` / `job_deleted` behind a change gate (a boolean pair, so a reconnect replay cannot double-count). **CDP cause** CLOSED: `ErrBrowserLadderBlocked` → 409, `ErrBrowserReadUnanswered` → 502, both with a stable `cause` token, through ONE `writeBrowserReadError` shared by the refresh and finish handlers (teaching one of two consumers is the junction defect); neither wraps `ErrNoCookiesInProfile`; the frontend needs no branch (`serverErrorMessage` returns `data.error` verbatim). **Rows-came-back flag** CLOSED: `fetchedNoCredential` — a NEW flag beside `fetchedRows` (never a sentinel redefinition), measured on what THIS pass fetched before the merge, one new arm placed below the two inconclusive arms and naming the platforms; `netscapeCookiesHoldACredential` asks a throwaway jar's own predicates (`Load` split into `Load`/`loadFrom` so no temp file is written to answer an in-memory question). **`lastError` writer audit** CLOSED: the policy is written on the field (one SET funnel; three earned clears — `case renewed`, `StartSetup`'s slot claim, the "nothing to verify" arm, which appears UNREACHABLE and is kept with its derivation; `cleanup()`/`cleanupLocked()` never clear, pinned; "EVERY exit that returns an error sets") — **three `FinishSetup` exits were found setting nothing** (mkdir, `writeFileAtomic`, `jar.Load`) and routed, the `jar.Load` one deliberately unpinned as unstageable; the two guard clauses `ErrNoSetupInProgress`/`ErrSetupCancelled` are named as excluded on purpose (`8cc212b`). Rendered: the web settings panel (`#auto-cookie-last-error`, hidden when empty) and appended to the `R C` line (`| Last cookie error: …`). **`FlagManualRelogin`** DELETED (zero callers on a security-sensitive service); `docker-ingest-brief.md` carries the re-add-with-its-caller note and the two test files record the restore condition. **`/auto-status` no-service branch** under H5. **Colorizer** CLOSED STRUCTURALLY: conclusive "not authenticated" is red in `R C` as in `R F` — and **the width clamp ran BEFORE the colour**, so on any terminal ≤ ~42 columns a recorded cookie failure rendered GREEN, and (broader than the review reported — the implementer measured it) at ≤ ~34 columns the conclusive refusal itself did too; `cookieRecheckFeedback` now returns `(line, feedbackSeverity)` computed monotonically from the verdicts, the active set and `LastError`, `feedbackColor(msg, stated)` obeys a stated severity and falls back to the substring scan only for factless lines (`severityUnstated` is the zero value, so ~40 call sites changed colour by nothing); the gray "deleted:" branch was NOT reordered by ruling (its real producers interpolate user-content titles). **`AuthStatus`'s three unread fields:** `YouTubeError`/`TwitchError` now have readers on the PER-REQUEST paths only — `CookieStatusPayload`/`TwitchAuthStatusPayload` (`youtubeError`/`twitchError`, safe to render because every producer is status-and-cause only and the one body-shaped error is a fixed sentence, pinned), the web badge title's inconclusive arm, and the `R C` line; the push-driven TUI status bar deliberately renders NO reason. **`LastCheck` is still unread and undocumented as such** → carry-over (v). Two production call sites (auto-refresh, finish) were found UNPINNED by mutation and got AST shape guards.

**Item 11 — Arc 5's carry-overs (a)-(h); TWO controller rulings OVERTURNED by evidence:** (a) **ENOENT-as-empty KEPT, and the rename window is a THEORY, proven at the go1.26.1 sources:** `os.Rename` is one `MoveFileEx(MOVEFILE_REPLACE_EXISTING)` with no `DeleteFile` ahead of it, and `os.Open` asks for `FILE_SHARE_READ|WRITE` without `FILE_SHARE_DELETE`, so a `Load` in flight makes the RENAME fail loudly rather than being handed a missing file — true of `os.Open`/`os.ReadFile` (what `Load` uses), NOT of `os.Root` opens, which pass `FILE_SHARE_DELETE` (qualified in the doc). `Load` unchanged; the derivation is its doc comment; the ruling's load-bearing half (ENOENT clears both jars) was UNPINNED and got a test. (b) `ChatDownloaderOptions.OnAuthDowngrade(reason)` — four opaque reason tokens (`login-refused`, `login-never-acknowledged`, `no-login-cookie`, `unusable-login-cookie`; no format verb, so no leak is possible), fired at most once per downloader across every site (`downgradeReported`, a THIRD latch: `authRefused` is a behaviour switch, `warnedNoLogin` is per-site); the worker sends ONE `TypeWarning` notification on the existing `"auth"` event through `Manager.Send`'s exact signature (`notifySend`), no DB column, no cross-job dedup by design; the callback takes the notifier LOOKUP so the production closure is the one under test. **The description names the VIDEO consequence** — the owner's mid-review question, answered from source: the playback token is fetched ONCE per capture, so THIS download is unaffected; the NEXT starts anonymous — stitched-ad gaps skipped at Info only, subscriber-only VODs fail outright. (c) **`login` NOT added to `twitchAuthCookieNames` — the controller's ruling was WRONG and the brief's precondition caught it:** `checkTwitchAuth` returns a conclusive `(false, nil)` with NO request on an absent token, and `shouldFireRecovery`'s first-check arm returns `cookiesPresent` verbatim, so `login` in the list would fire "twitch auth lost" on the first check of EVERY start for every Twitch user (a re-auth notification or a headless-browser launch). Shipped instead: `noteMissingLogin` — one Warn per downloader on login-UNUSABLE, absent OR unsendable (**the fourth silent state, found in review:** `login = "archiver account"` passes `login != ""` and `ircHandshakeLines` throws it away; `hasRowBreakingChar` is ONE predicate with two callers so the two cannot drift), and the `jar.go` comment now ENUMERATES four states instead of declaring a last — that superlative was wrong twice. (d) both `essentialYouTubeCookies[name]` tests domain-guarded via one `rowHasEssential`. (e) ride-along 5 above. (f) under H2. (g) the `!cd.IsRunning()` half pinned — `Stop()` and `MarkStreamEnded()` mid-handshake on a credentialed session are not refusals. (h) **RULED NO to both** Arc 2 evaluation items — recorded under Deferred as rulings, not as open items.

**Test-quality events worth carrying (junction count now ~20):** the deadlock test that proved only "the defer exists" (T1); `rowFor` returning the FIRST match, hiding an inserted duplicate until a row-count assertion was added (T2); a routes test wiring its own copy of the closure, replaced by an AST guard on the real wiring (T4); a `"BETA-SAPISID"` fixture containing `"A-SAPISID"` making a negative assertion vacuous, and a reversed-order test that could not fail under the merge mutant (T8 — the standing rule's third half, exactly); a latch-on-`authRefused` mutant that SURVIVED the whole pre-existing suite and would have turned a report into a permanent demotion to anonymous (T9); a worker "leak mutant" that was really an edit — the real proof is the twitch-side assertion (T10); `TestPeriodicBrowserlessTickImportsOnlyWithNothingToLose` failing once under parallel-package load (real-time ticks), classified as a flake and made deterministic — it now waits on the guard's own stand-down count, `-count=20` 20/20 (T12a); a test that fed only UNCLAMPED strings to a colorizer that ran on clamped ones (T12b). **No aggregate survivor found at the close:** no task after T3 touched `refresh.go`'s admission or `updateCookieFile` (T5 deleted one constant above them; T12a left the file read-only), and the three foreign tests T3 rewrote were re-verified to discriminate against their predecessors' defects.

**Process rules this arc produced (binding on Arc 9):** (1) a proof-by-revert must NAME the exact line it reverted (T1 fix — a false-positive revert "proved" the point on an unrelated hang); (2) the pre-mutation backup is mandatory and byte-exact, the restore path is verified to point at THIS round's file before any mutant runs, and **no `git checkout <file>` to revert a mutant either** (T3 fix 2's harness silently restored the PREVIOUS round's file over both fixes; T12b fix 1's `git checkout` discarded uncommitted edits — both caught within a command, both re-applied byte-identically); (3) **no `git stash` of any kind** — use a worktree for A/B timing (T11); (4) never two implementers in one file — fix rounds queue behind the live task, reviews run concurrently in a worktree; (5) a fresh worktree cannot `go build ./...` (gitignored embed blobs) — single-package `go test` is the review gate there; (6) the monthly spend cap kills sessions mid-task — the safety-net cron resumes, the controller re-dispatches fresh, never retries in the main session.

**Field gates — all open, all the owner's; nothing in this arc is presented as field-verified, and every "verified" in the ledger is a source read, a mutant or a gate run:** (1) badges on a real subscriber-only Twitch capture — and, with a deliberately broken `login` row, the degradation notification arriving in a real Discord channel (nobody has seen it arrive); (2) a credentialed reconnect surviving a real outage; (3) a real archive on each platform end to end (YouTube past one ~30-minute rotation on a members-only or age-gated stream); (4) the Twitch keepalive observation — `AuthCookieHorizonFor(twitch)` before and after a browser refresh, a timestamp never a value; (5) **S8** — `journal_mode` and a row count read twice against a RUNNING Chromium while its cookies rotate.

> **Carry-overs, ONE block, each with a home** (nothing found in a report or a review is left without one):
> **`refresh.go` — a later owner; homed in Arc 9 as one small doc-and-gate task, because this arc's `refresh.go` tasks are closed:** (i) `authStatusChanged` (`:324-327`) excludes `YouTubeError`/`TwitchError` from its change gate — the precondition for ANY push-driven surface rendering a reason (the TUI status bar says so in its own comment); widen it to fire on a reason change under `RefreshUnknown`, or leave it and document that no push surface may render the strings. (ii) Three stale comments and two stale pointers: `:1508-1523` ("`YouTubeError` still has no reader … a DECISION" — reversed by 12a(v)), `:324-327` ("nothing renders the strings" — false since 12a), `:1546-1552` ("Follow-up 1 … until it lands, an install … still RENDERS as not authenticated" — closed at `f2b4e30`), and the "see `errGuideLoginMarkerUnreadable`'s doc for the real surface" pointers at `:1843-1847` and `:2973-2977` (the real surface is now also the wire; the safety argument lives at `CookieStatusPayload`). (iii) The third answer in `shouldFireRecovery` — conclusive-by-evidence vs conclusive-by-absence-of-credential — is the precondition for `login` (or any name that can outlive `auth-token`) ever joining `twitchAuthCookieNames`; it can live entirely in `shouldFireRecovery` without reopening `checkTwitchAuth`'s contract. (iv) The `admitSetCookie` doc bullet vs rule 3's platform-wide value refresh (the "one rule" note above): reconcile the doc to the shipped rule, or decide that a host-only youtube.com cookie may not REWRITE a `.google.com` row either — the "different cookie" argument applied to insertion says the latter; the pinned test says the former. (v) `AuthStatus.LastCheck` — the one field of the three still unread: delete, or document it as unread by design.
> **Twitch — owner decisions:** (vi) **a "capture started anonymous despite credentials in the jar" check** at `Service.GetHLSMasterPlaylist` (`service.go:63`, the sole per-capture token point) or its caller (`stream_processor_twitch.go:410`, which holds `job` and the notifier) — with chat capture disabled (`Downloader.DownloadChat = false`) a dead Twitch token is invisible at EVERY level today; premise to verify first: does the playback-access-token reply say whether the token was honoured, or must "anonymous" be inferred from the first stitched-ad DATERANGE. Recommended build; the owner asked exactly this. (vii) A job-row "chat: anonymous" indicator — needs a v19→v20 column for one Twitch-only bool plus both UIs; the downloader is already shaped for it (`OnAuthDowngrade` fires once with a stable token). (viii) Splitting `authCookieNamesFor` so the DIAGNOSTIC predicates (`ExpiredAuthCookiesFor`/`AuthCookieHorizonFor`) see `login` while the ALARM does not — today there is no advance warning before `mergeCookieFiles` prunes an expiring `login` and chat goes anonymous; a design change to a deliberately shared set → Deferred.
> **dpapi:** (ix) S8 — field gate (5). (x) Two-pass per-platform profile selection (H7's known limitation) → Deferred. (xi) Arc 9 X2's remainder is SMALLER now: `autocookies_dpapi.go` HAS tests (T8, 586 lines, seams over `FindBrowserProfiles`/`ReadChromeCookiesStats`); `removeStaleLock`, `cleanChromiumLockFiles`, `getFreePort`, `waitForCDP` still have none.
> **TUI (small follow-ups, no arc):** (xii) `feedbackSev` is unstated-safe by INVARIANT, not construction — three setters write it, five clear-only sites leave it behind harmlessly; folding the pair into one struct makes it structural. (xiii) `fitFeedback` clamps at compose time; a resize narrower inside the 3 s window re-opens the wrap. (xiv) The substring scan is still the arbiter for every factless caller — correct today (locally composed, unclamped); any future path that composes foreign prose AND goes through `fitFeedback` must state a severity. (xv) A stale `COOKIES?` job keeps BOTH surfaces' badge red until the job leaves — by design; if the field finds it noisy, fix both at once.
> **Arc 9 (docs and normalisation):** (xvi) `*.html text eol=lf` is missing from `.gitattributes` (`*.js` has it; the committed `index.html` blob is LF and round-trips, so nothing is at risk yet). (xvii) `docs/spec/user-interfaces.md` omits `youtubeError`/`twitchError` on the two status payloads, the `cause` key on the 409/502 browser-read answers, and `availableBrowsers: []` in the no-service branch. (xviii) `docs/spec/data-and-storage.md` must carry: the `lastError` write policy; `cookieOrigin` and admission by parsed attributes (the three-step rule, the 400-day clamp, the no-de-quoting ruling); the temp-file sweep and the DACL memo-on-success; the one-profile DPAPI rule and the `"chrome"`-means-family vocabulary; `CheckNow`'s single-flight; sidecar-first platform detection; the `YouTubeError`/`TwitchError` readers and the gate they wait on. (xix) `settingsPanelVM` is duplicated between `internal/tui` and `internal/web/routes` tests (the packages cannot share helpers; a third copy would be a smell — no action).
> **Closed, not carried — so nobody re-finds them:** the `/recheck` router move (strictly worse without its own limiter); a web `PUT /api/config` invalidation hook (detection reads the filesystem, not config); a `logger` parameter on `navigateAllPlatforms` (the pure fold logs through its caller by design); a `twilight-user` evidence pass (ruled over-cautious — a per-user JSON record, shipped without false alarms); the gray-branch reorder in the colorizer; `cause` branching in the frontend; the 12-value `var` block in `refresh` (the aesthetic cost of not extracting a method mid-arc — transfers directly if extracted later); `TestUpdateCookieFileSubdomainFlag`'s `include_subdomains=FALSE` branch pins a shape production cannot produce (every inserted domain is dot-prefixed by construction) — harmless, noted.

**Ledger gap found at the close, to record before merge:** Task 12b's fix-round-1 re-review (dispatched on `review-f2869c9..0039708.diff`) has no recorded verdict in `progress-arc8.md`. The arc-close reviewer read `0039708`'s diff directly: `app_update.go` carries all of round 1 (`cookieRecheckFeedback` returning the severity, `setFeedbackWithSeverity`, the `setFeedbackWithDuration` reset, `fitFeedback`) and `app_layout.go` carries `feedbackColor(msg, stated)` — the `git checkout` slip left nothing behind. Everything else the ledger asserts was chased to shipped source at `8cc212b` and matched, including both overturns (11(a): `Load` unchanged, ENOENT clears both jars, `FILE_SHARE_DELETE` and the `os.Root` qualification in the doc; 11(c): `twitchAuthCookieNames = {"auth-token", "twilight-user"}`, the Warn fires on absent-or-unsendable, the enumerated comment lists four states) and the three process-slip incidents (both Task 3 fixes present in `refresh.go`; the stash list unchanged).

**Standing rules unchanged:** `main.go:276-278` no-touch (G5); `livenessRecoveryArmed = false` (`refresh.go:606` at `8cc212b`); grow-broadly/destroy-narrowly is deliberate — this arc added ENFORCEMENT (the declared origin), not a new rule; Arc 6's `countNetscapeCookieRows` over-count is load-bearing and `fetchedNoCredential` is a separate flag by construction; `ErrNoCookiesInProfile` is still never the auth predicate and the two new browser-read sentinels never wrap it.

*(The pre-execution finding list and ride-along block that followed here were superseded by the paragraph above on 2026-08-29; the ledger's citation table at `5eb5266` is the record of what each finding looked like before the arc.)*

**Arc 9 — Docs and tests — DONE, MERGED to `main` as `dd5dd18` (2026-08-29, `--no-ff`, branch deleted; the three named one-clause edits landed as `7599ada`; post-merge gates 27/0).** Branch `cookie-arc9-docs-tests` from `8389009` @ `7599ada`, 20 commits (11 implementer, 8 controller, 1 arc-close plan edit). Ledger `progress-arc9.md` (the authority for every ruling below, and for the owner's Q1-Q13 sweep); arc-close review `arc9-arc-close-review.md` (cross-doc 14/14; 147 citations sampled, 144 PASS; 43 absence claims, 40 PASS; the Arcs 10-12 + housekeeping doc-dependency map). Task → commit: T1 `ed89fd2` (X2) · T2 `fd1ca03` + fix rounds `851d668` (comment-only) and `c6cf9c2` (test-only) + controller `c0bcfc0` · T3 `38f8a57` · T4a `4d8d90b` + fix `034b39c` + controller `07c3d07` · T4b `b7d14e3` + fix `c1035a7` + controller `09d25bd` · T4c `4a91c74` + fix `8f6306b` + controllers `cd7c6a8`, `2cb1ace`, `97cfecf` · plan docs tracked `b5b6d07` (Q11). Gates at every landing: `go build` · `go vet` · `gofmt -l internal/ cmd/` · `go test -count=1 ./...` **27 / 0 from ONE run**; `livenessRecoveryArmed = false` at `refresh.go:632`; G5 untouched.

**Both stated preconditions were overtaken by rulings, not met:** the pilot did NOT arm first — the owner ruled (progress-arc8.md §ARMING) that the docs describe the pilot AS DISARMED, which every doc now does in one sentence each (`SPEC.md` §Cookies, `data-and-storage.md` §Refresh Service, `operations.md` §Credential Notifications); and 7f was NOT measured — the docs state the Linux launcher-handoff premise as UNMEASURED with the settling experiment (`operations.md` §Browser Cookie Acquisition), and Q9 rules no Linux testing this release.

**What each doc now states, per the blockquote below (every mechanism sentence cites its file and function; three implementers, one Fable arc-close checked them against `8389009`/`97cfecf`):**
- **Arc 2** → `data-and-storage.md` §Refresh Service: Set-Cookie deletion semantics are CPython's (the old S7 "yt-dlp" sentence corrected), the two deletion forms honoured and bare `NAME=` REFUSED with a Warn on an essential name, `ErrCookieFileUnreadable` and the read-abort rule, `Domain=` lowercased at parse.
- **Arc 3** → NEW `operations.md` `## Browser Cookie Acquisition (Platform Differences)`: the reap keys on a Job Object reporting zero live processes, never `cmd.Wait()` (`setupBrowserGone` returns TWO bools); `queryable()` is false on `job_linux.go`/`job_other.go` so **a browser left by an abandoned setup is not reaped on Linux or in Docker** (both Windows families do reap); `AbandonSetup` releases only where releasing cannot kill; the 60 s grace priced against the clients' 60 s caps (Chromium column 45.3 s binding, ~14.7 s margin) and the TUI's separate 300 s countdown cancelling in-process through `OnCancelAutoCookie`; the drain's numbers as observations; "acted" vs "succeeded" per engine (Firefox screenshot, Chromium navigation fold with `errNavigateBudgetExhausted` and the every-platform rule).
- **Arc 4+7** → `user-interfaces.md`: the `verification` tri-state on both payloads and every endpoint body, `CookieStatusUnknown` dropped at `tierEssential` on purpose, the parked-`COOKIES?` escalation per platform on BOTH surfaces (no `ParkReason` filter, absent platform = YouTube), the full status-bar tier table, the Web/TUI parity table with every divergence stated as a specification; the bogus "expired" state removed from `CookieStatusMsg` and the bar.
- **Arc 6** → `data-and-storage.md` §Auto-Cookie Service and `[cookies]`, `SPEC.md` §Cookies, `operations.md` Docker paragraph: `auto_enabled` owns exactly three things (the headless timer, the one automatic recovery attempt, the `SetExpectedPlatforms` read at `main.go:276-278`) and is restart-required on both UIs; the timer is `gateExempt`; the `R F` / shift+click / Settings-button ("Refresh cookies from browser profile") three-rung ladder with the two pinned rung-3 sentences; `automaticImportGuard` (absent or zero rows; unreadable ABORTS) and WHY `countNetscapeCookieRows` must over-count; `StartProfileSeed` unconditional, 15 s, re-asking; the Docker workflow, with `docker/entrypoint.sh`'s comment block named as the operator-facing wording.
- **Arc 5** → `data-and-storage.md` §Cookie Jar, `platform-services.md`, `SPEC.md`: one file, two jars, domain-first admission, the within-jar total order, expiry CAPTURED never filtered (and why the jar/merge disagreement is load-bearing), `GetCookieHeader()` YouTube-only, `GetCookie` → Twitch jar, `GetTwitchCredentials` under one `RLock`, `YouTubeIdentity`, the SAPISIDHASH derivation with its origin allowlist, every long-lived consumer reading its credential at use time, Twitch validate-only (`ValidateToken` decodes `user_id` only; bounded error detail), the IRC PASS+NICK pair from one decision, the one-shot anonymous fallback (absence of `001` + `heardFromServer`), the four `AuthDowngrade*` tokens and `noteMissingLogin`, `login` deliberately NOT in `twitchAuthCookieNames` and why, the keepalive research conclusion with the twitch.tv-navigation renewal stated UNMEASURED and its settling observation, `passport.twitch.tv/login` attributed to yt-dlp's extractor only.
- **Arc 8** → `data-and-storage.md` (single-flight on `refreshInFlight`; `Start`'s recover and the every-`rs.mu`-section-defers rule; ONE guide exchange, `checkAndRefreshYouTube` the sole writer; `cookieOrigin` as a SITE feeding three decisions; admission by parsed attributes — row-breakers, scoped-on-platform, `trackedCookieName`, 400-day clamp, no de-quoting; the three-verb table with `admitSetCookie`'s comment named as the authority and the owner's rule verbatim; the write-path rules; the three liveness maps and the fresh-window bound; `authStatusChanged` as a CONTRACT; `AuthStatus` every-field-has-a-reader with `LastCheck` deleted; the DPAPI one-profile rule with S8 open; the temp-file sweep and DACL memo-on-success; sidecar-first platform detection; the `lastError` write policy; `fetchedNoCredential`; `ReloginStatus` reaps; cached `DetectBrowsers`), `user-interfaces.md` (`youtubeError`/`twitchError`, 409/502 + `cause`, `availableBrowsers: []`, the R C severity rule and the R F outcome lines), `operations.md` (the Credential Notifications table, `RefreshDeclinedCauses` and the deliberately-omitted fourth decline, the Brave v20 hedge in the code comment's own words), `appendix-metrics.md` (v19, 10 browsers, 27/31 packages, every number re-derived; the fabricated 500 ms CDP line deleted).

**Tests added (X2, mutation statements in the arc-close review §4):** `autocookies_chromium_launch_test.go` — ten tests over `removeStaleLock`, `cleanChromiumLockFiles`, `getFreePort`, `waitForCDP`, fixture built from `chromiumLockFiles` itself with `SingletonSocket` aged FRESH; `TestScopedInsertionCreatesOnItsDeclaredDomain` — the owner's rule end to end (real header → admission → CREATE on `.google.com` → readable from the YouTube jar), with the two edges and its own over-claim corrected; `LastCheck`'s two tests updated. One surviving mutant recorded, harmless: dropping `cleanChromiumLockFiles`' canonical loop is masked by the `Singleton*` / `*lockfile*` globs, which cover every canonical name today.

**`refresh.go` (T2 + rounds):** the `admitSetCookie` doc gained WHAT AN ADMITTED HEADER MAY DO, BY VERB, opening with the owner's rule (YOUTUBE COOKIES SHOULD ALLOW GOOGLE COOKIES AS WELL — one platform, one jar, the only refusal being misattribution of a Domain-less youtube.com cookie onto `.google.com`); "cannot invent a `.google.com` row" was FALSE for a scoped header and is gone; REFRESH/CREATE/DELETE each stated for scoped and unscoped with rule 3's `refreshes == 1` disambiguation and `hasScopedSibling`; the `authStatusChanged` gate is DOCUMENTED, not widened (ruling: both readers are per-request, widening buys nothing; the later code item is "pin the contract", not "widen" — this SUPERSEDES carry-over (i)); the five stale comments/pointers corrected ((ii) CLOSED); **`AuthStatus.LastCheck` DELETED by reachability** — no projection carried it ((v) CLOSED); (iv) CLOSED by the three-verb section. Line-number citations inside that section are symbol-paired and, at `97cfecf`, uniformly +3 stale since `c0bcfc0` (accepted rot per the T2 ruling; the housekeeping citation-rot test covers them).

**Arc-close findings, each a one-clause doc edit the controller makes before merge:** F1 `data-and-storage.md` `[cookies]` `Platforms` row says `PATCH /api/config` — the route is `PUT /api/config` (`config_routes.go:724`; `user-interfaces.md` already says PUT); F2 `data-and-storage.md` §Refresh Service "Whether that periodic re-alarm, a once-per-process one, or a back-off is wanted has not been decided" — the owner decided Q1 (back-off) on 2026-08-29 after the sentence was written; state it as decided and NOT YET BUILT; F3 `platform-services.md` §IRC Chat "nothing here parses NOTICE" — `ircIsLoginFailureNotice` does, and the same doc says so ten lines later; say the HANDSHAKE does not depend on parsing NOTICE.

**Residuals with homes:** the doc-dependency map for Arcs 10-12 and housekeeping (which sentence each future arc must edit, because the docs state TODAY's truth and none pre-announces unbuilt work) is `arc9-arc-close-review.md` §3 — carry it into each arc's brief; the `mux-wait-chat-idle` rebase must drop `LastCheck:` at its `refresh.go:755` (the field no longer exists); `internal/config/types.go:209-211` `Platforms` comment is stale (housekeeping); on-disk CRLF in `web/public/favicon.svg`, `web/public/moombox.css` and three older `docs/superpowers/` files (blobs are LF; housekeeping `*.css`/`*.svg` rule + `git add --renormalize`); `.gitattributes` gained `*.html text eol=lf` (T3, the whole of that task — the two JS blobs were already LF).

> **What the docs must now state that Arcs 2-6 and Arc 5 introduced** (the review's X3 list covered only Arcs 0/1 — drain-wait launch model, `RefreshResult`, the two-tier liveness system and its disarmed pilot, `RefreshDeclinedCauses`, `attempted` semantics):
> - **Arc 2:** Set-Cookie deletion semantics are CPython's, not yt-dlp's (the spec's S7 still says yt-dlp — Arc 9 owns that correction); the two deletion forms honoured, the bare `NAME=` form REFUSED; `ErrCookieFileUnreadable` and the read-abort rule; `Domain=` lowercased at parse.
> - **Arc 3:** the reap keys on a Job Object reporting zero live processes, never on `cmd.Wait()`; `AbandonSetup` (`/auto-setup/abandon`) releases only where releasing cannot kill; the 60 s grace window priced against the clients' 60 s abort; the TUI countdown is 300 s (`setup_wizard.go:46`); **A1 is NOT fixed on Linux/Docker** (no Job Object primitive) — the docs must say so.
> - **Arc 4+7:** the `verification` tri-state on every cookie payload; `CookieStatusUnknown` tier placement; parked-job badge semantics per platform; the web/TUI parity exceptions (parked-badge on the dashboard).
> - **Arc 6:** `auto_enabled`'s settled meaning (EXECUTION STATUS block, verbatim — including the G5 exception and the `gateExempt` timer); the `R F` three-rung ladder and its owner-verbatim rung-3 sentence; `automaticImportGuard`'s "absent or zero rows; unreadable aborts" rule and WHY it must over-count; `StartProfileSeed`; the Docker guidance (flag off, update the mounted profile, `R F` / shift+click / the Settings action).
> - **Arc 5:** ONE `cookies.txt`, TWO in-memory jars partitioned by domain at parse time (youtube.com+google.com → `youtube`; twitch.tv → `twitch`); `GetCookieHeader()` is YouTube-only and authenticated youtube.com requests no longer carry Twitch rows; `GetCookie(name)` reads the TWITCH jar (sole consumer `twitch/auth.go`); expiry is CAPTURED per entry and reported per platform (`expiredYouTubeAuth` / `expiredTwitchAuth` on the startup line; `AuthCookieHorizonFor`) but NEVER filtered on load — `mergeCookieFiles`/`rowExpired` remain the only pruner, and that disagreement is load-bearing for loss detection; within-jar name collisions resolve by a total order on domain (youtube > google, fewer labels, dot-prefixed, lexical); every long-lived downloader reads its credential at use time (`func() string`; the IRC one is a paired `func() (token, login string)` from a single-`RLock` accessor); **Twitch IRC now sends NICK from the `login` cookie** — before Task 7 it never authenticated at all — **and falls back to anonymous, once per job with one Warn, when Twitch answers a credentialed handshake without `001`; a session that heard nothing is a drop, not a refusal; a token without a `login` cookie is anonymous silently (Arc 8 item 11c)**; there is NO in-process Twitch keepalive (`research-twitch-keepalive.md`: yt-dlp never writes `auth-token` back, chatterino7 only detects expiry; the only issuer is `passport.twitch.tv/login`) — whether the browser path's twitch.tv navigation renews the token is UNMEASURED, and the settling observation is `AuthCookieHorizonFor(twitch)` before/after a browser refresh, a timestamp never a value; the re-auth ingest path remains unbuilt.
> - **Arc 8:** `CheckNow`/ticker/`Start` single-flight on `refreshInFlight` (a second caller is a no-op, never a `RefreshDeclinedCauses` member; a dropped ticker tick waits a full interval); `Start`'s recover and the rule that every `rs.mu` section in `refresh` defers its unlock; ONE guide exchange that never writes, with `checkAndRefreshYouTube` the sole writer; the declared `cookieOrigin` (a SITE — youtube.com / google.com / twitch.tv) and the three decisions it feeds (rule 2, `sameCookiePlatform`'s Domain-less default, the insertion loop's platform guard); Set-Cookie ADMISSION by parsed attributes — row-breakers refused (tab is the only live vector), scoped headers gated on the origin's platform under any name, unscoped headers gated on `trackedCookieName`, `Max-Age` clamped to 400 days, quoted values NOT de-quoted (RFC 6265 §5.2, not CPython's RFC 2109), an unscoped insertion lands on the origin's own site, an unscoped key is not inserted beside a scoped non-deleting sibling; `#HttpOnly_` preserved on rewrite and added only on insertion; the WEB client version pinned at `constants.WebClient.ClientVersion` (Go tests + a sidecar text pin); `cookies.txt.*.tmp` swept once per process after 1 h; the DACL memo on SUCCESS with retry per write (Linux really chmods); `ReloginStatus()` (reaps) and the cached `DetectBrowsers()` with `InvalidateBrowserDetection()`; first-run platform detection sidecar-first, loose predicates second; the DPAPI fallback reads ONE profile (configured-browser filter with `"chrome"` meaning the whole Chromium family, best auth-set score, tie → first with an Info line) and never merges across profiles, with S8's WAL question OPEN; H6's Brave sentence corrected and hedged; `ValidateToken`'s error carries status + media type + ≤200 B of text/json only, and never the login; ENOENT-as-empty is a RULING with the rename-window derivation in `jar.go`; the four enumerated silent Twitch chat states, `noteMissingLogin`'s Warn on an absent OR unsendable `login`, `login` deliberately NOT in `twitchAuthCookieNames` (and why), and the once-per-job `OnAuthDowngrade` → "Twitch chat is anonymous" notification with its NEXT-capture video consequence; `cdpNavigateAndWait`'s `errNavigateBudgetExhausted` and the every-platform fold; the `lastError` write policy (one SET funnel, three earned clears, `cleanup()` never clears) and its two readers (web settings panel, `R C` line); `fetchedNoCredential` as a new flag; 409/502 + `cause` for the two browser-read sentinels; the web parked-badge escalation (relogin > parked > authenticated, four events, change-gated); `YouTubeError`/`TwitchError` rendered on per-request paths ONLY and why the push-driven TUI bar renders no reason; the TUI's stated `feedbackSeverity` (the clamp-before-colour defect); `FlagManualRelogin` deleted pending the ingest path.
> - **Spec facts still stale from the review's spot-check:** schema "v15" (now v19), the 6-browser table (`knownBrowsers` has 10), the fabricated "Poll interval: 500ms" CDP description.

**Namespace rule (follow-up 8, now standing practice):** every task brief citing an `H1`/`H2`-style ID must name which namespace it means — the observations file, or a review's own findings. The Fable report uses observation-file IDs throughout and tags review findings with their review of origin.

## Deferred — owner decisions taken 2026-08-29 (Q1-Q13, `progress-arc9.md` OWNER ANSWERS; two REVISED entries are the binding ones)

Every bullet below now carries its ruling. Nothing here is open for re-litigation; the "Owner decision" tails are history.

- **G4** (explicit `acquisition` mode) and **G3** (splitting the launch guard from the read-only import). G3 is inert without G4. Not needed for Docker; the only route to browserless import from a real profile on a Windows desktop. **RULED (Q10): SCHEDULED into this release — Arc 12 planning after Arc 11** (own plan, or one combined Arc 12 plan if the file map overlaps V9's settings/TUI files).
- **V9** — no TUI cookie-setup entry point outside the wizard. May be a deliberate scope decision. **RULED (Q10): SCHEDULED — Arc 12 planning.**
- **A2** — rejected as written. Only the narrowed regression-branch form is defensible. **RULED (Q13 REVISED): the narrowed form ships THIS release as a housekeeping task** — extend the browser-refresh rollback for the regression arm only (`before.ok() && after.state == verifyFailed`, `autocookies_profile.go:691`), the `verifyUnknown` arm (`:693`) explicitly NOT restoring on the browser path; the import-path gate at `autocookies.go:2072` grows a browser-path twin; cloned rollback test, both arms mutated. "Rejected as written" stands.
- **T3 full convergence** — the three identity keys currently cancel out; a partial fix is worse than none. *(Narrowed by Arc 5: the cross-platform half is CLOSED structurally by the jar partition (`72f4373`) — names cannot collide across platforms any more, and `37eb62e` closed the write-path leak that could have re-created the collision in `cookies.txt` before the jar ever saw it. What remains deferred is the within-platform write-path trio: `deduplicateAndFormat` by bare name (`autocookies_merge.go:94`, deliberately — it collapses youtube/google twins on purpose), `mergeCookieFiles` by name+domain (`:166`), `updateCookieFile` name-loose for updates / domain-strict for deletions (deliberate — grow-broadly/destroy-narrowly). Only `mergeCookieFiles`' key is unexamined. Still deferred; nothing in Arcs 2-6 touched it. Re-verified at `db1993a` by the arc-close review: `byName` bare-name key at `autocookies_merge.go:94` with the youtube-incumbent skip at `:107`, `cookieKey{name, domain}` at `:166`, `updateCookieFile` untouched — the narrowing holds, and with `essentialYouTubeCookies` and `essentialTwitchCookies` name-disjoint the bare-name key can no longer collide across platforms even on the write path.)* **RULED (Q13): SCHEDULED — one review task on `mergeCookieFiles`' name+domain key (`autocookies_merge.go:166`), fix only if a same-platform row can be lost; Arc 12 planning.**
- **Twitch liveness** — `oauth2/validate` returns 200 for an unentitled token. The equivalent probe is a playback-access-token request. *(Still deferred and still a different question from Arc 5's IRC finding: this is channel ENTITLEMENT; Task 7 fixes session IDENTITY at IRC connect. `checkTwitchAuth` (`refresh.go:3048` at `97cfecf`) remains the only tier-1 Twitch signal — validate-only, no refresh, no tier 2.)* **RULED (Q10): SCHEDULED — the Twitch tier-2 liveness probe, Arc 12 planning** (cookies + twitch). The docs' "Twitch has no tier-2 producer" (`data-and-storage.md` §Refresh Service) is the sentence that arc edits.
- **Arc 5's Twitch keepalive** — RESEARCHED, not built: no in-process refresh path exists for a web-session `auth-token`. Whether the browser path's periodic twitch.tv navigation already renews it is unmeasured; the settling observation (horizon timestamp before/after a browser refresh) costs one run. If it does not renew, the answer is the unbuilt re-auth ingest path (`docker-ingest-brief.md`). **RULED (Q5 + Q8):** (a) `AuthCookieHorizonFor` gets its surface — `youtubeAuthHorizon` / `twitchAuthHorizon` (ISO timestamps, never values; absent or zero when no auth cookie) on the `Cookies loaded` line at `cmd/moombox/services.go:375-376` and on the browser-refresh completion log — a housekeeping code item whose SAME commit must edit the three doc absence claims ("no UI or log carries a horizon today" in `data-and-storage.md` §Cookie Jar, `user-interfaces.md` §Facts these surfaces deliberately do not carry, and `platform-services.md` §Twitch Authentication, whose settling observation becomes "read the two log lines"); (b) **the re-auth ingest path is Arc 11, this release, after Arc 10:** `POST /api/cookies/import` (paste or upload), a dashboard affordance, the re-login prompt naming it; `docker-ingest-brief.md` is the starting brief (its post-Arc-8 appendix: `FlagManualRelogin` was deleted — re-add it WITH its caller). Arc 11 edits `SPEC.md` §Cookies "What is NOT built", `operations.md` "The re-authentication ingest path is UNBUILT" (and its "nothing else" route list), and `user-interfaces.md` §Cookies' route table and re-login copy.
- **A1 on Linux/Docker (the Job-Object reap)** — **RULED (Q9 REVISED, superseding the first "accept as documented residual"): BUILT this release, no Linux live gate.** A non-Job-Object reap on Linux: process group (`Setpgid` at launch), kill the group when the setup ends or is abandoned, same 60 s grace, same "release only where releasing cannot kill" rule, unit-tested with a fake process table; a user's bug report is the gate. Its own small arc after Arc 11 (touches `internal/cookies/autocookies_*launch*` + the setup flow). That arc must edit the "not reaped on Linux or in Docker" and "Off Windows nothing can say" sentences in `operations.md` §Browser Cookie Acquisition (absence-claim rule); the Linux launcher-handoff premise stays UNMEASURED.
- **Persistent-loss re-alarm cadence (progress-arc8.md §ARMING item 7)** — **DECIDED (Q1): back-off.** Replace the flat `livenessRefireWindow` re-fire with a per-platform back-off (base 30 min, x2, cap ~24 h, reset on a conclusive positive); tests mutate the base, the factor, the cap and the reset. Housekeeping, landed BEFORE the final whole-plan review and before arming, so the flip is later a one-constant change with no untested code behind it; §ARMING item 7's "at arming" step becomes "confirm the back-off landed". `data-and-storage.md` §Refresh Service's "has not been decided" sentence is arc-close finding F2 and is rewritten before the Arc 9 merge; the back-off commit rewrites the paragraph again to describe the shipped schedule.
- **The two Arc 2 evaluation items — RULED NO in Arc 8 (11(h)), recorded here so they are not re-raised as open.** (1) Cross-writer lost update: three readers of one file with no lock (`updateCookieFile`, the import merge, `automaticImportGuard`'s unlocked read); the window is seconds-wide only on the import path, and what is lost is a seconds-old rotation the next cycle repairs. (2) The browser path's no-rollback overwrite. Standing doctrine is against building mechanism without a profile, and neither has one. Cost if wrong: a rare lost rotation that self-heals within one cycle. Re-open only with a profile or a field report.
- **Twitch `login` diagnostic split (Arc 8 T9 concern 4)** — `ExpiredAuthCookiesFor`/`AuthCookieHorizonFor` cannot count an expiring `login` row, so there is no advance warning before `mergeCookieFiles` prunes it and chat goes anonymous; `noteMissingLogin` fires only afterwards, and only when a job runs. The cheap fix — split `authCookieNamesFor` so the DIAGNOSTIC predicates see a broader set than `HasAnyTwitchAuthCookie`, which drives the alarm — is a design change to a set `jar.go` states is deliberately shared. **RULED (Q7): folded into the horizon log, `authCookieNamesFor` NOT split** — the same housekeeping commit as Q5 logs `twitchLoginExpiry` on the startup line, and the browser-refresh merge logs ONE Warn when it prunes an expired `login` row while an `auth-token` remains. No UI, no new state; same doc-dependency rule (`user-interfaces.md`'s "An expired Twitch `auth-token` in particular has no other warning").
- **Twitch job-row "chat: anonymous" indicator (Arc 8 T10)** — a v19→v20 column for one Twitch-only bool plus a render in both UIs; the notification covers the operator-facing need without it, and the downloader is already shaped for it (`OnAuthDowngrade` fires once per job with a stable token). **RULED NO (Q3): "a symptom of a larger issue" — replaced by Arc 10, "Twitch chat credential lifecycle"** (worker + cookies + twitch), sequenced after the `mux-wait-chat-idle` merge. Design settled by Q3a-d and three fill-in rulings: a downgrade on ANY of the four routes marks Twitch as needing re-authorization (Q3a); the status shape is the EXISTING not-authenticated plus a reason string per route, every existing surface reused, no new state (Q3b); the mark feeds the automatic recovery path exactly as a validate-found dead token does (Q3c); the mark clears when the Twitch credential pair CHANGES in the jar (import, `R F`, browser refresh, cookies.txt edit + Recheck), every active Twitch chat session — including one that started cookieless — reconnects on that change, a credentialed `001` confirms green, a second refusal re-marks with the new reason (Q3d + i); `downgradeReported` and `authRefused` both reset on a credential change (ii); the video side's playback token stays fetched once per capture (iii). Facts at `8389009`: `chat.go:38-54, :258-263, :419-437`; `chat_irc.go:165`; `stream_processor_twitch.go:217`; today the callback only notifies, `authRefused` latches for the downloader's life, reconnects happen only on socket death. Arc 10 edits: `platform-services.md` §Anonymous Fallback ("every later reconnect in this job uses the anonymous handshake", "One notification per job", "Cost if wrong: a cookie repaired mid-job does not re-authenticate chat until the next job"), `SPEC.md` §Twitch IRC ("falls back to anonymous once, for the rest of the job"), `operations.md` §Credential Notifications (the Twitch row, the "sole producer of a recovery is `shouldFireRecovery`" sentence — the mark is a SECOND producer, and `handleRecoveryNeeded`'s "WHY conclusive holds" comment must be re-checked against it), `user-interfaces.md` ("The chat-downgrade notification is not a UI surface ... a state neither badge can show" — after Arc 10 the badge shows it; the reason-on-the-`unknown`-arm-only rule for `cookieIndicatorState`; "There is no cookie-status WebSocket event" if the mark is to reach the dashboard between fetches), `data-and-storage.md` §Refresh Service (`shouldFireRecovery`'s two cases gain a third producer; `authStatusChanged`'s gate must push the mark).
- **Twitch "capture started anonymous despite credentials in the jar" (Arc 8 T10 review, recommended build)** — with chat capture disabled a dead token is invisible at every level; the site is `Service.GetHLSMasterPlaylist` (`service.go:63`) or its caller at `stream_processor_twitch.go:410`; the premise to verify first is whether the playback-access-token reply says the token was honoured or "anonymous" must be inferred from the first stitched-ad marker. **RULED (Q6): folded into Arc 10** — the video-side twin of the chat mark, fed from the playback-token fetch at `Service.GetHLSMasterPlaylist`; Arc 10's FIRST task verifies the premise with ONE live probe; if the reply cannot tell, the arc records that and builds nothing on that side. Edits `platform-services.md` §IRC Chat's "A job with chat recording off gets no signal at all ... a recorded follow-up, not built".
- **DPAPI two-pass per-platform profile selection (Arc 8 H7's known limitation)** — one score for both platforms means YouTube-on-profile-A / Twitch-on-profile-B loses one platform on the DPAPI fallback, now deterministically and logged rather than as a coin flip. **CLOSED — NEVER (Q10 REVISED, 2026-08-29): one profile is the design.** The managed setup profile signs into both platforms; the fallback's one-profile rule and its log line are the documented behaviour (`data-and-storage.md` §Auto-Cookie Service, "Known limitation, deliberately not built here" — that sentence stays true and is not to be re-proposed).
- **Owner housekeeping rulings (Q2, Q4, Q11, Q12):** the `mux-wait-chat-idle` worktree (`376ff90`, one commit off `5850ec2`, never reviewed) merges THIS release, after the Arc 9 merge and before Arc 10 — rebase across the nine cookie merges by reading (never `checkout`; its `refresh.go:755` `LastCheck:` line must go, the field was deleted in Arc 9), full gates from ONE run, a fresh opus review with the mutate-every-claim rule, `--no-ff`, delete worktree AND branch; `RELEASE_NOTES.md` is drafted ONCE at the end from `git log v2.8.5..HEAD` per CLAUDE.md, F3 lines included, arming state stated plainly, owner-reviewed before the bump; both plan docs are tracked (`b5b6d07`) — the final whole-plan review deletes this one, the field-test plan goes after the owner has run it; `stash@{0}` was shown (`internal/monitor/decapi.go`, +10, already on `main` at `decapi.go:402-404`) and DROPPED — the stash list is empty.

## Self-review

**Spec coverage:** all 52 findings appear in the coverage map; V12 added from the 2026-08-25 owner requirement. **Placeholders:** none — every Arc 1 code step carries real code; Arcs 2-9 are explicitly scoped as separate plans rather than stubbed tasks. **Type consistency:** `sessionAuthFromBytes` (Task 3) is used under that name in Tasks 4 and 5; `HasAnyYouTubeAuthCookie` (Task 2) is used in Tasks 4 and 5; `FetchMembershipVideos`' new three-value signature (Task 4) matches its use in Task 6; `ObserveLiveness(platform string, loggedIn bool)` (Task 6) matches both call sites.

**Hardening pass (2026-08-25), and what it changed:**

- **Added Task 0, blocking.** The arc rested on an unverified empirical premise — that the two probe pages carry a readable login marker. If they do not, the arc fails safe but does nothing while *looking* shipped. Task 0's anonymous half needs no credentials and gates Tasks 3-6.
- **Added a staged rollout.** Tasks 3-6 land log-only; Task 7 connects notifications only after real traffic shows the verdict is correct. This removes the one way the arc can make things worse (a systematically-wrong verdict alerting every 30 minutes forever).
- **Replaced the acceptance test.** "Wait for the ticker" became five deterministic checks driven by `POST /api/cookies/recheck` → `CheckNow` → `doRefresh`, including a false-alarm check (A3) and a dedupe check (A5), plus the 30-minute cooldown caveat that would otherwise make a working fix read as broken.
- **Corrected the allocation claim in Task 3 — and this bullet itself was then corrected twice more.** The hardening pass moved Task 3 to package-level `[]byte` vars claiming the inline conversion allocates; measurement reversed that (inline consts are 0 allocs/op, pinned by `AllocsPerRun`); the reversal's stated mechanism (a universal 32-byte ceiling) was then also found wrong by the Task 4 review's full const/var × length matrix. Final state: inline string consts, `TestSessionAuthFromBytesDoesNotAllocate` + `TestLivenessVerdictDoesNotAllocate`, and the matrix recorded in `session_auth_test.go:201-229`. This claim was wrong twice in opposite directions — do not restate it without the matrix.
- **Verified, no longer assumptions:** `utils.FetchBody` errors on non-200 (`http.go:103-105`) and caps at 50 MB (`:16,107`); `noopLogger` already exists (`internal/youtube/service_test.go:10`) and must be reused, not redeclared; the `MOOMBOX_LIVE_YT_TEST=1` gate is the house convention (`extraction_live_test.go:30`).
- **Added preconditions, stop-and-ask triggers, and rollback**, including the Task 2↔Task 4 coupling (revert in reverse order) and Task 4's atomicity requirement.

**Remaining soft spot:** Task 6 Step 5's scheduling location is described rather than coded, because it depends on the shape of the refresh goroutine at execution time. The constraints that matter are the mandated `defer recover()` and that it must not fire when a membership observation is recent.
