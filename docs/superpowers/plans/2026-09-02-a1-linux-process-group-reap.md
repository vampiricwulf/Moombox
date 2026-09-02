# A1 on Linux/Docker — the process-group reap: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Linux and Docker a real answer to "is the setup browser still alive?" — a process group where Windows has a Job Object — so the abandoned-setup reap (A1) fires there, and so every site that on Windows relies on closing the job to kill the browser kills it explicitly on Linux.

**Architecture:** `configureCmdSysProcAttr` gains `Setpgid: true` on Linux, so every browser this package launches leads a process group whose id is its own pid. A new file with **no build tag** (`internal/cookies/job_pgroup.go`) holds all of the counting and killing DECISIONS against two package-level function variables — `listProcessGroups` (pid → pgid) and `killProcessGroup` (SIGKILL a group). `job_linux.go` binds those to a `/proc` walk and `syscall.Kill(-pgid, SIGKILL)` and forwards five thin methods to them; tests on Windows bind them to a fake process table and a recorder, which is how "unit-tested with a fake process table, no Linux box" is satisfied — every branch runs in the ordinary `go test ./...` on the machine this project is developed on. `job_windows.go` and `job_other.go` are not touched.

**Tech Stack:** Go 1.26, `syscall` (Linux `SysProcAttr.Setpgid`/`Pdeathsig`, `Kill`), `os.ReadDir`/`os.ReadFile` over `/proc`, standard `testing`. No new dependency, no new goroutine, no cgo.

**Spec:** `docs/superpowers/specs/2026-09-02-a1-linux-process-group-reap-design.md` (R1–R7, non-goals, invariants, the doc list). The owner's ruling it implements is `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2015`: *"BUILT this release, no Linux live gate… process group (`Setpgid` at launch), kill the group when the setup ends or is abandoned, same 60 s grace, same 'release only where releasing cannot kill' rule, unit-tested with a fake process table; a user's bug report is the gate."*

---

## Global Constraints

Every task's requirements implicitly include this section.

**From the spec's §4 (invariants) and §3 (non-goals):**

- A failed process-table read **never releases the slot and never kills**. It answers `known = false` ("cannot say"), which the reap already reads as "still running".
- `killProcessGroup` is **never** called with `pgid <= 0`. `kill(-0, …)` signals the caller's own process group — Moombox and everything it launched, and in Docker the container. Three checks enforce this: `pgroupJob.adopt` refuses `pid <= 0` before a group can exist, `pgroupJob.killGroup` refuses an unadopted group, and `sigkillProcessGroup` refuses at the syscall. The first two are pinned by tests that run on Windows; the third lives in `job_linux.go`, which no test on this machine executes, and is reviewed by eye.
- **No goroutine is added.** Every existing `recover()` posture is untouched; there is no reaper goroutine and one must never be added (`reapAbandonedSetupLocked`'s doc says why).
- **Tests mutate the claim for every assertion.** Each test in this plan names the mutation that must break it. The Windows-only test files (`autocookies_setup_reap_windows_test.go`, `autocookies_setup_job_windows_test.go`) are **not** the home of any new logic test.
- **No change to the Windows path.** `job_windows.go` and `job_other.go` are not edited by any task in this plan.
- No UI, no config, no schema, no route. No sidecar or launcher process-group work — `internal/bgutils/sidecar/job_linux.go` and `cmd/moombox/launcher_child_linux.go` keep `Pdeathsig` only.
- **No Linux live gate.** Nothing in this plan is verified on a Linux box; a field report is the gate. The `//go:build linux` smoke test is opt-in and is not run anywhere automatically.

**Session rules:**

- `const livenessRecoveryArmed = false` (`internal/cookies/refresh.go:748`) stays `false`. The pilot is disarmed and this plan does not arm it.
- `cmd/moombox/main.go:276-278` (the `AutoEnabled && len(Platforms) > 0` guard around `SetExpectedPlatforms`) is **no-touch**.
- Every goroutine carries an inline `defer func() { if r := recover(); r != nil { … } }()`. This plan adds none.
- The logger is the anonymous interface repeated inline — `Debug/Info/Warn/Error(msg string, args ...any)` — never extracted to a named interface (CLAUDE.md).
- **Never read or print any cookie file or value.** No test in this plan opens `cookies.txt`, `cookies.sqlite`, or a browser profile; no log line added here carries a cookie name or value.
- Package-variable seams follow the existing convention: tests swap them and restore every swap with `t.Cleanup`. ONE production site reassigns the three new ones — `job_linux.go`'s `init()` (Task 5), which is the platform binding and runs before any test — and every doc comment on those variables names that site rather than claiming nothing in production does.

**Gates — run all of these at the end of every task, before the commit:**

```bash
go build ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./...      # the Linux arm must compile
GOOS=linux GOARCH=arm64 go build ./...      # …on both architectures
gofmt -l internal/ cmd/                      # must print nothing
go test -count=1 ./...                       # once, all green
```

PowerShell form for the two cross-builds (this is a Windows workstation):

```powershell
$env:GOOS="linux"; $env:GOARCH="amd64"; go build ./...
$env:GOARCH="arm64"; go build ./...
Remove-Item Env:GOOS; Remove-Item Env:GOARCH
```

If `go build ./...` fails inside `internal/bgutils/embed` the embed blobs are missing from this checkout — see CLAUDE.md § "BotGuard sidecar embed prerequisites". Produce them once (`go run ./tools/fetch-node`, then the `bgutil-sidecar` build), or, if that is not possible in this environment, scope the four build gates to `./internal/cookies/...` and say so in the commit body. Never delete or stub the embed directives to make a gate pass.

**Commit trailers — every commit in this plan ends with:**

```
Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
```

---

## Decisions pinned before Task 1

These were settled by reading the tree at `ecb484f`. Do not re-litigate them mid-execution.

**1. `job_windows.go` gets no `//go:build windows` tag, and is not edited at all.** Its filename already IS the build constraint: Go applies an implicit GOOS constraint to any file whose name ends `_windows.go`, exactly as `_linux.go` and `_test.go` are implicit. An explicit tag would be redundant with the filename and would change nothing about what compiles. `job_linux.go` carries a redundant explicit tag today (belt and braces); `job_other.go`'s tag is **required**, because `_other` is not a GOOS. Leaving `job_windows.go` byte-identical is also what makes this arc reviewable: a reviewer can confirm "the Windows path did not change" by seeing the file absent from the diff.

**2. The new file is `internal/cookies/job_pgroup.go` and it has no build tag.** `pgroup` is not a GOOS or GOARCH, so the implicit-suffix rule does not apply and the file compiles on every target — which is the point: the decision logic must execute in the Windows test suite.

**3. Closing a Linux job kills nothing; the kill is asked for out loud.** This is R4, and the code gives it a second, decisive reason the spec draft does not state. `trackedSetupJob` (`autocookies.go:3272`) closes a job whose `assign` FAILED, deliberately and correctly: on Windows that job holds nothing, so the close is free. If a Linux `close()` killed, that same line would SIGKILL the process group containing the browser the user is signed into, because a bookkeeping call went wrong. A Windows Job Object is a kernel HANDLE — it cannot name a process that is not in it. A process group is an INTEGER the kernel recycles. The two cannot share one "closing kills" rule, so Linux gets an explicit `killTrackedProcesses(job)` at the three sites that need it and nothing at the site that must not have it.

**4. The group kill lands at the `killProcessTree` seam (`autocookies.go:3071`).** That one closure is reached from five call sites — `killSetupProcess` (`:3138`), `killRefreshProcess` (`:3170`), `refreshChromium`'s teardown defer (`autocookies_chromium.go:266`), `closeFirefoxGracefully` (`autocookies_firefox.go:161`) and `runWithTimeout`'s timeout arm (`autocookies_firefox.go:972`) — and on Linux **the tree IS the group**, because `Setpgid` makes the launched child its own group leader. One edit fixes all five, versus five edits that could drift. Its non-Windows arm today is `proc.Kill()`, which kills the launcher — the process that on the Firefox family exited ~170 ms after start and is not the browser. The new arm goes THROUGH `pgroupJob` (`adopt`, then `killGroup`) rather than handing the pid to `killProcessGroup` directly, so it inherits the same refusals: it signals only a group the pid currently leads and that currently has members, and falls back to the direct kill — today's behaviour exactly — when the table cannot be read, the pid is gone, or the pid sits in someone else's group. That matters because `proc.Kill()` has a protection a bare `kill(-pid)` lacks: Go refuses it on a process that has already been waited on (`os.ErrProcessDone`), and `killSetupProcess` reaches this arm with a reaped pid whenever no job could vouch for the browser — a blind `kill(-pid)` there could land on whatever group the kernel has since given that number to.

**5. Where Windows relies on close-to-kill and Linux must ask.** One row per site. "Linux arm" describes the tree after Task 5.

| Site | Windows today | Linux after this plan | Pinned by |
|---|---|---|---|
| `killSetupProcess` `autocookies.go:3122` | `killProcessTree` → `taskkill /F /T /PID`; **or** returns early when `browserExited && setupJob.queryable()`, on the stated premise that `cleanupLocked` closes the job a moment later and KILL_ON_JOB_CLOSE finishes the job | Same shape. The early return now fires on Linux too (`queryable()` is true once a group is adopted) — and its premise is kept true by `cleanupLocked` killing the group. When it does not return early, `killProcessTree` kills the group | `TestKillProcessTreeUnixKillsTheGroupNotJustTheLauncher`; `TestCleanupAsksForTheKillBeforeItDropsTheJob` |
| `cleanupLocked` `autocookies.go:3229` | `setupJob.close()` — KILL_ON_JOB_CLOSE is the kill | `killTrackedProcesses(s.setupJob)` first, then `close()` (which forgets a number) | `TestCleanupAsksForTheKillBeforeItDropsTheJob` |
| `runWithTimeout` teardown `autocookies_firefox.go:915-920` | deferred `job.close()` kills the headless browser on the drain-timeout arm and on the ctx-cancelled arm (the explicit-timeout arm already ran `killProcessTree` first) | the defer becomes `closeLaunchJob(job, logger)`: kill the group, then close | `TestCloseLaunchJobAsksForTheKill` |
| `adoptSetupJobLocked` `autocookies.go:3305` | closes a handle an earlier attempt left behind — deliberately a kill | `killTrackedProcesses` then close, same intent | `TestAdoptClosingAStaleJobAsksForTheKill` |
| `trackedSetupJob` `autocookies.go:3272` | closes a job whose assign failed; kills nothing because the job is empty | **must not kill** — decision 3 above | `TestTrackedSetupJobDoesNotAskForAKillWhenTheAssignFailed` |
| `Stop` `autocookies.go:2762` | `killSetupProcess` + `killRefreshProcess` + `cleanup` | unchanged — covered transitively by rows 1 and 2 | the two rows above |
| `CancelSetup` `autocookies.go:1457` | `killSetupProcess` + `cleanup` | unchanged — covered transitively | the two rows above |
| `FinishSetupDetailed`, Chromium branch `autocookies.go:1179` | `killSetupProcess`, then `cleanup` on the way out | unchanged — covered transitively | the two rows above |
| `refreshChromium` teardown `autocookies_chromium.go:246`/`:266` | deferred `killProcessTree(cmd.Process)` runs BEFORE the deferred `job.close()` (defers run LIFO) | the `killProcessTree` defer already kills the group; the close needs nothing | `TestKillProcessTreeUnixKillsTheGroupNotJustTheLauncher` |
| Both launchers' `cmd.Start()` failure close (`autocookies_firefox.go:83`, `autocookies_chromium.go:77`) | closes an empty job | nothing to do: `assign` was never called, `pgid` is 0, `close()` forgets 0 | `TestPGroupJobCannotAnswerWithoutAGroup` |

**6. What else changes on Linux, stated now rather than discovered in review.** Giving `activeProcesses()` a real count on Linux also arms `drainJob` there. Until now every Linux launch returned on lap zero with "no tracked processes to wait for"; after Task 5 `runWithTimeout` genuinely waits for the browser group to empty, up to `processTimeout`. That is the Arc 0 fix arriving on Linux and it is desirable. Say precisely what changes, because the Windows picture does not transfer: Mozilla's launcher process is a Windows feature (`operations.md`'s "launcher-handoff premise is UNMEASURED" paragraph), so on Linux the direct child of `runWithTimeout` is normally the browser itself, and `cmd.Wait()` already waits for it today — a Linux refresh has never returned instantly. What the count adds is a wait for whatever OUTLIVES the direct child: a wrapper's handoff (a snap or a shell shim), a content process still shutting down. Where nothing does, the drain still lands on lap zero exactly as before. It also lets `errBrowserDrainTimeout` reach `browserLaunchActed` there, which it never could while the count was a stub — the verdict itself is the screenshot's on every platform and is unchanged. Task 5 edits the drain paragraphs that say otherwise. This is a change the spec draft does not mention; see Self-Review.

**7. Named residual: pgid recycling.** A Job Object handle cannot name a process that is not in the job. A pgid can: the kernel recycles pids, so a group id we adopted could, after every member has exited, name an unrelated group whose leader happens to have that pid. `pgroupJob.killGroup` narrows the window as far as it can be narrowed — it refuses to signal when the table cannot be read, and refuses when the group is already empty, so a kill only ever fires at a group that currently has members — but it cannot close it. `killProcessTreeUnix` goes through the same refusals (decision 4), so this is ONE window, not two. Task 5 writes it into `operations.md` as a residual rather than leaving it for a field report to discover.

