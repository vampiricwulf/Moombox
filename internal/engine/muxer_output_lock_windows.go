//go:build windows

package engine

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var (
	modkernel32    = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = modkernel32.NewProc("LockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errLockViolation        = syscall.Errno(33) // ERROR_LOCK_VIOLATION

	// muxTempLockOffsetHigh puts the one byte the writer's lock covers at
	// 2^62, past the end of any file FFmpeg writes. A byte-range lock on
	// Windows is mandatory — no other handle may read or write the range it
	// covers — so a lock on the file's own bytes would fail FFmpeg's writes;
	// one past its end blocks nothing but another lock.
	muxTempLockOffsetHigh = 1 << 30
)

// createMuxTempFile creates a temporary output for its writer, refusing a
// name that exists. Opened with every share mode, FILE_SHARE_DELETE among
// them, which os.OpenFile never asks for: FFmpeg opens the file by name
// beside this handle, and the rename onto the final name — or the removal of
// a failed run's file — happens while it is still open, because closing it is
// what releases the lock.
func createMuxTempFile(path string) (*os.File, error) {
	return openSharingDelete(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.CREATE_NEW)
}

// openMuxTempForSweep opens a temporary output the boot sweep found, to try
// its writer's lock. Every share mode here too: a live FFmpeg's handle must
// not refuse the open, and the sweep removes the file through the same open.
func openMuxTempForSweep(path string) (*os.File, error) {
	return openSharingDelete(path, syscall.GENERIC_READ, syscall.OPEN_EXISTING)
}

// openSharingDelete opens path with read, write and delete sharing, through
// ffmpegPathArg's long-path form — the temporary name is the archive's, which
// may pass MAX_PATH. The handle is not inheritable, so no FFmpeg holds it.
func openSharingDelete(path string, access, createMode uint32) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(ffmpegPathArg(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, access,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, createMode, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// lockMuxTempFile takes f's lock without waiting: LockFileEx, exclusive, on
// the one byte at muxTempLockOffsetHigh. Windows releases it when the handle
// closes, which it does when the process holding it exits however it exits,
// and a lock through any other handle — this process's or another's —
// conflicts with it. errMuxTempHeld when someone else holds it; any other
// error says the lock cannot be tested here.
func lockMuxTempFile(f *os.File) error {
	ol := new(syscall.Overlapped)
	ol.OffsetHigh = muxTempLockOffsetHigh
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(ol)))
	if r != 0 {
		return nil
	}
	if errors.Is(err, errLockViolation) {
		return errMuxTempHeld
	}
	return err
}
