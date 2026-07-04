//go:build linux

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// configureLauncherChild ties the supervised child's lifetime to the
// launcher on Linux via PR_SET_PDEATHSIG: if the launcher is hard-killed
// (kill -9 on the PID the operator sees — which releases the
// single-instance flock), the kernel delivers SIGTERM to the child so it
// shuts down gracefully instead of continuing to record as an orphaned,
// unlocked instance. SIGTERM (not the sidecar's SIGKILL) deliberately:
// the child's shutdown() drains workers and preserves staging state.
func configureLauncherChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}

// newLauncherJob is Windows-only (Job Object); Linux uses Pdeathsig above.
func newLauncherJob() uintptr { return 0 }

// assignLauncherJob is Windows-only; no-op on Linux.
func assignLauncherJob(job uintptr, p *os.Process) {}
