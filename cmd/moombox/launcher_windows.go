package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// createNoWindow prevents a console window from appearing when spawning
// detached processes on Windows (passed to SysProcAttr.CreationFlags).
const createNoWindow = 0x08000000

// configureLauncherChild is a no-op on Windows: the supervised child shares
// the launcher's console (required for the TUI), and parent-death cleanup
// is handled by the Job Object instead of SysProcAttr.
func configureLauncherChild(cmd *exec.Cmd) {}

// Job Object plumbing: mirrors internal/cookies/job_windows.go (which owns
// the browser processes the same way). The launcher assigns each supervised
// child to a kill-on-close job so hard-killing the launcher PID — which
// releases the single-instance mutex — takes the whole recorder tree
// (moombox child + its ffmpeg children) down with it instead of leaving an
// orphaned, unlocked instance recording. NOTE: only the moombox child is
// assigned explicitly; the deferred ~-file deleter is spawned by the
// LAUNCHER (outside the job) so job teardown can't cancel it.
var (
	procCreateJobObjectW      = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObj  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj = kernel32.NewProc("AssignProcessToJobObject")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
)

type jobobjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobobjectExtendedLimitInformationT struct {
	BasicLimitInformation jobobjectBasicLimitInformation
	IoInfo                jobIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// newLauncherJob creates the kill-on-close Job Object for supervised
// children. Best-effort: returns 0 on failure (logged to stderr once) and
// supervision continues without the tie — degraded, exactly today's
// behavior. The handle is deliberately never closed: it must outlive the
// launcher so job teardown fires on launcher DEATH, and process exit
// releases it anyway.
func newLauncherJob() uintptr {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		fmt.Fprintf(os.Stderr, "warning: CreateJobObject failed (%v) — child won't be tied to launcher lifetime\n", err)
		return 0
	}
	info := jobobjectExtendedLimitInformationT{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, err := procSetInformationJobObj.Call(
		h,
		uintptr(jobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	if r == 0 {
		fmt.Fprintf(os.Stderr, "warning: SetInformationJobObject failed (%v) — child won't be tied to launcher lifetime\n", err)
		procCloseHandle.Call(h)
		return 0
	}
	return h
}

// assignLauncherJob adds a freshly-spawned child to the job. Best-effort.
func assignLauncherJob(job uintptr, p *os.Process) {
	if job == 0 || p == nil {
		return
	}
	const processSetQuota = 0x0100
	const processTerminate = 0x0001
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(p.Pid))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: OpenProcess for job assignment failed: %v\n", err)
		return
	}
	defer syscall.CloseHandle(h)
	if r, _, callErr := procAssignProcessToJobObj.Call(job, uintptr(h)); r == 0 {
		fmt.Fprintf(os.Stderr, "warning: AssignProcessToJobObject failed: %v\n", callErr)
	}
}

// launcherWarnf is where this file's update-restart warnings go: stderr, in the
// same `warning: ...` shape as the Job Object failures above. A package
// variable so a test can read what was warned without capturing the process's
// real stderr — the launcher has no logger, it runs before and outside the
// child's logging stack.
var launcherWarnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

// cleanupOrphans removes any stale `~` files left over from a prior
// session. Runs once at launcher startup, before the supervised child
// is spawned. The .exe~ may exist if a prior launcher exited before
// the deferred ping/del fired (system shutdown during deferDeleteOldLauncher's ~4s wait,
// antivirus blocked the cmd, etc.). Now-unlocked, removable.
//
// EXCEPT while a failed-update marker is present: then the ~ file is the
// deliberately-preserved rollback binary the marker's recovery
// instructions point at — a user who simply relaunches after a failed
// update must not have the launcher destroy their way back.
func cleanupOrphans(exePath string) {
	for _, marker := range []string{exePath + ".update-failed", exePath + ".update-broken"} {
		if _, err := os.Stat(marker); err == nil {
			return
		}
	}
	os.Remove(exePath + "~")
}

// handleUpdateRestart runs after the child exits with exitCodeRestart.
// On Windows the launcher cannot delete its own running binary, so we
// rename the just-superseded .old to ~ to free the .old name for the
// next update. The ~ file is then deferred-cleaned on launcher exit.
//
// Returns whether this restart followed a binary update (.old existed) —
// config-change restarts never create .old, so this is the launcher's
// only signal that the NEXT child is the first boot of a fresh update.
func handleUpdateRestart(exePath string) bool {
	oldPath := exePath + ".old"
	if _, statErr := os.Stat(oldPath); statErr != nil {
		return false
	}
	if err := os.Rename(oldPath, exePath+"~"); err != nil {
		// REPORTED, not discarded. A stale ~ file on its own does NOT fail
		// this: Go's os.Rename on Windows is MoveFileEx with
		// MOVEFILE_REPLACE_EXISTING, so an ordinary ~ file is silently
		// replaced. The rename fails only when the ~ name is held by something
		// Windows will not let it replace — on the second update of one
		// launcher lifetime that file is this launcher's own mapped image,
		// which denies delete-sharing, and which the child's CleanupOldBinary
		// could not delete for the same reason. The .old that then stays behind
		// is the version that was running a moment ago, and
		// rollbackArtifactPath prefers it for exactly that reason. This line is
		// how the operator learns the name shuffle did not happen.
		launcherWarnf("warning: could not rename %s to %s (%v) — the previous binary stays at .old and remains the rollback target\n",
			oldPath, exePath+"~", err)
	}
	// True either way: a .old existed, so this restart follows a BINARY update
	// and the launcher's one-shot post-update failure window must arm
	// (launcher.go firstAfterUpdate). A failed rename changes which file is the
	// artifact, never whether there was an update.
	return true
}

// rollbackArtifactPath is where the previous version's binary survives after an
// update on this platform, in preference order:
//
//   - `.old`, when handleUpdateRestart could NOT rename it away (the ~ name was
//     still held by this launcher's mapped image). It is then the version that
//     was running a moment ago; the ~ file is the one BEFORE it, so restoring ~
//     would roll back two versions and the restored child's CleanupOldBinary
//     would delete the real previous binary on its way up.
//   - `~` otherwise — the successful-rename case, i.e. every first update of a
//     launcher lifetime and therefore the ordinary path. Also what a caller
//     gets when neither file exists, so preserveUpdateRollback's written
//     instructions keep naming the ~ file exactly as they always have.
//
// Referenced in recovery instructions when the first post-update boot fails.
func rollbackArtifactPath(exePath string) string {
	oldPath := exePath + ".old"
	if _, err := os.Stat(oldPath); err == nil {
		return oldPath
	}
	return exePath + "~"
}

// keepAsideByLink always declines on Windows, where attemptAutoRollback moves
// the broken binary aside instead: renaming the previous one over it needs
// delete access to it, which a scanner still holding the fresh download
// denies, and only the move aside can be retried around that.
func keepAsideByLink(exePath, failedPath string) bool { return false }

// deferDeleteOldLauncher schedules deletion of the .exe~ file via a
// detached cmd /c invocation that uses ping as a sleep mechanism,
// then runs del. The ~4s wait covers normal launcher exit-and-
// handle-release time (os.Exit releases handles within milliseconds;
// 4s is generous headroom). The launcher's startup cleanupOrphans
// is the safety net if this somehow doesn't fire.
//
// The command line is fixed text (deferDeleteCmdLine) and the path reaches
// del through an environment variable — see deferDeleteCommand.
//
// History: tried timeout.exe earlier — it errors out unconditionally
// when stdin is redirected (per Microsoft docs), which it always is
// for a Go-spawned cmd subprocess. The `& del` then ran immediately,
// before the launcher had released the file lock. ping has no stdin
// dependency: 5 pings × 1s default interval = 4s of wall-clock delay.
//
// Earlier history: also tried schtasks (task name containing ~ was
// rejected, /st time wraparound, /tr quoting fragility, no exit-code
// check). ping is the simplest reliable option.
func deferDeleteOldLauncher(exePath string) {
	// While a failed-update marker exists, the ~ file is the PRESERVED
	// rollback binary the marker's instructions point at — every launcher
	// exit path (fail-fast, crash-loop give-up, Start failure on a
	// relaunch) must leave it alone, not just startup's cleanupOrphans.
	// Without this, relaunching after a failed update deleted the backup
	// on the way out while the marker still said "restore from it".
	for _, marker := range []string{exePath + ".update-failed", exePath + ".update-broken"} {
		if _, err := os.Stat(marker); err == nil {
			return
		}
	}
	oldPath := exePath + "~"
	if _, err := os.Stat(oldPath); err != nil {
		return // no .exe~ file to clean up
	}
	cleanup := deferDeleteCommand(oldPath)
	cleanup.Start() // fire-and-forget; we exit shortly anyway
}

// oldLauncherEnv names the environment variable that carries the old
// launcher's path to the deferred del.
const oldLauncherEnv = "MOOMBOX_OLD_LAUNCHER"

// deferDeleteCmdLine is the whole command line the deferred cleanup runs.
// The path is never part of it: cmd expands the variable inside the quotes,
// does not expand the value a second time, and reads every character of it
// literally there — a Windows path cannot contain '"'.
const deferDeleteCmdLine = `cmd /C ping 127.0.0.1 -n 5 >nul & del /f /q "%` + oldLauncherEnv + `%" >nul 2>nul`

// deferDeleteCommand builds the deferred cleanup for oldPath.
//
// The path used to be one of cmd's arguments, and Go quotes an argument only
// when it contains a space, a tab or a quote. An install directory like
// D:\Tools&Apps reached cmd bare, cmd split the line at its '&', and
// `del /f /q D:\Tools` — del on a directory deletes every file in it, /q
// suppresses the prompt and >nul 2>nul the output — emptied the parent
// directory without a word. Quoting the path in the command line would still
// leave a '%NAME%' inside it to cmd's expansion; the variable leaves nothing
// of the path for cmd to parse. CmdLine is set because Go escapes an inner
// '"' as \", which cmd does not read as a quote; Windows ignores Args once
// CmdLine is set.
func deferDeleteCommand(oldPath string) *exec.Cmd {
	cleanup := exec.Command("cmd", "/C",
		"ping", "127.0.0.1", "-n", "5", ">nul", "&",
		"del", "/f", "/q", "%"+oldLauncherEnv+"%", ">nul", "2>nul")
	cleanup.Env = append(os.Environ(), oldLauncherEnv+"="+oldPath)
	cleanup.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow,
		CmdLine:       deferDeleteCmdLine,
	}
	return cleanup
}
