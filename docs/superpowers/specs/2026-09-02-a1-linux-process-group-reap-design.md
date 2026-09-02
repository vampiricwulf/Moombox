# A1 on Linux/Docker — the process-group reap: design

**Status:** owner-ruled 2026-08-29 (Q9 REVISED: "build it for this release, but we don't need to test it
ourselves with a linux box, a user can submit a bug report if it's broken"); the ruling's exact text is
`a1-linux-file-map.md`; to be committed to `docs/superpowers/specs/` after the Arc 11 merge.
**Scope:** `internal/cookies` (`job_linux.go`, one new tag-free file, the seams in `autocookies.go`),
the spec docs that say the reap is Windows-only. No UI. No config. The liveness pilot stays disarmed.

## 1. The problem, as the code stands

On Windows the abandoned-setup reap (A1) keys on a Job Object: `processJob.queryable()` says a
judgement is possible, `activeProcesses()` says whether the browser is still alive, and closing the
handle kills what is left (`internal/cookies/job_windows.go`; `autocookies.go` `reapAbandonedSetupLocked`,
`setupBrowserGone`, `AbandonSetup`). Off Windows `queryable()` is unconditionally `false`
(`job_linux.go:36`, `job_other.go:29`), so `realSetupBrowserGone` never answers, the reap never fires,
and an abandoned wizard holds the acquisition slot until a client calls `AbandonSetup`/`CancelSetup`
— which is exactly the wedge A1 fixed on Windows. `Pdeathsig` (`job_linux.go:45`) covers only the
direct child, and the direct child is a launcher that hands off in ~170 ms (Arc 0's diagnosis).

## 2. Required behaviour

**R1 — Every setup and refresh browser on Linux is launched into its own process group.**
`configureCmdSysProcAttr` on Linux sets `Setpgid: true` (keeping `Pdeathsig`), at all four launch
sites unchanged (`startFirefoxSetup`, `startChromiumSetup`, `refreshFirefox`, `refreshChromium` —
they already call it). The group id is the child's pid.

**R2 — `processJob` on Linux is real.** `newProcessJob()` returns a job with no group yet;
`assign(p)` records `pgid = p.Pid`; `queryable()` is `j != nil && j.pgid > 0`; `activeProcesses()`
counts processes whose process-group id equals `j.pgid`, read from the process table. The Windows
contract (`queryable` → the reap may judge; zero live → gone) is unchanged in meaning.

**R3 — The process table is a seam, and the decision logic is tag-free.** The counting and kill
DECISIONS live in a file with no build tag (say `job_pgroup.go`): `pgroupJob{pgid int}` with
`activeProcesses()`/`queryable()`/`killGroup()` written against two function variables —
`listProcessGroups func() (map[int]int, error)` (pid → pgid) and `killProcessGroup func(pgid int)
error`. `job_linux.go` binds them to `/proc/<pid>/stat` (field 5) and `syscall.Kill(-pgid, SIGKILL)`;
tests on Windows bind them to a fake table and a recorder. That is how "unit-tested with a fake process
table, no Linux box" is satisfied: every branch runs in the Windows test suite.

**R4 — Closing a group kills nothing; killing is explicit — and one close must NOT kill.** There is
no `KILL_ON_JOB_CLOSE` equivalent. `close()` on Linux only forgets the pgid. Every site that on Windows
relies on the close to kill must, on Linux, kill the group explicitly: `killSetupProcess`
(`autocookies.go:3122`, via the `killProcessTree` seam — on Linux the tree IS the group, so the seam's
non-Windows arm kills `-pgid`), `cleanupLocked` (`:3229` — the comment "ON WINDOWS, CLOSING THE JOB
OBJECT KILLS BROWSERS" gains its Linux sentence), `adoptSetupJobLocked` (`:3305`, a close that is
deliberately a kill on Windows), and `runWithTimeout`'s TEARDOWN (the drain-timeout and ctx-cancelled
arms reach the deferred `job.close()` with a browser alive; the explicit timeout arm already calls
`killProcessTree`). The one close that must NEVER kill is `trackedSetupJob` (`:3272`): on Windows the
job it drops is empty, on Linux the group is the user's live browser — a test pins it. The plan
enumerates every site in a table (site · Windows today · Linux arm · pinning test); none is left to
`Pdeathsig`.

**R5 — The same rules, verbatim.** The 60 s grace (`setupAbandonGrace`), measured from the last
time the setup was seen alive; `reapAbandonedSetupLocked` unchanged; `AbandonSetup`'s "release only
where releasing cannot kill": with `queryable()` now true on Linux, `AbandonSetup` takes the
leave-it-alone branch (the reap judges), exactly as on Windows — and because a Linux close cannot
kill, the other branch stays safe where it still applies (`job_other.go`, unchanged: `queryable`
false, no-op close).

**R6 — A real count arms the drain on Linux.** With `activeProcesses()` real, `runWithTimeout`'s
`drainJob` genuinely waits for the group to empty where every Linux launch previously returned on lap
zero ("nothing was waited on"), and `browserLaunchActed` reports a real verdict there. This is Arc 0's
fix arriving on Linux — desirable, and a timing change the docs state (`drainJob`'s comment;
`docs/spec/operations.md:134`).

**R7 — What stays unmeasured is said plainly.** A browser that calls `setsid` leaves the group; a
`/proc` that is not readable (a hardened container) makes `listProcessGroups` fail, and a failed read
answers "cannot say" (`known=false`), never "gone". No Linux live gate: the field-test plan row for A1
on Linux changes from "Not fixed" to "built, not field-verified — a field report is the gate". A pgid
is recyclable where a Job Object handle is not, so a group kill could in principle name a stranger's
group; `killGroup` refuses a group it cannot see or that has no members, and `operations.md` carries
the rest as a named residual.

## 3. Non-goals

No change to the Windows path. No change to `job_other.go` beyond keeping its contract. No reaper
goroutine (the inline-under-`s.mu` design stands). No sidecar or launcher process-group work
(`internal/bgutils/sidecar/job_linux.go`, `cmd/moombox/launcher_child_linux.go` keep `Pdeathsig` only).

## 4. Invariants

- A failed process-table read never releases the slot and never kills.
- `killProcessGroup` is never called with `pgid <= 0` (a `kill(-0, …)` would signal the caller's own
  group — a test pins the guard).
- No goroutine is added; every existing recover posture is untouched.
- Tests: mutate the claim for every assertion (the guard, the count, the grace, the branch); the
  Windows-only test files are not the home of any new logic test.

## 5. Docs that change

`docs/spec/operations.md:124-128` and `:134` (the drain paragraph; the two "Off Windows nothing can say" / "unconditionally false"
sentences; `AbandonSetup`'s rule gains its Linux clause); `SPEC.md:680`; the remediation plan's
`:1880` and `:1996` ("A1 is NOT fixed on Linux/Docker"); the field-test plan's `:302` row.
