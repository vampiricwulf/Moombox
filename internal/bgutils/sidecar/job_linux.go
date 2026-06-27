//go:build linux

package sidecar

import (
	"os"
	"os/exec"
	"syscall"
)

// processJob has no Job Object equivalent on Linux; parent-death cleanup
// is handled by PR_SET_PDEATHSIG configured pre-start (see
// configureCmdSysProcAttr). The struct stays a no-op so sidecar.go's
// lifecycle code is platform-agnostic.
type processJob struct{}

func newProcessJob() (*processJob, error)      { return &processJob{}, nil }
func (j *processJob) assign(*os.Process) error { return nil }
func (j *processJob) close()                   {}

// configureCmdSysProcAttr arranges for the kernel to SIGKILL the sidecar
// when Moombox dies — the Linux counterpart of the Windows Job Object's
// KILL_ON_JOB_CLOSE, covering the crash path where Stop() never runs.
// Without it, every Moombox crash leaks an orphaned Node process.
//
// Caveat: Pdeathsig fires when the spawning THREAD exits, and Go can in
// principle retire that thread while the process lives — which would kill
// the sidecar spuriously. If that ever happens, readPump sees stdout EOF,
// marks the sidecar unhealthy, and callers fall back to the goja path: a
// degraded-but-safe outcome, strictly better than orphan accumulation.
func configureCmdSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
