# Arc 12a — TUI Cookie Login and the Merge Key Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the TUI a chord (`R L`) that opens the existing setup-wizard cookie step on a configured install, have the `Re-login` badge name that chord, and make `mergeCookieFiles` key rows by RFC 6265 identity — name + domain + **path**.

**Architecture:** Two unrelated items, one plan (they share only the arc). **V9** adds no state machine: `SetupWizardModel` already owns the whole interactive-login flow — `OnStartAutoCookie` / `OnFinishAutoCookie` / `OnCancelAutoCookie`, the 300 s countdown, the three-state verdict rendering — and `cmd/moombox/tui_wiring.go:461-531` already binds all three callbacks unconditionally. The only thing missing is a second entrance: today the wizard opens at exactly one site, `internal/tui/app.go:674-677 if a.IsFirstRun { a.setupWiz.Open() }`. So V9 is a new `OpenCookieLogin(platform string)` that lands on the *existing* simple-mode cookie step with a `cookieOnly` flag changing only the way OUT (Esc and the third list row close the overlay instead of walking to the channel editor), plus one `buildMenuItems()` entry and one `dispatchAction()` case. **T3** adds one field to a local struct type and one `fields[2]` read.

**Tech Stack:** Go 1.26, Bubble Tea v2 (`charm.land/bubbletea/v2`), bubbles/huh/lipgloss v2, `internal/tui`, `internal/cookies`. No new dependency, no config key, no REST route, no database change.

**Spec:** `docs/superpowers/specs/2026-09-02-arc12a-tui-cookie-login-and-merge-key-design.md` (R1-R5, non-goals, invariants) — drafted as `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/arc12a-design-draft.md`, reviewed 2026-09-02 (`arc12a-plan-review.md` beside it; its edits are applied here), and committed to that `docs/` path together with this plan.

## Global Constraints

Every task's requirements implicitly include this section.

- **Logger interface stays anonymous.** Any struct needing a logger repeats the four-method anonymous interface inline. Do not extract a named interface. (No task here adds a logger — this constraint is satisfied vacuously and is recorded so a "helpful" refactor is out of bounds.)
- **Every goroutine gets an inline `defer func() { if r := recover(); ... }()`.** No task here starts a goroutine; TUI async work goes through `safeCmd` (`internal/tui`), which already carries the recover. The chord case added in Task 3 dispatches **no** command at all — it opens an overlay synchronously — so no `safeCmd` is needed there either.
- **`const livenessRecoveryArmed = false` (`internal/cookies/refresh.go:748`) stays false.** Nothing in this plan reads or flips it.
- **`cmd/moombox/main.go:276-278` is no-touch** (the `SetExpectedPlatforms` seeding gated on `cookies.auto_enabled`).
- **Never read, log, print, or assert on a cookie file or a cookie value.** Fixtures in `internal/cookies` use invented values (`root_scope`, `live_scope`, `new_value`); no test opens `D:\Moombox\cookies.txt`, `cookies.sqlite`, or a browser profile. No TUI string ever renders a cookie value.
- **LF line endings** on every file created or edited.
- **Gates, per task, before the commit:**
  ```bash
  go build ./...
  go vet ./...
  GOOS=linux GOARCH=amd64 go build ./...
  gofmt -l internal/ cmd/          # must print nothing
  go test -count=1 ./...           # once, whole tree
  ```
- **Commit trailers** on every commit in this plan:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
  ```
- **The chord table is the single source of truth** (CLAUDE.md § TUI chord system): one entry in `buildMenuItems()`, one case in `dispatchAction()`. Hints, the action menu, the help overlay and the chord-feedback line all derive from `buildMenuItems()` — do not hand-write the chord anywhere else. `TestHelpCoversEveryChord` (`internal/tui/help_coverage_test.go`) checks every chord `buildMenuItems()` registers on a bare `NewApp()` — which has no callbacks, so a callback-gated chord such as `R L` is outside its view; the help assertion for `R L` is `rlIsOffered` in Task 3, which builds the menu from a wired `App`.
- **Charm-first** (`.claude/skills/moombox-charm-suite`): reuse the wizard's existing components. Task 2 deliberately adds **no** new `huh` form — see its design note.
- **Docs are edited in the task that makes them true**, never in a trailing "docs" task.

---

## Chord letter: `R L`, and what was rejected

`buildMenuItems()` (`internal/tui/app_actions.go:474-556`) registers these `R` chords today, all Request-category, all conditional on a callback:

| Chord | Label | Gate |
|-------|-------|------|
| `R B` | Re-scan Feed History | `OnBackfillRescan != nil` |
| `R C` | Recheck Cookies | `OnRecheckCookies != nil` |
| `R F` | Force Cookie Refresh | `OnForceRefreshCookies != nil` |
| `R V` | Check for Updates | `OnCheckUpdate != nil` |
| `R M` | Check Monitors Now | `OnForceCheck != nil` |
| `R N` | View Release Notes | an update is pending, or `OnFetchReleaseNotes != nil` |
| `R U` | Apply Update | update pending **and** `OnApplyUpdate != nil` |
| `R S` | Verify Signature | `OnVerifySignature != nil` |
| `R P` | Restart Program | `OnRestart != nil`, confirm chord |

Taken: **B C F V M N U S P**. Chosen: **`R L`** — L for Login, free, and the word the operator is looking for.

Rejected, recorded so this is not re-litigated: **`R I`** (sIgn In) — `A I` is already Reinitialize Job, and a near-collision across prefixes gets mistyped under pressure; **`R A`** (Auth) — `A` is a chord *prefix*, so it reads as a typo in the feedback line; **`R W`** (Web) — the login opens a browser *on the host*, and `O W` is already "Open Web UI"; **`R G`** (siGn in) — no mnemonic, and `O G` is GitHub; **`R K`** (cooKie) — `A K` is Manage Client Tokens, and both are credential-shaped, which is exactly when a mistype is expensive.

**No confirm keypress.** `NeedsConfirm` is for irreversible acts (`A C`, `A D`, `A M`, `R P`). `R L` opens an overlay whose own first keypress is the destructive one, and Esc dismisses it — a third keypress would be ceremony guarding nothing.

---

### Task 1: `mergeCookieFiles` keys by name + domain + path (T3)

**Files:**
- Modify: `internal/cookies/autocookies_merge.go:140-146` (doc comment + `cookieKey`), `:164-166` (`parseCookies`'s key construction)
- Modify: `internal/cookies/autocookies.go:2224` (a comment that states the old key — line number at `390adb6`; the A1-Linux arc edits this file on `cookie-a1-linux-reap`, so locate the comment by content, see Step 5)
- Modify: `internal/cookies/autocookies_profile.go:691` (same)
- Modify: `internal/cookies/autocookies_profile_rollback_test.go:51` (same, in a test comment)
- Modify: `internal/cookies/cookie_import.go:24` and `:129` (same — "name+domain keyed" / "wins by name+domain")
- Modify: `internal/cookies/cookie_import_test.go:132`, `:188`, `:195`, `:203`, `:228` (same, in test comments and one `t.Errorf` message)
- Modify: `internal/cookies/autocookies_merge_test.go:230-231` (the `PrefersNew` doc comment) and append the new test
- Modify: `docs/spec/data-and-storage.md:872` (§Auto-Cookie Service, "keyed by name+domain")
- Modify: `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2012` (the T3 paragraph)
- Test: `internal/cookies/autocookies_merge_test.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: nothing other tasks read. `mergeCookieFiles(existing, newCookies string) string` keeps its exact signature; `cookieKey` is a function-local type and is not exported or referenced anywhere else. The three callers — `autocookies.go:1248` (`FinishSetup`), `autocookies.go:2207` (`refreshCookiesDetailed`), `cookie_import.go:52` (`ImportCookies`) — are **untouched**.

**What is NOT in scope, and why the fix is still worth making.** `CookieJar` is path-blind: within one platform jar a name maps to one entry, and rows whose stored domain string compares equal keep last-wins (`internal/cookies/jar.go:321-331`). So after this change a `cookies.txt` holding `SAPISID` on `/` and on `/live` still loads exactly one of them into the jar — the last one in the file, which is the same row the old merge would have kept. **The jar-visible outcome is unchanged in every ordering**; what changes is that the file stops *losing* the other row on each rewrite. Making the jar path-aware is a different, larger change to a structure the jar states deliberately (`jar.go:280-295`) and is not this task.

- [ ] **Step 1: Write the failing test**

Append to `internal/cookies/autocookies_merge_test.go`. **The fixture rows are TAB-separated** — copy them verbatim. A space-separated row parses as fewer than 7 fields, is skipped by `parseCookies`, and every assertion below would then pass vacuously against an empty merge.