**8. A zombie is not a member.** A task that has exited but not been reaped keeps its `/proc/<pid>/stat` entry, state `Z`, with its `pgrp` field intact, so a naive count would keep it in the group. `parseProcStatPGID` reports `Z` (and `X`, dead) as unparsed. Two reasons. The direct child is a zombie between its exit and the `cmd.Wait()` that reaps it, and the smoke test counts inside that window. And in a container where Moombox is PID 1 — a bare `docker run` without `--init`; `docker-compose.yml:10` sets `init: true`, so the compose path is covered — an orphaned grandchild of a killed browser is reparented to Moombox, which reaps only the children it started, and that zombie would hold the count above zero for the life of the process: the reap would never fire after the one crash it exists for. A Windows Job Object's `ActiveProcesses` does not count terminated processes either.

---

## File structure

| File | Change | Responsibility |
|---|---|---|
| `internal/cookies/job_pgroup.go` | **create** (Task 1, extended in Task 3) | The tag-free decision layer: `pgroupJob`, `parseProcStatPGID`, the `listProcessGroups`/`killProcessGroup`/`killTrackedProcesses` seams |
| `internal/cookies/job_pgroup_test.go` | **create** (Tasks 1, 2) | Fake-process-table fixtures; every branch of the decision layer and of the non-Windows kill arm |
| `internal/cookies/autocookies.go` | modify (Tasks 2, 3, 4, 5) | `killProcessTree`'s arm; the three close-to-kill sites; `browserGoneFrom`; the comments that say the reap is Windows-only |
| `internal/cookies/autocookies_firefox.go` | modify (Tasks 3, 5) | `closeLaunchJob`; the launch-site and drain comments |
| `internal/cookies/autocookies_chromium.go` | modify (Task 5) | the two launch-site comments |
| `internal/cookies/autocookies_setup_reap_pgroup_test.go` | **create** (Tasks 3, 4) | Service-level: who asks for the kill, who must not, and the two-bool pairing over a fake table |
| `internal/cookies/job_linux.go` | modify (Task 5) | Binds the seams to `/proc` and `syscall.Kill`; `Setpgid`; the five forwarding methods |
| `internal/cookies/job_linux_smoke_test.go` | **create** (Task 5) | `//go:build linux`, opt-in, not run in CI |
| `docs/spec/operations.md`, `SPEC.md` | modify (Task 5) | The behaviour claims that stop being true in the same commit |
| the two plan docs under `docs/superpowers/plans/` | modify (Task 6) | The ledger rows that call A1-on-Linux unfixed |

---

## Task 1: The tag-free process-group core

**Files:**
- Create: `internal/cookies/job_pgroup.go`
- Create: `internal/cookies/job_pgroup_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `var listProcessGroups func() (map[int]int, error)`; `var killProcessGroup func(int) error`; `var errNoProcessTable error`; `type pgroupJob struct{ pgid int }` with `adopt(pid int) error`, `queryable() bool`, `activeProcesses() (int, error)`, `killGroup() error`, `forget()`; `func parseProcStatPGID(stat string) (int, bool)`. Test helpers `fakeProcessTable(t *testing.T, table map[int]int)`, `unreadableProcessTable(t *testing.T) error`, `captureGroupKills(t *testing.T) *[]int`.

Nothing calls any of this yet, on any platform. The file is inert until Task 5 binds the two variables; that is deliberate, so a reviewer can gate the decision logic on its own.

- [ ] **Step 1: Write the failing test — the `/proc` stat parser**

Create `internal/cookies/job_pgroup_test.go`:

```go
package cookies

import (
	"errors"
	"testing"
)

// Test group ids. Odd, and far outside any plausible live range, following the
// convention autocookies_setup_reap_test.go states for its PIDs: nothing here
// may reach a real process on the machine running the tests. Every test that
// could signal one installs captureGroupKills first.
const (
	setupGroupPid = 424247
	setupChildPid = 424249
	strangerPid   = 424251
	strangerGroup = 424253
)

// fakeProcessTable binds the package's process-table hook to a fixed pid→pgid
// map for one test and restores the real one afterwards.
//
// This is the "fake process table" the owner's ruling names. It is the whole
// reason the decision logic lives in a file with no build tag: with it, every
// branch of the Linux reap runs in the ordinary Windows test suite, and no
// Linux box is needed to review this arc.
func fakeProcessTable(t *testing.T, table map[int]int) {
	t.Helper()
	prev := listProcessGroups
	listProcessGroups = func() (map[int]int, error) { return table, nil }
	t.Cleanup(func() { listProcessGroups = prev })
}

// unreadableProcessTable binds the hook to a failure — a hardened container
// where /proc cannot be walked — and returns the error it will answer with, so
// a test can assert the error travels rather than being swallowed into a zero.
func unreadableProcessTable(t *testing.T) error {
	t.Helper()
	prev := listProcessGroups
	err := errors.New("read /proc: permission denied")
	listProcessGroups = func() (map[int]int, error) { return nil, err }
	t.Cleanup(func() { listProcessGroups = prev })
	return err
}

// captureGroupKills swaps the group-kill hook for a recorder and restores it
// when the test ends. Same job captureKills does one layer up, and for the same
// reason: no signal may reach a real process group on the developer machine.
func captureGroupKills(t *testing.T) *[]int {
	t.Helper()
	prev := killProcessGroup
	killed := []int{}
	killProcessGroup = func(pgid int) error {
		killed = append(killed, pgid)
		return nil
	}
	t.Cleanup(func() { killProcessGroup = prev })
	return &killed
}

// TestParseProcStatPGIDReadsFieldFiveAfterTheLastParen pins the one piece of
// /proc parsing that is easy to get wrong and impossible to notice: field 2 of
// /proc/<pid>/stat is `comm`, the executable name in parentheses, and the
// kernel does not escape it. Firefox's content processes really are named
// "(Web Content)" and systemd's helper really is "((sd-pam))".
//
// Mutation that must break this: replace strings.LastIndexByte with
// IndexByte — the "Web Content" row then parses the word "Content)" and
// answers 0, so every Firefox content process is silently missing from the
// table and an abandoned setup reads as empty. Second mutation: read
// fields[1] instead of fields[2] — every row returns the PPID, which for a
// browser started by Moombox is Moombox's own pid. Third mutation: drop the
// state check and the zombie row parses as a member — a browser's orphaned
// child, reparented to a PID-1 Moombox that never reaps it, then holds the
// count above zero for the life of the process (decision 8).
func TestParseProcStatPGIDReadsFieldFiveAfterTheLastParen(t *testing.T) {
	cases := []struct {
		name string
		stat string
		want int
		ok   bool
	}{
		{
			"plain comm",
			"4242 (firefox) S 1 4242 4242 0 -1 4194560 1234 0 0 0 5 3 0 0 20 0 1 0 100 0 0",
			4242, true,
		},
		{
			"comm with a space",
			"4310 (Web Content) S 4242 4242 4242 0 -1 4194560 999 0 0 0 1 0 0 0 20 0 12 0 140 0 0",
			4242, true,
		},
		{
			"comm wrapped in its own parens",
			"4311 ((sd-pam)) S 1 4311 4311 0 -1 1077936384 100 0 0 0 0 0 0 0 20 0 1 0 90 0 0",
			4311, true,
		},
		{
			"comm with both a space and a paren",
			"4312 (Isolated Web Co (x)) S 4242 4242 4242 0 -1 4194560 50 0 0 0 0 0 0 0 20 0 9 0 150 0 0",
			4242, true,
		},
		{
			"zombie keeps its group id but is not a live member",
			"4314 (Web Content) Z 4242 4242 4242 0 -1 4194560 0 0 0 0 1 0 0 0 20 0 1 0 140 0 0",
			0, false,
		},
		{"kernel thread in group zero", "2 (kthreadd) S 0 0 0 0 -1 2129984 0 0 0 0 0 0 0 0 20 0 1 0 1 0 0", 0, false},
		{"truncated after the comm", "4242 (firefox) S 1", 0, false},
		{"no parenthesis at all", "4242 firefox S 1 4242", 0, false},
		{"non-numeric group", "4242 (firefox) S 1 nope 4242", 0, false},
		{"empty", "", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseProcStatPGID(tc.stat)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseProcStatPGID(%q) = (%d, %v), want (%d, %v)",
					tc.stat, got, ok, tc.want, tc.ok)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -count=1 -run TestParseProcStatPGID ./internal/cookies/`
Expected: FAIL to build — `undefined: parseProcStatPGID`, `undefined: listProcessGroups`, `undefined: killProcessGroup`.

- [ ] **Step 3: Create `internal/cookies/job_pgroup.go` with the seams and the parser**

```go
package cookies

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// errNoProcessTable is what the two hooks below answer with until a platform
// binds them — which is every platform but Linux.
//
// It is an ERROR and not an empty table on purpose. An empty table counts as
// "the group has no members left", which is the one sentence this package must
// never say when it does not know: the reap acts on it by releasing the
// acquisition slot. See pgroupJob.activeProcesses.
var errNoProcessTable = errors.New("no process table reader on this platform")

// listProcessGroups returns pid → process-group id for every process the
// caller can see, or an error when the table cannot be read at all.
//
// A package variable for two reasons. job_linux.go's init binds it to a /proc
// walk — the ONE production site that reassigns it — which is how the decision
// logic in this file stays free of build tags; and a test on any platform binds
// it to a fixed map, which is how every branch below is exercised on the
// Windows machine this project is developed on, with no Linux box and no
// launched browser. Same seam convention as setupBrowserGone, killProcessTree
// and writeCookieFile otherwise: tests swap it and restore it with t.Cleanup.
var listProcessGroups = func() (map[int]int, error) { return nil, errNoProcessTable }

// killProcessGroup delivers SIGKILL to every member of pgid. job_linux.go's
// init binds it to syscall.Kill(-pgid, SIGKILL); tests bind it to a recorder.
//
// IT IS NEVER CALLED WITH pgid <= 0. kill(-0, …) signals the CALLER'S OWN
// process group — Moombox and everything it has launched, and in a container
// where Moombox leads its group, the container. Its one caller is
// pgroupJob.killGroup, which refuses an unadopted group; adopt refuses
// pid <= 0 before a group can exist (killProcessTreeUnix reaches this through
// both); and the Linux binding checks a third time at the syscall. Tests on
// Windows pin the first two; the third is in a file no test here executes.
var killProcessGroup = func(int) error { return errNoProcessTable }

// parseProcStatPGID pulls the process-group id — field 5, `pgrp` — out of one
// /proc/<pid>/stat line.
//
// PARSE FROM THE LAST ')', NEVER BY SPLITTING THE WHOLE LINE. Field 2 is
// `comm`, the executable name in parentheses, and the kernel does not escape
// it: Firefox's content processes are literally "(Web Content)" and systemd's
// user helper is "((sd-pam))". Splitting on spaces gets the wrong field for the
// first, and only the last-')' rule survives the second. After the closing
// parenthesis the fields are state (3), ppid (4), pgrp (5) — index 2 of what is
// left.
//
// A pgrp of 0 (kernel threads) is reported as unparsed rather than as a group.
// It is not a group any browser of ours can be in, and admitting it would put a
// zero key into a table that killGroup's guards would then have to re-check.
//
// A ZOMBIE IS NOT A MEMBER. A task that has exited but not been reaped keeps
// its stat line, state Z, with pgrp intact. It holds no window and no profile
// lock, and in a container where Moombox is PID 1 with no init (a bare
// `docker run`; compose sets `init: true`) an orphaned grandchild of a killed
// browser is reparented to Moombox, which reaps only the children it started —
// counted, that zombie would hold the group above zero for the life of the
// process and the reap would never fire after the one crash it exists for.
// A Windows Job Object's ActiveProcesses does not count the terminated either.
func parseProcStatPGID(stat string) (int, bool) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 3 {
		return 0, false
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return 0, false // exited, not yet reaped (or dead): not a live member
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil || pgid <= 0 {
		return 0, false
	}
	return pgid, true
}

// unusedByDesign keeps the fmt import honest until adopt lands in the next
// step; delete this line with that step.
var _ = fmt.Sprintf
```

- [ ] **Step 4: Run the parser test and watch it pass**

Run: `go test -count=1 -run TestParseProcStatPGID ./internal/cookies/`
Expected: PASS, all ten subtests.

- [ ] **Step 5: Write the failing tests for `pgroupJob`**

Append to `internal/cookies/job_pgroup_test.go`:

```go
// TestPGroupJobCannotAnswerWithoutAGroup is the honest-zero rule, the same one
// job_windows.go's queryable() carries: a count of zero from a job that never
// adopted anything is "there is nothing to ask", not "the browser is gone", and
// the reap acts on the second.
//
// Mutation: make queryable() return true unconditionally and the reap releases
// the acquisition slot for every launch where the group could not be adopted.
func TestPGroupJobCannotAnswerWithoutAGroup(t *testing.T) {
	fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})

	for _, tc := range []struct {
		name string
		job  *pgroupJob
	}{
		{"nil job", nil},
		{"never adopted", &pgroupJob{}},
		{"nonsense negative group", &pgroupJob{pgid: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.job.queryable() {
				t.Fatal("queryable() said it could answer with no group adopted")
			}
			n, err := tc.job.activeProcesses()
			if err != nil || n != 0 {
				t.Fatalf("activeProcesses() = (%d, %v), want (0, nil)", n, err)
			}
		})
	}
}

