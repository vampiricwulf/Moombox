# Housekeeping H2 — the cookie-side items Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the eight cookie-side housekeeping items (R1-R8) the owner ruled during the Q1-Q13 sweep and the Arc 10/12a reviews carried, each with the doc absence-claims it flips, on branch `cookie-housekeeping-h2` cut from `main` after Arc 12c merges.

**Architecture:** Seven tasks over five packages. `internal/cookies` gets a per-platform back-off behind the still-disarmed liveness pilot (R1), a contract pin and a dedupe assertion on `refresh.go` (R4, R6), horizon/expiry fields on two log lines plus one prune Warn (R2), and a browser-path rollback that takes the regression arm only (R3). `internal/tui` folds the feedback triple into one struct and renders the wizard's accepted verdict inside the overlay (R5, R8). `cmd/moombox` gains an AST test pinning the deferred re-check sites (R7). Nothing arms the pilot; no new goroutine; no REST or Web change except Task 7's ruled exception (one additive payload key, one exported JS helper, and the toast subjects that read them).

**Tech Stack:** Go 1.26, `log/slog` (`cmd/moombox` only), `go/ast` + `go/parser` for the call-site test, `charmbracelet/lipgloss` for one new style. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-03-housekeeping-h2-design.md`

## Global Constraints

Every task's requirements implicitly include this section.

- **`const livenessRecoveryArmed = false`** (`internal/cookies/refresh.go:771`) stays false. Nothing here arms it. R1 shapes WHEN a re-fire would happen and stays inert until arming.
- **`cmd/moombox/main.go:276-278`** (the `AutoEnabled && len(Platforms) > 0` → `SetExpectedPlatforms` seed) is **no-touch**.
- **No cookie value or token in any log, error, or UI string.** The new fields are TIMESTAMPS only. Never read, print, or open any real cookie file or browser profile while developing (`D:\Moombox\cookies.txt`, `cookies.sqlite`, real browser profiles). Tests build synthetic files in `t.TempDir()`.
- **`authCookieNamesFor` is NOT split** (Q7). `youtubeAuthCookieNames` and `twitchAuthCookieNames` are unchanged. `AuthCookieHorizonFor`'s existing callers are NOT swept.
- **`shouldFireRecovery` is unchanged.** No REST route, no Web asset, no config key, no database column.
- **Every goroutine gets an inline `defer func() { if r := recover(); ... }()`.** None is added here; if one appears, it carries the recover.
- **The logger is the anonymous interface** repeated in place — `Debug`/`Info`/`Warn`/`Error`, each `(msg string, args ...any)`. Never extracted to a named interface.
- **House comment style.** The comment blocks below are the SUBSTANCE each site must carry, written compactly. Match the surrounding file's density when you write them in; never drop a stated reason.
- **LF line endings, byte-wise,** on every file touched. Verify with `perl -0777 -ne 'print tr/\r//' <file>` (must print `0`). Never `grep -c $'\r'` inside a double-quoted `$( )` — it returns the LINE count.
- **Every `go build` / `go test` / `go vet` is prefixed `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`.**
- **Gates, at the end of every task, before the commit:**
  ```bash
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp GOOS=linux GOARCH=amd64 go build ./...
  gofmt -l internal/ cmd/       # must print nothing
  GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./...
  ```
  Expected: **27 ok / 0 FAIL / 4 no-test-files**.
- **The two known timing flakes.** `TestTightenCookieDirOncePermanentFailureCostIsOnePerWrite` and its sibling in `internal/cookies` can hit the whole-suite 10-minute budget under full-tree load. On a FAIL in `internal/cookies` only, re-run that package alone ONCE (`go test -count=1 ./internal/cookies/...`) and record both results. A second failure is a real failure.
- **Mutation discipline.** Every assertion added is checked by a NAMED mutation: make the edit, watch the named test fail, revert. Tables are per task. A test that survives its mutation is not finished.
- **Every commit message ends with:**
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
  ```

