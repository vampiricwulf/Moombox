//go:build windows

package sidecar

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// Job Object pinning for the sidecar subprocess. Mirror of the pattern in
// internal/cookies/job_windows.go: an anonymous Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so the assigned child process (and
// any children IT spawns) die when the job handle is closed -- which
// happens implicitly when Moombox exits, ensuring no zombie node.exe
// processes outlive a Moombox crash.

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

type processJob struct {
	handle syscall.Handle
}

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

func (j *processJob) assign(p *os.Process) error {
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

func (j *processJob) close() {
	if j != nil && j.handle != 0 {
		syscall.CloseHandle(j.handle)
		j.handle = 0
	}
}

// configureCmdSysProcAttr is a no-op on Windows — parent-death cleanup is
// handled by the Job Object (KILL_ON_JOB_CLOSE) assigned after start.
func configureCmdSysProcAttr(*exec.Cmd) {}