// TestPGroupJobCountsOnlyItsOwnGroup is the count the reap reads. The table
// deliberately contains a stranger process AND a stranger group leader, because
// the failure this guards is a count that answers "still alive" for someone
// else's browser.
//
// Mutation: compare the map's KEY instead of its value and the count collapses
// to one (only the leader itself), so a live browser with three content
// processes reads as gone the moment its launcher exits.
func TestPGroupJobCountsOnlyItsOwnGroup(t *testing.T) {
	fakeProcessTable(t, map[int]int{
		setupGroupPid: setupGroupPid, // the launcher/leader
		setupChildPid: setupGroupPid, // a content process it forked
		strangerPid:   strangerGroup, // someone else's browser
		strangerGroup: strangerGroup,
	})
	job := &pgroupJob{pgid: setupGroupPid}

	n, err := job.activeProcesses()
	if err != nil {
		t.Fatalf("activeProcesses: %v", err)
	}
	if n != 2 {
		t.Fatalf("activeProcesses() = %d, want 2 (the leader and its child, not the stranger)", n)
	}
}

// TestPGroupJobReportsAFailedTableReadAsAnError is invariant 1 at its source: a
// container that cannot walk /proc must not have its acquisition slot released.
// The error has to TRAVEL — a (0, nil) here reads as an empty group upstream.
//
// Mutation: swallow the error and return (0, nil) and a hardened container
// reaps every setup 60 seconds after the launcher exits, with the browser still
// on screen.
func TestPGroupJobReportsAFailedTableReadAsAnError(t *testing.T) {
	want := unreadableProcessTable(t)
	job := &pgroupJob{pgid: setupGroupPid}

	n, err := job.activeProcesses()
	if !errors.Is(err, want) {
		t.Fatalf("activeProcesses() error = %v, want the table's own error", err)
	}
	if n != 0 {
		t.Fatalf("activeProcesses() = %d alongside an error, want 0", n)
	}
}

// TestPGroupJobAdoptsOnlyAProcessThatLeadsItsOwnGroup is the safety argument
// for the whole arc, and the assertion whose absence would be catastrophic
// rather than merely wrong.
//
// configureCmdSysProcAttr sets Setpgid, so a browser Moombox launched leads a
// group whose id is its own pid. A process started WITHOUT that flag inherits
// MOOMBOX'S group — and recording that pid would later point killGroup at
// Moombox's own process group: SIGKILL to Moombox and everything it spawned,
// and in Docker to the container.
//
// Mutation: drop the `pgid != pid` comparison and the "inherited Moombox's
// group" case adopts group 1 — which is exactly the group a containerised
// Moombox leads.
func TestPGroupJobAdoptsOnlyAProcessThatLeadsItsOwnGroup(t *testing.T) {
	t.Run("leads its own group", func(t *testing.T) {
		fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})
		job := &pgroupJob{}
		if err := job.adopt(setupGroupPid); err != nil {
			t.Fatalf("adopt: %v", err)
		}
		if !job.queryable() || job.pgid != setupGroupPid {
			t.Fatalf("adopt left pgid %d, queryable %v", job.pgid, job.queryable())
		}
	})

	t.Run("inherited someone else's group", func(t *testing.T) {
		fakeProcessTable(t, map[int]int{setupGroupPid: 1})
		job := &pgroupJob{}
		if err := job.adopt(setupGroupPid); err == nil {
			t.Fatal("adopt accepted a process that did not lead its own group — " +
				"killGroup would later SIGKILL group 1")
		}
		if job.queryable() {
			t.Fatalf("a refused adopt still left pgid %d", job.pgid)
		}
	})

	t.Run("not in the table at all", func(t *testing.T) {
		fakeProcessTable(t, map[int]int{})
		job := &pgroupJob{}
		if err := job.adopt(setupGroupPid); err == nil {
			t.Fatal("adopt accepted a pid the process table does not contain")
		}
	})

	t.Run("unreadable table", func(t *testing.T) {
		unreadableProcessTable(t)
		job := &pgroupJob{}
		if err := job.adopt(setupGroupPid); err == nil {
			t.Fatal("adopt accepted a group it could not confirm")
		}
	})

	t.Run("nonsense pid", func(t *testing.T) {
		fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})
		job := &pgroupJob{}
		if err := job.adopt(0); err == nil {
			t.Fatal("adopt accepted pid 0 — kill(-0) is Moombox's own group")
		}
	})
}

// TestPGroupJobNeverKillsWhatItCannotSee covers all three refusals in killGroup.
// Each is an invariant, not a nicety.
//
// Mutations, one per subtest: drop the queryable() guard and pgid 0 reaches the
// hook, i.e. kill(-0) — Moombox's own group; drop the error check and an
// unreadable /proc fires a blind signal at a number; drop the `active == 0`
// check and every reap fires a signal at a group id the kernel may since have
// recycled onto a stranger.
func TestPGroupJobNeverKillsWhatItCannotSee(t *testing.T) {
	t.Run("no group adopted", func(t *testing.T) {
		killed := captureGroupKills(t)
		fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})
		if err := (&pgroupJob{}).killGroup(); err != nil {
			t.Fatalf("killGroup: %v", err)
		}
		if len(*killed) != 0 {
			t.Fatalf("killGroup signalled %v with no group adopted", *killed)
		}
	})

	t.Run("table unreadable", func(t *testing.T) {
		killed := captureGroupKills(t)
		want := unreadableProcessTable(t)
		if err := (&pgroupJob{pgid: setupGroupPid}).killGroup(); !errors.Is(err, want) {
			t.Fatalf("killGroup error = %v, want the table's own error", err)
		}
		if len(*killed) != 0 {
			t.Fatalf("killGroup signalled %v with an unreadable process table", *killed)
		}
	})

	t.Run("group already empty", func(t *testing.T) {
		killed := captureGroupKills(t)
		fakeProcessTable(t, map[int]int{strangerPid: strangerGroup})
		if err := (&pgroupJob{pgid: setupGroupPid}).killGroup(); err != nil {
			t.Fatalf("killGroup: %v", err)
		}
		if len(*killed) != 0 {
			t.Fatalf("killGroup signalled %v at an empty group", *killed)
		}
	})
}

// TestPGroupJobKillsALiveGroupOnce is the positive case the three refusals
// above exist to protect: when the table says the group still has members, one
// signal goes to that group and to no other.
//
// Mutation: signal j.pgid+1, or signal every key in the table, and the test
// names the group it did not mean to kill.
func TestPGroupJobKillsALiveGroupOnce(t *testing.T) {
	killed := captureGroupKills(t)
	fakeProcessTable(t, map[int]int{
		setupGroupPid: setupGroupPid,
		setupChildPid: setupGroupPid,
		strangerPid:   strangerGroup,
	})

	if err := (&pgroupJob{pgid: setupGroupPid}).killGroup(); err != nil {
		t.Fatalf("killGroup: %v", err)
	}
	if len(*killed) != 1 || (*killed)[0] != setupGroupPid {
		t.Fatalf("killGroup signalled %v, want exactly [%d]", *killed, setupGroupPid)
	}
}

// TestPGroupJobForgetsWithoutKilling is R4 at the type level: close() on Linux
// drops a number, it does not signal. The sites that need a kill ask for one.
//
// Mutation: make forget() call killGroup and trackedSetupJob's failed-assign
// close becomes a SIGKILL of the browser window the user is signed into.
func TestPGroupJobForgetsWithoutKilling(t *testing.T) {
	killed := captureGroupKills(t)
	fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})
	job := &pgroupJob{pgid: setupGroupPid}

	job.forget()

	if job.queryable() {
		t.Fatal("forget() left a group behind")
	}
	if len(*killed) != 0 {
		t.Fatalf("forget() signalled %v; closing a Linux job must kill nothing", *killed)
	}
}
```

- [ ] **Step 6: Run them and watch them fail**

Run: `go test -count=1 -run 'TestPGroupJob' ./internal/cookies/`
Expected: FAIL to build — `undefined: pgroupJob`.

- [ ] **Step 7: Add `pgroupJob` to `job_pgroup.go`**

Insert after the `killProcessGroup` var and delete the `unusedByDesign` placeholder line:

```go
// pgroupJob is the tag-free half of the Linux processJob: the Job Object's
// counting and killing decisions, expressed against a process-group id and the
// two hooks above.
//
// It lives in a file with NO build tag on purpose. A Windows Job Object is a
// kernel handle and can only be exercised on Windows; a process group is an
// integer and a lookup, so every branch of this type runs in the ordinary
// `go test ./...` once the hooks are bound to a fake table. That leaves
// job_linux.go thin enough to read in one screen — it binds three hooks and
// forwards five methods — which is what makes an arc with no Linux gate
// reviewable at all.
type pgroupJob struct {
	pgid int
}

// adopt records pid as the group this job tracks, but ONLY when the process
// table says pid actually LEADS its own group.
//
// That check is the safety argument for the whole mechanism, not defensive
// padding. configureCmdSysProcAttr sets Setpgid, so a browser Moombox launched
// leads a group whose id is its own pid; a process started WITHOUT that flag
// inherits MOOMBOX'S group, and recording that pid would later point killGroup
// at Moombox's own process group — SIGKILL to Moombox and everything it
// spawned, and in Docker to the container.
//
// Refusing degrades to exactly the pre-A1 behaviour: no group, queryable false,
// "nothing can say", no reap and no kill. That is the safe direction, and it is
// why this returns an ERROR rather than silently recording nothing —
// trackedSetupJob's answer to an assign it cannot trust is already to drop the
// job and log why.
func (j *pgroupJob) adopt(pid int) error {
	if j == nil {
		return errors.New("no job to adopt a process group into")
	}
	if pid <= 0 {
		return fmt.Errorf("refusing to track pid %d as a process group", pid)
	}
	table, err := listProcessGroups()
	if err != nil {
		return fmt.Errorf("read process table: %w", err)
	}
	pgid, ok := table[pid]
	if !ok {
		return fmt.Errorf("pid %d is not in the process table", pid)
	}
	if pgid != pid {
		return fmt.Errorf("pid %d did not lead its own process group (pgid %d); "+
			"refusing to track a group Moombox does not own", pid, pgid)
	}
	j.pgid = pid
	return nil
}

// queryable reports whether this job can give a MEANINGFUL answer — the same
// contract job_windows.go's queryable carries, for the same reason: a zero from
// activeProcesses must never be confused with "there is nothing to ask". See
// setupBrowserGone, whose whole contract is that distinction.
func (j *pgroupJob) queryable() bool { return j != nil && j.pgid > 0 }

// activeProcesses counts the processes still in the group.
//
// A failed table read is an ERROR, never a zero. realSetupBrowserGone turns
// that error into known=false, which the reap reads as "still running" — so a
// container with an unreadable /proc keeps the pre-A1 behaviour instead of
// releasing a slot whose browser is on screen.
func (j *pgroupJob) activeProcesses() (int, error) {
	if !j.queryable() {
		return 0, nil
	}
	table, err := listProcessGroups()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, pgid := range table {
		if pgid == j.pgid {
			n++
		}
	}
	return n, nil
}

// killGroup SIGKILLs everything left in the group.
//
// Three refusals, in the order they are cheap, each an invariant:
//
//   - no group adopted → nothing to kill, and a pgid of 0 would name Moombox's
//     OWN group;
//   - the table could not be read → DO NOT KILL. "I cannot see" is not licence
//     to fire a signal at a number;
//   - the group is already empty → nothing to kill, and firing anyway would be
//     a blind signal at an integer the kernel may since have recycled onto an
//     unrelated group leader.
//
// That last one is the closest thing available to the protection a Windows Job
// Object HANDLE gives for free, and it is weaker: a handle cannot name a
// process that is not in the job, a pgid can. The residual is recorded in
// docs/spec/operations.md rather than hidden here.
func (j *pgroupJob) killGroup() error {
	if !j.queryable() {
		return nil
	}
	active, err := j.activeProcesses()
	if err != nil {
		return err
	}
	if active == 0 {
		return nil
	}
	return killProcessGroup(j.pgid)
}

// forget drops the group id without signalling anything. It is what close()
// means on Linux: there is no KILL_ON_JOB_CLOSE to imitate, and the sites that
// relied on the Windows close to kill ask for the kill out loud instead. See
// killTrackedProcesses.
func (j *pgroupJob) forget() {
	if j != nil {
		j.pgid = 0
	}
}
```

- [ ] **Step 8: Run the full package and watch it pass**

Run: `go test -count=1 ./internal/cookies/`
Expected: PASS. `TestDrainJobReturnsImmediatelyWithoutAJob` and the sixteen existing reap tests are unaffected — nothing calls the new code yet.

- [ ] **Step 9: Run every gate**

Run the six commands from Global Constraints. Expected: all clean, `gofmt -l` silent.

- [ ] **Step 10: Commit**

```bash
git add internal/cookies/job_pgroup.go internal/cookies/job_pgroup_test.go
git commit -m "$(cat <<'EOF'
feat(cookies): the process-group reap's decision layer, with no build tag

A1's liveness question on Linux is a process group where Windows has a Job
Object. The counting and killing DECISIONS go in a tag-free file so every
branch runs in the Windows test suite against a fake process table: the owner's
ruling is "unit-tested with a fake process table", with no Linux box and a
field report as the gate.

Nothing calls this yet on any platform. The parser reads /proc/<pid>/stat field
5 from the LAST ')' because comm is unescaped and can contain both spaces and
parentheses. adopt() refuses a process that does not lead its own group, which
is what keeps killGroup from ever naming Moombox's own group.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 2: `killProcessTree`'s non-Windows arm kills the group

**Files:**
- Modify: `internal/cookies/autocookies.go:3071-3080` (the `killProcessTree` closure)
- Modify: `internal/cookies/job_pgroup_test.go` (append)

**Interfaces:**
- Consumes: `pgroupJob` (`adopt`, `killGroup`) and, through it, `listProcessGroups`/`killProcessGroup` (Task 1).
- Produces: `func killProcessTreeUnix(proc *os.Process)`; `var killOneProcess func(proc *os.Process) error`; test helper `captureOneProcessKills(t *testing.T) *[]int`.