**Line numbers** were read at `78590d2` and re-verified at `8558f5f` (the 12c tree's head) on `cookie-arc12c-acquisition-mode` — every cited anchor matched there. H2 branches off `main` AFTER Arc 12c merges, so `internal/cookies/autocookies.go` and `cmd/moombox/services.go` will have drifted. **Re-derive by SYMBOL** — each task's Files block gives the symbol or grep string.

**Review note (2026-09-03, worktree at `8558f5f`):** every code block in Tasks 1, 2, 3, 4 and 6 was applied verbatim and its tests run; the named mutants below were run where marked *(verified)*. Two mutation rows that did NOT hold as first written were corrected (Task 1 rows 1 and 6) and one missing import was added (Task 2 Step 2).

### Task order

| # | Item(s) | Files | Why its own task |
|---|---|---|---|
| 1 | R1 back-off | `refresh.go`, `refresh_liveness_test.go`, `data-and-storage.md` | A behaviour change with four mutants of its own |
| 2 | R4 contract pin + R6 ARMING row 12 | `refresh_threestate_test.go`, `refresh_twitch_mark_test.go`, `data-and-storage.md` | Test-and-doc only; rejectable without touching Task 1 |
| 3 | R2 horizons + login expiry | `jar.go`, `autocookies.go`, `autocookies_merge.go`, `cmd/moombox/services.go`, 3 spec docs | New log surface + a new Warn; three absence claims flip together |
| 4 | R3 browser-path rollback | `autocookies.go`, `autocookies_profile.go`, `data-and-storage.md`, remediation plan | The only write-path behaviour change |
| 5 | R5 feedback fold + R8 wizard verdict | `internal/tui` (7 files + 7 test files) | One package, one review |
| 6 | R7 AST call-site test | `cmd/moombox` (test only) | Test only |
| 7 | R9 boot line + post-flight mechanism | `autocookies.go`, `cmd/moombox/services.go`, `internal/tui` (2 files), `internal/web/routes` (1 file), `web/public` (2 files), 3 spec docs, 2 plan docs | The only task that crosses the REST/web line (ruled exception, 2026-09-03), and the only one that must follow Task 5 |

Tasks 3, 4 and 7 all touch `autocookies.go` (3 and 7 touch `cmd/moombox/services.go` as well), and Task 7 reads `a.feedback.msg`, which is Task 5's shape. Run in order; do not parallelise 3, 4 or 7 with each other, and run 7 after 5.

---

### Task 1: R1 — the tier-2 re-alarm backs off per platform

Owner ruling Q1: re-alarm at 30 min, then double (1 h, 2 h, 4 h …), capped at ~24 h, resetting when auth is seen to return. The pilot stays disarmed, so the schedule is mutation-tested now and arming later is a one-constant change with no untested code behind it.

**Files:**
- Modify: `internal/cookies/refresh.go` — the const block holding `livenessRefireWindow` (`:42-77`); the `RefreshService` liveness fields (after `lastLivenessKnown`, `:595-610`); `NewRefreshService`'s map initialisers (`:808-810`); `recordLiveness` (`:1026-1063`); `noteRecoveryDecided`'s doc (`:1101-1114`)
- Modify: `docs/spec/data-and-storage.md:843`, `:850`, `:867`
- Modify: `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2014`
- Test: `internal/cookies/refresh_liveness_test.go` (gains five tests: four schedule tests + one literal pin of the three numbers)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `const livenessRefireFactor = 2`; `const livenessRefireCap = 24 * time.Hour`; field `livenessRefireBackoff map[string]time.Duration` on `RefreshService`; `func (rs *RefreshService) livenessRefireWindowFor(platform string) time.Duration`; `func (rs *RefreshService) escalateLivenessRefire(platform string)`; `func (rs *RefreshService) resetLivenessRefire(platform string)` — all three called with `rs.mu` held. `livenessRefireWindow` keeps its name, its value and its doc; it is now the BASE, not the whole schedule.

**The schedule, walked:**

| Event | Window consulted | Verdict | Backoff after |
|---|---|---|---|
| first signed-out verdict `t0` | — (no stamp) | FIRE | 30 min |
| `t0 + 29m` | 30 min | suppressed | 30 min |
| `t0 + 30m` | 30 min | FIRE (the 30-min re-alarm) | 1 h |
| `t0 + 1h30m` | 1 h | FIRE | 2 h |
| … | | | 4 h, 8 h, 16 h, **24 h** (capped) |
| any conclusive `loggedIn=true` | — | silent | reset to base |

`escalateLivenessRefire` sets the base on the FIRST fire and doubles thereafter — that is what puts the first re-alarm at 30 minutes and starts the doubling after it, matching the owner's "30 min, then double".

**The reset is narrow.** It clears the ESCALATION only, never `lastRecoveryDecided`: clearing the stamp would let a healthy verdict from the first channel of a feed cycle swallow a dead verdict from the second, which is why those are two maps. And it lives in `recordLiveness` alone — a tier-1 recovery does not reset the tier-2 escalation. Bounded: on an install with channels the membership probe delivers a conclusive `loggedIn=true` within one feed cycle of auth returning.

- [ ] **Step 1: Write the five failing tests**

Append to `internal/cookies/refresh_liveness_test.go` after `TestRecordLivenessSeparatesPlatforms`. The four schedule tests drive the same YouTube seam the existing liveness tests use — `recordLiveness("youtube", …)` on `NewRefreshService(jarWithAuth(t), 0, nopLogger{})` — so they extend `TestRecordLivenessRefiresAfterTheWindow` (the BASE case), `TestRecordLivenessDedupesAcrossChannels` and `TestRecordLivenessSeparatesPlatforms` rather than standing up a new fixture. They use the constants SYMBOLICALLY, which is what makes them schedule tests and not number tests — so the fifth pins the numbers themselves (review finding: with only the four, `livenessRefireWindow = 15 * time.Minute` and `livenessRefireCap = 20 * time.Hour` both survived).

```go
// TestRecordLivenessRefireWindowDoublesAfterEachAlarm is the FACTOR.
//
// TestRecordLivenessRefiresAfterTheWindow above is the BASE case and is
// unchanged: the first re-alarm lands one livenessRefireWindow after the first
// alarm. This picks up where that stops. "30 min, then double" means the
// doubling begins AFTER the first re-alarm, so a session dead for 90 minutes
// has produced three alarms, not four.
//
// Mutation: drop `next *= livenessRefireFactor` from escalateLivenessRefire.
// Every re-alarm then lands on the flat window and an armed tier 2 pages an
// operator 48 times a day, forever, for one loss.
func TestRecordLivenessRefireWindowDoublesAfterEachAlarm(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()

	if due, _ := rs.recordLiveness("youtube", false, t0); !due {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	if due, _ := rs.recordLiveness("youtube", false, t0.Add(livenessRefireWindow)); !due {
		t.Fatal("premise broken: the first re-alarm must land one base window later")
	}

	second := t0.Add(livenessRefireWindow)
	if due, _ := rs.recordLiveness("youtube", false, second.Add(livenessRefireWindow)); due {
		t.Error("a verdict one BASE window after the first re-alarm warranted recovery — the window did not double")
	}
	if due, _ := rs.recordLiveness("youtube", false, second.Add(2*livenessRefireWindow)); !due {
		t.Error("a verdict two base windows after the first re-alarm did not warrant recovery — the doubled window is wrong or the schedule latched")
	}
}

// TestRecordLivenessRefireWindowStopsAtTheCap is the CAP. Uncapped doubling
// reaches a fortnight in eleven alarms, and a session that recovered two weeks
// ago is still suppressed.
//
// Mutation: delete the `if next > livenessRefireCap` clamp — the window after
// the sixth alarm is then 32 h.
func TestRecordLivenessRefireWindowStopsAtTheCap(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	now := time.Now()

	// A full DAY between alarms is longer than every window on the way up, so
	// each one fires and each one escalates: 30m, 1h, 2h, 4h, 8h, 16h, 24h.
	for i := range 8 {
		if due, _ := rs.recordLiveness("youtube", false, now.Add(time.Duration(i)*24*time.Hour)); !due {
			t.Fatalf("alarm %d did not fire although a full day had passed — the schedule is not advancing", i)
		}
	}

	rs.mu.RLock()
	got := rs.livenessRefireBackoff["youtube"]
	rs.mu.RUnlock()
	if got != livenessRefireCap {
		t.Errorf("the back-off settled at %v, want the %v cap", got, livenessRefireCap)
	}

	// The cap is a real window, not just a stored number.
	last := now.Add(7 * 24 * time.Hour)
	if due, _ := rs.recordLiveness("youtube", false, last.Add(livenessRefireCap-time.Minute)); due {
		t.Error("a verdict inside the capped window warranted recovery")
	}
	if due, _ := rs.recordLiveness("youtube", false, last.Add(livenessRefireCap)); !due {
		t.Error("a verdict a full cap later did not warrant recovery — the schedule latched at the cap")
	}
}

// TestRecordLivenessRefireResetsWhenAuthReturns is the RESET. A session that
// died, escalated for a day, was repaired and died again is a NEW loss;
// reporting it on the old schedule holds the alarm for up to 24 hours.
//
// The reset touches the ESCALATION only — lastRecoveryDecided is left standing
// (see recordLiveness), so this also states that boundary. The final verdict is
// therefore placed exactly one BASE window after the second alarm's stamp: the
// base schedule fires there and the escalated one (2x base) does not, which is
// the only interval that tells the two apart.
//
// Mutation: drop resetLivenessRefire from the loggedIn branch.
func TestRecordLivenessRefireResetsWhenAuthReturns(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()

	if due, _ := rs.recordLiveness("youtube", false, t0); !due {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	// Stamps at t0+base and escalates the window to 2x base.
	if due, _ := rs.recordLiveness("youtube", false, t0.Add(livenessRefireWindow)); !due {
		t.Fatal("premise broken: the first re-alarm must land one base window later")
	}

	// Auth comes back a minute later. Silent, and it moves no stamp — only the
	// escalation.
	if due, _ := rs.recordLiveness("youtube", true, t0.Add(livenessRefireWindow+time.Minute)); due {
		t.Fatal("a logged-in observation warranted recovery")
	}

	// One BASE window after the last stamp. On the base schedule that fires;
	// on the escalated one it is still half a window short.
	if due, _ := rs.recordLiveness("youtube", false, t0.Add(2*livenessRefireWindow)); !due {
		t.Error("the re-alarm after a repair still wanted the escalated window — a fresh loss must be reported on the base schedule")
	}
}

// TestRecordLivenessRefireBackoffIsPerPlatform: the schedule is keyed like the
// three maps beside it. TestRecordLivenessSeparatesPlatforms pins the same
// property for the dedupe STAMP; this pins it for the WINDOW, which a single
// shared time.Duration would collapse.
//
// Mutation: make livenessRefireBackoff a plain time.Duration field.
func TestRecordLivenessRefireBackoffIsPerPlatform(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	now := time.Now()

	for i := range 6 {
		rs.recordLiveness("youtube", false, now.Add(time.Duration(i)*24*time.Hour))
	}

	last := now.Add(5 * 24 * time.Hour)
	if due, _ := rs.recordLiveness("twitch", false, last); !due {
		t.Fatal("twitch's first logged-out verdict did not warrant recovery")
	}
	if due, _ := rs.recordLiveness("twitch", false, last.Add(livenessRefireWindow)); !due {
		t.Error("twitch's re-alarm waited on YouTube's escalated window — the back-off is not per platform")
	}
}

// TestLivenessRefireScheduleIsTheRuledNumbers pins the three constants to the
// owner's numbers (2026-08-29): 30 min, then double, capped at 24 h.
//
// The four schedule tests above use the constants SYMBOLICALLY — each one
// passes just as well for a 15-minute base or a 20-hour cap — so this is the
// only thing in the file that fails when a number drifts. It is deliberately a
// literal pin and nothing more: the SHAPE of the schedule is the other four
// tests' job, and restating it here with numbers would be a second copy of the
// same arithmetic to keep in step.
//
// Mutations: livenessRefireWindow = 15 * time.Minute; livenessRefireFactor = 3;
// livenessRefireCap = 20 * time.Hour. Each fails exactly one line below.
func TestLivenessRefireScheduleIsTheRuledNumbers(t *testing.T) {
	if livenessRefireWindow != 30*time.Minute {
		t.Errorf("livenessRefireWindow = %v, want 30m — the owner's base", livenessRefireWindow)
	}
	if livenessRefireFactor != 2 {
		t.Errorf("livenessRefireFactor = %d, want 2 — \"then double\"", livenessRefireFactor)
	}
	if livenessRefireCap != 24*time.Hour {
		t.Errorf("livenessRefireCap = %v, want 24h — \"capped near a day\"", livenessRefireCap)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestRecordLivenessRefire ./internal/cookies/`
Expected: FAIL to COMPILE — `rs.livenessRefireBackoff undefined`, `livenessRefireCap undefined`.

- [ ] **Step 3: Add the two constants**

In `internal/cookies/refresh.go`, in the same `const` block, immediately below `livenessRefireWindow = 30 * time.Minute` (whose doc comment is unchanged):

```go
	// livenessRefireFactor and livenessRefireCap turn livenessRefireWindow
	// above into a per-platform back-off, decided by the owner 2026-08-29:
	// alarm, re-alarm 30 minutes later, then double — 1 h, 2 h, 4 h — capped
	// near a day, and start over when auth returns.
	//
	// WHY A SCHEDULE. Tier 1 notifies once per process for a session already
	// dead at startup: shouldFireRecovery's witnessed-transition arm needs
	// prevAuth to have been true, and the first conclusive negative clears it.
	// An armed tier 2 has the opposite problem — it re-fires for as long as
	// the session stays dead, so a flat window is 48 notifications a day for
	// one loss. The back-off keeps the first hour responsive and then gets out
	// of the way.
	//
	// INERT UNTIL ARMING: recordLiveness computes the schedule and
	// ObserveLiveness logs the answer, but livenessRecoveryArmed is false, so
	// nothing is called. That is the point of landing it now — the schedule is
	// mutation-tested here and arming stays a one-constant change.
	livenessRefireFactor = 2
	livenessRefireCap    = 24 * time.Hour
```

- [ ] **Step 4: Add the state field**

In the `RefreshService` struct, immediately after `lastLivenessKnown` and its doc:

```go
	// livenessRefireBackoff is the interval that must pass before this
	// platform's next signed-out verdict may clear the dedupe again — the
	// schedule livenessRefireFactor / livenessRefireCap describe.
	//
	// A FOURTH map beside the other three rather than a field on
	// livenessRecord (a uint8 enum with no room in it) and rather than a
	// scalar, which would let a YouTube session dead all day delay Twitch's
	// FIRST alarm by 24 hours
	// (TestRecordLivenessRefireBackoffIsPerPlatform). A missing entry reads as
	// the base window, so the zero value is the right answer.
	//
	// Written by escalateLivenessRefire and resetLivenessRefire, read by
	// livenessRefireWindowFor; all three take rs.mu from their caller.
	livenessRefireBackoff map[string]time.Duration
```

In `NewRefreshService`, beside the other three initialisers (gofmt re-aligns the literal):

```go
		lastLivenessObserved:  make(map[string]time.Time),
		lastRecoveryDecided:   make(map[string]time.Time),
		lastLivenessKnown:     make(map[string]livenessRecord),
		livenessRefireBackoff: make(map[string]time.Duration),
```

Run `gofmt -w internal/cookies/refresh.go` after this edit: the new key is longer than its siblings, so the whole literal re-aligns and `gofmt -l` flags the file until it has.

- [ ] **Step 5: Add the three helpers**

Immediately below `recordLiveness`, above `recordInconclusiveLiveness`:

```go
// livenessRefireWindowFor returns how long must pass since this platform's last
// cleared dedupe before another signed-out verdict may clear it again: the base
// until something has fired, the escalated window after. Callers hold rs.mu.
func (rs *RefreshService) livenessRefireWindowFor(platform string) time.Duration {
	if w := rs.livenessRefireBackoff[platform]; w > 0 {
		return w
	}
	return livenessRefireWindow
}

// escalateLivenessRefire advances one platform's back-off after a verdict that
// cleared the dedupe.
//
// The FIRST call sets the base rather than doubling it, and that is what puts
// the first re-alarm 30 minutes after the first alarm — the owner's "30 min,
// then double". Doubling on the first call would put it at an hour and lose
// the responsive window entirely. Callers hold rs.mu.
func (rs *RefreshService) escalateLivenessRefire(platform string) {
	if rs.livenessRefireBackoff == nil {
		rs.livenessRefireBackoff = make(map[string]time.Duration)
	}
	next := rs.livenessRefireBackoff[platform]
	if next == 0 {
		next = livenessRefireWindow
	} else {
		next *= livenessRefireFactor
	}
	if next > livenessRefireCap {
		next = livenessRefireCap
	}
	rs.livenessRefireBackoff[platform] = next
}

// resetLivenessRefire puts one platform back on the base schedule.
//
// One caller — recordLiveness's conclusive-LoggedIn branch — and it clears the
// ESCALATION only. Clearing lastRecoveryDecided here would let a healthy
// verdict from one channel swallow a dead verdict from the next in the same
// cycle, which is the whole reason those are two maps.
//
// A tier-1 recovery does NOT reach here: noteRecoveryDecided stamps the dedupe
// and nothing else, by the one-directional rule it has always followed. On an
// install with channels the membership probe delivers a conclusive LoggedIn
// within one feed cycle of auth returning. Callers hold rs.mu.
func (rs *RefreshService) resetLivenessRefire(platform string) {
	delete(rs.livenessRefireBackoff, platform)
}
```

- [ ] **Step 6: Wire the three steps into `recordLiveness`**

Replace:

```go
	if loggedIn {
		// Positive evidence is silent, and must not touch lastRecoveryDecided:
		// stamping it here would let a healthy verdict swallow a dead one
		// arriving a moment later from another channel in the same cycle.
		return false, notable
	}
	if last, ok := rs.lastRecoveryDecided[platform]; ok && now.Sub(last) < livenessRefireWindow {
		return false, notable
	}
	if rs.lastRecoveryDecided == nil {
		rs.lastRecoveryDecided = make(map[string]time.Time)
	}
	rs.lastRecoveryDecided[platform] = now
	return true, notable
```

with:

```go
	if loggedIn {
		// Positive evidence is silent, and must not touch lastRecoveryDecided:
		// stamping it here would let a healthy verdict swallow a dead one
		// arriving a moment later from another channel in the same cycle.
		//
		// It DOES clear the back-off. "Auth came back" is the reset condition
		// the owner named, and a session that dies again after a repair is a
		// new loss to report on the base schedule rather than yesterday's
		// escalated one. See resetLivenessRefire for why this is the
		// escalation only and not the stamp.
		rs.resetLivenessRefire(platform)
		return false, notable
	}
	if last, ok := rs.lastRecoveryDecided[platform]; ok && now.Sub(last) < rs.livenessRefireWindowFor(platform) {
		return false, notable
	}
	if rs.lastRecoveryDecided == nil {
		rs.lastRecoveryDecided = make(map[string]time.Time)
	}
	rs.lastRecoveryDecided[platform] = now
	// After the window CHECK above (its position relative to the stamp is
	// immaterial), so this verdict was judged against the window in force when
	// it arrived. Escalating before the check would judge every alarm against
	// the window meant for the one after it — and a SUPPRESSED verdict would
	// grow the window too, so the base re-alarm would never land at all.
	rs.escalateLivenessRefire(platform)
	return true, notable
```

- [ ] **Step 7: Correct `noteRecoveryDecided`'s doc**

Append to its doc comment, after "…livenessRefireWindow has the accounting.":

```go
//
// It stamps and does NOT escalate. The back-off counts TIER-2 alarms; a tier-1
// fire consumes one tier-2 window without growing the next, which is the
// conservative direction — the platform stays on the shorter schedule.
```

- [ ] **Step 8: Run to verify they pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestRecordLiveness|TestLivenessRefire|TestLivenessRecoveryPilotIsDisarmed|TestObserveLiveness|TestFallback|TestTierOne' ./internal/cookies/ -v`
Expected: PASS, including all five pre-existing `TestRecordLiveness*` unchanged.

- [ ] **Step 9: Mutations to run**

Rows marked *(verified)* were run by the plan review in a worktree at `8558f5f` with this task's code applied verbatim.

| # | Mutation | Test that must fail |
|---|---|---|
| 1 | `livenessRefireWindow = 15 * time.Minute` | `TestLivenessRefireScheduleIsTheRuledNumbers` — NOT the base test, which uses the constant symbolically and survives this *(verified: the base test survived; the pin is what catches it)* |
| 1b | `livenessRefireCap = 20 * time.Hour` | `TestLivenessRefireScheduleIsTheRuledNumbers` — the cap test's `got != livenessRefireCap` is symbolic and survives a wrong value (a 48 h cap happens to trip its one-day premise; a 20 h cap does not) |
| 1c | `livenessRefireFactor = 3` | `TestLivenessRefireScheduleIsTheRuledNumbers`; `TestRecordLivenessRefireWindowDoublesAfterEachAlarm` also fails (its "two base windows later" verdict lands inside a tripled window) |
| 2 | drop `next *= livenessRefireFactor` | `TestRecordLivenessRefireWindowDoublesAfterEachAlarm` *(verified; the cap test fails too)* |
| 3 | delete the `if next > livenessRefireCap` clamp | `TestRecordLivenessRefireWindowStopsAtTheCap` *(verified)* |
| 4 | drop `rs.resetLivenessRefire(platform)` from the `loggedIn` branch | `TestRecordLivenessRefireResetsWhenAuthReturns` *(verified)* |
| 5 | `livenessRefireBackoff time.Duration` (scalar) | `TestRecordLivenessRefireBackoffIsPerPlatform` |
| 6 | move `rs.escalateLivenessRefire(platform)` ABOVE the `if last, ok := rs.lastRecoveryDecided[platform]` window check | `TestRecordLivenessRefiresAfterTheWindow` (a suppressed verdict escalates too, so the verdict at `t0 + base` is judged against 2 h and refused) *(verified; all four schedule tests fail)*. Swapping the escalate with the STAMP, as an earlier draft of this row said, changes nothing and no test fails — the window is consulted before both *(verified survived)* |

- [ ] **Step 10: Flip the three doc sentences**

`docs/spec/data-and-storage.md:843` — replace the last sentence of the paragraph. Old:

```
The owner ruled a back-off — 30 min, doubling, capped near 24 h, reset when auth returns — and until that lands the code still re-fires every `livenessRefireWindow`.
```

New:

```
The owner ruled a back-off and it is BUILT: the first re-alarm lands one `livenessRefireWindow` (30 min) after the first alarm, each alarm after that doubles the window (`livenessRefireFactor`) up to a `livenessRefireCap` of 24 hours, and a conclusive signed-in observation resets the platform to the base. The reset is the escalation only — `lastRecoveryDecided` is left standing, because clearing it would let a healthy verdict from one channel swallow a dead one from the next channel in the same cycle. A tier-1 fire stamps the dedupe without escalating, so a platform is never pushed onto a longer schedule by the check that predates this signal.
```

`:845` — the sentence introducing the table: `Three maps, deliberately separate` → `Four maps, deliberately separate`.

`:850` — in the `lastRecoveryDecided` table row, replace `` `recordLiveness`'s `livenessRefireWindow` check `` with `` `recordLiveness`'s back-off check (`livenessRefireWindowFor`) ``. Rest of the row unchanged.

`:851` — after the `lastLivenessKnown` row, add a fourth row:

```
| `livenessRefireBackoff` | `escalateLivenessRefire` after every verdict that clears the dedupe (the first sets the base, each later one doubles, clamped at `livenessRefireCap`); `resetLivenessRefire` from a conclusive `LoggedIn` verdict — the ONLY writer that ever shrinks it | `livenessRefireWindowFor`, which `recordLiveness` consults instead of the flat constant. A missing entry reads as the base window. A tier-1 fire (`noteRecoveryDecided`) stamps the dedupe without touching this map |
```

`:867` — replace the bullet. Old:

```
- `livenessRefireWindow` = 30 minutes (its own constant on purpose — it is neither the notification cooldown in `wireMonitorCallbacks` nor `defaultRefreshInterval`, however the three numbers line up today).
```

New:

```
- `livenessRefireWindow` = 30 minutes — the BASE of the per-platform tier-2 back-off, doubling per alarm (`livenessRefireFactor` = 2) to a `livenessRefireCap` of 24 hours. Its own constant on purpose: it is neither the notification cooldown in `wireMonitorCallbacks` nor `defaultRefreshInterval`, however the three numbers line up today.
```

- [ ] **Step 11: Close the remediation plan's F2 bullet**

`docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2014` — append to the "Persistent-loss re-alarm cadence" bullet:

```
 ***(BUILT — H2 Task 1: `livenessRefireFactor` = 2, `livenessRefireCap` = 24 h and `livenessRefireBackoff` beside the three liveness maps; `livenessRefireWindowFor` / `escalateLivenessRefire` / `resetLivenessRefire` in `recordLiveness`; five mutation-checked tests in `refresh_liveness_test.go` — four schedule tests with `TestRecordLivenessRefiresAfterTheWindow` as the base case, plus `TestLivenessRefireScheduleIsTheRuledNumbers` pinning 30 min / ×2 / 24 h literally. Pilot still disarmed. §ARMING item 7's step is now "confirm the back-off landed".)***
```

- [ ] **Step 12: Verify line endings and run the gates**

```bash
for f in internal/cookies/refresh.go internal/cookies/refresh_liveness_test.go docs/spec/data-and-storage.md docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md; do
  printf '%s: ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```
Expected: `0` for every file. Then the full gate block.

- [ ] **Step 13: Commit**

```bash
git add internal/cookies/refresh.go internal/cookies/refresh_liveness_test.go docs/spec/data-and-storage.md docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md
git commit -m "feat(cookies): the tier-2 re-alarm backs off per platform

30 min, doubling, capped at 24 h, reset on a conclusive signed-in
observation. The pilot stays disarmed, so the schedule is testable by
mutation now and arming later is a one-constant change.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN"
```

---

### Task 2: R4 + R6 — the `authStatusChanged` contract is pinned, and the Twitch mark's dedupe stamp is asserted

**R4 — the missing row, named precisely.** `TestAuthStatusChangedGateCoversEverySurfaceInput` (`refresh_threestate_test.go:453-507`) has seven rows and a base whose two verdicts are `RefreshFailed`. Every row that moves a platform's `Authenticated` boolean ALSO moves that platform's verdict ("youtube signed back in" sets both). No row moves either boolean ON ITS OWN — so the mutants "delete `next.YouTubeAuthenticated != prev.YouTubeAuthenticated`" and its Twitch twin both survive the entire tree today; the verdict comparisons cover for them. The other two tests do not close it: `TestOnAuthChangeOnlyFiresWhenAuthFlagsChange` (`refresh_transitions_test.go:320`) moves boolean and verdict together off a zero `AuthStatus`, and `refresh_twitch_mark_test.go:392` pins the EXCLUSION half.

**R6 — ARMING row 12.** `NoteTwitchAuthLoss`'s fire path calls `rs.noteRecoveryDecided("twitch", time.Now())` (`refresh.go:1235`) and nothing asserts it. The stamp is a map write, not a callback, so it and the tier-2 suppression it produces are fully assertable while DISARMED. Only the double-FIRE suppression needs arming; that half stays for the arming commit.

**Files:**
- Test: `internal/cookies/refresh_threestate_test.go` — two table rows + one doc paragraph
- Test: `internal/cookies/refresh_twitch_mark_test.go` — one new test
- Modify: `docs/spec/data-and-storage.md:857` (the CONTRACT paragraph — restated, not widened)
- **Not a commit:** the controller writes the ARMING row 12 update into the gitignored ledger `progress-arc8.md`. Text in Step 6.

**Interfaces:** Consumes nothing. Produces nothing new — `authStatusChanged`'s body (`refresh.go:447-454`) is UNCHANGED.

- [ ] **Step 1: Write the two failing rows (R4)**

In `refresh_threestate_test.go`, append to the doc comment of `TestAuthStatusChangedGateCoversEverySurfaceInput`:

```go
// The two "alone" rows close the gap the rest leave: every other row that
// moves an Authenticated boolean also moves that platform's verdict, so a gate
// with either boolean comparison DELETED still passes this table — the verdict
// comparison covers for it. They are synthetic on purpose. The gate is a pure
// function over six fields and its contract is that each of the six is a
// surface input in its own right; whether today's producers can move one
// without the other is a fact about the producers, and the day one of them can
// is not the day to discover the comparison was never pinned.
```

Add these rows after `"youtube cookies disappeared"` and before `"only the error wording changed"`:

```go
		{
			"youtube authenticated flipped and nothing else did",
			with(func(s *AuthStatus) { s.YouTubeAuthenticated = true }),
			true,
		},
		{
			"twitch authenticated flipped and nothing else did",
			with(func(s *AuthStatus) { s.TwitchAuthenticated = true }),
			true,
		},
```

- [ ] **Step 2: Write the failing test (R6)**

`refresh_twitch_mark_test.go` does not import `time` today (its imports are `context`, `fmt`, `net/http`, `os`, `path/filepath`, `strings`, `sync`, `testing`), and the test below uses `time.Second`. Add `"time"` to the import block first — without it the package fails to compile (review finding, verified at `8558f5f`).

Append to `refresh_twitch_mark_test.go` after `TestTwitchMarkFiresAuthChangeOnAVerdictTransitionOnly`:

```go
// TestTwitchMarkStampsTheSharedRecoveryDedupe is ARMING checklist row 12, the
// half assertable while the pilot is DISARMED.
//
// NoteTwitchAuthLoss's fire path calls noteRecoveryDecided("twitch", …). That
// stamp looks inert today — livenessRecoveryArmed is false — and was therefore
// asserted by nothing. It is not inert: recordLiveness consults the same map,
// so the stamp is what stops an ARMED tier 2 firing a second recovery for a
// loss the chat mark already raised. Only the double-FIRE needs arming.
//
// Two assertions, and the second is the one that matters. "The map has an
// entry" is satisfied by any write; "a signed-out verdict one second later is
// refused" only by a stamp the tier-2 dedupe actually reads.
//
// Mutation: delete `rs.noteRecoveryDecided("twitch", time.Now())` from
// NoteTwitchAuthLoss. Both fail. Under it, arming pages the operator twice for
// one chat downgrade — once from the mark, once from the next probe.
func TestTwitchMarkStampsTheSharedRecoveryDedupe(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "", http.StatusOK)
	rs.OnRecoveryNeeded = func(string) {}

	// A healthy first pass, so the mark below is a WITNESSED fall and its fire
	// path is reached at all. It must stamp nothing itself.
	rs.doRefresh(context.Background())
	rs.mu.RLock()
	_, stampedByThePass := rs.lastRecoveryDecided["twitch"]
	rs.mu.RUnlock()
	if stampedByThePass {
		t.Fatal("premise broken: a healthy pass stamped the recovery dedupe for twitch")
	}

	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)

	rs.mu.RLock()
	stamp, ok := rs.lastRecoveryDecided["twitch"]
	rs.mu.RUnlock()
	if !ok {
		t.Fatal("the Twitch mark fired recovery without stamping lastRecoveryDecided — once the pilot is armed, the next membership-probe verdict raises a second alarm for the same loss")
	}
	if due, _ := rs.recordLiveness("twitch", false, stamp.Add(time.Second)); due {
		t.Error("a tier-2 signed-out verdict one second after the mark still warranted recovery — the mark's stamp is not reaching the dedupe recordLiveness reads")
	}
}
```

- [ ] **Step 3: Run to verify the premise**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestAuthStatusChangedGateCoversEverySurfaceInput|TestTwitchMarkStampsTheSharedRecoveryDedupe' ./internal/cookies/ -v`
Expected: BOTH PASS. Deliberate — R4 and R6 are PINS on behaviour that already exists; the failing half is Step 4's mutations. If either FAILS, stop and report: R4 failing means `authStatusChanged` does not compare the booleans; R6 failing means the stamp is absent.

- [ ] **Step 4: Mutations to run**

| # | Mutation | Test that must fail |
|---|---|---|
| 1 | delete `next.YouTubeAuthenticated != prev.YouTubeAuthenticated \|\|` | `…/youtube_authenticated_flipped_and_nothing_else_did` *(verified at `8558f5f`: only the new row fails; `TestOnAuthChange*` and every `TestTwitchMark*` pass under it)* |
| 2 | delete `next.TwitchAuthenticated != prev.TwitchAuthenticated \|\|` | `…/twitch_authenticated_flipped_and_nothing_else_did` *(verified, same)* |
| 3 | add `next.TwitchError != prev.TwitchError \|\|` | `…/only_the_error_wording_changed` (pre-existing — confirms the exclusion survives the two new rows) |
| 4 | delete `rs.noteRecoveryDecided("twitch", time.Now())` from `NoteTwitchAuthLoss` | `TestTwitchMarkStampsTheSharedRecoveryDedupe` *(verified)* |

- [ ] **Step 5: Restate the contract paragraph**

`docs/spec/data-and-storage.md:857` — replace the LAST sentence of the `**`authStatusChanged` is a CONTRACT…**` paragraph. Old:

```
The verdicts and the cookies-present flags have to be in the gate: a platform going from conclusively-rejected to could-not-check leaves both booleans false, and on a boolean-only gate that badge transition was silent.
```

New:

```
The verdicts and the cookies-present flags have to be in the gate: a platform going from conclusively-rejected to could-not-check leaves both booleans false, and on a boolean-only gate that badge transition was silent. All six are surface inputs in their own right and each is pinned on its own — including the two `Authenticated` booleans, which every other row of the gate's table moves together with a verdict, so a gate missing either comparison passed the whole tree until `TestAuthStatusChangedGateCoversEverySurfaceInput` gained its two "alone" rows. Whether today's producers can move one without the other is a fact about the producers, not about the gate. **This paragraph is the contract restated, not widened: the exclusion list is unchanged and no `OnAuthChange`-driven surface may render `YouTubeError`/`TwitchError`.**
```

- [ ] **Step 6: Verify line endings, run the gates, commit**

```bash
for f in internal/cookies/refresh_threestate_test.go internal/cookies/refresh_twitch_mark_test.go docs/spec/data-and-storage.md; do
  printf '%s: ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```
Expected `0` each. Then the full gate block, then:

```bash
git add internal/cookies/refresh_threestate_test.go internal/cookies/refresh_twitch_mark_test.go docs/spec/data-and-storage.md
git commit -m "test(cookies): the auth gate's two booleans and the Twitch mark's dedupe stamp are pinned

authStatusChanged's table never moved either Authenticated flag on its
own, so a gate missing either comparison passed the whole tree; two rows
close it. The Twitch mark's noteRecoveryDecided stamp (ARMING row 12) is
asserted through the suppression it produces, which needs no arming.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN"
```

- [ ] **Step 7: Controller ledger note — NOT a commit**

Tell the controller to replace ARMING row 12's State cell in `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/progress-arc8.md:1433` (gitignored; do not `git add`) with:

```
**HALF PINNED (H2 Task 2, 2026-09-03):** `TestTwitchMarkStampsTheSharedRecoveryDedupe` asserts the stamp itself and the tier-2 suppression it produces — both observable while DISARMED. What remains for the arming commit is the double-FIRE assertion: with `livenessRecoveryArmed = true`, one chat-marked loss must reach `OnRecoveryNeeded` once, not twice.
```

---

### Task 3: R2 — the two log lines carry the auth horizons and the Twitch `login` expiry

Owner rulings Q5 and Q7. `AuthCookieHorizonFor` gets its first production consumer; `twitchLoginExpiry` gets a helper over the jar; the merge prune gets ONE Warn. Timestamps only. The SAME commit flips the three "no UI or log carries a horizon" absence claims.

**Where the second line goes, and why not `autocookies_firefox.go:288`.** The design draft anchors it on `"firefox <platform> refresh completed"`. That line fires per LAUNCH inside `refreshFirefox`'s platform loop — before `readFirefoxCookies`, before the merge, before `writeCookieFile` and before `s.jar.Load` — so a horizon read there is the horizon the pass STARTED with, which the boot line already reported, and the settling observation would compare two before-values. `refreshChromium` has no Info completion line at all (its tail is `return netscapeCookies, allNavigated, nil`), so a "Chromium twin" would have to be invented. The line that says a refresh completed AND stands after the write and reload is `"cookie refresh succeeded"` in `refreshCookiesDetailed`'s outcome switch — one site, both browser families, and the import path too.

**Files:**
- Modify: `internal/cookies/jar.go` — add `AuthHorizonString`, `TwitchLoginExpiry`, `HorizonLogFields` after `AuthCookieHorizonFor`
- Modify: `internal/cookies/autocookies_merge.go` — add `twitchCredentialsIn`, `twitchLoginPrunedFromMerge` after `mergeCookieFiles`
- Modify: `internal/cookies/autocookies.go` — the merge site in `refreshCookiesDetailed` (grep `netscapeCookies = mergeCookieFiles(previousCookies`) and the `"cookie refresh succeeded"` Info line
- Modify: `cmd/moombox/services.go` — the `Cookies loaded` line (grep `log.Info("Cookies loaded"`) and a new `cookiesLoadedFields` beside `detectCookiePlatforms`
- Modify: `docs/spec/data-and-storage.md:704-705`, `docs/spec/user-interfaces.md:724`, `docs/spec/platform-services.md:495`
- Modify: `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2012` and `:2016`
- Test: create `internal/cookies/jar_horizon_test.go`, `internal/cookies/autocookies_merge_login_prune_test.go`, `cmd/moombox/cookies_loaded_fields_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func AuthHorizonString(unix int64) string` (RFC3339 UTC, `"none"` for `<= 0`); `func (j *CookieJar) TwitchLoginExpiry() int64`; `func (j *CookieJar) HorizonLogFields() []any` (the three fields, alternating key/value — the single producer); `func twitchCredentialsIn(netscape string) (token, login string)`; `func twitchLoginPrunedFromMerge(previous, fetched, merged string) bool`; `func cookiesLoadedFields(jar *cookies.CookieJar, now int64) []any` in `cmd/moombox`.

`logger.Logger.Info(msg string, args ...any)` forwards to `slog.Logger.Log` and to `formatLogLine`, both of which accept a MIX of `slog.Attr` values and bare key/value pairs (`internal/logger/logger.go:378-390` detects `slog.Attr` explicitly). So `cookiesLoadedFields` may append `HorizonLogFields()`'s string pairs after four `slog.Attr`s.

- [ ] **Step 1: Write the failing jar tests**

Create `internal/cookies/jar_horizon_test.go`:

```go
package cookies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// horizonJar builds a jar from literal Netscape rows so the expiries under test
// are exactly the ones written here. No real cookie file is ever read.
func horizonJar(t *testing.T, rows ...string) *CookieJar {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	body := "# Netscape HTTP Cookie File\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestAuthHorizonStringIsATimestampOrNone. Zero is not a timestamp —
// AuthCookieHorizonFor returns it for a jar of session cookies and for an
// empty jar alike, and rendering that as 1970-01-01 would tell an operator
// their credentials expired 56 years ago.
//
// Mutation: format unconditionally.
func TestAuthHorizonStringIsATimestampOrNone(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int64
		want string
	}{
		{"no expiry to run out", 0, "none"},
		{"a negative expiry is not a date either", -1, "none"},
		{"a real expiry renders as UTC RFC3339", 1788000000, "2026-08-29T10:40:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthHorizonString(tc.in); got != tc.want {
				t.Errorf("AuthHorizonString(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTwitchLoginExpiryReadsTheLoginRow is Q7's helper. `login` is deliberately
// NOT in twitchAuthCookieNames — adding it would make a file holding only
// `login` fire "twitch auth lost" on the first check of every start — so
// AuthCookieHorizonFor cannot see it and this reads the row directly. A
// DIAGNOSTIC; it drives no alarm.
//
// Mutation: read "auth-token" instead of "login".
func TestTwitchLoginExpiryReadsTheLoginRow(t *testing.T) {
	const tokenRow = "#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t1788000000\tauth-token\ttest-token"

	t.Run("a login row with an expiry", func(t *testing.T) {
		jar := horizonJar(t, tokenRow, ".twitch.tv\tTRUE\t/\tFALSE\t1787000000\tlogin\tarchiveraccount")
		if got := jar.TwitchLoginExpiry(); got != 1787000000 {
			t.Errorf("TwitchLoginExpiry = %d, want 1787000000 — it must read the login row, not the token's", got)
		}
	})
	t.Run("a session-scoped login row", func(t *testing.T) {
		jar := horizonJar(t, tokenRow, ".twitch.tv\tTRUE\t/\tFALSE\t0\tlogin\tarchiveraccount")
		if got := jar.TwitchLoginExpiry(); got != 0 {
			t.Errorf("TwitchLoginExpiry = %d, want 0 — a session cookie has no expiry to run out", got)
		}
	})
	t.Run("no login row at all", func(t *testing.T) {
		if got := horizonJar(t, tokenRow).TwitchLoginExpiry(); got != 0 {
			t.Errorf("TwitchLoginExpiry = %d on a jar with no login row, want 0", got)
		}
	})
	t.Run("a nil jar", func(t *testing.T) {
		var jar *CookieJar
		if got := jar.TwitchLoginExpiry(); got != 0 {
			t.Errorf("TwitchLoginExpiry on a nil jar = %d, want 0", got)
		}
	})
}

// TestHorizonLogFieldsCarryTimestampsAndNothingElse is the leak scan, and the
// reason the three fields have ONE producer rather than a spelling at each of
// the two log sites. Every value must be "none" or a parseable RFC3339 stamp:
// a cookie value reaching a log line is the failure this subsystem is
// disciplined against, and a horizon field is exactly where one arrives by
// accident (`entry` instead of `entry.expiry`).
//
// Mutation: add a fourth pair carrying jar.GetTwitchAuthToken().
func TestHorizonLogFieldsCarryTimestampsAndNothingElse(t *testing.T) {
	jar := horizonJar(t,
		".youtube.com\tTRUE\t/\tTRUE\t1788000000\tSAPISID\tsapisid-secret-value",
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t1789000000\tauth-token\ttoken-secret-value",
		".twitch.tv\tTRUE\t/\tFALSE\t1787000000\tlogin\tarchiveraccount",
	)

	fields := jar.HorizonLogFields()
	if len(fields)%2 != 0 {
		t.Fatalf("HorizonLogFields returned %d entries — it must be alternating key/value pairs", len(fields))
	}
	want := map[string]string{
		"youtubeAuthHorizon": "2026-08-29T10:40:00Z",
		"twitchAuthHorizon":  "2026-09-10T00:26:40Z",
		"twitchLoginExpiry":  "2026-08-17T20:53:20Z",
	}
	got := map[string]string{}
	for i := 0; i < len(fields); i += 2 {
		key, _ := fields[i].(string)
		val, _ := fields[i+1].(string)
		got[key] = val
		if val == "none" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, val); err != nil {
			t.Errorf("field %q carries %q, which is not a timestamp", key, val)
		}
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("field %q = %q, want %q", key, got[key], wantVal)
		}
	}
	if len(got) != len(want) {
		t.Errorf("HorizonLogFields carries %d fields (%v), want exactly the three horizons", len(got), got)
	}
	for _, secret := range []string{"sapisid-secret-value", "token-secret-value", "archiveraccount"} {
		for key, val := range got {
			if strings.Contains(val, secret) {
				t.Errorf("field %q carries a cookie value (%q)", key, val)
			}
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestAuthHorizonString|TestTwitchLoginExpiry|TestHorizonLogFields' ./internal/cookies/`
Expected: FAIL to COMPILE — `undefined: AuthHorizonString`.

- [ ] **Step 3: Add the three jar helpers**

In `internal/cookies/jar.go`, after `AuthCookieHorizonFor`'s closing brace, before `twitchAuthCookieNames`:

```go
// AuthHorizonString renders a unix expiry as an ISO-8601 (RFC 3339) UTC
// timestamp, or "none" when there is no expiry to render.
//
// Zero is not a timestamp here, for the reason AuthCookieHorizonFor's own doc
// gives: it means "no auth cookie in this jar has an expiry to run out", the
// honest answer for a jar of session cookies and for an empty jar alike.
// Formatting it prints 1970-01-01.
//
// UTC on purpose: these strings are read out of a log file, often from a
// container whose local time is not the operator's, and compared with each
// other across a restart.
func AuthHorizonString(unix int64) string {
	if unix <= 0 {
		return "none"
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// TwitchLoginExpiry returns the expiry on Twitch's `login` row, or 0 when the
// row is absent or session-scoped.
//
// Its own accessor because `login` is deliberately OUTSIDE
// twitchAuthCookieNames and stays outside it (owner ruling Q7: that set is not
// split). The list drives an ALARM — HasAnyTwitchAuthCookie feeds
// shouldFireRecovery — so a file holding `login` with no auth-token would fire
// "twitch auth lost" on the first check of every start.
//
// What this is for is the gap that comment names: an expiring `login` has no
// advance warning anywhere, mergeCookieFiles prunes it on expiry while the
// auth-token survives, and chat then captures anonymously with every predicate
// here still reading the platform as configured. It reaches exactly two log
// lines (HorizonLogFields) and nothing else — no UI, no state, no alarm.
func (j *CookieJar) TwitchLoginExpiry() int64 {
	if j == nil {
		return 0
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.twitch["login"].expiry
}

// HorizonLogFields is the ONE producer of the three credential-lifetime fields
// the startup line and the refresh-completion line both carry, as alternating
// key/value pairs.
//
// One producer rather than two spellings, because the two lines are meant to be
// READ AGAINST EACH OTHER — the settling observation for "does the periodic
// twitch.tv navigation renew auth-token" is exactly "compare the boot horizon
// with the post-refresh horizon" — and two field lists is how the keys drift
// apart and the comparison stops being possible.
//
// TIMESTAMPS ONLY (TestHorizonLogFieldsCarryTimestampsAndNothingElse). Nothing
// here may carry a cookie value: one is a credential, one names the account.
func (j *CookieJar) HorizonLogFields() []any {
	return []any{
		"youtubeAuthHorizon", AuthHorizonString(j.AuthCookieHorizonFor(PlatformYouTube)),
		"twitchAuthHorizon", AuthHorizonString(j.AuthCookieHorizonFor(PlatformTwitch)),
		"twitchLoginExpiry", AuthHorizonString(j.TwitchLoginExpiry()),
	}
}
```

- [ ] **Step 4: Run to verify the jar tests pass**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestAuthHorizonString|TestTwitchLoginExpiry|TestHorizonLogFields' ./internal/cookies/ -v`
Expected: PASS.

- [ ] **Step 5: Write the failing prune tests**

Create `internal/cookies/autocookies_merge_login_prune_test.go`:

```go
package cookies

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// itoa keeps the fixture rows below readable.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// TestTwitchLoginPrunedFromMerge is the predicate behind Q7's single Warn.
//
// The state it names is narrow and the only one worth a line: an expired
// `login` goes on the expiry prune while the `auth-token` beside it survives.
// That leaves a Twitch session every predicate in jar.go reads as configured
// and an IRC handshake that goes fully anonymous WITHOUT attempting the login
// — so no refusal happens and the chat downgrade path's own Warn never runs.
//
// It must stay quiet elsewhere: a login that was never there is not a prune, a
// login that survives is not a prune, and a prune taking the auth-token with it
// is a total credential loss the existing loss reporting already names.
func TestTwitchLoginPrunedFromMerge(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour).Unix()
	future := time.Now().Add(24 * time.Hour).Unix()
	row := func(name, value string, expiry int64) string {
		return ".twitch.tv\tTRUE\t/\tFALSE\t" + itoa(expiry) + "\t" + name + "\t" + value
	}
	header := "# Netscape HTTP Cookie File\n"

	for _, tc := range []struct {
		name     string
		previous string
		fetched  string
		want     bool
	}{
		{
			name:     "the expired login goes and the token stays",
			previous: header + row("auth-token", "tok", future) + "\n" + row("login", "archiveraccount", past) + "\n",
			fetched:  header,
			want:     true,
		},
		{
			name:     "the login survives",
			previous: header + row("auth-token", "tok", future) + "\n" + row("login", "archiveraccount", future) + "\n",
			fetched:  header,
			want:     false,
		},
		{
			name:     "there was never a login row",
			previous: header + row("auth-token", "tok", future) + "\n",
			fetched:  header,
			want:     false,
		},
		{
			name:     "both halves expired — a total loss, not a half credential",
			previous: header + row("auth-token", "tok", past) + "\n" + row("login", "archiveraccount", past) + "\n",
			fetched:  header,
			want:     false,
		},
		{
			name:     "the fetched set replaces the expired login",
			previous: header + row("auth-token", "tok", future) + "\n" + row("login", "archiveraccount", past) + "\n",
			fetched:  header + row("login", "archiveraccount", future) + "\n",
			want:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeCookieFiles(tc.previous, tc.fetched)
			if got := twitchLoginPrunedFromMerge(tc.previous, tc.fetched, merged); got != tc.want {
				t.Errorf("twitchLoginPrunedFromMerge = %v, want %v\nmerged:\n%s", got, tc.want, merged)
			}
		})
	}
}

// TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned drives the predicate
// through the real refresh so the Warn is wired, not merely writable. The
// import path (detectBrowser nil) reaches the shared merge site with no
// browser. Asserted: the LINE, once, carrying no value.
//
// Mutation: delete the `if twitchLoginPrunedFromMerge(...)` block.
func TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour).Unix()
	previous := "# Netscape HTTP Cookie File\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\t" + goodTwitchToken + "\n" +
		".twitch.tv\tTRUE\t/\tFALSE\t" + itoa(past) + "\tlogin\tarchiveraccount\n"

	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}

	log := &argRecordingLogger{}
	jar := NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, jar, log)
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	const marker = "row expired and was pruned while the auth-token survived"
	all := log.all()
	if n := strings.Count(all, marker); n != 1 {
		t.Errorf("the half-credential prune was reported %d times, want exactly 1:\n%s", n, all)
	}
	if strings.Contains(all, "archiveraccount") || strings.Contains(all, goodTwitchToken) {
		t.Errorf("the prune report carries a cookie value:\n%s", all)
	}
}
```

- [ ] **Step 6: Run to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestTwitchLoginPrunedFromMerge|TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned' ./internal/cookies/`
Expected: FAIL to COMPILE — `undefined: twitchLoginPrunedFromMerge`.

- [ ] **Step 7: Add the two merge helpers**

In `internal/cookies/autocookies_merge.go`, after `mergeCookieFiles`, before `rowExpired`:

```go
// twitchCredentialsIn reads a Netscape cookie text's Twitch credential pair
// through a throwaway jar.
//
// A probe jar rather than a second row scanner, as netscapeCookiesHoldACredential
// already does it: Load knows which rows are twitch.tv rows, which are
// essential, and how a `#HttpOnly_` prefix parses, and a private
// reimplementation would disagree the first time one of those moved.
//
// Load deliberately ignores expiry, so a row that is PRESENT reads as present
// whatever its date — which makes the before/after comparison in
// twitchLoginPrunedFromMerge mean "the prune removed it".
func twitchCredentialsIn(netscape string) (token, login string) {
	probe := NewCookieJar()
	probe.loadFrom([]byte(netscape), "")
	return probe.GetTwitchCredentials()
}

// twitchLoginPrunedFromMerge reports the one merge outcome worth an
// operator-visible line (Q7): the expiry prune dropped Twitch's `login` while
// an `auth-token` survived.
//
// That is a degradation this tree can NAME. ircHandshakeLines throws away a
// missing login and renders the full anonymous pair, so chat is captured with
// no subscriber-only messages and no badges — WITHOUT a refusal, so the IRC
// path's own once-per-downloader Warn never fires. Every predicate in jar.go
// still reads the platform as configured, because `login` is outside
// twitchAuthCookieNames and stays outside it.
//
// Deliberately silent on the neighbours: a file that never held a `login` is a
// hand-written cookies.txt, and a prune taking the auth-token too is a total
// credential loss RefreshCookiesDetailed already reports — saying "chat went
// anonymous" about that points at the smaller of two problems.
//
// The `login` may arrive from either input (an import can supply a fresh one),
// so both are consulted going in.
func twitchLoginPrunedFromMerge(previous, fetched, merged string) bool {
	_, prevLogin := twitchCredentialsIn(previous)
	_, fetchedLogin := twitchCredentialsIn(fetched)
	if prevLogin == "" && fetchedLogin == "" {
		return false
	}
	mergedToken, mergedLogin := twitchCredentialsIn(merged)
	return mergedLogin == "" && mergedToken != ""
}
```

- [ ] **Step 8: Wire the Warn at the merge site**

In `internal/cookies/autocookies.go`, in `refreshCookiesDetailed`, replace:

```go
	if previousCookies != "" {
		netscapeCookies = mergeCookieFiles(previousCookies, netscapeCookies)
	}
```

with:

```go
	if previousCookies != "" {
		fetchedCookies := netscapeCookies
		netscapeCookies = mergeCookieFiles(previousCookies, netscapeCookies)
		// ONE line, for the one prune outcome that leaves a credential pair
		// half alive with nothing else in the process able to see it. See
		// twitchLoginPrunedFromMerge; it names no value and no account.
		if twitchLoginPrunedFromMerge(previousCookies, fetchedCookies, netscapeCookies) {
			s.logger.Warn("the Twitch login row expired and was pruned while the auth-token survived — " +
				"chat will capture anonymously (no subscriber-only messages, no badges) until a new login row arrives")
		}
	}
```

- [ ] **Step 9: Add the horizons to the refresh-completion line**

In the same function's outcome switch, replace:

```go
		default:
			s.logger.Info("cookie refresh succeeded", "verified", strings.Join(verified, " + "))
```

with:

```go
		default:
			// The horizons ride THIS line and not the per-launch
			// "<browser> <platform> refresh completed" lines: this is the only
			// completion point downstream of the write and the jar reload, so
			// a horizon logged inside refreshFirefox would describe the
			// credentials the pass started with — which the startup line
			// already reported. One site covers both browser families
			// (refreshChromium has no Info completion line of its own) and the
			// import path. Read against the boot line's identical three
			// fields, this is the settling observation for whether the
			// periodic twitch.tv navigation renews auth-token. Timestamps
			// only; see HorizonLogFields.
			refreshFields := append([]any{"verified", strings.Join(verified, " + ")}, s.jar.HorizonLogFields()...)
			s.logger.Info("cookie refresh succeeded", refreshFields...)
```

- [ ] **Step 10: Run the cookies package**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestTwitchLoginPrunedFromMerge|TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned|TestHorizonLogFields' ./internal/cookies/ -v`
Expected: PASS.

- [ ] **Step 11: Write the failing `cmd/moombox` test**

Create `cmd/moombox/cookies_loaded_fields_test.go`:

```go
package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestCookiesLoadedFieldsCarryTheHorizons pins the startup line's field list.
//
// The line is built inside initServices, which stands up the whole service
// graph and cannot be driven from a test — so the field list is its own pure
// function and this asserts it. Q5 puts the horizons here; Q7 adds the Twitch
// login expiry beside them.
//
// The four pre-existing fields are asserted too: they answer questions the
// horizons do not, and dropping one while adding three would be a silent
// regression in the one line an operator reads at boot.
//
// Mutations: drop any horizon pair; format a zero horizon as a date; carry
// jar.GetSapisid() as a value.
func TestCookiesLoadedFieldsCarryTheHorizons(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	body := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1788000000\tSAPISID\tsapisid-secret-value\n" +
		"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-info-secret\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t1789000000\tauth-token\ttoken-secret-value\n" +
		".twitch.tv\tTRUE\t/\tFALSE\t0\tlogin\tarchiveraccount\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	fields := cookiesLoadedFields(jar, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix())

	// Flatten both shapes the logger accepts, as formatLogLine does.
	got := map[string]string{}
	for i := 0; i < len(fields); {
		if attr, ok := fields[i].(slog.Attr); ok {
			got[attr.Key] = attr.Value.String()
			i++
			continue
		}
		if i+1 >= len(fields) {
			t.Fatalf("field %v has no value — the list is not well formed", fields[i])
		}
		key, _ := fields[i].(string)
		val, _ := fields[i+1].(string)
		got[key] = val
		i += 2
	}

	for key, want := range map[string]string{
		"completeAuthSet":    "true",
		"anyAuthCookie":      "true",
		"expiredYouTubeAuth": "0",
		"expiredTwitchAuth":  "0",
		"youtubeAuthHorizon": "2026-08-29T10:40:00Z",
		"twitchAuthHorizon":  "2026-09-10T00:26:40Z",
		"twitchLoginExpiry":  "none",
	} {
		if got[key] != want {
			t.Errorf("field %q = %q, want %q", key, got[key], want)
		}
	}
	if len(got) != 7 {
		t.Errorf("the startup line carries %d fields (%v), want the four predicates plus the three horizons", len(got), got)
	}
	for _, secret := range []string{"sapisid-secret-value", "login-info-secret", "token-secret-value", "archiveraccount"} {
		for key, val := range got {
			if strings.Contains(val, secret) {
				t.Errorf("field %q carries a cookie value (%q) — this line is timestamps and counts only", key, val)
			}
		}
	}
}
```

- [ ] **Step 12: Run to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestCookiesLoadedFieldsCarryTheHorizons ./cmd/moombox/`
Expected: FAIL to COMPILE — `undefined: cookiesLoadedFields`.

- [ ] **Step 13: Extract and extend the startup line**

In `cmd/moombox/services.go`, after `detectCookiePlatforms`, before `func (s *runState) initServices`:

```go
// cookiesLoadedFields builds the field list for the startup "Cookies loaded"
// line. Its own function because initServices cannot be driven from a test and
// this is the ONE place an operator sees what their cookie file actually
// contains at boot — see TestCookiesLoadedFieldsCarryTheHorizons.
//
// The first four are the predicates and counts that were always here. The last
// three are the credential lifetimes (Q5, Q7), produced by
// CookieJar.HorizonLogFields so this line and the "cookie refresh succeeded"
// line carry byte-identical keys and formatting and can be read against each
// other across a restart or a refresh.
//
// The list deliberately MIXES slog.Attr values with bare key/value pairs. Both
// sinks handle it — slog.Logger.Log and internal/logger's formatLogLine each
// detect slog.Attr and fall through to pairs — and the alternative is a second
// spelling of the horizon keys here, which is how the two lines drift apart.
//
// Never a cookie value. Counts, booleans and timestamps only.
func cookiesLoadedFields(jar *cookies.CookieJar, now int64) []any {
	return append([]any{
		slog.Bool("completeAuthSet", jar.HasYouTubeAuthCookies()),
		slog.Bool("anyAuthCookie", jar.HasAnyYouTubeAuthCookie()),
		slog.Int("expiredYouTubeAuth", jar.ExpiredAuthCookiesFor(cookies.PlatformYouTube, now)),
		slog.Int("expiredTwitchAuth", jar.ExpiredAuthCookiesFor(cookies.PlatformTwitch, now)),
	}, jar.HorizonLogFields()...)
}
```

Replace the call site in `initServices`:

```go
			now := time.Now().Unix()
			log.Info("Cookies loaded",
				slog.Bool("completeAuthSet", jar.HasYouTubeAuthCookies()),
				slog.Bool("anyAuthCookie", jar.HasAnyYouTubeAuthCookie()),
				slog.Int("expiredYouTubeAuth", jar.ExpiredAuthCookiesFor(cookies.PlatformYouTube, now)),
				slog.Int("expiredTwitchAuth", jar.ExpiredAuthCookiesFor(cookies.PlatformTwitch, now)))
```

with (keeping the whole existing comment block above it and appending this paragraph immediately before the call):

```go
			// And the three credential LIFETIMES beside the counts: the
			// soonest auth-cookie expiry per platform and the Twitch `login`
			// row's own, ISO-8601 UTC or "none". The counts say what has
			// already lapsed; the horizons say when the next thing will, which
			// is the half a file refreshed yesterday needs. Read against the
			// identical fields on "cookie refresh succeeded" they are also the
			// settling observation for whether a browser refresh renews the
			// Twitch token. See CookieJar.HorizonLogFields.
			log.Info("Cookies loaded", cookiesLoadedFields(jar, time.Now().Unix())...)
```

- [ ] **Step 14: Run to verify it passes**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestCookiesLoadedFieldsCarryTheHorizons ./cmd/moombox/ -v`
Expected: PASS.

- [ ] **Step 15: Mutations to run**

| # | Mutation | Test that must fail |
|---|---|---|
Rows marked *(verified)* were run by the plan review in a worktree at `8558f5f` with this task's code applied verbatim; all five tests passed as written first (the three RFC 3339 literals are correct).

| # | Mutation | Test that must fail |
|---|---|---|
| 1 | `AuthHorizonString` formats unconditionally | `TestAuthHorizonStringIsATimestampOrNone/no_expiry_to_run_out`; `TestCookiesLoadedFieldsCarryTheHorizons` *(verified)* |
| 2 | `TwitchLoginExpiry` reads `j.twitch["auth-token"].expiry` | `TestTwitchLoginExpiryReadsTheLoginRow` (rows 1 and 3) *(verified; the fields test fails too)* |
| 3 | `HorizonLogFields` adds `"twitchToken", j.GetTwitchAuthToken()` | `TestHorizonLogFieldsCarryTimestampsAndNothingElse` *(verified — AND `TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned`, whose leak scan reads every line the refresh logged, including `cookie refresh succeeded`; so a value leaking through the second log line IS caught, which narrows row 8 to the fields' presence)* |
| 4 | drop the `twitchAuthHorizon` pair | `TestHorizonLogFieldsCarryTimestampsAndNothingElse`; `TestCookiesLoadedFieldsCarryTheHorizons` |
| 5 | `twitchLoginPrunedFromMerge` drops `mergedToken != ""` | `…/both_halves_expired…` *(verified)* |
| 6 | delete the `prevLogin == "" && fetchedLogin == ""` guard | `…/there_was_never_a_login_row` *(verified)* |
| 7 | delete the `if twitchLoginPrunedFromMerge(...)` Warn block | `TestRefreshWarnsWhenTheExpiredTwitchLoginIsPruned` *(verified)* |
| 8 | drop `s.jar.HorizonLogFields()...` from `"cookie refresh succeeded"` | none — **accepted residual**: the line sits inside a 900-line function with no test seam; its fields come from the producer, which is pinned above, and a VALUE leaking through it is caught by row 3's second test. Verify by eye that the line carries seven fields. |

- [ ] **Step 16: Flip the three absence claims**

`docs/spec/data-and-storage.md:704-705` — replace both bullets. Old:

```
- `ExpiredAuthCookiesFor(platform, now)` has one production caller — the startup `Cookies loaded` line (`initServices`, `cmd/moombox/services.go:375-376`), which emits it per platform as `expiredYouTubeAuth` / `expiredTwitchAuth`. That log line is the only horizon-shaped output an operator ever sees.
- `AuthCookieHorizonFor(platform)` is an exported accessor with **test callers only**. Nothing in production calls it, so no UI and no log carries a horizon timestamp today.
```

New:

```
- `ExpiredAuthCookiesFor(platform, now)` has one production caller — the startup `Cookies loaded` line (`cookiesLoadedFields`, `cmd/moombox/services.go`), which emits it per platform as `expiredYouTubeAuth` / `expiredTwitchAuth`.
- `AuthCookieHorizonFor(platform)` and `TwitchLoginExpiry()` reach production through `CookieJar.HorizonLogFields()`, the single producer of `youtubeAuthHorizon` / `twitchAuthHorizon` / `twitchLoginExpiry` — ISO-8601 UTC, or `none` when there is no expiry to run out. Those three fields ride exactly TWO log lines: the startup `Cookies loaded` line and `refreshCookiesDetailed`'s `cookie refresh succeeded` line, the only refresh completion point downstream of the write and the jar reload. Read against each other they are the settling observation for whether a browser refresh renews the Twitch `auth-token`. **No UI carries a horizon**: there is no badge, payload key or API field, and none is planned. `TwitchLoginExpiry` is its own accessor because `login` is deliberately outside `twitchAuthCookieNames` and stays outside it — that list drives an alarm, this is a diagnostic. Timestamps only, never a value.
```

`docs/spec/user-interfaces.md:724` — two edits to the one bullet. First, its opening clause: `and the jar's two expiry accessors are not equally shipped` → `and the jar's expiry accessors reach the log and nothing else`. Second, replace from `` `AuthCookieHorizonFor` has no production consumer at all `` through the END of the bullet — that is THREE sentences (`…has no production consumer at all: …today (…).`, `Nothing on either UI reads either accessor.`, and `An expired Twitch `auth-token` in particular has no other warning: … instead of failing.`); the new text restates the third, so leaving it in place would print it twice. New:

```
`AuthCookieHorizonFor` and `TwitchLoginExpiry` reach the LOG and nothing else: `youtubeAuthHorizon` / `twitchAuthHorizon` / `twitchLoginExpiry` ride the startup `Cookies loaded` line and the `cookie refresh succeeded` line (`data-and-storage.md §Cookie Jar`), as ISO-8601 UTC or `none`. **No badge and no payload key carries a horizon** — nothing on either UI reads any of the three accessors, and that is the deliberate half. An expired Twitch `auth-token` still has no UI warning: `RefreshService` rotates YouTube in-process but only *checks* Twitch, and an expired token downgrades chat capture to anonymous instead of failing. The Twitch `login` row has one more: when the merge prunes it on expiry while the `auth-token` survives, the refresh logs a single Warn naming the degradation (anonymous chat, no subscriber-only messages, no badges) and no value.
```

`docs/spec/platform-services.md:495` — replace the last three sentences (from "The settling observation costs one run and has no surface yet" to the end of the paragraph). New:

```
The settling observation costs one run and now has a surface: **read the two log lines.** `twitchAuthHorizon` appears on the startup `Cookies loaded` line and again on `cookie refresh succeeded` after a browser refresh has written and reloaded the jar — same producer (`CookieJar.HorizonLogFields`), same ISO-8601 UTC formatting — so comparing them across one refresh answers it directly. A timestamp, never a value. No UI carries a horizon and none is planned; this is a diagnostic to read out of the log, not a measurement that needs a test harness.
```

- [ ] **Step 17: Close the two remediation-plan bullets**

`:2012` — append to the "Arc 5's Twitch keepalive" bullet:

```
 ***(BUILT — H2 Task 3: `CookieJar.HorizonLogFields()` produces `youtubeAuthHorizon` / `twitchAuthHorizon` / `twitchLoginExpiry` for BOTH lines — `cookiesLoadedFields` at boot and `cookie refresh succeeded` after the write and jar reload. NOT the per-launch `firefox <platform> refresh completed` line the ruling named: that fires before the merge, so the horizon there is the pass's INPUT, and `refreshChromium` has no Info twin. The three absence claims are flipped and the settling observation now says "read the two log lines".)***
```

`:2016` — append to the "Twitch `login` diagnostic split" bullet:

```
 ***(BUILT — H2 Task 3: `authCookieNamesFor` untouched; `CookieJar.TwitchLoginExpiry()` is its own accessor, logged as `twitchLoginExpiry` on the same two lines, and `twitchLoginPrunedFromMerge` fires ONE Warn from `refreshCookiesDetailed`'s merge when the expired `login` goes while the `auth-token` stays. No UI, no state, no alarm.)***
```

- [ ] **Step 18: Verify line endings, run the gates, commit**

```bash
for f in internal/cookies/jar.go internal/cookies/jar_horizon_test.go internal/cookies/autocookies_merge.go internal/cookies/autocookies_merge_login_prune_test.go internal/cookies/autocookies.go cmd/moombox/services.go cmd/moombox/cookies_loaded_fields_test.go docs/spec/data-and-storage.md docs/spec/user-interfaces.md docs/spec/platform-services.md docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md; do
  printf '%s: ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```
Expected `0` each. Then the full gate block, then:

```bash
git add internal/cookies/jar.go internal/cookies/jar_horizon_test.go internal/cookies/autocookies_merge.go internal/cookies/autocookies_merge_login_prune_test.go internal/cookies/autocookies.go cmd/moombox/services.go cmd/moombox/cookies_loaded_fields_test.go docs/spec/ docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md
git commit -m "feat(cookies): the two log lines carry the auth horizons and the Twitch login expiry

Q5 and Q7. One producer, CookieJar.HorizonLogFields, feeds the startup
Cookies loaded line and the cookie refresh succeeded line, so the two can
be read against each other. ONE Warn when the merge prunes an expired
login while the auth-token stays. Timestamps only. The three no-horizon
absence claims flip in the same commit.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN"
```

---

### Task 4: R3 — the browser path rolls back on a regression, and only on a regression

Owner ruling Q13 revised: A2's narrowed form ships. The import gate grows a browser-path twin taking ONLY `platformsToRestore`'s regression arm.

**Why the `verifyUnknown` arm must NOT restore there.** The inconclusive arm exists because a mounted profile can be arbitrarily stale and committing a set nobody could evaluate over one that may be fine is a bet with no upside. A browser refresh has just re-fetched from the live site, so an unreachable check afterwards is evidence about the NETWORK. Restoring there would throw away a genuinely fresher set on every DNS blip, on the desktop path that runs every 30 minutes.

**The shape.** A SIBLING function rather than a boolean parameter — the two functions are the two policies, and a parameter puts the decision at the call site instead of in a named thing. The regression condition moves into a shared `regressedAfterWrite` so the arm they agree on cannot drift.

**Files:**
- Modify: `internal/cookies/autocookies_profile.go` — `platformsToRestore` (`:684-723`); add `regressedAfterWrite`, `platformsToRestoreAfterBrowserRefresh`
- Modify: `internal/cookies/autocookies.go` — in `refreshCookiesDetailed`: the `pre` snapshot (grep `pre := map[string]platformAuth{}`), the scoping comment (grep `Only the import path does this`), the restore selection (grep `platformsToRestore(pre, importCheck)`), the two restore-failure wraps, and the `case len(restoredPlatforms) > 0` arm
- Modify: `docs/spec/data-and-storage.md:907`; `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2015`
- Test: `internal/cookies/autocookies_profile_rollback_test.go` (two tests added)

**Interfaces:**
- Consumes: nothing. (Task 3 edited the same function; rebase, do not revert.)
- Produces: `func regressedAfterWrite(before, after platformAuth) bool`; `func platformsToRestoreAfterBrowserRefresh(pre, post map[string]platformAuth) map[string]bool`. `platformsToRestore`'s signature and behaviour are unchanged, so `restorePolicy` can hold either.

- [ ] **Step 1: Write the two failing tests**

Append to `internal/cookies/autocookies_profile_rollback_test.go`:

```go
// TestBrowserRefreshRestoresARegressedPlatform is A2's narrowed form (Q13
// revised): the browser path now rolls back too, for the regression arm only.
//
// A twin of TestRefreshCookiesRestoresOnlyTheRegressedPlatform above, driven
// through the BROWSER path — a Firefox that cannot launch, so refreshFirefox
// swallows the failure and reads the profile, the only way to reach this
// branch without a real browser. YouTube alone, because refreshFirefox sleeps
// firefoxLaunchSpacing (5 s) BETWEEN platform launches.
//
// The premise is that a browser refresh CAN regress a platform, and it is not
// exotic: a profile the desktop browser signed out of, or one an extension
// cleared, hands back rows that win the merge by name+domain+path and do not
// work — the credential that worked is gone from disk under a refresh that
// reported success. The scoping comment this replaces asserted the opposite
// ("cannot be staler than what was on disk"), which is true of the values' AGE
// and says nothing about whether they authenticate.
//
// Mutation: skip the restore on the browser path entirely.
func TestBrowserRefreshRestoresARegressedPlatform(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	// Answers off the jar's live value: yes before the write, no after it — a
	// genuine regression rather than a flat failure.
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		return s.jar.GetSapisid() == "previous-sapisid", nil
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "previous-sapisid") {
		t.Errorf("the working YouTube credential was clobbered by a browser refresh that regressed it:\n%s", got)
	}
	if strings.Contains(got, "sapisid-from-profile") {
		t.Errorf("the regressed browser-refresh value survived the rollback:\n%s", got)
	}
	if s.jar.GetSapisid() != "previous-sapisid" {
		t.Errorf("jar holds %q after rollback, want the restored value", s.jar.GetSapisid())
	}
}

// TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive is the OTHER
// half of the narrowing, and the reason this is not simply "call
// platformsToRestore on both paths".
//
// The inconclusive arm exists for a mounted profile of unknown age. A browser
// refresh has just re-fetched from the live site, so a check that then could
// not reach the network says something about the NETWORK — restoring there
// discards a fresher set on every DNS blip, on the path a desktop install runs
// every thirty minutes. Its import-path twin
// (TestRefreshCookiesDoesNotCommitOnInconclusiveVerification) asserts the
// opposite outcome for the opposite reason; both must hold.
//
// Mutation: have the browser path call platformsToRestore (both arms).
func TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	// Conclusive before the write, unreachable after it.
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		if s.jar.GetSapisid() == "previous-sapisid" {
			return true, nil
		}
		return false, errors.New("dial tcp: i/o timeout")
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "sapisid-from-profile") {
		t.Errorf("a browser refresh's freshly fetched cookies were rolled back over an unreachable "+
			"network — the inconclusive arm must not apply to the browser path:\n%s", got)
	}
}
```

- [ ] **Step 2: Run to verify**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestBrowserRefreshRestoresARegressedPlatform|TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive' ./internal/cookies/ -v`
Expected: `TestBrowserRefreshRestoresARegressedPlatform` FAILS ("the working YouTube credential was clobbered"). `TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive` PASSES — it is the guard rail, and Step 6's mutation 1 is what proves it has teeth.

- [ ] **Step 3: Add the shared predicate and the browser sibling**

In `internal/cookies/autocookies_profile.go`, immediately BEFORE `platformsToRestore`'s doc comment:

```go
// regressedAfterWrite is the REGRESSION arm, shared by both rollback policies
// so the two cannot drift.
//
// "It verified before the write and is conclusively rejected after it" — the
// case that silently destroys a working credential, because mergeCookieFiles
// lets the newly written value win by name+domain+path and a sibling platform
// verifying can mask the loss entirely.
//
// Conclusive on BOTH sides on purpose: before.ok() is verifyOK and nothing
// else, so a platform already inconclusive has nothing established to have
// lost; after.state == verifyFailed is a rejection rather than a non-answer,
// so a network failure cannot make this fire.
func regressedAfterWrite(before, after platformAuth) bool {
	return before.ok() && after.state == verifyFailed
}
```

Rewrite `platformsToRestore`'s body (doc comment otherwise unchanged):

```go
func platformsToRestore(pre, post map[string]platformAuth) map[string]bool {
	restore := map[string]bool{}
	for platform, before := range pre {
		after := post[platform]
		switch {
		case regressedAfterWrite(before, after):
			restore[platform] = true
		case before.hasCookies && after.state == verifyUnknown:
			restore[platform] = true
		}
	}
	return restore
}
```

Append to `platformsToRestore`'s doc comment:

```
// This is the IMPORT policy. The browser path has its own —
// platformsToRestoreAfterBrowserRefresh — which takes the regression arm and
// not the inconclusive one. Both share regressedAfterWrite.
```

Add immediately after `platformsToRestore`:

```go
// platformsToRestoreAfterBrowserRefresh is the BROWSER path's rollback policy:
// the regression arm, and only the regression arm.
//
// The import policy's second arm — "had credentials before, could not be
// checked after" — is deliberately excluded here, and that exclusion is the
// whole content of this function. That arm reasons about a mounted profile of
// unknown age: it may be days stale, so committing a set nobody could evaluate
// over one that may be fine is a bet with no upside. A browser refresh has
// just re-fetched from the live site; its values cannot be older than what was
// on disk, so a check that then could not reach the network is evidence about
// the NETWORK — and restoring on it would discard a genuinely fresher set on
// every DNS blip, on the path a desktop install runs every thirty minutes.
//
// What the browser path DOES need is the first arm, which is the half the old
// scoping comment got wrong. "Cannot be staler than what was on disk" is true
// of the values' AGE and says nothing about whether they authenticate: a
// profile the desktop browser signed out of hands back rows that win the merge
// and do not work.
//
// TestBrowserRefreshRestoresARegressedPlatform and
// TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive are the halves.
func platformsToRestoreAfterBrowserRefresh(pre, post map[string]platformAuth) map[string]bool {
	restore := map[string]bool{}
	for platform, before := range pre {
		if regressedAfterWrite(before, post[platform]) {
			restore[platform] = true
		}
	}
	return restore
}
```

- [ ] **Step 4: Open the `pre` gate and select the policy**

In `internal/cookies/autocookies.go`, `refreshCookiesDetailed`. Replace the `pre` block:

```go
	// Verify BEFORE overwriting, on the import path only. Rolling back a
	// regression is impossible without knowing what worked beforehand, and
	// "the file had cookies" is not the same as "those cookies worked".
	// Skipped when there is nothing to protect, so the common container case
	// (no cookies.txt yet) costs no extra round trips.
	pre := map[string]platformAuth{}
	if importedFromProfile && previousCookies != "" {
		if loadErr := s.jar.Load(s.cookiePath); loadErr != nil {
			s.logger.Warn("could not load existing cookies before import — rollback protection is off", "err", loadErr)
		}
		preYT, preTW := s.checkPlatformAuth(ctx)
		pre["youtube"], pre["twitch"] = preYT, preTW
	}
```

with:

```go
	// Verify BEFORE overwriting, on BOTH paths. Rolling back a regression is
	// impossible without knowing what worked beforehand, and "the file had
	// cookies" is not the same as "those cookies worked".
	//
	// The browser path was excluded until A2's narrowed form (ruling Q13).
	// What the two paths DO with this snapshot still differs — see the policy
	// selection below — but the snapshot itself is the same question.
	//
	// Skipped when there is nothing to protect, so the common container case
	// (no cookies.txt yet) costs no extra round trips. On the browser path the
	// cost is two verification round trips per pass on an install that already
	// has credentials: the price of not silently destroying them.
	pre := map[string]platformAuth{}
	if previousCookies != "" {
		if loadErr := s.jar.Load(s.cookiePath); loadErr != nil {
			s.logger.Warn("could not load existing cookies before the refresh — rollback protection is off", "err", loadErr)
		}
		preYT, preTW := s.checkPlatformAuth(ctx)
		pre["youtube"], pre["twitch"] = preYT, preTW
	}
```

Replace the scoping comment and the selection. Old:

```go
	// Only the import path does this. The browser path writes cookies it just
	// re-fetched from the live site, which cannot be staler than what was on
	// disk.
	//
	// importCheck is kept under its own name because the rollback branch below
	// REPLACES postYT/postTW with a re-verification of what was restored. The
	// question "why did we reject the import" can only be answered by the check
	// that rejected it, and after the re-verify that check is no longer in
	// scope. See rollbackWasInconclusive.
	importCheck := map[string]platformAuth{"youtube": postYT, "twitch": postTW}
	var restoredPlatforms []string
	if restore := platformsToRestore(pre, importCheck); len(restore) > 0 {
```

New:

```go
	// BOTH paths do this now, under DIFFERENT policies (ruling Q13, A2's
	// narrowed form). The import path takes both arms — a regression and an
	// inconclusive check on a platform that had credentials. The browser path
	// takes the regression arm ONLY: it has just re-fetched from the live
	// site, so a check that could not reach the network afterwards is evidence
	// about the network, and restoring on it would discard a fresher set on
	// every blip. See platformsToRestoreAfterBrowserRefresh for the full
	// argument.
	//
	// This comment used to say the browser path needed no rollback at all,
	// because its cookies "cannot be staler than what was on disk". True of
	// their AGE; silent on whether they authenticate — a profile the browser
	// signed out of hands back rows that win the merge and do not work.
	//
	// importCheck is kept under its own name because the rollback branch below
	// REPLACES postYT/postTW with a re-verification of what was restored. The
	// question "why did we reject the new cookies" can only be answered by the
	// check that rejected them. See rollbackWasInconclusive.
	importCheck := map[string]platformAuth{"youtube": postYT, "twitch": postTW}
	restorePolicy := platformsToRestoreAfterBrowserRefresh
	if importedFromProfile {
		restorePolicy = platformsToRestore
	}
	var restoredPlatforms []string
	if restore := restorePolicy(pre, importCheck); len(restore) > 0 {
```

- [ ] **Step 5: Make the operator-facing strings true on both paths**

Still in the restore branch, replace the Warn:

```go
		s.logger.Warn("imported profile cookies did not hold up — restoring the previous credentials for those platforms",
			"platforms", strings.Join(restoredPlatforms, ","), "profile_dir", s.profileDir)
```

with:

```go
		source := "the browser refresh"
		if importedFromProfile {
			source = "the imported profile cookies"
		}
		s.logger.Warn("the newly written cookies did not hold up — restoring the previous credentials for those platforms",
			"source", source, "platforms", strings.Join(restoredPlatforms, ","), "profile_dir", s.profileDir)
```

Change the two internal wraps: `"restore pre-import cookies: %w"` → `"restore previous cookies: %w"`, and the Error message `"could not restore pre-import cookies.txt"` → `"could not restore the previous cookies.txt"`. The two `errMsg` sentences reaching the operator ("the browser profile did not verify for …" and "restored the previous cookies for … after the browser profile did not verify") stay: both paths read a browser profile, so both remain true, and rewording strings an operator may have learned buys nothing.

In the outcome switch, replace:

```go
	case len(restoredPlatforms) > 0:
		errMsg = "kept the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
			" — the mounted browser profile did not verify"
```

with:

```go
	case len(restoredPlatforms) > 0:
		rejected := "the refreshed browser cookies"
		if importedFromProfile {
			rejected = "the mounted browser profile"
		}
		errMsg = "kept the previous cookies for " + strings.Join(restoredPlatforms, " + ") +
			" — " + rejected + " did not verify"
```

The `rollbackWasInconclusive` arm above it is unreachable on the browser path by construction — that policy never restores a `verifyUnknown` platform — so it needs no change.

- [ ] **Step 6: Run, then run the mutations**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestBrowserRefresh|TestRefreshCookies|TestRollback|TestImportIs' ./internal/cookies/ -v`
Expected: PASS, every pre-existing import-path rollback test unchanged.

| # | Mutation | Test that must fail |
|---|---|---|
Rows marked *(verified)* were run by the plan review in a worktree at `8558f5f` with this task's code applied verbatim. Step 2's prediction also held there: against the unmodified code `TestBrowserRefreshRestoresARegressedPlatform` failed on all three of its assertions and `…KeepsFreshCookiesWhenTheCheckIsInconclusive` passed; after Steps 3-5 the whole `TestBrowser|TestRefreshCookies|TestRollback|TestImport|TestAcquisition|TestRealProfile|TestAuto|TestRestore` set passed, including the tests that count verification calls on the browser path (`autocookies_browsergate_test.go`, `autocookies_acquisition_test.go`, `autocookies_launchguard_test.go` assert `verified` as a boolean or against zero, so the added pre-check does not move them).

| # | Mutation | Test that must fail |
|---|---|---|
| 1 | `restorePolicy := platformsToRestore` unconditionally | `TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive` *(verified)* |
| 2 | `restorePolicy := platformsToRestoreAfterBrowserRefresh` unconditionally | `TestRefreshCookiesDoesNotCommitOnInconclusiveVerification`, `TestImportIsNotCommittedWhenTheRealCheckIsRateLimited` *(verified; four more import-path rollback tests fail with them)* |
| 3 | restore the `importedFromProfile &&` guard on the `pre` snapshot | `TestBrowserRefreshRestoresARegressedPlatform` *(verified)* |
| 4 | `platformsToRestoreAfterBrowserRefresh` returns `map[string]bool{}` at the top | `TestBrowserRefreshRestoresARegressedPlatform` |
| 5 | `regressedAfterWrite` drops `before.ok()` | `TestRefreshCookiesKeepsImportWhenNothingWasEverGood` *(verified; `TestImportIsCommittedWhenTwitchConclusivelyRejectsTheToken` and `TestBrowserRefreshWithEmptyProfileSurfacesItWhenAuthIsDead` fail with it)* |
| 6 | `regressedAfterWrite` uses `after.state != verifyOK` | `TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive` *(verified)* |

- [ ] **Step 7: Add the browser-path sentence to the spec**

`docs/spec/data-and-storage.md:907` — replace the last two sentences of the paragraph beginning "The guard is **not for the manual triggers**". Old:

```
The two-platform case (only one platform died) is covered not by the guard but by `RefreshCookiesDetailed`'s own abort/merge/rollback, which re-checks at write time: it verifies each platform BEFORE the import, and `platformsToRestore` (`internal/cookies/autocookies_profile.go`) hands back the rows of any platform that either verified before and failed after (a regression) or had credentials before and could not be checked after (inconclusive — committing a set nobody could evaluate over one that may be fine is a bet with no upside). A platform that was already dead is deliberately not restored.
```

New:

```
The two-platform case (only one platform died) is covered not by the guard but by `RefreshCookiesDetailed`'s own abort/merge/rollback, which re-checks at write time: it verifies each platform BEFORE the write — **on both paths**, whenever a `cookies.txt` already exists — and then applies one of two policies. The IMPORT path uses `platformsToRestore` (`internal/cookies/autocookies_profile.go`), which hands back the rows of any platform that either verified before and failed after (a regression) or had credentials before and could not be checked after (inconclusive — committing a set nobody could evaluate over one that may be fine is a bet with no upside). The BROWSER path uses `platformsToRestoreAfterBrowserRefresh`, the **regression arm only**: a browser refresh has just re-fetched from the live site, so a check that then could not reach the network is evidence about the network, and restoring on it would discard a fresher set on every DNS blip — on the path a desktop install runs every thirty minutes. Both share `regressedAfterWrite`, so the arm they agree on cannot drift. A platform that was already dead is deliberately not restored on either path. The browser path's exclusion used to be total, on the grounds that its cookies "cannot be staler than what was on disk"; that is true of their age and says nothing about whether they authenticate, which is why the regression arm now applies there too.
```

- [ ] **Step 8: Close the remediation-plan item**

`:2015` — append to the "two Arc 2 evaluation items" bullet:

```
 ***(Item (2) is CLOSED, not open: BUILT — H2 Task 4, ruling Q13 revised. `platformsToRestoreAfterBrowserRefresh` gives the browser path the regression arm only; `regressedAfterWrite` is shared with the import policy; the `pre` snapshot now runs on both paths whenever a `cookies.txt` exists. Item (1), the cross-writer lost update, stands as RULED NO.)***
```

`:2009` — the **A2** bullet is the primary record of the Q13-REVISED ruling and must not be left reading as still-to-do. Append to it:

```
 ***(BUILT — H2 Task 4: `platformsToRestoreAfterBrowserRefresh` (regression arm only) beside `platformsToRestore` (both arms), sharing `regressedAfterWrite`; the `pre` snapshot runs on both paths; `TestBrowserRefreshRestoresARegressedPlatform` and `TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive` are the two arms, each mutation-checked. "Rejected as written" still stands — the inconclusive arm does not apply to the browser path.)***
```

- [ ] **Step 9: Verify line endings, run the gates, commit**

```bash
for f in internal/cookies/autocookies_profile.go internal/cookies/autocookies.go internal/cookies/autocookies_profile_rollback_test.go docs/spec/data-and-storage.md docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md; do
  printf '%s: ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```
Expected `0` each. Then the full gate block, then:

```bash
git add internal/cookies/autocookies_profile.go internal/cookies/autocookies.go internal/cookies/autocookies_profile_rollback_test.go docs/spec/data-and-storage.md docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md
git commit -m "feat(cookies): the browser path rolls back a regression, and only a regression

A2's narrowed form (Q13 revised). The pre-write snapshot now runs on both
paths; the browser path takes platformsToRestore's regression arm through
platformsToRestoreAfterBrowserRefresh and never its inconclusive one — a
refresh has just re-fetched from the live site, so an unreachable check
afterwards is about the network.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN"
```

---

### Task 5: R5 + R8 — the TUI feedback triple becomes one struct, and the wizard shows its accepted verdict

**R5.** `App.feedbackMsg` / `feedbackSev` / `feedbackTimer` are three fields with an invariant maintained by convention. Folding them into one struct written as a whole makes it structural: a message cannot be set without its severity because there is nowhere to put one without the other. Rendering is byte-identical.

**R8.** The `setupCookieFinishMsg` accepted arm calls `a.setFeedback(...)`, which renders on the App feedback line — a line `app_layout.go:107-109` never draws while the wizard overlay is visible, because the overlay takes the whole view. The operator sees only the green ✓ on the platform row. The verdict goes inside the overlay, beside `errorMsg`.

**The feedback line still gets it.** Setting BOTH is the decision: holding the message until the overlay closes needs new state (a pending-feedback field) and a close hook to drain it — a mechanism built to contain a mechanism. Setting both costs nothing, and every non-overlay caller of `setFeedback` and every existing test stays byte-identical.

**Files:**
- Modify: `internal/tui/app.go` (`:355-369`), `app_update.go` (`:55-57`, `:720-734`, `:924-948`), `app_layout.go` (`:151-156`, and the `feedbackSeverity` doc at `:233`), `app_actions.go:89,93`, `app_keys.go:355,366`, `styles.go`, `setup_wizard.go` (`:195`, `Open`, `OpenCookieLogin`, `HandleKey`, `viewSimpleCookies`, `viewAdvancedCookies`)
- Test: `app_layout_test.go`, `cookie_forcerefresh_chord_test.go`, `cookie_login_chord_test.go`, `cookie_recheck_reason_test.go`, `cookie_refresh_feedback_test.go`, `cookie_threestate_test.go` (field renames); `setup_cookie_feedback_test.go` (renames + one new test)

**Interfaces:**
- Consumes: nothing.
- Produces: `type appFeedback struct { msg string; sev feedbackSeverity; until time.Time }`; field `feedback appFeedback` on `App`; `func (a *App) clearFeedback()`; `SetupWizardModel.successMsg string`; `var SuccessStyle = lipgloss.NewStyle().Foreground(ColorGreen)`. `setFeedback`, `setFeedbackWithSeverity`, `setFeedbackWithDuration` keep their names and signatures.

**NOT touched:** `FilesDialogModel.feedbackMsg` and `ClientTokensDialogModel.feedbackMsg` are those dialogs' own fields with their own lifecycles. The `a.filesDlg.feedbackMsg` / `a.clientTokensDlg.feedbackMsg` writes in `app_update.go:536,553,570` stay exactly as they are.

- [ ] **Step 1: Write the failing test (R8)**

Append to `internal/tui/setup_cookie_feedback_test.go`:

```go
// TestSetupCookieAcceptedVerdictRendersInsideTheOverlay is the 12a arc-close
// finding F4.
//
// The accepted arm reported through the App's transient feedback line, which
// app_layout.go never draws while the setup wizard is visible — the overlay
// takes the whole view. So an operator who signed in and pressed Enter saw the
// green tick on the platform row and nothing else: not which platforms were
// accepted, and not the "saved, but could not establish" hedge that is the
// point of the four-arm split beside it.
//
// The verdict now renders INSIDE the wizard, where errorMsg already does. It
// ALSO still reaches the feedback line, deliberately: the alternative is
// holding it in new state until the overlay closes and draining it from a
// close hook — a mechanism built to contain a mechanism. Together they cost
// nothing: the wizard's line is read while the overlay stands, the feedback
// line if it is closed at once.
//
// goja-free: NewApp() plus one Update, like TestSetupCookieFinishFeedback.
//
// Mutations: drop the successMsg write from the accepted arm; render
// successMsg with ErrorStyle.
func TestSetupCookieAcceptedVerdictRendersInsideTheOverlay(t *testing.T) {
	app := NewApp()
	app.setupWiz.OpenCookieLogin("youtube")
	app.setupWiz.SetSize(100, 30)

	app.Update(setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
		YouTube: cookies.RefreshOK, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
	}})

	if !app.setupWiz.IsVisible() {
		t.Fatal("premise broken: the finish arm closed the overlay, so there is nothing to render into")
	}

	view := app.setupWiz.View()
	if !strings.Contains(view, "YouTube cookies configured") {
		t.Errorf("the accepted verdict is not reachable through the overlay the operator is looking at:\n%s", view)
	}
	if !strings.Contains(view, SuccessStyle.Render("YouTube cookies configured")) {
		t.Error("the accepted verdict is not rendered with SuccessStyle — a confirmed sign-in must not read as an error")
	}
	if !strings.Contains(app.feedback.msg, "YouTube cookies configured") {
		t.Errorf("the App feedback line no longer carries the verdict: %q", app.feedback.msg)
	}

	// The hedged verdict travels the same way — the arm the ✓ alone cannot express.
	hedged := NewApp()
	hedged.setupWiz.OpenCookieLogin("youtube")
	hedged.setupWiz.SetSize(100, 30)
	hedged.Update(setupCookieFinishMsg{Platform: "youtube", Result: cookies.SetupResult{
		YouTube: cookies.RefreshUnknown, Twitch: cookies.RefreshFailed, YouTubeAccepted: true,
	}})
	if v := hedged.setupWiz.View(); !strings.Contains(v, "could not establish") {
		t.Errorf("the accepted-but-unverified hedge does not reach the overlay:\n%s", v)
	}

	// A rejected finish still uses the error line and sets no success line.
	rejected := NewApp()
	rejected.setupWiz.OpenCookieLogin("youtube")
	rejected.setupWiz.SetSize(100, 30)
	rejected.Update(setupCookieFinishMsg{Platform: "youtube", Err: "cookies.txt could not be read"})
	if rejected.setupWiz.successMsg != "" {
		t.Errorf("a failed finish set the wizard's success line: %q", rejected.setupWiz.successMsg)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestSetupCookieAcceptedVerdictRendersInsideTheOverlay ./internal/tui/`
Expected: FAIL to COMPILE — `app.feedback undefined`, `SuccessStyle undefined`, `successMsg undefined`.

- [ ] **Step 3: Fold the three fields**

In `internal/tui/app.go`, replace the three declarations and their comment block with:

```go
	// feedback is the transient status line (auto-clears after 3 s) together
	// with everything known about it.
	//
	// ONE struct because the invariant is "a message never outlives the
	// severity it was stated for", which three fields could only maintain by
	// convention: every setter wrote all three in the same statement pair and
	// every clear-only site left the severity behind, harmlessly but on trust.
	// Written as a whole value there is nowhere to put a message without a
	// severity, and clearing is `appFeedback{}`.
	//
	// Behaviour is unchanged by the fold: the severity is still only read
	// while msg != "" (viewMain), and feedbackColor still falls back to
	// scanning the text when sev is severityUnstated — the zero value a
	// composer that knew nothing about its own line leaves in place.
	// TestStatedSeverityDoesNotLeakToTheNextMessage pins the invariant this
	// makes structural.
	feedback appFeedback
```

Add after the `App` struct's closing brace:

```go
// appFeedback is the App's transient status line: what it says, what its
// composer knew about how alarming it is, and when it stops being shown.
//
// The dialogs' own feedbackMsg fields (FilesDialogModel, ClientTokensDialogModel)
// are unrelated — a different line with a different lifecycle, keyed to a
// confirm chord rather than a timer.
type appFeedback struct {
	msg string
	// sev is what the COMPOSER knew, where it knew anything. severityUnstated
	// — the zero value — means it did not, and feedbackColor falls back to
	// scanning the text. See feedbackSeverity for why the inference is not
	// good enough on the one line that carries a stated fact.
	sev feedbackSeverity
	// until is when the line stops being shown. The zero value means "nothing
	// scheduled", which is what an empty struct reads as.
	until time.Time
}
```

- [ ] **Step 4: Rewrite the setters, add the clear helper, fix the tick**

In `app_update.go`, replace the two setter bodies (doc comments stay, minus the now-stale "Reset, never left alone: …" sentence in `setFeedbackWithDuration`):

```go
func (a *App) setFeedbackWithSeverity(msg string, stated feedbackSeverity) {
	a.feedback = appFeedback{msg: msg, sev: stated, until: time.Now().Add(3 * time.Second)}
}

func (a *App) setFeedbackWithDuration(msg string, d time.Duration) {
	// severityUnstated by construction rather than by an explicit reset: every
	// one of these is a chord message whose severity the scan reads correctly,
	// and the whole value is replaced, so the previous line's stated fact
	// cannot survive into this one.
	a.feedback = appFeedback{msg: msg, until: time.Now().Add(d)}
}

// clearFeedback takes the transient line down now.
//
// The whole value, including the severity: a line that is not on screen has no
// severity, and leaving one behind was the "harmless by convention" half of
// the invariant the struct exists to make structural. Rendering is unchanged —
// viewMain reads msg alone, and the tick's expiry branch is simply not reached
// once until is zero.
func (a *App) clearFeedback() {
	a.feedback = appFeedback{}
}
```

Replace the tick expiry block:

```go
		if !a.feedback.until.IsZero() && time.Now().After(a.feedback.until) {
			a.clearFeedback()
		}
```

- [ ] **Step 5: Update the four clear sites and the render site**

`app_actions.go:89` and `:93`, `app_keys.go:355` and `:366` — replace each `a.feedbackMsg = ""` with `a.clearFeedback()`. (Four sites; `grep -n 'a\.feedbackMsg = ""' internal/tui/*.go` must then print nothing.)

`app_layout.go:233` — in `feedbackSeverity`'s doc comment, `feedbackColor reads a.feedbackMsg, which is the line AFTER fitFeedback has` → `feedbackColor reads a.feedback.msg, which is the line AFTER fitFeedback has`.

`app_layout.go:151-156` — replace with:

```go
	if a.feedback.msg != "" {
		msgColor := feedbackColor(a.feedback.msg, a.feedback.sev)
		content = addOverlayMessage(content, a.width,
			lipgloss.NewStyle().Foreground(msgColor).Render(a.feedback.msg),
		)
	}
```

- [ ] **Step 6: Rename in the seven test files**

```bash
perl -pi -e 's/\bapp\.feedbackMsg\b/app.feedback.msg/g; s/\bapp\.feedbackSev\b/app.feedback.sev/g' \
  internal/tui/app_layout_test.go internal/tui/cookie_forcerefresh_chord_test.go \
  internal/tui/cookie_login_chord_test.go internal/tui/cookie_recheck_reason_test.go \
  internal/tui/cookie_refresh_feedback_test.go internal/tui/cookie_threestate_test.go \
  internal/tui/setup_cookie_feedback_test.go
grep -rn "app\.feedbackMsg\|app\.feedbackSev\|a\.feedbackMsg\|a\.feedbackSev\|a\.feedbackTimer" internal/tui/   # must print nothing
grep -c "m\.feedbackMsg" internal/tui/files_dialog.go internal/tui/client_tokens_dialog.go                       # must be unchanged
```

In `app_layout_test.go`, update `TestStatedSeverityDoesNotLeakToTheNextMessage`'s doc: replace "The severity lives beside the message rather than on it" with "The severity lives in the same struct as the message, so a setter cannot write one without the other — this is what says the two setters still replace the whole value rather than patching a field".

- [ ] **Step 7: Add the success style and the wizard field**

`styles.go`, beside `ErrorStyle`:

```go
	ErrorStyle     = lipgloss.NewStyle().Foreground(ColorError)
	SuccessStyle   = lipgloss.NewStyle().Foreground(ColorGreen)
```

`setup_wizard.go`, beside `errorMsg` in `SetupWizardModel`:

```go
	errorMsg string
	// successMsg is the ACCEPTED verdict of a cookie step, rendered where
	// errorMsg is.
	//
	// It exists because the App's transient feedback line is invisible while
	// this overlay stands — app_layout.go returns the wizard's whole view — so
	// the operator who signed in and pressed Enter saw the platform row's tick
	// and nothing else, not even the "saved, but could not establish" hedge
	// that is the point of the four-arm split in app_update.go.
	//
	// Never set at the same time as errorMsg: the finish arm writes the pair
	// in one statement, and HandleKey clears both on the next keypress.
	successMsg string
```

- [ ] **Step 8: Render it, clear it, set it**

In `viewSimpleCookies`, replace the `// Error` block:

```go
	// Verdict — at most one of the two is ever set (see successMsg).
	if m.errorMsg != "" {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Render(m.errorMsg))
	}
	if m.successMsg != "" {
		lines = append(lines, "")
		lines = append(lines, SuccessStyle.Render(m.successMsg))
	}
```

Make the identical replacement at `viewAdvancedCookies`'s own `if m.errorMsg != ""` block (the one immediately above the navigation separator).

In `Open()` and `OpenCookieLogin()`, add `m.successMsg = ""` after each `m.errorMsg = ""`.

In `HandleKey`, replace the error clear with:

```go
	// The next keypress takes both verdicts down. A stale "cookies configured"
	// under a step the operator has moved on from is the same mistake a stale
	// error would be.
	if m.errorMsg != "" {
		m.errorMsg = ""
	}
	if m.successMsg != "" {
		m.successMsg = ""
	}
```

In `app_update.go`'s `setupCookieFinishMsg` switch, replace the three arms:

```go
		// Each arm writes the PAIR, so the wizard can never show a verdict and
		// its opposite at once.
		switch {
		case msg.Err != "":
			a.setupWiz.errorMsg, a.setupWiz.successMsg = msg.Err, ""
		case !msg.Result.YouTubeAccepted && !msg.Result.TwitchAccepted:
			a.setupWiz.errorMsg, a.setupWiz.successMsg = setupCookieRejectedMessage(msg.Platform, msg.Result), ""
		default:
			var parts []string
			if msg.Result.YouTubeAccepted {
				parts = append(parts, setupCookieAcceptedMessage("YouTube", msg.Result.YouTube))
			}
			if msg.Result.TwitchAccepted {
				parts = append(parts, setupCookieAcceptedMessage("Twitch", msg.Result.Twitch))
			}
			verdict := strings.Join(parts, "; ")
			// BOTH surfaces, and that is the decision rather than an oversight.
			// The wizard's own line is what the operator reads while the
			// overlay stands — app_layout.go draws nothing else — and the 3 s
			// feedback line is what they read if they close it immediately.
			// Holding the message until the overlay closes would need a
			// pending-feedback field and a close hook to drain it: a mechanism
			// built to contain a mechanism, for a line that costs nothing to
			// set twice.
			a.setupWiz.errorMsg, a.setupWiz.successMsg = "", verdict
			a.setFeedback(verdict)
		}
```

- [ ] **Step 9: Run the package**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/tui/`
Expected: PASS, all of it. The layout tests are the byte-identity check on R5 — if `TestLastCookieErrorNeverLowersSeverity`, `TestRecheckWithNoPlatformsIsAnAdvisory` or `TestStatedSeverityDoesNotLeakToTheNextMessage` moved, the fold changed behaviour and the CODE must be corrected, not the test.

- [ ] **Step 10: Mutations to run**

| # | Mutation | Test that must fail |
|---|---|---|
| 1 | `setFeedbackWithDuration` patches `msg`/`until` instead of replacing the struct | `TestStatedSeverityDoesNotLeakToTheNextMessage/setFeedbackWithDuration` |
| 2 | `clearFeedback` sets only `a.feedback.msg = ""` | none — **accepted**: behaviourally identical, which is the point of the struct replacement |
| 3 | drop `a.setupWiz.successMsg = verdict` | `TestSetupCookieAcceptedVerdictRendersInsideTheOverlay` |
| 4 | render `successMsg` with `ErrorStyle` | `TestSetupCookieAcceptedVerdictRendersInsideTheOverlay` (style assertion) |
| 5 | drop `a.setFeedback(verdict)` | `TestSetupCookieFinishFeedback` (4 rows), `TestSetupCookieUncheckedOutcomeIsNotRed`, and the new test's feedback assertion |
| 6 | drop `m.successMsg = ""` from `HandleKey` | none — **accepted residual**: the two writes are paired at the only producer, so a stale line needs a producer that does not clear its twin |

- [ ] **Step 11: Verify line endings, run the gates, commit**

```bash
for f in $(git diff --name-only -- internal/tui/) internal/tui/setup_cookie_feedback_test.go; do
  printf '%s: ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```
Expected `0` each (`perl -pi` preserves LF). Then the full gate block, then:

```bash
git add internal/tui/
git commit -m "refactor(tui): the feedback triple is one struct, and the wizard shows its accepted verdict

App.feedbackMsg/feedbackSev/feedbackTimer fold into appFeedback, written
as a whole value, so 'a message never outlives its severity' is
structural rather than conventional. Rendering is byte-identical.

The setup wizard's ACCEPTED cookie verdict now renders inside the overlay
beside errorMsg (12a arc-close F4) — the App feedback line it used to
report through is not drawn while the overlay stands. It still reaches
the feedback line for after the overlay closes.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN"
```

---

### Task 6: R7 — the deferred re-check sites are pinned by an AST test

Arc 10's reload-site table names five `cmd/moombox` gestures that put new credentials on disk and must reach `CheckNow`, because `refresh`'s status block is the ONLY place the Twitch fingerprint is compared, the auth mark cleared and `OnCredentialsChanged` fired. Four sit in a `defer` so the `result.Ran` / `result.Wrote` gate is evaluated independently of the error return — three of the eight `refreshAborted()` exits happen AFTER `cookies.txt` was rewritten, and returning on `err` first skipped exactly the passes whose write nobody had compared. The fifth is `OnPassCompleted`, a plain closure fired later by `notePassCompleted()` through `postRefreshRecheckHook`'s recover guard. Nothing pins the shape; hoisting one call out of its `defer` is invisible to every behavioural test.

**Files:** create `cmd/moombox/recheck_callsite_test.go`.

**Interfaces:** Consumes nothing; produces nothing importable. No production code changes.

**The five sites at `78590d2`** (the test re-derives them; grep `recheckAfterCookieWrite(`):

| File | Gesture literal | Shape |
|---|---|---|
| `monitor_callbacks.go` | `"recovery"` | `defer func(){ if result.Ran { … } }()` |
| `services.go` | `"an automatic cookie refresh"` | `postRefreshRecheckHook(func(){ … }, log)` assigned to `autoCookieSvc.OnPassCompleted` |
| `services.go` | `"the job-triggered cookie refresh"` | `defer func(){ if result.Ran { … } }()` |
| `tui_wiring.go` | `"browser refresh"` | `defer func(){ if result.Ran { … } }()` |
| `tui_wiring.go` | `"the setup wizard"` | `defer func(){ if result.Wrote { … } }()` |

`tui_wiring.go`'s bare `s.cookieRefresh.CheckNow(context.Background())` (the `R C` chord) is out of scope: it is the gesture itself, not a re-check after a write, and goes through no helper.

- [ ] **Step 1: Write the test**

Create `cmd/moombox/recheck_callsite_test.go`:

```go
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// recheckSite is one production call to recheckAfterCookieWrite with what the
// walk could establish about where it sits.
type recheckSite struct {
	file                string
	line                int
	gesture             string
	deferred            bool
	inPassCompletedHook bool
}

// TestEveryCookieWriteRecheckIsDeferred pins the SHAPE of the five re-check
// sites the Arc 10 reload-site table names.
//
// Shape and not behaviour: refresh's status block is the only place the Twitch
// credential fingerprint is compared, the auth mark cleared and
// OnCredentialsChanged fired, and it runs only inside a refresh pass — so every
// gesture that can put new credentials on disk has to reach CheckNow, or a
// repaired cookie waits on the 30-minute ticker while a stale "Twitch needs
// re-authorization" stands over a file that no longer has that problem and no
// live chat session is told to reconnect. Driving any of these five from a test
// means a live guide POST and a live oauth2/validate (youtubeGuideURL and
// twitchValidateURL are unexported package vars in internal/cookies), so the
// behavioural half is pinned inside that package and what can only be asserted
// HERE is that the call is placed where it cannot be skipped.
//
// THE DEFER IS LOAD-BEARING. Three of the eight refreshAborted() exits happen
// AFTER cookies.txt was rewritten, so a call placed after the error check
// returns first on exactly those passes — the ones whose write nobody
// compared. Hoisting one out is the obvious "simplification" and is invisible
// to every behavioural test in the tree. THE MUTANT: move any of the four out
// of its defer and place it after the `if err != nil` block.
//
// The fifth site is OnPassCompleted, which is NOT deferred and must not be: it
// is a hook fired later by notePassCompleted() from inside internal/cookies,
// through postRefreshRecheckHook's recover guard. It is pinned by its own
// shape. Its mutant: assign the bare closure, losing the recover, and a panic
// on the auto-cookie goroutine takes the process down.
func TestEveryCookieWriteRecheckIsDeferred(t *testing.T) {
	var sites []recheckSite
	for _, name := range []string{"monitor_callbacks.go", "services.go", "tui_wiring.go"} {
		sites = append(sites, recheckSitesIn(t, name)...)
	}

	// The reload-site table, by the gesture each call names. A gesture missing
	// here is a site deleted or renamed; an unexpected one is a new
	// credential-writing gesture whose shape nobody has decided.
	wantGestures := []string{
		"an automatic cookie refresh",
		"browser refresh",
		"recovery",
		"the job-triggered cookie refresh",
		"the setup wizard",
	}
	var gotGestures []string
	for _, s := range sites {
		gotGestures = append(gotGestures, s.gesture)
	}
	sort.Strings(gotGestures)
	if len(gotGestures) != len(wantGestures) {
		t.Fatalf("found %d recheckAfterCookieWrite call sites %v, want the %d in the Arc 10 reload-site table %v",
			len(gotGestures), gotGestures, len(wantGestures), wantGestures)
	}
	for i := range wantGestures {
		if gotGestures[i] != wantGestures[i] {
			t.Fatalf("call sites are %v, want %v — a gesture was renamed, or a new credential-writing site appeared with no decision about its shape",
				gotGestures, wantGestures)
		}
	}

	hooks := 0
	for _, s := range sites {
		switch {
		case s.inPassCompletedHook:
			hooks++
			if s.deferred {
				t.Errorf("%s:%d (%q) is BOTH the OnPassCompleted hook and deferred — one reading is wrong", s.file, s.line, s.gesture)
			}
		case !s.deferred:
			t.Errorf("%s:%d (%q) calls recheckAfterCookieWrite outside a defer. Three of the eight "+
				"refreshAborted() exits happen after cookies.txt was rewritten, so a call placed after "+
				"the error check is skipped on exactly the passes whose write nobody compared: the Twitch "+
				"auth mark taken under the old pair stands for up to thirty minutes over a file that no "+
				"longer has that problem, and no live chat session is told to reconnect",
				s.file, s.line, s.gesture)
		}
	}
	if hooks != 1 {
		t.Errorf("%d call sites are shaped as the OnPassCompleted hook, want exactly 1 (%q). The periodic "+
			"timer and the boot profile seed have no caller outside internal/cookies, so they are the one "+
			"site with no defer to sit in — and the hook must stay wrapped in postRefreshRecheckHook, "+
			"whose recover is the only thing between a panic there and the process",
			hooks, "an automatic cookie refresh")
	}
}

// recheckSitesIn parses one file of package main and reports every call to
// recheckAfterCookieWrite in it with the enclosing shapes.
//
// A parent STACK rather than nested ast.Inspect calls: "is this inside a defer"
// is a question about ancestry at any depth, and the enclosing func literal may
// be several nodes up.
func recheckSitesIn(t *testing.T, filename string) []recheckSite {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	var stack []ast.Node
	var found []recheckSite

	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "recheckAfterCookieWrite" {
			return true
		}

		site := recheckSite{file: filename, line: fset.Position(call.Pos()).Line, gesture: gestureArg(call)}
		// stack[len(stack)-1] is the call itself; walk outward.
		for i := len(stack) - 2; i >= 0; i-- {
			if _, isDefer := stack[i].(*ast.DeferStmt); isDefer {
				site.deferred = true
			}
			if outer, isCall := stack[i].(*ast.CallExpr); isCall && isPassCompletedHook(stack, i, outer) {
				site.inPassCompletedHook = true
			}
		}
		found = append(found, site)
		return true
	})
	return found
}

// gestureArg returns the 4th argument's string literal —
// recheckAfterCookieWrite(ctx, checkNow, log, gesture, args...) — or "" if it is
// not a literal, which is itself a finding: the gesture names the site in the
// log and in this test's table.
func gestureArg(call *ast.CallExpr) string {
	if len(call.Args) < 4 {
		return ""
	}
	lit, ok := call.Args[3].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING || len(lit.Value) < 2 {
		return ""
	}
	return lit.Value[1 : len(lit.Value)-1]
}

// isPassCompletedHook reports whether `outer` is the postRefreshRecheckHook
// call AND its result is assigned to a selector named OnPassCompleted.
//
// Both halves are required: the wrapper's name alone would accept a hook wired
// to something else, and the assignment alone would accept a bare closure with
// no recover around it — the mutant that matters, since a panic on the
// auto-cookie goroutine has nothing else between it and the process.
func isPassCompletedHook(stack []ast.Node, i int, outer *ast.CallExpr) bool {
	fn, ok := outer.Fun.(*ast.Ident)
	if !ok || fn.Name != "postRefreshRecheckHook" {
		return false
	}
	for j := i - 1; j >= 0; j-- {
		assign, ok := stack[j].(*ast.AssignStmt)
		if !ok {
			continue
		}
		for _, lhs := range assign.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "OnPassCompleted" {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 2: Run to verify it passes against the shipped code**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestEveryCookieWriteRecheckIsDeferred ./cmd/moombox/ -v`
Expected: PASS. Like Task 2, this is a PIN on existing shape; the failing half is Step 3. If it fails, read the failure: a gesture mismatch means the table drifted and `wantGestures` must be corrected against the code (and Arc 10's table noted as stale); a "not deferred" failure means a call really is misplaced and the CODE must be fixed.

- [ ] **Step 3: Mutations to run**

| # | Mutation | Expected failure |
|---|---|---|
Rows marked *(verified)* were run by the plan review in a worktree at `8558f5f`; the test passed against the shipped code first.

| # | Mutation | Expected failure |
|---|---|---|
| 1 | hoist the `"browser refresh"` call out of its `defer` (place it above `return result, err`) | `tui_wiring.go:NNN ("browser refresh") calls recheckAfterCookieWrite outside a defer` *(verified)* |
| 2 | hoist the `"recovery"` call out of its `defer` | the same message for `"recovery"` |
| 3 | assign `autoCookieSvc.OnPassCompleted = func(){ recheckAfterCookieWrite(...) }` directly | `0 call sites are shaped as the OnPassCompleted hook, want exactly 1` (and the "outside a defer" line for the same site) *(verified)* |
| 4 | change the `"the setup wizard"` gesture literal to `"wizard"` | `call sites are [...], want [...]` *(verified)* |
| 5 | delete the `"the job-triggered cookie refresh"` call | `found 4 recheckAfterCookieWrite call sites …, want the 5 …` |

- [ ] **Step 4: Verify line endings, run the gates, commit**

```bash
printf 'cmd/moombox/recheck_callsite_test.go: '; perl -0777 -ne 'print tr/\r//' cmd/moombox/recheck_callsite_test.go; echo
```
Expected `0`. Then the full gate block, then:

```bash
git add cmd/moombox/recheck_callsite_test.go
git commit -m "test(moombox): the five cookie-write re-check sites are pinned by shape

Four sit in a defer so the Ran/Wrote gate is evaluated independently of
the error return — three of the eight refreshAborted() exits happen after
cookies.txt was rewritten. The fifth is OnPassCompleted, pinned by its
own shape (the closure goes through postRefreshRecheckHook). Hoisting one
out of its defer was invisible to every behavioural test.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN"
```

---

### Task 7: R9 — the boot line and the post-flight sentences name the mechanism that ran

Arc 12c arc-close findings F1 and F2, homed here by controller ruling (2026-09-03). F1 moves ONE log
sentence out of a constructor that cannot know the acquisition mode into a method the wiring site
calls once it can. F2 puts the mechanism a refresh pass actually used onto `RefreshResult`, onto the
wire, and into ONE subject-producer per surface, pinned across the two by exact equality the way
Arc 12c pinned the pre-flight pair.

**This task is the H2 spec's ruled exception to "No REST/web change."** It adds exactly one additive
key to `cookieRefreshOutcome`, one exported function to `web/public/modules/utils.js`, and rewrites
the toast subjects in `web/public/app.js`. No route, no status code, no config key, no schema, no
new goroutine.

**Order.** Run AFTER Task 5: the tests below read `a.feedback.msg`, which is Task 5's fold. It also
touches `internal/cookies/autocookies.go` and `cmd/moombox/services.go`, so it must not run beside
Tasks 3 or 4.

**Files:**
- Modify: `internal/cookies/autocookies.go` — the constructor's verdict line (grep `profileDirErr := validateBrowserProfileDirForLaunch`); a new `LogProfileDirVerdict` after `readOnlyProfileDirErr` (`:1032-1043`); two new constants and one field on `RefreshResult` (grep `type RefreshResult struct`); `refreshCookiesDetailed`'s signature and the `importedFromProfile` decision (grep `importedFromProfile := browser == nil`)
- Modify: `cmd/moombox/services.go` — one call after the `AcquisitionMode` closure (grep `autoCookieSvc.AcquisitionMode = func`)
- Modify: `internal/web/routes/cookies.go` — `cookieRefreshOutcome` (`:55`) and its doc
- Modify: `internal/tui/app_actions.go` — a new `cookieRefreshMechanismLabel` after `cookieRefreshFeedback` (`:77-82`), plus the `internal/cookies` import
- Modify: `internal/tui/app_update.go` — the `noProfileFallback` resolution and the post-flight arms (`:387-435`): five arms re-subjected, rung 3 and `!Renewed` untouched
- Modify: `web/public/modules/utils.js` — a new export after `cookieRefreshPreflightToast` (`:654-658`)
- Modify: `web/public/app.js` — the import list (`:10`), one `const` after `:817`, the five result arms (`:847-871`), the transport arm (`:906`) and the catch (`:909`)
- Modify: `internal/web/routes/cookies_test.go` — one row in `TestAppJSReadsTheFieldsTheHandlerEmits` (`:388-400`)
- Modify: `internal/web/routes/cookies_shiftclick_test.go` — the premise assertion at `:194`
- Modify: `docs/spec/data-and-storage.md:897`, `docs/spec/user-interfaces.md:622-629`, `docs/spec/security.md:461`
- Modify: `docs/superpowers/plans/2026-09-02-arc12c-acquisition-mode.md:2327` and `:2328`;
  `docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md:181` (row 23's reading rule)
- Test: create `internal/cookies/autocookies_profiledir_verdict_test.go`,
  `internal/cookies/autocookies_mechanism_test.go`,
  `cmd/moombox/profiledir_verdict_callsite_test.go`,
  `internal/tui/cookie_postflight_mechanism_test.go`;
  add one case to `internal/web/routes/cookies_test.go`

**Interfaces:**
- Consumes: Task 5's `appFeedback` fold — every TUI assertion below reads `app.feedback.msg`, never
  `app.feedbackMsg`. Nothing else.
- Produces: `func (s *AutoCookieService) LogProfileDirVerdict()` (exported — `cmd/moombox` calls it);
  `const RefreshMechanismBrowser = "browser"`; `const RefreshMechanismProfileImport = "profile-import"`;
  field `RefreshResult.Mechanism string`; payload key `"mechanism"` on `cookieRefreshOutcome`;
  `func cookieRefreshMechanismLabel(mechanism, mode string) string` (`internal/tui`, unexported);
  `export function cookieRefreshMechanismLabel(mechanism, acquisition)` (`web/public/modules/utils.js`).
  `refreshCookiesDetailed`'s results become NAMED (`out RefreshResult, retErr error`); its signature
  is otherwise unchanged and `RefreshCookiesDetailed`'s is untouched.

**Lock preconditions, stated:**
- `LogProfileDirVerdict` — NO lock taken, and it must NOT be called with `s.mu` held: it calls
  `resolvedAcquisition()`, which reaches the config store's RWMutex through `AcquisitionMode`. Same
  rule as the four launch sites and `readOnlyProfileDirErr`. Reads `profileDirErr`, `profileDir`,
  `logger`, `AcquisitionMode` — all written once, before the service reaches any goroutine.
- `refreshCookiesDetailed`'s new `defer` — touches the named result and nothing else. It is
  registered BEFORE the function's first `s.mu.Lock()`, so it runs LAST, after every path has
  released the mutex. It takes no lock and must not.
- `cookieRefreshMechanismLabel` (Go) — pure. `a.cookieAcquisitionMode()` takes the config store's
  read lock and is called from the Bubble Tea update goroutine, exactly as the pre-flight sentence
  already does at `app_actions.go:239`.

**Named values, for the record:** wire key `mechanism`; Go field `RefreshResult.Mechanism`; values
`"browser"` / `"profile-import"` / `""`. The TUI gets NO new message field — `cookieForceRefreshResultMsg`
already carries `Result` whole (`internal/tui/app.go:238-254`), and `msg.Result.Mechanism` is the read.

- [ ] **Step 1: Write the failing F1 tests**

Create `internal/cookies/autocookies_profiledir_verdict_test.go`:

```go
package cookies

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// levelTaggedLogger records the LEVEL with the message and the args, which is
// what these three tests are about: the same verdict has to arrive at two
// different levels depending on the acquisition mode, and a logger that keeps
// only the text cannot tell the fix from the defect.
//
// Its own type rather than capturingLogger next door: that one folds Error into
// msgs without a level-specific slice, so "logged at ERROR" is not assertable
// through it.
type levelTaggedLogger struct {
	lines []loggedLine
}

type loggedLine struct {
	level string
	msg   string
	args  []any
}

func (l *levelTaggedLogger) record(level, msg string, args ...any) {
	l.lines = append(l.lines, loggedLine{level: level, msg: msg, args: args})
}

func (l *levelTaggedLogger) Debug(msg string, args ...any) { l.record("debug", msg, args...) }
func (l *levelTaggedLogger) Info(msg string, args ...any)  { l.record("info", msg, args...) }
func (l *levelTaggedLogger) Warn(msg string, args ...any)  { l.record("warn", msg, args...) }
func (l *levelTaggedLogger) Error(msg string, args ...any) { l.record("error", msg, args...) }

// at returns every line logged at one level, rendered as message plus args, so
// an assertion about the CONTENT reads the whole line and not just its heading.
// The guard's refusal travels as an error ARG, not in the message, so a helper
// that returned msg alone could not see it.
func (l *levelTaggedLogger) at(level string) []string {
	var out []string
	for _, ln := range l.lines {
		if ln.level != level {
			continue
		}
		out = append(out, ln.msg+" "+fmt.Sprint(ln.args...))
	}
	return out
}

// TestProfileDirVerdictIsSilentAtConstruction is F1's first half.
//
// NewAutoCookieService cannot know the acquisition mode: cmd/moombox builds the
// service and only then wires AcquisitionMode, so an ERROR chosen in the
// constructor is chosen blind — and on the configuration the README recipe
// prescribes (cookies.acquisition = "profile" pointed at a REAL profile) it was
// wrong on every boot. The verdict is still computed here; the sentence is not
// said here.
//
// Zero lines AT ANY LEVEL, not "no error line": a Warn or an Info emitted from
// the constructor would be the same defect at a quieter volume, and the mode is
// no more knowable for it.
//
// Mutation: put the `if profileDirErr != nil && logger != nil { logger.Error(...) }`
// block back in NewAutoCookieService.
func TestProfileDirVerdictIsSilentAtConstruction(t *testing.T) {
	log := &levelTaggedLogger{}
	s := NewAutoCookieService(dangerousProfileDir,
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)

	if s.profileDirErr == nil {
		t.Fatal("premise broken: the fixture is not a refused profile dir, so nothing would be logged anyway")
	}
	if len(log.lines) != 0 {
		t.Errorf("the constructor logged %d line(s) about a verdict it cannot level correctly: %v",
			len(log.lines), log.lines)
	}
}

// TestProfileDirVerdictLevelFollowsTheMode is F1's second half: the same
// verdict, two levels, chosen where the mode is finally knowable.
//
// Under "auto" a refused directory means a browser refresh the operator expects
// will silently not happen — ERROR, wording unchanged, because "refusing to
// launch a headless session against it" is the cue. Under "profile" nothing was
// going to launch, the read-only import runs regardless, and an ERROR names a
// failure that did not occur — one INFO that says both halves instead.
//
// Each half asserts the level AND the content, because either alone passes a
// mutant: a right-level line saying the wrong thing, or the right sentence at
// the wrong level. The auto half also pins today's line VERBATIM — message
// and both args — because the arc-close asked only for the profile case to
// change, and a Contains on the refusal wording would pass a reworded heading.
func TestProfileDirVerdictLevelFollowsTheMode(t *testing.T) {
	t.Run("auto", func(t *testing.T) {
		log := &levelTaggedLogger{}
		s := NewAutoCookieService(dangerousProfileDir,
			filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
		s.AcquisitionMode = func() string { return AcquisitionAuto }

		s.LogProfileDirVerdict()

		errs := log.at("error")
		if len(errs) != 1 {
			t.Fatalf("want exactly one ERROR line under auto, got %d: %v", len(errs), errs)
		}
		if !strings.Contains(errs[0], "refusing to launch") {
			t.Errorf("the auto line dropped the guard's own refusal wording, which is the operator's "+
				"cue that a launch they expect will not happen: %q", errs[0])
		}
		if got := len(log.at("info")); got != 0 {
			t.Errorf("auto also logged %d INFO line(s); the verdict is said once, at one level", got)
		}
		// Today's line, byte for byte: the message, the "err" key and the
		// guard's own error value. The Contains above is the operator's cue;
		// this is the claim that nothing under auto changed at all.
		for _, ln := range log.lines {
			if ln.level != "error" {
				continue
			}
			if ln.msg != "auto-cookie profile dir rejected at construction" || len(ln.args) != 2 ||
				ln.args[0] != "err" || ln.args[1] != any(s.profileDirErr) {
				t.Errorf("the auto line is not today's line verbatim: msg=%q args=%v", ln.msg, ln.args)
			}
		}
	})

	t.Run("profile", func(t *testing.T) {
		log := &levelTaggedLogger{}
		s := NewAutoCookieService(dangerousProfileDir,
			filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
		s.AcquisitionMode = func() string { return AcquisitionProfile }

		s.LogProfileDirVerdict()

		if got := log.at("error"); len(got) != 0 {
			t.Errorf("the README-prescribed configuration still logs at ERROR on every boot: %v", got)
		}
		if got := log.at("warn"); len(got) != 0 {
			t.Errorf("the profile line was downgraded to WARN rather than stated as the normal case: %v", got)
		}
		infos := log.at("info")
		if len(infos) != 1 {
			t.Fatalf("want exactly one INFO line under profile, got %d: %v", len(infos), infos)
		}
		if strings.Contains(infos[0], "refusing to launch") {
			t.Errorf("the profile line claims a refused launch, on a path that launches nothing: %q", infos[0])
		}
		for _, want := range []string{"no headless browser will be launched", "read-only import"} {
			if !strings.Contains(infos[0], want) {
				t.Errorf("the profile line does not say %q, so it does not tell the operator which "+
					"mechanism actually runs: %q", want, infos[0])
			}
		}
	})
}

// TestProfileDirVerdictSaysNothingForAnAcceptableDir is the premise for both
// tests above: the line is about a REFUSED directory, and an ordinary install
// — which is nearly every install — must see nothing at all.
//
// Mutation: drop the `s.profileDirErr == nil` guard from LogProfileDirVerdict.
func TestProfileDirVerdictSaysNothingForAnAcceptableDir(t *testing.T) {
	for _, mode := range []string{AcquisitionAuto, AcquisitionProfile} {
		t.Run(mode, func(t *testing.T) {
			log := &levelTaggedLogger{}
			s := NewAutoCookieService(t.TempDir(),
				filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), log)
			s.AcquisitionMode = func() string { return mode }
			if s.profileDirErr != nil {
				t.Fatalf("premise broken: a plain temp dir was refused: %v", s.profileDirErr)
			}

			s.LogProfileDirVerdict()

			if len(log.lines) != 0 {
				t.Errorf("an ordinary profile dir produced %d boot line(s): %v", len(log.lines), log.lines)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestProfileDirVerdict' ./internal/cookies/`
Expected: FAIL to COMPILE — `s.LogProfileDirVerdict undefined`.

- [ ] **Step 3: Move the verdict's sentence out of the constructor and add the method**

In `internal/cookies/autocookies.go`, replace the four lines

```go
	profileDirErr := validateBrowserProfileDirForLaunch(profileDir)
	if profileDirErr != nil && logger != nil {
		logger.Error("auto-cookie profile dir rejected at construction", "err", profileDirErr)
	}
```

with (keeping the whole comment block above them exactly as it is, and appending this paragraph to
its tail):

```go
	//
	// Computed here, SAID somewhere else. cmd/moombox builds the service and
	// only afterwards wires AcquisitionMode, so at this point the mode is not
	// knowable and any level chosen here is chosen blind — which is how an
	// install following the README's `cookies.acquisition = "profile"` recipe
	// logged a red "profile dir rejected ... refusing to launch" on every boot,
	// for the directory it is SUPPOSED to point at (Arc 12c arc-close F1). The
	// verdict and every reader of it are unchanged; only the sentence moved, to
	// LogProfileDirVerdict, which the wiring site calls once the mode is there.
	profileDirErr := validateBrowserProfileDirForLaunch(profileDir)
```

Then, immediately after `readOnlyProfileDirErr`'s closing brace:

```go
// LogProfileDirVerdict says ONCE what the launch guard decided about the
// configured browser profile directory, at the level the acquisition mode
// earns. Silent when the directory is fine, which is nearly every install.
//
// A method rather than a line in NewAutoCookieService because the constructor
// runs BEFORE AcquisitionMode is wired (cmd/moombox/services.go builds the
// service, then assigns the callbacks), so it cannot tell the two
// configurations apart and logged the same red line for both.
//
// Under "auto" that line is right, and its wording is kept verbatim: a browser
// refresh will be refused this directory, so an operator has a launch they
// believe is happening and is not, and ERROR is the level that gets read.
//
// Under "profile" nothing was going to launch anyway. The refusal describes an
// event that never occurs, and the pass that DOES run — the read-only import,
// which copies cookies.sqlite and its -wal into a 0700 temp dir and opens the
// COPY mode=ro — is not affected by the guard at all. So that mode gets one
// INFO saying both halves out loud, because an operator who followed the README
// recipe is owed an acknowledgement rather than a rejection.
//
// The verdict itself is untouched: validateBrowserProfileDirForLaunch is still
// called exactly once, at construction, and the four subprocess sites still
// read s.profileDirErr directly, in every mode (see the field's comment and
// TestLaunchGuardHoldsEveryLaunchSiteInEveryMode). This changes a sentence, not
// a decision.
//
// NO LOCK, and it must not be called with s.mu held: resolvedAcquisition
// reaches the config store's own read lock through AcquisitionMode, which is
// the same rule the launch sites and readOnlyProfileDirErr already follow.
// Everything it reads — profileDirErr, profileDir, logger, AcquisitionMode —
// is written once, before the service is handed to any goroutine. Called once,
// from the wiring sequence.
func (s *AutoCookieService) LogProfileDirVerdict() {
	if s.profileDirErr == nil || s.logger == nil {
		return
	}
	if s.resolvedAcquisition() == AcquisitionProfile {
		s.logger.Info("browser profile dir sits inside a real installed browser's profile tree — "+
			"no headless browser will be launched against it; cookies.acquisition is \"profile\", "+
			"so the read-only import is what runs",
			"profile_dir", s.profileDir)
		return
	}
	s.logger.Error("auto-cookie profile dir rejected at construction", "err", s.profileDirErr)
}
```

- [ ] **Step 4: Call it from the one site that knows the mode**

In `cmd/moombox/services.go`, immediately after the `autoCookieSvc.AcquisitionMode = func() string { … }`
closure and immediately before `s.autoCookieSvc = autoCookieSvc`:

```go
	// The launch guard's verdict, said once, at the level the mode earns.
	//
	// HERE and not in the constructor, and the ORDER is the whole point: the
	// service is built at the top of this block with no AcquisitionMode, so a
	// line logged there cannot tell "auto" — where a refused directory means a
	// browser refresh the operator expects will not happen, ERROR — from
	// "profile", where nothing was going to launch and the read-only import
	// runs regardless, INFO. Hoisting this call above the closure above puts
	// the red line back on every profile-mode boot, which is Arc 12c
	// arc-close F1; TestProfileDirVerdictIsLoggedAfterTheModeIsWired fails if
	// it moves. Silent unless the directory is actually refused.
	autoCookieSvc.LogProfileDirVerdict()
```

- [ ] **Step 5: Write the failing call-site test**

Create `cmd/moombox/profiledir_verdict_callsite_test.go`:

```go
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestProfileDirVerdictIsLoggedAfterTheModeIsWired pins the ORDER, which is the
// only thing that makes the fix a fix.
//
// LogProfileDirVerdict picks its level from resolvedAcquisition, which reads
// AcquisitionMode; a nil callback resolves to "auto". So calling it before the
// AcquisitionMode closure is assigned reproduces Arc 12c arc-close F1 exactly —
// an ERROR on every boot of the README's profile-mode recipe — while every
// behavioural test in internal/cookies stays green, because they wire the
// callback themselves. Nothing but the call ORDER in this file can catch it,
// and this package cannot drive initServices.
//
// Structural for the same reason internal/web/routes'
// cookies_import_callsite_test.go is: the seam is a statement's position, and
// there is no seam to inject.
//
// THE THREE MUTANTS:
//   - hoist the call above the AcquisitionMode assignment: the index check fails.
//   - delete the call: the "not found" fatal fires.
//   - move the call into some other function: initServices' body no longer
//     contains it, and the same fatal fires.
func TestProfileDirVerdictIsLoggedAfterTheModeIsWired(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "services.go", nil, 0)
	if err != nil {
		t.Fatalf("parse services.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "initServices" && fn.Body != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("services.go has no initServices with a body — re-anchor this test rather than deleting it")
	}

	assignIdx, callIdx := -1, -1
	for i, stmt := range body.List {
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) == 1 && selectorIs(s.Lhs[0], "autoCookieSvc", "AcquisitionMode") {
				assignIdx = i
			}
		case *ast.ExprStmt:
			call, ok := s.X.(*ast.CallExpr)
			if ok && selectorIs(call.Fun, "autoCookieSvc", "LogProfileDirVerdict") {
				callIdx = i
			}
		}
	}

	if assignIdx < 0 {
		t.Fatal("initServices no longer assigns autoCookieSvc.AcquisitionMode at its top level — " +
			"the ordering this test exists for cannot be checked")
	}
	if callIdx < 0 {
		t.Fatal("initServices never calls autoCookieSvc.LogProfileDirVerdict, so the launch guard's " +
			"verdict is never reported at all: a refused browser_profile_dir now boots silently in " +
			"both modes")
	}
	if callIdx < assignIdx {
		t.Errorf("LogProfileDirVerdict is called at statement %d, before AcquisitionMode is wired at "+
			"%d — resolvedAcquisition reads nil as \"auto\", so a cookies.acquisition = \"profile\" "+
			"install logs the launch refusal at ERROR on every boot (Arc 12c arc-close F1)",
			callIdx, assignIdx)
	}
}

// selectorIs reports whether e is exactly `recv.sel`.
func selectorIs(e ast.Expr, recv, sel string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != sel {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == recv
}
```

- [ ] **Step 6: Run the F1 half**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestProfileDirVerdict' ./internal/cookies/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestProfileDirVerdictIsLoggedAfterTheModeIsWired' ./cmd/moombox/ -v
```
Expected: PASS — three tests, five subtests (`…IsSilentAtConstruction`, `…LevelFollowsTheMode/auto`,
`…/profile`, `…SaysNothingForAnAcceptableDir/auto`, `…/profile`) plus the call-site test.

- [ ] **Step 7: Write the failing mechanism test**

Create `internal/cookies/autocookies_mechanism_test.go`:

```go
package cookies

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRefreshResultCarriesTheMechanismThatRan is F2 at the source.
//
// Every post-flight sentence on both surfaces opened with "Browser cookie
// refresh ..." after a pass that launched nothing, because the only thing
// either surface had to go on was cookies.acquisition — and the mode is not the
// answer. A host with no browser installed imports in "auto" mode and always
// has, long before the setting existed. So the pass reports what it actually
// did, and this is the assertion that it does.
//
// FOUR ROWS, and the empty one is the point of the design. Mechanism is
// stamped where the path is CHOSEN (the importedFromProfile decision), so a
// pass that declined above that decision reports "" rather than guessing — and
// "" is what the surfaces fall back to the mode for. A field that guessed would
// be worse than the mode alone, because it would look authoritative.
//
// The second row is the boundary, and it is where a first draft of this test
// was wrong: the browser branch's empty-jar gate (`len(refreshPlatforms()) ==
// 0`) is a decline that sits BELOW the decision, so it carries "browser" — the
// branch was chosen and then declined, and the mode fallback would have said
// the same. The decline that is genuinely above the decision, and cheap to
// drive, is the single-flight slot: refreshCmd non-nil means "already
// refreshing", and the pass returns before it looks at anything.
//
// The fixture is TestAcquisitionModeSelectsTheRefreshPath's, deliberately: same
// synthetic WAL profile, same gatedBrowser at a path that does not exist, so
// nothing here can execute a browser however the branch goes.
//
// Mutations: delete the defer that stamps it; initialise mechanism to a
// non-empty value; swap the two constants at the decision.
func TestRefreshResultCarriesTheMechanismThatRan(t *testing.T) {
	newService := func(t *testing.T, mode string, jar *CookieJar) *AutoCookieService {
		t.Helper()
		s := NewAutoCookieService(
			writeWALCookieProfile(t, youtubeAuthRows()),
			filepath.Join(t.TempDir(), "cookies.txt"),
			jar, nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
		s.AcquisitionMode = func() string { return mode }
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		return s
	}

	t.Run("a pass that declined above the decision reports no mechanism", func(t *testing.T) {
		// The single-flight slot is held (the same sentinel refreshFirefox
		// claims), so the pass declines at the "already refreshing" gate —
		// above the launch-vs-import decision, in EVERY mode. Profile mode
		// here, so a stamp that leaked would be the import's, which is not
		// what the mode fallback says for auto. Nothing was chosen, so nothing
		// is claimed.
		s := newService(t, AcquisitionProfile, NewCookieJar())
		s.mu.Lock()
		s.refreshCmd = &exec.Cmd{}
		s.mu.Unlock()
		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("this row must not error: %v", err)
		}
		if result.Ran {
			t.Fatal("premise broken: the in-flight gate no longer declines, so this row is not testing a decline")
		}
		if result.Mechanism != "" {
			t.Errorf("Mechanism = %q for a pass that never chose a path, want \"\" — a guessed "+
				"mechanism reads as authoritative and is exactly the claim F2 is about", result.Mechanism)
		}
	})

	t.Run("a browser-path decline on an empty jar reports the browser", func(t *testing.T) {
		// auto + a browser + an empty jar: the browser branch is CHOSEN, and
		// then its refreshPlatforms() gate declines. That gate is below the
		// decision, so the stamp has already happened — and "browser" is the
		// truth of it. The mode fallback says the same for this row, which is
		// why nothing observable turns on it; what this pins is WHERE the
		// stamp sits, so a later reader does not "fix" the row above by
		// moving the stamp into the branches.
		s := newService(t, AcquisitionAuto, NewCookieJar())
		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("this row must not error: %v", err)
		}
		if result.Ran {
			t.Fatal("premise broken: the empty-jar gate no longer declines")
		}
		if result.Mechanism != RefreshMechanismBrowser {
			t.Errorf("Mechanism = %q, want %q — the browser path was chosen before it declined",
				result.Mechanism, RefreshMechanismBrowser)
		}
	})

	t.Run("the import path names itself", func(t *testing.T) {
		s := newService(t, AcquisitionProfile, NewCookieJar())
		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("this row must not error: %v", err)
		}
		if !result.Ran {
			t.Fatal("premise broken: profile mode no longer takes the import path")
		}
		if result.Mechanism != RefreshMechanismProfileImport {
			t.Errorf("Mechanism = %q, want %q — this pass launched nothing and every sentence "+
				"about it still said \"Browser cookie refresh\"", result.Mechanism, RefreshMechanismProfileImport)
		}
		// The reason both surfaces keep their browser wording on the !Renewed
		// arm: renewed := importedFromProfile || browserActed, and browserActed
		// starts true and is cleared only inside the browser branch, so an
		// import that reaches a verdict always renewed — held shut by two
		// guards, each sufficient alone. This pins the PROPERTY: dropping
		// `importedFromProfile ||` alone survives it (verified), because the
		// initialiser still holds; only a change that lets an import reach a
		// verdict with Renewed false fails it, and that is the change that
		// would start telling a profile-mode operator their BROWSER could not
		// be confirmed.
		if !result.Renewed {
			t.Error("an import that ran reported Renewed = false, which makes the browser-worded " +
				"\"could not confirm the browser refreshed them\" arm reachable for a pass that " +
				"launched no browser")
		}
	})

	t.Run("the browser path names itself", func(t *testing.T) {
		// A jar with YouTube auth puts a platform in refreshPlatforms(), so the
		// pass gets past the gate that declined the first row and takes the
		// launch branch — which cannot execute anything, and does not need to:
		// the stamp happens at the decision, above every launch.
		s := newService(t, AcquisitionAuto, jarWithAuth(t))
		result, _ := s.RefreshCookiesDetailed(context.Background())
		if !result.Ran {
			t.Fatal("premise broken: the browser branch declined, so no mechanism was ever chosen")
		}
		if result.Mechanism != RefreshMechanismBrowser {
			t.Errorf("Mechanism = %q, want %q", result.Mechanism, RefreshMechanismBrowser)
		}
	})
}
```

- [ ] **Step 8: Run to verify it fails**

Run: `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run TestRefreshResultCarriesTheMechanismThatRan ./internal/cookies/`
Expected: FAIL to COMPILE — `result.Mechanism undefined`, `undefined: RefreshMechanismProfileImport`.

- [ ] **Step 9: Add the two constants, the field, and the one stamp**

In `internal/cookies/autocookies.go`, immediately ABOVE `type RefreshResult struct`:

```go
// The two values of RefreshResult.Mechanism: which cookie source a refresh pass
// actually used. Exported because internal/web/routes puts them on the wire and
// internal/tui renders them, and because a literal repeated across three
// packages and one JavaScript file is how a vocabulary drifts.
//
// Deliberately NOT the cookies.acquisition values, and not spelled like them.
// Those name what the operator ASKED for; these name what happened, and the two
// differ on every host where no browser resolves — a container in "auto" mode
// imports, and did so years before the setting existed.
const (
	RefreshMechanismBrowser       = "browser"
	RefreshMechanismProfileImport = "profile-import"
)
```

Immediately after `TwitchStored  bool` inside the struct:

```go

	// Mechanism is which cookie source this pass actually used —
	// RefreshMechanismBrowser or RefreshMechanismProfileImport — or "" when it
	// stopped before choosing one. Every exit above the importedFromProfile
	// decision is "": stopped service, setup in flight, refresh in flight, and
	// the three no-source errors (no browser and no profile; launches disabled
	// and no profile; profile not found). The one decline BELOW it — the
	// browser branch's empty-jar gate — carries "browser": the path was chosen
	// before it declined.
	//
	// WORDING ONLY, exactly the rule YouTubeStored / TwitchStored carry above.
	// Nothing may branch a decision on it: whether the credentials work is what
	// the verdicts are for, and whether this pass produced them is Renewed.
	// What it answers is the question every post-flight sentence was getting
	// wrong — "Browser cookie refresh successful" after an import that launched
	// nothing (Arc 12c arc-close F2).
	//
	// "" is not a fourth state to render. Both surfaces fall back to
	// cookies.acquisition for the sentence's subject when it is empty, which is
	// also what an OLDER binary's payload degrades to, since it carries no
	// `mechanism` key at all — the same additive rule `ran` and `verdict` set.
	Mechanism string
```

Change `refreshCookiesDetailed`'s signature to named results and register the stamp as its first
statement (the `RefreshCookiesDetailed` wrapper above it is UNCHANGED):

```go
func (s *AutoCookieService) refreshCookiesDetailed(ctx context.Context, policy browserGatePolicy) (out RefreshResult, retErr error) {
	// ONE stamp for eighteen returns.
	//
	// Mechanism has to be true of every exit — eight aborts, seven declines
	// and three verdicts — and threading it through each return literal is
	// exactly how the nineteenth one gets added without it. The named result
	// plus this defer make the stamp structural instead: a return site added
	// later carries it whether or not its author knew the field existed.
	//
	// It starts empty and is set only where the path is actually chosen, at the
	// importedFromProfile decision below, so a pass that declined above that
	// point reports "" — the honest answer, and the one both surfaces know how
	// to fall back from. The one decline below that point — the browser
	// branch's empty-jar gate — carries "browser", because the branch WAS
	// chosen.
	//
	// NO LOCK: the closure touches the named result and nothing else, and it is
	// registered before the first s.mu.Lock() so it runs LAST, after every path
	// has already released the mutex.
	mechanism := ""
	defer func() { out.Mechanism = mechanism }()

	s.mu.Lock()
```

and, immediately after the `importedFromProfile` decision block:

```go
	importedFromProfile := browser == nil
	if s.resolvedAcquisition() == AcquisitionProfile {
		importedFromProfile = true
	}
	// The one place the mechanism is known. Everything above this line declined
	// without choosing; everything below it ran the branch named here, and the
	// defer at the top carries the answer out of whichever exit is taken.
	if importedFromProfile {
		mechanism = RefreshMechanismProfileImport
	} else {
		mechanism = RefreshMechanismBrowser
	}
```

- [ ] **Step 10: Run the cookies package**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 -run 'TestRefreshResultCarriesTheMechanismThatRan|TestProfileDirVerdict|TestAcquisitionMode|TestLaunchGuard|TestReadOnly|TestStartupSeed|TestPeriodicTick' ./internal/cookies/ -v
```
Expected: PASS. (The 12c acquisition and launch-guard tests are run alongside because Step 9 edited
the function they all drive.) Then the whole package once: `go test -count=1 ./internal/cookies/`.

- [ ] **Step 11: Put the mechanism on the wire**

In `internal/web/routes/cookies.go`, add a fourth bullet to `cookieRefreshOutcome`'s doc list
(after the `ran` bullet) and the key to the map:

```go
//   - mechanism — WHICH cookie source ran: "browser", "profile-import", or ""
//     when the pass declined before it chose one. Wording only: it exists so a
//     post-flight toast stops saying "Browser cookie refresh ..." after an
//     import that launched nothing, and nothing may branch a decision on it.
//     Additive like `ran` and `verdict` before it — an older frontend ignores it, and a newer
//     frontend against an older binary reads undefined and falls back to
//     cookies.acquisition, which is what it already used for the pre-flight
//     toast.
```

```go
func cookieRefreshOutcome(result cookies.RefreshResult) map[string]any {
	return map[string]any{
		"success":   result.AnyVerified(),
		"renewed":   result.Renewed,
		"verdict":   result.Overall().String(),
		"ran":       result.Ran,
		"mechanism": result.Mechanism,
	}
}
```

Append to `internal/web/routes/cookies_test.go`:

```go
// TestCookieRefreshOutcomeCarriesTheMechanism pins the additive key the two
// post-flight surfaces read.
//
// The empty row is the one with teeth. A pass that declined before choosing a
// path has no mechanism, and the payload has to say so rather than default to
// "browser": the dashboard treats empty and absent identically and falls back
// to cookies.acquisition, and a payload that asserted "browser" there would
// override that fallback with a guess.
func TestCookieRefreshOutcomeCarriesTheMechanism(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a browser pass", cookies.RefreshMechanismBrowser, "browser"},
		{"a profile import", cookies.RefreshMechanismProfileImport, "profile-import"},
		{"a pass that declined before choosing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cookieRefreshOutcome(cookies.RefreshResult{Ran: true, Mechanism: tc.in})
			if got["mechanism"] != tc.want {
				t.Errorf("mechanism = %v, want %q — the dashboard's toast subject comes from this key",
					got["mechanism"], tc.want)
			}
		})
	}
}
```

and one row in `TestAppJSReadsTheFieldsTheHandlerEmits`'s table, with the sentence that explains why
its expression is a read rather than a comparison:

```go
	for _, tc := range []struct{ key, expr string }{
		{"ran", "data.ran === false"},
		{"verdict", `data.verdict === "failed"`},
		// Read, not compared: app.js hands this to cookieRefreshMechanismLabel
		// rather than branching on it, and the label owns the fallback for an
		// absent or empty value. What must not drift is the NAME.
		{"mechanism", "data.mechanism"},
	} {
```

- [ ] **Step 12: One subject-producer per surface, and the cross-surface pin**

In `internal/tui/app_actions.go`, add `"github.com/vampiricwulf/Moombox/internal/cookies"` to the
import block (after `internal/constants`, before `internal/database`), and after
`cookieRefreshFeedback`:

```go
// cookieRefreshMechanismLabel is the SUBJECT of every post-flight sentence R F
// renders: the mechanism that actually ran.
//
// cookieRefreshFeedback above names what WILL run, from the mode, because
// before the pass that is all there is. Afterwards the pass knows better —
// RefreshResult.Mechanism is what it chose — and the two disagree wherever the
// HOST decides rather than the setting: a machine with no browser installed
// imports in "auto" mode and always has. That is why every post-flight sentence
// said "Browser cookie refresh ..." after an import (Arc 12c arc-close F2), and
// why this reads the RESULT first.
//
// The mode is the fallback, not the source. An empty Mechanism means the pass
// declined before choosing — a setup in flight, nothing worth refreshing — and
// the mode is then the best answer available AND the one the pre-flight
// sentence already gave, so the two lines agree rather than contradict.
//
// The dashboard's twin is cookieRefreshMechanismLabel in
// web/public/modules/utils.js;
// TestRefreshPostflightMechanismAgreesAcrossSurfaces pins the two by exact
// equality over every combination, the way
// TestRefreshPreflightSentenceAgreesAcrossSurfaces pins the pre-flight pair.
// Like that pair these name no per-surface affordance, so they do not diverge;
// unlike the rung-3 pair, which does and must.
func cookieRefreshMechanismLabel(mechanism, mode string) string {
	switch mechanism {
	case cookies.RefreshMechanismProfileImport:
		return "Browser-profile cookie import"
	case cookies.RefreshMechanismBrowser:
		return "Browser cookie refresh"
	}
	if mode == cookies.AcquisitionProfile {
		return "Browser-profile cookie import"
	}
	return "Browser cookie refresh"
}
```

In `web/public/modules/utils.js`, after `cookieRefreshPreflightToast`:

```js
/**
 * cookieRefreshMechanismLabel is the SUBJECT of every post-flight toast the
 * manual refresh renders: the mechanism that actually ran.
 *
 * cookieRefreshPreflightToast above names what WILL run, from the mode, because
 * before the pass that is all there is. Afterwards the server knows better —
 * `mechanism` on the /api/cookies/auto-refresh payload is the path the pass
 * chose — and the two disagree wherever the HOST decides rather than the
 * setting: a machine with no browser installed imports in "auto" mode and
 * always has. Every result toast said "Browser cookie refresh ..." after such
 * an import, which is why this reads the payload first.
 *
 * The mode is the fallback, not the source, and it covers two cases with one
 * rule: a pass that declined before choosing sends an empty `mechanism`, and an
 * OLDER binary sends no such key at all. Both land here, and both get the
 * sentence the pre-flight toast already used.
 *
 * The TUI's twin is cookieRefreshMechanismLabel in internal/tui/app_actions.go;
 * the two are pinned to identical output by exact equality
 * (TestRefreshPostflightMechanismAgreesAcrossSurfaces).
 */
export function cookieRefreshMechanismLabel(mechanism, acquisition) {
  if (mechanism === "profile-import") return "Browser-profile cookie import";
  if (mechanism === "browser") return "Browser cookie refresh";
  return acquisition === "profile"
    ? "Browser-profile cookie import"
    : "Browser cookie refresh";
}
```

Create `internal/tui/cookie_postflight_mechanism_test.go`:

```go
package tui

import (
	"testing"

	"github.com/dop251/goja"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestRefreshPostflightMechanismAgreesAcrossSurfaces is the cross-UI pin for
// the post-flight subject, built exactly like
// TestRefreshPreflightSentenceAgreesAcrossSurfaces next door: literal sentences
// first so the test is self-checking, then the SHIPPED utils.js run through
// goja and compared to the Go renderer by exact equality — never Contains,
// because "Browser cookie refresh" is a substring of nothing here but a drift
// that kept one string a prefix of the other must still fail.
//
// The grid is every mechanism against every mode, including the values neither
// side should ever see. That is what pins the PRECEDENCE: a renderer that read
// the mode first would agree with its twin on twelve of the sixteen rows.
//
// The mechanism values arrive as the Go CONSTANTS, so a rename that changes
// their VALUE is caught here too — the JS compares literals, so a changed
// constant falls through its two arms to the mode fallback and the two answers
// part company on the rows where the mode disagrees.
func TestRefreshPostflightMechanismAgreesAcrossSurfaces(t *testing.T) {
	const (
		browserLabel = "Browser cookie refresh"
		importLabel  = "Browser-profile cookie import"
	)
	for _, tc := range []struct{ mechanism, mode, want string }{
		{cookies.RefreshMechanismBrowser, "auto", browserLabel},
		{cookies.RefreshMechanismBrowser, "profile", browserLabel},
		{cookies.RefreshMechanismProfileImport, "auto", importLabel},
		{cookies.RefreshMechanismProfileImport, "profile", importLabel},
		{"", "auto", browserLabel},
		{"", "profile", importLabel},
		{"", "", browserLabel},
	} {
		if got := cookieRefreshMechanismLabel(tc.mechanism, tc.mode); got != tc.want {
			t.Errorf("cookieRefreshMechanismLabel(%q, %q) = %q, want %q",
				tc.mechanism, tc.mode, got, tc.want)
		}
	}
	// The RESULT outranks the mode, and these two rows are the only place that
	// is visible: a renderer that consulted the mode first answers the import
	// label for the first and the browser label for the second.
	if got := cookieRefreshMechanismLabel(cookies.RefreshMechanismBrowser, "profile"); got != browserLabel {
		t.Errorf("a browser pass in profile mode is labelled %q — the mode was consulted ahead of "+
			"what the pass actually did", got)
	}
	if got := cookieRefreshMechanismLabel(cookies.RefreshMechanismProfileImport, "auto"); got != importLabel {
		t.Errorf("an import in auto mode is labelled %q — this is the host-decided case that made "+
			"every post-flight sentence wrong before the mode setting existed", got)
	}

	vm := utilsModuleVM(t)
	fn, ok := goja.AssertFunction(vm.Get("cookieRefreshMechanismLabel"))
	if !ok {
		t.Fatal("utils.js does not export cookieRefreshMechanismLabel — the dashboard's post-flight " +
			"toasts have no shared subject and the two surfaces cannot be held together")
	}
	for _, mechanism := range []string{
		cookies.RefreshMechanismBrowser, cookies.RefreshMechanismProfileImport, "", "headless",
	} {
		for _, mode := range []string{"auto", "profile", "", "browser"} {
			v, err := fn(goja.Undefined(), vm.ToValue(mechanism), vm.ToValue(mode))
			if err != nil {
				t.Fatalf("mechanism %q mode %q: %v", mechanism, mode, err)
			}
			if web, tui := v.String(), cookieRefreshMechanismLabel(mechanism, mode); web != tui {
				t.Errorf("mechanism %q mode %q: the dashboard says %q and the TUI says %q — one "+
					"pass, two names for what ran", mechanism, mode, web, tui)
			}
		}
	}
	// An ABSENT argument, which is what an older binary's payload produces on
	// the dashboard (`data.mechanism` is undefined, not ""). It must land on
	// the mode fallback rather than on some third answer.
	undef, err := fn(goja.Undefined(), goja.Undefined(), vm.ToValue("profile"))
	if err != nil {
		t.Fatalf("undefined mechanism: %v", err)
	}
	if got := undef.String(); got != importLabel {
		t.Errorf("an older binary's payload (no mechanism key) is labelled %q in profile mode, want "+
			"%q — the fallback is the whole reason the key can be additive", got, importLabel)
	}
}

// TestPostFlightSentencesNameTheMechanismThatRan is the TUI half, through the
// real Update loop, because the sentence is assembled at the arm and not in the
// label function.
//
// Three rows, three different ways the subject is reached: the result says
// "import", the result says "browser", and the result says nothing so the mode
// answers. The browser rows are asserted BYTE-IDENTICALLY to what shipped
// before, because feedbackColor classifies by substring and
// TestFeedbackColorWarningMessages pins two of these strings verbatim — this
// change re-subjects the import case and must leave the browser case alone.
func TestPostFlightSentencesNameTheMechanismThatRan(t *testing.T) {
	appIn := func(t *testing.T, mode string) *App {
		t.Helper()
		a := NewApp()
		cfg := config.Defaults()
		cfg.Cookies.Acquisition = mode
		a.configStore = config.NewStore(cfg, "")
		return a
	}

	t.Run("an import that succeeded", func(t *testing.T) {
		a := appIn(t, "profile")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, Renewed: true, YouTube: cookies.RefreshOK,
			Mechanism: cookies.RefreshMechanismProfileImport,
		}})
		if want := "Browser-profile cookie import successful"; a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q — the pass launched no browser", a.feedback.msg, want)
		}
	})

	t.Run("a browser pass that succeeded keeps its shipped wording", func(t *testing.T) {
		a := appIn(t, "auto")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, Renewed: true, YouTube: cookies.RefreshOK,
			Mechanism: cookies.RefreshMechanismBrowser,
		}})
		if want := "Browser cookie refresh successful"; a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q — the browser wording is unchanged by this task", a.feedback.msg, want)
		}
	})

	t.Run("a decline in profile mode falls back to the mode", func(t *testing.T) {
		// refreshDeclined() is the zero RefreshResult: Ran false, Mechanism "".
		// The pass never chose, so the sentence's subject comes from the same
		// setting the pre-flight line used a moment earlier — the two lines
		// have to agree or the operator watched one gesture describe itself
		// twice, differently.
		a := appIn(t, "profile")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{}})
		want := "Browser-profile cookie import declined to run (" + cookies.RefreshDeclinedCauses +
			") — nothing was learned about these cookies"
		if a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q", a.feedback.msg, want)
		}
	})

	t.Run("a host-decided import in auto mode", func(t *testing.T) {
		// THE ROW THAT MAKES THE PRECEDENCE OBSERVABLE, and the case that made
		// every post-flight sentence wrong years before cookies.acquisition
		// existed: no browser on the host, so the pass imports while the mode
		// still says "auto". A renderer that consulted the mode would say
		// "Browser cookie refresh successful" here, and did.
		a := appIn(t, "auto")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, Renewed: true, YouTube: cookies.RefreshOK,
			Mechanism: cookies.RefreshMechanismProfileImport,
		}})
		if want := "Browser-profile cookie import successful"; a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q — the mode was consulted ahead of what the pass did",
				a.feedback.msg, want)
		}
	})

	t.Run("a browser pass that verified but could not confirm keeps its arm", func(t *testing.T) {
		// The one arm that names the browser and is NOT re-subjected: an import
		// forces Renewed true (renewed := importedFromProfile || browserActed),
		// so it is unreachable for one — pinned in internal/cookies by
		// TestRefreshResultCarriesTheMechanismThatRan's Renewed assertion.
		a := appIn(t, "auto")
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, Renewed: false, YouTube: cookies.RefreshOK,
			Mechanism: cookies.RefreshMechanismBrowser,
		}})
		if want := "Cookies still work, but this pass could not confirm the browser refreshed them"; a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q", a.feedback.msg, want)
		}
	})
}
```

- [ ] **Step 13: Use the label in the TUI arms**

In `internal/tui/app_update.go`, insert between the `noProfileFallback` resolution and the `switch`:

```go
		// The SUBJECT of every arm below, from what the pass ACTUALLY did —
		// msg.Result.Mechanism — with the configured mode as the fallback for a
		// pass that declined before choosing. See cookieRefreshMechanismLabel.
		// Computed once rather than per arm: the rung-3 arm below does not use
		// it, and one config-store read on that path is cheaper than five call
		// sites that can drift.
		mechanismLabel := cookieRefreshMechanismLabel(msg.Result.Mechanism, a.cookieAcquisitionMode())
		switch {
```

and re-subject the five arms that name a mechanism (the rung-3 arm and the `!Renewed` arm are
UNCHANGED — see their comments):

```go
		case msg.Err != nil:
			a.setFeedback(mechanismLabel + " failed: " + msg.Err.Error())
		case !msg.Result.Ran:
			// Causes from the shared constant, not restated: this line, the
			// worker's log note and the Web toast are three renderings of one
			// exhaustive list, and they had already drifted apart once.
			a.setFeedback(mechanismLabel + " declined to run (" + cookies.RefreshDeclinedCauses +
				") — nothing was learned about these cookies")
		case msg.Result.Overall() == cookies.RefreshFailed:
			a.setFeedback(mechanismLabel + " ran and auth verification failed")
		case msg.Result.Overall() == cookies.RefreshUnknown:
			a.setFeedback(mechanismLabel + " ran but could not establish whether these cookies work")
		case !msg.Result.Renewed:
			// NOT re-subjected, and not an oversight: renewed is
			// `importedFromProfile || browserActed`, so an import that reaches
			// a verdict always renewed and this arm is unreachable for one.
			// TestRefreshResultCarriesTheMechanismThatRan pins that upstream.
			a.setFeedback("Cookies still work, but this pass could not confirm the browser refreshed them")
		default:
			a.setFeedback(mechanismLabel + " successful")
		}
```

- [ ] **Step 14: Use the label in the dashboard toasts**

`web/public/app.js:10` — add `cookieRefreshMechanismLabel` to the `./modules/utils.js` import list,
after `cookieRefreshPreflightToast`.

Immediately after the `const data = await response.json().catch(...)` that follows
`fetch("/api/cookies/auto-refresh", { method: "POST" })` — the same statement opens six other
handlers in `app.js`; this is the one inside `autoCookieRefresh`:

```js
      // The SUBJECT of every result toast below, from what the pass actually
      // did rather than from what the mode asked for. `data.mechanism` is
      // undefined against an older binary and empty when the pass declined
      // before choosing; both fall back to the configured mode, which is the
      // same answer the pre-flight toast above already gave. See
      // cookieRefreshMechanismLabel in ./modules/utils.js.
      const mechanismLabel = cookieRefreshMechanismLabel(
        data.mechanism,
        this.config?.cookies?.acquisition,
      );
```

Re-subject the five result arms. Only the subject changes; every predicate, every variant and the
declined-cause clause stay byte-identical (`TestAppJSMatchesTheDeclinedCauses` pins that clause
against `cookies.RefreshDeclinedCauses`, and it must stay on one line):

```js
        let refreshMsg, refreshVariant;
        if (!data.success && data.ran === false) {
          // The cause list is one exhaustive set rendered in three places (see
          // cookies.RefreshDeclinedCauses). This copy cannot import it, so
          // TestAppJSMatchesTheDeclinedCauses pins the two against each other —
          // keep the phrase below byte-identical to the constant.
          refreshMsg = `${mechanismLabel} declined to run — a setup or another refresh is already in flight, or no platform has cookies worth refreshing. Nothing was learned about these cookies.`;
          refreshVariant = "neutral";
        } else if (!data.success && data.verdict === "failed") {
          refreshMsg = `${mechanismLabel} completed — auth verification failed`;
          refreshVariant = "danger";
        } else if (!data.success) {
          refreshMsg = `${mechanismLabel} ran but could not establish whether these cookies work — nothing has been concluded about them`;
          refreshVariant = "warning";
        } else if (data.renewed === false) {
          // NOT re-subjected — the Go twin's comment explains why: an import
          // always renews, so this arm is unreachable for one.
          refreshMsg =
            "Cookies still work — but this pass could not confirm the browser refreshed them, so the last-refresh time is unchanged";
          refreshVariant = "warning";
        } else {
          refreshMsg = `${mechanismLabel} successful`;
          refreshVariant = "success";
        }
```

The transport arm (the `else` that follows the 404/424 fallback) becomes:

```js
        this.showToast(data.error || `${mechanismLabel} failed`, "danger");
```

and the `catch`, which cannot see `data` at all, uses the mode alone — the one input it has:

```js
    } catch (e) {
      this.showToast(
        `${cookieRefreshMechanismLabel("", this.config?.cookies?.acquisition)} failed: ${e.message}`,
        "danger",
      );
    } finally {
```

Then update the premise assertion in `internal/web/routes/cookies_shiftclick_test.go` (the
`"Browser cookie refresh failed"` literal), which is now composed:

```go
	// The premise: without a failure arm left, "it falls back" would be
	// trivially true and would say nothing about which failures it covers. The
	// subject is now composed from the mechanism that ran (H2 R9), so the
	// literal to look for is the template, not the old sentence — and matching
	// the template also asserts that this arm did not keep a hardcoded
	// "Browser cookie refresh" after an import.
	if !strings.Contains(code, "`${mechanismLabel} failed`") {
		t.Error("autoCookieRefresh no longer reports any failure at all, so the fallback assertions " +
			"above cannot distinguish a targeted fallback from one that swallows everything")
	}
```

and append the pin that closes the JS half of F2, in the same file (the goja harness in
`internal/tui` executes the shared HELPER; nothing executes `app.js`'s arms, so this reads them):

```go
// TestAutoCookieRefreshToastsNameTheMechanismThatRan is the dashboard half of
// Arc 12c arc-close F2.
//
// The subject of a result toast is not testable through goja — app.js is a
// class method on a live DOM, not a module function — so it is read out of the
// shipped script, bracketed to this one handler the way the rung-3 assertions
// above are. What can be asserted is exactly the defect: a subject that is a
// LITERAL cannot have come from the pass, so an import renders "Browser cookie
// refresh ..." forever.
//
// The two assertions catch different mutants. Dropping mechanismLabel from one
// arm leaves the count short; hardcoding the old sentence back into one arm
// leaves the count short AND trips the literal check, which is the one that
// names the defect in its failure message.
func TestAutoCookieRefreshToastsNameTheMechanismThatRan(t *testing.T) {
	code := jsCode(jsBlock(t, readEmbeddedModule(t, "public/app.js"), "async autoCookieRefresh() {"))

	if !strings.Contains(code, "cookieRefreshMechanismLabel(") {
		t.Fatal("autoCookieRefresh never calls cookieRefreshMechanismLabel, so its toasts cannot " +
			"name the mechanism that ran and every profile import reads as a browser refresh")
	}
	// Five interpolated subjects: declined, verdict-failed, inconclusive,
	// successful, and the transport failure. The renewed === false arm has none
	// by design (an import always renews, so it is unreachable for one) and the
	// catch calls the helper directly, both covered above.
	if n := strings.Count(code, "${mechanismLabel}"); n < 5 {
		t.Errorf("only %d toast subject(s) are composed from the mechanism, want at least 5 — an arm "+
			"still names a mechanism the pass may not have used", n)
	}
	for _, literal := range []string{`"Browser cookie refresh`, "`Browser cookie refresh"} {
		if strings.Contains(code, literal) {
			t.Errorf("autoCookieRefresh still hardcodes %s...` as a toast subject — that is the "+
				"sentence a profile import rendered (Arc 12c arc-close F2)", literal)
		}
	}
}
```

- [ ] **Step 15: Run the three packages**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/cookies/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/web/routes/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/tui/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./cmd/moombox/
```
Expected: `ok` on all four. `TestRefreshPreflightSentenceAgreesAcrossSurfaces` must still PASS and must
still not SKIP — Step 12 adds a second export to `utils.js` and a broken edit there would take the
whole module down, and that test is the canary.

