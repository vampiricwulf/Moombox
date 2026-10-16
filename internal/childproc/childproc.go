// Package childproc starts the programs Moombox runs for a job — FFmpeg and
// ffprobe — so that they die with the process that started them.
//
// The launcher's crash supervision respawns a child that died abnormally (an
// OOM kill, a fatal runtime error, kill -9 of the child alone), and the
// respawned child's boot re-mux of a Muxing row runs FFmpeg into the same
// archive (enqueueExistingJobs → muxOnRestart → MuxJob, internal/worker). An
// FFmpeg the dead child had started used to keep running beside it: nothing
// tied it to the child — on Linux it had no parent-death signal, and on
// Windows the kill-on-close Job Object it lived in was the LAUNCHER's, which
// stays alive. Two writers on one file left a corrupt archive the row then
// called Finished, and the staging cleanup deleted the only good copy (W28-A01).
//
// Every FFmpeg and ffprobe the child starts goes through CommandContext here,
// and only here; TestNoFFmpegCommandOutsideChildproc holds the rest of the
// tree to it. The tie is per platform:
//
//   - Linux: PR_SET_PDEATHSIG (SIGKILL). The kernel sends it when the THREAD
//     that forked the program exits, not the process, and Go can retire an OS
//     thread under a running process: a goroutine that exits while locked to
//     one takes the thread with it, and any goroutine may be scheduled onto
//     the thread the spawning goroutine left while it waits on a pipe. Start
//     therefore locks the calling goroutine to its OS thread, and Wait
//     unlocks it once the program has exited, so the thread that is the
//     program's parent lives exactly as long as the program needs it to.
//   - Windows: a kill-on-close Job Object this process owns (one for the
//     process, created on first use and never closed), each program assigned
//     to it as it starts. The handle closes when this process exits, however
//     it exits, and the system then terminates every program in the job. The
//     launcher's own job is still there around it: a process in a job can be
//     assigned to a second one since Windows 8, which nests the new job under
//     the old, so the launcher dying still takes everything with it too.
//   - Anywhere else: nothing; the programs run as exec.Cmd runs them.
//
// The tie is best effort, like the sidecar's and the browsers': a program the
// platform refuses to tie still runs, and the refusal is logged once.
//
// It covers the program Moombox started, not what that program starts in
// turn. An ffmpeg_path that is a wrapper script which execs FFmpeg keeps the
// tie (a parent-death signal survives execve, and on Windows a job is
// inherited); one that forks FFmpeg and waits leaves, on Linux, the FFmpeg
// itself untied.
package childproc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"runtime"
	"sync"
)

// Cmd is an exec.Cmd whose program dies with this process. Configure it like
// the exec.Cmd it embeds (Stdout, Stderr, StderrPipe, Env, Dir); start it with
// the methods below, never with the embedded Cmd's own, which would start it
// untied.
//
// Start and Wait belong to ONE goroutine: on Linux Start locks the calling
// goroutine to its OS thread for as long as the program runs, and only that
// goroutine can unlock it. Run, Output and CombinedOutput keep the pair
// together; a caller that streams a pipe between Start and Wait (FFmpeg's
// progress reader) does it on the goroutine that called Start.
type Cmd struct {
	*exec.Cmd
	locked bool
}

// CommandContext is exec.CommandContext for a program that must die with this
// process: FFmpeg, ffprobe, and the checks that run them.
func CommandContext(ctx context.Context, name string, arg ...string) *Cmd {
	return &Cmd{Cmd: exec.CommandContext(ctx, name, arg...)}
}

// Start starts the program tied to this process (see the package comment).
// A failed start leaves nothing locked. Wait must follow, on this goroutine.
func (c *Cmd) Start() error {
	if tieLocksThread {
		runtime.LockOSThread()
		c.locked = true
	}
	prepare(c.Cmd)
	if err := c.Cmd.Start(); err != nil {
		c.unlock()
		return err
	}
	if err := adopt(c.Cmd.Process); err != nil {
		warnUntied(err)
	}
	return nil
}

// Wait waits for the program to exit, as exec.Cmd.Wait does, and only then
// lets go of the OS thread Start locked.
func (c *Cmd) Wait() error {
	defer c.unlock()
	return c.Cmd.Wait()
}

// Run starts the program and waits for it.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// Output runs the program and returns its standard output, as exec.Cmd.Output
// does: with Stderr unset, a failed run's *exec.ExitError carries the tail of
// what the program wrote to it.
func (c *Cmd) Output() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var stdout bytes.Buffer
	c.Stdout = &stdout
	var stderr *tailBuffer
	if c.Stderr == nil {
		stderr = &tailBuffer{max: outputStderrTail}
		c.Stderr = stderr
	}
	err := c.Run()
	if stderr != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			ee.Stderr = stderr.Bytes()
		}
	}
	return stdout.Bytes(), err
}

// CombinedOutput runs the program and returns its standard output and
// standard error together, as exec.Cmd.CombinedOutput does.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	if c.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	var b bytes.Buffer
	c.Stdout = &b
	c.Stderr = &b
	err := c.Run()
	return b.Bytes(), err
}

func (c *Cmd) unlock() {
	if c.locked {
		c.locked = false
		runtime.UnlockOSThread()
	}
}

// outputStderrTail is how much of a failed program's stderr Output keeps for
// its ExitError — exec.Cmd.Output keeps 32 KiB, as the head and tail of it.
const outputStderrTail = 32 << 10

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte { return t.buf }

// untiedOnce makes the refusal to tie a warning once per process: whatever
// stops this platform tying one program stops it tying the next, and a probe
// runs for every mux.
var untiedOnce sync.Once

// warnUntied logs that a program could not be tied to this process. Through
// slog.Default, which the logger bridges into the full pipeline: this package
// carries no logger, and every one of its callers would log the same line.
func warnUntied(err error) {
	untiedOnce.Do(func() {
		slog.Warn("FFmpeg and ffprobe could not be tied to this process; one Moombox starts may outlive a crash of it",
			"err", err)
	})
}
