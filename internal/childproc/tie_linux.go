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
// FFmpeg answers SIGTERM by writing its trailer first.
//
// It also puts the program in a process group of its own (Setpgid). In
// Moombox's group, which a terminal signals as a whole, it died of what was
// meant for Moombox: closing the terminal or a dropped SSH session sent SIGHUP
// to the launcher, the child and FFmpeg alike, and Ctrl+C at a --headless run
// SIGINT. Moombox catches both and shuts down in order — the worker cancels
// its muxes itself, and a cancelled mux leaves its row Muxing for the next
// start — but FFmpeg died first, with the default disposition, while its mux
// was still live: MuxJob read "ffmpeg: signal: hangup" as a failure and wrote
// Error, which the next start's re-mux skips, and a trim was reported failed
// instead of stopped. Out of that group nothing else ends the program when
// Moombox dies, which is what the parent-death signal is for. Its stdin and
// stdout are never the terminal (exec gives a nil one /dev/null, and the
// callers pipe stderr), so a program outside the terminal's foreground group
// is never stopped for touching it.
//
// Any SysProcAttr the caller set is kept.
func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	cmd.SysProcAttr.Setpgid = true
}

// adopt has nothing to do on Linux: the tie was made before the fork.
func adopt(*os.Process) error { return nil }