- [ ] **Step 16: Mutations to run**

Every row: make the edit, watch the NAMED test fail, revert, confirm the file is identical.

| # | File | Mutation | Test that must fail |
|---|---|---|---|
| 1 | `autocookies.go` | put the `if profileDirErr != nil && logger != nil { logger.Error(...) }` block back in `NewAutoCookieService` | `TestProfileDirVerdictIsSilentAtConstruction` (and both halves of `…LevelFollowsTheMode`, which then see two lines) *(verified at `1559a36`)* |
| 2 | `autocookies.go` | `LogProfileDirVerdict` drops the `resolvedAcquisition()` branch and always logs Error | `TestProfileDirVerdictLevelFollowsTheMode/profile` (an ERROR line, and no INFO line) *(verified)* |
| 3 | `autocookies.go` | `LogProfileDirVerdict` always logs the Info arm | `TestProfileDirVerdictLevelFollowsTheMode/auto` (zero ERROR lines) *(verified)* |
| 4 | `autocookies.go` | drop the `s.profileDirErr == nil` guard | `TestProfileDirVerdictSaysNothingForAnAcceptableDir` (both rows) *(verified)* |
| 5 | `autocookies.go` | the profile Info line reuses the guard's error as its text (`"err", s.profileDirErr`) | `…LevelFollowsTheMode/profile` — the "refusing to launch" check, which is the claim a path that launches nothing cannot make; caught only because `at()` renders the ARGS, not the heading alone *(verified)* |
| 6 | `services.go` | hoist `autoCookieSvc.LogProfileDirVerdict()` above the `AcquisitionMode` closure | `TestProfileDirVerdictIsLoggedAfterTheModeIsWired` (index check) — **this mutant IS F1**, so it is the row that matters most *(verified)* |
| 7 | `services.go` | delete the call | same test, the "never calls" fatal *(verified)* |
| 8 | `autocookies.go` | delete `defer func() { out.Mechanism = mechanism }()` | `TestRefreshResultCarriesTheMechanismThatRan` rows 2, 3 and 4 *(verified)* |
| 9 | `autocookies.go` | `mechanism := RefreshMechanismBrowser` (a non-empty initialiser) | `…CarriesTheMechanismThatRan` row 1 — the in-flight decline would claim a path it never chose *(verified)* |
| 10 | `autocookies.go` | swap the two constants at the decision | `…CarriesTheMechanismThatRan` rows 2, 3 and 4 *(verified)* |
| 11 | `autocookies.go` | `browserActed := false` AND `renewed := browserActed` — both guards, because the `!Renewed` arm is held shut by each on its own | `…CarriesTheMechanismThatRan` row 3's `Renewed` assertion *(verified)*. **Dropping `importedFromProfile \|\|` alone SURVIVES** *(verified)*: `browserActed` starts true and only the browser branch clears it, so the import still renews — which is what the code's own comment above `renewed :=` says the `\|\|` is for (visibility at the point of use, not behaviour). The assertion pins the property, not the line |
| 12 | `routes/cookies.go` | drop `"mechanism"` from `cookieRefreshOutcome` | `TestCookieRefreshOutcomeCarriesTheMechanism` (all three rows — a missing key reads back `nil`, which equals none of the wants) and `TestAppJSReadsTheFieldsTheHandlerEmits` (its `payload[key]` fatal) *(verified)* |
| 13 | `app_actions.go` | the Go label reads the mode ONLY (`mechanism` never consulted) | `TestRefreshPostflightMechanismAgreesAcrossSurfaces` — two literal rows, both precedence assertions, four goja grid rows — and `…NameTheMechanismThatRan/a_host-decided_import_in_auto_mode` *(verified)*. The partial mutant (mode `profile` checked FIRST, then the mechanism) is caught by one literal row, the first precedence assertion and the `("browser","profile")` goja row *(verified)* |
| 14 | `utils.js` | delete the `"profile-import"` arm from the JS label | the goja grid, rows `("profile-import","auto")`, `("profile-import","")` and `("profile-import","browser")` *(verified)* |
| 15 | `utils.js` | the JS label drops the mode fallback and always returns the browser label | the goja grid rows `("","profile")` and `("headless","profile")`, and the undefined-mechanism assertion *(verified)* |
| 16 | `app_update.go` | the default arm hardcodes `"Browser cookie refresh successful"` | `TestPostFlightSentencesNameTheMechanismThatRan/an_import_that_succeeded` (and `…/a_host-decided_import_in_auto_mode`) *(verified)* |
| 17 | `app_update.go` | `mechanismLabel` is built from `a.cookieAcquisitionMode()` alone, ignoring `msg.Result.Mechanism` | `…NameTheMechanismThatRan/a_host-decided_import_in_auto_mode` *(verified)* |
| 18 | `app_update.go` | the declined arm keeps its old literal subject | `…NameTheMechanismThatRan/a_decline_in_profile_mode_falls_back_to_the_mode` *(verified)* |
| 19 | `app.js` | the success arm hardcodes `"Browser cookie refresh successful"` | `TestAutoCookieRefreshToastsNameTheMechanismThatRan` (the literal check names the defect; the count check also drops) *(verified)* |
| 20 | `app.js` | delete `mechanismLabel` from the declined arm only | `TestAutoCookieRefreshToastsNameTheMechanismThatRan` (count < 5) *(verified)* |
| 21 | `autocookies.go` | the auto arm rewords its message, or swaps the `"err"` arg for `"profile_dir"` | `…LevelFollowsTheMode/auto` — the verbatim check (message and both args), which is the claim that today's line is unchanged under `auto` *(verified, both shapes)* |