```go
// TestMergeCookieFilesKeepsRowsThatDifferOnlyInPath pins RFC 6265 §5.3 cookie
// identity — name + domain + PATH — at the one function that rewrites
// cookies.txt for all three credential writers (FinishSetup, the browser
// refresh, ImportCookies). The key carried only name+domain, so two rows
// differing solely in path collided on one map entry and the later-parsed line
// silently replaced the earlier: a row left the file on every merge, with
// nothing logged. THE MUTANT is exactly the old key — delete `path` from
// cookieKey (or stop filling it from fields[2]) and subtest 1 loses a row.
//
// Subtests 2 and 3 are the PREMISE, and without them the first proves nothing:
// a key that also carried the value — or the whole line — would keep both rows
// in subtest 1 while destroying the merge's actual job, which is letting a
// freshly-extracted row replace the stale one it supersedes.
func TestMergeCookieFilesKeepsRowsThatDifferOnlyInPath(t *testing.T) {
	// Far-future expiry (2100-01-01) throughout: merge prunes rows whose
	// expiry is in the past, so a small epoch offset would delete the fixture
	// before the key was ever consulted.
	t.Run("two paths in one file both survive", func(t *testing.T) {
		existing := "# Netscape HTTP Cookie File\n" +
			".youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\troot_scope\n" +
			".youtube.com\tTRUE\t/live\tTRUE\t4102444800\tSAPISID\tlive_scope\n"

		merged := mergeCookieFiles(existing, "")

		if !strings.Contains(merged, "root_scope") {
			t.Errorf("the / row was dropped — a same-name row on a different path evicted it, "+
				"which is the collision the path in the key exists to prevent:\n%s", merged)
		}
		if !strings.Contains(merged, "live_scope") {
			t.Errorf("the /live row was dropped:\n%s", merged)
		}
		if got := strings.Count(merged, "SAPISID"); got != 2 {
			t.Errorf("merged holds %d SAPISID rows, want 2 (one per path):\n%s", got, merged)
		}
	})

	t.Run("a new row on a new path is added, not substituted", func(t *testing.T) {
		existing := ".youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\troot_scope\n"
		newer := ".youtube.com\tTRUE\t/live\tTRUE\t4102444800\tSAPISID\tlive_scope\n"

		merged := mergeCookieFiles(existing, newer)

		if !strings.Contains(merged, "root_scope") {
			t.Errorf("a refresh that produced a differently-scoped row deleted the existing one:\n%s", merged)
		}
		if !strings.Contains(merged, "live_scope") {
			t.Errorf("the new row is missing:\n%s", merged)
		}
	})

	t.Run("same path still collapses and the newer row wins", func(t *testing.T) {
		existing := ".youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\told_value\n"
		newer := ".youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\tnew_value\n"

		merged := mergeCookieFiles(existing, newer)

		if strings.Contains(merged, "old_value") {
			t.Errorf("the superseded row survived — the key has stopped identifying a cookie and "+
				"cookies.txt now accumulates every value it has ever held:\n%s", merged)
		}
		if !strings.Contains(merged, "new_value") {
			t.Errorf("the new value is missing:\n%s", merged)
		}
		if got := strings.Count(merged, "SAPISID"); got != 1 {
			t.Errorf("merged holds %d SAPISID rows for one identity, want 1:\n%s", got, merged)
		}
	})
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 -run TestMergeCookieFilesKeepsRowsThatDifferOnlyInPath ./internal/cookies/`
Expected: FAIL — the first subtest reports `merged holds 1 SAPISID rows, want 2` and a dropped `root_scope`; the second reports the deleted existing row. The third subtest PASSES already (it is the premise).

- [ ] **Step 3: Add the path to the key**

In `internal/cookies/autocookies_merge.go`, replace the doc comment and the `cookieKey` declaration (currently `:140-146`):

```go
// mergeCookieFiles merges existing and new Netscape cookie strings.
// New cookies take priority over existing ones with the same name+domain+path.
func mergeCookieFiles(existing, newCookies string) string {
	// RFC 6265 §5.3: a cookie's identity is name + domain + PATH. The key
	// carried only the first two, so two rows differing solely in path
	// collided on one map entry and the later-parsed line silently replaced
	// the earlier one — a row lost from cookies.txt on every write, through
	// all three writers (FinishSetup, the browser refresh, ImportCookies).
	//
	// Secure and HttpOnly stay OUT deliberately: they are attributes OF a
	// cookie, not part of its identity, and keying on them would let a row
	// that merely flipped Secure accumulate beside its own replacement.
	//
	// This does NOT make the jar path-aware — CookieJar keys by name within a
	// platform and keeps last-wins for equal domains (jar.go). Two paths still
	// load as one entry, the same one the old key kept; what changes is that
	// the file stops losing the other row.
	type cookieKey struct {
		name   string
		domain string
		path   string
	}
```

Then, inside `parseCookies`, replace the three lines that build the key (currently `:164-166`):

```go
			domain := fields[0]
			path := fields[2]
			name := fields[5]
			k := cookieKey{name: name, domain: domain, path: path}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -count=1 -run TestMergeCookieFiles ./internal/cookies/`
Expected: PASS — the new test and the four existing merge tests (`PrefersNew`, `HandlesHttpOnlyPrefix`, `PrunesExpiredRows`, `SkipsCommentsAndBlanks`) all green. Those four use path `/` on every row, so they are unaffected by construction; if any of them fails, the key is wrong rather than the test.

- [ ] **Step 5: Correct every in-tree comment that states the old key**

Each says "name+domain" about *this* merge and is now false. Change the phrase to `name+domain+path` in place, leaving the surrounding argument intact (twelve mentions, seven files — `grep -rn "name+domain" internal/` is the check, and after this step its only survivors are the two exclusions named at the end):

- `internal/cookies/autocookies_merge.go:141` — done in Step 3.
- `internal/cookies/autocookies.go:2224` — `// value win by name+domain — so a dead Twitch token in the profile` → `// value win by name+domain+path — so a dead Twitch token in the profile`. Locate by content, not line: it is the `// A mounted profile can be STALE, and mergeCookieFiles lets the imported` paragraph in the per-platform rollback block of `refreshCookiesDetailed`. The A1-Linux arc is editing this file on this branch; a one-word comment edit merges cleanly unless that paragraph itself is rewritten.
- `internal/cookies/autocookies_profile.go:691` — `//     mergeCookieFiles lets the imported value win by name+domain and a` → `//     mergeCookieFiles lets the imported value win by name+domain+path and a`.
- `internal/cookies/autocookies_profile_rollback_test.go:51` — `// mergeCookieFiles lets the imported value win by name+domain, so without a` → `// mergeCookieFiles lets the imported value win by name+domain+path, so without a`.
- `internal/cookies/autocookies_merge_test.go:231` — `// overwrite existing ones with the same name+domain (finding #54).` → `// overwrite existing ones with the same name+domain+path (finding #54).`
- `internal/cookies/cookie_import.go:24` — `// an unrelated-looking reason. mergeCookieFiles is the merge — name+domain` → `// an unrelated-looking reason. mergeCookieFiles is the merge — name+domain+path`.
- `internal/cookies/cookie_import.go:129` — `// by name+domain over a working credential on disk.` → `// by name+domain+path over a working credential on disk.`
- `internal/cookies/cookie_import_test.go:132` — the `t.Errorf` text `the new row must win by name+domain` → `the new row must win by name+domain+path`.
- `internal/cookies/cookie_import_test.go:188` — `//     empty-valued SAPISID in a paste win by name+domain over a working one on` → `//     empty-valued SAPISID in a paste win by name+domain+path over a working one on`.
- `internal/cookies/cookie_import_test.go:195` — `//     mergeCookieFiles keys a 7-field row by name+domain whatever its value,` → `//     mergeCookieFiles keys a 7-field row by name+domain+path whatever its value,`.
- `internal/cookies/cookie_import_test.go:203` — `//     no such guard — it keys by name+domain and "" is a perfectly good map` → `//     no such guard — it keys by name+domain+path and "" is a perfectly good map`.
- `internal/cookies/cookie_import_test.go:228` — `// nothing else: a stale row the paste would replace by name+domain never` → `// nothing else: a stale row the paste would replace by name+domain+path never`.

Leave `internal/cookies/autocookies_merge_test.go:32` alone — that "name+domain filter" is `isEssentialCookie`, a different thing. Leave `internal/cookies/refresh_setcookie_test.go:276` alone — that is `processSetCookies`' per-domain admission, not this merge. Leave the Arc 11 plan and spec docs (`docs/superpowers/plans/2026-09-02-arc11-docker-ingest.md`, `docs/superpowers/specs/2026-09-02-arc11-docker-ingest-design.md`) alone: they are dated records of what was true when they were executed, and rewriting them would be rewriting history.

- [ ] **Step 6: Update the two living docs**

`docs/spec/data-and-storage.md:872` — in the §Auto-Cookie Service opening paragraph, replace

> merge through `mergeCookieFiles` keyed by name+domain, write through `writeFileAtomic`

with

> merge through `mergeCookieFiles` keyed by name+domain+path (RFC 6265 identity — Secure and HttpOnly are attributes, not identity, and stay out; the jar itself remains path-blind, so two paths still load as one entry and the file merely stops losing the other row), write through `writeFileAtomic`

`docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2012` — append to the end of the T3 bullet, after the existing `**RULED (Q13): …Arc 12 planning.**`:

> **DONE (Arc 12a, 2026-09-02):** reviewed and FIXED — a same-platform row *could* be lost, whenever two rows shared name+domain and differed in path. `cookieKey` is now `{name, domain, path}` with `path` read from `fields[2]`; `TestMergeCookieFilesKeepsRowsThatDifferOnlyInPath` pins it with the old key as the named mutant. The other two members of the trio are unchanged and still deliberate: `deduplicateAndFormat`'s bare-name key (`autocookies_merge.go:94`) and `updateCookieFile`'s name-loose/domain-strict split.

- [ ] **Step 7: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/ && go test -count=1 ./...
```
Expected: builds clean, `gofmt -l` prints nothing, whole tree passes.

- [ ] **Step 8: Commit**

```bash
git add internal/cookies/autocookies_merge.go internal/cookies/autocookies_merge_test.go \
        internal/cookies/autocookies.go internal/cookies/autocookies_profile.go \
        internal/cookies/autocookies_profile_rollback_test.go \
        internal/cookies/cookie_import.go internal/cookies/cookie_import_test.go \
        docs/spec/data-and-storage.md docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md
git commit -m "$(cat <<'EOF'
fix(cookies): the merge key is a cookie's identity — name, domain and path

