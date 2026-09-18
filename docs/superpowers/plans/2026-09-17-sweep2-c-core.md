# Sweep 2 — Arc C (TUI + core: launcher / updater / config / logger / cmd wiring) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement every Arc C row of the sweep-2 fix chain — the failed-update recovery path, the TUI render cost, the config/save/threshold correctness set, the key and logger fixes, and the two doc passes — without changing any success path of the update mechanism and without making any update *rarer*.

**Architecture:** Twelve TDD tasks on one branch. Three of them (2–4) rework how the TUI spends a frame: the log viewport stops soft-wrapping 1,000 buffered lines on every render, and the task list / details panels gain render caches keyed on their own observable inputs plus the wall-clock second, so `View()` still runs per message (the protected ~60 Hz cadence is untouched) but most frames cost nothing. One new file, `internal/jobfilter/archive.go`, becomes the single archive-age predicate that the REST filter, the WS broadcast gate, the `cmd/moombox` list filter and the TUI all call, replacing four hand-copies (three of which truncated a fractional threshold to whole hours). Everything else is local, surgical edits.

**Tech Stack:** Go 1.27 (no CGo), bubbletea v2 / bubbles v2.2.1 / lipgloss v2, `charmbracelet/x/ansi`, modernc/sqlite, chi v5, vanilla-JS front end under `web/public/`, node `--test` for the JS twin.

**Spec:** `docs/superpowers/specs/2026-09-17-sweep2-fix-chain-design.md` (§0 owner decisions, §2 global constraints, §3 "Arc C", §4 concurrency, §5 rulings). Report rows: `reports/sweep-2026-09-15b.md` #7, #14, #15, #16, #34, #35, #36, #37, #38, #72, #73, #79, #82, #83, #85–#90, #99, #102 (CORE-18/19). Area reports: `reports/sweep-2026-09-15b/core.md`, `tooling.md` (TOOL-15, TOOL-18), `web.md` (WEB-8). Verifier: `reports/sweep-2026-09-15b/_verify-core.md` — **its corrections override the area report**.

---

## Global Constraints