Two rows are deliberately absent and are recorded rather than hidden. The `!Renewed` arms on both
surfaces are UNCHANGED by this task, so there is no mutation of them to run; what protects them is
row 11, which fails the moment that arm becomes reachable for an import. And `cookieRefreshReportFor`
(`cmd/moombox/services.go:54`) is untouched: its wording, "automatic cookie refresh", is already true
of both mechanisms.

- [ ] **Step 17: Flip the three doc sentences**

`docs/spec/data-and-storage.md:897` — append to the end of the `cookies.acquisition` ladder paragraph
(after "…so they do not diverge."):

```
Afterwards the mechanism is no longer a guess: `RefreshResult.Mechanism` records which source the pass actually used (`"browser"`, `"profile-import"`, or empty when it declined before choosing), rides the wire as `mechanism` on `cookieRefreshOutcome`, and feeds ONE subject-producer per surface — `cookieRefreshMechanismLabel` in `internal/tui/app_actions.go` and in `web/public/modules/utils.js`, pinned by exact equality in `TestRefreshPostflightMechanismAgreesAcrossSurfaces`. The RESULT outranks the mode there, and the mode is only the fallback, because the two disagree wherever the host decides rather than the setting: a machine with no browser installed imports in `"auto"` mode and always has, which is why every post-flight sentence used to open `Browser cookie refresh ...` after a pass that launched nothing. Only the SUBJECT is shared; each surface keeps its own predicates. The `renewed === false` arm keeps its browser wording on both, and is allowed to: `renewed := importedFromProfile || browserActed`, so an import that reaches a verdict always renewed and that arm is unreachable for one.
```

