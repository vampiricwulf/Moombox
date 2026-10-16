//go:build windows

package childproc

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// The Job Object plumbing mirrors internal/bgutils/sidecar/job_windows.go and
// cmd/moombox/launcher_windows.go: an anonymous job whose limit is
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. What differs is the owner. The
// launcher's job is the LAUNCHER's — it kills the child and everything under
// it when the launcher dies, and nothing when only the child does, which is
// the crash the launcher respawns from. This one is the child's: its handle is
// held by this process, never closed, and never inherited (CreateJobObject
// with no security attributes makes it non-inheritable, and os/exec passes a
// child only the handles it names), so it closes when this process exits and
// nothing that outlives this process can hold it open.

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
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

// tieLocksThread: a job is a handle, not a thread's; nothing to hold.
const tieLocksThread = false

// processJob is this process's kill-on-close job, created on first use. A
// failure to create it is kept and answered to every adopt: whatever refused
// it once refuses it again.
var processJob struct {
	once   sync.Once
	handle syscall.Handle
	err    error
}

func jobHandle() (syscall.Handle, error) {
	processJob.once.Do(func() {
		h, _, err := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			processJob.err = fmt.Errorf("CreateJobObject: %w", err)
			return
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
			syscall.CloseHandle(syscall.Handle(h))
			processJob.err = fmt.Errorf("SetInformationJobObject: %w", err)
			return
		}
		processJob.handle = syscall.Handle(h)
	})
	return processJob.handle, processJob.err
}

// prepare has nothing to do on Windows: the tie is made after the start.
func prepare(*exec.Cmd) {}

// adopt assigns a started program to this process's job. The window between
// the start and the assignment is the one the sidecar and the browsers accept
// as well: a crash inside it leaves that one program in the launcher's job
// only.
func adopt(p *os.Process) error {
	if p == nil {
		return nil
	}
	job, err := jobHandle()
	if err != nil {
		return err
	}
	const processSetQuota = 0x0100
	const processTerminate = 0x0001
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(h)
	// r, not callErr: LazyProc.Call always returns a non-nil Errno.
	if r, _, callErr := procAssignProcessToJobObj.Call(uintptr(job), uintptr(h)); r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", callErr)
	}
	return nil
}