Two rows differing only in path collided on one cookieKey and the
later-parsed line silently won, so cookies.txt lost a row on every write
through all three writers. RFC 6265 §5.3 identity is name+domain+path;
Secure and HttpOnly are attributes and stay out. The jar stays
path-blind, so the loaded entry is the same one the old key kept — what
changes is that the file stops losing the other row.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 2: the wizard opens at its cookie step alone (V9, model half)

**Files:**
- Modify: `internal/tui/setup_wizard.go` — one struct field (after `cookieTickActive`, `:215`), one reset line in `Open()` (`:280`), two new methods after `armCookieTick` (`:414-421`), two branches in `handleSimpleCookieKey` (`:674-675`, `:710-713`), three branches in `viewSimpleCookies` (`:1383-1396`, `:1429-1436`, `:1469-1472`)
- Test: `internal/tui/setup_wizard_cookie_login_test.go` (create)

**Interfaces:**
- Consumes: `SetupWizardModel`'s existing cookie state — `cookieFocus int` (0 = YouTube, 1 = Twitch, 2 = the third row), `cookieActive`, `cookieFinishing`, `cookiePlatform`, `cookieCountdown`, `cookieTimedOut`, `cookieTickActive`, `cookieYTDone`, `cookieTWDone`; the callbacks `OnStartAutoCookie func(platform string) error`, `OnFinishAutoCookie func() (cookies.SetupResult, error)`, `OnCancelAutoCookie func()`; `cookieSetupCountdownSeconds = 300`; `armCookieTick() tea.Cmd`; `Close()`; `IsVisible() bool`.
- Produces, for Task 3:
  - `func (m *SetupWizardModel) OpenCookieLogin(platform string)` — opens the wizard as a cookie-login-only overlay at the simple-mode cookie step. `platform == "twitch"` preselects Twitch; anything else preselects YouTube. A **no-op** when the wizard is already visible.
  - `func (m *SetupWizardModel) closeCookieLogin()` — the single exit funnel; cancels an in-flight setup, then `Close()`.
  - unexported field `cookieOnly bool`, readable from tests and from `App` (same package).

**Design note — why no new `huh.NewSelect`, and why no new mode.** The spec's R1 offers a `huh` select "if the wizard does not already ask" for the platform. It already asks: the simple-mode cookie step *is* a platform picker (YouTube / Twitch / third row, `setup_wizard.go:673-713` for keys, `:1428-1464` for the view), driven by `cookieFocus`. Adding a `huh.NewSelect` would put a second platform picker in front of the first. A new `setupMode` would fork the key handler and the view, which is the state-machine duplication R1 forbids. So the overlay is the **same** mode and the **same** stage, with one boolean changing the two exits. `handleAdvancedCookieKey` is untouched: the cookie-login overlay is always `setupModeSimple`.

- [ ] **Step 1: Write the failing test**

Create `internal/tui/setup_wizard_cookie_login_test.go`:

```go
package tui

import "testing"

// The wizard has always owned the whole interactive cookie login — start,
// countdown, finish, cancel, the three-state verdict — and has always been
// reachable from one place: app.go's `if a.IsFirstRun`. These tests pin the
// second entrance, and what they are NOT allowed to pin is a second
// implementation: every assertion runs through the same handleSimpleCookieKey
// the first-run flow runs through.

// TestOpenCookieLoginLandsOnTheCookieStep pins where the overlay opens and what
// it preselects.
//
// MUTANT: have OpenCookieLogin call Open() instead. Open() is the first-run
// entrance — it drops to setupModeSelect, so a chord that promised a login
// serves the "Quick or Advanced" screen, and it clears the config values and
// channel list on the way.
func TestOpenCookieLoginLandsOnTheCookieStep(t *testing.T) {
	for _, tc := range []struct {
		platform  string
		wantFocus int
		wantStart string
	}{
		{"youtube", 0, "youtube"},
		{"twitch", 1, "twitch"},
		{"", 0, "youtube"},          // no platform flagged — YouTube is the default row
		{"nonsense", 0, "youtube"},  // an unknown value costs a keystroke, never a wrong browser
	} {
		t.Run("platform="+tc.platform, func(t *testing.T) {
			m := NewSetupWizardModel()
			started := ""
			m.OnStartAutoCookie = func(p string) error { started = p; return nil }

			m.OpenCookieLogin(tc.platform)

			if !m.IsVisible() {
				t.Fatal("OpenCookieLogin left the wizard hidden")
			}
			if !m.cookieOnly {
				t.Error("the overlay is not marked cookie-only, so Esc and the third row will " +
					"walk the operator into the first-run channel editor")
			}
			if m.mode != setupModeSimple {
				t.Errorf("mode = %v, want setupModeSimple — a chord that promised a login opened " +
					"the mode-selection screen", m.mode)
			}
			if m.simpleStage != setupSimpleCookies {
				t.Errorf("stage = %v, want setupSimpleCookies", m.simpleStage)
			}
			if m.cookieFocus != tc.wantFocus {
				t.Errorf("cookieFocus = %d, want %d", m.cookieFocus, tc.wantFocus)
			}

			// The step is not a picture of one: pressing Enter must reach the
			// SAME callback the first-run step reaches, and arm the SAME
			// countdown constant.
			m.HandleKey(keyEnter)
			if started != tc.wantStart {
				t.Errorf("Enter started %q, want %q", started, tc.wantStart)
			}
			if m.cookieCountdown != cookieSetupCountdownSeconds {
				t.Errorf("armed %d seconds, want cookieSetupCountdownSeconds (%d) — the overlay is "+
					"running its own timer instead of the wizard's", m.cookieCountdown,
					cookieSetupCountdownSeconds)
			}
		})
	}
}

// TestCookieLoginOverlayCancelsWhateverItLeavesBehind pins the abandon rule.
// AutoCookieService holds the setup slot until someone cancels, finishes, or
// the server-side reap notices the browser is gone, so walking out with a
// browser open must release it — otherwise the next R L, and the next periodic
// refresh, meet ErrSetupInProgress for the whole grace window.
//
// MUTANT: make closeCookieLogin call m.Close() directly. The third subtest then
// leaves a live setup behind with no cancel.
func TestCookieLoginOverlayCancelsWhateverItLeavesBehind(t *testing.T) {
	t.Run("esc while the browser is open cancels and stays", func(t *testing.T) {
		m := NewSetupWizardModel()
		cancels := 0
		m.OnStartAutoCookie = func(string) error { return nil }
		m.OnCancelAutoCookie = func() { cancels++ }
		m.OpenCookieLogin("youtube")
		m.HandleKey(keyEnter) // browser open, countdown armed

		m.HandleKey(keyEsc)

		if cancels != 1 {
			t.Fatalf("Esc over a live setup cancelled %d times, want 1", cancels)
		}
		if m.cookieActive {
			t.Error("the overlay still advertises a live cookie flow after Esc")
		}
		if !m.IsVisible() {
			t.Error("Esc over a live setup closed the overlay; it should cancel the browser and " +
				"leave the picker up so the operator can try the other platform")
		}
	})

	t.Run("esc at the picker closes and cancels nothing", func(t *testing.T) {
		m := NewSetupWizardModel()
		m.OnCancelAutoCookie = func() { t.Error("cancelled a setup that was never started") }
		m.OpenCookieLogin("youtube")

		m.HandleKey(keyEsc)

		if m.IsVisible() {
			t.Error("Esc at the picker left the cookie-login overlay open")
		}
		if m.cookieOnly {
			t.Error("cookieOnly survived the close, so the next first-run wizard would inherit it")
		}
	})

	t.Run("the close funnel cancels a live setup", func(t *testing.T) {
		m := NewSetupWizardModel()
		cancels := 0
		m.OnStartAutoCookie = func(string) error { return nil }
		m.OnCancelAutoCookie = func() { cancels++ }
		m.OpenCookieLogin("twitch")
		m.HandleKey(keyEnter)

		m.closeCookieLogin()

		if cancels != 1 {
			t.Fatalf("closeCookieLogin cancelled %d times, want 1 — a browser was left holding "+
				"the acquisition slot", cancels)
		}
		if m.IsVisible() || m.cookieActive {
			t.Error("closeCookieLogin left the overlay or the flow alive")
		}
	})
}

// TestCookieLoginOverlayDoesNotWalkIntoTheFirstRunFlow pins the third list row.
// In the first-run wizard that row is "Skip / Next" and leads to the channel
// editor, whose Tab finishes setup and REWRITES config.toml — not something a
// configured install should reach from a cookie chord.
//
// MUTANT 1: drop the cookieOnly branch from case 2. The first subtest then
// lands on setupSimpleChannels.
// MUTANT 2: drop the `if m.cookieOnly` guard from the keyEsc case so Esc
// always closes. The cookie-only Esc subtest in
// TestCookieLoginOverlayCancelsWhateverItLeavesBehind still passes; the third
// subtest here is what catches it.
func TestCookieLoginOverlayDoesNotWalkIntoTheFirstRunFlow(t *testing.T) {
	t.Run("cookie-only: the third row closes", func(t *testing.T) {
		m := NewSetupWizardModel()
		m.OpenCookieLogin("youtube")
		m.cookieFocus = 2

		m.HandleKey(keyEnter)

		if m.IsVisible() {
			t.Error("the third row did not close the cookie-login overlay")
		}
		if m.simpleStage == setupSimpleChannels {
			t.Error("the cookie-login overlay walked into the first-run channel editor")
		}
	})

	// THE PREMISE. Without it the subtest above is satisfied by a build where
	// the third row closes the wizard for everyone, which would break first run.
	t.Run("first run: the third row still advances to channels", func(t *testing.T) {
		m := NewSetupWizardModel()
		m.Open()
		m.mode = setupModeSimple
		m.simpleStage = setupSimpleCookies
		m.cookieFocus = 2

		m.HandleKey(keyEnter)

		if !m.IsVisible() {
			t.Fatal("premise lost: Skip / Next closed the first-run wizard")
		}
		if m.simpleStage != setupSimpleChannels {
			t.Fatalf("premise lost: Skip / Next left the first-run wizard at stage %v, so the "+
				"cookie-only subtest above proves nothing about the branch", m.simpleStage)
		}
	})

	// The Esc branch has the same shape and needs its own premise: a build
	// where Esc closes the wizard for EVERYONE passes the cookie-only Esc
	// subtest and strands a first-run operator who pressed Esc to go back a
	// screen — the wizard is gone and the process is still unconfigured.
	t.Run("first run: Esc at the picker still returns to mode selection", func(t *testing.T) {
		m := NewSetupWizardModel()
		m.Open()
		m.mode = setupModeSimple
		m.simpleStage = setupSimpleCookies

		m.HandleKey(keyEsc)

		if !m.IsVisible() {
			t.Fatal("premise lost: Esc closed the first-run wizard at its cookie step")
		}
		if m.mode != setupModeSelect {
			t.Fatalf("premise lost: Esc left the first-run wizard in mode %v, want setupModeSelect", m.mode)
		}
	})
}

// TestCookieLoginRefusesToReopenOverAVisibleWizard: a chord must not throw away
// a first-run setup in progress, nor stack a second cookie overlay on top of a
// countdown that has a real browser behind it.
//
// MUTANT: drop the `if m.visible { return }` guard. The wizard then jumps from
// the mode-selection screen straight to a cookie-only picker mid-setup.
func TestCookieLoginRefusesToReopenOverAVisibleWizard(t *testing.T) {
	m := NewSetupWizardModel()
	m.Open() // first run, sitting on the mode-selection screen

	m.OpenCookieLogin("twitch")

	if m.cookieOnly {
		t.Error("OpenCookieLogin converted a first-run wizard into a cookie-only overlay")
	}
	if m.mode != setupModeSelect {
		t.Errorf("mode = %v, want setupModeSelect — the first-run wizard was moved out from under "+
			"the operator", m.mode)
	}
}

// TestOpenClearsTheCookieOnlyFlag: Open() is the first-run entrance and must
// leave no trace of a previous cookie-login overlay.
//
// MUTANT: omit `m.cookieOnly = false` from Open(). The first-run wizard's cookie
// step then closes on Esc and never reaches the channel editor — first run
// breaks, and only after someone used the chord first.
func TestOpenClearsTheCookieOnlyFlag(t *testing.T) {
	m := NewSetupWizardModel()
	m.OpenCookieLogin("youtube")
	m.closeCookieLogin()

	m.Open()

	if m.cookieOnly {
		t.Error("Open() inherited cookieOnly from an earlier cookie-login overlay")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 -run "CookieLogin|TestOpenClearsTheCookieOnlyFlag" ./internal/tui/`