`docs/spec/user-interfaces.md:622` — the lead sentence and one table row. Old lead:

```
**`POST /api/cookies/auto-refresh`** on success adds the four `cookieRefreshOutcome` keys to the same status block. Three of them are independent facts and none can be derived from another:
```

New:

```
**`POST /api/cookies/auto-refresh`** on success adds the five `cookieRefreshOutcome` keys to the same status block. Three of them are independent facts and none can be derived from another:
```

and, after the `ran` row:

```
| `mechanism` | WHICH cookie source ran: `"browser"`, `"profile-import"`, or `""` when the pass declined before it chose one. **Wording only** — the toast's subject, so an import stops rendering as a browser refresh. Additive: an older frontend ignores it, and a newer frontend against an older binary reads `undefined` and falls back to `cookies.acquisition`, the same value it used for the pre-flight toast. |
```

and, in the paragraph after the table (`:631`), old
`The refresh's own outcome comes from the four keys above and is unaffected.` → new
`The refresh's own outcome comes from the five keys above and is unaffected.`

`docs/spec/security.md:461` — insert into the launch-boundary paragraph immediately after the
sentence "Nothing lifts it." and before "`dangerousProfilePathSubstrings` is Windows-only …":

```
The verdict is REPORTED separately from where it is computed, and at the level the acquisition mode earns: `AutoCookieService.LogProfileDirVerdict`, called once from `cmd/moombox/services.go` after `AcquisitionMode` is wired, logs the refusal at ERROR under `"auto"` (a browser refresh the operator expects will silently not happen) and one INFO under `"profile"` (nothing was going to launch, and the read-only import below runs regardless). The constructor cannot make that choice — it runs before the callback exists — which is why it now computes the verdict silently.
```