Behaviour is unchanged on every platform until Task 5: `listProcessGroups` is still the unbound stub, so `adopt` refuses every pid and the fallback is today's `proc.Kill()` exactly.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cookies/job_pgroup_test.go` (add `"os"` to its imports):

```go
// captureOneProcessKills swaps the single-process fallback for a recorder. The
// package's rule is that a fabricated PID must never reach a real signal on the
// machine running the tests; killProcessTree has been a variable for that
// reason since it was written, and its fallback needs the same treatment.
func captureOneProcessKills(t *testing.T) *[]int {
	t.Helper()
	prev := killOneProcess
	killed := []int{}
	killOneProcess = func(p *os.Process) error {
		if p != nil {
			killed = append(killed, p.Pid)
		}
		return nil
	}
	t.Cleanup(func() { killOneProcess = prev })
	return &killed
}

// TestKillProcessTreeUnixKillsTheGroupNotJustTheLauncher is the reason the
// group exists at all. proc.Kill() kills the process Moombox spawned — which on
// the Firefox family is a launcher that handed off and exited ~170 ms after
// start (Arc 0's measurement). On Linux the tree IS the group, so one
// kill(-pgid) reaches the browser the launcher left behind.
//
// Mutation: put killOneProcess(proc) back as the primary and the recorder shows
// a single PID, which is the bug: the browser survives every cancel, every
// Stop, and every refresh timeout.
func TestKillProcessTreeUnixKillsTheGroupNotJustTheLauncher(t *testing.T) {
	groups := captureGroupKills(t)
	single := captureOneProcessKills(t)
	fakeProcessTable(t, map[int]int{
		setupGroupPid: setupGroupPid, // the leader, still alive
		setupChildPid: setupGroupPid, // what it handed off to
	})

	killProcessTreeUnix(&os.Process{Pid: setupGroupPid})

	if len(*groups) != 1 || (*groups)[0] != setupGroupPid {
		t.Fatalf("group kills = %v, want exactly [%d]", *groups, setupGroupPid)
	}
	if len(*single) != 0 {
		t.Fatalf("it also killed %v directly; the group kill already covers the leader", *single)
	}
}

// TestKillProcessTreeUnixFallsBackWhereThereAreNoProcessGroups keeps darwin and
// every other non-Linux, non-Windows target on exactly today's behaviour: the
// table hook is unbound there, it answers errNoProcessTable, adopt refuses,
// and the direct kill is all there ever was. A Linux /proc that cannot be read
// takes the same arm.
//
// Mutation: drop the fallback and a darwin build stops killing browsers
// entirely.
func TestKillProcessTreeUnixFallsBackWhereThereAreNoProcessGroups(t *testing.T) {
	groups := captureGroupKills(t)
	single := captureOneProcessKills(t)
	unreadableProcessTable(t)

	killProcessTreeUnix(&os.Process{Pid: setupGroupPid})

	if len(*groups) != 0 {
		t.Fatalf("group kills = %v; nothing may be signalled through a table that cannot be read", *groups)
	}
	if len(*single) != 1 || (*single)[0] != setupGroupPid {
		t.Fatalf("fallback kills = %v, want exactly [%d]", *single, setupGroupPid)
	}
}

// TestKillProcessTreeUnixWillNotSignalAGroupThePidNoLongerLeads is why the arm
// goes through adopt rather than handing the pid to the hook. killSetupProcess
// reaches here with a REAPED pid whenever no job could vouch for the browser
// (a failed assign), and proc.Kill() was safe there — Go refuses it on a
// process that has been waited on. A bare kill(-pid) has no such memory: it
// fires at whatever group the kernel has since given that number to. So the
// arm signals only a pid that is in the table right now and leads its own
// group, and otherwise falls back to the direct kill, which is harmless on a
// reaped process.
//
// Mutations, one per subtest: replace adopt with a bare killProcessGroup(pid)
// and the first case signals a number the table does not contain; drop the
// `pgid != pid` refusal in adopt and the second case signals a stranger's
// group — or, for a child that inherited Moombox's group, Moombox's own.
func TestKillProcessTreeUnixWillNotSignalAGroupThePidNoLongerLeads(t *testing.T) {
	t.Run("pid is not in the table", func(t *testing.T) {
		groups := captureGroupKills(t)
		single := captureOneProcessKills(t)
		fakeProcessTable(t, map[int]int{strangerPid: strangerGroup})

		killProcessTreeUnix(&os.Process{Pid: setupGroupPid})

		if len(*groups) != 0 {
			t.Fatalf("group kills = %v for a pid the table does not contain", *groups)
		}
		if len(*single) != 1 || (*single)[0] != setupGroupPid {
			t.Fatalf("fallback kills = %v, want exactly [%d]", *single, setupGroupPid)
		}
	})

	t.Run("pid sits in someone else's group", func(t *testing.T) {
		groups := captureGroupKills(t)
		single := captureOneProcessKills(t)
		fakeProcessTable(t, map[int]int{setupGroupPid: strangerGroup, strangerGroup: strangerGroup})

		killProcessTreeUnix(&os.Process{Pid: setupGroupPid})

		if len(*groups) != 0 {
			t.Fatalf("group kills = %v; the pid does not lead that group and it is not ours to signal", *groups)
		}
		if len(*single) != 1 || (*single)[0] != setupGroupPid {
			t.Fatalf("fallback kills = %v, want exactly [%d]", *single, setupGroupPid)
		}
	})
}

// TestKillProcessTreeUnixRefusesANonPositivePid is the pid <= 0 guard as seen
// from this arm. A zero-valued os.Process is not hypothetical: the refresh
// slot is claimed with `&exec.Cmd{}` whose Process is nil until the launcher
// publishes one, and a future caller reaching this with a zero pid must fall
// back rather than signal. The refusal is adopt's; this pins that the arm
// cannot route around it. The table deliberately contains a 0 → 0 row so
// that a dropped guard would "succeed" and the recorder would show it.
//
// Mutation: drop adopt's `pid <= 0` check — the group is adopted as 0,
// killGroup refuses it silently, and the fallback never runs, so the second
// assertion fails; kill(-0, SIGKILL) is Moombox's own process group.
func TestKillProcessTreeUnixRefusesANonPositivePid(t *testing.T) {
	groups := captureGroupKills(t)
	single := captureOneProcessKills(t)
	fakeProcessTable(t, map[int]int{0: 0})

	killProcessTreeUnix(&os.Process{Pid: 0})
	killProcessTreeUnix(nil)

	if len(*groups) != 0 {
		t.Fatalf("group kills = %v; a non-positive pid must never reach the hook", *groups)
	}
	if len(*single) != 1 || (*single)[0] != 0 {
		t.Fatalf("fallback kills = %v, want exactly [0] (and nothing for the nil process)", *single)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 -run TestKillProcessTreeUnix ./internal/cookies/`
Expected: FAIL to build — `undefined: killProcessTreeUnix`, `undefined: killOneProcess`.

- [ ] **Step 3: Rewrite the closure's else branch in `autocookies.go`**

Replace lines 3071-3080 (`var killProcessTree = func(proc *os.Process) { … }`) with:

```go
var killProcessTree = func(proc *os.Process) {
	if proc == nil {
		return
	}
	if isWindows() {
		exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		killProcessTreeUnix(proc)
	}
}

// killProcessTreeUnix is the non-Windows arm, split out of the closure above so
// a test on ANY platform can execute it: isWindows() reads runtime.GOOS through
// a plain function rather than a seam, so on the Windows machine this project
// is developed on the else branch is otherwise unreachable. The one-line wiring
// above is reviewed by eye — the same coverage posture startChromiumSetup
// states in prose for its own trackedSetupJob call.
//
// ON LINUX THE TREE IS THE GROUP. configureCmdSysProcAttr sets Setpgid on every
// browser this package launches, so the child leads a group whose id is its own
// pid, and one kill(-pgid) reaches the browser the launcher handed off to.
// proc.Kill() alone never did: it kills the launcher, which on the Firefox
// family exited ~170 ms after start.
//
// IT GOES THROUGH pgroupJob, NOT STRAIGHT TO THE HOOK, so it inherits adopt's
// and killGroup's refusals: the pid must be in the table right now and lead its
// own group, and the group must still have members. killSetupProcess reaches
// here with a REAPED pid whenever no job could vouch for the browser, and
// proc.Kill() was safe there — Go refuses it on a process that has already
// been waited on (os.ErrProcessDone). A bare kill(-pid) has no such memory; it
// fires at whatever group the kernel has since given that number to.
//
// Everywhere the refusals apply — darwin and the fallback build, where the
// table hook is unbound and answers errNoProcessTable; a Linux /proc that
// cannot be read; a pid that is gone or sits in someone else's group — the
// direct kill below is exactly today's behaviour. The pid <= 0 guard is
// adopt's; see killProcessGroup's doc for the three checks on kill(-0, …).
func killProcessTreeUnix(proc *os.Process) {
	if proc == nil {
		return
	}
	group := &pgroupJob{}
	if err := group.adopt(proc.Pid); err == nil {
		if err := group.killGroup(); err == nil {
			return
		}
	}
	killOneProcess(proc)
}

// killOneProcess is (*os.Process).Kill behind a package variable, for the same
// reason killProcessTree itself is one: the fallback above has to be
// exercisable without a real process, and a fabricated PID must never reach a
// real signal on the machine running the tests. Nothing in production
// reassigns it.
var killOneProcess = func(proc *os.Process) error { return proc.Kill() }
```

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test -count=1 ./internal/cookies/`
Expected: PASS, including the four new tests (six cases) and the existing `captureKills`-based reap tests (which swap `killProcessTree` itself and so never reach the new arm).

- [ ] **Step 5: Run every gate, then commit**

```bash
git add internal/cookies/autocookies.go internal/cookies/job_pgroup_test.go
git commit -m "$(cat <<'EOF'
feat(cookies): the non-Windows kill reaches the process group, not the launcher

killProcessTree is the seam five call sites go through — killSetupProcess,
killRefreshProcess, refreshChromium's teardown, closeFirefoxGracefully and
runWithTimeout's timeout arm — and its non-Windows arm killed the process
Moombox spawned. On the Firefox family that process exited 170 ms after start;
the browser it handed off to was never touched.

On Linux the tree is the group. The arm goes through pgroupJob's adopt and
killGroup rather than straight to the hook, so it signals only a group the pid
still leads — proc.Kill() refuses a reaped process; a bare kill(-pid) would
not, and killSetupProcess reaches this arm with a reaped pid whenever no job
could vouch for the browser. Inert until job_linux.go binds the table: adopt
refuses every pid and the direct kill is today's behaviour exactly.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 3: The close-to-kill sites ask out loud

