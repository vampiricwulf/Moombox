//go:build linux

package childproc

import (
	"os"
	"os/exec"
	"syscall"
)

// tieLocksThread: the parent-death signal is the forking THREAD's (see the
// package comment), so Start holds that thread for the program's life.
const tieLocksThread = true

// prepare asks the kernel to SIGKILL the program when its parent thread
// exits — which, with Start's lock, is when this process does. SIGKILL and not
// SIGTERM: what is being prevented is a second writer on an archive, and
// FFmpeg answers SIGTERM by writing its trailer first. Any SysProcAttr the
// caller set is kept.
func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// adopt has nothing to do on Linux: the tie was made before the fork.
func adopt(*os.Process) error { return nil }