- [ ] **Step 18: Close the two Arc 12c residual rows and correct field-test row 23**

`docs/superpowers/plans/2026-09-02-arc12c-acquisition-mode.md:2327` — append to that row's second
cell:

```
 ***(HOMED — H2 Task 7: the constructor computes silently and `AutoCookieService.LogProfileDirVerdict()` says it once from the wiring site, ERROR under `auto` with the wording unchanged, one INFO under `profile`. The call ORDER is pinned by an AST test in `cmd/moombox`, because hoisting it above the `AcquisitionMode` closure reproduces this finding exactly while every behavioural test stays green.)***
```

`:2328` — append to that row's second cell:

```
 ***(HOMED — H2 Task 7: not from the mode, which is wrong whenever the HOST decides — `RefreshResult.Mechanism` records what the pass actually ran, rides the wire as `mechanism`, and feeds one `cookieRefreshMechanismLabel` per surface, pinned by exact equality. The mode is the fallback for a pass that declined before choosing, and for an older binary's payload.)***
```

`docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md:181` — row 23's **Reading
rules** sentence is now false in both halves. Inside that cell (the fifth column, before its
closing ` | `), replace exactly this (old):

```
Reading rules: every boot with this directory configured logs `auto-cookie profile dir rejected at construction ... refusing to launch a headless session` at ERROR - that is the launch guard's verdict (launches DO refuse the directory), not a refusal of the read, and the import in (a) still runs; and the post-flight line still says `Browser cookie refresh ...` after an import (pre-existing wording; both are residuals in the plan's final-state section)
```