Copied verbatim from spec §2. Every task's requirements implicitly include this section.

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build (`GOOS=linux GOARCH=amd64 go vet ./...` is part of every merge candidate).
- LF line endings in every file the chain touches. Every commit's LAST TWO LINES are exactly `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq` — the project's rule, which takes precedence over any attribution reminder in an implementer's own context whatever model name it shows.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils`.
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names the mutant that fails it (the reviewer verifies at least one by execution).
- Every JS-touching task gates `go test ./internal/web/routes/` AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes a Go symbol, or edits `docs/spec/*.md`/`SPEC.md`/`CLAUDE.md`, gates `go test ./internal/docs/` (the citation test requires the DECLARING file; after O-Z it also scans `SPEC.md` and `CLAUDE.md`).
- Protected behaviour stays: ~60 Hz progress pipeline and DB write cadence (make updates cheaper, never rarer); the DB layer untouched for perf; `monitors.probe_cooldown` default 0; connectivity-monitor probe design; the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie import stays unbounded (the timeout); RotateCookies rejected; DPAPI two-pass never; never kill processes by image name; never `rm -rf` under %TEMP%; no Shoelace bundling; extraction mirrors yt-dlp (android_vr retained; VISIONOS split-adaptive; homepage minting); update-path compatibility (old launcher + new child analysed before any launcher/updater/exit-code/swap-artifact change; success paths byte-identical); the setup wizard stays loopback-gated.
- Implementers commit with the pathspec ON the commit (`git add <files> && git commit -m … -- <same files>`); no stash/checkout/rebase/reset/amend; reviewers never edit (they reproduce in `git archive` scratch exports, copying the four gitignored embed blobs when a package needs them — youtube, chat, twitch, worker, engine, monitor, web all pull `internal/bgutils` transitively); scratch test files never named `*_linux_test.go`/`*_windows_test.go`; `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command; ONE controller-run `go test -count=1 ./...` at a time.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

### Arc-C-specific constraints

- **Owner decisions that are Arc C's** (spec §0): **O-M** Ctrl+C ALWAYS quits the TUI (the check is hoisted above overlay dispatch in `handleKey`). **O-V REJECTED** — Esc on the FFmpeg-not-found overlay KEEPS quitting ("the user needs ffmpeg for the muxing part of the process"); CORE-14 is dropped and `internal/tui/ffmpeg_check.go`'s `if key == keyEsc { return "quit" }` must NOT be touched by this arc. **O-W** `O C` gains a `clip.exe` fallback when `WT_SESSION` is unset, plus hedged wording. **O-X** ADD task-list paging PgUp/PgDn/Home/End (forward to the bubbles list; add the help row; update the Arc 6 help test). **O-Y** an explicit `-config` path is AUTHORITATIVE (no fall-through search); `SPEC.md:665` and the `Load` comment updated.
- **Spec §5 rulings for Arc C:** CORE-2's per-panel cache keys on the wall-clock second; CORE-1's `.failed` keep is failure-path-only and cross-version safe; **the exe is 90 MB** (not the ~30 MB the area report's Owner-choices line assumes).
- **Verifier corrections that override `core.md`** (`_verify-core.md`): CORE-1's `.update-pending` "marks that version skipped" clause does **not** apply to this strand — the restored binary dies inside `initServices` and never reaches the breadcrumb handling, so nothing is marked skipped; CORE-24's cited `adapters.go:316-324` does **not exist** (`cmd/moombox/adapters.go` is 183 lines) and that half of the evidence is dropped — the real unlocked reads are in `cmd/moombox/tui_wiring.go`; CORE-7's five apply sites are at `settings.go:755-756, :778-779, :791-794, :815` (the row cited only three); `operations.md:290`'s "the broken binary is removed (it is bit-identical to the published GitHub asset, so nothing is lost)" is the assumption CORE-1 disproves and must change with the code.
- **Line-number drift:** wave-1 arcs (E, Y, T) are implemented in parallel on other files, and wave 2's other arcs (M, W) touch `cmd/moombox/monitor_callbacks.go` and `cmd/moombox/services.go` in *different hunks*. Every line number in this plan was read from main at `8b09fe4a`. **If a cited line has drifted when this plan executes, locate the text by the quoted source — never by the number.** Every hunk below quotes the exact code it replaces for that purpose.
- **Branch/worktree:** `git worktree add -b sweep2-c-core .worktrees/sweep2-c-core main`, cut from main **AFTER wave 1 (Arcs E, Y, T) has merged**. Copy `internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}` and `internal/cipher/testdata/*.js` into the worktree; `cd web/tests && npm ci --no-audit --no-fund`.
- **Gate set for this arc** (spec §3): `go test ./internal/tui/ ./internal/jobfilter/ ./internal/config/ ./internal/updater/ ./internal/logger/ ./cmd/moombox/ ./internal/web/routes/ ./internal/docs/ ./internal/database/`, plus `node --test web/tests/*.test.mjs` for the one JS-touching task. No live gates.
- **Ownership fences.** Arc C owns `cmd/moombox/**` EXCEPT: the sidecar block in `services.go` (Arc Y), the resolver hunk in `services.go` (Arc M), the `:93-104` hunk of `monitor_callbacks.go` (Arc M) and the `:1563` broadcast hunk of `monitor_callbacks.go` (Arc W). Arc C owns the `:1557` archive-gate hunk of `monitor_callbacks.go` and `internal/web/routes/jobs.go:86-134` only. Four surfaces are touched outside the spec's literal Arc C list and are called out where they appear: `internal/database/migrations.go` (the refusal message CORE-1 requires), `internal/database/database.go` (one new map field beside `jobLogs`, for CORE-12), `internal/web/routes/config_routes.go` (two validator-range hunks, for CORE-21), and one NEW file under `web/tests/` (the WEB-8 JS parity pin). None overlaps another arc's cited hunks; merge main before the merge candidate as spec §4 already requires.

---

### Task 1: The failed-update artifact survives, and a startup error is never a failed update

Rows: **#7 (CORE-1)**, **#89 (CORE-23)**, **#83 (CORE-13)**.

**Files:**
- Modify: `cmd/moombox/launcher.go` (`attemptAutoRollback` ~:355-387, `writeAutoRollbackMarker` ~:389-408, the exit-code `switch` ~:210-300)
- Modify: `cmd/moombox/main.go` (`exitCodeRestart` const block ~:30-32; the `initServices` failure exit ~:212-215; the headless web-bind failure exit ~:376-382)
- Modify: `internal/updater/updater.go` (`CleanupOldBinary` ~:498-541)
- Modify: `internal/database/migrations.go` (downgrade-guard message ~:245-253)
- Test: `cmd/moombox/launcher_rollback_test.go` (exists — append), `internal/updater/updater_test.go` (exists — append)

**Interfaces:**
- Produces: `const failedBinarySuffix = ".failed"` and `classifyPostUpdateExit(code int) postUpdateVerdict` in `cmd/moombox/launcher.go`; `const exitCodeStartupError = 3` in `cmd/moombox/main.go`. No later task consumes them.

#### Update-path compatibility analysis (required by spec §2; do not skip)

The protected ruling is *"never break existing users' update path; old launcher + new child must be analysed; success paths stay byte-identical."* Three changes are proposed. The reviewer re-verifies rows (a), (c) and (e) by reading `cmd/moombox/launcher.go` on the branch.

**(a) `attemptAutoRollback` renames instead of removing — failure path only.** `os.Remove(exePath)` appears exactly once in the tree, inside `attemptAutoRollback`, reachable from exactly two places: the `case wasFirstAfterUpdate && code != 0 && ranFor < postUpdateFailureWindow:` arm of the exit switch, and the `cmd.Start()` failure branch above the loop. Every SUCCESS path exits earlier and never enters either: exit 42 (`case code == exitCodeRestart`), exit 0 (`case code == 0`), a launcher-initiated stop (`case terminating.Load()`), 130/143. A healthy update therefore never observes this change — the on-disk artifacts (`.old`, `~`, `rollbackArtifactPath`, `CleanupOldBinary`, the marker sweep, the `.update-pending` breadcrumb) are identical byte for byte.

**(b) Old launcher + new child.** The rollback is performed by the **launcher**, which after an update is still the *previous* version's image (`handleUpdateRestart` renames `.old` → `~` and the launcher keeps running from `~`). An OLD launcher paired with a NEW child keeps today's delete behaviour exactly; the fix is strictly forward-only and cannot regress an install that has not yet run a fixed launcher. The `.failed` name is new and no existing binary looks for it, so an old child never trips over it either.

**(c) New launcher + old child.** "Swept by `CleanupOldBinary` at the milestone" only holds if the binary that boots after the rollback knows the new suffix. In the rollback flow that binary is the same version as the launcher (the launcher rolls back to its own previous image and respawns it), so a fixed launcher implies a fixed sweeper. It is also self-consistent with the strand this row is about: when the DB is the blocker the restored child dies inside `initServices` and never reaches `upd.CleanupOldBinary()`, so `.failed` **survives precisely in the case where the refusal message needs to point at it**; when the restored child boots fine, `.failed` is swept and nothing lingers. Cost: one ~90 MB file until the next healthy boot (`moombox.exe` = 90,488,320 bytes — spec §5).

**(d) A pre-existing `.failed`.** `os.Rename` is `MoveFileEx(MOVEFILE_REPLACE_EXISTING)` on Windows and replaces on POSIX, so a second failed update overwrites the first artifact rather than failing. The retry loop is kept because a rename needs the same DELETE access to the source that a remove needed — an AV scanner still holding the freshly-downloaded binary denies it, and brief retries ride it out. A rename that never succeeds returns `false`, which is today's `preserveUpdateRollback` fallback, unchanged.

**(e) The new exit code.** `exitCodeStartupError = 3` is used only by the CHILD, for two deterministic startup failures that today exit 1. An OLD launcher has no case for 3: it falls into `wasFirstAfterUpdate && code != 0 && …` (today's auto-rollback) or the fail-fast arm — exactly today's behaviour. A NEW launcher gains one decision ahead of the rollback that preserves the artifact with instructions instead of rolling back. Nothing outside the launcher branches on the child's exit code, and both sites were already non-zero, so a service supervisor sees "non-zero" either way. `waitForKeypress()` stays: with a dedicated code the 2-minute window's timing no longer decides the classification, which is the whole point of CORE-23.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/moombox/launcher_rollback_test.go` (add `strings` to its imports if absent):

```go
// A failed first-post-update boot must KEEP the broken binary as
// <exe>.failed instead of deleting it: the DB downgrade guard the restored
// (older) binary then hits tells the operator to restore the newer binary,
// and before this the rollback had just deleted the only copy (CORE-1).
//
// Mutant: restoring `os.Remove(exePath)` in attemptAutoRollback — the
// .failed file is absent and the marker no longer names it.
func TestAutoRollbackKeepsTheFailedBinary(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox-test.exe")
	if err := os.WriteFile(exePath, []byte("BROKEN"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollbackArtifactPath(exePath), []byte("PREVIOUS"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !attemptAutoRollback(exePath, 1) {
		t.Fatal("attemptAutoRollback must succeed when the artifact exists")
	}

	restored, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("the previous binary must be back at the plain name: %v", err)
	}
	if string(restored) != "PREVIOUS" {
		t.Errorf("plain name holds %q, want the restored previous binary", restored)
	}
	failed, err := os.ReadFile(exePath + failedBinarySuffix)
	if err != nil {
		t.Fatalf("the broken binary must survive as %s: %v", failedBinarySuffix, err)
	}
	if string(failed) != "BROKEN" {
		t.Errorf("%s holds %q, want the broken binary", failedBinarySuffix, failed)
	}
	marker, err := os.ReadFile(exePath + ".update-failed")
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if !strings.Contains(string(marker), exePath+failedBinarySuffix) {
		t.Errorf("the marker must name the kept binary by path, got:\n%s", marker)
	}
}

// A second failed update replaces the first .failed artifact rather than
// failing the rollback — os.Rename replaces an existing destination on both
// platforms.
//
// Mutant: guarding the rename with an os.Stat "already exists" bail-out —
// the artifact still holds BROKEN-1.
func TestAutoRollbackReplacesAnOlderFailedArtifact(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox-test.exe")
	for _, f := range []struct{ path, body string }{
		{exePath, "BROKEN-2"},
		{exePath + failedBinarySuffix, "BROKEN-1"},
		{rollbackArtifactPath(exePath), "PREVIOUS"},
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !attemptAutoRollback(exePath, 1) {
		t.Fatal("attemptAutoRollback must succeed")
	}
	failed, err := os.ReadFile(exePath + failedBinarySuffix)
	if err != nil {
		t.Fatalf("read %s: %v", failedBinarySuffix, err)
	}
	if string(failed) != "BROKEN-2" {
		t.Errorf("%s holds %q, want the newest broken binary", failedBinarySuffix, failed)
	}
}
```

Append to `internal/updater/updater_test.go`:

```go
// CleanupOldBinary sweeps the artifacts Moombox itself writes, plus the
// binary a failed update left behind — and NOT <exe>.sig, which Moombox
// never writes (VerifyCurrentSignature uses os.CreateTemp, ApplyUpdate uses
// ".new.sig") but which is exactly the published release-asset name a manual
// verifier leaves beside the exe (CORE-13).
//
// Mutant: keeping ".sig" in the suffix list — the .sig assertion fails.
// Mutant: dropping ".failed" from the list — the .failed assertion fails.
func TestCleanupOldBinarySweepsFailedAndSparesSig(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox-test.exe")
	if err := os.WriteFile(exePath, []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".old", ".new", ".new.sig", ".sig", ".failed"} {
		if err := os.WriteFile(exePath+suffix, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	u := &Updater{exePath: exePath, currentVersion: "9.9.9", logger: cleanupTestLogger{}}
	u.CleanupOldBinary()

	for _, suffix := range []string{".old", ".new", ".new.sig", ".failed"} {
		if _, err := os.Stat(exePath + suffix); err == nil {
			t.Errorf("%s must be swept", suffix)
		}
	}
	if _, err := os.Stat(exePath + ".sig"); err != nil {
		t.Errorf(".sig is a manual verifier's artifact and must survive: %v", err)
	}
}

// cleanupTestLogger satisfies the Updater's anonymous logger interface for
// the sweep test, which only asserts file-system effects.
type cleanupTestLogger struct{}

func (cleanupTestLogger) Debug(string, ...any) {}
func (cleanupTestLogger) Info(string, ...any)  {}
func (cleanupTestLogger) Warn(string, ...any)  {}
func (cleanupTestLogger) Error(string, ...any) {}
```

If `internal/updater/updater_test.go` already declares an equivalent no-op logger stub, use that one and drop `cleanupTestLogger` (a duplicate would be dead weight). Likewise check the `Updater` struct's real field names (`exePath`, `currentVersion`, `logger`) before writing the literal — construct it through whatever constructor the file's existing tests use if direct field access is not how they do it.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/ -run TestAutoRollback -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/updater/ -run TestCleanupOldBinarySweeps -v
```
Expected: compile failure — `undefined: failedBinarySuffix`.

- [ ] **Step 3: Keep the failed binary**

In `cmd/moombox/launcher.go`, add the constant directly above `attemptAutoRollback`:

```go
// failedBinarySuffix names the binary a failed first-post-update boot leaves
// behind. The rollback used to DELETE it on the grounds that it is
// bit-identical to the published GitHub asset — but when the failure is a
// refused DB downgrade, the older binary the rollback restores then prints
// "restore the newer binary … if still present" about a file the rollback
// had just removed, and the install is down until a manual re-download
// (CORE-1). Keeping it costs ~90 MB until the next healthy boot sweeps it
// (internal/updater.CleanupOldBinary).
const failedBinarySuffix = ".failed"
```

Replace the remove loop inside `attemptAutoRollback`. The text to replace:

```go
	var rmErr error
	for range 3 {
		if rmErr = os.Remove(exePath); rmErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if rmErr != nil {
		fmt.Fprintf(os.Stderr, "auto-rollback: could not remove failed binary: %v\n", rmErr)
		return false
	}
```

The replacement:

```go
	failedPath := exePath + failedBinarySuffix
	var mvErr error
	for range 3 {
		if mvErr = os.Rename(exePath, failedPath); mvErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if mvErr != nil {
		fmt.Fprintf(os.Stderr, "auto-rollback: could not move failed binary aside: %v\n", mvErr)
		return false
	}
```

In the comment block immediately above it, change the opening sentence `// Remove the broken binary first, then rename over the empty name. NOT` to `// Move the broken binary aside first, then rename over the freed name. NOT`, and change `// hit on the remove instead makes it RETRYABLE` to `// hit on moving it aside instead makes it RETRYABLE (a rename needs the same` / `// DELETE access to the source that a remove does)`. In the function's godoc, replace `the broken binary at exePath is removed (it is\n// bit-identical to the published GitHub asset, so nothing diagnostic is\n// lost)` with `the broken binary at exePath is KEPT, renamed to\n// <exe>.failed so the DB downgrade guard's "restore the newer binary"\n// advice names a file that exists`.

Rewrite `writeAutoRollbackMarker` so the marker names the artifact:

```go
func writeAutoRollbackMarker(exePath string, exitCode int) {
	msg := fmt.Sprintf(
		"Moombox: the first launch after a self-update failed (exit code %d) at %s.\n"+
			"Moombox AUTOMATICALLY ROLLED BACK to the previous binary and restarted it.\n"+
			"The failed release was KEPT at:\n  %s\n"+
			"(If the restored binary refuses to start — a database the newer version\n"+
			"already migrated is one such case — rename that file back over %s.)\n"+
			"Automatic update checks will skip the failed version where supported; use a\n"+
			"manual \"Check for updates\" to retry it deliberately.\n"+
			"Delete this marker file once acknowledged.\n",
		exitCode, time.Now().Format(time.RFC3339), exePath+failedBinarySuffix, exePath)
	markerPath := exePath + ".update-failed"
	if err := os.WriteFile(markerPath, []byte(msg), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write %s: %v\n", markerPath, err)
	}
	fmt.Fprint(os.Stderr, "\n"+msg)
}
```

- [ ] **Step 4: Sweep `.failed`, spare `.sig`**

In `internal/updater/updater.go`, replace

```go
	suffixes := []string{".old", ".new", ".new.sig", ".sig"}
```
with
```go
	suffixes := []string{".old", ".new", ".new.sig", ".failed"}
```

and replace the first paragraph of the `CleanupOldBinary` godoc with:

```go
// CleanupOldBinary removes stale files left over from previous updates:
// .old (previous binary), .new (interrupted download), .new.sig (interrupted
// verification), and .failed (the binary an automatic rollback moved aside —
// see attemptAutoRollback in cmd/moombox/launcher.go; it is kept only until
// a boot proves healthy, which is exactly this call).
//
// <exe>.sig is deliberately NOT swept. Moombox never writes it
// (VerifyCurrentSignature downloads to an os.CreateTemp file, ApplyUpdate
// writes ".new.sig"), but it is exactly the published release-asset name in
// releaseAssetMap that an operator verifying a manual download leaves beside
// the binary — sweeping it destroyed their artifact on every boot (CORE-13).
```

- [ ] **Step 5: Run the two tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/ -run TestAutoRollback -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/updater/ -run TestCleanupOldBinarySweeps -v
```
Expected: PASS.

- [ ] **Step 6: Point the DB refusal at the artifact**

In `internal/database/migrations.go`, replace the downgrade-guard string

```go
			"database schema v%d is newer than this binary supports (v%d) — you appear to have downgraded after an update migrated the database; restore the newer binary (moombox.exe.old from the update swap, if still present) or upgrade again",
```

with

```go
			"database schema v%d is newer than this binary supports (v%d) — you appear to have downgraded after an update migrated the database; restore the newer binary (an automatic rollback keeps it beside this executable as moombox.exe.failed and names the exact path in the moombox.exe.update-failed marker; a manual downgrade leaves moombox.exe.old from the update swap) or upgrade again",
```

Run `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/database/` — if an existing test asserts the old substring, update it to the new wording and note it in the commit body.

- [ ] **Step 7: Write the failing test for the non-rollback exit code**

Append to `cmd/moombox/launcher_rollback_test.go`:

```go
// A deterministic startup error is the operator's environment (bad config,
// bad flags, a refused migration), not proof the new binary is broken — so
// it must never auto-roll-back and never mark the release skipped. Before
// this, whether such an exit counted as "the update failed" depended on
// whether the operator pressed Enter at waitForKeypress inside the 2-minute
// window (CORE-23). classifyPostUpdateExit is that decision, extracted so it
// can be asserted without spawning a launcher.
//
// Mutant: returning postUpdateRollback for exitCodeStartupError — the
// "startup error" row reports rollback.
func TestStartupErrorIsNeverAFailedUpdate(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want postUpdateVerdict
	}{
		{"startup error", exitCodeStartupError, postUpdatePreserve},
		{"panic-shaped crash", 2, postUpdateRollback},
		{"generic failure", 1, postUpdateRollback},
	} {
		if got := classifyPostUpdateExit(tc.code); got != tc.want {
			t.Errorf("%s: classifyPostUpdateExit(%d) = %v, want %v", tc.name, tc.code, got, tc.want)
		}
	}
}
```

- [ ] **Step 8: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/ -run TestStartupErrorIsNever -v
```
Expected: compile failure — `undefined: exitCodeStartupError`, `undefined: classifyPostUpdateExit`.

- [ ] **Step 9: Add the exit code and the classifier**

In `cmd/moombox/main.go`, extend the constant block:

```go
// exitCodeRestart is the exit code the child uses to signal the launcher
// that it should respawn. Used for both config restarts and update restarts.
const exitCodeRestart = 42

// exitCodeStartupError is the exit code the child uses for a DETERMINISTIC
// startup failure — a config the process cannot load, a database it refuses,
// a web bind that cannot succeed in headless mode. The launcher treats it as
// "this install's environment is wrong", never as "the update is broken": no
// automatic rollback, no skipped version, the rollback artifact preserved
// with instructions. Before this, the classification depended on whether the
// operator pressed Enter at waitForKeypress inside postUpdateFailureWindow
// (CORE-23). Launchers that predate this constant have no case for it and
// fall through to exactly today's behaviour, so the change is forward-only.
const exitCodeStartupError = 3
```

Change both deterministic startup exits from `os.Exit(1)` to `os.Exit(exitCodeStartupError)`:

1. in `run()`:
```go
	if err := s.initServices(logLevelOverride); err != nil {
		fmt.Fprintf(os.Stderr, "Startup error: %v\n", err)
		waitForKeypress()
		os.Exit(exitCodeStartupError)
	}
```
2. in the headless web-bind failure branch:
```go
				fmt.Fprintln(os.Stderr, "Web dashboard is unavailable.")
				waitForKeypress()
				os.Exit(exitCodeStartupError)
```

In `cmd/moombox/launcher.go`, add above `attemptAutoRollback`:

```go
// postUpdateVerdict is what the launcher does with a non-zero exit from the
// first boot of a freshly-applied update.
type postUpdateVerdict int

const (
	// postUpdateRollback restores the previous binary and respawns it.
	postUpdateRollback postUpdateVerdict = iota
	// postUpdatePreserve keeps the rollback artifact on disk with written
	// instructions and propagates the exit code without respawning.
	postUpdatePreserve
)

// classifyPostUpdateExit decides how a failed first-post-update boot is
// treated. Only exitCodeStartupError is spared the rollback: it names a
// deterministic environment failure the new binary is not responsible for,
// and rolling back would hide the real cause behind a version downgrade
// while marking a perfectly good release skipped (CORE-23).
func classifyPostUpdateExit(code int) postUpdateVerdict {
	if code == exitCodeStartupError {
		return postUpdatePreserve
	}
	return postUpdateRollback
}
```

Replace the arm body. The text to replace:

```go
			if attemptAutoRollback(exePath, code) {
				crashRespawnCode = 0
				consecutiveCrashes = 0
				continue
			}
			preserveUpdateRollback(exePath, code)
			os.Exit(code)
```

The replacement:

```go
			if classifyPostUpdateExit(code) == postUpdateRollback && attemptAutoRollback(exePath, code) {
				crashRespawnCode = 0
				consecutiveCrashes = 0
				continue
			}
			preserveUpdateRollback(exePath, code)
			os.Exit(code)
```

Append to that arm's comment block:

```go
			// A deterministic startup error (exitCodeStartupError) skips the
			// rollback entirely: the environment, not the binary, is what
			// failed. The artifact is still PRESERVED with instructions —
			// this branch does not call deferDeleteOldLauncher — so a manual
			// rollback stays one rename away, and because the next boot runs
			// the SAME version the .update-pending breadcrumb names,
			// shouldSkipPendingVersion is false and the release is not
			// marked skipped.
```

- [ ] **Step 10: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/ -run 'TestAutoRollback|TestStartupErrorIsNever' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/ ./internal/updater/ ./internal/database/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./cmd/moombox/ ./internal/updater/ ./internal/database/
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./cmd/moombox/
```
Expected: all PASS / clean.

- [ ] **Step 11: Commit**

```bash
git add cmd/moombox/launcher.go cmd/moombox/main.go cmd/moombox/launcher_rollback_test.go internal/updater/updater.go internal/updater/updater_test.go internal/database/migrations.go
git commit -m "fix(launcher,updater,database): keep a failed update as .failed; a startup error is never a failed update; spare .sig

CORE-1: attemptAutoRollback renames the broken binary to <exe>.failed instead
of deleting it, names that path in the .update-failed marker, and the schema
downgrade guard points at it - the restored older binary's advice now names a
file that exists. CleanupOldBinary sweeps it at the first-healthy-boot
milestone.

CORE-23: exitCodeStartupError (3) for the two deterministic startup exits;
classifyPostUpdateExit spares it the rollback and preserves the artifact with
instructions instead. Old launchers have no case for it and behave as today.

CORE-13: .sig dropped from the cleanup list - Moombox never writes it and it
is the published signature asset a manual verifier leaves behind.

Success paths are byte-identical: os.Remove(exePath) only ever ran on the
failure arm. Report rows #7, #89, #83.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- cmd/moombox/launcher.go cmd/moombox/main.go cmd/moombox/launcher_rollback_test.go internal/updater/updater.go internal/updater/updater_test.go internal/database/migrations.go
```

---
### Task 2: The log panel stops walking 1,000 buffered lines on every render

Row: **#34 (CORE-2)**, part 1 of 3.

Measured on main (`_verify-core.md`, 1,000 jobs, 200×60, log at the `maxLogLines = 1000` cap): a whole frame is 3.93 ms / 21,716 allocs and **`VFLogViewerAlone` alone is 2.35 ms / 6,192 allocs — 60 % of it**. The mechanism is `bubbles/v2@v2.2.1/viewport.calculateLine`: under `SoftWrap` it walks **every** buffered line calling `ansi.StringWidth` on each, and `visibleLines`/`maxYOffset`/`ScrollPercent` each call it. With `SoftWrap = false` that same function is `total = len(m.lines); ridx = min(yoffset, len(m.lines))` — O(1).

So the fix is: hard-wrap each line ONCE, when it arrives (or when the panel width changes), and let the viewport hold already-wrapped content. That moves an O(n) width pass from *per render* (≈120/s) to *per insertion* (≈10/s), where `viewport.SetContentLines` already pays an O(n) `maxLineWidth` pass anyway. A second, independent win lands in the same file: the panel's rendered string is cached, so a frame that carries no log change re-renders nothing.

**Files:**
- Modify: `internal/tui/log_viewer.go`
- Test: `internal/tui/log_viewer_test.go` (exists — append)

**Interfaces:**
- Consumes: nothing.
- Produces (all unexported, package `tui`): `func wrapLogLine(line string, width int) []string`; fields `LogViewerModel.rawCount int`, `.wrapWidth int`, `.renderCache string`, `.cacheKey logViewerKey`; method `func (m *LogViewerModel) invalidate()`. Task 4 consumes `wrapLogLine` indirectly only (through `View()`); no other task references these.

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/log_viewer_test.go` (imports: `strings`, `testing`, and `github.com/charmbracelet/x/ansi`):

```go
// The viewport must never soft-wrap: SoftWrap makes calculateLine walk every
// buffered line on EVERY render (2.35 ms at the 1,000-line cap, 60% of a
// whole frame), so lines are hard-wrapped once at insertion instead
// (CORE-2). Every display line is therefore at or under the content width.
//
// Mutant: restoring vp.SoftWrap = true and feeding raw lines — the long line
// stays one 400-column entry and the width assertion fails.
func TestLogLinesAreWrappedAtInsertion(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(42, 12) // content width 40
	m.AddLine("2026-09-17 12:00:00 INFO " + strings.Repeat("x", 400))

	if m.viewport.SoftWrap {
		t.Error("the viewport must not soft-wrap; lines are pre-wrapped at insertion")
	}
	if len(m.filtered) < 2 {
		t.Fatalf("a 425-column line at content width 40 must wrap into several display lines, got %d", len(m.filtered))
	}
	for i, line := range m.filtered {
		if w := ansi.StringWidth(line); w > 40 {
			t.Errorf("display line %d is %d columns wide, want <= 40", i, w)
		}
	}
	if len(m.filtered) != len(m.filteredLevels) {
		t.Fatalf("levels must stay in lock-step with display lines: %d vs %d", len(m.filtered), len(m.filteredLevels))
	}
	for i, lvl := range m.filteredLevels {
		if lvl != "INFO" {
			t.Errorf("continuation line %d carries level %q, want INFO from its source line", i, lvl)
		}
	}
}

// The header counts LOG LINES, not display lines. Wrapping must not inflate
// it — "Logs (1)" for one wrapped line, never "Logs (11)".
//
// Mutant: rendering len(m.filtered) in the header again.
func TestLogHeaderCountsSourceLinesNotWrappedOnes(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(42, 12)
	m.AddLine("2026-09-17 12:00:00 INFO " + strings.Repeat("y", 400))

	if got := stripANSI(m.View()); !strings.Contains(got, "Logs (1)") {
		t.Errorf("header must read \"Logs (1)\" for one wrapped line, got:\n%s", got)
	}
}

// A width change re-wraps the buffer: without it the panel keeps rows wrapped
// to the OLD width, which Tab (each panel gets a different share of the
// terminal) changes on every press.
//
// Mutant: dropping the width comparison in SetSize so rebuildFiltered is not
// re-run — the display lines stay 40 columns wide after the shrink.
func TestLogSetSizeRewrapsTheBuffer(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(42, 12)
	m.AddLine("2026-09-17 12:00:00 INFO " + strings.Repeat("z", 400))
	wide := len(m.filtered)

	m.SetSize(22, 12) // content width 20
	if len(m.filtered) <= wide {
		t.Fatalf("halving the width must produce more display lines: %d -> %d", wide, len(m.filtered))
	}
	for i, line := range m.filtered {
		if w := ansi.StringWidth(line); w > 20 {
			t.Errorf("display line %d is %d columns wide after the resize, want <= 20", i, w)
		}
	}
}

// The rendered panel is cached: a frame that changes nothing about the log
// panel must not re-render it. The probe writes straight into the viewport,
// bypassing updateViewportContent (the only thing that bumps contentSeq) and
// keeping the line COUNT identical, so nothing in the key moves — exactly
// the "message that changed nothing here" case bubbletea delivers ~120 times
// a second.
//
// Mutant: deleting the cache lookup at the top of View() — the injected text
// appears in the second frame.
func TestLogViewIsCachedBetweenIdenticalFrames(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(60, 12)
	m.AddLine("2026-09-17 12:00:00 INFO original")
	first := m.View()

	inject := make([]string, len(m.filtered))
	for i := range inject {
		inject[i] = "MUTATED"
	}
	m.viewport.SetContentLines(inject)

	if second := m.View(); second != first {
		t.Errorf("an unchanged log panel must return the cached frame; got a re-render:\n%s", stripANSI(second))
	}

	// Not a vacuous assertion: the injection IS visible once the cache is
	// dropped, so the previous check proved the cache and not an inert probe.
	m.invalidate()
	if got := stripANSI(m.View()); !strings.Contains(got, "MUTATED") {
		t.Fatalf("the probe must be visible without the cache, got:\n%s", got)
	}

	// Any real content change invalidates it through contentSeq.
	m.AddLine("2026-09-17 12:00:01 INFO second")
	if got := stripANSI(m.View()); !strings.Contains(got, "second") {
		t.Errorf("a new line must invalidate the cache, got:\n%s", got)
	}
}
```

`stripANSI` already exists in the package (used by `frame_counts_test.go`); do not redefine it.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestLogLinesAreWrapped|TestLogHeaderCounts|TestLogSetSizeRewraps|TestLogViewIsCached' -v
```
Expected: `TestLogLinesAreWrappedAtInsertion` fails on `m.viewport.SoftWrap` being true; the others fail on the wrapped/cached behaviour being absent.

- [ ] **Step 3: Pre-wrap at insertion**

In `internal/tui/log_viewer.go`:

1. Add the import `"github.com/charmbracelet/x/ansi"`.

2. In `NewLogViewerModel`, replace `vp.SoftWrap = true` with:

```go
	// SoftWrap = false is load-bearing for the frame budget. Under SoftWrap
	// the viewport's calculateLine walks EVERY buffered line calling
	// ansi.StringWidth on each, on every render, and visibleLines /
	// maxYOffset / ScrollPercent each call it — 2.35 ms and 6,192 allocs at
	// the 1,000-line cap, 60% of a whole TUI frame (CORE-2). With it off the
	// same function is a length and a min(). Lines are hard-wrapped once at
	// insertion instead (rebuildFiltered / wrapLogLine), which moves that
	// width pass to the ~10/s insertion path where SetContentLines already
	// pays an O(n) maxLineWidth pass.
	vp.SoftWrap = false
```

3. Add the fields to `LogViewerModel`, beside `filteredLevels`:

```go
	// rawCount is how many SOURCE log lines survive the level filter. The
	// header reports this, not len(filtered) — filtered holds WRAPPED
	// display lines, so a single long line would otherwise read as ten.
	rawCount int
	// wrapWidth is the content width filtered was wrapped to. SetSize
	// re-wraps when it moves (Tab changes every panel's share).
	wrapWidth int

	// renderCache / cacheKey memoise View(). bubbletea calls View() after
	// EVERY message (~120/s with one active download: 60 progress updates
	// plus the 60 Hz tick), and the vast majority of those carry no log
	// change at all. The key is every input View() reads; the cache is
	// dropped outright while the search box is open, because the textinput
	// renders a blinking cursor whose state is not in the key.
	renderCache string
	cacheKey    logViewerKey
```

4. Add the key type and the invalidator, above `View()`:

```go
// logViewerKey is every input LogViewerModel.View() reads. It is a
// comparable struct so a cache hit is one ==. Content changes are covered by
// contentSeq, which updateViewportContent (the single funnel for
// filtered/filteredLevels) bumps; everything else here is read straight off
// the model or the viewport, so no mutator can forget to invalidate it.
type logViewerKey struct {
	contentSeq  uint64
	width       int
	height      int
	focused     bool
	autoScroll  bool
	level       LogLevel
	searchQuery string
	matchCount  int
	rawCount    int
	displayed   int
	yOffset     int
	vpHeight    int
}

func (m *LogViewerModel) key() logViewerKey {
	return logViewerKey{
		contentSeq:  m.contentSeq,
		width:       m.width,
		height:      m.height,
		focused:     m.focused,
		autoScroll:  m.autoScroll,
		level:       m.level,
		searchQuery: m.searchQuery,
		matchCount:  m.matchCount,
		rawCount:    m.rawCount,
		displayed:   len(m.filtered),
		yOffset:     m.viewport.YOffset(),
		vpHeight:    m.viewport.Height(),
	}
}

// invalidate drops the memoised frame. Called where the rendered output
// changes without any keyed field moving — the only such case is the
// viewport's own highlight cursor (HighlightNext/HighlightPrevious), which
// bubbles keeps private.
func (m *LogViewerModel) invalidate() {
	m.renderCache = ""
}
```

5. Add `contentSeq uint64` to the struct (beside `renderCache`) and bump it at the single content funnel. Replace `updateViewportContent`'s opening:

```go
func (m *LogViewerModel) updateViewportContent() {
	// One funnel for every content change — the render cache keys on this.
	m.contentSeq++
	if len(m.filtered) == 0 {
```

6. Add the wrap helper directly above `rebuildFiltered`:

```go
// wrapLogLine hard-wraps one plain-text log line to width columns, using the
// same character-wrap rule the viewport's own softWrap applies
// (ansi.Cut(line, idx, idx+width)) so nothing about the rendered result
// changes — only WHEN the work is done. Lines that already fit are returned
// as a one-element slice sharing the original string.
func wrapLogLine(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}
	total := ansi.StringWidth(line)
	if total <= width {
		return []string{line}
	}
	out := make([]string, 0, (total+width-1)/width)
	for idx := 0; idx < total; idx += width {
		out = append(out, ansi.Cut(line, idx, idx+width))
	}
	return out
}
```

7. Replace the whole of `rebuildFiltered` with:

```go
// rebuildFiltered rebuilds the DISPLAY buffer: level-filter the raw lines,
// then hard-wrap each survivor to the panel's content width. filtered and
// filteredLevels hold WRAPPED lines (a source line contributes one entry per
// display row, repeating its level so styleLogLine still colours
// continuations); rawCount holds the SOURCE count the header reports.
func (m *LogViewerModel) rebuildFiltered() {
	m.filtered = m.filtered[:0]
	m.filteredLevels = m.filteredLevels[:0]
	m.rawCount = 0
	m.wrapWidth = m.contentWidth()
	for i, line := range m.lines {
		if m.level != LogLevelAll && !m.matchLevel(m.levels[i]) {
			continue
		}
		m.rawCount++
		for _, seg := range wrapLogLine(line, m.wrapWidth) {
			m.filtered = append(m.filtered, seg)
			m.filteredLevels = append(m.filteredLevels, m.levels[i])
		}
	}
	m.updateViewportContent()
}

// contentWidth is the viewport's usable width — the same value SetSize hands
// the viewport, and therefore the width wrapLogLine must target.
func (m *LogViewerModel) contentWidth() int {
	return max(m.width-2, 1)
}
```

8. Make `SetSize` re-wrap on a width change. Replace:

```go
func (m *LogViewerModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.SetWidth(w - 2) // account for borders
	m.resizeViewport()
	m.updateViewportContent()
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}
```

with:

```go
func (m *LogViewerModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.SetWidth(w - 2) // account for borders
	m.resizeViewport()
	// A width change invalidates the WRAP, not just the layout: filtered
	// holds lines already cut to the old content width, so a re-render alone
	// would leave them short (or overflowing, after a widen). Tab gives each
	// panel a different share of the terminal, so this runs on every press.
	if m.wrapWidth != m.contentWidth() {
		m.rebuildFiltered()
	} else {
		m.updateViewportContent()
	}
	if m.autoScroll {
		m.viewport.GotoBottom()
	}
}
```

9. In `Clear()`, reset the new counters — replace `m.lines = nil` / `m.levels = nil` with:

```go
	m.lines = nil
	m.levels = nil
	m.rawCount = 0
```
(`rebuildFiltered` further down already recomputes `rawCount`, but resetting here keeps the field truthful for any read between the two statements.)

10. In `View()`, report `m.rawCount` in the header. Replace:

```go
	header := titleStyle.Render(fmt.Sprintf("Logs (%d)", len(m.filtered)))
```
with:
```go
	// rawCount, not len(m.filtered): filtered holds wrapped DISPLAY lines, so
	// one long line would otherwise be counted as several.
	header := titleStyle.Render(fmt.Sprintf("Logs (%d)", m.rawCount))
```

The scroll-percentage suffix keeps `len(m.filtered)` — that comparison is about display rows and is correct as written.

- [ ] **Step 4: Add the render cache to `View()`**

At the very top of `LogViewerModel.View()`:

```go
func (m *LogViewerModel) View() string {
	// The search box renders a blinking textinput cursor whose state is not
	// in the key, so while it is open the panel is rendered every frame.
	if !m.searching {
		if k := m.key(); m.renderCache != "" && k == m.cacheKey {
			return m.renderCache
		}
	}
	contentW := max(m.width-2, 1)
```

and at the bottom, replace:

```go
	return style.Width(m.width).Height(m.height).Render(content)
```
with:
```go
	out := style.Width(m.width).Height(m.height).Render(content)
	if !m.searching {
		m.renderCache = out
		m.cacheKey = m.key()
	} else {
		m.renderCache = ""
	}
	return out
}
```

In `HandleSearchKey`, call `m.invalidate()` in the three branches that move the viewport's highlight cursor without changing any keyed field — immediately after `m.viewport.HighlightNext()` in the Enter branch and in the `case "n":` branch, and after `m.viewport.HighlightPrevious()` in the `case "N":` branch:

```go
		case "n":
			m.viewport.HighlightNext()
			m.invalidate() // the selected-highlight index is bubbles-private
			m.setAutoScroll(m.viewport.AtBottom())
			return nil, true
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestLogLinesAreWrapped|TestLogHeaderCounts|TestLogSetSizeRewraps|TestLogViewIsCached' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/
```
Expected: PASS, and the whole `internal/tui` suite green. If an existing `log_viewer_test.go` assertion counted `len(m.filtered)` as a LOG-line count, update it to `m.rawCount` and say so in the commit body — that is the one intended semantic change.

**Known, accepted behaviour change (record it in the commit body):** a search term that straddles a wrap boundary no longer matches. `applySearchHighlights` runs its regex over `viewport.GetContent()`, which is now the joined *wrapped* lines, so a query split across a cut is invisible to it. Log lines are ~100–200 columns against a panel that is usually wider, so a line wraps rarely and a query landing exactly on the cut is rarer still; the alternative (keeping SoftWrap) costs 60 % of every frame.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/log_viewer.go internal/tui/log_viewer_test.go
git commit -m "perf(tui): pre-wrap log lines at insertion and memoise the log panel

CORE-2 (1/3). bubbles' viewport walks every buffered line calling
ansi.StringWidth on each render under SoftWrap - 2.35 ms and 6,192 allocs at
the 1,000-line cap, 60% of a whole frame, paid ~120 times a second. SoftWrap
is now off and lines are hard-wrapped once at insertion with the same
ansi.Cut rule the viewport used, so calculateLine is O(1). The rendered
panel is additionally memoised on every input View() reads, so a frame with
no log change re-renders nothing. The header counts SOURCE lines (rawCount),
not wrapped display rows.

Cadence untouched: View() still runs per message; each frame is cheaper.
Accepted: a search term straddling a wrap boundary no longer matches.

Report row #34.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/log_viewer.go internal/tui/log_viewer_test.go
```

---

### Task 3: Render caches for the task list and the details panel

Row: **#34 (CORE-2)**, part 2 of 3.

The remaining per-frame cost is the task list (0.61 ms) and the details panel (0.39 ms). Both are pure functions of their model state, so both get the same treatment as the log panel: a comparable key built from what `View()` actually reads, plus **the wall-clock second** — `task_list.go`'s `renderHeader` draws live monitor countdowns (`d := time.Until(next)`) and `job_details.go` renders relative timestamps, so a cache that ignores the second would freeze them (spec §5; `_verify-core.md`'s implementation caveat).

The task list has one input that is not on the model: `progressCellText` reads `m.progressStore`, and progress-only job updates write there **without touching the task list at all** (`app_update.go` gates `taskList.UpdateJob` on `hasDisplayChange`). So `ProgressStore` gains a revision counter and the key carries it.

**Files:**
- Modify: `internal/tui/progress_store.go`, `internal/tui/task_list.go`, `internal/tui/job_details.go`, `internal/tui/app_update.go` (the `marqueeTickMsg` branch only)
- Test: Create `internal/tui/render_cache_test.go`

**Interfaces:**
- Consumes: nothing from Task 2 (different file, different key type).
- Produces (package `tui`): `func (s *ProgressStore) Rev() uint64`; `func (m *TaskListModel) invalidate()`; fields `TaskListModel.rebuildSeq/renderCache/cacheKey`, `JobDetailsModel.contentSeq/renderCache/cacheKey`; key types `taskListKey`, `jobDetailsKey`. The details panel gets NO `invalidate()` helper — every input it renders is in its key, and an unexported method with only a test caller is a staticcheck U1000 hit. Task 4 consumes none of these directly.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/render_cache_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// bubbletea calls View() after EVERY message, so a frame that changes
// nothing must cost nothing. The probe mutates a job IN PLACE, which no
// production path does without ending in rebuildVirtualList: if the second
// View() shows the new title, the cache was not consulted (CORE-2).
//
// Mutant: deleting the cache lookup at the top of TaskListModel.View().
func TestTaskListViewIsCachedBetweenIdenticalFrames(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 12)
	job := &database.Job{ID: "a", Title: "ORIGINAL", Status: database.StatusLive}
	m.SetJobs([]*database.Job{job})
	first := m.View()

	job.Title = "MUTATED"
	if second := m.View(); second != first {
		t.Errorf("an unchanged task list must return the cached frame:\n%s", stripANSI(second))
	}

	// A rebuild is the real invalidator.
	m.SetJobs([]*database.Job{job})
	if got := stripANSI(m.View()); !strings.Contains(got, "MUTATED") {
		t.Errorf("a rebuild must invalidate the cache, got:\n%s", got)
	}
}

// The task list renders each active row's live percent straight out of the
// progress store, and a progress-only job update never touches the task list
// model (app_update.go gates UpdateJob on hasDisplayChange). The store's
// revision counter is therefore part of the cache key.
//
// Mutant: dropping progressRev from taskListKey — the row keeps showing 10%.
func TestTaskListCacheFollowsTheProgressStore(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 12)
	job := &database.Job{ID: "a", Title: "A", Status: database.StatusDownloading}
	m.SetJobs([]*database.Job{job})
	m.progressStore.Set("a", &ProgressData{Progress: "V:1 A:1", Percent: 10})
	if got := stripANSI(m.View()); !strings.Contains(got, "10%") {
		t.Fatalf("want the seeded 10%% in the row, got:\n%s", got)
	}

	m.progressStore.Set("a", &ProgressData{Progress: "V:2 A:2", Percent: 55})
	if got := stripANSI(m.View()); !strings.Contains(got, "55%") {
		t.Errorf("a progress-store write must invalidate the row cache, got:\n%s", got)
	}
}

// renderHeader draws live monitor countdowns (time.Until), so the key
// carries the wall-clock second — otherwise the countdown freezes for as
// long as nothing else changes (spec §5).
//
// Mutant: dropping sec from taskListKey.
func TestTaskListCacheKeyCarriesTheWallClockSecond(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(80, 12)
	m.SetJobs([]*database.Job{{ID: "a", Title: "A", Status: database.StatusLive}})
	m.NextFeedCheck = time.Now().Add(90 * time.Second)

	k1 := m.taskListKey()
	k2 := m.taskListKey()
	if k1 != k2 {
		t.Fatalf("two keys sampled in the same second must be equal:\n%+v\n%+v", k1, k2)
	}
	if k1.sec != time.Now().Unix() {
		t.Errorf("key.sec = %d, want the current wall-clock second", k1.sec)
	}
}

// The details panel is memoised on the same rule. The probe writes straight
// into the viewport, keeping the line count identical so nothing in the key
// moves; only updateViewportContent (the single funnel, which bumps
// contentSeq) may make a change visible.
//
// Mutant: deleting the cache lookup at the top of JobDetailsModel.View() —
// the injected text appears in the second frame.
func TestJobDetailsViewIsCachedBetweenIdenticalFrames(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(60, 20)
	m.SetJob(&database.Job{ID: "a", Title: "ORIGINAL", Status: database.StatusLive, Platform: "youtube"})
	first := m.View()
	if !strings.Contains(stripANSI(first), "ORIGINAL") {
		t.Fatalf("the first frame must show the job's title, got:\n%s", stripANSI(first))
	}

	inject := make([]string, m.viewport.TotalLineCount())
	for i := range inject {
		inject[i] = "MUTATED"
	}
	m.viewport.SetContentLines(inject)

	if second := m.View(); second != first {
		t.Errorf("an unchanged details panel must return the cached frame:\n%s", stripANSI(second))
	}

	// Not vacuous: the injection IS visible once the cache is dropped. The
	// field is poked directly rather than through a helper — the details
	// model has no production reason to invalidate out of band, and an
	// unexported method with only a test caller is a staticcheck U1000 hit.
	m.renderCache = ""
	if got := stripANSI(m.View()); !strings.Contains(got, "MUTATED") {
		t.Fatalf("the probe must be visible without the cache, got:\n%s", got)
	}

	// The real funnel restores the rows and invalidates through contentSeq.
	m.updateViewportContent()
	if got := stripANSI(m.View()); !strings.Contains(got, "ORIGINAL") {
		t.Errorf("a content rebuild must invalidate the cache, got:\n%s", got)
	}
}
```

Add `"time"` to the import block.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestTaskListView|TestTaskListCache|TestJobDetailsView' -v
```
Expected: compile failure — `undefined: (*TaskListModel).taskListKey`.

- [ ] **Step 3: Give `ProgressStore` a revision**

In `internal/tui/progress_store.go`, add the field and bump it in the three writers:

```go
type ProgressStore struct {
	items map[string]*ProgressData
	// rev increments on every write. The task list renders each active row's
	// percent straight out of this store, and a progress-only job update
	// never touches the task list model at all (app_update.go gates
	// UpdateJob on hasDisplayChange) — so the row's render cache keys on
	// this counter rather than on anything the model can observe (CORE-2).
	rev uint64
}

// Rev returns the store's write revision. Equal revisions mean no entry has
// been Set, Deleted or Cleared since.
func (s *ProgressStore) Rev() uint64 { return s.rev }
```

```go
func (s *ProgressStore) Set(jobID string, data *ProgressData) {
	s.items[jobID] = data
	s.rev++
}

func (s *ProgressStore) Delete(jobID string) {
	delete(s.items, jobID)
	s.rev++
}

func (s *ProgressStore) Clear() {
	s.items = make(map[string]*ProgressData)
	s.rev++
}
```

- [ ] **Step 4: Cache the task list**

In `internal/tui/task_list.go`:

1. Add to `TaskListModel`, after `JustCompletedSetup`:

```go
	// rebuildSeq increments in rebuildVirtualList — the ONE funnel every
	// content change (SetJobs, AddJob, RemoveJob, UpdateJob, CycleFilter,
	// ToggleArchive, applyQuery, ResweepArchive, SetHideFinishedAgeDays)
	// ends in. Everything else the frame depends on is read directly off the
	// model or the embedded list in taskListKey, so no mutator can forget to
	// invalidate the cache.
	rebuildSeq uint64
	// renderCache / cacheKey memoise View(). bubbletea renders after every
	// message (~120/s with one active download); most carry no list change.
	renderCache string
	cacheKey    taskListKey
```

2. Bump the counter — add as the FIRST statement of `rebuildVirtualList`:

```go
func (m *TaskListModel) rebuildVirtualList() {
	m.rebuildSeq++
	prevSelectedID := m.captureSelection()
```

3. Add the key and the invalidator directly above `View()`:

```go
// taskListKey is every input TaskListModel.View() reads, as a comparable
// struct so a cache hit is one ==.
//
//   - rebuildSeq covers the rows themselves (rebuildVirtualList is the only
//     writer of m.list's items).
//   - progressRev covers each active row's live percent, which is read from
//     the progress store at render time by a code path the model never sees.
//   - sec covers renderHeader's monitor countdowns (time.Until) — without it
//     the countdown would freeze for as long as nothing else moved
//     (spec §5 ruling).
//   - marqueeOffset covers the scrolling selected title.
//   - selectedCount is faithful because ToggleSelection always moves the
//     count by one.
type taskListKey struct {
	rebuildSeq    uint64
	progressRev   uint64
	sec           int64
	width         int
	height        int
	focused       bool
	cursor        int
	page          int
	items         int
	selectedCount int
	marqueeOffset int
	summary       string
	query         string
	justSetup     bool
	nextFeed      time.Time
	nextDecapi    time.Time
	nextTwitch    time.Time
}

func (m *TaskListModel) taskListKey() taskListKey {
	var rev uint64
	if m.progressStore != nil {
		rev = m.progressStore.Rev()
	}
	return taskListKey{
		rebuildSeq:    m.rebuildSeq,
		progressRev:   rev,
		sec:           time.Now().Unix(),
		width:         m.width,
		height:        m.height,
		focused:       m.focused,
		cursor:        m.list.Index(),
		page:          m.list.Paginator.Page,
		items:         len(m.list.Items()),
		selectedCount: len(m.selected),
		marqueeOffset: m.marquee.offset,
		summary:       m.statusSummary,
		query:         m.queryText,
		justSetup:     m.JustCompletedSetup,
		nextFeed:      m.NextFeedCheck,
		nextDecapi:    m.NextDecapiCheck,
		nextTwitch:    m.NextTwitchCheck,
	}
}

// invalidate drops the memoised frame.
func (m *TaskListModel) invalidate() {
	m.renderCache = ""
}
```

4. Wrap `View()`. At the top:

```go
func (m *TaskListModel) View() string {
	// The search box renders a blinking textinput cursor whose state is not
	// in the key, so while it is open the panel is rendered every frame.
	if !m.searching {
		if k := m.taskListKey(); m.renderCache != "" && k == m.cacheKey {
			return m.renderCache
		}
	}
	contentW := max(m.width-2, 1)
```

and at the bottom, replace `return style.Width(m.width).Height(m.height).Render(content)` with:

```go
	out := style.Width(m.width).Height(m.height).Render(content)
	if !m.searching {
		m.renderCache = out
		m.cacheKey = m.taskListKey()
	} else {
		m.renderCache = ""
	}
	return out
}
```

- [ ] **Step 5: Cache the details panel**

In `internal/tui/job_details.go`:

1. Add to `JobDetailsModel`, after `updateInfo`:

```go
	// contentSeq increments in updateViewportContent — the single funnel for
	// every row change (SetJob, SetProgress, RefreshMarqueeFrame,
	// RefreshRelativeTimes, SetSize, ToggleDescription all end there).
	contentSeq uint64
	// renderCache / cacheKey memoise View(); bubbletea renders after every
	// message and most carry no change to this panel.
	renderCache string
	cacheKey    jobDetailsKey
```

2. Bump it — first statement of `updateViewportContent`:

```go
func (m *JobDetailsModel) updateViewportContent() {
	m.contentSeq++
	contentW := max(m.width-2, 1)
```

3. Add the key and invalidator above `View()`:

```go
// jobDetailsKey is every input JobDetailsModel.View() reads. sec is present
// for the same reason the task list's is: the panel renders wall-clock text,
// and the 1 Hz RefreshRelativeTimes that recomputes it is itself gated, so
// the frame must be allowed to change once a second regardless (spec §5).
type jobDetailsKey struct {
	contentSeq uint64
	sec        int64
	width      int
	height     int
	focused    bool
	hideDesc   bool
	job        *database.Job
	status     database.JobStatus
	version    string
	update     *UpdateStatusMsg
	yOffset    int
	totalLines int
	vpHeight   int
}

func (m *JobDetailsModel) jobDetailsKey() jobDetailsKey {
	var status database.JobStatus
	if m.job != nil {
		status = m.job.Status
	}
	return jobDetailsKey{
		contentSeq: m.contentSeq,
		sec:        time.Now().Unix(),
		width:      m.width,
		height:     m.height,
		focused:    m.focused,
		hideDesc:   m.hideDescription,
		job:        m.job,
		status:     status,
		version:    m.version,
		update:     m.updateInfo,
		yOffset:    m.viewport.YOffset(),
		totalLines: m.viewport.TotalLineCount(),
		vpHeight:   m.viewport.Height(),
	}
}
```

(No `invalidate()` helper here: every input this panel renders is in the key, and an unexported method with only a test caller is a staticcheck U1000 hit. The test pokes `m.renderCache` directly — same package.)

4. Wrap `View()` the same way:

```go
func (m *JobDetailsModel) View() string {
	if k := m.jobDetailsKey(); m.renderCache != "" && k == m.cacheKey {
		return m.renderCache
	}
	contentW := max(m.width-2, 1)
```

and at the bottom replace `return style.Width(m.width).Height(m.height).Render(content)` with:

```go
	out := style.Width(m.width).Height(m.height).Render(content)
	m.renderCache = out
	m.cacheKey = m.jobDetailsKey()
	return out
}
```

5. `ScrollUp`/`ScrollDown`/`UpdateViewport` move `yOffset`, which IS in the key — nothing extra needed. `SetFocused` sets a keyed field — likewise.

- [ ] **Step 6: Invalidate the task list on a marquee step**

`app_update.go`'s `marqueeTickMsg` branch advances the task list's marquee directly. `marquee.offset` is in the key, so the cache already follows it; but `Marquee.Tick()` also returns whether the offset MOVED, and the details panel uses that return. Make the task-list call read the same way so the intent is explicit. Replace:

```go
		a.taskList.marquee.Tick()
```
with:
```go
		if a.taskList.marquee.Tick() {
			// The offset is part of taskListKey, so the cache already
			// follows it; invalidate explicitly so the dependency is stated
			// at the mutation site rather than inferred from the key.
			a.taskList.invalidate()
		}
```

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestTaskListView|TestTaskListCache|TestJobDetailsView' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/tui/
```
Expected: PASS, whole package green. `TestHeaderStatusSummaryIsCached` and `TestResweepArchiveRefreshesTheSummary` in `frame_counts_test.go` must still pass unchanged — the first because an in-place job mutation moves nothing in the key, the second because `statusSummary` IS in the key.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/progress_store.go internal/tui/task_list.go internal/tui/job_details.go internal/tui/app_update.go internal/tui/render_cache_test.go
git commit -m "perf(tui): memoise the task list and details panels on their own inputs

CORE-2 (2/3). Both panels are pure functions of their model state, so View()
now returns a cached string when nothing they read has moved. The keys are
built from observable state rather than a hand-maintained dirty flag: one
sequence counter at each package's single content funnel (rebuildVirtualList,
updateViewportContent) plus scalars read straight off the model and the
viewport, so no mutator can forget to invalidate.

Two inputs are not on the models and are named in the keys: the progress
store gains a revision counter (a progress-only job update writes there
without touching the task list at all), and the wall-clock second, without
which the header's monitor countdowns and the panel's relative timestamps
would freeze.

Cadence untouched: View() still runs per message; each frame is cheaper.

Report row #34.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/progress_store.go internal/tui/task_list.go internal/tui/job_details.go internal/tui/app_update.go internal/tui/render_cache_test.go
```

---

### Task 4: The frame-cost benchmark, and an assertion that holds it

Row: **#34 (CORE-2)**, part 3 of 3.

Tasks 2 and 3 have to be *measured*, and the number has to be defended against regression. This task adds the benchmark harness the report's own numbers came from, records before/after `ns/op` and `allocs/op` at the 1,000-line log cap, and pins the after-number with a test. The assertion is on **allocs/op**, which is deterministic across machines; `ns/op` is recorded in the commit body and the ledger but never asserted.

**Files:**
- Test: Create `internal/tui/frame_cost_test.go`

**Interfaces:**
- Consumes: the caches from Tasks 2 and 3 (through `App.View()` only).
- Produces: nothing importable.

- [ ] **Step 1: Measure the BEFORE numbers**

Before writing anything, produce the baseline from the pre-Task-2 tree so the commit body can state a real before/after:

```bash
mkdir -p "C:/Users/Wulf/AppData/Local/Temp/claude/D--Git-Moombox/<session>/scratchpad/plan-c/before"
git archive main | tar -x -C "C:/Users/Wulf/AppData/Local/Temp/claude/D--Git-Moombox/<session>/scratchpad/plan-c/before"
cp internal/bgutils/embed/*.gz "…/before/internal/bgutils/embed/"
cp internal/cipher/testdata/*.js "…/before/internal/cipher/testdata/"
```

Copy the benchmark file from Step 2 into `…/before/internal/tui/frame_cost_test.go` with the `TestFrameCostAtLogCap` function deleted (it will not pass there — that is the point), run:

```bash
cd "…/before" && GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run '^$' -bench 'BenchmarkFrame|BenchmarkLogPanel' -benchmem -benchtime=200x
```

Record the four numbers. Expected order of magnitude from `_verify-core.md`: `BenchmarkFrameAtLogCap` ≈ 3.9–4.5 ms / ~21,700 allocs; `BenchmarkLogPanelAtLogCap` ≈ 2.35 ms / ~6,200 allocs.

- [ ] **Step 2: Write the benchmark and the failing assertion**

Create `internal/tui/frame_cost_test.go`:

```go
package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// frameBenchApp builds the shape CORE-2 measured: 1,000 jobs at 200x60 with
// the log panel at its maxLogLines cap. Everything a real frame reads is
// populated (progress entries for the active rows, a selected job in the
// details panel), so View() exercises all four panels.
func frameBenchApp(tb testing.TB) *App {
	tb.Helper()
	a := NewApp()
	a.width, a.height = 200, 60
	a.recalcLayout()

	jobs := make([]*database.Job, 0, 1000)
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range 1000 {
		status := database.StatusFinished
		if i%5 == 0 {
			status = database.StatusDownloading
		}
		j := &database.Job{
			ID:          fmt.Sprintf("job-%04d", i),
			VideoID:     fmt.Sprintf("vid%08d", i),
			Title:       fmt.Sprintf("A reasonably long stream title number %d", i),
			ChannelName: fmt.Sprintf("Channel %d", i%37),
			Platform:    "youtube",
			Status:      status,
			UpdatedAt:   now,
			CreatedAt:   now,
		}
		jobs = append(jobs, j)
		if status == database.StatusDownloading {
			a.progressStore.Set(j.ID, &ProgressData{Progress: "V:1234 A:1234", Percent: float64(i % 100)})
		}
	}
	a.taskList.SetJobs(jobs)
	a.statusBar.SetJobs(jobs)
	a.details.SetJob(jobs[0])

	batch := make([]string, maxLogLines)
	for i := range batch {
		batch[i] = fmt.Sprintf("2026-09-17 12:00:00 INFO job-%04d segment fetched seq=%d size=123456 speed=4.2MiB/s", i%1000, i)
	}
	a.logs.AddLines(batch)
	return a
}

// BenchmarkFrameAtLogCap is one bubbletea frame: the whole View() with every
// panel populated and the log panel at its cap. This is the number CORE-2 is
// about — bubbletea calls View() after EVERY message, ~120/s with one active
// download.
func BenchmarkFrameAtLogCap(b *testing.B) {
	a := frameBenchApp(b)
	a.View() // warm the caches, as a running TUI always is
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		a.View()
	}
}

// BenchmarkLogPanelAtLogCap isolates the panel that dominated the frame
// before CORE-2 (60% of it), with its cache defeated each iteration so the
// pre-wrap win is what is measured rather than the memoisation.
func BenchmarkLogPanelAtLogCap(b *testing.B) {
	a := frameBenchApp(b)
	a.logs.View()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		a.logs.invalidate()
		a.logs.View()
	}
}

// BenchmarkProgressFrameAtLogCap is the real steady-state message: one
// progress-store write followed by the frame bubbletea renders for it.
func BenchmarkProgressFrameAtLogCap(b *testing.B) {
	a := frameBenchApp(b)
	a.View()
	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		i++
		a.progressStore.Set("job-0000", &ProgressData{Progress: "V:1234 A:1234", Percent: float64(i % 100)})
		a.View()
	}
}

// maxCachedFrameAllocs bounds a bubbletea frame that carries NO change,
// which is what the 60 Hz tick delivers most of the time. On main this frame
// cost ~21,700 allocations because every panel re-rendered unconditionally;
// with the CORE-2 caches it is four key comparisons and a JoinVertical.
//
// Asserted on allocations, not nanoseconds: allocation counts are
// deterministic across machines and CI runners, wall time is not.
const maxCachedFrameAllocs = 400

// maxLogPanelAllocs bounds ONE uncached log-panel render at the 1,000-line
// cap. On main this was ~6,200 allocations because the viewport soft-wrapped
// every buffered line on every render; with pre-wrapped content the viewport
// touches only the visible rows.
const maxLogPanelAllocs = 2000

// TestFrameCostAtLogCap is the regression pin for CORE-2. It runs the two
// benchmarks in-process and asserts their allocation counts.
//
// Mutant: deleting the cache lookup at the top of TaskListModel.View() (or
// JobDetailsModel.View(), or LogViewerModel.View()) — the cached-frame
// assertion fails by more than an order of magnitude.
// Mutant: restoring vp.SoftWrap = true in NewLogViewerModel — the log-panel
// assertion fails.
func TestFrameCostAtLogCap(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation budget is a benchmark-backed pin; skipped in -short")
	}

	frame := testing.Benchmark(BenchmarkFrameAtLogCap)
	t.Logf("cached frame: %d ns/op, %d allocs/op, %d B/op",
		frame.NsPerOp(), frame.AllocsPerOp(), frame.AllocedBytesPerOp())
	if got := frame.AllocsPerOp(); got > maxCachedFrameAllocs {
		t.Errorf("a no-change frame allocates %d times, budget %d — a panel render cache is not being consulted",
			got, maxCachedFrameAllocs)
	}

	logs := testing.Benchmark(BenchmarkLogPanelAtLogCap)
	t.Logf("uncached log panel: %d ns/op, %d allocs/op, %d B/op",
		logs.NsPerOp(), logs.AllocsPerOp(), logs.AllocedBytesPerOp())
	if got := logs.AllocsPerOp(); got > maxLogPanelAllocs {
		t.Errorf("one log-panel render allocates %d times, budget %d — the viewport is soft-wrapping the whole buffer again",
			got, maxLogPanelAllocs)
	}
}
```

- [ ] **Step 3: Run the assertion and the benchmarks**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run TestFrameCostAtLogCap -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run '^$' -bench 'BenchmarkFrame|BenchmarkLogPanel|BenchmarkProgressFrame' -benchmem -benchtime=200x
```
Expected: PASS, with the logged numbers far under both budgets. **If either budget is exceeded on the branch, do NOT raise the constant** — the cache or the pre-wrap is not doing its job; find out which panel and fix it. If a budget turns out to be unreachably tight for a legitimate reason (e.g. `lipgloss.JoinVertical` allocating more than expected on this Go version), raise it to the measured number **rounded up to the next hundred** and record the measurement in the commit body.

- [ ] **Step 4: Verify both named mutants by execution**

```bash
# Mutant A: defeat one panel cache.
#   In internal/tui/task_list.go, temporarily delete the `if !m.searching { … return m.renderCache }`
#   block at the top of View(), then:
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run TestFrameCostAtLogCap -v   # expect FAIL on the cached-frame budget
#   Revert the edit.

# Mutant B: restore soft wrapping.
#   In internal/tui/log_viewer.go, temporarily set vp.SoftWrap = true in NewLogViewerModel, then:
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run TestFrameCostAtLogCap -v   # expect FAIL on the log-panel budget
#   Revert the edit.
```
Record both outputs in the task report. Confirm `git status` is clean of the temporary edits before committing.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/frame_cost_test.go
git commit -m "test(tui): benchmark and pin the frame cost at the 1,000-line log cap

CORE-2 (3/3). BenchmarkFrameAtLogCap / BenchmarkLogPanelAtLogCap /
BenchmarkProgressFrameAtLogCap reproduce the shape the sweep measured (1,000
jobs at 200x60, log panel at maxLogLines), and TestFrameCostAtLogCap runs the
first two in-process and asserts their allocation counts: 400 for a no-change
frame (~21,700 on main) and 2,000 for one uncached log-panel render (~6,200
on main). Allocations, not nanoseconds - deterministic across runners.

Before/after (200x, -benchmem): recorded in the arc ledger.

Report row #34.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/frame_cost_test.go
```

---
### Task 5: A dropped TUI job update is replayed, and the hide threshold crosses both UIs

Rows: **#35 (CORE-6)**, **#82 (CORE-11)**.

`tuiDroppedJobs` is incremented on a full 100-slot channel and read exactly once — at TUI exit. `docs/spec/user-interfaces.md:418` documents "The next successful send triggers a full state refresh" for a mechanism that was never built, so a dropped `Downloading → Finished` leaves a stale row for the whole session. CORE-2 is what makes the drop reachable; this makes it recoverable.

The same wiring file carries CORE-11: `OnHideFinishedAgeChanged` (the Web PUT's broadcast) has no TUI twin, and a Web-side change reaches the TUI list only when its settings overlay is next opened and closed.

**Files:**
- Modify: `cmd/moombox/tui_wiring.go` (the five DB forwarders ~:743-795; `OnSaveConfig` ~:255-308; the cleanup block ~:945-960)
- Modify: `cmd/moombox/routes_wiring.go` (`OnHideFinishedAgeChanged` ~:75-93)
- Modify: `internal/tui/app.go` (add `syncHideFinishedAge`), `internal/tui/app_update.go` (the 60 s sweep block), `internal/tui/task_list.go` (add `HideFinishedAgeDays()`)
- Test: Create `cmd/moombox/tui_resync_test.go`; append to `internal/tui/render_cache_test.go` is NOT used — create `internal/tui/hide_threshold_sync_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func (s *runState) broadcastHideFinishedAge()` in `cmd/moombox/routes_wiring.go`; `func (a *App) syncHideFinishedAge()` in `internal/tui/app.go`; `func (m *TaskListModel) HideFinishedAgeDays() float64` in `internal/tui/task_list.go`. **Task 7 changes `SetHideFinishedAgeDays` from `int` to `float64`** — write the accessor as `float64` now and have Task 7's edit find it already correct; if Task 7 has not run yet the field is still `int`, so write the accessor to return the field's CURRENT type and let Task 7 widen it with the field. State which you did in the task report.

- [ ] **Step 1: Write the failing tests**

Create `cmd/moombox/tui_resync_test.go`:

```go
package main

import (
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// A TUI job update dropped on a full channel must be replayed: the flag is
// set on the drop and the next forwarded event (or the 1 s sweep) pushes a
// full GetAllJobs snapshot down jobsUpdateCh. Without it a dropped
// Downloading->Finished leaves a stale row for the rest of the session, and
// user-interfaces.md documents a recovery that did not exist (CORE-6).
//
// Mutant: making resyncTUIJobs a no-op when the flag is set (or dropping the
// CompareAndSwap so it never fires) — no snapshot arrives.
func TestDroppedJobUpdateSchedulesAResync(t *testing.T) {
	var needed atomic.Bool
	jobsCh := make(chan []*database.Job, 1)
	snapshot := []*database.Job{{ID: "a"}, {ID: "b"}}

	resync := newTUIResync(&needed, jobsCh, func() ([]*database.Job, error) {
		return snapshot, nil
	})

	// Nothing dropped yet: the resync must be free.
	resync()
	select {
	case got := <-jobsCh:
		t.Fatalf("no drop happened; nothing must be pushed, got %d jobs", len(got))
	default:
	}

	needed.Store(true)
	resync()
	select {
	case got := <-jobsCh:
		if len(got) != len(snapshot) {
			t.Errorf("resync pushed %d jobs, want %d", len(got), len(snapshot))
		}
	default:
		t.Fatal("a pending drop must push a full snapshot")
	}
	if needed.Load() {
		t.Error("a delivered snapshot must clear the pending flag")
	}
}

// A snapshot that cannot be fetched or cannot be delivered must leave the
// flag ARMED so the next event tries again — silently clearing it would turn
// one dropped update into a permanently stale row.
//
// Mutant: clearing the flag unconditionally instead of re-arming on failure.
func TestResyncStaysArmedWhenItCannotDeliver(t *testing.T) {
	var needed atomic.Bool
	full := make(chan []*database.Job) // unbuffered, nobody reading
	resync := newTUIResync(&needed, full, func() ([]*database.Job, error) {
		return []*database.Job{{ID: "a"}}, nil
	})

	needed.Store(true)
	resync()
	if !needed.Load() {
		t.Error("an undeliverable snapshot must leave the resync armed")
	}

	var needed2 atomic.Bool
	ok := make(chan []*database.Job, 1)
	failing := newTUIResync(&needed2, ok, func() ([]*database.Job, error) {
		return nil, errResyncTest
	})
	needed2.Store(true)
	failing()
	if !needed2.Load() {
		t.Error("a failed GetAllJobs must leave the resync armed")
	}
}

var errResyncTest = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }
```

Create `internal/tui/hide_threshold_sync_test.go`:

```go
package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// A hide_finished_age_days change made from the DASHBOARD has no TUI-side
// event at all: before this the TUI re-read the threshold only when its own
// settings overlay was opened and closed, so the two UIs disagreed about
// which Finished jobs are archived indefinitely (CORE-11). The 60 s archive
// sweep now re-reads it.
//
// Mutant: deleting the syncHideFinishedAge call from the sweep — the list
// keeps the boot threshold.
func TestSyncHideFinishedAgePicksUpAWebSideChange(t *testing.T) {
	cfg := config.Defaults()
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 30}
	store := config.NewStore(cfg, "")

	a := NewApp()
	a.SetConfigStore(store)
	a.SetConfig(cfg)
	if got := a.taskList.HideFinishedAgeDays(); got != 30 {
		t.Fatalf("boot threshold = %v, want 30", got)
	}

	// The dashboard's PUT writes through the store.
	if err := store.Update(func(c *config.MoomboxConfig) {
		c.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}

	a.syncHideFinishedAge()
	if got := a.taskList.HideFinishedAgeDays(); got != 0.5 {
		t.Errorf("after the sweep the list threshold = %v, want 0.5", got)
	}
}
```

`config.NewStore(cfg, "")` with an empty path must not write to disk in this test — `store.Update` calls `config.Save`. If `Save` with an empty path errors, pass `t.TempDir()+"/config.toml"` instead and keep the assertion identical.

- [ ] **Step 2: Run both tests red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/ -run 'TestDroppedJobUpdate|TestResyncStaysArmed' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run TestSyncHideFinishedAge -v
```
Expected: compile failures — `undefined: newTUIResync`, `undefined: syncHideFinishedAge`, `undefined: HideFinishedAgeDays`.

- [ ] **Step 3: Build the resync**

In `cmd/moombox/tui_wiring.go`, add above `runTUI` (or beside the other file-local helpers):

```go
// newTUIResync builds the replay for a dropped TUI job update. The forwarders
// below send non-blocking on 100-slot channels and count the drop; nothing
// ever replayed it, so a dropped Downloading->Finished left a stale row for
// the rest of the session while user-interfaces.md claimed a resync existed
// (CORE-6).
//
// The returned func is free when nothing was dropped (one atomic load). When
// a drop IS pending it takes the flag, fetches a full snapshot and pushes it
// down the full-list channel the TUI already handles; if either the fetch or
// the push fails the flag is re-armed so the next caller tries again —
// clearing it there would turn one dropped update into a permanent
// divergence.
func newTUIResync(needed *atomic.Bool, jobsCh chan []*database.Job, getAll func() ([]*database.Job, error)) func() {
	return func() {
		if !needed.CompareAndSwap(true, false) {
			return
		}
		jobs, err := getAll()
		if err != nil {
			needed.Store(true)
			return
		}
		select {
		case jobsCh <- jobs:
		default:
			needed.Store(true)
		}
	}
}
```

Declare the flag beside the drop counters:

```go
	// Dropped-message counters — track silent drops on TUI channels
	var tuiDroppedJobs, tuiDroppedLogs atomic.Int64
	// tuiResyncNeeded arms the full-snapshot replay after a dropped job
	// event (CORE-6). See newTUIResync.
	var tuiResyncNeeded atomic.Bool
	resyncTUIJobs := newTUIResync(&tuiResyncNeeded, jobsUpdateCh, s.db.GetAllJobs)
```

In each of the FOUR forwarders that count a drop (`OnJobChange`, `OnJobAdded`, `OnJobDeleted`, `OnTrimsChanged`), arm the flag on the drop and run the replay on the way in. The `OnJobChange` forwarder becomes:

```go
	unsubTUIJobUpdate := s.db.OnJobChange(func(ev *database.JobChange) {
		resyncTUIJobs()
		select {
		case jobUpdateCh <- ev:
		default:
			tuiDroppedJobs.Add(1)
			if tuiResyncNeeded.CompareAndSwap(false, true) {
				s.log.Warn("TUI job update dropped — a full refresh is queued",
					slog.String("job", ev.Job.ID))
			}
		}
	})
```

Apply the identical two changes to `OnJobAdded` (`ev.Job.ID`), `OnJobDeleted` (`ev.JobID`) and `OnTrimsChanged` (`ev.JobID`, using the re-fetched `job.ID`). The `OnJobsChange` forwarder already carries a full list; leave its `default:` empty as it is (dropping a snapshot when a snapshot is already queued is harmless) but add `resyncTUIJobs()` at its top so a queued replay is satisfied by the newer list.

Add the 1 s backstop so the row's "or the 1 s tick" holds even when no further event arrives. Immediately after the forwarders are registered:

```go
	// 1 s backstop for the resync: the forwarders above cover the common
	// case (drops happen under event pressure, so more events follow), but a
	// drop whose job then goes quiet would otherwise never be replayed. One
	// atomic load per second when nothing is pending.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("TUI resync backstop panic", "panic", r)
			}
		}()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				resyncTUIJobs()
			}
		}
	}()
```

- [ ] **Step 4: Share the hide-threshold broadcast**

In `cmd/moombox/routes_wiring.go`, extract the closure body to a method and reference it from the callbacks struct. Replace the whole `OnHideFinishedAgeChanged: func() { … },` literal with:

```go
		OnHideFinishedAgeChanged: s.broadcastHideFinishedAge,
```

and add the method at the bottom of the file, carrying the existing comment verbatim plus the new sentence:

```go
// broadcastHideFinishedAge pushes a hide_finished_age_days change to every
// dashboard: the config_update first, then the re-filtered job list.
//
// Send config_update FIRST so the Web UI's hideFinishedAgeDays is already up
// to date by the time the jobs_update payload (filtered with the new
// threshold) arrives. Otherwise the per-client FIFO queue would deliver
// jobs_update first, and the Web UI's archive re-eval would run with the
// stale threshold and undo the server's widening on a threshold increase.
// Capture the threshold ONCE and reuse it for both the config_update payload
// and the job filtering below. Re-reading the store for the filter (via
// filterJobsByAge) would race a concurrent config change and could broadcast
// a hideFinishedAgeDays that disagrees with the threshold the jobs_update was
// filtered by.
//
// TWO callers, deliberately one method: the Web PUT's
// ConfigRoutesCallbacks.OnHideFinishedAgeChanged and the TUI's OnSaveConfig
// (tui_wiring.go). Before that pairing a TUI save never reached the
// dashboard at all (CORE-11).
func (s *runState) broadcastHideFinishedAge() {
	var hideAge float64
	s.configStore.Read(func(c *config.MoomboxConfig) {
		hideAge = c.Monitors.HideFinishedAgeDays.Value
	})
	s.wsHub.Broadcast("config_update", map[string]any{"hideFinishedAgeDays": hideAge})
	jobs, _ := s.db.GetAllJobs()
	s.wsHub.BroadcastJobsUpdate(filterJobsByAgeThreshold(jobs, hideAge))
}
```

In `cmd/moombox/tui_wiring.go`'s `OnSaveConfig`, call it in the success branch, right after `cookies.InvalidateBrowserDetection()`:

```go
			cookies.InvalidateBrowserDetection()
			// A TUI settings save must reach the dashboards too — the Web
			// PUT has always broadcast this, the TUI never did (CORE-11).
			s.broadcastHideFinishedAge()
```

- [ ] **Step 5: Re-read the threshold on the TUI's 60 s sweep**

In `internal/tui/task_list.go`, add beside `SetHideFinishedAgeDays`:

```go
// HideFinishedAgeDays returns the archive threshold the list is currently
// bucketing with. Read by the App's periodic config resync so a threshold
// changed from the dashboard can be applied without an unconditional rebuild.
func (m *TaskListModel) HideFinishedAgeDays() float64 { return m.hideFinishedAgeDays }
```

(If Task 7 has not run yet, the field is still `int` — return `int` and let Task 7 widen the accessor together with the field. Say which you did.)

In `internal/tui/app.go`, add beside `SetConfig`:

```go
// syncHideFinishedAge re-reads hide_finished_age_days from the config store
// and pushes it to the list when it moved.
//
// The TUI's own settings save applies it on overlay close (app_keys.go), but
// a change made from the DASHBOARD produces no TUI-side event at all — so
// before this the two UIs disagreed about which Finished jobs are archived
// until the operator happened to open and close the TUI settings overlay
// (CORE-11). Called from the same 60 s archive sweep that already re-buckets
// aged rows, so it costs one store read a minute.
func (a *App) syncHideFinishedAge() {
	if a.configStore == nil {
		return
	}
	var days float64
	a.configStore.Read(func(c *config.MoomboxConfig) {
		days = c.Monitors.HideFinishedAgeDays.Days()
	})
	if days == a.taskList.HideFinishedAgeDays() {
		return
	}
	a.taskList.SetHideFinishedAgeDays(days)
}
```

In `internal/tui/app_update.go`, inside the `tickMsg` handler's sweep block:

```go
		if now := time.Now(); now.Sub(a.lastArchiveSweep) >= time.Minute {
			a.lastArchiveSweep = now
			a.syncHideFinishedAge()
			a.taskList.ResweepArchive()
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/ -run 'TestDroppedJobUpdate|TestResyncStaysArmed' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run TestSyncHideFinishedAge -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ ./cmd/moombox/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/tui/ ./cmd/moombox/
```
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/moombox/tui_wiring.go cmd/moombox/routes_wiring.go cmd/moombox/tui_resync_test.go internal/tui/app.go internal/tui/app_update.go internal/tui/task_list.go internal/tui/hide_threshold_sync_test.go
git commit -m "fix(tui,cmd): replay dropped TUI job updates; carry hide_finished_age_days across both UIs

CORE-6: a drop on a full 100-slot channel now arms an atomic flag that the
next forwarded event - or a 1 s backstop ticker - satisfies with a full
GetAllJobs snapshot down the channel the TUI already handles. The flag is
re-armed when the fetch or the push fails, so a drop can never become a
permanently stale row. The drop is logged once per streak instead of only at
TUI exit.

CORE-11: OnHideFinishedAgeChanged's body becomes runState.broadcastHideFinishedAge
and the TUI's OnSaveConfig calls it, so a terminal-side change reaches the
dashboards; in the other direction the TUI's 60 s archive sweep re-reads the
threshold from the config store.

Report rows #35, #82.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- cmd/moombox/tui_wiring.go cmd/moombox/routes_wiring.go cmd/moombox/tui_resync_test.go internal/tui/app.go internal/tui/app_update.go internal/tui/task_list.go internal/tui/hide_threshold_sync_test.go
```

---

### Task 6: A failed config save says so, and `-log-level` never reaches disk

Rows: **#14 (CORE-4)**, **#16 (CORE-10)**.

`OnSaveConfig` returns nothing, so a failed `config.Save` (disk full, permissions, a validation the TUI did not pre-check) is only logged while the overlay shows "Saved" / "Password set successfully" — and because `applyValues` already wrote into the live store struct, the running process keeps values that are not on disk. In the same boot path, the `-log-level` CLI override is written into `cfg.Logs.LogLevel` *before* the auto-persist, the password auto-hash `Store.Update` and every later UI save, so a one-off diagnostic override becomes the persisted level.

**Files:**
- Modify: `internal/tui/app.go` (`OnSaveConfig` field), `internal/tui/settings.go` (`OnSave` field), `internal/tui/settings_keys.go` (`saveAndClose`), `internal/tui/settings_security.go` (two call sites), `internal/tui/app_update.go` (the ffmpeg-path save), `cmd/moombox/tui_wiring.go` (`OnSaveConfig` body), `cmd/moombox/services.go` (the log-level override)
- Test: `internal/tui/settings_security_test.go` (exists — append); Create `internal/tui/settings_save_error_test.go`; Create `cmd/moombox/log_level_override_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `App.OnSaveConfig func(cfg *config.MoomboxConfig) error` and `SettingsModel.OnSave func(cfg *config.MoomboxConfig) error` (signature change — every call site in the package must be updated in this task).

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/settings_save_error_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// A failed config.Save must be visible and must not leave the running
// process on values that are not on disk. applyValues writes straight into
// the live *MoomboxConfig the store holds, so the pre-apply snapshot is what
// makes the failure recoverable (CORE-4).
//
// Mutant: dropping the `*m.cfg = snapshot` restore — the live struct keeps
// the typed value after the refusal.
func TestFailedSaveIsReportedAndRolledBack(t *testing.T) {
	cfg := config.Defaults()
	cfg.Downloader.NumParallelDownloads = 4
	store := config.NewStore(cfg, "")

	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)
	m.values["num_parallel_downloads"] = "9"
	m.dirty = true
	m.OnSave = func(*config.MoomboxConfig) error { return errors.New("disk full") }

	m.saveAndClose()

	if m.status != saveError {
		t.Errorf("status = %v, want saveError", m.status)
	}
	if !strings.Contains(m.errorMsg, "disk full") {
		t.Errorf("errorMsg = %q, want it to carry the save error", m.errorMsg)
	}
	if got := cfg.Downloader.NumParallelDownloads; got != 4 {
		t.Errorf("live config kept %d after a failed save, want the pre-apply 4", got)
	}
}

// The success path is unchanged: the callback's nil is "Saved".
//
// Mutant: treating a nil error as a failure.
func TestSuccessfulSaveStillReportsSaved(t *testing.T) {
	cfg := config.Defaults()
	store := config.NewStore(cfg, "")

	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)
	m.values["num_parallel_downloads"] = "9"
	m.dirty = true
	called := 0
	m.OnSave = func(*config.MoomboxConfig) error { called++; return nil }

	m.saveAndClose()

	if called != 1 {
		t.Errorf("OnSave called %d times, want 1", called)
	}
	if m.status != saveSaved {
		t.Errorf("status = %v, want saveSaved", m.status)
	}
	if got := cfg.Downloader.NumParallelDownloads; got != 9 {
		t.Errorf("live config = %d after a successful save, want the typed 9", got)
	}
}
```

Adjust the constructor/method names to whatever `internal/tui/settings.go` actually exports (`NewSettingsModel`, `Open`, `saveAndClose`, `saveError`, `saveSaved` were read from main — confirm before writing). `Open(cfg)` seeds `m.values`; if the model needs `SetSize` first, call it.

Create `cmd/moombox/log_level_override_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// -log-level is a one-off diagnostic. Writing it into cfg.Logs.LogLevel (as
// initServices did) made the boot auto-persist, the password auto-hash
// Store.Update and every later UI save write it to disk, so a single
// `-log-level=debug` run permanently changed the configured level (CORE-10).
//
// Mutant: restoring `cfg.Logs.LogLevel = logLevelOverride` — the saved file
// reads DEBUG.
func TestLogLevelOverrideNeverReachesDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[logs]\nlog_level = \"INFO\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(cfg, path)

	// The boot sequence initServices runs, with the override applied the way
	// the fix applies it: to the logger's level only.
	level := effectiveLogLevel(cfg.Logs.LogLevel, "DEBUG")
	if level != "DEBUG" {
		t.Fatalf("effectiveLogLevel = %q, want the override DEBUG", level)
	}
	if err := store.SaveLocked(); err != nil {
		t.Fatalf("SaveLocked: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(string(data)), "DEBUG") {
		t.Errorf("the override reached disk:\n%s", data)
	}
}
```

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestFailedSaveIsReported|TestSuccessfulSaveStill' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/ -run TestLogLevelOverrideNever -v
```
Expected: compile failures — the `OnSave` literal does not match `func(*config.MoomboxConfig)`, and `effectiveLogLevel` is undefined.

- [ ] **Step 3: Widen the callback and restore on failure**

`internal/tui/app.go`:
```go
	// OnSaveConfig persists the settings model's config. It returns the save
	// error so the overlay can report a failure instead of showing "Saved"
	// over a write that never landed (CORE-4).
	OnSaveConfig      func(cfg *config.MoomboxConfig) error
```

`internal/tui/settings.go`:
```go
	OnSave    func(cfg *config.MoomboxConfig) error
```

`internal/tui/settings_keys.go` — replace the body of `saveAndClose`'s dirty branch:

```go
	if m.dirty && m.status != saveError {
		// Snapshot BEFORE applyValues. applyValues writes straight into the
		// live *MoomboxConfig the store holds (Open stores the store's own
		// pointer), so without a snapshot a refused save leaves the running
		// process on values that are not on disk while the overlay says
		// "Saved" (CORE-4). A SHALLOW copy is enough: applyValues REPLACES
		// the slice fields it touches (ActivePlatforms, TrustedProxies,
		// ProbeTargets) rather than mutating them in place, so the headers in
		// the copy still point at the pre-save backing arrays.
		snapshot := *m.cfg
		m.applyValues()
		if m.status == saveError {
			m.restoreConfig(snapshot)
			return "" // Validation failed, show error
		}
		if m.OnSave != nil {
			if err := m.OnSave(m.cfg); err != nil {
				m.restoreConfig(snapshot)
				m.errorMsg = "Save failed: " + err.Error()
				m.status = saveError
				return ""
			}
		}
		m.status = saveSaved
```

and add beside it:

```go
// restoreConfig puts the live config back to a pre-applyValues snapshot,
// under the store lock so a concurrent reader never observes the half-rolled
// struct.
func (m *SettingsModel) restoreConfig(snapshot config.MoomboxConfig) {
	if m.cfg == nil {
		return
	}
	if m.configStore != nil {
		mu := m.configStore.RWMutex()
		mu.Lock()
		*m.cfg = snapshot
		mu.Unlock()
		return
	}
	*m.cfg = snapshot
}
```

`internal/tui/settings_security.go` — both sites. The set-password site:

```go
		if m.OnSave != nil {
			if err := m.OnSave(m.cfg); err != nil {
				m.secMessage = "Save failed: " + err.Error()
				m.secMessageColor = ColorRed
				return
			}
		}
```
(placed before `if m.OnSecurityChanged != nil { … }` so a failed write does not announce a security change). The remove-password site takes the identical shape, returning before `m.OnSecurityChanged` and before the "Password removed" message.

`internal/tui/app_update.go` — the ffmpeg-path save:

```go
				if saveCb != nil {
					if err := saveCb(cfgSnapshot); err != nil {
						a.setFeedback("Failed to save FFmpeg path: " + err.Error())
					}
				}
```

`cmd/moombox/tui_wiring.go` — the callback returns the error:

```go
	app.OnSaveConfig = func(updatedCfg *config.MoomboxConfig) error {
		…
		if saveErr != nil {
			s.log.Error("Failed to save config from TUI", slog.String("error", saveErr.Error()))
			// Return before the hot-reload block: applying runtime settings
			// from a config that is not on disk would make the process and
			// the file diverge in the OTHER direction (CORE-4).
			return saveErr
		}
		…existing success body, unchanged…
		s.kickMonitors()
		return nil
	}
```

Keep every existing statement of the success body in place and in order; the only structural change is the early `return saveErr` and the closing `return nil`.

- [ ] **Step 4: Apply `-log-level` to the logger only**

In `cmd/moombox/services.go`, delete

```go
	if logLevelOverride != "" {
		cfg.Logs.LogLevel = logLevelOverride
	}
```

and change the logger construction to

```go
	// The -log-level override is a one-off diagnostic: it reaches the LOGGER
	// and nothing else. Writing it into cfg.Logs.LogLevel (as this used to)
	// made the boot auto-persist below, the password auto-hash Store.Update
	// and every later UI save write the override to disk, so a single
	// `-log-level=debug` run permanently changed the configured level, and
	// the operator's only clue was a level that never went back (CORE-10).
	// A later settings save legitimately re-applies the CONFIGURED level via
	// Logger.SetLevel and drops the override — that is the operator having
	// chosen a level explicitly.
	log, err := logger.New(cfg.Paths.LogFilePath, effectiveLogLevel(cfg.Logs.LogLevel, logLevelOverride), cfg.Logs.LogMaxFileSize, cfg.Logs.LogMaxFiles)
```

Add the helper to `cmd/moombox/helpers.go`:

```go
// effectiveLogLevel picks the level the logger starts at: the -log-level
// override when one was given, otherwise the configured level. Deliberately
// a pure function of the two strings so the "the override never reaches the
// config struct" rule can be asserted without a boot (CORE-10).
func effectiveLogLevel(configured, override string) string {
	if override != "" {
		return override
	}
	return configured
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestFailedSaveIsReported|TestSuccessfulSaveStill' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./cmd/moombox/ -run TestLogLevelOverrideNever -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ ./cmd/moombox/ ./internal/config/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/tui/ ./cmd/moombox/
```
Expected: PASS. `settings_security_test.go`'s existing `m.OnSave = func(*config.MoomboxConfig) { saved++ }` will not compile — update it to `func(*config.MoomboxConfig) error { saved++; return nil }` and add one assertion beside it:

```go
	// A refused save must not announce success (CORE-4).
	//
	// Mutant: ignoring OnSave's error in handleSetPassword.
	m.OnSave = func(*config.MoomboxConfig) error { return errors.New("nope") }
	m.handleSetPassword()
	if !strings.Contains(m.secMessage, "nope") {
		t.Errorf("secMessage = %q, want the save error", m.secMessage)
	}
```
(name the real method if `handleSetPassword` differs).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/app.go internal/tui/settings.go internal/tui/settings_keys.go internal/tui/settings_security.go internal/tui/settings_security_test.go internal/tui/app_update.go internal/tui/settings_save_error_test.go cmd/moombox/tui_wiring.go cmd/moombox/services.go cmd/moombox/helpers.go cmd/moombox/log_level_override_test.go
git commit -m "fix(tui,cmd): report a failed config save and roll it back; -log-level never persists

CORE-4: OnSaveConfig/OnSave return error. saveAndClose takes a shallow
snapshot of the live *MoomboxConfig before applyValues and restores it (under
the store lock) when either validation or the save fails, so the overlay can
no longer show 'Saved' over a write that never landed while the running
process keeps values that are not on disk. The two security sites and the
FFmpeg-path save report the failure the same way.

CORE-10: the -log-level override reaches logger.New through
effectiveLogLevel and is never written into cfg.Logs.LogLevel, so the boot
auto-persist, the password auto-hash Store.Update and every later UI save
stop writing a one-off diagnostic to config.toml.

Report rows #14, #16.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/app.go internal/tui/settings.go internal/tui/settings_keys.go internal/tui/settings_security.go internal/tui/settings_security_test.go internal/tui/app_update.go internal/tui/settings_save_error_test.go cmd/moombox/tui_wiring.go cmd/moombox/services.go cmd/moombox/helpers.go cmd/moombox/log_level_override_test.go
```

---

### Task 7: ONE archive-age predicate, in `internal/jobfilter`

Rows: **#15 (CORE-8 + WEB-8 halves)**.

There are four Go classifiers for "is this Finished job archived", and three of them compute `time.Duration(hideAgeDays*24) * time.Hour`, which converts the float to an INTEGER number of hours *before* multiplying — so every threshold below one hour (`FlexDuration` accepts `"0.02d"` ≈ 29 min) collapses to zero and archives every Finished job immediately, while the JS twin computes `ageDays * 86400 * 1000` ms exactly. The fourth, the TUI's, is worse: it takes `int(HideFinishedAgeDays)`, so `0.5` becomes `0` — "archive every Finished job immediately" — and then compares `ceil(hours/24) > ageDays`, a different rule again.

**Files:**
- Create: `internal/jobfilter/archive.go`, `internal/jobfilter/archive_test.go`
- Create: `web/tests/archive-boundary.test.mjs`
- Modify: `cmd/moombox/helpers.go` (`filterJobsByAgeThreshold` + delete `jobArchivedAt`), `cmd/moombox/monitor_callbacks.go` (the `:1557` archive-gate hunk ONLY), `internal/web/routes/jobs.go` (`filterJobsByAge`, `:86-134`), `internal/tui/task_list.go` (`isJobArchived`, `archiveBucketsDirty`, `rebuildVirtualList`, `SetHideFinishedAgeDays`, the `hideFinishedAgeDays` field), `internal/tui/app.go` (`SetConfig`), `internal/tui/app_keys.go` (the settings-close re-read)

**Interfaces:**
- Produces (package `jobfilter`): `func ArchiveCutoff(now time.Time, hideAgeDays float64) time.Time`, `func IsArchived(j *database.Job, cutoff time.Time) bool`, `func IsArchivedAt(j *database.Job, hideAgeDays float64, now time.Time) bool`.
- Changes: `TaskListModel.hideFinishedAgeDays` and `SetHideFinishedAgeDays` / `HideFinishedAgeDays` become `float64`; `isJobArchived` takes a `time.Time` cutoff instead of an `int` age.
- Deletes: `cmd/moombox.jobArchivedAt` (docs gate required).

- [ ] **Step 1: Write the failing tests**

Create `internal/jobfilter/archive_test.go`:

```go
package jobfilter

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// The threshold is a float number of DAYS and every fraction of it counts.
// The three Go copies this replaces wrote time.Duration(days*24)*time.Hour,
// which truncates to whole HOURS before multiplying — so 0.02d (≈29 min, a
// value FlexDuration accepts) became a zero cutoff and archived every
// Finished job immediately, while the JS twin
// (_evaluateArchiveBoundary in web/public/app.js) computed
// ageDays*86400*1000 ms exactly (WEB-8).
//
// Mutant: time.Duration(hideAgeDays*24) * time.Hour — the 0.02 and 0.5 rows
// return a zero-length window.
func TestArchiveCutoffKeepsSubHourFractions(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		days float64
		want time.Duration
	}{
		{0, 0},
		{0.02, 28*time.Minute + 48*time.Second}, // 0.02 * 86400 s
		{0.5, 12 * time.Hour},
		{1.5, 36 * time.Hour},
		{30, 720 * time.Hour},
	} {
		if got := now.Sub(ArchiveCutoff(now, tc.days)); got != tc.want {
			t.Errorf("ArchiveCutoff(%v): window = %v, want %v", tc.days, got, tc.want)
		}
	}
}

// IsArchived is the one classification: Finished only, a parseable
// updated_at only, strictly before the cutoff.
//
// Mutant: !t.After(cutoff) instead of t.Before(cutoff) — the "exactly at the
// cutoff" row flips.
func TestIsArchivedRules(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	cutoff := ArchiveCutoff(now, 0.5) // 12 hours

	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	for _, tc := range []struct {
		name string
		job  *database.Job
		want bool
	}{
		{"finished 13h ago", &database.Job{Status: database.StatusFinished, UpdatedAt: at(13 * time.Hour)}, true},
		{"finished 11h ago", &database.Job{Status: database.StatusFinished, UpdatedAt: at(11 * time.Hour)}, false},
		{"finished exactly at the cutoff", &database.Job{Status: database.StatusFinished, UpdatedAt: at(12 * time.Hour)}, false},
		{"cancelled 13h ago", &database.Job{Status: database.StatusCancelled, UpdatedAt: at(13 * time.Hour)}, false},
		{"finished, no timestamp", &database.Job{Status: database.StatusFinished}, false},
		{"finished, unparseable timestamp", &database.Job{Status: database.StatusFinished, UpdatedAt: "yesterday"}, false},
	} {
		if got := IsArchived(tc.job, cutoff); got != tc.want {
			t.Errorf("%s: IsArchived = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A negative threshold means "never archive" — the single-job form must not
// turn it into a cutoff in the FUTURE, which would archive everything.
//
// Mutant: dropping the hideAgeDays < 0 guard from IsArchivedAt.
func TestIsArchivedAtNeverArchivesOnANegativeThreshold(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	j := &database.Job{Status: database.StatusFinished, UpdatedAt: now.Add(-365 * 24 * time.Hour).Format(time.RFC3339)}
	if IsArchivedAt(j, -1, now) {
		t.Error("a negative threshold must never archive")
	}
	if !IsArchivedAt(j, 0, now) {
		t.Error("a zero threshold archives every Finished job with a past updated_at")
	}
}
```

Create `internal/tui/archive_threshold_test.go`:

```go
package tui

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// The TUI used int(HideFinishedAgeDays), so 0.5 days (12 h — valid config
// the Web UI and the config file both accept) became 0, which its own
// isJobArchived documents as "instantly archive all finished jobs". The list
// was the fourth classifier and it disagreed with the three the shared
// predicate now pins together (CORE-8).
//
// Mutant: int(cfg.Monitors.HideFinishedAgeDays.Days()) in SetConfig — the
// one-minute-old Finished job is archived.
func TestTaskListHonoursAFractionalThreshold(t *testing.T) {
	cfg := config.Defaults()
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}

	a := NewApp()
	a.SetConfig(cfg)
	if got := a.taskList.HideFinishedAgeDays(); got != 0.5 {
		t.Fatalf("threshold = %v, want 0.5", got)
	}

	now := time.Now()
	fresh := &database.Job{
		ID: "a", Title: "A", Status: database.StatusFinished,
		UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339),
	}
	old := &database.Job{
		ID: "b", Title: "B", Status: database.StatusFinished,
		UpdatedAt: now.Add(-13 * time.Hour).Format(time.RFC3339),
	}
	if isJobArchived(fresh, archiveCutoff(0.5, now)) {
		t.Error("a Finished job one minute old must stay active under a 12-hour threshold")
	}
	if !isJobArchived(old, archiveCutoff(0.5, now)) {
		t.Error("a Finished job thirteen hours old must archive under a 12-hour threshold")
	}
}
```

Create `web/tests/archive-boundary.test.mjs`:

```js
// The dashboard's archive boundary is the JS twin of
// internal/jobfilter.ArchiveCutoff / IsArchived (Go) — the same table is
// asserted in internal/jobfilter/archive_test.go
// (TestArchiveCutoffKeepsSubHourFractions, TestIsArchivedRules). The Go side
// used to truncate a fractional threshold to whole hours while this side was
// exact; this suite is the pin that stops the two drifting apart again
// (WEB-8).
//
// Like app.test.mjs, this needs jsdom and skips (never fails) without it.
import { test } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const skip = jsdomMissing || false;

// Same rows as the Go table: a threshold below one hour must produce a
// window of exactly that many seconds, not zero.
const ROWS = [
  { days: 0, ageSecs: 1, archived: true },
  { days: 0, ageSecs: 0, archived: false },
  { days: 0.02, ageSecs: 29 * 60, archived: true },
  { days: 0.02, ageSecs: 20 * 60, archived: false },
  { days: 0.5, ageSecs: 13 * 3600, archived: true },
  { days: 0.5, ageSecs: 11 * 3600, archived: false },
  { days: 1.5, ageSecs: 37 * 3600, archived: true },
  { days: 1.5, ageSecs: 35 * 3600, archived: false },
];

test("the archive boundary keeps sub-hour fractions", { skip }, async () => {
  const { app } = await harness.makeApp();
  for (const row of ROWS) {
    app.hideFinishedAgeDays = row.days;
    app.archivedJobs = [];
    app.jobs = [{
      id: "j1",
      status: "Finished",
      updatedAt: new Date(harness.NOW - row.ageSecs * 1000).toISOString(),
    }];
    app._evaluateArchiveBoundary({ silent: true });
    const moved = app.jobs.length === 0;
    assert.equal(
      moved,
      row.archived,
      `${row.days}d threshold, ${row.ageSecs}s old: archived=${moved}, want ${row.archived}`,
    );
  }
});
```

Check `helpers/app-dom.mjs`'s real `makeApp` return shape before writing (it may return the app directly rather than `{ app }`), and use `harness.NOW` as the frozen clock the harness publishes. If `_evaluateArchiveBoundary` reads `Date.now()` rather than the frozen clock, seed `updatedAt` from `Date.now()` instead and say so in the commit body.

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/jobfilter/ -run TestArchive -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run TestTaskListHonoursAFractional -v
cd web/tests && node --test archive-boundary.test.mjs
```
Expected: the two Go runs fail to compile (`undefined: ArchiveCutoff`, `undefined: archiveCutoff`); the node test should PASS immediately — the JS twin is already exact, and this file is the pin that keeps it that way. Record that it passed on the first run.

- [ ] **Step 3: Write the shared predicate**

Create `internal/jobfilter/archive.go`:

```go
package jobfilter

import (
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// ArchiveCutoff turns a hide_finished_age_days threshold into the instant a
// Finished job's updated_at must be strictly BEFORE to count as archived.
//
// The multiplication order is load-bearing. `time.Duration(days*24) *
// time.Hour` — what the three Go copies this replaces all did — converts the
// float to an INTEGER number of hours first, so every threshold below one
// hour collapses to zero and archives every Finished job immediately.
// config.FlexDuration accepts "0.02d" (≈29 minutes) and the JS twin,
// _evaluateArchiveBoundary in web/public/app.js, computes
// `ageDays * 86400 * 1000` ms exactly — so the server archived a job and
// stopped broadcasting it while the dashboard kept showing it for another 29
// minutes (WEB-8). Scaling the float by the whole day keeps every fraction.
//
// Callers with a negative threshold ("never archive") must not call this:
// the cutoff would land in the FUTURE and archive everything. IsArchivedAt
// carries that guard for single-job callers; loop callers check it once
// before computing the cutoff.
func ArchiveCutoff(now time.Time, hideAgeDays float64) time.Time {
	return now.Add(-time.Duration(hideAgeDays * float64(24*time.Hour)))
}

// IsArchived reports whether j is a Finished row old enough to be archived
// at the given cutoff. Only Finished jobs archive — Cancelled and Error rows
// stay in the active list because they may still need attention — and a
// missing or unparseable updated_at is treated as active.
//
// FOUR callers, deliberately one predicate: the REST archived filter
// (internal/web/routes/jobs.go), the job_update broadcast gate and the
// full-list filter (cmd/moombox), and the TUI's archive bucket
// (internal/tui/task_list.go). An earlier hand-copied version of this logic
// suppressed broadcasts for jobs the list filter still showed.
func IsArchived(j *database.Job, cutoff time.Time) bool {
	if j.Status != database.StatusFinished || j.UpdatedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, j.UpdatedAt)
	if err != nil {
		return false
	}
	return t.Before(cutoff)
}

// IsArchivedAt is the single-job form, carrying the "never archive" guard.
// Semantics, matching the JS twin exactly:
//
//   - hideAgeDays < 0 : never archive
//   - hideAgeDays == 0: archive every Finished job whose updated_at is
//     strictly in the past (the cutoff IS now)
//   - hideAgeDays > 0 : archive Finished jobs older than the threshold
func IsArchivedAt(j *database.Job, hideAgeDays float64, now time.Time) bool {
	if hideAgeDays < 0 {
		return false
	}
	return IsArchived(j, ArchiveCutoff(now, hideAgeDays))
}
```

- [ ] **Step 4: Point the three Go copies at it**

`cmd/moombox/helpers.go` — replace the cutoff line inside `filterJobsByAgeThreshold`:

```go
	cutoff := jobfilter.ArchiveCutoff(time.Now(), hideAgeDays)
```
replace both `jobArchivedAt(j, cutoff)` calls with `jobfilter.IsArchived(j, cutoff)`, and **delete the `jobArchivedAt` function entirely** (its godoc's "Keeping one predicate prevents the two from drifting" point now lives on `jobfilter.IsArchived`). Update `filterJobsByAgeThreshold`'s godoc so the cross-reference names the new home:

```go
// filterJobsByAgeThreshold removes finished jobs older than hideAgeDays from
// the slice using jobfilter.ArchiveCutoff + jobfilter.IsArchived — the one
// predicate the REST archived filter, the WS broadcast gate, the TUI list
// and the Web UI's _evaluateArchiveBoundary all classify by, so a given job
// is archived in all four or in none.
```

`cmd/moombox/monitor_callbacks.go` — the `:1557` hunk ONLY (the `if hideAgeDays >= 0 { … }` block inside the `OnJobChange` WS subscriber). Replace:

```go
			if hideAgeDays >= 0 {
				cutoff := time.Now().Add(-time.Duration(hideAgeDays*24) * time.Hour)
				if jobArchivedAt(job, cutoff) {
					return
				}
			}
```
with:
```go
			if jobfilter.IsArchivedAt(job, hideAgeDays, time.Now()) {
				return
			}
```
Do not touch the `s.wsHub.BroadcastJobUpdate(job)` line below it — that is Arc W's `:1563` hunk.

`internal/web/routes/jobs.go` — replace the body of `filterJobsByAge` from `hideAge := …` to the end of the loop:

```go
	cutoff := jobfilter.ArchiveCutoff(time.Now(), hideAgeDays)

	var result []*database.Job
	for _, j := range jobs {
		// jobfilter.IsArchived is false for non-Finished rows and for an
		// unparseable updated_at, which is exactly the "treat as active"
		// rule this loop applied by hand.
		if jobfilter.IsArchived(j, cutoff) == archived {
			result = append(result, j)
		}
	}
	return result
```
Keep the `hideAgeDays < 0` early return above it exactly as it is. Add the `jobfilter` import; drop `time` only if nothing else in the file uses it.

- [ ] **Step 5: Widen the TUI threshold to a float**

`internal/tui/task_list.go`:

```go
	hideFinishedAgeDays float64 // from config, default 30
```
```go
// SetHideFinishedAgeDays updates the archive threshold. A FLOAT: 0.5 is a
// valid twelve-hour threshold the config file and the Web UI both accept,
// and int() turned it into 0, which this file's own archive rule reads as
// "instantly archive all finished jobs" (CORE-8).
func (m *TaskListModel) SetHideFinishedAgeDays(days float64) {
	m.hideFinishedAgeDays = days
	m.rebuildVirtualList()
}
```
(and the `HideFinishedAgeDays()` accessor Task 5 added returns `float64`).

Replace `isJobArchived` with a cutoff-shaped wrapper over the shared predicate, and add the local cutoff helper the tests use:

```go
// archiveCutoff is the list's one cutoff computation, shared with the REST
// filter, the WS broadcast gate and the Web UI through
// jobfilter.ArchiveCutoff. Returns the zero time for a negative threshold
// ("never archive"), which isJobArchived reads as "nothing is archived".
func archiveCutoff(ageDays float64, now time.Time) time.Time {
	if ageDays < 0 {
		return time.Time{}
	}
	return jobfilter.ArchiveCutoff(now, ageDays)
}

// isJobArchived reports whether a finished job belongs in the archive
// section. Only Finished jobs archive — Cancelled jobs stay in the active
// list since they may need user attention (retry, investigate).
//
// A zero cutoff means "never archive" (see archiveCutoff).
func isJobArchived(j *database.Job, cutoff time.Time) bool {
	if cutoff.IsZero() {
		return false
	}
	return jobfilter.IsArchived(j, cutoff)
}
```

Update the two callers. In `archiveBucketsDirty`:

```go
	now := time.Now()
	cutoff := archiveCutoff(m.hideFinishedAgeDays, now)
	for _, j := range m.jobs {
		if !m.passes(j) {
			continue
		}
		if isJobArchived(j, cutoff) != m.archivedSet[j.ID] {
			return true
		}
	}
```
(its `if m.hideFinishedAgeDays <= 0 { return false }` early return stands — 0 archives on the status change itself and <0 never archives, so neither has time-driven crossings.)

In `rebuildVirtualList`, replace the `ageDays := m.hideFinishedAgeDays` seed with `cutoff := archiveCutoff(m.hideFinishedAgeDays, now)` and every `isJobArchived(j, ageDays, now)` call with `isJobArchived(j, cutoff)`. Do the same in `buildStatusSummary` if it calls the predicate.

`internal/tui/app.go` — `SetConfig`:
```go
	a.taskList.SetHideFinishedAgeDays(cfg.Monitors.HideFinishedAgeDays.Days())
```

`internal/tui/app_keys.go` — the settings-close re-read:
```go
			if a.configStore != nil {
				var days float64
				a.configStore.Read(func(c *config.MoomboxConfig) {
					days = c.Monitors.HideFinishedAgeDays.Days()
				})
				a.taskList.SetHideFinishedAgeDays(days)
			}
```

- [ ] **Step 6: Run everything**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/jobfilter/ ./internal/tui/ ./cmd/moombox/ ./internal/web/routes/ ./internal/docs/
cd web/tests && node --test *.test.mjs
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/jobfilter/ ./internal/tui/ ./cmd/moombox/ ./internal/web/routes/
```
Expected: PASS. `./internal/docs/` is gated because `jobArchivedAt` is deleted — if a doc cites it by symbol, repoint that citation at `internal/jobfilter/archive.go`'s `IsArchived` in this task.

- [ ] **Step 7: Commit**

```bash
git add internal/jobfilter/archive.go internal/jobfilter/archive_test.go internal/tui/task_list.go internal/tui/app.go internal/tui/app_keys.go internal/tui/archive_threshold_test.go cmd/moombox/helpers.go cmd/moombox/monitor_callbacks.go internal/web/routes/jobs.go web/tests/archive-boundary.test.mjs
git commit -m "fix(jobfilter,tui,web,cmd): one archive-age predicate, exact on fractional days

CORE-8 + WEB-8. internal/jobfilter/archive.go carries ArchiveCutoff /
IsArchived / IsArchivedAt, and the four classifiers that disagreed now call
it: the REST archived filter, the job_update broadcast gate, the cmd/moombox
list filter and the TUI's archive bucket.

The three Go copies computed time.Duration(days*24)*time.Hour, which
truncates the float to whole HOURS before multiplying, so any threshold below
one hour (FlexDuration accepts '0.02d') archived every Finished job at once
while the JS twin stayed exact. The TUI was worse: int(HideFinishedAgeDays)
turned 0.5 into 0, which its own rule reads as 'archive everything'. The
threshold is a float64 end to end now.

web/tests/archive-boundary.test.mjs pins the JS twin against the same table
the Go test asserts. cmd/moombox.jobArchivedAt is deleted.

Report row #15.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/jobfilter/archive.go internal/jobfilter/archive_test.go internal/tui/task_list.go internal/tui/app.go internal/tui/app_keys.go internal/tui/archive_threshold_test.go cmd/moombox/helpers.go cmd/moombox/monitor_callbacks.go internal/web/routes/jobs.go web/tests/archive-boundary.test.mjs
```

---

### Task 8: The five FlexDuration settings survive a TUI save

Row: **#15 (CORE-7 half)**.

`feed_check_interval`, `probe_cooldown`, `interruption_timeout`, `incomplete_staging_expiry_days` and `refresh_interval` are loaded with `%.0f` / `int()` and written back as integers on **any** TUI save — so a `"90s"` feed interval (a documented `FlexDuration` string form = 1.5 minutes) becomes 2, and a `0.5` interruption timeout becomes 0, which `config.go` documents as "0 disables". `validateDigitsOnly` additionally makes a fractional value untypeable. `hide_finished_age_days` already has the correct treatment and its comment names the defect verbatim; this applies it to the other five.

**Files:**
- Modify: `internal/tui/settings.go` (load block, the numeric validation table, the apply block, the `twitch_check_interval` help text), `internal/tui/settings_components.go` (the normal-field validator choice), `internal/tui/text_input.go` (`validateDecimal`)
- Test: Create `internal/tui/settings_flex_duration_test.go`

Also lands here: **#88 (CORE-20)**'s TUI half — the `twitch_check_interval` help text says "range: 1-3600" while a 1–4 value is silently cleared to "dynamic" (the config floor is 5), and both interval fields nil an out-of-range value instead of reporting it.

**Interfaces:**
- Consumes: nothing.
- Produces: `func validateDecimal(s string) error` and `var decimalValueFields map[string]bool` in package `tui`.

- [ ] **Step 1: Write the failing test**

Create `internal/tui/settings_flex_duration_test.go`:

```go
package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// Five FlexDuration settings were loaded with %.0f/int() and written back as
// integers on ANY TUI save, so a "90s" feed interval (1.5 minutes) became 2
// and a 0.5 interruption timeout became 0 — which config.go documents as "0
// disables". The loss happened whether or not the operator touched the field
// (CORE-7).
//
// Mutant: restoring fmt.Sprintf("%.0f", …) on the load side, or
// strconv.Atoi on the apply side — the round-tripped value is rounded.
func TestFlexDurationSettingsRoundTripFractions(t *testing.T) {
	cfg := config.Defaults()
	cfg.Monitors.FeedCheckInterval = config.FlexDuration{Value: 1.5}
	cfg.Monitors.ProbeCooldown = config.FlexDuration{Value: 2.5}
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}
	cfg.Downloader.InterruptionTimeout = config.FlexDuration{Value: 0.5}
	cfg.Downloader.IncompleteStagingExpiryDays = config.FlexDuration{Value: 0.5}
	cfg.Cookies.RefreshInterval = config.FlexDuration{Value: 90.5}

	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)

	for _, c := range []struct{ key, want string }{
		{"feed_check_interval", "1.5"},
		{"probe_cooldown", "2.5"},
		{"hide_finished_age_days", "0.5"},
		{"interruption_timeout", "0.5"},
		{"incomplete_staging_expiry_days", "0.5"},
		{"refresh_interval", "90.5"},
	} {
		if got := m.values[c.key]; got != c.want {
			t.Errorf("loaded %s = %q, want %q", c.key, got, c.want)
		}
	}

	m.applyValues()
	if m.status == saveError {
		t.Fatalf("applyValues refused a valid config: %s", m.errorMsg)
	}

	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"feed_check_interval", cfg.Monitors.FeedCheckInterval.Value, 1.5},
		{"probe_cooldown", cfg.Monitors.ProbeCooldown.Value, 2.5},
		{"hide_finished_age_days", cfg.Monitors.HideFinishedAgeDays.Value, 0.5},
		{"interruption_timeout", cfg.Downloader.InterruptionTimeout.Value, 0.5},
		{"incomplete_staging_expiry_days", cfg.Downloader.IncompleteStagingExpiryDays.Value, 0.5},
		{"refresh_interval", cfg.Cookies.RefreshInterval.Value, 90.5},
	} {
		if c.got != c.want {
			t.Errorf("after applyValues %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// validateDigitsOnly made a fractional value untypeable in the first place.
// The six FlexDuration-backed number fields accept one decimal point.
//
// Mutant: wiring validateDigitsOnly to every fieldNumber again — "0.5" is
// rejected.
func TestDecimalFieldsAcceptAPoint(t *testing.T) {
	if err := validateDecimal("0.5"); err != nil {
		t.Errorf("validateDecimal(\"0.5\") = %v, want nil", err)
	}
	if err := validateDecimal("30"); err != nil {
		t.Errorf("validateDecimal(\"30\") = %v, want nil", err)
	}
	if err := validateDecimal("0.5.5"); err == nil {
		t.Error("validateDecimal must reject a second decimal point")
	}
	if err := validateDecimal("1e3"); err == nil {
		t.Error("validateDecimal must reject anything but digits and one point")
	}
	if !decimalValueFields["refresh_interval"] {
		t.Error("refresh_interval is FlexDuration-backed and must accept a decimal point")
	}
	if decimalValueFields["log_max_files"] {
		t.Error("log_max_files is an int field and must keep the digits-only validator")
	}
}

// An out-of-range Twitch/DECAPI interval must be REPORTED, not silently
// cleared to "dynamic", and the help text must name the range config.Validate
// actually enforces (floor 5, not 1) (CORE-20).
//
// Mutant: restoring the silent `else { … = nil }` fallback — status stays
// saveSaved-able and the value vanishes.
func TestOutOfRangeIntervalIsReported(t *testing.T) {
	cfg := config.Defaults()
	store := config.NewStore(cfg, "")
	m := NewSettingsModel()
	m.configStore = store
	m.cfg = cfg
	m.Open(cfg)

	m.values["twitch_check_interval"] = "3"
	m.applyValues()
	if m.status != saveError {
		t.Fatalf("a 3-second Twitch interval must be refused, status = %v", m.status)
	}

	for _, f := range settingsSections {
		for _, fd := range f.fields {
			if fd.key == "twitch_check_interval" && strings.Contains(fd.help, "1-3600") {
				t.Errorf("help text still claims the 1-3600 range config.Validate refuses: %q", fd.help)
			}
		}
	}
}
```

Add `"strings"` to the imports. `settingsSections` and `fieldDef`'s field names (`key`, `help`) were read from `internal/tui/settings.go` on main — confirm the real identifiers and adjust the last loop to them; if the section table is not addressable from a test, assert on `m.fieldHelp("twitch_check_interval")` or whatever accessor exists, and if none does, drop that half of the assertion and instead assert the help string constant directly.

- [ ] **Step 2: Run it red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestFlexDurationSettings|TestDecimalFields|TestOutOfRangeInterval' -v
```
Expected: `undefined: validateDecimal`, `undefined: decimalValueFields`, and the round-trip assertions failing on rounded values.

- [ ] **Step 3: Load with `FormatFloat(-1)`**

In `internal/tui/settings.go`'s load block, replace the five `%.0f` / `strconv.Itoa(int(…))` lines:

```go
	// FormatFloat with -1 precision round-trips fractional values ("0.5"
	// stays "0.5", "30" stays "30") — %.0f and int() silently rounded them
	// away on EVERY save, touched field or not (CORE-7).
	m.values["feed_check_interval"] = strconv.FormatFloat(cfg.Monitors.FeedCheckInterval.Minutes(), 'f', -1, 64)
```
```go
	m.values["hide_finished_age_days"] = strconv.FormatFloat(cfg.Monitors.HideFinishedAgeDays.Days(), 'f', -1, 64)
	m.values["probe_cooldown"] = strconv.FormatFloat(cfg.Monitors.ProbeCooldown.Value, 'f', -1, 64)
```
```go
	m.values["interruption_timeout"] = strconv.FormatFloat(cfg.Downloader.InterruptionTimeout.Minutes(), 'f', -1, 64)
	m.values["incomplete_staging_expiry_days"] = strconv.FormatFloat(cfg.Downloader.IncompleteStagingExpiryDays.Days(), 'f', -1, 64)
```
```go
	m.values["refresh_interval"] = strconv.FormatFloat(cfg.Cookies.RefreshInterval.Minutes(), 'f', -1, 64)
```
(The existing `hide_finished_age_days` line already reads this way — keep its comment, which is the one that named the defect, and extend it to cover all six.)

- [ ] **Step 4: Validate and apply as floats**

In `applyValues`, delete these five rows from the integer range table:
`feed_check_interval`, `probe_cooldown`, `interruption_timeout`, `incomplete_staging_expiry_days`, `refresh_interval`.

Delete the standalone `hideAge, hideAgeErr := strconv.ParseFloat(…)` block and replace it (in the same position, after the integer table and the disk cross-check) with one float table that yields every value the apply block needs:

```go
	// The FlexDuration-backed fields are FLOATS. Fractional days/minutes are
	// valid config the Web UI and the config file both accept, and parsing
	// them as ints here silently rewrote them on any unrelated TUI save
	// (CORE-7). The explicit NaN/Inf rejection matters: ParseFloat accepts
	// "nan" (and TOML 1.0 has nan/inf literals a hand-edited config could
	// carry), and NaN slips through a min/max range check because both
	// comparisons are false.
	flex := make(map[string]float64, 6)
	for _, c := range []struct {
		key, msg string
		min, max float64
	}{
		{"feed_check_interval", "Feed check interval must be 1-1440 minutes", 1, 1440},
		{"probe_cooldown", "Probe cooldown must be >= 0 seconds (0 disables)", 0, math.MaxFloat64},
		{"interruption_timeout", "Interruption resume timeout must be >= 0 minutes (0 disables)", 0, math.MaxFloat64},
		{"incomplete_staging_expiry_days", "Incomplete staging expiry must be >= 0 days (0 preserves forever)", 0, math.MaxFloat64},
		{"refresh_interval", "Cookie refresh interval must be 10-10080 minutes", 10, 10080},
		{"hide_finished_age_days", "Hide finished after must be 0-365 days", 0, 365},
	} {
		v, err := strconv.ParseFloat(strings.TrimSpace(m.values[c.key]), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < c.min || v > c.max {
			m.errorMsg = c.msg
			m.status = saveError
			return
		}
		flex[c.key] = v
	}

	// The two optional interval overrides: empty means "dynamic". A value
	// OUT of range used to be silently nil'ed here too, which read as the
	// operator having asked for dynamic — the Web path returns a field error
	// instead, and the TUI's help text advertised a floor of 1 that
	// config.Validate refuses (CORE-20).
	for _, c := range []struct {
		key, msg string
		min, max int
	}{
		{"decapi_check_interval", "DECAPI check interval must be 15-3600 seconds (or empty for dynamic)", 15, 3600},
		{"twitch_check_interval", "Twitch check interval must be 5-3600 seconds (or empty for dynamic)", 5, 3600},
	} {
		v := strings.TrimSpace(m.values[c.key])
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < c.min || n > c.max {
			m.errorMsg = c.msg
			m.status = saveError
			return
		}
	}
```

Then rewrite the corresponding apply lines:

```go
	m.cfg.Monitors.FeedCheckInterval = config.FlexDuration{Value: flex["feed_check_interval"]}
```
```go
	if v := strings.TrimSpace(m.values["decapi_check_interval"]); v != "" {
		d, _ := strconv.Atoi(v) // range-checked above
		m.cfg.Monitors.DecapiCheckInterval = &d
	} else {
		m.cfg.Monitors.DecapiCheckInterval = nil
	}
	if v := strings.TrimSpace(m.values["twitch_check_interval"]); v != "" {
		tv, _ := strconv.Atoi(v) // range-checked above
		m.cfg.Monitors.TwitchCheckInterval = &tv
	} else {
		m.cfg.Monitors.TwitchCheckInterval = nil
	}
	m.cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: flex["hide_finished_age_days"]}
	m.cfg.Monitors.ProbeCooldown = config.FlexDuration{Value: flex["probe_cooldown"]}
```
```go
	m.cfg.Downloader.InterruptionTimeout = config.FlexDuration{Value: flex["interruption_timeout"]}
	m.cfg.Downloader.IncompleteStagingExpiryDays = config.FlexDuration{Value: flex["incomplete_staging_expiry_days"]}
```
```go
	m.cfg.Cookies.RefreshInterval = config.FlexDuration{Value: flex["refresh_interval"]}
```

Delete the now-unused `feedMin`, `probeCd`, `interruptionMin`, `expiryDays`, `refreshMin` locals and the old `hideAge` local (staticcheck U1000/unused is a hard gate).

Correct the help text:

```go
			{"twitch_check_interval", "Twitch check interval", fieldNumber, nil, "seconds, 5-3600 or empty for dynamic (default: 15)", nil},
```

- [ ] **Step 5: Let the six fields be typed**

In `internal/tui/text_input.go`, beside `validateDigitsOnly`:

```go
// validateDecimal accepts a bare non-negative decimal number — digits and at
// most one ".". The six FlexDuration-backed number fields need it: "0.5" is
// a valid twelve-hour / thirty-second value that the config file and the Web
// UI both accept, and validateDigitsOnly made it untypeable in the terminal
// (CORE-7). Range and NaN/Inf checking stays in applyValues; this is only
// the keystroke filter.
func validateDecimal(s string) error {
	seenDot := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && !seenDot:
			seenDot = true
		default:
			return fmt.Errorf("only digits and one decimal point allowed")
		}
	}
	return nil
}
```

In `internal/tui/settings_components.go`, above `updateTextInputForField`:

```go
// decimalValueFields are the fieldNumber keys backed by config.FlexDuration,
// where a fractional value is legal config the TUI must be able to type and
// must not round away on save. Every other fieldNumber keeps the digits-only
// filter.
var decimalValueFields = map[string]bool{
	"feed_check_interval":            true,
	"probe_cooldown":                 true,
	"interruption_timeout":           true,
	"incomplete_staging_expiry_days": true,
	"refresh_interval":               true,
	"hide_finished_age_days":         true,
}
```

and in the NORMAL-field branch (the one guarded by `if sec.fields != nil`), replace:

```go
			if field.ftype == fieldNumber {
				m.textInput.Validate = validateDigitsOnly
			} else {
				m.textInput.Validate = nil
			}
```
with:
```go
			switch {
			case field.ftype == fieldNumber && decimalValueFields[field.key]:
				m.textInput.Validate = validateDecimal
			case field.ftype == fieldNumber:
				m.textInput.Validate = validateDigitsOnly
			default:
				m.textInput.Validate = nil
			}
```
Leave the CHANNEL-edit branch alone — its field keys are per-channel overrides, none of which is FlexDuration-backed.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestFlexDurationSettings|TestDecimalFields|TestOutOfRangeInterval' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ ./internal/config/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/tui/
```
Expected: PASS. `settings_restart_parity_test.go` must stay green (it keys on field names, not types).

- [ ] **Step 7: Commit**

```bash
git add internal/tui/settings.go internal/tui/settings_components.go internal/tui/text_input.go internal/tui/settings_flex_duration_test.go
git commit -m "fix(tui): FlexDuration settings round-trip fractions; out-of-range intervals are reported

CORE-7: feed_check_interval, probe_cooldown, interruption_timeout,
incomplete_staging_expiry_days and refresh_interval are loaded with
FormatFloat(-1) and parsed with ParseFloat like hide_finished_age_days
already was, so a '90s' feed interval (1.5 minutes) no longer becomes 2 and a
0.5 interruption timeout no longer becomes 0 - which config.go documents as
'0 disables'. The loss used to happen on ANY save, touched field or not. A
new validateDecimal lets the six FlexDuration-backed fields be typed at all.

CORE-20: twitch_check_interval's help text names the 5-3600 range
config.Validate enforces instead of 1-3600, and an out-of-range Twitch or
DECAPI value is refused with a message instead of being silently cleared to
'dynamic'.

Report rows #15, #88 (TUI half).

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/settings.go internal/tui/settings_components.go internal/tui/text_input.go internal/tui/settings_flex_duration_test.go
```

---
### Task 9: Ctrl+C always quits, the task list pages, and `O C` stops over-promising

Rows: **#37 (CORE-5 + O-M)**, **#86 (CORE-16 + O-X)**, **#85 (CORE-15 + O-W)**.

bubbletea v2 delivers Ctrl+C as a plain key (`InterruptMsg` comes only from a real SIGINT), and fourteen overlays intercept every key before the app's handler — only `ffmpegCheck` and `setupWiz` special-case it, so the twelve others swallow the "Ctrl+C Quit immediately" the help promises. The task list's `pgup/pgdown/home/end` bindings are configured on the bubbles `KeyMap` but nothing ever delivers a message to the list, so at 1,000 rows navigation is ↑/↓ and a 3-row wheel. And `O C` reports "Copied: …" unconditionally although `tea.SetClipboard` is OSC 52, which conhost and tmux-without-`set-clipboard` silently drop.

**O-V is REJECTED and is NOT part of this task:** Esc on the FFmpeg-not-found overlay keeps quitting ("the user needs ffmpeg for the muxing part of the process"). Do not touch `ffmpeg_check.go`'s `if key == keyEsc { return "quit" }`.

**Files:**
- Modify: `internal/tui/app_keys.go` (hoist the Ctrl+C check; `handleTaskKey`), `internal/tui/task_list.go` (paging wrappers), `internal/tui/help.go` (two rows), `internal/tui/app_actions.go` (`O C`)
- Create: `internal/tui/openbrowser_windows.go` is NOT the right home — create `internal/tui/clipboard_windows.go` and `internal/tui/clipboard_other.go`
- Test: `internal/tui/help_keys_test.go` (exists — rewrite the Arc 6 assertions); Create `internal/tui/key_priority_test.go`

**Interfaces:**
- Produces: `func (m *TaskListModel) PrevPage() / NextPage() / GoToStart() / GoToEnd()`; `func osClipboardFallback(text string) bool` (build-tagged, `internal/tui/clipboard_windows.go` + `clipboard_other.go`); `func clipboardFeedback(url string, sentViaOSC52 bool) string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/key_priority_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// help.go promises "Ctrl+C  Quit immediately", but bubbletea v2 delivers
// Ctrl+C as a plain key and fourteen overlays intercept every key before the
// app's handler — twelve of them swallowed it. The check is hoisted above
// overlay dispatch (O-M) (CORE-5).
//
// Mutant: moving the keyCtrlC check back below the overlay intercepts — every
// overlay row fails.
func TestCtrlCQuitsThroughEveryOverlay(t *testing.T) {
	open := map[string]func(a *App){
		"help":            func(a *App) { a.help.Toggle() },
		"action menu":     func(a *App) { a.actionMenu.Open(a.buildMenuItems()) },
		"add video":       func(a *App) { a.addVideo.Show() },
		"stats":           func(a *App) { a.statsDlg.Show() },
		"ytdlp":           func(a *App) { a.ytdlpDlg.Show() },
		"client tokens":   func(a *App) { a.clientTokensDlg.Show() },
		"cookie import":   func(a *App) { a.cookieImportDlg.Show() },
		"settings":        func(a *App) { a.settings.Open(a.cfg) },
	}
	for name, show := range open {
		a := NewApp()
		a.width, a.height = 120, 40
		a.recalcLayout()
		show(a)

		_, cmd := a.handleKey(keyCtrlC)
		if cmd == nil {
			t.Errorf("%s overlay: Ctrl+C produced no command, want tea.Quit", name)
			continue
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s overlay: Ctrl+C did not quit", name)
		}
	}
}

// The task list's pgup/pgdown/home/end bindings were configured on the
// bubbles KeyMap and never delivered, so 1,000 rows were navigable only by
// arrow key and a 3-row wheel — dead configuration either way. O-X adds the
// paging rather than deleting the bindings (CORE-16).
//
// Mutant: dropping the four cases from handleTaskKey — End leaves the cursor
// on row 0.
func TestTaskPanelPages(t *testing.T) {
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	jobs := make([]*database.Job, 0, 200)
	for i := range 200 {
		jobs = append(jobs, &database.Job{
			ID: string(rune('a'+i%26)) + strings.Repeat("x", 1+i/26),
			Title: "job", Status: database.StatusLive, Platform: "youtube",
		})
	}
	a.taskList.SetJobs(jobs)
	a.focusedPanel = PanelTasks

	first := a.taskList.list.Index()
	a.handleKey(keyPgDown)
	if a.taskList.list.Index() == first {
		t.Error("PgDn must move the selection off the first page")
	}
	a.handleKey(keyHome)
	if got := a.taskList.list.Index(); got != 0 {
		t.Errorf("Home must select the first row, got index %d", got)
	}
	a.handleKey(keyEnd)
	if got := a.taskList.list.Index(); got != len(a.taskList.list.Items())-1 {
		t.Errorf("End must select the last row, got index %d of %d", got, len(a.taskList.list.Items()))
	}
}

// O C hands the URL to the terminal over OSC 52, which conhost and
// tmux-without-set-clipboard silently drop — so "Copied:" was a promise the
// TUI could not keep. O-W: hedge the wording where OSC 52 is used, and fall
// back to clip.exe on Windows outside Windows Terminal (CORE-15).
//
// Mutant: returning "Copied: " for the OSC 52 case again.
func TestClipboardFeedbackHedgesOSC52(t *testing.T) {
	osc := clipboardFeedback("https://example.test/x", true)
	if !strings.Contains(osc, "OSC 52") {
		t.Errorf("OSC 52 feedback = %q, want it to name the mechanism", osc)
	}
	if strings.HasPrefix(osc, "Copied") {
		t.Errorf("OSC 52 feedback = %q, must not claim a completed copy", osc)
	}
	direct := clipboardFeedback("https://example.test/x", false)
	if !strings.HasPrefix(direct, "Copied") {
		t.Errorf("direct-copy feedback = %q, want it to claim the copy", direct)
	}
}
```

Adjust the overlay-opening calls to the real methods (`Show`, `Open`, `Toggle` were read from main; some dialogs may need arguments). Drop any overlay whose constructor needs backend wiring and note which you dropped — eight of the twelve is the verifier's own sample size.

Rewrite `internal/tui/help_keys_test.go`'s `TestHelpDoesNotClaimTaskPanelPaging` (its name changes with its meaning):

```go
// The help must claim exactly the panels that page. Arc 6 pinned "(Details ·
// Logs)" because the Tasks panel did not page; O-X adds the paging, so the
// claim widens and Home/End become documented rather than forbidden
// (CORE-16).
//
// Mutant: adding the four cases to handleTaskKey without widening the help
// row — the PgUp line still reads "(Details · Logs)" and fails here.
func TestHelpClaimsTaskPanelPaging(t *testing.T) {
	app := NewApp()
	h := NewHelpModel()
	h.SetMenuItems(app.buildMenuItems())
	h.SetSize(100, 200)
	h.Toggle()

	view := stripANSI(h.viewport.View())
	sawPaging, sawHomeEnd := false, false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "PgUp") {
			sawPaging = true
			if !strings.Contains(line, "(Tasks · Details · Logs)") {
				t.Errorf("PgUp/PgDn pages all three panels now: %q", line)
			}
		}
		if strings.Contains(line, "Home/End") {
			sawHomeEnd = true
			if !strings.Contains(line, "(Tasks)") {
				t.Errorf("Home/End is the task list's jump: %q", line)
			}
		}
	}
	if !sawPaging {
		t.Error("the help must document PgUp/PgDn")
	}
	if !sawHomeEnd {
		t.Error("the help must document Home/End now that the task list handles them")
	}
}
```

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestCtrlCQuits|TestTaskPanelPages|TestClipboardFeedback|TestHelpClaimsTaskPanelPaging' -v
```
Expected: `undefined: clipboardFeedback`; the Ctrl+C, paging and help assertions failing.

- [ ] **Step 3: Hoist Ctrl+C (O-M)**

In `internal/tui/app_keys.go`, move the Ctrl+C check to the very top of `handleKey`, above the first overlay intercept (`if a.settings.IsVisible()`):

```go
func (a *App) handleKey(key string) (tea.Model, tea.Cmd) {
	// Ctrl+C ALWAYS quits — checked before ANY overlay intercept (O-M).
	// bubbletea v2 delivers Ctrl+C as a plain key (InterruptMsg comes only
	// from a real SIGINT), and fourteen overlays consume every key before
	// reaching the app's handler, so twelve of them swallowed the "Ctrl+C
	// Quit immediately" help.go promises. No text input in the TUI binds
	// Ctrl+C, so hoisting costs nothing (CORE-5).
	if key == keyCtrlC {
		return a, tea.Quit
	}
```

Delete the now-dead check further down (`// Ctrl+C: immediate quit (bypass chord)` … `if key == keyCtrlC { return a, tea.Quit }`) and the two overlay-local special cases that exist only to work around the old ordering — in `ffmpeg_check.go`'s and `setup_wizard.go`'s key handlers, wherever they test for `keyCtrlC`. **Only remove those two if they do nothing but quit**; if either performs cleanup before quitting, leave it in place (it is unreachable now but harmless) and say so in the task report. Likewise, `log_viewer.go`'s `HandleSearchKey` returns `(nil, false)` for `keyCtrlC` so it "passes through to the app" — that path is now unreachable from `handleKey`, but `routeComponentMsg` runs BEFORE `handleKey`, so keep it exactly as it is.

- [ ] **Step 4: Page the task list (O-X)**

In `internal/tui/task_list.go`, beside `MoveUp`/`MoveDown`:

```go
// PrevPage / NextPage / GoToStart / GoToEnd forward the four paging keys to
// the embedded bubbles list. Its KeyMap has had pgup/pgdown/home/end
// configured since the list was built, but nothing ever delivered a message
// to it — handleTaskKey handled only up/down/enter — so at 1,000 rows
// navigation was one row at a time (CORE-16, O-X). The marquee resets like
// every other selection change.
func (m *TaskListModel) PrevPage() {
	m.list.PrevPage()
	m.resetMarquee()
}

func (m *TaskListModel) NextPage() {
	m.list.NextPage()
	m.resetMarquee()
}

func (m *TaskListModel) GoToStart() {
	m.list.GoToStart()
	m.resetMarquee()
}

func (m *TaskListModel) GoToEnd() {
	m.list.GoToEnd()
	m.resetMarquee()
}
```

In `internal/tui/app_keys.go`'s `handleTaskKey`, add the four cases (each refreshes the details panel like the arrow keys do):

```go
	case keyPgUp:
		a.taskList.PrevPage()
		a.updateSelectedJob()
	case keyPgDown:
		a.taskList.NextPage()
		a.updateSelectedJob()
	case keyHome:
		a.taskList.GoToStart()
		a.updateSelectedJob()
	case keyEnd:
		a.taskList.GoToEnd()
		a.updateSelectedJob()
```

In `internal/tui/help.go`, widen the paging row and add the jump row:

```go
		{"PgUp/PgDn", "Page scroll (Tasks · Details · Logs)"},
		{"Ctrl+U/Ctrl+D", "Half-page scroll (Details · Logs)"},
		{"Home/End", "Jump to first / last task (Tasks)"},
		{"End", "Resume auto-scroll (Logs)"},
```

- [ ] **Step 5: Hedge and back up `O C` (O-W)**

Create `internal/tui/clipboard_windows.go`:

```go
//go:build windows

package tui

import (
	"os"
	"os/exec"
	"strings"
)

// osClipboardFallback copies text with clip.exe when the terminal is NOT
// Windows Terminal. tea.SetClipboard speaks OSC 52, which conhost (and
// legacy consoles generally) silently drop — so on those terminals "Copied"
// was a promise nothing kept (CORE-15, O-W). WT_SESSION is Windows
// Terminal's own marker; when it is set OSC 52 works and this is skipped so
// the terminal's own clipboard integration stays authoritative.
//
// Returns whether the text reached the system clipboard.
func osClipboardFallback(text string) bool {
	if os.Getenv("WT_SESSION") != "" {
		return false
	}
	cmd := exec.Command("clip.exe")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run() == nil
}
```

Create `internal/tui/clipboard_other.go`:

```go
//go:build !windows

package tui

// osClipboardFallback has no non-Windows implementation: there is no
// dependency-free equivalent of clip.exe, and adding an X11/Wayland client
// would pull a CGo-shaped dependency the project refuses. OSC 52 is the
// mechanism everywhere else, and the feedback says so (CORE-15, O-W).
func osClipboardFallback(string) bool { return false }
```

Add the message helper to `internal/tui/app_actions.go`, above `dispatchAction`:

```go
// clipboardFeedback words the O C result honestly. tea.SetClipboard is OSC
// 52 — the terminal may accept it, ignore it, or be a multiplexer that needs
// `set-clipboard on` — so the TUI cannot claim a completed copy on that
// path, only that it handed the URL over (CORE-15, O-W).
func clipboardFeedback(url string, sentViaOSC52 bool) string {
	if sentViaOSC52 {
		return "Sent to terminal clipboard (OSC 52): " + url
	}
	return "Copied: " + url
}
```

Rewrite the `O C` case:

```go
	case "O C":
		if job != nil {
			if url := streamURL(job); url != "" {
				if osClipboardFallback(url) {
					a.setFeedback(clipboardFeedback(url, false))
					return a, nil
				}
				a.setFeedback(clipboardFeedback(url, true))
				return a, tea.SetClipboard(url)
			}
			a.setFeedback("No URL to copy")
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run 'TestCtrlCQuits|TestTaskPanelPages|TestClipboardFeedback|TestHelpClaimsTaskPanelPaging' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/tui/
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/tui/
```
Expected: PASS on both GOOS (the build-tagged pair must compile for each). `help_coverage_test.go` must stay green.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/app_keys.go internal/tui/task_list.go internal/tui/help.go internal/tui/help_keys_test.go internal/tui/app_actions.go internal/tui/clipboard_windows.go internal/tui/clipboard_other.go internal/tui/key_priority_test.go
git commit -m "fix(tui): Ctrl+C quits through every overlay; the task list pages; O C stops over-promising

CORE-5 / O-M: bubbletea v2 delivers Ctrl+C as a plain key and fourteen
overlays intercept every key before the app handler, so twelve of them
swallowed the 'Quit immediately' the help promises. The check is hoisted to
the top of handleKey. No text input binds Ctrl+C.

CORE-16 / O-X: pgup/pgdown/home/end were configured on the bubbles KeyMap and
never delivered. handleTaskKey now forwards them to the list's own
PrevPage/NextPage/GoToStart/GoToEnd, and the help rows say so - the Arc 6
test that forbade a Tasks-panel paging claim is rewritten to require it.

CORE-15 / O-W: tea.SetClipboard is OSC 52, which conhost and tmux without
set-clipboard drop silently. On Windows outside Windows Terminal (WT_SESSION
unset) the URL goes through clip.exe and the feedback says 'Copied'; on the
OSC 52 path it says 'Sent to terminal clipboard (OSC 52)'.

O-V is REJECTED: Esc on the FFmpeg overlay still quits. ffmpeg_check.go's Esc
handling is untouched.

Report rows #37, #86, #85.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/app_keys.go internal/tui/task_list.go internal/tui/help.go internal/tui/help_keys_test.go internal/tui/app_actions.go internal/tui/clipboard_windows.go internal/tui/clipboard_other.go internal/tui/key_priority_test.go
```

---

### Task 10: The logger stops storming, formats once, and routes to live jobs only

Rows: **#36 (CORE-3)**, **#73 (CORE-22)**, **#72 (CORE-12)**, **#99 (TOOL-15)**.

On Windows, while any process holds a rotated log file open (tail, editor, AV), **every** log write past the cap re-attempts the rotation, both renames fail, two WARN diagnostics go into the ring buffer and every subscriber, and the live file keeps growing — reproduced at 94 diagnostics per 50 writes and 7.4× the cap. Separately, every line is formatted twice (slog's `TextHandler` for file/stdout, `formatLogLine` for ring/subscribers) with **independently sampled timestamps**, so the file line and the UI line can show different seconds. And `RouteLogToJobs` scans every job the database has ever held — 47 µs per line at 5,000 jobs, under the write lock.

**Files:**
- Modify: `internal/logger/logger.go`
- Modify: `internal/logger/logger_stress_test.go` (TOOL-15: the empty `if runtime.GOOS == "windows" { }`)
- Modify: `internal/database/database.go` (one new map field), `internal/database/database_jobs.go` (`RouteLogToJobs`, `TrackJobForLogs`, `ClearJobLogs`, `PruneJobLogs`)
- Modify: `cmd/moombox/monitor_callbacks.go` (the boot tracking loop ~:1530-1533, the `OnJobChange` subscriber, the `OnJobsChange` tracking loop ~:1605-1612)
- Test: `internal/logger/logger_test.go` (append); Create `internal/database/log_routing_test.go`

**Interfaces:**
- Produces: `func (db *Database) UntrackJobForLogs(jobID string)`; `Database.logRouted map[string]struct{}`.
- Removes: `Logger.slog` (the `*slog.Logger` field) — replaced by a direct level check. **Gate `go test ./internal/docs/`** in case a doc cites it.

- [ ] **Step 1: Write the failing tests**

Append to `internal/logger/logger_test.go`:

```go
// A rotation that cannot rename must back off. On Windows any process
// holding <log>.1 open makes both renames fail, and because rotate() is
// re-attempted after EVERY write past the cap, the failure repeated per
// line: ~2 WARN diagnostics per log line into the ring buffer, every WS
// subscriber and the TUI log panel, while the live file grew unbounded
// (CORE-3; reproduced at 94 diagnostics per 50 writes).
//
// The fixture is portable: renaming a file onto a NON-EMPTY directory fails
// on Windows and POSIX alike, and maxFiles=1 keeps the shift loop out of it.
//
// Mutant: calling rotate() unconditionally from Write again — dozens of
// diagnostics instead of one.
func TestRotationBacksOffAfterAFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "backoff.log")

	blocker := logPath + ".1"
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := New(logPath, "DEBUG", minLogRotationSize, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	sub := l.Subscribe()
	t.Cleanup(func() { l.Unsubscribe(sub) })

	done := make(chan int, 1)
	go func() {
		n := 0
		for line := range sub {
			if strings.Contains(line, "rotation") {
				n++
			}
		}
		done <- n
	}()

	for i := range 60 {
		l.Info("a line long enough to push the file past the rotation floor quickly", "i", i, "pad", strings.Repeat("p", 200))
	}
	l.Unsubscribe(sub)
	diagnostics := <-done

	if diagnostics > 2 {
		t.Errorf("%d rotation diagnostics for 60 writes; the back-off must emit one per streak", diagnostics)
	}
}

// The file line and the UI line are the SAME string: one format pass, one
// clock sample. Before this, slog's TextHandler formatted its own copy from
// the record's time while formatLogLine sampled time.Now() again, so the two
// could disagree about the second (CORE-22).
//
// Mutant: reinstating a second formatLogLine call with its own time.Now() —
// the file and the ring can differ.
func TestFileAndRingLinesAreTheSameFormat(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "once.log")
	l, err := New(logPath, "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Info("single format", "key", "value")

	lines := l.GetRecentLines()
	if len(lines) != 1 {
		t.Fatalf("ring holds %d lines, want 1", len(lines))
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), lines[0]) {
		t.Errorf("the file must contain the ring's line verbatim.\nring: %q\nfile: %q", lines[0], data)
	}
}
```

Create `internal/database/log_routing_test.go`:

```go
package database

import (
	"strings"
	"testing"
)

// Every job the database has ever held was tracked for log routing, so each
// log line did an O(jobs) substring scan under the write lock — 47 µs per
// line at 5,000 jobs, with years-old Finished rows making up the bulk
// (CORE-12). Tracking follows only non-terminal jobs now; the BUFFER stays
// readable, because the operator reads a failed job's log right after it
// fails.
//
// Mutant: implementing UntrackJobForLogs as ClearJobLogs — the preserved
// lines assertion fails.
func TestUntrackStopsRoutingButKeepsTheBuffer(t *testing.T) {
	db := &Database{jobLogs: map[string][]string{}, logRouted: map[string]struct{}{}}

	db.TrackJobForLogs("job-1")
	db.RouteLogToJobs("2026-09-17 12:00:00 INFO job-1 started")
	if got := db.GetJobLogs("job-1"); len(got) != 1 {
		t.Fatalf("tracked job holds %d lines, want 1", len(got))
	}

	db.UntrackJobForLogs("job-1")
	db.RouteLogToJobs("2026-09-17 12:00:01 INFO job-1 finished")

	got := db.GetJobLogs("job-1")
	if len(got) != 1 {
		t.Errorf("an untracked job must stop receiving lines, holds %d", len(got))
	}
	if len(got) == 1 && !strings.Contains(got[0], "started") {
		t.Errorf("the buffer must survive untracking, holds %q", got[0])
	}

	db.ClearJobLogs("job-1")
	if got := db.GetJobLogs("job-1"); got != nil {
		t.Errorf("ClearJobLogs must drop the buffer, holds %v", got)
	}
}
```

If `Database` cannot be constructed as a bare struct literal from the test (unexported fields in another file are fine — same package), use whatever the package's other tests do.

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/logger/ -run 'TestRotationBacksOff|TestFileAndRingLines' -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/database/ -run TestUntrackStopsRouting -v
```
Expected: the rotation test failing on the diagnostic count; the format test failing on the file/ring mismatch; the routing test failing to compile (`undefined: logRouted`, `UntrackJobForLogs`).

- [ ] **Step 3: Back off after a failed rotation**

In `internal/logger/logger.go`, add to the `Logger` struct beside the rotation fields:

```go
	// rotateFailing / rotateBackoffUntil back off after a rotation whose
	// renames failed. On Windows any process holding <log>.1 open (tail,
	// editor, AV) makes both renames fail, and rotate() is re-attempted
	// after EVERY write past the cap — so the failure repeated per log line:
	// two WARN diagnostics into the ring buffer, every WS subscriber and the
	// TUI log panel, per line, while the live file grew unbounded (CORE-3,
	// reproduced at 7.4x the cap). Guarded by fileMu like every other
	// rotation field.
	rotateFailing      bool
	rotateBackoffUntil time.Time
```

Add the constant beside `minLogRotationSize`:

```go
// rotateBackoff is how long a failed rotation waits before the next attempt.
// While it runs, the live log file is allowed to grow past
// log_max_file_size: an oversize file for up to a minute is strictly better
// than two diagnostics per log line for as long as the holder keeps the
// handle (CORE-3).
const rotateBackoff = 60 * time.Second
```

In `Write`, replace `l.rotate()` with the gate:

```go
	// Check if rotation is needed
	if l.currentSize >= int64(l.maxSize) {
		l.rotateIfDue()
	}
```

and add:

```go
// rotateIfDue rotates unless a previous attempt failed recently.
func (l *Logger) rotateIfDue() {
	if l.rotateFailing && time.Now().Before(l.rotateBackoffUntil) {
		return
	}
	l.rotate()
}
```

In `rotate()`, track the outcome and emit at most one diagnostic per streak. The three rename/remove `diagf` calls become conditional on `!l.rotateFailing`, and the function records the verdict:

```go
func (l *Logger) rotate() {
	oldFile := l.file
	l.file = nil

	if oldFile != nil {
		oldFile.Close()
	}

	// firstOfStreak: only the FIRST failed attempt of a streak reports. The
	// rest are the same holder, the same handle, the same message.
	firstOfStreak := !l.rotateFailing
	rotated := true

	// Shift existing log files
	for i := l.maxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.filePath, i)
		dst := fmt.Sprintf("%s.%d", l.filePath, i+1)
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			rotated = false
			if firstOfStreak {
				l.diagf("logger: rotation rename %s -> %s failed: %v (retrying no sooner than %s)", src, dst, err, rotateBackoff)
			}
		}
	}

	// Rename current to .1
	if err := os.Rename(l.filePath, l.filePath+".1"); err != nil && !os.IsNotExist(err) {
		rotated = false
		if firstOfStreak {
			l.diagf("logger: rotation rename current log failed: %v (retrying no sooner than %s)", err, rotateBackoff)
		}
	}

	// Remove excess files. Loop upward until the first gap so a runtime
	// DECREASE of log_max_files cleans every stale higher-numbered file —
	// removing only .{maxFiles+1} left .{maxFiles+2}.. behind forever.
	for i := l.maxFiles + 1; ; i++ {
		excess := fmt.Sprintf("%s.%d", l.filePath, i)
		if err := os.Remove(excess); err != nil {
			if !os.IsNotExist(err) && firstOfStreak {
				l.diagf("logger: rotation remove excess file failed: %v", err)
			}
			break
		}
	}

	if rotated {
		l.rotateFailing = false
		l.rotateBackoffUntil = time.Time{}
	} else {
		l.rotateFailing = true
		l.rotateBackoffUntil = time.Now().Add(rotateBackoff)
	}

	// Open fresh file — if this fails, surface it so we don't silently lose
	// all logging. NOT streak-suppressed: a reopen failure is a different
	// fault from a rename failure and loses every subsequent line.
	if err := l.openFile(); err != nil {
		l.diagf("logger: rotation failed to open new log file: %v", err)
	}
}
```

- [ ] **Step 4: Format each line once**

Replace the handler construction. Delete the `opts := &slog.HandlerOptions{…}` block, the `multi := io.MultiWriter(writers...)` line, the `writers` slice, and `l.slog = slog.New(slog.NewTextHandler(multi, opts))`. Remove the `slog` field from the struct and the now-unused `io` import if nothing else needs it. Keep `l.stdout` / `l.stderrGate` exactly as they are.

Replace `Logger.log` with:

```go
func (l *Logger) log(level slog.Level, msg string, args ...any) {
	if l.closed.Load() {
		return
	}

	// Check level before doing any work.
	if level < l.level.Level() {
		return
	}

	// ONE format pass, ONE clock sample. The file/stdout line and the
	// ring-buffer/subscriber line are now the SAME string, so they can no
	// longer disagree about the second — slog's TextHandler used to format
	// its own copy from the record's time while formatLogLine sampled
	// time.Now() again (CORE-22). The file format changes with it, from
	// `time=… level=INFO msg="…" k=v` to the shape the TUI log panel and the
	// dashboard's log stream have always shown; nothing parses the file.
	line := formatLogLine(time.Now(), level, msg, args...)
	l.writeLine(line)
	l.addToRingBuffer(line)
	l.broadcast(line)
}

// writeLine sends one already-formatted line to stdout (when the TUI is not
// holding the terminal) and to the rotating file.
func (l *Logger) writeLine(line string) {
	b := make([]byte, 0, len(line)+1)
	b = append(b, line...)
	b = append(b, '\n')
	if l.stdout != nil {
		l.stdout.Write(b)
	}
	// An empty filePath is the deliberate "stdout + ring buffer only" mode
	// (tests, the launcher's pre-init phase) — Write would try to open "".
	if l.filePath != "" {
		l.Write(b)
	}
}
```

Change `formatLogLine` to take the sampled instant:

```go
func formatLogLine(now time.Time, level slog.Level, msg string, args ...any) string {
	sb := logLineBuilderPool.Get().(*strings.Builder)
	defer func() {
		sb.Reset()
		logLineBuilderPool.Put(sb)
	}()

	ts := now.Format("2006-01-02 15:04:05")
```
(the rest of the body is unchanged). Update every other caller in the package to pass `time.Now()`.

Change `defaultSlogBridge.Enabled`:

```go
func (b defaultSlogBridge) Enabled(_ context.Context, level slog.Level) bool {
	return level >= b.l.level.Level()
}
```

Everything else about `SetLevel`, `Subscribe`, `broadcast`, `diagf` and `Close` is untouched. `TestNewLogger`'s three assertions ("test message", "debug message", "key=value") all still hold against the new file format — verify rather than assume.

- [ ] **Step 5: Route logs to live jobs only**

In `internal/database/database.go`, beside the `jobLogs` field:

```go
	// logRouted is the SET of job IDs RouteLogToJobs scans. It is deliberately
	// not the same map as jobLogs: tracking follows only non-terminal jobs
	// (a years-old Finished row cost 47 µs of substring scanning per log
	// line at 5,000 jobs, under the write lock — CORE-12), while the BUFFER
	// must outlive the terminal transition because the operator reads a
	// failed job's log right after it fails. Guarded by jobLogsMu.
	logRouted map[string]struct{}
```
Initialise it wherever `jobLogs` is initialised (`make(map[string]struct{})`).

In `internal/database/database_jobs.go`:

```go
// RouteLogToJobs checks if a log line contains any TRACKED job ID and routes
// it to the corresponding per-job log buffer. Only non-terminal jobs are
// tracked (see TrackJobForLogs / UntrackJobForLogs). Each log line belongs to
// at most one job.
func (db *Database) RouteLogToJobs(line string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()

	for jobID := range db.logRouted {
		if strings.Contains(line, jobID) {
			db.jobLogs[jobID] = capLogLines(append(db.jobLogs[jobID], line))
			return // Each log line belongs to at most one job
		}
	}
}

// TrackJobForLogs starts routing log lines to a job's buffer. Callers gate
// this on the job being non-terminal.
func (db *Database) TrackJobForLogs(jobID string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	db.logRouted[jobID] = struct{}{}
	if _, ok := db.jobLogs[jobID]; !ok {
		db.jobLogs[jobID] = nil
	}
}

// UntrackJobForLogs stops routing to a job while KEEPING its buffer: a job
// that just failed is exactly the one whose log the operator opens next
// (CORE-12). ClearJobLogs is what drops the buffer.
func (db *Database) UntrackJobForLogs(jobID string) {
	db.jobLogsMu.Lock()
	defer db.jobLogsMu.Unlock()
	delete(db.logRouted, jobID)
}
```

`ClearJobLogs` deletes from BOTH maps; `PruneJobLogs` prunes both against `activeIDs`.

In `cmd/moombox/monitor_callbacks.go`:

1. the boot loop:
```go
	// Initialize per-job log tracking with existing jobs (matches TS
	// knownJobIds). Terminal rows are skipped: their in-memory buffers are
	// empty at boot anyway, and tracking them made every log line scan them
	// (CORE-12).
	if existingJobs, err := s.db.GetAllJobs(); err == nil {
		for _, j := range existingJobs {
			if !j.IsTerminal() {
				s.db.TrackJobForLogs(j.ID)
			}
		}
	}
```
2. in the `OnJobChange` WS subscriber, before the archive gate:
```go
		// Stop routing once the job reaches a terminal state; the buffer
		// stays readable (CORE-12).
		if job.IsTerminal() {
			s.db.UntrackJobForLogs(job.ID)
		}
```
3. in the `OnJobsChange` tracking loop:
```go
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
			if j.IsTerminal() {
				s.db.UntrackJobForLogs(j.ID)
			} else {
				s.db.TrackJobForLogs(j.ID)
			}
		}
```
Confirm `(*database.Job).IsTerminal()` exists and covers Finished/Cancelled/Error; if it does not, write the predicate in `monitor_callbacks.go` using `database.Status*` constants and name it `jobRoutingDone`.

- [ ] **Step 6: Delete the empty platform guard (TOOL-15)**

In `internal/logger/logger_stress_test.go`, replace

```go
	if runtime.GOOS == "windows" {
		// Windows handles open-file rename differently; on POSIX the
		// rename always succeeds even if a reader holds the file. The
		// assertion below ("post-rotate writes still land") is the
		// invariant we care about regardless of which branch fires.
	}
```
with the comment alone, moved above the test body's first statement:

```go
	// Windows handles open-file rename differently; on POSIX the rename
	// always succeeds even if a reader holds the file. The assertion below
	// ("post-rotate writes still land") is the invariant we care about
	// regardless of which branch fires.
```
and drop the `runtime` import if nothing else in the file uses it.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/logger/ -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/database/ ./cmd/moombox/ ./internal/docs/
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/logger/ ./internal/database/ ./cmd/moombox/
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/logger/ ./internal/database/ ./cmd/moombox/
```
Expected: PASS. `./internal/docs/` is gated because the `Logger.slog` field is removed.

- [ ] **Step 8: Commit**

```bash
git add internal/logger/logger.go internal/logger/logger_test.go internal/logger/logger_stress_test.go internal/database/database.go internal/database/database_jobs.go internal/database/log_routing_test.go cmd/moombox/monitor_callbacks.go
git commit -m "fix(logger,database): back off failed rotations, format once, route logs to live jobs only

CORE-3: a rotation whose renames fail sets a 60 s back-off and reports once
per streak. Before this, rotate() was re-attempted after every write past the
cap, so a held <log>.1 produced ~2 WARN diagnostics per log line into the ring
buffer, every WS subscriber and the TUI log panel while the live file grew
unbounded (reproduced at 7.4x the cap).

CORE-22: one format pass and one clock sample per line. slog's TextHandler is
gone; the level gate reads the LevelVar directly and formatLogLine takes the
instant. The file line and the UI line are now the same string, so they can no
longer disagree about the second. The FILE FORMAT changes to the shape the TUI
and the dashboard already show; nothing parses it.

CORE-12: log routing scans a separate tracked-ID set that follows only
non-terminal jobs. The per-job BUFFER outlives the terminal transition,
because a failed job's log is the one the operator opens next.

TOOL-15: the empty `if runtime.GOOS == \"windows\" {}` in the stress test is
replaced by its comment.

Report rows #36, #73, #72, #99.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/logger/logger.go internal/logger/logger_test.go internal/logger/logger_stress_test.go internal/database/database.go internal/database/database_jobs.go internal/database/log_routing_test.go cmd/moombox/monitor_callbacks.go
```

---

### Task 11: The action menu stops touching the disk, `-config` is authoritative, and the Web ranges match `config.Validate`

Rows: **#38 (CORE-9)**, **#87 (CORE-17 + O-Y)**, **#88 (CORE-21 half)**, **#90 (CORE-24)**, **#79 (TOOL-18)**.

Five small, independent corrections that share a review surface (the TUI's read paths and the config surface).

**Files:**
- Modify: `internal/tui/action_menu.go`, `internal/tui/app_actions.go` (two `StatusFilter` entries)
- Modify: `internal/config/config.go` (`Load`), `SPEC.md` (the Config search-order sentence, ~:665)
- Modify: `internal/web/routes/config_routes.go` (TWO validator-range hunks ONLY)
- Modify: `cmd/moombox/tui_wiring.go` (four unlocked config reads)
- Modify: `internal/tui/app_commands.go` (`apiClient`)
- Test: Create `internal/tui/action_menu_probe_test.go`; append to `internal/config/config_test.go`; append to `internal/web/routes/config_routes_test.go`

**Interfaces:**
- Produces: `ActionMenuItem.StatusFilter func(*database.Job) bool`; `func (s *runState) httpsEnabled() bool` and `func (s *runState) ffmpegPathOrDefault() string` in `cmd/moombox/tui_wiring.go`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/action_menu_probe_test.go`:

```go
package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Opening the action menu evaluated every NeedsJob filter over every job, and
// A R's / A M's filters call HasStagingFiles / HasSegmentFiles — os.ReadDir
// per eligible job, on the bubbletea event goroutine. 1,000 directory reads
// at 1,000 jobs, seconds of freeze on a NAS staging dir (CORE-9). The menu's
// "no jobs" is a STATUS question; the disk is consulted when the action is
// actually chosen.
//
// Mutant: dropping StatusFilter from the noJobs computation in Open() — the
// probe counters are non-zero.
func TestActionMenuOpenTouchesNoDisk(t *testing.T) {
	a := NewApp()
	staging, segments := 0, 0
	a.HasStagingFiles = func(string) bool { staging++; return true }
	a.HasSegmentFiles = func(string) bool { segments++; return true }

	jobs := make([]*database.Job, 0, 300)
	for i := range 300 {
		st := database.StatusError
		if i%3 == 0 {
			st = database.StatusCancelled
		}
		jobs = append(jobs, &database.Job{
			ID: "j" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Title: "t", Status: st, Platform: "youtube",
		})
	}
	a.actionMenu.SetJobs(jobs)
	a.actionMenu.Open(a.buildMenuItems())

	if staging != 0 || segments != 0 {
		t.Errorf("opening the menu probed the disk: HasStagingFiles=%d HasSegmentFiles=%d, want 0/0", staging, segments)
	}
}

// The status-only twin must still disable an action that genuinely has no
// candidate — otherwise the cheap path turns every entry green.
//
// Mutant: making StatusFilter always return true.
func TestActionMenuStillDisablesImpossibleActions(t *testing.T) {
	a := NewApp()
	a.HasStagingFiles = func(string) bool { return true }
	a.HasSegmentFiles = func(string) bool { return true }
	a.actionMenu.SetJobs([]*database.Job{
		{ID: "a", Title: "t", Status: database.StatusLive, Platform: "youtube"},
	})
	a.actionMenu.Open(a.buildMenuItems())

	found := false
	for _, it := range a.actionMenu.mainList.Items() {
		mi, ok := it.(menuActionItem)
		if !ok || mi.action == nil || mi.action.Chord != "A R" {
			continue
		}
		found = true
		if !mi.noJobs {
			t.Error("A R must be disabled when no job has a resumable status")
		}
	}
	if !found {
		t.Fatal("A R was not in the menu")
	}
}
```

Append to `internal/config/config_test.go`:

```go
// O-Y: an explicit -config path is AUTHORITATIVE. Load's comment claimed the
// cwd/./config/~ search happened only when customPath was empty, but the code
// appended the search paths unconditionally — so `-config /not/yet/there`
// silently adopted ~/.config/moombox/config.toml when one existed (CORE-17).
//
// Mutant: appending the search paths after customPath again — the home file
// is adopted and LoadedFrom names it.
func TestExplicitConfigPathIsAuthoritative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	fallback := filepath.Join(home, ".config", "moombox")
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fallback, "config.toml"), []byte("[network]\nport = 9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	asked := filepath.Join(t.TempDir(), "does-not-exist.toml")
	cfg, err := Load(asked)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoadedFrom != "" {
		t.Errorf("LoadedFrom = %q, want \"\" — an explicit path that does not exist must not fall through", cfg.LoadedFrom)
	}
	if cfg.Network.Port == 9999 {
		t.Error("Load adopted the home config despite an explicit -config path")
	}

	// The explicit path IS honoured when it exists.
	present := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(present, []byte("[network]\nport = 7411\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(present)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Network.Port != 7411 {
		t.Errorf("explicit config gave port %d, want 7411", cfg2.Network.Port)
	}
}
```

Append to `internal/web/routes/config_routes_test.go`:

```go
// Two Web validator ranges were wider than config.Validate, so a value the
// API's own message called valid was refused inside config.Save as an opaque
// 500 "failed to save config" (CORE-21).
//
// Mutant: restoring 1..100 / no-maximum — the 100 and 20000 rows report no
// field error.
func TestWebValidatorRangesMatchConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates map[string]any
		field   string
	}{
		{"disk warn 100", map[string]any{"disk": map[string]any{"disk_warn_percent": float64(100)}}, "disk.disk_warn_percent"},
		{"disk critical 100", map[string]any{"disk": map[string]any{"disk_critical_percent": float64(100)}}, "disk.disk_critical_percent"},
		{"refresh 20000", map[string]any{"cookies": map[string]any{"refresh_interval": float64(20000)}}, "cookies.refresh_interval"},
	} {
		errs := validateConfigUpdates(tc.updates)
		if _, ok := errs[tc.field]; !ok {
			t.Errorf("%s: want a field error on %s, got %v", tc.name, tc.field, errs)
		}
	}
}
```

Confirm `validateConfigUpdates`'s real signature (it may take more arguments) and adapt.

- [ ] **Step 2: Run them red**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run TestActionMenu -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/config/ -run TestExplicitConfigPath -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/routes/ -run TestWebValidatorRanges -v
```
Expected: all three fail.

- [ ] **Step 3: Status-only menu, disk on selection (CORE-9)**

In `internal/tui/action_menu.go`, add the field to `ActionMenuItem`:

```go
	// StatusFilter is a CHEAP, status-only twin of JobFilter, used only to
	// decide whether the menu shows this entry as "no jobs". JobFilter may
	// touch the disk (A R's HasStagingFiles, A M's HasSegmentFiles are
	// os.ReadDir per job), and running every filter over every job on the
	// bubbletea goroutine froze the M key for seconds against a NAS staging
	// directory (CORE-9). nil means JobFilter is already cheap and is used.
	StatusFilter   func(*database.Job) bool
```

and in `Open`'s list build:

```go
		noJobs := false
		if items[i].NeedsJob {
			// Status only: the disk is consulted when the ACTION is chosen
			// and its job selector is built, not when the menu opens.
			filter := items[i].JobFilter
			if items[i].StatusFilter != nil {
				filter = items[i].StatusFilter
			}
			count := 0
			for _, j := range m.jobs {
				if filter == nil || filter(j) {
					count++
				}
			}
			noJobs = count == 0
		}
```

In `internal/tui/app_actions.go`, add the two status-only twins. For `A R`:

```go
			StatusFilter: func(j *database.Job) bool {
				return (j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies || (j.Status == database.StatusFinished && j.IncompleteTail)) &&
					j.Platform == "youtube"
			},
```
For `A M`:
```go
			StatusFilter: func(j *database.Job) bool {
				return j.Status == database.StatusCancelled || j.Status == database.StatusError
			},
```
Leave both `JobFilter`s exactly as they are — they are what the job selector and `dispatchAction` still use, and that IS the "probe on selection" step.

- [ ] **Step 4: Honour `-config` exclusively (O-Y)**

In `internal/config/config.go`, rewrite `Load`'s head:

```go
// Load reads configuration from a TOML file.
//
// An explicit customPath (the -config flag) is AUTHORITATIVE: it is the only
// file considered, and when it does not exist Load returns defaults rather
// than silently adopting a config from somewhere else. Before O-Y the search
// paths were appended unconditionally, so `-config /not/yet/there` adopted
// ~/.config/moombox/config.toml when one happened to exist — the operator
// asked for one file and got another (CORE-17).
//
// When customPath is empty the search runs: cwd -> ./config/ ->
// ~/.config/moombox/. SPEC.md documents this order.
//
// The file that answers is recorded in cfg.LoadedFrom — callers must save
// back to it rather than to the path they asked for (see the field's doc).
// When no file is found, LoadedFrom is "" and the caller's own path stays the
// target.
func Load(customPath string) (*MoomboxConfig, error) {
	if customPath != "" {
		if _, err := os.Stat(customPath); err == nil {
			return loadFromFile(customPath)
		}
		// Named but absent — the caller's path stays the save target
		// (storePathFor in cmd/moombox/helpers.go keeps it when LoadedFrom
		// is empty), so a fresh install writes exactly where it was told.
		return Defaults(), nil
	}

	paths := []string{}
	cwd, err := os.Getwd()
	…
```
(the remainder of the function is unchanged, minus the now-dead `if customPath != "" { paths = append(paths, customPath) }`).

In `SPEC.md`, replace the Config search-order sentence:

```
TOML format parsed by `BurntSushi/toml`. An explicit `-config` path is authoritative — it is the only file considered, and a path that does not yet exist falls back to defaults rather than to another file. Without the flag the search order is `./config.toml`, `./config/config.toml`, `~/.config/moombox/config.toml`; if none exists, defaults are used.
```

- [ ] **Step 5: Align the two Web ranges (CORE-21)**

In `internal/web/routes/config_routes.go`, the disk hunk:

```go
		if warnOK {
			// 1..99 mirrors config.validateOrNormalize — a value this
			// validator called valid was then refused inside config.Save as
			// an opaque 500 "failed to save config" (CORE-21).
			if warnPct < 1 || warnPct > 99 {
				errs["disk.disk_warn_percent"] = "disk_warn_percent must be between 1 and 99"
			}
		}
		if critOK {
			if critPct < 1 || critPct > 99 {
				errs["disk.disk_critical_percent"] = "disk_critical_percent must be between 1 and 99"
			}
		}
```

and the cookies hunk:

```go
		if v, ok := ck["refresh_interval"].(float64); ok {
			// 10..10080 mirrors config.validateOrNormalize (CORE-21).
			if v < 10 || v > 10080 {
				errs["cookies.refresh_interval"] = "refresh_interval must be between 10 and 10080"
			}
		}
```

Touch nothing else in this file — the restart-required lists in it belong to Arc W.

- [ ] **Step 6: Read config through the store (CORE-24)**

In `cmd/moombox/tui_wiring.go`, add two helpers at the bottom of the file:

```go
// httpsEnabled and ffmpegPathOrDefault read through the config store.
// Closures that outlive wiring must never touch s.cfg's fields directly:
// PUT /api/config assigns *cfg = cfgCopy under the store's lock, so an
// unlocked field read races a whole-struct replacement (CORE-24).
func (s *runState) httpsEnabled() bool {
	enabled := false
	s.configStore.Read(func(c *config.MoomboxConfig) { enabled = c.Network.HTTPSEnabled })
	return enabled
}

// ffmpegPathOrDefault returns the configured FFmpeg path, or "ffmpeg" for
// the PATH lookup when none is set — the fallback every caller applied
// itself.
func (s *runState) ffmpegPathOrDefault() string {
	path := ""
	s.configStore.Read(func(c *config.MoomboxConfig) { path = c.Paths.FfmpegPath })
	if path == "" {
		return "ffmpeg"
	}
	return path
}
```

and repoint the four reads:

```go
	app.OnYtdlpPluginStatus = func() (routes.YtdlpPluginInfo, error) {
		return routes.YtdlpPluginStatus(s.currentWebPort(), s.httpsEnabled())
	}
	app.OnInstallYtdlpPlugin = func() error {
		return routes.InstallYtdlpPlugin(s.currentWebPort(), s.httpsEnabled())
	}
```
```go
	app.SetupWizFFmpegCheck(func() (bool, string) {
		valid, ver, _ := routes.CheckFFmpegCached(s.ffmpegPathOrDefault())
		return valid, ver
	})
```
```go
	if s.cfg.ConfigLoaded {
		ffmpegPath := s.ffmpegPathOrDefault()
```
(`ConfigLoaded` is written once at load and never mutated, so it stays a direct read; note that in the task report.)

- [ ] **Step 7: Route the TUI's HTTP client through `httpx` (TOOL-18)**

In `internal/tui/app_commands.go`, replace the transport construction inside `apiClient`:

```go
	// The shared transport shape lives in internal/httpx, which declares
	// itself the single source of truth for Moombox's *http.Client shapes;
	// this was the one hand-built &http.Transport{} outside it (TOOL-18).
	// A fresh transport per HTTPS toggle matches the previous behaviour (the
	// HTTPS branch already built one each time) and the client is cached, so
	// a toggle is the only thing that builds another; its idle connections
	// expire on the shared 90 s IdleConnTimeout.
	base := httpx.NewTransport(httpx.TransportOptions{})
	if httpsEnabled {
		// The web server presents a self-signed certificate on loopback.
		base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	a.cachedClient = httpx.ClientWithTransport(
		30*time.Second, // generous for a loopback call; stops a silent pipe stall hanging the TUI
		&internalTokenTransport{base: base, token: a.internalToken},
	)
	a.cachedClientHTTPS = httpsEnabled
	return a.cachedClient
```
Add the `internal/httpx` import; drop `net/http` only if nothing else in the file needs it (`internalTokenTransport` likely does).

- [ ] **Step 8: Run everything**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ -run TestActionMenu -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/config/ -run TestExplicitConfigPath -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/web/routes/ -run TestWebValidatorRanges -v
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ ./internal/config/ ./internal/web/routes/ ./cmd/moombox/ ./internal/docs/
cd web/tests && node --test *.test.mjs
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./internal/tui/ ./internal/config/ ./internal/web/routes/ ./cmd/moombox/
```
Expected: PASS. `./internal/docs/` is gated because `SPEC.md` changed; the node suite because `internal/web/routes` is in the diff. If a `settings.js` mobile-validation twin repeats the two ranges, update it in this task and say so — spec §2's "match the twin UI's exact output".

- [ ] **Step 9: Commit**

```bash
git add internal/tui/action_menu.go internal/tui/app_actions.go internal/tui/action_menu_probe_test.go internal/tui/app_commands.go internal/config/config.go internal/config/config_test.go SPEC.md internal/web/routes/config_routes.go internal/web/routes/config_routes_test.go cmd/moombox/tui_wiring.go
git commit -m "fix(tui,config,web,cmd): status-only action menu, authoritative -config, aligned ranges, store reads, httpx client

CORE-9: opening the action menu ran every NeedsJob filter over every job, and
A R's / A M's call os.ReadDir per job on the bubbletea goroutine - 1,000
directory reads at 1,000 jobs. The menu's 'no jobs' now uses a status-only
StatusFilter twin; JobFilter (and its disk probe) still runs when the action
is chosen and its job selector is built.

CORE-17 / O-Y: an explicit -config path is authoritative. Load no longer
appends the search paths after it, so `-config /not/yet/there` returns
defaults instead of silently adopting ~/.config/moombox/config.toml.
SPEC.md's search-order sentence and Load's comment say so.

CORE-21: disk_warn_percent / disk_critical_percent are 1..99 and
refresh_interval is 10..10080 in the Web validator, matching
config.validateOrNormalize - a value the API called valid was refused inside
config.Save as an opaque 500.

CORE-24: the four closures that read s.cfg.Network.HTTPSEnabled /
s.cfg.Paths.FfmpegPath unlocked go through configStore.Read. (The row's
adapters.go citation does not exist; the verifier dropped that half.)

TOOL-18: the TUI's apiClient builds its transport with httpx.NewTransport and
wraps it with httpx.ClientWithTransport - the one hand-built http.Transport
outside internal/httpx.

Report rows #38, #87, #88, #90, #79.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/action_menu.go internal/tui/app_actions.go internal/tui/action_menu_probe_test.go internal/tui/app_commands.go internal/config/config.go internal/config/config_test.go SPEC.md internal/web/routes/config_routes.go internal/web/routes/config_routes_test.go cmd/moombox/tui_wiring.go
```

---

### Task 12: Docs — the TUI section and the launcher section, then delete this plan

Rows: **#102 (CORE-18, CORE-19)**.

**Files:**
- Modify: `docs/spec/user-interfaces.md` (the TUI section)
- Modify: `docs/spec/operations.md` (the Launcher/Supervisor and Automatic Rollback sections)
- Delete: `docs/superpowers/plans/2026-09-17-sweep2-c-core.md`

**Interfaces:** none.

- [ ] **Step 1: Verify every claim before writing**

The citation test requires the DECLARING file for every symbol a doc names, so confirm each of these on the branch before editing:

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/ -v
grep -rln 'charm.land/huh' internal/tui/            # which files actually use huh
ls internal/tui/*.go | grep -v _test | wc -l        # the Source Files table's row count
grep -n 'ping' cmd/moombox/launcher_windows.go       # -n 3 or -n 5
grep -n 'suffixes :=' internal/updater/updater.go    # the current cleanup list
```

- [ ] **Step 2: Fix the TUI section (CORE-18)**

In `docs/spec/user-interfaces.md`:

1. **`huh` ×3.** `:123`, `:192` and `:322` claim the Settings overlay is built with `huh`; no `settings*.go` file imports it (only `app.go`, `app_keys.go`, `app_update.go`, `ffmpeg_check.go`, `release_notes_overlay.go`, `setup_wizard.go`, `styles.go`, `text_input.go` do). Correct the package table row to `Form builder framework. Used by the Setup Wizard and the FFmpeg check overlay.`, the Source Files row for `settings.go` to `Settings overlay. Full config editing, built from the package's own field/section tables and text_input.go — not huh.`, and the Overlays row for Settings to `Full config editor built from the package's own section/field tables (`internal/tui/settings.go`).` Keep every other sentence of those rows.

2. **"Searchable" ×2.** `:185` and `:316` call the action menu searchable; filtering is disabled (`l.SetFilteringEnabled(false)` in `internal/tui/action_menu.go`). Replace both with `Command palette. A categorised list of every available action; selecting an entry executes it.`

3. **`keys.go` "using `bubbles/key`".** `:197` — `internal/tui/keys.go` holds string constants (`keyCtrlC = "ctrl+c"`), not `key.Binding`s. Replace with `Key name constants shared by the chord system and the panel handlers.`

4. **Status bar "compact mode when width < 100".** `:433` — the bar uses the `barTier` ladder described two paragraphs below. Replace the clause with `— the left side sheds its labels along the same width-tier ladder as the right (see below).`

5. **`JobsUpdateMsg` "job added or deleted".** `:340` — `JobAddedMsg` and `JobDeletedMsg` are their own lifecycle events. Replace the Content cell with `Full job list snapshot — the resync after a dropped update, and the re-filtered list after a hide_finished_age_days change. Contains []*database.Job.`

6. **The drop-resync claim.** `:418` describes a mechanism that did not exist until Task 5 of this arc. Rewrite the paragraph to what now exists:

```
If a send is dropped, a drop counter increments and a resync flag is set (the first drop of a streak is logged). The next forwarded database event — or a one-second backstop ticker — pushes a full `GetAllJobs` snapshot through the full-list channel, so the TUI catches up on the missed transition; the flag is re-armed if that snapshot cannot be fetched or delivered. This keeps the TUI from ever blocking a backend goroutine while making a dropped update recoverable (`newTUIResync` in `cmd/moombox/tui_wiring.go`).
```

7. **The Source Files table (21 of 41).** Add rows for the twenty missing files, and correct the `app.go` row — `Update`, the chord state machine and the action dispatcher live in `app_update.go`, `app_keys.go` and `app_actions.go`. Suggested `app.go` row: `Application model and its wiring: fields, constructor, backend callbacks, tick scheduling, View delegation.` The twenty to add are `app_update.go`, `app_keys.go`, `app_actions.go`, `app_commands.go`, `app_layout.go`, `app_mouse.go`, `monitor_checking.go`, `openbrowser_windows.go`, `openbrowser_other.go`, `clipboard_windows.go`, `clipboard_other.go`, `release_notes_overlay.go`, `stats_dialog.go`, `ytdlp_dialog.go`, `cookie_import_dialog.go`, `settings_view.go`, `settings_keys.go`, `settings_channels.go`, `settings_notifications.go`, `settings_security.go`, `settings_components.go` — reconcile against the `ls` in Step 1 (Task 9 added the two clipboard files) and state the final count in the section's own prose if it names one.

- [ ] **Step 3: Fix the launcher section (CORE-19)**

In `docs/spec/operations.md`:

1. **`:334` "Any other exit code: Propagate the exit code and terminate."** predates crash supervision. Replace the bullet list under **Parent behavior on child exit** with:

```
- Exit code 42: Respawn the child (loop continues). If `<exe>.old` exists (from an update), rename it to
  `<exe>~` to free the `.old` name for future updates. A rename that fails — the `~` name is still held
  by this launcher's own mapped image, which happens on the second update of one launcher lifetime — is
  reported on stderr and leaves `.old` in place as the rollback artifact.
- Normal exit (code 0): Terminate.
- A launcher-forwarded stop (SIGTERM): propagate the child's code without respawning, and without
  writing an update-failed marker.
- Exit code 130 or 143 (128+SIGINT / 128+SIGTERM): propagate — user intent.
- A non-zero exit from the FIRST boot after an update, inside `postUpdateFailureWindow` (2 minutes):
  automatic rollback (below), unless the code is `exitCodeStartupError`, which is preserved-with-
  instructions instead.
- Any other non-zero exit within `launcherHealthyWindow` (60 s) on a FRESH launch: propagate and
  terminate — a deterministic startup failure must fail fast and visibly, not crash-loop.
- Any other non-zero exit from a child that had proven it can run, or from a respawned child: crash
  supervision respawns it with 1/2/4/8/16 s backoff, giving up after `maxConsecutiveCrashes` (5).
```

2. **`:341` `ping … -n 3`.** The code is `-n 5` (`cmd/moombox/launcher_windows.go`). Correct the number.

3. **`:290`'s removal claim** — the sentence CORE-1 disproves. Replace `the broken binary is removed (it is bit-identical to the published GitHub asset, so nothing is lost)` with `the broken binary is KEPT as <exe>.failed and named in the .update-failed marker — the restored (older) binary may refuse a database the new version already migrated, and its refusal message points the operator at exactly that file`. Add one sentence to the same paragraph: `A deterministic startup failure (exit code 3, exitCodeStartupError) is never treated as a broken update: the rollback is skipped, the artifact is preserved with written instructions, and the release is not marked skipped.`

4. **The cleanup list** (the `CleanupOldBinary` sentence in the Cleanup step) omits `~` and now must name `.failed` and exclude `.sig`. Replace with `Removes stale .old, .new, .new.sig and .failed files left by previous updates, interrupted downloads or an automatic rollback; on Windows it also sweeps an orphaned ~. <exe>.sig is deliberately spared — Moombox never writes it, and it is the published signature asset a manual verifier leaves beside the binary.`

- [ ] **Step 4: Gate the docs**

```bash
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/docs/ -v
```
Expected: PASS. Every symbol newly named above (`newTUIResync`, `exitCodeStartupError`, `CleanupOldBinary`, `attemptAutoRollback`, `barTier`) must be cited with its DECLARING file; if the citation test objects, fix the citation rather than removing the fact.

- [ ] **Step 5: Commit the docs**

```bash
git add docs/spec/user-interfaces.md docs/spec/operations.md
git commit -m "docs(spec): correct the TUI section and the launcher section

CORE-18: Settings is not built with huh (only the setup wizard and the FFmpeg
overlay are); the action menu is not searchable; keys.go holds string
constants, not bubbles/key bindings; the status bar uses the barTier ladder,
not a width-100 compact mode; JobsUpdateMsg is the resync/re-filter snapshot,
not 'job added or deleted'; the drop-resync paragraph now describes the
mechanism that exists; the Source Files table covers every file.

CORE-19: the exit-code bullet list describes crash supervision, the rollback
window and the startup-error code instead of 'propagate and terminate'; the
cleanup command is ping -n 5; the rollback paragraph no longer claims the
broken binary is removed; the CleanupOldBinary list names .failed and ~ and
explains why .sig is spared.

Report row #102 (CORE-18, CORE-19).

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/spec/user-interfaces.md docs/spec/operations.md
```

- [ ] **Step 6: Run the arc's full gate set**

```bash
gofmt -l ./cmd ./internal ./tools ./web
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go vet ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./...
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./...
GOOS=linux GOARCH=amd64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
GOOS=linux GOARCH=arm64 GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go build ./cmd/moombox
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test ./internal/tui/ ./internal/jobfilter/ ./internal/config/ ./internal/updater/ ./internal/logger/ ./cmd/moombox/ ./internal/web/routes/ ./internal/docs/ ./internal/database/
cd web/tests && node --test *.test.mjs
```
Expected: `gofmt -l` empty, everything else clean. The controller runs the ONE full `go test -count=1 ./...` at the merge candidate — do not run it here.

- [ ] **Step 7: Delete this plan**

The plan is implemented and verified; git history is the archive (`feedback_delete_implemented_plans`).

```bash
git rm docs/superpowers/plans/2026-09-17-sweep2-c-core.md
git commit -m "chore(plans): delete the implemented Arc C plan

All twelve tasks are implemented and their gates are green; the arc's record
lives in the SDD ledger and in git history.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/superpowers/plans/2026-09-17-sweep2-c-core.md
```

---

## Task-to-row map (for the arc-close review)

| Row | Item | Task |
|---|---|---|
| #7 | CORE-1 failed-update binary kept as `.failed` | 1 |
| #89 | CORE-23 dedicated non-rollback exit code | 1 |
| #83 | CORE-13 `.sig` dropped from the cleanup list | 1 |
| #34 | CORE-2 TUI render cost | 2, 3, 4 |
| #35 | CORE-6 dropped-update resync | 5 |
| #82 | CORE-11 `hide_finished_age_days` across both UIs | 5 |
| #14 | CORE-4 `OnSaveConfig` returns error + snapshot restore | 6 |
| #16 | CORE-10 `-log-level` applied to the logger only | 6 |
| #15 | CORE-8 + WEB-8 shared archive predicate | 7 |
| #15 | CORE-7 fractional FlexDuration settings | 8 |
| #88 | CORE-20 TUI help-text ranges (TUI half) | 8 |
| #37 | CORE-5 + O-M Ctrl+C above overlay dispatch | 9 |
| #86 | CORE-16 + O-X task-list paging + help + Arc 6 test | 9 |
| #85 | CORE-15 + O-W `clip.exe` fallback + hedged wording | 9 |
| #36 | CORE-3 rotation back-off, one diagnostic per streak | 10 |
| #73 | CORE-22 format each line once | 10 |
| #72 | CORE-12 log routing tracks non-terminal jobs only | 10 |
| #99 | TOOL-15 empty `if runtime.GOOS` block | 10 |
| #38 | CORE-9 action menu: status-only, probe on selection | 11 |
| #87 | CORE-17 + O-Y `-config` authoritative + `SPEC.md` | 11 |
| #88 | CORE-21 Web validator ranges (Web half) | 11 |
| #90 | CORE-24 closures read through the config store | 11 |
| #79 | TOOL-18 TUI HTTP client through `httpx` | 11 |
| #102 | CORE-18 + CORE-19 doc passes | 12 |
| #84 | CORE-14 — **DROPPED**, O-V rejected (Esc still quits) | — |
