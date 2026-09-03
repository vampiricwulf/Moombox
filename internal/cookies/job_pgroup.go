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
//
// A second adopt on the same job REPLACES the tracked group; last adopt wins,
// with no merge and no refusal. Production never calls it twice on one job —
// killProcessTreeUnix builds a fresh *pgroupJob per call, and
// processJob.assign adopts once — so nothing here relies on the replacement,
// but the type itself has no guard against it.
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