with (new):

```
Reading rules: with this directory configured, `"profile"` mode logs ONE INFO at boot naming the directory and saying no headless browser will be launched against it and that the read-only import is what runs (`AutoCookieService.LogProfileDirVerdict`); `"auto"` logs the same verdict at ERROR, which is correct there — a browser refresh WOULD be refused. An ERROR on a `"profile"` boot is a regression of H2 R9, not the expected line. The post-flight sentence in (a) must open `Browser-profile cookie import ...` on both surfaces, not `Browser cookie refresh ...`
```

- [ ] **Step 19: Verify line endings, run the gates, commit**

```bash
for f in internal/cookies/autocookies.go internal/cookies/autocookies_profiledir_verdict_test.go \
         internal/cookies/autocookies_mechanism_test.go cmd/moombox/services.go \
         cmd/moombox/profiledir_verdict_callsite_test.go internal/web/routes/cookies.go \
         internal/web/routes/cookies_test.go internal/web/routes/cookies_shiftclick_test.go \
         internal/tui/app_actions.go internal/tui/app_update.go \
         internal/tui/cookie_postflight_mechanism_test.go \
         web/public/modules/utils.js web/public/app.js \
         docs/spec/data-and-storage.md docs/spec/user-interfaces.md docs/spec/security.md \
         docs/superpowers/plans/2026-09-02-arc12c-acquisition-mode.md \
         docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md; do
  printf '%s: ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```
