//go:build linux

package cookies

import (
	"os"
	"os/exec"
	"syscall"
)

// processJob has no Job Object equivalent on Linux; parent-death cleanup is
// handled by PR_SET_PDEATHSIG configured pre-start (see
// configureCmdSysProcAttr). Returns a non-nil no-op (same shape as the sidecar
// package's stub) so the return contract is uniform across packages; the
// no-op assign/close make it harmless, and the `job != nil` guards at call
// sites remain for the Windows assign-failure path.
type processJob struct{}

func newProcessJob() (*processJob, error)        { return &processJob{}, nil }
func (j *processJob) assign(p *os.Process) error { return nil }
func (j *processJob) close()                     {}

// activeProcesses always reports zero because there is no job to count.
// runWithTimeout's drain loop treats that as "nothing left to wait for" and
// exits immediately, so this platform keeps exactly today's behaviour — the
// Firefox launcher handoff is unfixed here, but nothing is killed either
// (pdeathsig fires on Moombox's death, not the launcher's), so the browser
// runs on detached.
func (j *processJob) activeProcesses() (int, error) { return 0, nil }

// configureCmdSysProcAttr arranges for the kernel to SIGKILL the launched
// browser when Moombox dies — the Linux counterpart of the Windows Job
// Object's KILL_ON_JOB_CLOSE. Without it, a crashed Moombox orphans a
// headless refresh browser that keeps holding the profile lock, silently
// breaking every subsequent refresh (the exact failure the Job Object
// comment in refreshChromium describes). Best-effort: unlike a Job Object,
// pdeathsig does not propagate to the browser's own forked children.
func configureCmdSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
