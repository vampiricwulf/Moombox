# Arc X — post-sweep-2 residuals — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the four host-fixable residuals the post-sweep-2 follow-up chain left behind. The worker's `writeDescriptionAtomic` — the last fixed-name `.tmp` writer the chain found and did not adopt — delegates to `utils.WriteFileAtomic`. The status bar's re-tally gate stops being two keys typed out at the call site and becomes a named `tallyColumns` set with a test that pins it against the derivation it is coupled to. `reorderBuffer.release()` zeroes `b.bytes` so `residentBytes()` stops reporting bytes for an empty map. And the one cold `-race` failure in `internal/tui/render_cache_test.go` is diagnosed before anything is changed.

**Architecture:** One mini-arc, one worktree, four independent tasks plus the gates-and-delete task. The file sets are disjoint by construction: Task 1 is `internal/worker`, Task 2 is `internal/tui` (`app_update.go` + `frame_counts_test.go`), Task 3 is `internal/engine`, Task 4 is `internal/tui` tests (`render_cache_test.go` only — see the shared-file note below). Nothing else in the chain is in flight, so Task 5's merge of `main` is expected to be a no-op and the branch is the only merge candidate.

Tasks 1 and 3 are **adoptions and one-line fixes, not rewrites**. Task 1 keeps the named helper, its call site and its byte output; only the mechanism underneath moves. Task 3 adds one assignment inside an existing critical section. Task 2 is a **refactor with a new pin** — the gate's behaviour is unchanged on every input, and the new test is what makes that checkable. Task 4 is **diagnosis first**: it has a branch table and three possible outcomes, one of which changes no production code at all.

**Tech Stack:** Go 1.27 (`toolchain go1.27.1`), no CGo, pure-Go dependencies. `charm.land/bubbletea/v2` + `lipgloss/v2` drive the TUI panels Task 2 and Task 4 touch. `internal/utils.WriteFileAtomic` (sweep-2 Arc X, `internal/utils/writefile.go`) is Task 1's adopt target. Node 24 for `web/tests` (gates only; this arc touches no JS). Git Bash on Windows for every command below.

**Spec:** `docs/superpowers/specs/2026-09-24-followup-residuals-design.md` — §0 items 1–4 (the four residuals, verbatim from the chain's Fable reviews), §1 (carried constraints, copied verbatim below), §2 (order: the four items, then gates-and-delete; branch `followup-x-residuals`). Upstream context: `docs/superpowers/specs/2026-09-24-post-sweep2-followups-design.md` §0 and §3 (Arc U's two adoptions, whose discipline item 1 copies). Evidence for item 4: `.superpowers/sdd/2026-09-24-followup-c-core/close-wave-review.md`, "Flake note (not attributable to the wave)". Evidence for item 2: `.superpowers/sdd/2026-09-24-followup-c-core/progress.md`, the Task 4 fix-round-1 line and its concern (2).

---

## Global Constraints

Copied verbatim from spec §1, plus the four this arc adds. Every task's requirements implicitly include this section.