Expected `0` each. Then the full gate block from Global Constraints. (`web/public` is `go:embed`ed,
and `go test` recompiles the `web` package when an embedded file changes — the build cache keys the
asset contents — so the goja and source-reading tests see the edited `utils.js` and `app.js` with no
separate build step; verified at `1559a36`. The `go build ./...` in the gate block is the build
gate, not an asset refresh.)

```bash
git add internal/cookies/autocookies.go internal/cookies/autocookies_profiledir_verdict_test.go \
        internal/cookies/autocookies_mechanism_test.go cmd/moombox/services.go \
        cmd/moombox/profiledir_verdict_callsite_test.go internal/web/routes/cookies.go \
        internal/web/routes/cookies_test.go internal/web/routes/cookies_shiftclick_test.go \
        internal/tui/app_actions.go internal/tui/app_update.go \
        internal/tui/cookie_postflight_mechanism_test.go \
        web/public/modules/utils.js web/public/app.js docs/spec/ docs/superpowers/plans/
git commit -m "fix(cookies): the boot line and the post-flight sentences name the mechanism that ran

Arc 12c arc-close F1 and F2. NewAutoCookieService cannot know the
acquisition mode - the callback is wired after it returns - so it now
computes the launch guard's verdict silently and LogProfileDirVerdict
says it once from the wiring site: ERROR under auto, wording unchanged,
one INFO under profile naming the read-only import that actually runs.

RefreshResult carries the mechanism it chose, stamped once at the
launch-vs-import decision and out through every return by a named
result, onto the wire as an additive mechanism key, and into one
subject-producer per surface pinned across the two by exact equality.
The mode is only the fallback: a host with no browser imports in auto
mode too, which is why every post-flight sentence said browser.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN"
```

---

## Self-Review

### 1. Spec coverage

| Spec requirement | Task | Notes |
|---|---|---|
| R1 back-off — state shape, arithmetic, placement in `recordLiveness`, gate untouched, five tests (four schedule + one literal pin), two doc sentences | 1 | Complete. `livenessRecoveryArmed` untouched. `:843` and `:867` flipped, plus `:845`/`:850`/`:851` (the maps table gains its fourth row), which the draft did not name but cite `livenessRefireWindow` as the check `recordLiveness` reads. The reset trigger is the tier-2 conclusive positive only; a tier-1 `OnAuthRecovered` does not reset (stated in the task). |
| R2 horizons + `twitchLoginExpiry` + ONE merge Warn + three doc sentences + the settling observation | 3 | Complete, with one deviation on the second log line (below). No `AuthCookieHorizonFor` caller sweep; `authCookieNamesFor` not split. |
| R3 browser-path twin, regression arm only, cloned test + two arm mutants, scoping comment, `:907`, plan `:2015` (2) | 4 | Complete. Decided: a SIBLING function over a parameter, with a shared `regressedAfterWrite`. |
| R4 contract pin, one test, doc restated | 2 | Complete; the missing row is named precisely below. |
| R5 struct fold, three setters, every reading site, byte-identical | 5 | Complete. |
| R6 ARMING row 12 disarmed-half assertion + the ledger text | 2 | Complete; the ledger edit is Step 7, explicitly not a commit. |
| R7 AST test over the three files, `OnPassCompleted` by its own shape, the mutant | 6 | Complete. |
| R8 `successMsg` twin, rendered where `errorMsg` is, one goja-free test | 5 | Complete. Decided: set BOTH surfaces; reason in the task. |
| Non-goals (no arming, no caller sweep, no `shouldFireRecovery` change, no REST/web change) | — | Held. Nothing touches `shouldFireRecovery`, `internal/web`, `web/public`, or `internal/config`. |
| Invariants (`livenessRecoveryArmed` false; `main.go:276-278` untouched; timestamps only; no goroutine; anonymous logger; every assertion mutation-checked; byte-wise LF) | Global Constraints | Stated with exact values, plus a per-task mutation table. |

### 2. Placeholder scan

No "TBD", no "similar to Task N", no "add appropriate error handling". Every code step carries the actual code; every doc step carries the actual old and new sentences. The three "accepted" rows (Task 3 mutation 8, Task 5 mutations 2 and 6) are explicit residuals **with a stated reason**, not deferred work.

### 3. Type consistency

- `platformAuth`, `verifyOK`/`verifyFailed`/`verifyUnknown`, `platformsToRestore(pre, post map[string]platformAuth) map[string]bool` verified in `autocookies_profile.go`; the sibling matches exactly, so `restorePolicy` can hold either.
- `livenessRecord` is a `uint8` enum with no room for a duration, which is why R1's state is a sibling map, not "a `livenessRecord` field". The draft offered both; the code admits one.
- `AuthHorizonString` / `TwitchLoginExpiry` / `HorizonLogFields` are used under those exact names in `cmd/moombox` (Task 3 Step 13) and `internal/cookies` (Step 9).
- `appFeedback{msg, sev, until}` field names are used identically in `app.go`, `app_update.go`, `app_layout.go` and all seven test files.
- `recheckAfterCookieWrite(ctx, checkNow, log, gesture string, args ...any) bool` — the gesture is argument index 3, which is what `gestureArg` reads.

### 4. Where the code contradicted the first spec draft

Four places, each argued in full inside its task and now folded into the spec (`h2-design-draft.md` was corrected by the plan review, 2026-09-03; all four were checked against the code at `8558f5f`):

1. **R2's second log line** rides `refreshCookiesDetailed`'s `"cookie refresh succeeded"`, not the per-launch `autocookies_firefox.go:288` line (fires before the merge/write/reload; Chromium has no twin). Argued at the top of Task 3.
2. **R2's recording logger** — `argRecordingLogger` and `markWarnRecorder` capture args; Task 3 reuses the former, and `cmd/moombox` asserts an extracted field list instead of scraping a line.
3. **R4's missing rows** are the two `Authenticated` booleans moving ALONE (the reason-only case was already fully pinned). Task 2's opening paragraph.
4. **R3's operator strings** ("imported profile cookies did not hold up"; "the mounted browser profile did not verify") become path-conditional; the two `errMsg` sentences saying "the browser profile did not verify" stay true of both paths. Task 4 Step 5. No test asserts any of these strings today.

Residual noted by the review, not a defect: the horizons ride the SUCCESS completion line only. The failure tail (`"refresh completed but auth verification failed"`) does not carry them; the settling observation needs a pass that verified, and the boot line's `expiredTwitchAuth` count covers the dead-at-boot case. Add them there later if the field wants the expired-vs-revoked distinction on a failing pass.
