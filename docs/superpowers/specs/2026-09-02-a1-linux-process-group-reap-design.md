# A1 on Linux/Docker — the process-group reap: design

**Status:** owner-ruled 2026-08-29 (Q9 REVISED: "build it for this release, but we don't need to test it
ourselves with a linux box, a user can submit a bug report if it's broken"); the ruling's exact text is
`docs/superpowers/plans/2026-08-25-cookie-subsystem-remediation.md:2015`. Drafted 2026-09-02 from the Explore
file map (`a1-linux-file-map.md`, gitignored beside the remediation ledger); committed on branch
`cookie-a1-linux-reap` after the Arc 11 merge. Reconciled to the plan by its review on 2026-09-02 (R2, R4, R6,
R7, §5 — `a1-plan-review.md` beside the file map).
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
`assign(p)` records `pgid = p.Pid` — after confirming from the table that `p.Pid` currently LEADS its
own group, and refusing otherwise (a child that inherited Moombox's group would later point the kill
at Moombox, in Docker at the container; a refusal degrades to no group, no answer, no kill, and a
logged reason); `queryable()` is `j != nil && j.pgid > 0`; `activeProcesses()` counts processes whose
process-group id equals `j.pgid`, read from the process table, a zombie (state `Z`) not among them.
The Windows contract (`queryable` → the reap may judge; zero live → gone) is unchanged in meaning.

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
non-Windows arm kills the group, THROUGH the same adopt-then-kill refusals as everything else: it
signals only a group the pid currently leads and that has members, and otherwise falls back to today's
`proc.Kill()`, which Go refuses on a process it has already waited on — a protection a bare
`kill(-pid)` would discard on a path `killSetupProcess` reaches with a reaped pid), `cleanupLocked` (`:3229` — the comment "ON WINDOWS, CLOSING THE JOB
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
zero ("nothing was waited on"), and `errBrowserDrainTimeout` can reach `browserLaunchActed` there.
Stated precisely: Mozilla's launcher is a Windows feature, so on Linux the direct child is normally
the browser itself and `cmd.Wait()` already waited for it; what the count adds is a wait for whatever
OUTLIVES the direct child (a wrapper's handoff, a straggling content process), and where nothing does
the drain still lands on lap zero. This is Arc 0's fix arriving on Linux — desirable, and a timing
change the docs state (`drainJob`'s comment; `docs/spec/operations.md:130` and `:134`).

**R7 — What stays unmeasured is said plainly.** A `/proc` that is not readable (a hardened container)
makes `listProcessGroups` fail, and a failed read answers "cannot say" (`known=false`), never "gone".
A browser that calls `setsid` LEAVES the group, and that is a wrong answer rather than a missing one:
the count reads the group as empty, the reap releases the slot when the grace runs out with the
browser still on screen (no kill — the group it would signal has no members), and the next finish
answers `ErrNoSetupInProgress`; which packagings do it (snap/flatpak wrappers are the suspects) is
unmeasured, and the docs say so. A zombie is not a member: the parser skips state `Z`, because a
PID-1 Moombox without an init (compose sets `init: true`; a bare `docker run` does not) never reaps an
orphaned grandchild. No Linux live gate: the field-test plan row for A1 on Linux changes from "Not
fixed" to "built, not field-verified — a field report is the gate". A pgid is recyclable where a Job
Object handle is not, so a group kill could in principle name a stranger's group; `killGroup` refuses
a group it cannot see or that has no members, the `killProcessTree` arm goes through the same
refusals so it is one window, and `operations.md` carries the rest as a named residual.

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

`docs/spec/operations.md:122-130` and `:134` (the intro's "a primitive only Windows has"; the drain paragraph; the two
"Off Windows nothing can say" / "unconditionally false" sentences; `AbandonSetup`'s rule gains its Linux clause; the
UNMEASURED paragraph's "that platform drains nothing either way"); `SPEC.md:680`; the remediation plan's `:1876`,
`:1880`, `:1980` and `:1996` ("A1 is NOT fixed on Linux/Docker", "not reaped on Linux or in Docker"); the field-test
plan's `:172` and `:302` rows. The code comments that make the same claims (`AbandonSetup`'s and `killSetupProcess`'s
docs, `refreshLooksImplausiblyFast`'s, `drainJob`'s three `job.close()` lines) move in the plan's Tasks 3 and 5.