- DB layer untouched; `internal/tui` never imports `internal/web`, `internal/web/routes`, `internal/bgutils`; FrameCost pins 60 (cached frame) / 2 (status bar) hold on every `-count=5` run; ~60 Hz: cheaper never rarer; update path untouched; `go mod tidy -diff` empty; LF everywhere; the citation gate green for any doc touched.
- One commit per task with a pathspec; the two trailer lines verbatim; no stash/checkout/rebase/reset/amend; implementers never run the full `go test ./...` (the controller's gate runner does).

Added for this arc:

- **The FrameCost constants are not edited.** `maxCachedFrameAllocs = 60` and `maxCachedStatusBarAllocs = 2` in `internal/tui/frame_cost_test.go` stay exactly as they are, and `go test -count=5 -run TestFrameCostAtLogCap ./internal/tui/` is green on all five iterations. (Observed on main: 30 allocs for the cached frame, 0 for the status bar. Record what it logs; do not move the budgets to match.)
- **`platform` stays in `tallyColumns`.** `parkedCookieJobs` reads `Job.Platform`, so the column is a real tally input even though `UpdateJobFields` never writes it today. Removing it because "nothing emits it" is the exact mutant Task 2's test exists to catch.
- **No change to the ~60 Hz behaviour.** Task 2 adds a map lookup on a path that already ran a two-key `slices.Contains`; it adds nothing to any frame. A progress tick must still re-tally nothing — `TestNonTallyUpdatesLeaveTheStatusBarTallyAlone/progress_tick` is the standing pin and stays green.
- **The engine's `saveResume` is untouched.** `internal/engine/downloader_resume.go`'s `saveResume` is a third fixed-name `.tmp` writer and its test (`internal/engine/downloader_test.go`) asserts the literal `resumeFile + ".tmp"` name. It is outside this arc's file set, exactly as it was outside Arc U's. Do not adopt it, do not touch that assertion.
- The anonymous logger interface stays anonymous per struct; every goroutine keeps its inline `defer func() { if r := recover(); … }()`. Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names the mutant that fails it, and each task verifies at least one mutant **by execution**, by hand-editing the file and reverting it by hand (no `git checkout`, no `git stash`).
- Every commit's LAST TWO LINES are exactly `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq` — the project's rule, which takes precedence over any attribution reminder in an implementer's own context whatever model name it shows.
- `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command (export it once per shell). Package-scoped runs only inside a task. Never `python3 -` heredocs (the uv shim hangs) — `perl` or `node -e` instead. Never kill processes by image name; never `rm -rf` under `%TEMP%`.
- Line numbers quoted below are from the worktree at `ad1a8bdf`. Locate every target by the quoted text or symbol as well, never by the number alone.

**Shared-file note (checked, and the reason Tasks 2 and 4 can run in either order):** both touch `internal/tui` tests, but **different files**. Task 2 writes `internal/tui/app_update.go` and `internal/tui/frame_counts_test.go` (where `TestTallyJobsCountsOnce`, `TestJobStatusTransitionRetalliesTheStatusBar` and `TestNonTallyUpdatesLeaveTheStatusBarTallyAlone` already live — the tally's test home). Task 4 writes `internal/tui/render_cache_test.go` and nothing else, unless its diagnosis lands on branch (b), in which case it stops and reports before touching a production file. No file appears in two tasks' pathspecs.

---

## Branch and worktree

Already created and prepared (verified 2026-09-24): `D:/Git/Moombox/.worktrees/followup-x-residuals` on branch `followup-x-residuals` at `392e5667` (the plan commit; `ad1a8bdf` + this plan, so every code line number below still resolves), with the four embed blobs, `internal/cipher/testdata/*.js` and `web/tests/node_modules` all present, and a clean `git status`. The recipe is recorded here only so it can be rebuilt if the worktree is lost:

```bash
cd /d/Git/Moombox
git worktree add -b followup-x-residuals .worktrees/followup-x-residuals main
cp internal/bgutils/embed/node-windows-amd64.gz \
   internal/bgutils/embed/node-linux-amd64.gz \
   internal/bgutils/embed/node-linux-arm64.gz \
   internal/bgutils/embed/sidecar.tar.gz \
   .worktrees/followup-x-residuals/internal/bgutils/embed/
mkdir -p .worktrees/followup-x-residuals/internal/cipher/testdata
cp internal/cipher/testdata/*.js .worktrees/followup-x-residuals/internal/cipher/testdata/
cd .worktrees/followup-x-residuals/web/tests && npm ci --no-audit --no-fund
```

Every path below is relative to the worktree root `D:/Git/Moombox/.worktrees/followup-x-residuals`.

---

## File Structure

| File | Task | Responsibility after this arc |
|---|---|---|
| `internal/utils/writefile.go` | — | **Unchanged.** The adopt target: `WriteFileAtomic(path, data, perm)` = `os.CreateTemp` + write + fsync (via the `syncFile` seam) + `os.Chmod` + `utils.ReplaceFile`, with one deferred temp cleanup guarded by `done`. |
| `internal/worker/orchestrator_mux.go` | 1 | `writeDescriptionAtomic(finalPath, body string) error` = one call to `utils.WriteFileAtomic(finalPath, []byte(body), 0o644)`. No local temp name, no local `os.Remove`. The `copyAssets` call site at `:1098` is unchanged. |
| `internal/worker/orchestrator_mux_test.go` | 1 | **New.** The worker-side `assertNoTempSurvives` twin plus three tests: the unique-temp-name pin (red first), the verbatim-bytes regression pin, and the rename-failure pin. |
| `internal/tui/app_update.go` | 2 | Gains `tallyColumns` + `hasTallyChange` immediately after `displayColumns` + `hasDisplayChange`; `handleJobUpdate`'s re-tally gate reads `hasTallyChange(ev.Changes)` instead of a two-key `slices.Contains`. Behaviour identical on every input. |
| `internal/tui/frame_counts_test.go` | 2 | Gains `TestHasTallyChange` (the gate's table) and `TestTallyColumnsMatchWhatTheTallyReads` (the two-way coupling pin against `tallyJobs`/`parkedCookieJobs`). The three existing tally tests are untouched and stay green. |
| `internal/engine/downloader_parallel.go` | 3 | `release()` zeroes `b.bytes` beside `clear(b.seg)`, in the same critical section. One line plus a doc-comment sentence. `residentBytes()`, `admit`, `take` untouched. |
| `internal/engine/reorder_budget_test.go` | 3 | `TestTakeAndReleaseFreeExactlyWhatThisBufferReserved` gains three `residentBytes() == 0` assertions (after the release, after the idempotent second release, after the post-release take) and one more named mutant. |
| `internal/tui/render_cache_test.go` | 4 | Branch-dependent: (a) `runSteps` observes each memoised-vs-fresh pair inside one wall-clock second; (c) `runSteps`' failure message names the first differing byte and quotes a window around it. Branch (b) touches a production file and stops for the controller first. |
| `internal/engine/downloader_resume.go` | — | **Untouched, deliberately.** See the global constraint. |
| `docs/superpowers/plans/2026-09-24-followup-x-residuals.md` | 5 | Deleted — git history is the archive. |

---

### Task 1: `writeDescriptionAtomic` adopts `utils.WriteFileAtomic`

Spec §0 item 1. `writeDescriptionAtomic` is declared at `internal/worker/orchestrator_mux.go:30` and called from exactly one place, `copyAssets` at `:1098`. Today it is:

```go
func writeDescriptionAtomic(finalPath, body string) error {
	tmpPath := finalPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(body), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
```

Its doc comment promises that "a crash mid-write can't leave a partially-written `.description` file that the DB row still points at". That promise is only half kept, and the three gaps are the reason this adopt is worth doing rather than merely tidy:

- **No fsync at all.** `os.WriteFile` does not sync. A crash or power loss can journal the rename while the data pages never reach disk, so the DB row ends up pointing at a zero-length or torn `.description` — precisely the outcome the comment says is impossible.
- **A fixed temp name.** Two jobs whose resolved filename base collides in one output directory (a re-download, a restart muxed beside its predecessor, two parts of one stream finalising into the same folder) share `<base>.description.tmp` and can rename a torn result into place.
- **A bare `os.Rename`.** Every other output write in this package goes through `utils.ReplaceFile`, which retries the Windows AV/indexer `ERROR_ACCESS_DENIED` / `ERROR_SHARING_VIOLATION` window instead of reporting it as a hard failure. This one did not.

`WriteFileAtomic` supplies all three plus a single deferred cleanup, and the bytes on disk do not move.

**Files:**
- Modify: `internal/worker/orchestrator_mux.go`
- Create: `internal/worker/orchestrator_mux_test.go`

**Interfaces:**
- Consumes: `utils.WriteFileAtomic(path string, data []byte, perm os.FileMode) error` from `internal/utils/writefile.go`. `internal/utils` is **already imported** by `orchestrator_mux.go` (line 19) — no import change is needed on the production side. The `syncFile` seam is package-private to `internal/utils` and is deliberately NOT reached from here.
- Produces: `assertNoTempSurvives(t *testing.T, dir string)` in the new test file. The signature of `writeDescriptionAtomic(finalPath, body string) error` is UNCHANGED and its one call site is untouched.

**Ruling — the glob helper is a worker-side twin, not a new `utils` export.** `internal/utils/chatfile_test.go:442` already defines `assertNoTempSurvives`, but a `_test.go` symbol is invisible outside its package. The alternatives are (i) exporting a `testing`-dependent helper from `internal/utils`, which drags `testing` into a production package that ships in the binary, or (ii) standing up an `internal/utils/utilstest` package for nine lines of `filepath.Glob`. Both cost more than the duplication they remove, and spec §0 item 1 explicitly allows "`assertNoTempSurvives` **or its twin in the owning package**". So: a nine-line unexported twin, carrying the **same name** on both sides so they read as one idea, with a comment citing the original.

**Ruling — the tests live in a new `internal/worker/orchestrator_mux_test.go`.** The package leans toward one `_test.go` per declaring file (`orchestrator_utils_test.go`, `orchestrator_eviction_test.go`), and `orchestrator_mux.go` — the largest orchestrator file and the one this arc touches — has no twin. The two existing files that could absorb these tests are `mux_finalize_test.go` (18 lines, about `httpError`) and `chat_status_outcome_test.go` (chat verdicts, which happens to drive `copyAssets`); burying a writer's tests in either would make them unfindable from the declaring file.

**Behaviour delta to record (the only one):** the written file's POSIX mode becomes exactly `0644` (`WriteFileAtomic` chmods after an `os.CreateTemp` that creates at `0600`) instead of `0644 &^ umask` (what `os.WriteFile`'s create mode gave). A no-op on Windows and on a default-umask Linux host; on a `umask 077` host the `.description` widens from `0600` to `0644`. Intended — it matches every other file `WriteFileAtomic` writes, and the mode is already pinned by `internal/utils/writefile_test.go`'s `TestWriteFileAtomicWritesContentAndPerm`. State it in the doc comment; do not add a duplicate perm test.

- [ ] **Step 1: Write the new test file**

Create `internal/worker/orchestrator_mux_test.go` with exactly this content:

```go
package worker

import (
	"os"
	"path/filepath"
	"testing"
)

// assertNoTempSurvives fails if any *.tmp entry is left in dir. The twin of
// internal/utils/chatfile_test.go's helper of the same name — copied rather
// than shared because a _test.go symbol is invisible outside its package, and
// the alternatives (a testing-dependent export from internal/utils, or a
// utilstest package for nine lines) both cost more than the duplication. Same
// name on both sides so they read as one idea.
//
// It replaces the fixed-name `os.Stat(path + ".tmp")` check the adopting
// writer used to be pinned by: once the temp name comes from os.CreateTemp, a
// stat of one hard-coded name proves nothing, while the glob still proves the
// real property — the directory is left with the target and nothing else.
func assertNoTempSurvives(t *testing.T, dir string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("glob temps in %s: %v", dir, err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temp files survived: %v", leftovers)
	}
}

// TestWriteDescriptionAtomicUsesAUniqueTempName pins the adopt: the
// description writer now goes through utils.WriteFileAtomic, whose temp name
// comes from os.CreateTemp, so a writer already mid-write on the FIXED
// `finalPath + ".tmp"` name can no longer be clobbered. Two jobs whose
// resolved filename base collides in one output directory — a re-download, or
// a restart muxed beside its predecessor — aim at one .description and shared
// that single temp.
//
// Checked directly rather than by racing two writers: the directory is
// pre-seeded with the fixed name the old writer would have used, and it must
// come back untouched.
//
// Mutant this kills: restoring the local `tmpPath := finalPath + ".tmp"`
// writer (the pre-adopt code) — it truncates the seeded file and renames it
// onto the target, so the ReadFile below fails outright.
func TestWriteDescriptionAtomicUsesAUniqueTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "show.description")
	fixed := path + ".tmp"

	if err := os.WriteFile(fixed, []byte("another-writer-is-mid-write"), 0o644); err != nil {
		t.Fatalf("seed fixed temp: %v", err)
	}
	if err := writeDescriptionAtomic(path, "the description body"); err != nil {
		t.Fatalf("writeDescriptionAtomic: %v", err)
	}

	got, err := os.ReadFile(fixed)
	if err != nil {
		t.Fatalf("the fixed .tmp name was consumed: %v", err)
	}
	if string(got) != "another-writer-is-mid-write" {
		t.Errorf("fixed .tmp content: want it untouched, got %q", string(got))
	}
}

// descriptionGoldenBody is chosen to catch any transform an adopt could
// smuggle in: a CRLF, a lone LF, non-ASCII, and a trailing blank line.
const descriptionGoldenBody = "line one\r\nline two\nhttps://example.test/\u00fcn\u00efcode \u2713\n\n"

// TestWriteDescriptionAtomicWritesTheBodyVerbatim is a REGRESSION pin, not a
// red-first test: green before AND after the adopt, which is exactly its job.
// There is no encoder anywhere on this path — the writer takes a string and
// puts those bytes on disk — and orphan adoption (orphans.go, `ext ==
// ".description"`) reads files an earlier build wrote, so a byte of drift
// would be invisible until someone diffed an archive.
//
// Mutants this kills: appending a trailing newline; writing
// strings.TrimSpace(body); routing the body through a fmt.Fprintf that
// reinterprets a % in a description.
func TestWriteDescriptionAtomicWritesTheBodyVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "show.description")

	if err := writeDescriptionAtomic(path, descriptionGoldenBody); err != nil {
		t.Fatalf("writeDescriptionAtomic: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(raw) != descriptionGoldenBody {
		t.Errorf("the body was transformed.\n got: %q\nwant: %q", string(raw), descriptionGoldenBody)
	}

	// No temp of ANY name may survive a successful write. The fixed-name stat
	// the pre-adopt writer could have been pinned by is retired because the
	// name is now unpredictable; the glob is its honest replacement on this
	// SUCCESS path.
	assertNoTempSurvives(t, dir)
}

// TestWriteDescriptionAtomicRenameFailureLeavesNoTemp drives the one failure
// path reachable from this package. utils.WriteFileAtomic's syncFile seam is
// package-private to internal/utils, so the RENAME is what gets injected here:
// a directory standing where the .description belongs. os.Rename refuses to
// replace a directory with a file on every platform Moombox ships on, and
// whatever stood at the target path is still there afterwards.
//
// Green before AND after the adopt — the old writer hand-removed its temp on
// this branch too — so this is a regression pin rather than a red-first test.
// What it guards is THIS package re-diverging: an edit that open-codes
// os.CreateTemp + write + rename here and forgets the cleanup fails it, and so
// does deleting utils.WriteFileAtomic's single deferred os.Remove.
//
// Windows note, measured: MoveFileEx reports ERROR_ACCESS_DENIED for a
// directory in the target's place, which utils.transientReplaceError
// classifies as transient, so ReplaceFile spends its whole retry ladder
// (10+20+40+80+160+320+640 ms) before returning the error. This one test
// therefore costs about 1.3 s on Windows and nothing elsewhere. Expected, not
// a hang. (utils/replacefile_windows.go's own comment calls a directory in the
// way "permanent and must not be retried" — right about the intent, wrong
// about the Windows error code. Recorded as an observation; internal/utils is
// outside this arc's file set.)
func TestWriteDescriptionAtomicRenameFailureLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "show.description")
	marker := filepath.Join(path, "occupied")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("seed the blocking directory: %v", err)
	}
	if err := os.WriteFile(marker, []byte("still here"), 0o644); err != nil {
		t.Fatalf("seed the marker: %v", err)
	}

	if err := writeDescriptionAtomic(path, "a body that cannot land"); err == nil {
		t.Fatal("writeDescriptionAtomic: want the rename to fail against a directory, got nil")
	}

	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("what stood at the target path is gone: %v", err)
	}
	if string(got) != "still here" {
		t.Errorf("the previous target was disturbed: %q", string(got))
	}
	assertNoTempSurvives(t, dir)
}
```

- [ ] **Step 2: Run the tests and see them RED**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestWriteDescriptionAtomic' -v ./internal/worker/
```

Expected: **FAIL**, with exactly ONE of the three failing —

- `TestWriteDescriptionAtomicUsesAUniqueTempName` → `the fixed .tmp name was consumed: open …\show.description.tmp: The system cannot find the file specified.` (the old writer truncated the seeded temp and renamed it onto the target).

`TestWriteDescriptionAtomicWritesTheBodyVerbatim` and `TestWriteDescriptionAtomicRenameFailureLeavesNoTemp` must both **PASS** in this run — they are the regression pins, and a pass here is the capture against the pre-adopt writer. If the verbatim pin fails here, the golden literal in this plan is wrong: take the `got` value straight from the failure message, paste it in, re-run until green, and record the correction in the task report. If the rename-failure pin fails here, stop and report — it means the old writer did not clean up on that branch either, which contradicts the code as read.

Note the timing while you are here: on this RED run the rename-failure test is fast (a bare `os.Rename`, no retry ladder). Record both the RED and the GREEN wall time for it so the 1.3 s claim in its comment is measured rather than asserted.

- [ ] **Step 3: Adopt the shared writer**

In `internal/worker/orchestrator_mux.go`, replace the whole of `writeDescriptionAtomic` — doc comment and body, currently lines 27–40 (the three comment lines through the function's closing brace) — with:

```go
// writeDescriptionAtomic writes the description through utils.WriteFileAtomic:
// a uniquely named temp file in the same directory, fsync, chmod 0644 and
// utils.ReplaceFile.
//
// It used to do this by hand — os.WriteFile to a FIXED finalPath + ".tmp",
// then a bare os.Rename — and the old comment's promise, that "a crash
// mid-write can't leave a partially-written .description file that the DB row
// still points at", was only half kept. Three gaps, all closed by the shared
// writer:
//
//   - No fsync at all. os.WriteFile does not sync, so a crash could journal
//     the rename while the data pages never reached disk, leaving exactly the
//     torn file the comment said was impossible.
//   - A fixed temp name. Two jobs whose resolved filename base collides in one
//     output directory shared that single temp; os.CreateTemp gives each
//     writer its own.
//   - A bare os.Rename. Every other output write in this package goes through
//     utils.ReplaceFile, which retries the Windows AV/indexer sharing window
//     instead of reporting it as a hard failure. This one did not.
//
// Plus one deferred temp cleanup in place of the hand-written one. Those
// properties are pinned by writefile_test.go's
// TestWriteFileAtomicSyncsBeforeReplacingTarget,
// TestWriteFileAtomicSyncFailureLeavesNoTempAndTargetUntouched,
// TestWriteFileAtomicRenameFailureLeavesNoTempAndTargetIntact and
// TestWriteFileAtomicUsesAUniqueTempName — cited here rather than re-tested
// from this side.
//
// The bytes on disk are unchanged: the body is written verbatim, there is no
// encoder, and TestWriteDescriptionAtomicWritesTheBodyVerbatim pins it. The
// one deliberate difference is the POSIX mode — exactly 0644 now
// (WriteFileAtomic chmods) rather than 0644 masked by the process umask. A
// no-op on Windows and on a default-umask Linux host; on a umask 077 host the
// .description widens from 0600 to 0644, matching every other file the shared
// writer produces.
func writeDescriptionAtomic(finalPath, body string) error {
	return utils.WriteFileAtomic(finalPath, []byte(body), 0o644)
}
```

No import changes on the production side: `utils` is already imported, and `os` still has many users in this file. Confirm by build in the next step rather than by eye.

- [ ] **Step 4: Run the tests and see them GREEN**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestWriteDescriptionAtomic|TestCopyAssets' -v ./internal/worker/
```

Expected: `ok github.com/vampiricwulf/Moombox/internal/worker`, all three new tests passing plus `TestCopyAssetsRoutesTheWholeJobChatCopyThroughChatFileStatus` (the one existing test that drives `copyAssets`, the writer's only caller). `TestWriteDescriptionAtomicRenameFailureLeavesNoTemp` should now report roughly 1.3 s in the `-v` output; record the number.

- [ ] **Step 5: Verify a regression pin's teeth by execution**

`TestWriteDescriptionAtomicWritesTheBodyVerbatim` never ran red, so prove it can. Edit `internal/worker/orchestrator_mux.go` **by hand** (no `git checkout`, no `git stash`), changing

```go
	return utils.WriteFileAtomic(finalPath, []byte(body), 0o644)
```

to

```go
	return utils.WriteFileAtomic(finalPath, []byte(body+"\n"), 0o644)
```

then:

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestWriteDescriptionAtomicWritesTheBodyVerbatim' ./internal/worker/   # expect FAIL
```

Expected: `the body was transformed.` with the `got:` value carrying one extra `\n` against `want:`. Restore the line by hand and re-run the same command — expect `ok`. Record the observed failure line in the task report.

- [ ] **Step 6: Package gates**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./internal/worker
go vet ./internal/worker/
staticcheck ./internal/worker/
go test -count=1 -timeout 600s ./internal/worker/
go test -count=1 -timeout 600s -race -run 'TestWriteDescriptionAtomic|TestCopyAssets' ./internal/worker/
go test -count=1 -timeout 300s ./internal/utils/
```

Expected: `gofmt -l` and `staticcheck` print nothing; everything else `ok`. `staticcheck` matters in this task specifically — it is the one that introduces a new test helper, and an unused-helper slip is what the U1000 gate is there to catch. `internal/utils` is run unchanged as a witness that the adopt did not disturb the shared writer's own suite. Record the `internal/worker` wall time.

- [ ] **Step 7: Commit**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
git add internal/worker/orchestrator_mux.go internal/worker/orchestrator_mux_test.go
git commit -m "refactor(worker): writeDescriptionAtomic writes through WriteFileAtomic

The last fixed-name .tmp writer the follow-up chain found. Its doc comment
promised that a crash mid-write could not leave a partially-written
.description behind, and it kept half of that: os.WriteFile never synced, so
the rename could be journalled while the data pages never reached disk; the
fixed finalPath + \".tmp\" was shared by any two jobs whose resolved filename
base collided in one output directory; and the bare os.Rename skipped the
Windows AV/indexer retry every other output write in this package goes
through. utils.WriteFileAtomic supplies all three plus one deferred temp
cleanup.

The bytes on disk are unchanged — there is no encoder on this path — and a
verbatim-bytes pin says so. The one deliberate difference is the POSIX mode,
exactly 0644 now instead of 0644 masked by the umask.

New tests in the declaring file's twin: the seeded fixed .tmp name must come
back untouched (red before this change), the body must land verbatim, and a
rename onto a directory must leave no temp and the previous target intact —
the last two via a directory-wide *.tmp glob, the worker-side twin of
internal/utils/chatfile_test.go's assertNoTempSurvives.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/worker/orchestrator_mux.go internal/worker/orchestrator_mux_test.go
```

---

### Task 2: `tallyColumns` beside `displayColumns`

Spec §0 item 2. Arc C's Task-4 fix (commit `6ce6d40c`) made `handleJobUpdate` re-feed `statusBar.SetJobs` whenever a written column is one the status bar's stored tally reads, because `TaskListModel.UpdateJob` writes into the backing array `statusBar.jobs` aliases and a stored tally — unlike the per-frame walk it replaced — never noticed. The fix is correct and pinned. What is not pinned is the **coupling**: the gate at `internal/tui/app_update.go:1269` spells the two keys out inline,

```go
		if slices.Contains(ev.Changes, "status") || slices.Contains(ev.Changes, "platform") {
```

while the derivation lives elsewhere — `tallyJobs` (`internal/tui/status_bar.go:429`) reads `Job.Status`, and `parkedCookieJobs` (`:660`) reads `Job.Status` and `Job.Platform`. Nothing checks that those two lists agree. A future field added to `barJobCounts` would leave the bar frozen for whatever column feeds it, silently, and the existing tests would all stay green.

This task names the set, points the gate at it, and pins it in **both directions** against the derivation.

**Files:**
- Modify: `internal/tui/app_update.go`
- Modify: `internal/tui/frame_counts_test.go`

**Interfaces:**
- Produces: `tallyColumns map[string]struct{}` and `hasTallyChange(changes []string) bool`, both unexported, both in `app_update.go` immediately after `hasDisplayChange`.
- Consumes: `(*StatusBarModel).tallyJobs()` and the `barJobCounts` struct (comparable: `active int`, `ytParked bool`, `twParked bool`) from `internal/tui/status_bar.go` — the derivation the new test probes.
- Unchanged: `displayColumns`, `hasDisplayChange`, `TestDisplayColumnsCoverage` (`internal/tui/tui_test.go:276`), `TestHasDisplayChange`, and all three existing tally tests in `frame_counts_test.go`.

**Nesting stays.** The new gate stays **inside** the `if hasDisplayChange(ev.Changes)` branch, exactly where the old one was. `tallyColumns` is deliberately NOT a subset of `displayColumns` — `platform` is in one and not the other — and the nesting is what makes that safe: a hypothetical `platform`-only write would not re-tally, which is correct today because `UpdateJobFields` never writes `platform` and, if it ever did, it would arrive alongside a display column. Do not hoist the gate out of the branch.

**Cost:** one map lookup per change key, on a path that already ran up to two `slices.Contains` scans over the same slice. Strictly cheaper for the two-key case and unchanged in shape. Nothing is added to any frame; the FrameCost pins are re-run in Step 6 to say so.

- [ ] **Step 1: Write the new tests**

Append to `internal/tui/frame_counts_test.go`. Its imports (`strings`, `testing`, `internal/database`) already cover everything below.

```go
// tallyProbe is one column's claim: mutate writes the field that column
// carries, and moves says whether the status bar's stored tally is allowed to
// follow it. The fixture is per-row because platform only moves the tally for
// a job that is already parked — parkedCookieJobs filters on Status first, so
// a platform flip on a Downloading job is correctly invisible, and a probe
// that used one shared fixture would report platform as a non-input.
type tallyProbe struct {
	column string
	job    func() *database.Job
	mutate func(*database.Job)
	moves  bool
}

// TestTallyColumnsMatchWhatTheTallyReads is the coupling pin tallyColumns
// exists for. Arc C's fix gated handleJobUpdate's SetJobs call on two keys
// typed out beside the call; nothing tied those keys to tallyJobs or
// parkedCookieJobs, so a new field in barJobCounts could quietly freeze the
// bar for whatever column feeds it and every test would stay green.
//
// Both directions are checked, per column. The DERIVATION: does tallyJobs'
// answer actually move when this field moves? The SET: is the column in
// tallyColumns? They must agree, so a column can neither sit in the set
// without being an input nor be an input without sitting in the set. The
// count check at the end closes the third hole — a column added to the set
// with no probe beside it.
//
// Mutants this kill: deleting "platform" from tallyColumns (the platform
// row's set half fires); adding any of the eleven other display columns to it
// (that row's set half fires, and TestNonTallyUpdatesLeaveTheStatusBarTally
// Alone/title_rewrite fires too for "title"); adding a column to the set with
// no probe (the count check fires).
func TestTallyColumnsMatchWhatTheTallyReads(t *testing.T) {
	downloading := func() *database.Job {
		return &database.Job{ID: "a", Status: database.StatusDownloading, Platform: "youtube"}
	}
	probes := []tallyProbe{
		// The two inputs.
		{"status", downloading, func(j *database.Job) { j.Status = database.StatusFinished }, true},
		{"platform", func() *database.Job {
			return &database.Job{ID: "a", Status: database.StatusCookies, Platform: ""}
		}, func(j *database.Job) { j.Platform = "twitch" }, true},

		// The eleven other display columns, each moved on a job the tally
		// does count, so a false positive would show.
		{"title", downloading, func(j *database.Job) { j.Title = "retitled" }, false},
		{"channel_name", downloading, func(j *database.Job) { j.ChannelName = "other" }, false},
		{"thumbnail_url", downloading, func(j *database.Job) { j.ThumbnailURL = "https://example.test/t.jpg" }, false},
		{"description", downloading, func(j *database.Job) { j.Description = "some text" }, false},
		{"stream_start_time", downloading, func(j *database.Job) { j.StreamStartTime = "2026-09-24T00:00:00Z" }, false},
		{"stream_end_time", downloading, func(j *database.Job) { j.StreamEndTime = "2026-09-24T01:00:00Z" }, false},
		{"error", downloading, func(j *database.Job) { j.Error = "boom" }, false},
		{"output_file", downloading, func(j *database.Job) { j.OutputFile = "D:/out/show.mp4" }, false},
		{"filename", downloading, func(j *database.Job) { j.Filename = "show.mp4" }, false},
		{"is_vod", downloading, func(j *database.Job) { j.IsVod = true }, false},
		{"chat_status", downloading, func(j *database.Job) { j.ChatStatus = "incomplete" }, false},

		// Not a display column at all, and the ~10/sec one: it must not move
		// the tally either, or the 60 Hz rule is broken at the source.
		{"progress", downloading, func(j *database.Job) { j.Progress, j.Percent = "V:9 A:9", 42 }, false},
	}

	inputs := 0
	for _, p := range probes {
		if p.moves {
			inputs++
		}
		t.Run(p.column, func(t *testing.T) {
			m := NewStatusBarModel()
			job := p.job()
			m.SetJobs([]*database.Job{job})

			before := m.tallyJobs()
			p.mutate(job)
			after := m.tallyJobs()
			if moved := after != before; moved != p.moves {
				t.Errorf("mutating the field %s carries moved the tally = %v, want %v (before %+v, after %+v)",
					p.column, moved, p.moves, before, after)
			}

			_, inSet := tallyColumns[p.column]
			if inSet != p.moves {
				t.Errorf("tallyColumns[%q] = %v but the derivation says the tally does%s follow it — "+
					"the gate and tallyJobs/parkedCookieJobs disagree",
					p.column, inSet, map[bool]string{true: "", false: " not"}[p.moves])
			}
		})
	}

	if len(tallyColumns) != inputs {
		t.Errorf("tallyColumns has %d entries but only %d probed columns move the tally — a column was "+
			"added to the set with no probe beside it, so nothing checks that it is really an input",
			len(tallyColumns), inputs)
	}
}

// TestHasTallyChange is the gate's own table, the twin of TestHasDisplayChange,
// and the reason handleJobUpdate can read a named set instead of spelling two
// keys out at the call site.
//
// Mutant this kills: hasTallyChange returning true on the first key it sees
// rather than on a key in the set — "title only" and "unknown column" fire.
func TestHasTallyChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []string
		want    bool
	}{
		{"nil", nil, false},
		{"empty slice", []string{}, false},
		{"progress tick", []string{"progress", "percent", "speed", "eta"}, false},
		{"title only", []string{"title"}, false},
		{"status transition", []string{"status"}, true},
		{"platform", []string{"platform"}, true},
		{"mixed: progress + status", []string{"progress", "status", "eta"}, true},
		{"mixed: title + platform", []string{"title", "platform"}, true},
		{"unknown column", []string{"some_unknown_column"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasTallyChange(tc.changes); got != tc.want {
				t.Errorf("hasTallyChange(%v) = %v, want %v", tc.changes, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests and see them RED**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestTallyColumnsMatchWhatTheTallyReads|TestHasTallyChange' ./internal/tui/
```

Expected: **FAIL to build** —

```
internal/tui/frame_counts_test.go: undefined: tallyColumns    (×3)
internal/tui/frame_counts_test.go: undefined: hasTallyChange  (×1)
```

That build failure is this task's red: both symbols are introduced by Step 3. The *behavioural* red — that the gate really reads the set — is carried by the mutants in Step 5, two of which are verified by execution. If a name OTHER than `tallyColumns` or `hasTallyChange` is reported undefined (a field that does not exist on `database.Job`, say), fix the test before going on; the struct is `internal/database/types.go:58` and the spelling is `IsVod`, not `IsVOD`.

- [ ] **Step 3: Name the set, point the gate at it**

In `internal/tui/app_update.go`, insert immediately after `hasDisplayChange` (whose closing brace is line 1207, just before the blank line and the `isProgressTerminal` doc comment):

```go
// tallyColumns is the set of database column names the STATUS BAR's stored
// tally derives from: tallyJobs reads Job.Status for the active counter, and
// parkedCookieJobs reads Job.Status and Job.Platform for the B1 parked badge.
//
// NOT a subset of displayColumns, and not meant to be — platform is not a
// display column. The gate below stays nested inside the hasDisplayChange
// branch, which is what makes that safe: UpdateJobFields never writes platform
// today, and if it ever did it would arrive alongside a display column.
//
// Why a named set rather than the two keys spelled out at the call site, which
// is what this replaces: the gate is coupled to what tallyJobs and
// parkedCookieJobs actually read, and nothing checked that coupling. A new
// field in barJobCounts would have left the bar frozen for whatever column
// feeds it, silently. TestTallyColumnsMatchWhatTheTallyReads now pins the set
// in both directions against the derivation itself.
//
// platform earns its place even though nothing emits it: parkedCookieJobs
// reads it, so it is a real input, and dropping it because no writer exists
// today is the exact mutant that test names.
var tallyColumns = map[string]struct{}{
	"status":   {},
	"platform": {},
}

// hasTallyChange reports whether any column in changes is one the status
// bar's stored tally derives from. The twin of hasDisplayChange, and the gate
// on the one SetJobs call in handleJobUpdate.
func hasTallyChange(changes []string) bool {
	for _, col := range changes {
		if _, ok := tallyColumns[col]; ok {
			return true
		}
	}
	return false
}
```

Then in `handleJobUpdate`, replace the gate and the tail of its comment. Replace these lines (currently 1259–1271, starting at the `// Gated on exactly the columns` line and ending at the closing brace of the `if`):

```go
		// Gated on exactly the columns tallyJobs derives from, so a real
		// transition costs one walk and a progress tick costs none: progress
		// is not a display column, so it never reaches this branch at all,
		// and the gate keeps the other eleven that do (title, filename, …)
		// off the tally too. platform is here because parkedCookieJobs reads
		// it; it is not itself a display column, so it only ever arrives
		// alongside one, and status is the key that fires in practice.
		//
		// handleTrimsChanged is the other UpdateJob caller and deliberately
		// has no such call: a trim write touches neither column.
		if slices.Contains(ev.Changes, "status") || slices.Contains(ev.Changes, "platform") {
			a.statusBar.SetJobs(a.taskList.Jobs())
		}
```

with:

```go
		// Gated on tallyColumns — exactly the columns tallyJobs and
		// parkedCookieJobs derive from — so a real transition costs one walk
		// and a progress tick costs none: progress is not a display column,
		// so it never reaches this branch at all, and the gate keeps the
		// other eleven that do (title, filename, …) off the tally too. The
		// set is a named home rather than two keys typed out here so the
		// coupling to the derivation is checkable; status is the key that
		// fires in practice.
		//
		// handleTrimsChanged is the other UpdateJob caller and deliberately
		// has no such call: a trim write touches neither column.
		if hasTallyChange(ev.Changes) {
			a.statusBar.SetJobs(a.taskList.Jobs())
		}
```

`slices` stays imported — `slices.Clone` at line 282 still uses it. Confirm by build, not by eye.

- [ ] **Step 4: Run the tests and see them GREEN**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestTallyColumnsMatchWhatTheTallyReads|TestHasTallyChange|TestTallyJobsCountsOnce|TestJobStatusTransitionRetalliesTheStatusBar|TestNonTallyUpdatesLeaveTheStatusBarTallyAlone|TestHasDisplayChange|TestDisplayColumnsCoverage' -v ./internal/tui/
```

Expected: `ok github.com/vampiricwulf/Moombox/internal/tui`, with all fourteen `TestTallyColumnsMatchWhatTheTallyReads` subtests, all nine `TestHasTallyChange` subtests, and the five pre-existing tests passing unchanged. `TestJobStatusTransitionRetalliesTheStatusBar` and `TestNonTallyUpdatesLeaveTheStatusBarTallyAlone` are the behavioural witnesses that the rewiring changed nothing.

- [ ] **Step 5: Verify the mutants by execution**

Two of the three, by hand, reverting by hand each time (no `git checkout`, no `git stash`).

**M1 — `platform` deleted from the set.** In `internal/tui/app_update.go`, delete the `"platform": {},` line from `tallyColumns`, then:

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestTallyColumnsMatchWhatTheTallyReads' ./internal/tui/   # expect FAIL
```

Expected: `--- FAIL: TestTallyColumnsMatchWhatTheTallyReads/platform` with `tallyColumns["platform"] = false but the derivation says the tally does follow it`, plus the count check firing (`tallyColumns has 1 entries but only 2 probed columns move the tally`). Restore the line and re-run — expect `ok`.

**M3 — the gate dropped.** In `handleJobUpdate`, change `if hasTallyChange(ev.Changes) {` to `if true {`, then:

```bash
go test -count=1 -timeout 300s -run 'TestNonTallyUpdatesLeaveTheStatusBarTallyAlone' ./internal/tui/   # expect FAIL
```

Expected: `--- FAIL: TestNonTallyUpdatesLeaveTheStatusBarTallyAlone/title_rewrite` — `a title rewrite re-tallied the whole job list: counts.active is 200, want the untouched sentinel -1`. Restore `hasTallyChange(ev.Changes)` and re-run — expect `ok`.

**M2 (recorded, not run):** adding `"title": {},` to `tallyColumns` fails both `TestTallyColumnsMatchWhatTheTallyReads/title` (the set half) and `TestNonTallyUpdatesLeaveTheStatusBarTallyAlone/title_rewrite`. Record the reasoning; M1 and M3 already exercise both halves.

- [ ] **Step 6: Package gates, including the FrameCost pins**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./internal/tui
go vet ./internal/tui/
staticcheck ./internal/tui/
go test -count=1 -timeout 300s ./internal/tui/
go test -count=5 -timeout 600s -v -run TestFrameCostAtLogCap ./internal/tui/
go test -count=1 -timeout 600s -race -run 'Tally|StatusBar|FrameCost|Cache' ./internal/tui/
```

Expected: `gofmt -l` and `staticcheck` print nothing; `./internal/tui/` `ok`; `TestFrameCostAtLogCap` PASS on all five iterations with `cached frame: ≤ 60 allocs/op` and `cached status bar: ≤ 2 allocs/op` logged every time (observed on main: 30 and 0 — record the five pairs); no `WARNING: DATA RACE`. Run the FrameCost pin WITHOUT `-race`: it counts allocations, and the race detector's own allocations would swamp the measurement.

- [ ] **Step 7: Commit**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
git add internal/tui/app_update.go internal/tui/frame_counts_test.go
git commit -m "refactor(tui): name the status-bar tally's input columns

Arc C's fix gated handleJobUpdate's statusBar.SetJobs call on two keys typed
out beside the call. The keys are right, but nothing tied them to the
derivation they mirror — tallyJobs reads Job.Status and parkedCookieJobs reads
Job.Status and Job.Platform — so a future field in barJobCounts would have left
the bar frozen for whatever column feeds it, silently, with every test green.

The set is now named (tallyColumns, beside displayColumns), the gate reads it
through hasTallyChange, and a test pins the set in BOTH directions against the
derivation itself: fourteen columns, each mutated on a fixture the tally
counts, each required to agree with its membership. platform stays in the set
because parkedCookieJobs reads it, even though UpdateJobFields never writes it
today — dropping it for that reason is one of the mutants the test names.

Behaviour is unchanged on every input, and the gate stays nested inside the
hasDisplayChange branch: one map lookup where up to two slices.Contains scans
ran, nothing added to any frame. FrameCost 60 / 2 green on -count=5.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/app_update.go internal/tui/frame_counts_test.go
```

---

### Task 3: `release()` zeroes `b.bytes`

Spec §0 item 3. `reorderBuffer.release()` (`internal/engine/downloader_parallel.go:248`) hands the whole outstanding reservation back to the process-wide budget, drops the buffer from the registry, and — since Arc B's close wave — `clear(b.seg)`s the segment map so the bytes the budget just took back do not stay reachable. It does not zero `b.bytes`, the per-buffer resident counter, so `residentBytes()` (`:263`) keeps reporting whatever was resident at teardown for a buffer holding an empty map.

Nothing in production reads `residentBytes()` today — it is a tests-and-diagnostics accessor — which is exactly why this is a residual rather than a bug report. It is still wrong, and the counter is one line from being right.

**Files:**
- Modify: `internal/engine/downloader_parallel.go`
- Modify: `internal/engine/reorder_budget_test.go`

**Interfaces:**
- Consumes: the existing `newReorderBudget`, `newReorderBufferOn` (`downloader_parallel.go:119`) and the test helper `budgetReserved` (`reorder_budget_test.go:13`).
- Produces: nothing new. `release()`, `residentBytes()`, `admit`, `take`, `has` all keep their exact signatures.

**Safety, stated so the reviewer does not have to re-derive it.** `b.bytes` is only ever decremented in `take()`, under `b.mu`, and only when the segment was found in `b.seg`. `release()` clears `b.seg` in the same critical section it zeroes `b.bytes` in, so a late `take()` finds nothing and never subtracts from the zeroed counter — the counter cannot go negative. `admit()` returns false before reading `b.bytes` once `released` is set, so the `b.bytes < b.limit` arm is unreachable after a release. The budget accounting is untouched: `reserved` is zeroed as before and `budget.free(freed)` is still called with the pre-zero value outside the lock.

- [ ] **Step 1: Write the new assertions**

In `internal/engine/reorder_budget_test.go`, extend `TestTakeAndReleaseFreeExactlyWhatThisBufferReserved` (line 236). Add one mutant line to its doc comment, after the existing `clear(b.seg)` mutant:

```go
// MUTANT: release() clears b.seg and the reservation but leaves b.bytes —
// residentBytes() then reports 4 KiB resident for a buffer holding an empty
// map, and every later reader of it is lied to.
```

Then add three assertions inside the body. After the existing post-release `budgetReserved` check and before the `a.has(2)` check:

```go
	if got := a.residentBytes(); got != 0 {
		t.Errorf("residentBytes after release = %d, want 0 — release hands the whole reservation back "+
			"and clears b.seg, so the per-buffer byte counter must go with them", got)
	}
```

After the idempotent second release's `budgetReserved` check:

```go
	if got := a.residentBytes(); got != 0 {
		t.Errorf("residentBytes after a second release = %d, want 0", got)
	}
```

And after the post-release `a.take(2)`'s `budgetReserved` check:

```go
	if got := a.residentBytes(); got != 0 {
		t.Errorf("residentBytes after a post-release take = %d, want 0 — the cleared map means take "+
			"finds nothing to subtract, so the counter must not drift in either direction", got)
	}
```

The three existing `budgetReserved` assertions stay exactly as they are — the budget accounting must not move, and leaving them in place is how the task says so.

- [ ] **Step 2: Run the test and see it RED**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestTakeAndReleaseFreeExactlyWhatThisBufferReserved' -v ./internal/engine/
```

Expected: **FAIL** with all three new assertions firing and the arithmetic exactly as follows. `seg` is `1 << 10` = 1024. `admit(0, 1024)` + `admit(1, 2048)` + `admit(2, 3072)` leaves `bytes = 6144`; `take(1)` drops it to `4096`; `release()` leaves it there today. So:

```
    residentBytes after release = 4096, want 0 — release hands the whole reservation back and clears b.seg, so the per-buffer byte counter must go with them
    residentBytes after a second release = 4096, want 0
    residentBytes after a post-release take = 4096, want 0 — the cleared map means take finds nothing to subtract, so the counter must not drift in either direction
```

The three `budgetReserved` assertions must all still PASS in this run — if any of them fails, stop: something other than this residual is wrong with the accounting.

- [ ] **Step 3: Zero the counter**

In `internal/engine/downloader_parallel.go`, in `release()`, add one line and one doc-comment sentence. The body becomes:

```go
func (b *reorderBuffer) release() {
	b.mu.Lock()
	b.released = true
	freed := b.reserved
	b.reserved = 0
	clear(b.seg)
	b.bytes = 0
	b.mu.Unlock()
	if freed > 0 {
		b.budget.free(freed)
	}
	b.budget.unregister(b)
	b.cond.Broadcast()
}
```

And append to the doc comment, after the paragraph that ends "…holds a reference to this buffer.":

```go
// bytes goes with the map, in the same critical section: it is the resident
// total, and a cleared map holds nothing, so leaving it behind only meant
// residentBytes() reporting bytes for an empty buffer. Zeroing it cannot
// drive the counter negative either — take() subtracts only for a segment it
// found, and after release there are none.
```

- [ ] **Step 4: Run the test and see it GREEN**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestTakeAndRelease|TestAnUnbounded|residentBytes' -v ./internal/engine/
go test -count=1 -timeout 600s ./internal/engine/
```

Expected: `ok github.com/vampiricwulf/Moombox/internal/engine` for both. `TestAnUnboundedPerJobCeilingAdmitsEveryNonHead` is the other `residentBytes()` reader in this file (`reorder_budget_test.go:292`) and does not call `release()` before asserting; `TestAnUnboundedBudgetAdmitsEveryNonHead` runs beside it as its pair. Both must be unaffected. The whole-package run (~25 s) is the one that catches the `downloader_parallel_test.go` readers at lines 83, 124, 165 and 445.

- [ ] **Step 5: Verify the pin's teeth by execution**

Remove the `b.bytes = 0` line by hand, re-run:

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 300s -run 'TestTakeAndReleaseFreeExactlyWhatThisBufferReserved' ./internal/engine/   # expect FAIL
```

Expected: the same three `residentBytes after … = 4096, want 0` lines from Step 2. Restore the line by hand and re-run — expect `ok`. Record the observed failure lines in the task report.

- [ ] **Step 6: Package gates**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./internal/engine
go vet ./internal/engine/
staticcheck ./internal/engine/
go test -count=1 -timeout 900s -race ./internal/engine/
git diff --stat -- internal/engine/
```

Expected: `gofmt -l` and `staticcheck` print nothing; the `-race` run `ok` with no `WARNING: DATA RACE` (the changed line is inside an existing critical section, and this is the gate that says so). The `git diff --stat` must show exactly two files — `downloader_parallel.go` and `reorder_budget_test.go` — and **must not** show `downloader_resume.go`: the engine's `saveResume` is out of scope by standing constraint.

- [ ] **Step 7: Commit**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
git add internal/engine/downloader_parallel.go internal/engine/reorder_budget_test.go
git commit -m "fix(engine): release() zeroes the reorder buffer's resident byte count

Arc B's close wave added clear(b.seg) to reorderBuffer.release() so a torn-down
buffer stops holding the bytes it just handed back to the shared budget. The
per-buffer counter was left behind, so residentBytes() kept reporting the last
resident total for a buffer whose map is empty.

Zeroed in the same critical section as the clear and the reservation. It cannot
drive the counter negative: take() subtracts only for a segment it found, and
after a release there are none — a post-release take is asserted to leave the
counter at zero, alongside the second (idempotent) release.

The budget accounting is untouched; the three budgetReserved assertions in the
same test are unchanged and still green.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/engine/downloader_parallel.go internal/engine/reorder_budget_test.go
```

---

### Task 4: Diagnose the cold `-race` failure in `render_cache_test.go`

Spec §0 item 4. **Diagnose before fixing.** This task may end with a production fix, a test-only fix, or no code change beyond a sharper failure message — the branch table in Step 4 decides, and the evidence decides the branch.

**The observation, in full.** At Arc C's close (`.superpowers/sdd/2026-09-24-followup-c-core/close-wave-review.md`, "Flake note"): the very first `-race -count=3` combined invocation of that session failed in `internal/tui`; "the tail was a blank rounded-border panel dump — a cached-vs-fresh frame comparison". It did not recur in 19 further combined runs, 5 tui-only rounds, 60 targeted `-race` iterations of `CachedFramesMatchFreshFrames|WallClockSecond|IsCachedBetweenIdenticalFrames`, or 3 rounds of `-count=6 -v` under deliberate concurrent load; a cold first `-race` run of the same command on the BASE export also passed. Arc C touched no file in that family.

**What the shape tells us before any command is run.** Only one helper in `internal/tui/render_cache_test.go` emits a TWO-frame `cached:` / `fresh:` dump — `runSteps` (line 237), used by exactly two tests, `TestTaskListCachedFramesMatchFreshFrames` (272) and `TestJobDetailsCachedFramesMatchFreshFrames` (383). A "blank rounded-border panel" is the shape of the details panel with no job (`{"no job"}`, the last step of the details test) or the empty task list (`{"empty"}` / `{"setup flag"}`, the last two steps of the task-list test). If both halves of the dump looked blank, the difference was in ANSI bytes that `stripANSI` removed — which is a fact the current failure message cannot express, and which Step 5's branch (c) fixes.

**The standing hypothesis, and the one thing that would make it true.** Both panels' cache keys carry `time.Now().Unix()` (`taskListKey` at `task_list.go:1018`, `jobDetailsKey` at `job_details.go:931`), and `runSteps` is the ONE cached-vs-fresh observation in that file **not** wrapped in `observeInOneSecond` — every other one is (lines 48, 129, 155, 181, 456, plus `archive_threshold_test.go:104` and `frame_cost_test.go:209`). Under `-race` a render is an order of magnitude slower, so a `got`/`want` pair straddling a second boundary goes from vanishingly unlikely to merely rare. That only produces a MISMATCH, though, if something in the rendered bytes is second-dependent, and a static read says it may not be for these two fixtures: the task list's only live clock read is `renderHeader`'s `renderTimer` (`task_list.go:1120-1133`, `time.Until` → `formatCountdown`), and the fixture leaves the three countdowns either zero (not rendered) or at `MonitorCheckingTime()` (a static "…"); the details panel bakes its "5m ago" suffixes into the viewport rows at `buildRows` time, which is why `jobDetailsKey`'s own comment calls `sec` "insurance, not a live dependency". **Step 3 settles this by experiment rather than by reading.**

**The expected outcome is already known: branch (c).** The plan evaluation (opus, 2026-09-24, `.superpowers/sdd/2026-09-24-followup-x-residuals/plan-review.md`) ran this task's diagnosis in a disposable export and selected branch (c) by execution: 10/10 cold `-race` rounds of `render_cache_test.go` green after `go clean -testcache`, `-count=50` warm green, two cold whole-package unfiltered `-race` rounds green, no `WARNING: DATA RACE` anywhere, and the forced 1100 ms boundary of Step 3 passing all 36 step pairs (`TestTaskListCachedFramesMatchFreshFrames` 27.5 s PASS, `TestJobDetailsCachedFramesMatchFreshFrames` 12.1 s PASS). **Run Steps 1–3 anyway and record your own numbers** — the point of the diagnosis is the evidence, and a machine that behaves differently is itself the finding — but expect the (c) row and treat a branch-(a) or branch-(b) result as something to re-verify before acting on.

**Ruling (controller, 2026-09-24): branch (c) may sharpen the failure message — test-only, pin-neutral — because a diagnosable next failure is the evidence the spec asks to record.**

**Files:**
- Modify (branch a or c): `internal/tui/render_cache_test.go`
- Branch (b) only: whatever production file the race report names — and the task STOPS and reports before editing it.

**Interfaces:**
- Consumes: `observeInOneSecond` / `inSameSecond` (`render_cache_test.go:19,25`), `stripANSI` (`status_bar_test.go:19`), the `renderStep[M]` type and the two call sites of `runSteps`.
- Produces: branch-dependent; each branch's exact code is below.

- [ ] **Step 1: Static enumeration — write down every clock read a frame can reach**

No test runs yet. Produce a table in the task report with one row per wall-clock read reachable from `TaskListModel.View()` and `JobDetailsModel.View()`, and for each: the declaring file and line, whether it is read at RENDER time or baked at REBUILD time, and whether the corresponding fixture in `runSteps`' two callers activates it.

Start from:

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
grep -n 'time\.Now()\|time\.Until\|time\.Since' internal/tui/task_list.go internal/tui/job_details.go internal/tui/status_bar.go
```

The rows this plan already established, to be confirmed or corrected:

| Read | Where | When | Active in the fixture? |
|---|---|---|---|
| `time.Until(next)` → `formatCountdown` | `task_list.go:1128-1133` (`renderHeader`) | RENDER | No — the three `Next*Check` fields are zero (`renderTimer` returns `ok=false`) until the three monitor steps set them to `MonitorCheckingTime()`, which renders a static "…" |
| `archiveCutoff(…, time.Now())` | `task_list.go:832` (`archiveBucketsDirty`), `:867` (`rebuildVirtualList`), `:1199` (`buildStatusSummary`) | REBUILD | Yes, but the values are day-scale; the "archive sweep" step asserts `ResweepArchive()` returned false |
| `time.Now().Unix()` | `task_list.go:1018`, `job_details.go:931` | key only | Yes — this is the key field, not rendered content |
| `now := time.Now()` → `formatDateStrRelative` | `job_details.go:587`, `:700` (`buildRows`) | REBUILD (baked into `m.rows`) | Yes, but baked: `defeat()` clears only `renderCache`, never the rows |
| none | `status_bar.go` (`statusBarKey` carries NO clock, deliberately) | — | n/a |

If the table comes back with a RENDER-time, fixture-active clock read that this plan missed, branch (a) is already proven and Step 3 merely confirms it.

- [ ] **Step 2: Try to reproduce, warm and cold**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp

# Warm, targeted, high iteration count.
go test -count=200 -timeout 1800s -race -run 'CachedFramesMatchFreshFrames' ./internal/tui/

# Cold-ish and UNFILTERED, which is the shape that failed: the whole package,
# a fresh test binary each time, 30 rounds. Stops at the first failure.
go clean -testcache
for i in $(seq 1 30); do
  echo "=== round $i ==="
  go test -count=1 -timeout 900s -race ./internal/tui/ || break
done
```

Record: the number of rounds run, any failure verbatim (the whole `cached:` / `fresh:` dump and the step name), and whether any run printed `WARNING: DATA RACE`. Do NOT run `go clean -cache` — it is shared with every other Go process on this host and buys nothing here; `-testcache` plus `-count=1` already gives a fresh binary and no cached results.

- [ ] **Step 3: The decisive experiment — force the second boundary**

This is what settles branch (a) in about a minute, and it is the step that makes the rest of the task a decision rather than a guess. By hand (no `git checkout`, no `git stash`), in `internal/tui/render_cache_test.go`, inside `runSteps`, change:

```go
		got := view() // what the operator sees
		defeat()
		want := view() // the same frame with the cache defeated
```

to:

```go
		got := view() // what the operator sees
		time.Sleep(1100 * time.Millisecond) // TEMPORARY: force a second boundary
		defeat()
		want := view() // the same frame with the cache defeated
```

(`time` is already imported by this file.) Then:

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 900s -v -run 'CachedFramesMatchFreshFrames' ./internal/tui/
```

Roughly 26 + 11 steps × 1.1 s ≈ 41 s. Every pair now straddles at least one second boundary, so:

- **Any step FAILS** → the frames ARE second-dependent, the flake is branch (a), and the failing step names the render that moves. Record the step name and the dump.
- **Everything PASSES** → wall-clock dependence is ruled out for these two fixtures however slow the machine gets, and branch (a) is dead.

Remove the `time.Sleep` line by hand and re-run the same command to confirm the file is back to green before going on. Record both outcomes.

- [ ] **Step 4: Rule — the branch table**

Pick exactly one branch from the evidence in Steps 1–3 and record the reasoning in the task report before writing any code.

| Evidence | Branch | Action |
|---|---|---|
| Step 3's forced boundary makes a step fail (or Step 1 found a render-time, fixture-active clock read) | **(a) timing-sensitive assertion** | Apply the `runSteps` wrap in Step 5a. Test-only, no production file. Then apply Step 5a's own GREEN check: the sleep moves INSIDE the `observeInOneSecond` closure and the helper must give up with `20 attempts all straddled a second boundary` — the guard refusing to compare, which is the fix working. Remove the sleep, confirm green, then run Step 6. |
| Any run in Step 2 printed `WARNING: DATA RACE` | **(b) a real race** | STOP. Do not edit anything. Follow `superpowers:systematic-debugging`, capture both stacks from the report, and hand the finding to the controller before touching a production file — a race here is outside the four residuals' scope and the owner decides whether this arc carries the fix. If the controller says go: fix the PRODUCTION side, never the assertion; if the race is a goroutine leaked by an earlier test in the package, the leak is the bug. |
| Step 3 passes, Step 2's 200 warm iterations and 30 cold unfiltered rounds are all green, and Step 1 found no render-time fixture-active clock read | **(c) not reproducible** | No production change and no assertion change. Apply the failure-message sharpening in Step 5c so the next occurrence is diagnosable, and record the full evidence. |

**Ruling (recorded here so the implementer does not have to decide it, and CONFIRMED by the controller on 2026-09-24 — see the sentence in this task's intro):** branch (c) is not "do nothing". The spec says "record the evidence and leave it", and the message change leaves every assertion, every pin and every behaviour exactly as it is — it changes only what a failure PRINTS (verified in the evaluation: FrameCost 30/0 unchanged, all pins green, staticcheck clean). Without it the next occurrence produces the same undiagnosable artefact the first one did (two stripped dumps that look identical, because the difference was in bytes `stripANSI` removed). Cost if this ruling is wrong: six lines of test-only formatting code that never runs on a green suite.

- [ ] **Step 5a: BRANCH (a) — observe each pair inside one wall-clock second**

`observeInOneSecond` retries its closure, so the observation must be exactly repeatable: the memo has to be back in its pre-observation state at the top of each attempt. `runSteps` therefore takes a pointer to the model's memo field instead of a `defeat` closure. Replace the whole of `runSteps` (line 237) with:

```go
// runSteps drives one model through its steps, asserting after each that the
// frame the operator sees (memoised) is byte-for-byte the frame a renderer
// with no cache would produce. Equality across every step is also the
// byte-identity claim against the pre-change renderer, which is exactly the
// cache-defeated path. view is the model's View; cache points at its memo
// field — the two panels have no common interface, and &m.renderCache is
// cheaper than inventing one.
//
// Both panels' keys carry the wall-clock second, so a got/want pair that
// straddles a second boundary compares two frames the renderer was entitled
// to disagree about. observeInOneSecond is this file's existing guard for
// exactly that, and every other cache observation here already uses it;
// restoring warm at the top of the closure is what makes a retry an exact
// repeat rather than an observation of an already-defeated cache.
func runSteps[M any](t *testing.T, m M, steps []renderStep[M], view func() string, cache *string) {
	t.Helper()
	for _, step := range steps {
		before := view() // warm the cache on the pre-step frame
		step.apply(m)
		warm := *cache // still the pre-step frame: apply() renders nothing

		var got, want string
		observeInOneSecond(t, func() {
			*cache = warm
			got = view() // what the operator sees
			*cache = ""
			want = view() // the same frame with the cache defeated
		})

		if got != want {
			t.Errorf("%s: the memoised frame differs from a fresh render\ncached:\n%s\nfresh:\n%s",
				step.name, stripANSI(got), stripANSI(want))
		}
		if step.covers != "" && want == before {
			t.Errorf("%s: the step renders identically, so it cannot cover %s — the claim is vacuous",
				step.name, step.covers)
		}
	}
}
```

Both call sites change from `runSteps(t, m, steps, m.View, func() { m.renderCache = "" })` to:

```go
	runSteps(t, m, steps, m.View, &m.renderCache)
```

(line 370 in `TestTaskListCachedFramesMatchFreshFrames`, line 435 in `TestJobDetailsCachedFramesMatchFreshFrames`).

GREEN check for this branch: re-apply Step 3's `time.Sleep(1100 * time.Millisecond)` by hand — this time INSIDE the `observeInOneSecond` closure, between `got = view()` and `*cache = ""` — and run `go test -count=1 -timeout 900s -run 'CachedFramesMatchFreshFrames' ./internal/tui/`. Expected: the helper's own retry gives up after 20 straddled attempts and reports `20 attempts all straddled a second boundary`, which is the guard working as designed (it refuses to compare rather than comparing wrongly). Remove the sleep by hand and confirm green.

- [ ] **Step 5c: BRANCH (c) — make the next occurrence diagnosable**

Append two helpers to `internal/tui/render_cache_test.go`, immediately after `runSteps`:

```go
// firstDiff returns the index of the first byte at which a and b differ, or
// min(len(a), len(b)) when one is a prefix of the other.
func firstDiff(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// diffWindow quotes up to 40 bytes of a around its first difference from b.
// The stripped dumps runSteps prints are unreadable when the difference is an
// ANSI escape rather than text — which is the one shape a pair of frames that
// both LOOK blank can take, and the shape the single observed failure of this
// family had (the post-sweep-2 Arc C close review, 2026-09-24). Quoting the
// raw bytes is what turns a second occurrence into a diagnosis.
func diffWindow(a, b string) string {
	i := firstDiff(a, b)
	lo := max(i-20, 0)
	hi := min(i+20, len(a))
	return a[lo:hi]
}
```

And change `runSteps`' mismatch branch (only the `t.Errorf` call — nothing else in the function moves) to:

```go
		if got != want {
			t.Errorf("%s: the memoised frame differs from a fresh render at byte %d (cached %d bytes, fresh %d)\n"+
				"cached bytes: %q\nfresh bytes:  %q\ncached:\n%s\nfresh:\n%s",
				step.name, firstDiff(got, want), len(got), len(want),
				diffWindow(got, want), diffWindow(want, got),
				stripANSI(got), stripANSI(want))
		}
```

Verify the new message by execution: by hand, change `want := view()` to `want := view() + "\x1b[0m"`, run `go test -count=1 -run 'TestJobDetailsCachedFramesMatchFreshFrames' ./internal/tui/`, and confirm the output names a byte offset and quotes the escape in the `fresh bytes:` window while both stripped dumps look identical — the exact artefact the observed failure produced. Revert by hand, re-run, confirm green. Record the observed message.

- [ ] **Step 6: Package gates (branches a and c alike)**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./internal/tui
go vet ./internal/tui/
staticcheck ./internal/tui/
go test -count=1 -timeout 300s ./internal/tui/
go test -count=20 -timeout 1800s -race -run 'CachedFramesMatchFreshFrames|WallClockSecond|IsCachedBetweenIdenticalFrames' ./internal/tui/
go test -count=5 -timeout 600s -v -run TestFrameCostAtLogCap ./internal/tui/
```

Expected: `gofmt -l` and `staticcheck` silent; `./internal/tui/` `ok`; the 20 `-race` iterations green with no `WARNING: DATA RACE`; `TestFrameCostAtLogCap` PASS on all five with 60 / 2 intact (this task changes no production code, so the numbers must match Task 2's to the allocation).

- [ ] **Step 7: Commit (branches a and c; branch (b) commits nothing without the controller)**

Branch (a):

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
git add internal/tui/render_cache_test.go
git commit -m "test(tui): observe each cached-vs-fresh pair inside one wall-clock second

runSteps was the one cached-vs-fresh observation in render_cache_test.go not
wrapped in observeInOneSecond, and both panels' cache keys carry
time.Now().Unix(). <the Step 3 finding, one or two sentences: which step's
render moves with the second, and the dump it produced under a forced
boundary>. Under -race a render is an order of magnitude slower, which is why
this surfaced once on a cold run and never again.

The guard is the file's own: the got/want pair renders inside one second, and
the memo is restored to its pre-observation value at the top of each retry so
a retry is an exact repeat rather than an observation of an already-defeated
cache. runSteps takes the memo field by pointer instead of a defeat closure to
make that possible. Test-only; no assertion, pin or production path moves.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/render_cache_test.go
```

Branch (c):

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
git add internal/tui/render_cache_test.go
git commit -m "test(tui): name the differing byte when a memoised frame diverges

The one cold -race failure this family has ever produced (post-sweep-2 Arc C
close review) printed two stripped frame dumps that both looked like a blank
rounded-border panel — so the difference was in bytes stripANSI removes, and
the artefact said nothing about what moved. Diagnosis found no reproduction:
<the Step 2 and Step 3 counts, one sentence>, and a forced second boundary
between the memoised and the fresh render changes no frame, so the wall-clock
key is not the cause for these fixtures.

No production change and no assertion change. The mismatch message now names
the first differing byte, both lengths, and quotes 40 raw bytes of each frame
around it, so a second occurrence is a diagnosis instead of another blank
panel. Verified by injecting an ANSI-only difference.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- internal/tui/render_cache_test.go
```

Fill the `<…>` slots from observed output before running; no angle brackets may survive into a commit message.

---

### Task 5: Arc gates, then delete this plan

Spec §1's merge-candidate gates over a branch that has absorbed `main`. Nothing else in the chain is in flight, so the merge is expected to be a no-op — run it anyway, because that expectation is what the gate confirms.

**Files:**
- Delete: `docs/superpowers/plans/2026-09-24-followup-x-residuals.md`

**Interfaces:**
- Consumes: Tasks 1–4's commits.
- Produces: a green merge candidate for the controller to merge `--no-ff` into `main`.

- [ ] **Step 1: Merge main**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
git log --oneline -3 main
git merge main
```

Expected: `Already up to date.` — `main` is at `ad1a8bdf`, which is this branch's base, and no other arc is running. If the merge reports anything else, read what came in before continuing: this arc's file set is `internal/worker/orchestrator_mux*.go`, `internal/tui/app_update.go`, `internal/tui/frame_counts_test.go`, `internal/tui/render_cache_test.go`, `internal/engine/downloader_parallel.go`, `internal/engine/reorder_budget_test.go`. A CONFLICT means something landed on `main` inside that set — stop and report it to the controller rather than resolving it here.

- [ ] **Step 2: The merge-candidate gates**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./cmd ./internal ./tools
go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...
staticcheck ./...
go build ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/moombox
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./cmd/moombox
go mod verify
go mod tidy -diff && echo TIDY-CLEAN
node --test web/tests/*.test.mjs
go test -count=1 -timeout 300s ./internal/docs/
```

Expected: `gofmt -l` prints nothing; vet/staticcheck/builds silent; `go mod verify` prints `all modules verified`; `TIDY-CLEAN` printed; the node suite **0 fail** (242 pass on `main` at Arc U's close — record what it actually prints; this arc touches no JS, so any change is an environment finding, not a result); `ok` for `internal/docs`. The citation gate is run even though no `docs/spec/*.md`, `SPEC.md` or `CLAUDE.md` file was edited — verified by grep during planning that none of them cites `writeDescriptionAtomic`, `displayColumns`, `tallyColumns`, `residentBytes` or `reorderBuffer.release`, and this run is what says that is still true.

Also confirm the LF rule on every file the arc touched:

```bash
for f in internal/worker/orchestrator_mux.go internal/worker/orchestrator_mux_test.go \
         internal/tui/app_update.go internal/tui/frame_counts_test.go \
         internal/tui/render_cache_test.go \
         internal/engine/downloader_parallel.go internal/engine/reorder_budget_test.go; do
  printf '%s ' "$f"; perl -0777 -ne 'print tr/\r//' "$f"; echo
done
```

Expected: `0` beside every path. (`grep -c $'\r'` and `awk '/\r/'` both lie about CRLF under Git Bash — only `perl -0777 -ne 'print tr/\r//'` is trustworthy here.)

- [ ] **Step 3: The ONE full suite — controller-run**

Spec §1: implementers never run the full suite; the controller's gate runner does, one at a time across the chain. Hand off to the controller. Do not run this yourself under any circumstances (spec §1) — the command block below is the controller's.

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 ./...
```

Expected: 32 `ok` / 0 `fail` (four packages have no test files: `cmd/sign`, `internal/bgutils/embed`, `tools/sidecar-sig-probe`, `web`).

- [ ] **Step 4: Unfiltered `-race` over the arc's three packages**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=1 -timeout 1800s -race ./internal/worker/ ./internal/tui/ ./internal/engine/
```

Expected: `ok` for each, no `WARNING: DATA RACE`. Run it at least twice and record both, because `./internal/tui/` under `-race` is precisely the invocation Task 4 was diagnosing — a failure here is Task 4's finding recurring, and the task report must say which branch it lands in.

- [ ] **Step 5: The FrameCost pins, five times**

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go test -count=5 -timeout 600s -v -run TestFrameCostAtLogCap ./internal/tui/
```

Expected: PASS ×5, with `cached frame: N allocs/op` ≤ 60 and `cached status bar: N allocs/op` ≤ 2 on every iteration, and `git diff main -- internal/tui/frame_cost_test.go` empty (the budgets were never edited). Record all five pairs. No `-race` on this one.

- [ ] **Step 6: Delete this plan and commit**

The plan is implemented and verified; git history is the archive (standing project rule).

```bash
cd /d/Git/Moombox/.worktrees/followup-x-residuals
git rm docs/superpowers/plans/2026-09-24-followup-x-residuals.md
git commit -m "chore(plans): delete the Arc X plan — implemented and verified

The worker's description writer goes through utils.WriteFileAtomic, the status
bar's tally inputs are a named and pinned set, reorderBuffer.release() zeroes
its resident byte count, and the render-cache -race flake is diagnosed. Git
history is the archive.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq" -- docs/superpowers/plans/2026-09-24-followup-x-residuals.md
```

- [ ] **Step 7: Hand the candidate to the controller**

Report: Task 1's RED line and its measured Windows rename-failure timing, plus the `internal/worker` wall time; Task 2's five FrameCost pairs and the two mutants verified by execution (M1 and M3, with their failure lines); Task 3's three RED `residentBytes … = 4096` lines and the `git diff --stat` proving `downloader_resume.go` untouched; Task 4's branch, its full evidence table (Step 1's clock enumeration, Step 2's iteration counts, Step 3's forced-boundary outcome) and the mutant or message verified by execution; the LF zero-counts; and the complete gate transcript from Steps 2–5. The controller merges `--no-ff` into `main` without asking (standing ruling), runs the post-merge gates, then deletes the worktree **and** the `followup-x-residuals` branch.
