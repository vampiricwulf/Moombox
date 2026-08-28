package cookies

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW      = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObj  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj = kernel32.NewProc("AssignProcessToJobObject")
	procQueryInformationJobOb = kernel32.NewProc("QueryInformationJobObject")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	// jobObjectBasicAccountingInformation is the JOBOBJECTINFOCLASS value
	// that makes QueryInformationJobObject return live process counts.
	jobObjectBasicAccountingInformation = 1
)

// jobobjectExtendedLimitInformation matches the Windows
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION struct layout (64-bit).
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

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobobjectExtendedLimitInformationT struct {
	BasicLimitInformation jobobjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// processJob wraps a Windows Job Object handle. All processes assigned to the
// job are killed when the handle is closed, thanks to JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
type processJob struct {
	handle syscall.Handle
}

// newProcessJob creates a Windows Job Object configured to kill all assigned
// processes when the job handle is closed.
//
// The job is anonymous (lpName=NULL): named jobs are only useful for sharing
// across processes, and CreateJobObject returning a handle to an existing
// job-of-the-same-name would silently coalesce unrelated launches into one
// shared job. The previous per-process counter prevented that collision but
// served no other purpose, so dropping naming entirely is the simpler fix.
func newProcessJob() (*processJob, error) {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
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
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	return &processJob{handle: syscall.Handle(h)}, nil
}

// assign adds a process to the job object. The process and all its future
// children will be terminated when the job is closed.
func (j *processJob) assign(p *os.Process) error {
	// Open a handle to the process with the access rights required by
	// AssignProcessToJobObject (PROCESS_SET_QUOTA | PROCESS_TERMINATE).
	const processSetQuota = 0x0100
	const processTerminate = 0x0001
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(h)

	r, _, callErr := procAssignProcessToJobObj.Call(uintptr(j.handle), uintptr(h))
	if r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", callErr)
	}
	return nil
}

// jobobjectBasicAccountingInformation matches the Windows
// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION struct layout — 4x LARGE_INTEGER
// then 4x DWORD. No padding on amd64: 32 bytes of int64 followed by 16 bytes
// of uint32.
type jobobjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// activeProcesses reports how many processes are still alive in the job.
//
// This is what makes the Firefox-family refresh work at all. Firefox uses a
// launcher-process model: the exe we start hands off to the real browser and
// exits in ~170ms, so cmd.Wait() returning tells us nothing about whether the
// page loaded. Closing the job at that moment kills the browser mid-load —
// measured, and the reason every refresh silently did nothing.
//
// A nil job (newProcessJob failed; runWithTimeout carries on without one) or
// an already-closed handle reports zero, which reads as "nothing left to wait
// for" and degrades the caller to the pre-drain behaviour rather than erroring.
func (j *processJob) activeProcesses() (int, error) {
	if j == nil || j.handle == 0 {
		return 0, nil
	}
	var info jobobjectBasicAccountingInformation
	var retLen uint32
	// Check r == 0, NOT callErr != nil. syscall.LazyProc.Call always returns
	// a non-nil syscall.Errno, and Errno(0) formats as "The operation
	// completed successfully." — so an err-based check reports every success
	// as a failure with a cheerful message. The other calls in this file get
	// this right too (newProcessJob, assign); do not "fix" it.
	r, _, callErr := procQueryInformationJobOb.Call(
		uintptr(j.handle),
		uintptr(jobObjectBasicAccountingInformation),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
		uintptr(unsafe.Pointer(&retLen)),
	)
	if r == 0 {
		return 0, fmt.Errorf("QueryInformationJobObject: %w", callErr)
	}
	return int(info.ActiveProcesses), nil
}

// queryable reports whether activeProcesses can return a MEANINGFUL count for
// this job, as opposed to the zero it returns when there is nothing to ask.
//
// The distinction exists because a caller that acts on "the job is empty" must
// not be handed the same answer for "there is no job". activeProcesses returns
// (0, nil) for a nil receiver AND for an already-closed handle, both of which
// are absence of information; only a live handle on a platform with real Job
// Objects can say anything. See setupBrowserGone, whose whole contract is that
// distinction, and drainJob, which draws the same line in prose.
func (j *processJob) queryable() bool { return j != nil && j.handle != 0 }

// close terminates all processes in the job and releases the handle.
func (j *processJob) close() {
	if j != nil && j.handle != 0 {
		syscall.CloseHandle(j.handle)
		j.handle = 0
	}
}

// configureCmdSysProcAttr is a no-op on Windows — parent-death cleanup is
// handled by the Job Object (KILL_ON_JOB_CLOSE) assigned after start.
func configureCmdSysProcAttr(*exec.Cmd) {}