Expected: FAIL to compile — `m.OpenCookieLogin undefined`, `m.closeCookieLogin undefined`, `m.cookieOnly undefined`.

- [ ] **Step 3: Add the field and the reset**

In `internal/tui/setup_wizard.go`, after `cookieTickActive bool` (`:215`) inside the countdown bookkeeping block:

```go
	// cookieOnly makes this wizard a cookie-login overlay rather than the
	// first-run flow: the same step, the same callbacks, the same countdown,
	// but no stages around it. It changes only the two ways OUT — Esc and the
	// third list row close the overlay instead of walking to the channel
	// editor, whose Tab would rewrite config.toml on a configured install.
	// See OpenCookieLogin.
	cookieOnly bool
```

In `Open()`, immediately after `m.simpleStage = setupSimpleCookies` (`:280`):

```go
	m.cookieOnly = false
```

- [ ] **Step 4: Add the two methods**

In `internal/tui/setup_wizard.go`, immediately after `armCookieTick` (which ends at `:421`):

```go
// OpenCookieLogin opens the wizard at its cookie step alone — the R L chord's
// entrance, for an install that is already set up.
//
// It deliberately does NOT go through Open(). Open() is the FIRST-RUN entrance:
// it drops to setupModeSelect, clears the config values and channel list the
// wizard is about to collect, and runs OnCheckFFmpeg — none of which a cookie
// login on a configured install has any business doing. What this sets instead
// is exactly the cookie flow's own state, so the operator lands on the same
// state machine the first-run wizard drives: the same OnStartAutoCookie /
// OnFinishAutoCookie / OnCancelAutoCookie, the same cookieSetupCountdownSeconds
// countdown, the same three-state verdict. No second implementation to drift.
//
// platform PRESELECTS the focused row ("twitch" → Twitch, anything else →
// YouTube) and starts nothing, so an unrecognised value costs one keystroke and
// can never open a browser at the wrong site.
//
// A no-op while the wizard is already visible: a chord must not throw away a
// first-run setup in progress, and a second overlay over the first would reset
// a countdown that has a real browser window behind it.
func (m *SetupWizardModel) OpenCookieLogin(platform string) {
	if m.visible {
		return
	}
	m.visible = true
	m.cookieOnly = true
	m.mode = setupModeSimple
	m.simpleStage = setupSimpleCookies
	m.errorMsg = ""
	m.saving = false
	m.cookieFocus = 0
	if platform == "twitch" {
		m.cookieFocus = 1
	}
	m.cookieActive = false
	m.cookieFinishing = false
	m.cookiePlatform = ""
	m.cookieYTDone = false
	m.cookieTWDone = false
	m.cookieCountdown = 0
	m.cookieTimedOut = false
	m.cookieTickActive = false // any in-flight chain is stale-gen'd by the next arm
}

// closeCookieLogin ends a cookie-login overlay, cancelling an in-flight setup
// first. The cancel is not decoration: AutoCookieService holds the acquisition
// slot until someone cancels, finishes, or the server-side reap notices the
// browser is gone, so leaving with a browser open would meet the next R L —
// and the next periodic refresh — with ErrSetupInProgress for the whole grace
// window. Every way OUT funnels through here rather than calling Close()
// directly, so a later exit added beside one of them inherits the release
// instead of forgetting it.
func (m *SetupWizardModel) closeCookieLogin() {
	if m.cookieActive && m.OnCancelAutoCookie != nil {
		m.OnCancelAutoCookie()
	}
	m.cookieActive = false
	m.cookieFinishing = false
	m.cookiePlatform = ""
	m.cookieTimedOut = false
	m.cookieOnly = false
	m.Close()
}
```

- [ ] **Step 5: Branch the two exits in `handleSimpleCookieKey`**

In `internal/tui/setup_wizard.go`, in `handleSimpleCookieKey`, replace the `keyEsc` case of the final switch (`:674-675`):

```go
	case keyEsc:
		if m.cookieOnly {
			m.closeCookieLogin()
			return ""
		}
		m.mode = setupModeSelect
```

and replace the third arm of the focus switch (`:710-713`):

```go
		case 2: // Skip / Next — "Close" in the cookie-login overlay
			if m.cookieOnly {
				m.closeCookieLogin()
				return ""
			}
			m.simpleStage = setupSimpleChannels
			m.channelIndex = 0
			m.channelMode = "list"
```

Leave the `m.cookieActive` block above them alone: Esc over a live setup already cancels and returns to the picker, which is the right behaviour on both entrances. Leave the `m.cookieTimedOut` block alone too — its `S` clears the timeout and returns to the picker, and the overlay is one Esc from closing.

- [ ] **Step 6: Branch the view**

In `viewSimpleCookies` (`:1378`), replace the header block — from the `// Header` comment through the divider line that follows `Log in to platforms to enable cookie-based access` (`:1383-1396`; the replacement below re-emits those last two lines, so stopping at the `Cookie Setup` line would print them twice) — with:

```go
	// Header. The cookie-login overlay is ONE step and leads nowhere, so it
	// carries neither the "Step 1/2" counter nor the step dots; both would be
	// promising a second screen that does not exist here.
	if m.cookieOnly {
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Cookie Login"))
	} else {
		titleRendered := lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render("Quick Setup")
		stepRendered := DimStyle.Render("Step 1/2")
		titlePad := max(contentW-runewidth.StringWidth("Quick Setup")-runewidth.StringWidth("Step 1/2"), 1)
		lines = append(lines, titleRendered+strings.Repeat(" ", titlePad)+stepRendered)

		step1 := lipgloss.NewStyle().Foreground(ColorCyan).Render("[>] 1")
		step2 := lipgloss.NewStyle().Foreground(ColorGray).Render("[ ] 2")
		lines = append(lines, step1+DimStyle.Render(" - ")+step2)

		lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Cookie Setup"))
	}
	lines = append(lines, DimStyle.Render("Log in to platforms to enable cookie-based access"))
	lines = append(lines, DimStyle.Render(strings.Repeat("\u2500", contentW)))
```

In the platform-selection branch, immediately after the `options := []struct{…}{…}` literal (`:1429-1436`), add:

```go
		if m.cookieOnly {
			// Not "Skip / Next": there is nothing after this step to skip to.
			options[2].label = "Close"
		}
```

And in the navigation footer (`:1469-1472`), replace the four lines that build the hints so the left label is a variable rather than a literal repeated in the width maths:

