package cookies

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

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

// close terminates all processes in the job and releases the handle.
func (j *processJob) close() {
	if j != nil && j.handle != 0 {
		syscall.CloseHandle(j.handle)
		j.handle = 0
	}
}
