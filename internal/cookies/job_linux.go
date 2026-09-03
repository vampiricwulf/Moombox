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
// child. A CRASH SIGNALS NOTHING ELSE: the group is counted and killed only by
// a running Moombox (cleanupLocked, killProcessTreeUnix, closeLaunchJob), so
// where a wrapper handed off, what it handed off to outlives a Moombox crash —
// the one thing a Windows Job Object's KILL_ON_JOB_CLOSE covers that this does
// not. Named in docs/spec/operations.md as a residual; unmeasured, like the
// handoff itself.
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