```go
	escLabel := "Esc: Back"
	if m.cookieOnly {
		escLabel = "Esc: Close"
	}
	hintLeft := DimStyle.Render(escLabel)
	hintRight := DimStyle.Render("Enter: Select")
	gap := max(1, contentW-runewidth.StringWidth(escLabel)-runewidth.StringWidth("Enter: Select"))
	lines = append(lines, hintLeft+strings.Repeat(" ", gap)+hintRight)
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test -count=1 -run "CookieLogin|TestOpenClearsTheCookieOnlyFlag|TestEveryCookieSetupEntryPointArmsTheSameCountdown|TestSetupCookieFinishFeedback" ./internal/tui/`
Expected: PASS.

Note on `TestEveryCookieSetupEntryPointArmsTheSameCountdown` (`internal/tui/setup_wizard_cookie_countdown_test.go:33`): it enumerates the six sites that *arm* the countdown. This task adds **no seventh arming site** — `OpenCookieLogin` starts nothing, and the Enter that does go through `handleSimpleCookieKey`, which the test already covers twice. Do not add a row to it.

- [ ] **Step 8: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/ && go test -count=1 ./...
```
Expected: clean.

- [ ] **Step 9: Commit**

```bash
git add internal/tui/setup_wizard.go internal/tui/setup_wizard_cookie_login_test.go
git commit -m "$(cat <<'EOF'
feat(tui): the setup wizard opens at its cookie step alone

OpenCookieLogin lands on the existing simple-mode cookie step with a
cookieOnly flag that changes only the two exits — Esc and the third list
row close the overlay instead of walking into the first-run channel
editor, whose Tab would rewrite config.toml on a configured install. No
new mode, no new form, no second state machine: the same three cookie
callbacks and the same 300s countdown the first-run wizard drives, and
every exit funnels through closeCookieLogin so an abandoned overlay
releases the acquisition slot.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 3: the `R L` chord (V9, chord half) and the docs it makes true