**Files:**
- Modify: `internal/cookies/job_pgroup.go` (append the `killTrackedProcesses` seam)
- Modify: `internal/cookies/autocookies.go:3229-3237` (`cleanupLocked`), `:3305-3313` (`adoptSetupJobLocked`) — line numbers as of `390adb6`; Task 2 inserts ~45 lines above both, so find them by symbol
- Modify: `internal/cookies/autocookies_firefox.go:915-920` (`runWithTimeout`'s teardown defer) — new `closeLaunchJob`; and the three `drainJob` doc lines that name `job.close()` as the kill (`:753`, `:775-776`, `:786`)
- Create: `internal/cookies/autocookies_setup_reap_pgroup_test.go`

**Interfaces:**
- Consumes: `pgroupJob` (Task 1).
- Produces: `var killTrackedProcesses func(*processJob) error`; `func closeLaunchJob(job *processJob, logger interface{ Debug(msg string, args ...any); Info(msg string, args ...any); Warn(msg string, args ...any) })`. Test helper `captureTrackedKills(t *testing.T) *[]*processJob`.

Behaviour is unchanged on every platform until Task 5: the seam's default returns nil and does nothing.

- [ ] **Step 1: Write the failing tests**

Create `internal/cookies/autocookies_setup_reap_pgroup_test.go`:

```go
package cookies

import (
	"errors"
	"os"
	"testing"
)

// captureTrackedKills swaps the "finish off what the job still tracks" hook for
// a recorder. On Windows the real hook is a no-op — close() there is
// KILL_ON_JOB_CLOSE — so these tests are about WHO ASKS, which is the half that
// has to be right before job_linux.go binds anything to it.
func captureTrackedKills(t *testing.T) *[]*processJob {
	t.Helper()
	prev := killTrackedProcesses
	asked := []*processJob{}
	killTrackedProcesses = func(j *processJob) error {
		asked = append(asked, j)
		return nil
	}
	t.Cleanup(func() { killTrackedProcesses = prev })
	return &asked
}

// TestCleanupAsksForTheKillBeforeItDropsTheJob is R4 at the site that matters
// most. On Windows cleanupLocked's close IS the kill, which is why every caller
// of killSetupProcess calls cleanup immediately afterwards and why
// killSetupProcess is allowed to decline a reaped PID. On Linux the close
// forgets an integer, so unless this call is here — and BEFORE the field is
// nilled — a cancelled setup leaves the browser running forever.
//
// Mutation: move the call after `s.setupJob = nil` and it receives nil; delete
// it and the recorder is empty, which is the wedge on Linux.
func TestCleanupAsksForTheKillBeforeItDropsTheJob(t *testing.T) {
	asked := captureTrackedKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	job := &processJob{}
	s.mu.Lock()
	s.setupJob = job
	s.mu.Unlock()

	s.cleanup()

	if len(*asked) != 1 || (*asked)[0] != job {
		t.Fatalf("cleanup asked to kill %v, want exactly the job it was about to drop", *asked)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setupJob != nil {
		t.Fatal("cleanup left the job in the slot")
	}
}

// TestAdoptClosingAStaleJobAsksForTheKill covers the other close that is
// deliberately a kill on Windows: a handle an earlier attempt left behind, held
// by nothing else, whose only possible occupant is a setup browser the gate has
// already declared over.
//
// Mutation: delete the call and a Linux install that somehow reached adopt with
// a live stale group leaks that browser for the life of the process.
func TestAdoptClosingAStaleJobAsksForTheKill(t *testing.T) {
	asked := captureTrackedKills(t)
	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	stale := &processJob{}
	fresh := &processJob{}
	s.mu.Lock()
	s.setupJob = stale
	s.adoptSetupJobLocked(fresh)
	got := s.setupJob
	s.mu.Unlock()

	if len(*asked) != 1 || (*asked)[0] != stale {
		t.Fatalf("adopt asked to kill %v, want exactly the stale job", *asked)
	}
	if got != fresh {
		t.Fatal("adopt did not install the new job")
	}
}

// TestTrackedSetupJobDoesNotAskForAKillWhenTheAssignFailed is the site that
// must NOT kill, and it is the reason a Linux close() forgets instead of
// killing.
//
// trackedSetupJob drops a job whose assign failed, because a live handle
// tracking nothing reads as an EMPTY job and would license a premature release.
// On Windows that drop is free — the job holds nothing. If close() killed on
// Linux, or if this site asked for a kill, the group it would SIGKILL is the
// browser window the user is signed into, lost because a bookkeeping call went
// wrong.
//
// Mutation: add killTrackedProcesses to trackedSetupJob's drop path and this
// test fails, which is exactly what it is for.
func TestTrackedSetupJobDoesNotAskForAKillWhenTheAssignFailed(t *testing.T) {
	asked := captureTrackedKills(t)
	prevAssign := assignProcessToJob
	assignProcessToJob = func(*processJob, *os.Process) error {
		return errors.New("assign refused")
	}
	t.Cleanup(func() { assignProcessToJob = prevAssign })

	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	got := s.trackedSetupJob(&processJob{}, &os.Process{Pid: abandonedSetupPid}, "firefox")

	if got != nil {
		t.Fatal("a job whose assign failed was kept; an empty job reads as a closed browser")
	}
	if len(*asked) != 0 {
		t.Fatalf("the failed-assign drop asked to kill %v — that group is the user's live browser", *asked)
	}
}

// TestCloseLaunchJobAsksForTheKill covers runWithTimeout's teardown without
// launching anything. Two of its exits reach the deferred close with a browser
// still alive: the drain timing out with processes left, and the caller's
// context being cancelled. On Windows the close kills them; on Linux this call
// is the only thing that does.
//
// Mutation: swap the order so close() runs first and the kill is asked for a
// job that has already forgotten its group — a silent no-op.
func TestCloseLaunchJobAsksForTheKill(t *testing.T) {
	asked := captureTrackedKills(t)

	closeLaunchJob(nil, nopLogger{})
	if len(*asked) != 0 {
		t.Fatalf("a nil job asked to kill %v", *asked)
	}

	job := &processJob{}
	closeLaunchJob(job, nopLogger{})
	if len(*asked) != 1 || (*asked)[0] != job {
		t.Fatalf("closeLaunchJob asked to kill %v, want exactly the job", *asked)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 -run 'TestCleanupAsksForTheKill|TestAdoptClosing|TestTrackedSetupJobDoesNot|TestCloseLaunchJob' ./internal/cookies/`
Expected: FAIL to build — `undefined: killTrackedProcesses`, `undefined: closeLaunchJob`.

- [ ] **Step 3: Add the seam to `job_pgroup.go`**

Append:

```go
// killTrackedProcesses finishes off whatever a job is still tracking, for the
// platforms where CLOSING the job does not do it.
//
// The default is a no-op, and that default is CORRECT on Windows: close() there
// is KILL_ON_JOB_CLOSE, so by the time anyone could ask, the kill has already
// happened. job_linux.go binds it, because a process group is a NUMBER, not a
// kernel reference — closing a Linux job forgets an integer and kills nothing,
// so every site that on Windows relied on the close has to ask out loud.
//
// THE SITE THAT DOES NOT ASK MATTERS AS MUCH AS THE THREE THAT DO.
// trackedSetupJob closes a job whose assign FAILED and must not kill: on
// Windows that job holds nothing, but on Linux the group is the browser the
// launcher just put on screen, and killing it there would close the window the
// user is signing into because a bookkeeping call went wrong. That asymmetry is
// the whole reason close() forgets rather than kills — do not "simplify" it
// away.
//
// It returns an error so the three callers can say, in their own logger, that a
// browser may still be running. job_linux.go's init is the one production site
// that binds it; tests swap it and restore it with t.Cleanup.
var killTrackedProcesses = func(*processJob) error { return nil }
```

- [ ] **Step 4: Wire the three sites**

In `autocookies.go`, `cleanupLocked` (the body at `:3229`):

```go
func (s *AutoCookieService) cleanupLocked() {
	if s.setupJob != nil {
		// BEFORE the close, and before the field is nilled. On Windows the
		// close is itself the kill and this is a no-op; on Linux the close
		// forgets a number, so this is the only thing that reaches the browser.
		// See killTrackedProcesses.
		if err := killTrackedProcesses(s.setupJob); err != nil && s.logger != nil {
			s.logger.Warn("could not kill the setup browser's process group; it may still be running",
				"err", err)
		}
		s.setupJob.close()
		s.setupJob = nil
	}
	s.setupProcess = nil
	s.setupBrowser = nil
	s.browserExited = false
	s.setupRetainedSince = time.Time{}
	s.cdpPort = 0
	s.targetPlatform = ""
}
```

In `adoptSetupJobLocked` (`:3305`):

```go
	if s.setupJob != nil {
		if s.logger != nil {
			s.logger.Warn("closing a setup Job Object left behind by an earlier attempt")
		}
		if err := killTrackedProcesses(s.setupJob); err != nil && s.logger != nil {
			s.logger.Warn("could not kill the stale setup browser's process group", "err", err)
		}
		s.setupJob.close()
	}
	s.setupJob = job
```

In `autocookies_firefox.go`, replace the teardown defer at `:915-920` with a call, and add the function immediately above `runWithTimeout`:

```go
	defer closeLaunchJob(job, logger)
```

```go
// closeLaunchJob is runWithTimeout's teardown: finish off whatever the job
// still tracks, then release it.
//
// Named rather than inlined so it can be tested without launching a process.
// Two of runWithTimeout's exits arrive here with a browser still alive — the
// drain timing out with processes left in the job, and the caller's context
// being cancelled — and on Windows the close is what kills them. On Linux the
// close forgets a process-group id, so the kill has to be asked for first; the
// order matters, because a job that has already forgotten its group has nothing
// left to name.
func closeLaunchJob(job *processJob, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) {
	if job == nil {
		return
	}
	logger.Debug("closing job object (killing all tracked processes)")
	if err := killTrackedProcesses(job); err != nil {
		logger.Warn("could not kill the refresh browser's process group; it may still be running",
			"err", err)
	}
	job.close()
}
```

Then the three lines in `drainJob`'s doc comment that name `job.close()` as the thing that kills on the way out — true on Windows, and after this step the deferred teardown is `closeLaunchJob`, which kills and THEN closes, while on Linux the close alone never kills. Edit each:

`autocookies_firefox.go:753` — `// deferred job.close() is about to kill it.` becomes `// deferred closeLaunchJob is about to kill it.`

`:775-776` — replace
```go
// Returning at that moment runs the caller's deferred job.close(), whose
// KILL_ON_JOB_CLOSE kills the real browser mid-page-load — measured, and the
```
with
```go
// Returning at that moment runs the caller's deferred closeLaunchJob, whose
// kill (KILL_ON_JOB_CLOSE on Windows, the group kill on Linux) lands on the
// real browser mid-page-load — measured on Windows, and the
```

`:786` — `//     the caller's job.close() kills them;` becomes `//     the caller's closeLaunchJob kills them;`

- [ ] **Step 5: Run the tests and watch them pass**

Run: `go test -count=1 ./internal/cookies/`
Expected: PASS. Note for the reviewer: `cleanupLocked`'s doc paragraph still says "It kills NOTHING anywhere else"; that sentence is still TRUE at this commit (the seam is unbound) and is rewritten in Task 5, which is the commit that makes it false. The absence-claim rule is satisfied, not skipped.

- [ ] **Step 6: Run every gate, then commit**

```bash
git add internal/cookies/job_pgroup.go internal/cookies/autocookies.go \
        internal/cookies/autocookies_firefox.go \
        internal/cookies/autocookies_setup_reap_pgroup_test.go
git commit -m "$(cat <<'EOF'
feat(cookies): the three close-to-kill sites ask for the kill out loud

On Windows, closing the setup Job Object IS the kill, and cleanupLocked,
adoptSetupJobLocked and runWithTimeout's teardown all lean on that. A process
group is a number, not a handle, so closing one on Linux kills nothing.

Each of the three now calls killTrackedProcesses before the close, and logs when
it cannot. trackedSetupJob's failed-assign drop deliberately does NOT: on Linux
that group is the browser the user is signing into, and a bookkeeping failure
must not close their window. A test pins the omission.

Still a no-op everywhere; job_linux.go binds the hook.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 4: The reap's two-bool pairing, executed over a fake table

**Files:**
- Modify: `internal/cookies/autocookies.go:645-665` (`realSetupBrowserGone`)
- Modify: `internal/cookies/autocookies_setup_reap_pgroup_test.go` (append)

**Interfaces:**
- Consumes: `pgroupJob` (Task 1).
- Produces: `func browserGoneFrom(job interface{ queryable() bool; activeProcesses() (int, error) }) (gone, known bool)`.

Pure refactor: `realSetupBrowserGone` keeps its signature, `setupBrowserGone`'s type is unchanged, `jobReports` is unchanged, and the Windows-only tests are untouched. What it buys is the point of the arc — the Windows test suite can execute the REAL pairing over a real `pgroupJob` and a fake table, instead of a second copy of the rule written in a test.

- [ ] **Step 1: Write the failing test**

Append to `internal/cookies/autocookies_setup_reap_pgroup_test.go`:

```go
// TestBrowserGoneFromAProcessGroup runs the reap's actual predicate over the
// actual Linux liveness type, on Windows, with a fake process table. This is
// what "unit-tested with a fake process table, no Linux box" means: the four
// answers below are the four the reap can receive, and three of them are new
// on Linux.
//
// Mutations, one per case: return (true, true) for an unadopted group and every
// launch that could not be tracked is reaped immediately; return (false, true)
// for the unreadable table and a hardened container reaps a setup whose browser
// is on screen; return `active >= 0` and a live browser reads as gone; return
// known=false for the empty group and the reap never fires at all, which is
// today's bug.
func TestBrowserGoneFromAProcessGroup(t *testing.T) {
	cases := []struct {
		name       string
		table      map[int]int
		unreadable bool
		job        *pgroupJob
		wantGone   bool
		wantKnown  bool
	}{
		{
			name:      "no group adopted — nothing can say",
			table:     map[int]int{setupGroupPid: setupGroupPid},
			job:       &pgroupJob{},
			wantGone:  false,
			wantKnown: false,
		},
		{
			name:       "process table unreadable — nothing can say",
			unreadable: true,
			job:        &pgroupJob{pgid: setupGroupPid},
			wantGone:   false,
			wantKnown:  false,
		},
		{
			name:      "the browser outlived its launcher",
			table:     map[int]int{setupChildPid: setupGroupPid, strangerPid: strangerGroup},
			job:       &pgroupJob{pgid: setupGroupPid},
			wantGone:  false,
			wantKnown: true,
		},
		{
			name:      "the group is empty — gone, and we can say so",
			table:     map[int]int{strangerPid: strangerGroup},
			job:       &pgroupJob{pgid: setupGroupPid},
			wantGone:  true,
			wantKnown: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unreadable {
				unreadableProcessTable(t)
			} else {
				fakeProcessTable(t, tc.table)
			}
			gone, known := browserGoneFrom(tc.job)
			if gone != tc.wantGone || known != tc.wantKnown {
				t.Fatalf("browserGoneFrom = (gone %v, known %v), want (%v, %v)",
					gone, known, tc.wantGone, tc.wantKnown)
			}
		})
	}
}

