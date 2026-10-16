//go:build windows

package childproc

import (
	"syscall"
	"testing"
	"unsafe"
)

var (
	procIsProcessInJob            = kernel32.NewProc("IsProcessInJob")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
)

// TestAStartedProgramIsInThisProcesssKillOnCloseJob is the Windows half of
// W28-A01: a program Start launched is a member of the job this process owns,
// and that job kills what it holds when its handle closes — which is what
// happens to it when this process exits, however it exits. Around the Moombox
// child there is always the launcher's job, and around a test binary there
// may be a CI runner's; the assignment is the same either way (a process in a
// job is assigned to a second one by nesting it, Windows 8 and later).
//
// The kill itself is not exercised: the handle is this test process's, and
// closing it here would leave every later Start untied.
//
// Mutants: adopt returning nil before AssignProcessToJobObject — the program
// is not in the job; jobHandle not setting JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
// — the job's limit flags miss it.
func TestAStartedProgramIsInThisProcesssKillOnCloseJob(t *testing.T) {
	c := standIn(t.Context(), "sleep")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_ = c.Wait()
	})

	job, err := jobHandle()
	if err != nil {
		t.Fatalf("this process has no job to tie programs to: %v", err)
	}

	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(c.Process.Pid))
	if err != nil {
		t.Fatalf("OpenProcess: %v", err)
	}
	defer syscall.CloseHandle(h)
	var in int32
	if r, _, callErr := procIsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&in))); r == 0 {
		t.Fatalf("IsProcessInJob: %v", callErr)
	}
	if in == 0 {
		t.Error("the program Start launched is not in this process's job: a crash of this process leaves it running")
	}

	var info jobobjectExtendedLimitInformationT
	var n uint32
	if r, _, callErr := procQueryInformationJobObject.Call(
		uintptr(job),
		uintptr(jobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
		uintptr(unsafe.Pointer(&n)),
	); r == 0 {
		t.Fatalf("QueryInformationJobObject: %v", callErr)
	}
	if info.BasicLimitInformation.LimitFlags&jobObjectLimitKillOnJobClose == 0 {
		t.Errorf("the job's limit flags are %#x, without JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: closing it kills nothing",
			info.BasicLimitInformation.LimitFlags)
	}
}