**Files:**
- Modify: `internal/tui/app_actions.go` — one case in `dispatchAction` (after the `R F` case, `:198-211`), one entry in `buildMenuItems` (after the `R F` registration, `:531-533`)
- Modify: `internal/tui/status_bar.go` — add `ReloginPlatform()` after `SetActivePlatforms` (`:111-114`)
- Modify: `docs/spec/user-interfaces.md` — `:166` (file table), `:235` (Request chord table), `:282` (overlay table), `:687` (the in-process sentence's callback list), after `:713` (a new `R L` paragraph), after `:852` (a new parity row)
- Modify: `CLAUDE.md:109` (the `R` chords sentence)
- Modify: `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2010` (the V9 bullet)
- Test: `internal/tui/cookie_login_chord_test.go` (create)

**Interfaces:**
- Consumes: `(*SetupWizardModel).OpenCookieLogin(platform string)` and the `cookieOnly` field from Task 2; `SetupWizardModel.OnStartAutoCookie func(platform string) error` (bound unconditionally by `cmd/moombox/tui_wiring.go:461` via `App.SetSetupCallbacks`); `App.setupWiz`, `App.statusBar`, `App.setFeedback(string)`, `App.width` / `App.height`.
- Produces, for Task 4: `func (m *StatusBarModel) ReloginPlatform() string` — `"youtube"`, `"twitch"`, or `""`, over the *active* platforms only, YouTube first when both are flagged.

**`cmd/moombox` needs NO change — verified.** `app.SetSetupCallbacks(...)` at `cmd/moombox/tui_wiring.go:461` is called unconditionally and passes all three cookie callbacks: `OnStartAutoCookie` → `s.autoCookieSvc.StartSetup(platform)` (`:475-481`), `OnFinishAutoCookie` → `FinishSetupDetailed` under a 60 s ctx plus the `recheckAfterCookieWrite` defer (`:490-526`), `OnCancelAutoCookie` → `s.autoCookieSvc.CancelSetup()` (`:527-528`). There is no `auto_enabled` gate on any of them and `StartSetup` itself is never gated (`data-and-storage.md:884`). Nothing in `cmd/moombox` is edited by this plan.

**Refusal path — the chord has none of its own, and that is the design.** `StartSetup` has no "service not configured" gate to trip; what it returns is `ErrServiceStopped`, `ErrSetupInProgress`, `ErrRefreshInProgress` or `ErrNoBrowserFound`, and the wizard already renders those inline (`m.errorMsg = fmt.Sprintf("YouTube cookies: %v", err)`) — that is R1's "the wizard says so, as the web panel does", and it is reached by the operator's Enter, not by the chord. The one condition the chord can see for itself — **no callback at all** — does not produce a sentence of the chord's own, because it cannot: `processSecondKey` (`internal/tui/app_keys.go`) resolves the second key against `buildMenuItems()`, an unregistered pair falls through to `handleChord`'s `Invalid Chord: R L`, and the action menu is built from the same list. So with `OnStartAutoCookie == nil` the chord does not *exist* — the `R F` model verbatim — and `dispatchAction`'s nil-guard in Step 4 is defensive (a direct caller, a test) rather than operator-facing. Do not "fix" that by registering the chord unconditionally so the guard can speak: `R N` registers on either of two sources and refuses on a third condition (`OnFetchReleaseNotes == nil || a.version == ""`, `app_actions.go:242`), which is why *its* sentence is reachable; `R L` has one source and one gate.

- [ ] **Step 1: Write the failing test**

Create `internal/tui/cookie_login_chord_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// rlIsOffered reports whether R L is reachable by the three routes an operator
// has: the chord dispatcher, the action menu, and the help overlay. All three,
// because they are three separate reads of OnStartAutoCookie and a fix that
// restored one would look complete from the others. Same shape as rfIsOffered
// in cookie_forcerefresh_chord_test.go, deliberately — the two chords answer
// the same question about the same service.
func rlIsOffered(t *testing.T, app *App) (dispatch, menu, help bool) {
	t.Helper()

	app.dispatchAction("R L", nil)
	dispatch = app.setupWiz.IsVisible() && app.setupWiz.cookieOnly
	app.setupWiz.closeCookieLogin() // the funnel, not Close(): leaves no cookieOnly behind

	items := app.buildMenuItems()
	if len(items) == 0 {
		t.Fatal("buildMenuItems returned nothing — nothing below can be concluded")
	}
	for _, it := range items {
		if strings.TrimSpace(it.Chord) == "R L" {
			menu = true
		}
	}

	h := NewHelpModel()
	h.SetMenuItems(items)
	for _, sec := range h.orderedSections() {
		for _, k := range sec.keys {
			if strings.TrimSpace(k.key) == "R L" {
				help = true
			}
		}
	}
	return dispatch, menu, help
}

// wiredCookieLoginApp is an App with the interactive-setup callbacks bound the
// way cmd/moombox binds them — unconditionally, all three.
func wiredCookieLoginApp() *App {
	app := NewApp()
	app.SetSetupCallbacks(
		func(*config.MoomboxConfig) error { return nil },
		func(int, bool) {},
		func(string) error { return nil },
		func() (cookies.SetupResult, error) { return cookies.SetupResult{}, nil },
		func() {},
		func() {},
	)
	return app
}

// TestCookieLoginChordExistsWheneverTheWizardCanStartALogin is the junction
// guard, and the reason the chord is gated on the callback rather than on
// cookies.auto_enabled. A nil callback does not make a chord inert, it DELETES
// it — dispatchAction, buildMenuItems and the help overlay each test the field
// — which is exactly the defect R F was fixed for. cmd/moombox binds this
// callback unconditionally, so the chord exists on every real install,
// including one with auto_enabled off: StartSetup is acquisition, never gated.
//
// The nil row gives the wired row its meaning: without it, "R L is offered"
// would be satisfied by a build that offers every chord unconditionally.
func TestCookieLoginChordExistsWheneverTheWizardCanStartALogin(t *testing.T) {
	t.Run("wired", func(t *testing.T) {
		dispatch, menu, help := rlIsOffered(t, wiredCookieLoginApp())
		if !dispatch {
			t.Error("R L opened no cookie-login overlay although the callback is wired")
		}
		if !menu {
			t.Error("R L is absent from the action menu although the callback is wired")
		}
		if !help {
			t.Error("R L is absent from help although the callback is wired — an operator cannot " +
				"discover a chord that is documented nowhere")
		}
	})

	t.Run("not wired", func(t *testing.T) {
		app := NewApp() // SetSetupCallbacks never called
		dispatch, menu, help := rlIsOffered(t, app)
		if dispatch || menu || help {
			t.Fatalf("premise lost: R L is offered with no interactive-setup callback "+
				"(dispatch=%v menu=%v help=%v)", dispatch, menu, help)
		}
	})
}

// TestCookieLoginChordOpensAtTheCookieStep pins WHERE the chord lands.
//
// MUTANT: have the case call a.setupWiz.Open(). The wizard then opens on the
// "Welcome to Moombox — Quick or Advanced" mode-selection screen, from a chord
// whose label says Cookie Login, and its Advanced path leads to a form that
// rewrites config.toml.
func TestCookieLoginChordOpensAtTheCookieStep(t *testing.T) {
	app := wiredCookieLoginApp()

	app.dispatchAction("R L", nil)

	if !app.setupWiz.IsVisible() {
		t.Fatal("R L opened nothing")
	}
	if app.setupWiz.mode != setupModeSimple {
		t.Errorf("mode = %v, want setupModeSimple", app.setupWiz.mode)
	}
	if app.setupWiz.simpleStage != setupSimpleCookies {
		t.Errorf("stage = %v, want setupSimpleCookies", app.setupWiz.simpleStage)
	}
	if !app.setupWiz.cookieOnly {
		t.Error("the chord opened the first-run flow rather than the cookie-login overlay")
	}
}

// TestCookieLoginChordPreselectsThePlatformTheBadgeIsAlarmingAbout closes the
// loop between the alert and its remedy.
//
// MUTANT: pass "" instead of a.statusBar.ReloginPlatform(). The Twitch row then
// opens focused on YouTube, and the operator answering a "TW: Re-login" badge
// signs in to the wrong site.
func TestCookieLoginChordPreselectsThePlatformTheBadgeIsAlarmingAbout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		yt, tw    CookieStatus
		ytA, twA  bool
		wantFocus int
	}{
		{"twitch flagged", CookieStatusOK, CookieStatusRelogin, true, true, 1},
		{"youtube flagged", CookieStatusRelogin, CookieStatusOK, true, true, 0},
		{"both flagged — youtube first", CookieStatusRelogin, CookieStatusRelogin, true, true, 0},
		{"nothing flagged", CookieStatusOK, CookieStatusOK, true, true, 0},
		{"twitch flagged but inactive", CookieStatusOK, CookieStatusRelogin, true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := wiredCookieLoginApp()
			app.statusBar.SetActivePlatforms(tc.ytA, tc.twA)
			app.statusBar.SetCookieStatus(tc.yt, tc.tw)

			app.dispatchAction("R L", nil)

			if app.setupWiz.cookieFocus != tc.wantFocus {
				t.Errorf("cookieFocus = %d, want %d", app.setupWiz.cookieFocus, tc.wantFocus)
			}
		})
	}
}

// TestCookieLoginChordRefusesWithoutTheService pins the DEFENSIVE branch: a
// direct dispatch with no interactive-setup callback opens nothing and says
// so. From the keyboard this branch is unreachable — processSecondKey resolves
// the second key against buildMenuItems, and with the callback nil R L is not
// registered, so the operator sees the chord system's own "Invalid Chord: R L"
// and finds no menu or help entry (the "not wired" row above pins that). The
// guard exists so a programmatic caller cannot open an overlay whose every
// Enter dead-ends, and so the two reads of OnStartAutoCookie — buildMenuItems
// and this case — cannot disagree silently.
//
// MUTANT: drop the nil check. dispatchAction("R L") on a bare App then opens
// the overlay with nothing behind it.
func TestCookieLoginChordRefusesWithoutTheService(t *testing.T) {
	app := NewApp()

	app.dispatchAction("R L", nil)

	if app.setupWiz.IsVisible() {
		t.Error("R L opened a login overlay with no auto-cookie service behind it")
	}
	if app.feedbackMsg == "" {
		t.Fatal("R L refused silently — a direct caller cannot tell that from a no-op")
	}
	if !strings.Contains(strings.ToLower(app.feedbackMsg), "cookie login") {
		t.Errorf("the refusal does not name what was refused: %q", app.feedbackMsg)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 -run CookieLoginChord ./internal/tui/`
Expected: FAIL — `app.statusBar.ReloginPlatform undefined` at compile time; once that is added, the wired/dispatch rows fail because `R L` reaches no case and `buildMenuItems` has no entry.

- [ ] **Step 3: Add `ReloginPlatform`**

In `internal/tui/status_bar.go`, after `SetActivePlatforms` (`:111-114`):

```go
// ReloginPlatform names the platform this bar is currently asking the operator
// to sign back in to, or "" when neither is.
//
// Gated on the ACTIVE flags, so it answers about the platforms the bar actually
// renders: a Relogin verdict for a platform with no configured monitors is not
// being shown to anyone, and preselecting it would answer an alarm never raised.
// YouTube wins when both are flagged — the overlay signs in to one platform at
// a time, the operator can pick the other row, and more of the pipeline depends
// on YouTube's credentials.
//
// Two readers, deliberately ONE predicate: the R L chord preselects this
// platform, and renderCookieStatus decides on it whether to name the chord at
// all. A second copy would let the badge advertise a remedy the chord then
// opens elsewhere.
func (m *StatusBarModel) ReloginPlatform() string {
	if m.ytActive && m.ytCookie == CookieStatusRelogin {
		return "youtube"
	}
	if m.twActive && m.twCookie == CookieStatusRelogin {
		return "twitch"
	}
	return ""
}
```

- [ ] **Step 4: Add the chord**

In `internal/tui/app_actions.go`, in `dispatchAction`, immediately after the `case "R F":` block (which ends at `:211`, before `case "R V":`):

```go
	case "R L":
		// Defensive, and unreachable from the keyboard: with no callback the
		// chord is not registered (buildMenuItems below), so processSecondKey
		// reports "Invalid Chord: R L" before this case is consulted. The guard
		// keeps a direct caller from opening an overlay whose every Enter
		// dead-ends. StartSetup's own refusals — service stopped, a setup or
		// refresh already running, no supported browser — arrive on the
		// operator's Enter and are rendered inline by the wizard, which is
		// where the web panel puts them too.
		if a.setupWiz.OnStartAutoCookie == nil {
			a.setFeedback("Cookie login is unavailable — no auto-cookie service is configured")
			return a, nil
		}
		a.setupWiz.SetSize(a.width, a.height)
		// Preselect whatever the status bar is alarming about, so answering a
		// "TW: Re-login" badge does not open on YouTube.
		a.setupWiz.OpenCookieLogin(a.statusBar.ReloginPlatform())
```

In `buildMenuItems`, immediately after the `R F` registration (`:531-533`):

```go
	// R L: open the setup wizard's cookie step alone, so an interactive login
	// is reachable after first run. Gated on the callback for the same reason
	// R F is, and cmd/moombox binds it unconditionally: StartSetup is
	// acquisition and is never gated on cookies.auto_enabled.
	if a.setupWiz.OnStartAutoCookie != nil {
		items = append(items, ActionMenuItem{Chord: "R L", Label: "Cookie Login", HintLabel: "Login", Category: "Request"})
	}
```

No `NeedsConfirm`, no `NeedsJob`, no `JobFilter`: it opens an overlay and touches no job.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -count=1 -run "CookieLoginChord|TestHelpCoversEveryChord|TestForceRefreshChordExistsWheneverItIsWired" ./internal/tui/`
Expected: PASS. `TestHelpCoversEveryChord` picks the new chord up automatically because it is Request-category; if it fails, the chord was added somewhere other than `buildMenuItems()`.

- [ ] **Step 6: Update `docs/spec/user-interfaces.md`**

1. The TUI file table, `:166` — replace the `setup_wizard.go` row's description:

> | `setup_wizard.go` | First-run setup overlay, and — via `R L` — the standalone cookie-login step on a configured install. Built with `huh`. Config, FFmpeg, yt-dlp plugin, cookies. |

2. The Request chord table, after the `R F` row (`:235`):

> | `R L` | Cookie Login | Interactive-setup callback is configured (`SetSetupCallbacks`, bound unconditionally by `cmd/moombox`). Opens the setup wizard's cookie step **alone** — pick YouTube or Twitch, sign in in the browser that opens on the host, `Enter` extracts. Preselects the platform the status bar is flagging for re-login. |

3. The overlay table, the Setup Wizard row (`:282`) — replace the Trigger cell and extend the description:

> | Setup Wizard | First run, `R L` | Multi-step initial setup: configuration, FFmpeg check/install, yt-dlp plugin, cookie capture. Built with `huh`. `R L` opens the same overlay in **cookie-only** mode: the cookie step with no stages around it, `Esc` and the third row close it instead of advancing, and leaving cancels any browser it opened. |

4. A new paragraph in §What the TUI renders, immediately after the rung-3 divergence sentence at `:713`:

> **`R L` — Cookie Login.** The TUI's entrance to the interactive browser login, and the answer to a `Re-login` badge. It opens `SetupWizardModel` at its cookie step in cookie-only mode (`OpenCookieLogin`, `internal/tui/setup_wizard.go`) — the *same* state machine the first-run wizard drives, not a second one: `OnStartAutoCookie` → `AutoCookieService.StartSetup`, the 300 s countdown (`cookieSetupCountdownSeconds`), `Enter` → `OnFinishAutoCookie` → `FinishSetupDetailed` under `cmd/moombox`'s 60 s ctx, and the same four-arm verdict rendering that distinguishes *accepted* from *verified*. What cookie-only mode changes is the two exits: `Esc` at the picker and the third list row (`Close`, not `Skip / Next`) close the overlay rather than walking into the first-run channel editor, whose `Tab` would rewrite `config.toml` on a configured install. **Every exit funnels through `closeCookieLogin`, which cancels an in-flight setup first**, so an abandoned overlay releases the acquisition slot instead of leaving it for the server-side reap; `Esc` while the browser is open cancels and returns to the picker rather than closing. The chord is gated on the callback exactly as `R F` is — a nil callback deletes a chord rather than making it inert — and `cmd/moombox` binds it unconditionally, so `R L` exists with `cookies.auto_enabled` off: `StartSetup` is acquisition and is never gated. With no interactive-setup callback at all the chord does not exist: `R L` reports `Invalid Chord: R L` like any unregistered pair and is absent from the menu and from help (the `dispatchAction` nil-guard behind it is defensive and unreachable from the keyboard); `StartSetup`'s own refusals — stopped service, a setup or refresh already running, no supported browser — arrive on the operator's `Enter` and render inline in the wizard, where the dashboard puts them too.

6. The in-process sentence at `:687` — extend its callback list so it stays complete now that a fourth cookie chord exists. Replace

> The TUI's cookie chords do **not** go through these REST endpoints. `OnRecheckCookies`, `OnAutoCookieLastError` and `OnForceRefreshCookies` (`cmd/moombox/tui_wiring.go`) call `RefreshService` and `AutoCookieService` in-process, so both surfaces exercise the same services but not the same handlers

with

> The TUI's cookie chords do **not** go through these REST endpoints. `OnRecheckCookies`, `OnAutoCookieLastError` and `OnForceRefreshCookies` — and, behind `R L`, the wizard's `OnStartAutoCookie` / `OnFinishAutoCookie` / `OnCancelAutoCookie` (all bound in `cmd/moombox/tui_wiring.go`) — call `RefreshService` and `AutoCookieService` in-process, so both surfaces exercise the same services but not the same handlers

5. A new row in the cookie-parity table, after the "Manual refresh gesture" row (`:852`):

> | Interactive login | **Same operation, different affordances — and one question only the dashboard has to ask.** Both surfaces drive `StartSetup` / `FinishSetupDetailed` / `CancelSetup`: the TUI in-process through the wizard's three callbacks, the dashboard over the `/auto-setup/*` trio. The dashboard must decide whether *this viewer* may open a browser window on the host (`reloginPromptTarget`, and the import box for everyone else); a TUI session is the host, so `R L` has nothing to route. |

- [ ] **Step 7: Update `CLAUDE.md`**

`CLAUDE.md:109` — extend the `R` chords sentence, keeping its existing style. Replace the trailing clause

> and `R B` (Re-scan Feed History — forces a full-catalog backfill re-scan of every configured YouTube channel).

with

> `R B` (Re-scan Feed History — forces a full-catalog backfill re-scan of every configured YouTube channel) and `R L` (Cookie Login — opens the setup wizard's cookie step alone so the browser login is reachable after first run; preselects the platform the status bar flags for re-login).

- [ ] **Step 8: Update the remediation plan's V9 bullet**

`docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2010` — append to the end of the V9 bullet:

> **DONE (Arc 12a, 2026-09-02):** the `R L` chord opens `SetupWizardModel` at its cookie step in cookie-only mode (`OpenCookieLogin`), reusing the wizard's existing callbacks, countdown and verdict rendering; gated on the interactive-setup callback the way `R F` is gated on its own, and `cmd/moombox` needed no change — `SetSetupCallbacks` already bound all three unconditionally. `user-interfaces.md` gains the chord row, the `R L` paragraph and the interactive-login parity row.

- [ ] **Step 9: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/ && go test -count=1 ./...
```
Expected: clean.

- [ ] **Step 10: Commit**

```bash
git add internal/tui/app_actions.go internal/tui/status_bar.go internal/tui/cookie_login_chord_test.go \
        docs/spec/user-interfaces.md CLAUDE.md docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md
git commit -m "$(cat <<'EOF'
feat(tui): R L opens the cookie login after first run

The wizard has owned the whole interactive login and was reachable from
one site: `if a.IsFirstRun`. R L is the second entrance — one
buildMenuItems entry, one dispatchAction case, no new flow. It
preselects the platform the status bar is flagging (ReloginPlatform, the
predicate the badge will also read) and is gated on the interactive-setup
callback the way R F is on its own: a nil callback deletes a chord rather
than making it inert. cmd/moombox needed no change; it already bound all
three cookie callbacks unconditionally.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

### Task 4: the `Re-login` badge names the chord that answers it (R2)

**Files:**
- Modify: `internal/tui/status_bar.go` — `renderCookieStatus`, just before the `if len(parts) == 0` return (`:611`)
- Modify: `docs/spec/user-interfaces.md` — the §Cookie indicators paragraph (`:380`) and the "Re-login required" parity row (`:841`). Line numbers are as of `390adb6`, BEFORE Task 3's insertions (one chord row, one paragraph, one parity row) shift them to about `:381` and `:845` — locate both by content.
- Test: `internal/tui/status_bar_test.go` (append)

**Interfaces:**
- Consumes: `(*StatusBarModel).ReloginPlatform() string` from Task 3; the package-level `statusBarKeyStyle`, `DimStyle`; the `barTier` ladder.
- Produces: nothing further.

**Why `tierFull` only.** The hint is the widest thing this function can add and the least urgent (the alert is the information; the remedy is one keypress away in the menu and in help), so it belongs on the rung that is given up first. That placement is also what keeps the ladder trivially safe: `metricTiers` must narrow monotonically — `TestStatusBarTiersNarrowMonotonically` pins it, and `fitTiers`' "first fit is the richest fit" scan depends on it (`status_bar.go:178-223`) — and a suffix that appears only on the top rung can never make a lower rung wider than the one above it. Note what that existing test does NOT do: its `busyStatusBar()` fixture is `OK`/`OK`, so `ReloginPlatform()` is empty there and the hint never renders inside it; the new test below therefore sweeps the ladder on a *flagged* bar itself. Appending it **once for the bar** rather than once per platform is the same width reasoning: two copies spend the width twice to say one thing.

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/status_bar_test.go`:

```go
// TestReloginBadgeNamesTheChordThatAnswersIt pins R2: a bar that says
// "YT: Re-login" and stops has named a problem and no remedy. The dashboard's
// equivalent warning is clickable; R L is the TUI's click, and the badge is
// where an operator is looking when they need it.
//
// MUTANTS, one per assertion:
//   - drop the append → the widest bar names no remedy at all;
//   - append per platform instead of once → the both-flagged row reads
//     "YT: Re-login (R L) TW: Re-login (R L)", spending scarce width twice;
//   - append at tierCompact or below → the compact assertion fails (the hint
//     outlived the first squeeze); append at a LOWER tier but not tierFull →
//     that rung is wider than the one above it, which the ladder sweep at the
//     end catches. TestStatusBarTiersNarrowMonotonically cannot: its
//     busyStatusBar fixture is OK/OK and never renders the hint;
//   - gate on something other than ReloginPlatform (a bare `!= CookieStatusOK`,
//     say) → the healthy row advertises a login nobody needs.
func TestReloginBadgeNamesTheChordThatAnswersIt(t *testing.T) {
	flagged := func(yt, tw CookieStatus) *StatusBarModel {
		m := NewStatusBarModel()
		m.SetActivePlatforms(true, true)
		m.SetCookieStatus(yt, tw)
		return m
	}

	full := stripANSI(flagged(CookieStatusRelogin, CookieStatusOK).renderCookieStatus(tierFull))
	if !strings.Contains(full, "R L") {
		t.Errorf("the re-login badge names no chord at tierFull: %q", full)
	}

	both := stripANSI(flagged(CookieStatusRelogin, CookieStatusRelogin).renderCookieStatus(tierFull))
	if got := strings.Count(both, "R L"); got != 1 {
		t.Errorf("the chord is named %d times with both platforms flagged, want 1: %q", got, both)
	}

	compact := stripANSI(flagged(CookieStatusRelogin, CookieStatusOK).renderCookieStatus(tierCompact))
	if strings.Contains(compact, "R L") {
		t.Errorf("the hint survived past tierFull, which breaks the monotonic ladder: %q", compact)
	}

	healthy := stripANSI(flagged(CookieStatusOK, CookieStatusOK).renderCookieStatus(tierFull))
	if strings.Contains(healthy, "R L") {
		t.Errorf("a healthy bar advertises a cookie login: %q", healthy)
	}

	// The alert itself must still outlive the hint — the hint is the part that
	// is allowed to go, not the badge.
	tight := stripANSI(flagged(CookieStatusRelogin, CookieStatusOK).renderCookieStatus(tierTight))
	if !strings.Contains(tight, "YT") {
		t.Errorf("the re-login alert was dropped along with its hint: %q", tight)
	}

	// The ladder, swept on a FLAGGED bar. TestStatusBarTiersNarrowMonotonically
	// runs on busyStatusBar(), which is OK/OK, so this is the only place the
	// hint is inside the monotonicity check that fitTiers' scan relies on.
	ladder := flagged(CookieStatusRelogin, CookieStatusRelogin)
	ladder.SetWidth(200)
	tiers := ladder.metricTiers()
	for i := 1; i < len(tiers); i++ {
		if prev, cur := lipgloss.Width(tiers[i-1]), lipgloss.Width(tiers[i]); cur > prev {
			t.Errorf("metrics tier %d (%d cols) is wider than tier %d (%d cols) with the re-login "+
				"hint in play — the hint has landed on a rung below the one it is dropped from", i, cur, i-1, prev)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 -run TestReloginBadgeNamesTheChordThatAnswersIt ./internal/tui/`
Expected: FAIL — `the re-login badge names no chord at tierFull: "YT: Re-login "`.

- [ ] **Step 3: Append the hint**

In `internal/tui/status_bar.go`, in `renderCookieStatus`, immediately before `if len(parts) == 0 {` (`:611`):

```go
	// The chord that ANSWERS the alert, named once for the bar and only where
	// there is room. A badge that says "Re-login" and stops has named a problem
	// and no remedy; the dashboard's warning is clickable, and R L is this
	// surface's click.
	//
	// tierFull only: it is the widest thing this function can add and the least
	// urgent — the alert is the information, and the remedy is also in the menu
	// and in help — so it is given up first, which is what keeps metricTiers
	// narrowing monotonically for fitTiers' scan. ReloginPlatform is the same
	// predicate R L preselects on, so the badge cannot advertise a remedy that
	// then opens on the other platform.
	if t == tierFull && m.ReloginPlatform() != "" {
		parts = append(parts, DimStyle.Render("(")+statusBarKeyStyle.Render("R L")+DimStyle.Render(")"))
	}

	if len(parts) == 0 {
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/tui/`
Expected: PASS — the new test plus `TestStatusBarTiersNarrowMonotonically`, `TestStatusBarNeverExceedsWidth`, `TestStatusBarAlertsOutliveCounters` and `TestStatusBarHealthyAuthYieldsToAlerts`, all of which exercise the changed function.

- [ ] **Step 5: Update `docs/spec/user-interfaces.md`**

1. §Status Bar → Cookie indicators (`:380`) — append one sentence to the end of that paragraph:

> **At `tierFull` a flagged badge also names `R L`**, the chord that opens the login, once for the bar however many platforms are flagged. It is dropped at the first squeeze on purpose: the alert is the information, the remedy is also in the menu and in help, and the hint is the widest thing this section can add — `metricTiers` must narrow monotonically or `fitTiers`' first-fit-is-richest-fit scan stops holding. The badge and the chord read one predicate, `ReloginPlatform`, so the platform named in the bar is the platform the overlay opens on.

2. The parity table's "Re-login required" row (`:841`) — replace the TUI column:

> | Re-login required | `YT: Re-login` / `TW: Re-login` in the warnings area, clickable to start setup | Folded into the platform indicator as `YT: Re-login` / `YT!`, and at `tierFull` followed by `(R L)` — the chord that opens the same interactive setup the dashboard's click does |

- [ ] **Step 6: Run the full gates**

```bash
go build ./... && go vet ./... && GOOS=linux GOARCH=amd64 go build ./... && gofmt -l internal/ cmd/ && go test -count=1 ./...
```
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/status_bar.go internal/tui/status_bar_test.go docs/spec/user-interfaces.md
git commit -m "$(cat <<'EOF'
feat(tui): the Re-login badge names R L, the chord that answers it

A badge that says "Re-login" and stops names a problem and no remedy.
The hint is appended once for the bar and only at tierFull: it is the
widest thing renderCookieStatus can add and the least urgent, and
metricTiers has to keep narrowing monotonically for fitTiers' scan. Badge
and chord read one predicate — ReloginPlatform — so the platform named in
the bar is the platform the overlay opens on.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Self-Review

**1. Spec coverage.**

| Spec item | Task |
|-----------|------|
| R1 — a chord opens the cookie login from anywhere, one `R` chord, letter chosen from what `buildMenuItems()` leaves free with alternatives recorded, in `buildMenuItems()` + `dispatchAction()`, lands on the cookie step for a picked platform, no first-run steps re-run, reuses the existing state machine and callbacks unchanged, refused on the status line when the service is absent | Chord-letter section (choice + 5 rejections) + Task 2 (`OpenCookieLogin`, no new mode/form) + Task 3 (entry, case, refusal sentence, junction guard) |
| R2 — the `Relogin` badge names the chord | Task 4 |
| R3 — parity with the web wizard, not a new flow; Start → countdown → Finish/Cancel through the existing `On*AutoCookie`; the three-state verdict; abandoning calls the same cancel path | Task 2 (the flow is untouched; `closeCookieLogin` is the abandon funnel, pinned by three subtests) + Task 3's doc paragraph and parity row. **The cancel path is pinned as `OnCancelAutoCookie` → `cmd/moombox/tui_wiring.go:527` → `AutoCookieService.CancelSetup()`, not `AbandonSetup`** — see the deviation note below |
| R4 — `cookieKey` gains `path` from `fields[2]`; one collision test with the drop-`path` mutant; the three callers untouched | Task 1 |
| R5 — docs: `user-interfaces.md` (file table, chord table, overlay table, the `:687` callback list, the `R L` paragraph, two parity rows), CLAUDE.md's `R` chords line, plan `:2010`/`:2012`, `data-and-storage.md:872` | Tasks 1, 3, 4, each in the commit that makes its claim true. Grep found no standalone "no cookie-setup entry point" sentence in `docs/spec/`; the only absence claim is the plan's V9 bullet, closed in Task 3 |
| Non-goals — no REST/web change, no config, no change to the first-run flow or the 300 s countdown, `deduplicateAndFormat`'s bare-name key untouched | Honoured. Task 2 changes exit behaviour *only* under `cookieOnly`, with a premise subtest asserting first run still advances to channels; Task 1 explicitly leaves `autocookies_merge.go:94` alone |
| Invariants — chord table single source of truth; Charm-first; no goroutine without recover; no cookie value in any TUI string or test; every assertion mutation-checked | Global Constraints + every test's named mutant. The Charm-first invariant is honoured by *reusing* the wizard's picker rather than adding a `huh.NewSelect` — see the deviation note |

**2. Placeholder scan.** No "TBD", no "similar to Task N", no "add error handling". Every code step carries the actual code; every doc step carries the actual replacement prose; every test step carries the full test body. The four in-tree comment corrections in Task 1 Step 5 are quoted before-and-after.

**3. Type consistency.** `OpenCookieLogin(platform string)` is defined in Task 2 and called in Task 3 with a `string` from `ReloginPlatform()`. `closeCookieLogin()` is defined and used only within Task 2's file. `cookieOnly bool` is set in Task 2 and read in Task 3's test helper. `ReloginPlatform() string` is defined in Task 3 and read in Task 4. `cookieFocus` values are 0/1/2 everywhere. `mergeCookieFiles`' signature is unchanged, and `cookieKey` stays function-local.

**Where the code contradicted the spec draft — follow the code, the R's still bind:**

1. **No `huh.NewSelect` for the platform pick (R1), and there should not be one.** The draft qualifies it with "if the wizard does not already ask". It already asks: the simple-mode cookie step *is* the YouTube/Twitch picker (`setup_wizard.go:673-713` keys, `:1428-1464` view), hand-rolled on `cookieFocus`. A select would put a second picker in front of the first and be a new state machine, which R1's next clause forbids. The plan reuses the picker and preselects its row.

2. **`OpenCookieLogin` reuses neither `Open()` nor `armCookieTick` directly.** The countdown *is* reused — through `handleSimpleCookieKey`, which arms it on the operator's Enter, and `app_keys.go:185-193`, which drives the chain whenever `cookieActive`. Arming in `OpenCookieLogin` would be a seventh arming site counting down before a browser exists. `Open()` is the first-run entrance: it clears config values and channels and runs `OnCheckFFmpeg` (a shell-out on the update goroutine), none of which belongs on a configured install.

3. **The abandon path is `CancelSetup`, not `AbandonSetup`.** R3 asks the plan to pin which. The TUI has one cancel callback, `OnCancelAutoCookie`, bound to `s.autoCookieSvc.CancelSetup()` at `cmd/moombox/tui_wiring.go:527-528`; `AbandonSetup` is the dashboard's unload beacon and has no TUI binding. Correct for this surface, for Arc 3's stated reason: pressing Esc *consents* to the browser closing, a tab unloading does not. Pinned by `TestCookieLoginOverlayCancelsWhateverItLeavesBehind`.

4. **There is no "service not configured" error to catch from `StartSetup`, and the chord has no refusal sentence of its own.** R1 expected a refusal when `auto_enabled` is off or the host is headless, but `StartSetup` is *never* gated on `auto_enabled` (`data-and-storage.md:884`) and `cmd/moombox` wires the callback unconditionally. What it can return is `ErrServiceStopped` / `ErrSetupInProgress` / `ErrRefreshInProgress` / `ErrNoBrowserFound`, all rendered inline by the wizard on the operator's Enter. The one condition the chord can see — a nil callback — *deletes* the chord rather than making it speak: `processSecondKey` resolves `R L` against `buildMenuItems()` and an unregistered pair reports `Invalid Chord: R L`, the menu and help omit it, and `dispatchAction`'s nil-guard is defensive and unreachable from the keyboard. That is the `R F` model exactly; the spec's "existing status-line refusal path" was amended to say so (plan review, finding F2).

5. **Input line numbers have drifted.** At `390adb6` the remediation plan's V9 bullet is `:2010` (prompt and file map say `:2011`) and T3 is `:2012` (they say `:2015`). The file map's `status_bar.go:44-48` is the `CookieStatus` const block, not the render; the `Relogin` arms are at `:538` / `:579` as stated. Tasks cite the verified locations.

6. **T3's doc surface is wider than the draft says.** Twelve in-tree mentions across seven files state the old key as fact (`autocookies_merge.go:141`, `autocookies.go:2224`, `autocookies_profile.go:691`, `autocookies_profile_rollback_test.go:51`, the `PrefersNew` test comment, `cookie_import.go:24` and `:129`, and five in `cookie_import_test.go` — the review's grep found the last seven); Task 1 corrects all of them. The Arc 11 plan and spec docs also say "name+domain" and are left alone as dated records.

7. **T3 does not make the jar path-aware.** `CookieJar` keys by name within a platform and keeps last-wins for equal domains (`jar.go:321-331`), so two paths still load as one entry — the same entry the old key kept, in every ordering. The fix is at the *file* level. Task 1 says so in the code comment, the spec edit and its scope note, so the change is not later read as more than it is.

**Applied from the plan review (2026-09-02, `arc12a-plan-review.md`):**

8. **`viewSimpleCookies`' header replacement runs through the divider.** The replacement block re-emits the `Log in to platforms…` line and the divider, so the range it replaces is `:1383-1396`, not `:1383-1394` — the shorter range would have printed both lines twice (F1).
9. **The Esc branch has its own first-run premise.** A build where Esc always closes passes the cookie-only Esc subtest and strands a first-run operator; `TestCookieLoginOverlayDoesNotWalkIntoTheFirstRunFlow` gained the subtest that catches it (F9).
10. **The status-bar hint's ladder claim was wrong twice.** `TestStatusBarTiersNarrowMonotonically` runs on an `OK`/`OK` fixture and never renders the hint, and a suffix appended at *every* tier keeps the ladder monotone anyway; the new test now sweeps `metricTiers()` on a flagged bar itself, and the mutant list says what each assertion actually catches (F3).
11. **Cross-arc overlap on `autocookies.go`.** Task 1's one-word comment edit is anchored by content, since the A1-Linux arc edits that file on this branch (F12).