// TestBrowserGoneFromANilJobStillAnswers keeps the typed-nil path honest. Every
// platform's processJob nil-checks its receiver, which is why the question asked
// is queryable() and not `job != nil` — and why handing this function a nil
// *processJob through an interface parameter is safe rather than a panic.
//
// Mutation: change queryable() on any platform to dereference before checking
// and this panics, which is the failure the interface hides if nobody looks.
func TestBrowserGoneFromANilJobStillAnswers(t *testing.T) {
	gone, known := browserGoneFrom((*processJob)(nil))
	if gone || known {
		t.Fatalf("a nil job answered (gone %v, known %v), want (false, false)", gone, known)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -count=1 -run TestBrowserGoneFrom ./internal/cookies/`
Expected: FAIL to build — `undefined: browserGoneFrom`.

- [ ] **Step 3: Split the body out of `realSetupBrowserGone`**

Replace `realSetupBrowserGone`'s body (`autocookies.go:645-665`) with:

```go
func realSetupBrowserGone(job *processJob) (gone, known bool) {
	return browserGoneFrom(job)
}

// browserGoneFrom is realSetupBrowserGone's body, written against the two
// methods it actually uses rather than against *processJob.
//
// The split buys exactly one thing, and it is the thing the owner's ruling
// asked for. The Linux processJob forwards both methods to a pgroupJob, so a
// test on Windows can hand THIS function a pgroupJob backed by a fake process
// table and execute the real pairing — including the branch that matters most,
// a table that cannot be read answering "cannot say" rather than "gone". No
// Linux box, no browser, and no second copy of the rule to drift.
//
// queryable, not `job != nil`. activeProcesses answers 0 for three different
// situations and only one of them is "the job is empty": a nil job (a launch
// where newProcessJob failed, or where the assign failed and the launcher
// dropped the untrackable job rather than let it lie), an already-closed handle
// or a forgotten group, and a platform whose processJob cannot count at all.
// The type knows which it is; this does not.
//
// Passing a nil *processJob through the interface parameter still answers
// (false, false): all three platform implementations nil-check their receiver,
// which is the same property that makes queryable() the right question.
func browserGoneFrom(job interface {
	queryable() bool
	activeProcesses() (int, error)
}) (gone, known bool) {
	if !job.queryable() {
		return false, false
	}
	active, err := job.activeProcesses()
	if err != nil {
		return false, false
	}
	return active == 0, true
}
```

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test -count=1 ./internal/cookies/`
Expected: PASS — the six new subtests plus the sixteen existing cross-platform reap tests and the eleven Windows-only ones, all unchanged.

- [ ] **Step 5: Run every gate, then commit**

```bash
git add internal/cookies/autocookies.go internal/cookies/autocookies_setup_reap_pgroup_test.go
git commit -m "$(cat <<'EOF'
refactor(cookies): the reap's predicate can be run against a process group

realSetupBrowserGone keeps its signature and its seam; its body moves to
browserGoneFrom, which takes the two methods it uses instead of *processJob. The
Linux processJob forwards both to a pgroupJob, so the Windows test suite can now
execute the real predicate over a fake process table — including the branch that
matters most, an unreadable /proc answering "cannot say" rather than "gone".

No behaviour change on any platform.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 5: Linux binds it — `Setpgid`, `/proc`, and every sentence that stops being true

**Files:**
- Modify: `internal/cookies/job_linux.go` (whole file)
- Create: `internal/cookies/job_linux_smoke_test.go`
- Modify: `internal/cookies/autocookies.go` (`:219`, the `setupBrowserGone` doc at `:596-638`, `AbandonSetup`'s doc at `:1495-1502`, `killSetupProcess`'s doc at `:3118-3121`, `cleanupLocked`'s doc at `:3213-3226` — line numbers as of `390adb6`, before Tasks 2-4 shift them; find by symbol)
- Modify: `internal/cookies/autocookies_firefox.go` (`:60`, `:74-76`, `:232`, `refreshLooksImplausiblyFast`'s doc at `:376-380`, the `drainJob` comment at `:857-868`)
- Modify: `internal/cookies/autocookies_chromium.go` (`:65`, `:234`)
- Modify: `docs/spec/operations.md:122`, `:124`, `:126`, `:128`, `:130`, `:134`
- Modify: `SPEC.md:680`

**Interfaces:**
- Consumes: `listProcessGroups`, `killProcessGroup`, `killTrackedProcesses`, `pgroupJob`, `parseProcStatPGID` (Tasks 1, 3).
- Produces: on Linux only — `type processJob struct{ group pgroupJob }` with the same five-method contract `job_windows.go` carries; `func readProcProcessGroups() (map[int]int, error)`; `func sigkillProcessGroup(pgid int) error`.

This is the arming commit. It is also the commit where every doc sentence that says the reap is Windows-only stops being true, so it edits them — the absence-claim rule, same commit.

- [ ] **Step 1: Rewrite `internal/cookies/job_linux.go`**

```go
//go:build linux

package cookies

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// init binds the tag-free decision layer in job_pgroup.go to the real Linux
// primitives.
//
// An init rather than three var initialisers because the defaults have to live
// in the tag-free file: the alternative is a second file carrying `!linux`
// stubs, i.e. two places that must agree about what an unbound hook answers.
// One binding site, next to the platform code it binds, is the smaller of the
// two.
func init() {
	listProcessGroups = readProcProcessGroups
	killProcessGroup = sigkillProcessGroup
	killTrackedProcesses = func(j *processJob) error {
		if j == nil {
			return nil
		}
		return j.group.killGroup()
	}
}

// processJob is the Linux answer to the Windows Job Object: a process GROUP.
//
// Every browser this package launches leads one of its own —
// configureCmdSysProcAttr sets Setpgid, so the child's pid IS the group id —
// and that group is what makes the abandoned-setup reap work here at all. The
// launcher hands off and exits; the group is the only thing that outlives it
// and can still be counted.
//
// THE TYPE IS DELIBERATELY THIN. Every decision — what counts as a member, when
// a kill is allowed, what a failed table read means — is in job_pgroup.go,
// which carries no build tag and is therefore exercised by the ordinary test
// suite on the Windows machine this project is developed on. What is left here
// is the platform binding: /proc, syscall.Kill, Setpgid. Keep it that way.
// Anything with a branch in it belongs on the other side of that line, because
// nothing in this file is executed by any test that runs here.
type processJob struct {
	group pgroupJob
}

func newProcessJob() (*processJob, error) { return &processJob{}, nil }

// assign records the launched process's group.
//
// It returns an error — and so makes trackedSetupJob DROP the job — whenever
// the group cannot be confirmed: an unreadable /proc, a pid already gone, or a
// process that did not lead its own group. That is the honest degradation. No
// group means no answer, no reap and no kill, which is exactly the pre-A1
// behaviour, plus a Warn saying why the reap will not work on this install.
func (j *processJob) assign(p *os.Process) error {
	if j == nil || p == nil {
		return nil
	}
	return j.group.adopt(p.Pid)
}

// close FORGETS the group. It does not kill.
//
// There is no KILL_ON_JOB_CLOSE to imitate: a pgid is an integer the kernel
// recycles, not a handle, and a close that killed would fire at trackedSetupJob's
// failed-assign drop — where the group is the browser the user is signing into.
// The three sites that relied on the Windows close to kill call
// killTrackedProcesses first; see its doc for the list and for the site that
// deliberately does not.
func (j *processJob) close() {
	if j != nil {
		j.group.forget()
	}
}

// activeProcesses counts what is still in the group. A failed /proc read is an
// error, never a zero — see pgroupJob.activeProcesses.
func (j *processJob) activeProcesses() (int, error) {
	if j == nil {
		return 0, nil
	}
	return j.group.activeProcesses()
}

// queryable reports whether a group was adopted, which is this platform's
// version of the Windows "the handle is live" question. Unlike before this
// arc, it can now be TRUE — which is what lets realSetupBrowserGone answer, and
// the reap fire, on Linux and in Docker.
func (j *processJob) queryable() bool { return j != nil && j.group.queryable() }

// configureCmdSysProcAttr puts the launched browser in its OWN process group
// and arranges for the kernel to SIGKILL it when Moombox dies.
//
// Setpgid with a zero Pgid means "new group, id = the child's pid". That group
// is what processJob counts and kills: without it the child inherits Moombox's
// group, adopt() refuses it (correctly — killing that group would kill Moombox
// and, in Docker, the container), and this platform keeps the pre-A1 behaviour.
//
// Pdeathsig stays for the reason it arrived: a crashed Moombox would otherwise
// orphan a headless refresh browser that holds the profile lock and silently
// breaks every later refresh. It is best-effort and covers only the DIRECT
// child — the group is what covers what the launcher handed off to.
//
// Two consequences worth stating rather than discovering. A browser that calls
// setsid() (or setpgid) LEAVES the group and becomes invisible to both the
// count and the kill — and that is a WRONG answer, not a missing one: the group
// then reads as empty, the reap releases the slot when the grace runs out with
// the browser still on screen (no kill — killGroup finds no members), and the
// user's next finish answers ErrNoSetupInProgress. Which Linux packagings do
// this (a snap or flatpak wrapper is the suspect, not the browser binary) is
// unmeasured; a field report is the gate. And the browser is no longer in
// Moombox's foreground process group, so a terminal Ctrl-C no longer reaches
// it — which is the desirable direction for a window the operator is typing a
// password into.
func configureCmdSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// readProcProcessGroups walks /proc once and returns pid → pgid.
//
// AN UNREADABLE /proc IS AN ERROR, NEVER AN EMPTY MAP. An empty map counts as
// "the group has no members left", which is the reading that releases an
// acquisition slot, and a hardened container must not arrive there by accident.
// Individual entries that vanish between the readdir and the read are skipped:
// that is a process exiting, not a failure of the table.
func readProcProcessGroups() (map[int]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	table := make(map[int]int, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue // /proc/self, /proc/meminfo, and the rest of the non-pid entries
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited between the readdir and the read
		}
		if pgid, ok := parseProcStatPGID(string(data)); ok {
			table[pid] = pgid
		}
	}
	return table, nil
}

// sigkillProcessGroup SIGKILLs a whole process group; the negative pid is
// kill(2)'s group form.
//
// The guard is the third and last check on the same hazard, and it is the one
// closest to the syscall: kill(-0, …) signals the CALLER'S group, so a zero
// here would SIGKILL Moombox and everything it has launched — in Docker, where
// Moombox usually leads its own group, the container. pgroupJob.killGroup and
// killProcessTreeUnix both check before calling. Never remove any of the three.
func sigkillProcessGroup(pgid int) error {
	if pgid <= 0 {
		return fmt.Errorf("refusing to signal process group %d", pgid)
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}
```

- [ ] **Step 2: Prove it compiles for both Linux architectures**

Run: `GOOS=linux GOARCH=amd64 go build ./...` then `GOOS=linux GOARCH=arm64 go build ./...`
Expected: both silent. (`go vet` cannot check the Linux arm from Windows; the two builds are the gate that the arm is real code.)

- [ ] **Step 3: Add the opt-in Linux smoke test**

Create `internal/cookies/job_linux_smoke_test.go`:

```go
//go:build linux

package cookies

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestLivePgroupSeesAndKillsARealGroup is a SMOKE TEST and nothing depends on
// it.
//
// IT IS NOT RUN IN CI HERE, and that is deliberate rather than an oversight.
// The release workflow cross-compiles the Linux binaries from ubuntu and never
// runs `go test`; the machine this project is developed on is Windows; and the
// owner's ruling for this arc is explicit that there is no Linux live gate and
// that a user's bug report is the gate. Every DECISION this file's production
// code makes is already pinned cross-platform in job_pgroup_test.go against a
// fake process table. What is left here — Setpgid actually taking effect, /proc
// actually parsing, kill(-pgid) actually landing — needs a real kernel, so it
// sits behind an env var for whoever has one:
//
//	MOOMBOX_LIVE_PGROUP=1 go test -count=1 -run TestLivePgroup ./internal/cookies/
//
// It launches `sh -c "sleep 30"` and kills it. It never touches a browser, a
// profile or a cookie file.
func TestLivePgroupSeesAndKillsARealGroup(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_PGROUP") != "1" {
		t.Skip("set MOOMBOX_LIVE_PGROUP=1 on a Linux box to run this; it is not a CI gate")
	}

	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	configureCmdSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start /bin/sh: %v", err)
	}

	job, err := newProcessJob()
	if err != nil {
		t.Fatalf("newProcessJob: %v", err)
	}
	if err := job.assign(cmd.Process); err != nil {
		t.Fatalf("assign: %v — Setpgid did not put the child in its own group, "+
			"or /proc could not be read", err)
	}
	if !job.queryable() {
		t.Fatal("queryable() is false after a successful assign")
	}

	n, err := job.activeProcesses()
	if err != nil {
		t.Fatalf("activeProcesses: %v", err)
	}
	if n < 1 {
		t.Fatalf("activeProcesses() = %d for a group that is provably running", n)
	}

	if err := killTrackedProcesses(job); err != nil {
		t.Fatalf("killTrackedProcesses: %v", err)
	}
	// Reap the direct child BEFORE counting. Until it is waited on it is a
	// zombie whose stat line still carries the pgrp; the parser skips state Z
	// (decision 8), so this is belt and braces — but a smoke test that leaned
	// on that would be pinning the wrong thing. A deferred Wait here would
	// leave the zombie in place for the whole loop below.
	_ = cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := job.activeProcesses()
		if err != nil {
			t.Fatalf("activeProcesses after the kill: %v", err)
		}
		if n == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%d processes still in the group 5s after SIGKILL", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
```

- [ ] **Step 4: Flip the code comments that just stopped being true**

Nine edits, all comments, no behaviour:

1. `autocookies.go:219` — the field comment:
```go
	setupJob      *processJob // Windows: a Job Object. Linux: the browser's process group. nil elsewhere
```

2. `autocookies.go` — `setupBrowserGone`'s third "WHERE THAT LEAVES THE REAP" bullet (currently "Linux, and every non-Windows target — there is no Job Object PRIMITIVE at all…"). Replace that bullet with:
```go
//   - Linux and Docker — answerable since the process-group reap landed.
//     configureCmdSysProcAttr sets Setpgid, so every browser leads its own
//     group; queryable() is true once that group was adopted, and
//     activeProcesses counts its members from /proc. One case still answers
//     "no idea", honestly: a container whose /proc cannot be walked. One
//     answers WRONGLY: a browser that called setsid() and left the group
//     reads as gone, and the reap releases the slot with it still on screen
//     (no kill — the group it would signal is empty). Which packagings do
//     that is unmeasured. NOT FIELD-VERIFIED — built and unit-tested against
//     a fake process table, with a user's bug report as the gate.
//   - darwin and every other target — no primitive at all (job_other.go is
//     still a no-op stub), so nothing is answerable and the reap never fires.
//     The client-side cancel (the unload beacon, Skip, Escape, the TUI
//     countdown) is what clears an abandoned setup there.
```
Also change the summary line that follows it — "Two answerable cases and one that is not" — to "Three answerable targets and one that is not", and its "on Windows wherever newProcessJob or its assign failed" to "on Windows and Linux wherever newProcessJob or its assign failed"; keep the rest of the paragraph as written.

3. `autocookies.go` — `cleanupLocked`'s doc paragraph beginning "It kills NOTHING anywhere else":
```go
// IT KILLS ON LINUX TOO NOW, but by a different route: the close only forgets a
// process-group id there, so the killTrackedProcesses call in the body below is
// what reaches the browser. Same consequence, same requirement on callers. On darwin and the
// fallback build job_other.go is still a no-op stub, nothing is tracked and
// nothing is killed; a browser left behind there keeps running (pdeathsig ties
// it to Moombox's death, not to this call).
```

4. `autocookies_firefox.go:60`, `autocookies_chromium.go:65`, `autocookies_chromium.go:234` — the inline launch comments:
```go
	configureCmdSysProcAttr(cmd) // Linux: PR_SET_PDEATHSIG + Setpgid (the group the reap counts); Windows: no-op (Job Object below)
```
and `autocookies_firefox.go:232` with `(Job Object in runWithTimeout)` as its tail instead of `(Job Object below)`.

5. `autocookies_firefox.go` — `startFirefoxSetup`'s "WINDOWS ONLY" paragraph:
```go
	// WINDOWS AND LINUX. newProcessJob is a Job Object on Windows and a process
	// group on Linux, and the reap fires on both. It is still a no-op stub on
	// darwin and the fallback build, where the reap stays dead — see
	// setupBrowserGone's fourth case.
```

6. `autocookies_firefox.go` — `drainJob`'s "It is also the norm on two platforms" paragraph:
```go
				// It is the norm on darwin and the fallback build, where
				// processJob is still a no-op stub returning 0 from
				// activeProcesses unconditionally, so every launch there lands
				// here on lap zero having drained nothing. Linux is different
				// in kind: its process group gives a real count, so landing
				// here means the group really was empty when the direct child
				// was reaped — the usual case there, since without a launcher
				// the direct child IS the browser — and anything that had
				// outlived it would have been waited for. Claiming a finish
				// would still assert something a stub platform cannot observe,
				// the same distinction the !rendered branch in refreshFirefox
				// holds.
```

7. `autocookies.go` — `AbandonSetup`'s doc (`:1474-1512`). The second bullet of its `known` split and the paragraph after it are both false once Linux can answer. Replace the bullet beginning `//   - not known — no job, or a platform with no Job Object primitive at all` (four lines, through `releasing kills nothing. Release.`) with:
```go
//   - not known — no job (a failed assign on either platform), a Linux group
//     whose /proc cannot be read, or a platform with no primitive at all
//     (darwin and the fallback build). The reap can never fire there, so this
//     is the only thing that releases the slot; and with nothing able to reach
//     the browser, releasing kills nothing — on Linux the group kill inside
//     cleanupLocked refuses a group it cannot see. Release.
```
and the paragraph `// Which is to say plainly: this call is redundant on Windows and load-bearing` … `deleting the check would restore the kill.` with:
```go
// Which is to say plainly: this call is redundant wherever a group or a Job
// Object was adopted — Windows, and Linux since the process-group reap — and
// load-bearing where nothing was: darwin, the fallback build, and any Linux
// launch whose group could not be adopted. The declining arm is not dead code
// on either platform — deleting the check would restore the kill on both.
```

8. `autocookies.go` — `killSetupProcess`'s doc, its closing paragraph (`:3118-3121`). Replace the three lines from `// no job can vouch for the browser — a failed assign, and every non-Windows` through `// there it is the only thing that can work.` with:
```go
// no job can vouch for the browser — a failed assign on either platform, and
// darwin and the fallback build, where queryable() is always false — the kill
// still runs, because there it is the only thing that can work. On Linux that
// kill goes through killProcessTreeUnix, which signals a group only when the
// pid still leads one; a reaped pid falls back to proc.Kill, which Go refuses
// on a process it has already waited on.
```

9. `autocookies_firefox.go:376-378` — `refreshLooksImplausiblyFast`'s doc. Replace `// On Linux there is no Job Object, so drainJob returns on lap zero (see its` … `// here.` (the first two and a half lines of that paragraph) with:
```go
// On darwin and the fallback build there is no count at all, so drainJob
// returns on lap zero (see its doc comment) and a launch that genuinely worked
// can still read as fast here. Linux has a count since the process-group reap,
// but no timing has been recorded there, so the threshold is a Windows number.
```
keeping the sentence that follows (`The Debug message at the call site …`) as written.

- [ ] **Step 5: Rewrite the six `docs/spec/operations.md` sentences**

`:122` — the section's opening paragraph says "both depend on a primitive only Windows has". Replace that paragraph's first two sentences with:

> The interactive cookie setup and the headless refresh both launch a real browser, and both depend on a liveness primitive: a Job Object on Windows, a process group on Linux, nothing on darwin or the fallback build. What each platform can and cannot OBSERVE is stated here because the differences are recorded residuals, not oversights.

`:124` — extend the first sentence of the reap paragraph so the primitive is named per platform. Change its opening to:

> **The reap keys on a liveness primitive reporting zero live processes, never on `cmd.Wait()`.** On Windows that primitive is a Job Object; on Linux it is the browser's own process group. A Firefox-family launcher hands off … *(rest unchanged)*

`:126` — replace the whole "Off Windows nothing can say" paragraph with:

> **Windows counts a Job Object; Linux counts a process group; nothing else can say.** `processJob.queryable()` is `j != nil && j.handle != 0` in `job_windows.go` and `j != nil && j.group.queryable()` — that is, a process group was adopted — in `job_linux.go`. `configureCmdSysProcAttr` sets `Setpgid` on Linux, so every browser this package launches leads a group whose id is its own pid; `activeProcesses` counts that group's members by walking `/proc`, and `assign` REFUSES a process that did not lead its own group, because recording one that inherited Moombox's group would later point the kill at Moombox itself — in Docker, at the container. `job_other.go` is still unconditionally `false`: darwin and the fallback build have no primitive, `realSetupBrowserGone` always answers "not known" there, and a browser left open by an abandoned setup is not reaped. One Linux case answers "not known" too, honestly: a container whose `/proc` cannot be walked (a failed read is an error, never a zero — an empty table would read as "the group is empty", which is the answer that releases the slot). One answers WRONGLY: a browser that calls `setsid()` leaves the group, the count then reads the group as empty, and the reap releases the slot when the grace runs out with the browser still on screen — no kill, because the group it would signal has no members, but the operator's next finish answers `ErrNoSetupInProgress`. Which Linux packagings do that (a snap or flatpak wrapper is the suspect, not the browser binary) is unmeasured. A zombie is not a member: the `/proc` parser skips state `Z`, because a container where Moombox is PID 1 without an init (`docker-compose.yml` sets `init: true`; a bare `docker run` does not) never reaps an orphaned grandchild, and counted it would hold the group open for the life of the process. **The Linux reap is BUILT, NOT FIELD-VERIFIED.** Its decisions are unit-tested against a fake process table (`internal/cookies/job_pgroup_test.go`, which is why they live in a file with no build tag); nothing here has been run against a real Linux desktop or container, and a user's bug report is the gate. One named residual comes with it: a Job Object handle cannot name a process that is not in the job, but a pgid can, because the kernel recycles pids — `pgroupJob.killGroup` refuses to signal a group it cannot see or that has no members left, which narrows the window as far as it can be narrowed without closing it.

`:128` — append to the `AbandonSetup` paragraph, after "…because nothing was tracking the browser anyway.":

> Since the process-group reap landed, Linux takes the SAME declining arm as Windows wherever a group was adopted: the reap owns that slot. The release arm is now scoped to the cases where nothing can answer — a failed assign or an unreadable `/proc` on Linux, darwin and the fallback build — and it is still the only release there; the `cleanupLocked` it runs asks for a group kill on Linux, and that kill refuses a group it cannot see, so releasing still kills nothing. (A browser that left its group is the OTHER arm's problem: it reads as gone — see above.) The rule did not change; the set of platforms on each side of it did.

`:130` — the "launcher-handoff premise is UNMEASURED" paragraph says "that platform drains nothing either way", which stops being true here. Replace its third sentence (`Nothing depends on it being true — that platform drains nothing either way — but it should not be repeated as a fact.`) with:

> Nothing depends on it being true — the process group counts and kills whatever is in it whether or not a handoff happened, and where nothing outlives the direct child the drain lands on lap zero exactly as before — but it should not be repeated as a fact.

`:134` — in the drain paragraph, replace the closing "LibreWolf and Zen remain unverified, as does every non-Windows target." with:

> LibreWolf and Zen remain unverified. So does every non-Windows target — but the reason changed on Linux: it now HAS a count (the process group), so `drainJob` waits there for anything that outlives the direct child — a wrapper's handoff, a straggling content process — up to the budget, where before it returned on lap zero regardless. Without a launcher the direct child is the browser itself and `cmd.Wait()` already waited for it, so where nothing outlives it the timing is unchanged. That is the Arc 0 fix arriving on Linux, and it is unobserved: no timing has been recorded on any Linux box.

- [ ] **Step 6: Update `SPEC.md:680`**

In the Deep-dive line, change `(the Job-Object reap, `AbandonSetup`, the drain)` to:

> (the reap — a Job Object on Windows, a process group on Linux, nothing on darwin — `AbandonSetup`, the drain)

- [ ] **Step 7: Run every gate**

Run all six commands. Expected: all clean. `go test -count=1 ./...` passes on Windows with the Linux files excluded by their build tags; the two Linux cross-builds are what prove the new file compiles.

- [ ] **Step 8: Commit**

```bash
git add internal/cookies/job_linux.go internal/cookies/job_linux_smoke_test.go \
        internal/cookies/autocookies.go internal/cookies/autocookies_firefox.go \
        internal/cookies/autocookies_chromium.go docs/spec/operations.md SPEC.md
git commit -m "$(cat <<'EOF'
feat(cookies): the abandoned-setup reap fires on Linux and in Docker

A1 was fixed on Windows by a Job Object and left open everywhere else: queryable()
was unconditionally false off Windows, so realSetupBrowserGone never answered, the
reap never fired, and an abandoned wizard held the acquisition slot until a client
happened to call abandon or cancel.

Linux now launches every browser into its own process group (Setpgid), counts the
group's members from /proc, and kills it explicitly where Windows relies on
KILL_ON_JOB_CLOSE. Same 60s grace, same "release only where releasing cannot kill"
rule, same predicate. The decisions are unit-tested against a fake process table on
Windows; job_linux.go is the binding only.

Not field-verified, by the owner's ruling: no Linux live gate, a user's bug report
is the gate. operations.md and SPEC.md say so, along with the two cases that still
answer "cannot say" (unreadable /proc, a browser that calls setsid) and the pgid
recycling residual a Job Object handle does not have.

Side effect, stated: drainJob now has a real count on Linux, so a headless refresh
there waits for the browser instead of returning on lap zero.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Task 6: The ledgers stop calling it unfixed

**Files:**
- Modify: `docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:1876` (the Arc 3 completion paragraph's "load-bearing on Linux/Docker"), `:1878-1880` (the named-residual block), `:1980` (the Arc 3 doc-dependency bullet's "not reaped on Linux or in Docker" — the owner's own phrase at `:2015`), `:1996` (the Arc 3 bullet), and the Deferred entry whose ruling this plan discharged
- Modify: `docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md:172` (Part 4 row 14, the Docker deployment row) and `:302` (the Part 7 row)

**Interfaces:**
- Consumes: the shipped behaviour from Task 5.
- Produces: nothing code-facing.

Separate from Task 5 on purpose. The spec docs state what the software DOES, so they must move in the commit that changes it — that was Task 5. These two are project ledgers: they record what an arc decided and what a field test should look for, and a bookkeeping commit is the right home for them.

- [ ] **Step 1: Replace the named-residual block at `:1880`**

Replace the whole `> **NAMED RESIDUAL — A1 IS NOT FIXED ON LINUX OR IN DOCKER…** ` blockquote with:

```markdown
> **RESIDUAL CLOSED — A1 IS FIXED ON LINUX AND IN DOCKER, AND NOT FIELD-VERIFIED.** Owner ruling Q9 REVISED (`:2015`) sent this back as a build item, and its own arc delivered it: `configureCmdSysProcAttr` sets `Setpgid` on Linux, `processJob` is a process group, `queryable()` can answer, and the reap fires on the same predicate and the same 60 s grace as Windows. The counting and killing DECISIONS live in `internal/cookies/job_pgroup.go`, which carries no build tag, so every branch runs in the Windows test suite against a fake process table; `job_linux.go` is the `/proc` and `syscall.Kill` binding only. Because a Linux close cannot kill (a pgid is a recycled integer, not a kernel handle), three sites that relied on KILL_ON_JOB_CLOSE now ask for the kill explicitly — `cleanupLocked`, `adoptSetupJobLocked`, `runWithTimeout`'s teardown — and one deliberately does not: `trackedSetupJob`'s failed-assign drop, where the group is the browser the user is signing into. **The gate is a field report**, per the ruling: nothing here has run on a Linux desktop or in a container. `job_other.go` is unchanged and darwin keeps the residual, where the `abandon` beacon is still the only release.
```

- [ ] **Step 2: Rewrite the Arc 3 bullet at `:1996`**

In the "What the docs must now state" list, replace the Arc 3 bullet's final clause — `**A1 is NOT fixed on Linux/Docker** (no Job Object primitive) — the docs must say so.` — with:

```markdown
**A1 IS fixed on Linux/Docker as of the process-group arc** (`Setpgid` at launch, `/proc` for the count, an explicit group kill where Windows closes a handle) — **built, not field-verified**, with a user's bug report as the gate; darwin and the fallback build keep the residual. The docs say so at `operations.md` §Browser Cookie Acquisition.
```

- [ ] **Step 2b: The two Arc 3 history lines**

`:1876` — in the Arc 3 completion paragraph, `It is redundant on Windows (the reap owns it) and load-bearing on Linux/Docker (the only release there).` becomes `It was redundant on Windows (the reap owns it) and load-bearing on Linux/Docker (the only release there) until the process-group arc gave Linux a reap of its own — see the closed residual below.`

`:1980` — in the Arc 3 doc-dependency bullet, `so **a browser left by an abandoned setup is not reaped on Linux or in Docker** (both Windows families do reap)` becomes `so **a browser left by an abandoned setup was not reaped on Linux or in Docker** until the process-group arc (both Windows families reaped from the start)`.

- [ ] **Step 3: Discharge the Deferred entry**

In §Deferred, the bullet beginning `**A1 on Linux/Docker (the Job-Object reap)**` — append to its ruling text:

```markdown
*(DELIVERED. Own arc after Arc 11: `internal/cookies/job_pgroup.go` (tag-free decisions), `job_linux.go` (binding), the `killProcessTree` seam and the three close-to-kill sites. The `operations.md` sentences it was required to edit are edited. The Linux launcher-handoff premise stays UNMEASURED, as ruled.)*
```

- [ ] **Step 4: Flip the field-test plan row at `:302`**

Replace the A1 row in Part 7 with a Part 7 row that says what is and is not on the table:

```markdown
| A1 (abandoned setup wedges acquisition) on Linux / Docker | **Built, not field-verified.** The reap fires on Linux and in Docker via a process group (`Setpgid` at launch, `/proc` for the count, an explicit group kill where Windows closes a Job Object handle); the decisions are unit-tested against a fake process table on Windows. By owner ruling there is NO Linux live gate here — a user's bug report is the gate. Still unreaped on darwin and the fallback build, where `abandon` remains the only release | plan §Arc 3 residual (closed); `a1-linux-process-group-reap-design.md` |
```

- [ ] **Step 4b: Flip the field-test plan's Docker deployment row at `:172`**

Part 4 row 14 ("First real Docker deployment") carries the old claim in two columns. In the "Pass looks like" column replace `The `abandon` beacon is the ONLY release for an abandoned setup there (A1 not fixed on Linux - plan §Arc 3 named residual)` with `An abandoned setup there is reaped by the process group ~60 s after the browser is gone (A1 built on Linux, not field-verified — Part 7 row); the `abandon` beacon still releases where no group could be adopted`. In the "Fail looks like" column replace `setup wedged with no way out but restart (expected on Linux, but confirm the beacon releases on tab close)` with `setup wedged with no way out but restart (NOT expected any more — a wedge that outlives the grace is exactly the field report the owner's ruling names)`.

- [ ] **Step 5: Confirm nothing else in the tree still calls it unfixed**

Run:
```bash
grep -rn "not fixed on Linux\|NOT fixed on Linux\|no Job Object primitive\|Off Windows nothing can say\|not reaped on Linux\|load-bearing on Linux\|every non-Windows target\|drains nothing either way\|there is no Job Object, so" docs/ SPEC.md internal/
```
Expected hits, and only these: the owner's ruling at `2026-08-25-cookie-subsystem-remediation.md:2015` (it QUOTES the sentences it ordered edited — leave it); `internal/cookies/job_other.go` (still the stub it says it is — leave it); and this plan and its spec under `docs/superpowers/`, which describe the change. Any other hit is a sentence Task 5 or this task missed — fix it in this commit, and if it is a code comment say so in the commit body.

- [ ] **Step 6: Run every gate, then commit**

The code gates still run: a docs-only commit must not be the one that discovers a broken build.

```bash
git add docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md \
        docs/superpowers/plans/2026-08-29-cookie-remediation-field-test-plan.md
git commit -m "$(cat <<'EOF'
docs(plans): A1 on Linux/Docker is built — the ledgers say built, not fixed-and-proven

The named residual in the remediation plan, its two Arc 3 bullets and the Arc 3
completion line, its Deferred entry, and the field-test plan's Part 4 Docker row
and Part 7 row all said A1 was unfixed off Windows. It is built now. Each says
"built, not field-verified" and names the gate the owner set: a user's bug
report, not a Linux box of ours.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01N7hSoKxnW7sCfiCQXtMSyN
EOF
)"
```

---

## Self-Review

### 1. Spec coverage

| Spec | Where it lands |
|---|---|
| **R1** — every setup and refresh browser launched into its own group, all four launch sites unchanged because they already call the shared helper | Task 5 Step 1 (`configureCmdSysProcAttr` gains `Setpgid: true` beside `Pdeathsig`); the four call sites (`autocookies_firefox.go:60`, `:232`, `autocookies_chromium.go:65`, `:234`) get comment-only edits in Task 5 Step 4 |
| **R2** — `processJob` on Linux is real: `assign` records the group, `queryable` is `pgid > 0`, `activeProcesses` counts group members | Task 1 (`pgroupJob`), Task 5 Step 1 (the five forwarding methods) |
| **R3** — the process table is a seam and the decision logic is tag-free | Task 1 (`job_pgroup.go`, `listProcessGroups`, `killProcessGroup`, all tests on Windows), Task 4 (`browserGoneFrom`, so the reap's own predicate runs over a `pgroupJob` on Windows too) |
| **R4** — closing kills nothing; every close-to-kill site kills explicitly | Task 2 (`killProcessTree`'s five call sites in one edit), Task 3 (`cleanupLocked`, `adoptSetupJobLocked`, `runWithTimeout`'s teardown; `trackedSetupJob` pinned as the site that must not), decision 5's table |
| **R5** — the same rules verbatim: 60 s grace, `reapAbandonedSetupLocked` unchanged, `AbandonSetup` takes the declining arm on Linux | No task touches `setupAbandonGrace` (`autocookies.go:117`), `reapAbandonedSetupLocked`, `setupRetainedLocked` or `AbandonSetup`'s body; the branch flips by itself once `queryable()` answers. Task 5 Step 5 writes the consequence into `operations.md:128` |
| **R6** — a real count arms the drain on Linux (`drainJob` waits for whatever outlives the direct child; `errBrowserDrainTimeout` can reach `browserLaunchActed`); the timing change is stated in the docs precisely — a Linux refresh never returned instantly, because without a launcher the direct child is the browser | Decision 6; Task 3 (`runWithTimeout` teardown); Task 5 Step 4 (`drainJob`'s comment) and Step 5 (`operations.md:130`, `:134`) |
| **R7** — what stays unmeasured is said plainly; no Linux live gate; the field-test row flips | Task 5 Step 5 (`operations.md:126` names an unreadable `/proc` as "cannot say", setsid as a WRONG answer — gone, premature release, no kill — zombies as non-members, the pgid-recycling residual as one window, and "built, not field-verified"), Task 5 Step 3 (the smoke test's doc says it is not a CI gate), Task 6 Steps 4 and 4b (the field-test rows) |
| Non-goals — no Windows change, no `job_other.go` change, no reaper goroutine, no sidecar/launcher work | Global Constraints; decision 1; no task lists those files |
| §4 invariants | Global Constraints, and one test each: failed read never releases or kills (`TestPGroupJobReportsAFailedTableReadAsAnError`, `TestPGroupJobNeverKillsWhatItCannotSee/table unreadable`, `TestBrowserGoneFromAProcessGroup/process table unreadable`); `pgid <= 0` (`TestPGroupJobNeverKillsWhatItCannotSee/no group adopted`, `TestKillProcessTreeUnixRefusesANonPositivePid`, `sigkillProcessGroup`'s own guard); no goroutine added (none in any task) |
| §5 docs | `operations.md:122`/`:124`/`:126`/`:128`/`:130`/`:134` and `SPEC.md:680` in Task 5; the remediation plan's `:1876`/`:1880`/`:1980`/`:1996` and the field-test `:172`/`:302` in Task 6. The code comments that make the same claim (`AbandonSetup`'s and `killSetupProcess`'s docs, `refreshLooksImplausiblyFast`'s, `drainJob`'s three `job.close()` lines) are Task 5 Step 4 items 7-9 and Task 3 Step 4 |

### 2. Placeholder scan

No "TBD", no "add appropriate error handling", no "similar to Task N", no test described without its code. Every code step carries the literal text to write; every doc step carries the literal replacement sentence. The one deliberately non-literal step is Task 5 Step 4 items 2/3/5/6, which say which paragraph to replace and give the full replacement — the surrounding paragraph is quoted by its opening words rather than reproduced in full, because reproducing ~40 lines of unchanged prose would obscure the change.

### 3. Type consistency

`pgroupJob` has exactly one spelling of each method across Tasks 1, 4 and 5: `adopt(pid int) error`, `queryable() bool`, `activeProcesses() (int, error)`, `killGroup() error`, `forget()`. `killTrackedProcesses` is `func(*processJob) error` where it is declared (Task 3), where it is called (Task 3, three sites), where it is bound (Task 5) and where it is recorded (`captureTrackedKills`). `killProcessGroup` is `func(int) error` in its declaration, in `captureGroupKills`, in `killProcessTreeUnix` and in `sigkillProcessGroup`. `listProcessGroups` is `func() (map[int]int, error)` in its declaration, in both test binders and in `readProcProcessGroups`. `browserGoneFrom` takes the two-method anonymous interface in Task 4 and is called with both a `*pgroupJob` and a typed-nil `*processJob`, both of which satisfy it. `closeLaunchJob`'s logger parameter repeats the three-method anonymous interface `runWithTimeout` already uses, per CLAUDE.md.

### 4. Where the code contradicted the spec draft

Four places. The R's still bind; the plan follows the code's shape and says so here.

1. **`job_windows.go` "has no build tag" is not the whole truth.** The file map calls it "syscall-only, Windows in practice". It is Windows-only *by rule*: Go applies an implicit GOOS constraint to any file named `*_windows.go`. So the question "does it need `//go:build windows` now that a tag-free sibling exists" answers itself — no, the tag would be redundant with the filename, and the file stays out of the diff. Decision 1.

2. **R4 names three sites; the code has more, and one of them must NOT kill.** The draft lists `killSetupProcess`, `cleanupLocked` and "the headless refresh's timeout path". Reading the tree adds `adoptSetupJobLocked` (a close that is deliberately a kill on Windows) and, more importantly, `trackedSetupJob` (a close that must never kill on Linux, because the group there is the user's live browser while on Windows the job is empty). That last one is the strongest argument FOR R4 — stronger than the draft's own "there is no KILL_ON_JOB_CLOSE equivalent" — and it is why this plan pins it with a test rather than leaving it implicit. Decision 3, decision 5's table.

3. **`runWithTimeout`'s "timeout path" is not the arm that needs the kill.** The draft says the timeout path. The code's explicit timeout arm (`autocookies_firefox.go:972`) already calls `killProcessTree`, which Task 2 makes group-aware — so it is covered without touching it. The arms that reach the deferred `job.close()` with a browser still alive are the drain-timeout arm (`errBrowserDrainTimeout`) and the ctx-cancelled arm. Task 3 puts the kill in the teardown, which covers all three.

4. **R2 has a side effect the draft does not mention: `drainJob` arms on Linux.** Giving `activeProcesses()` a real count there means `runWithTimeout` starts genuinely waiting for the browser group to empty, where every Linux launch previously returned on lap zero with "nothing was waited on". This is the Arc 0 fix arriving on Linux and it is desirable, but it changes Linux refresh timing and lets `browserLaunchActed` report a real verdict there. The plan states it up front (decision 6) and edits the two paragraphs that assert the old behaviour (`drainJob`'s comment, `operations.md:134`) — the latter is a fifth doc sentence the draft's §5 does not list.

5. **R7's setsid case is a wrong answer, not a missing one** (found in the plan review, 2026-09-02). The draft grouped a browser that calls `setsid` with an unreadable `/proc` as things that answer "cannot say". They do not behave alike: an unreadable table is an error and `known` is false; a browser that left its group leaves an EMPTY group behind, which is `known` true, `gone` true — the reap releases the slot with the browser on screen, and kills nothing because the group it would signal has no members. The plan says so in every place it names the case, and R7 was reconciled to match.

6. **The `killProcessTree` arm goes through `pgroupJob`, not straight to the hook** (plan review, 2026-09-02). Read literally, R4's "the seam's non-Windows arm kills `-pgid`" discards a protection today's `proc.Kill()` has — Go refuses to signal a process it has already waited on — on the one path that reaches the seam with a reaped pid: `killSetupProcess` when no job could vouch for the browser. Routing the arm through `adopt` and `killGroup` keeps every refusal in one place and makes decision 7's "one window" true. R4 and R7 were reconciled.

7. **Zombies are not members** (plan review, 2026-09-02). Decision 8; the draft did not consider a PID-1 Moombox that never reaps an orphaned grandchild, nor the smoke test's own window between the SIGKILL and the `Wait`.

One further judgement the draft leaves open, recorded rather than hidden: a pgid is recyclable where a Job Object handle is not, so a group kill can in principle name a stranger's group. `killGroup` refuses to signal a group it cannot see or that has no members, `killProcessTreeUnix` goes through the same refusals, and that is the most that can be done; `operations.md` carries it as a named residual (decision 7).
