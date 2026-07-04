//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// singleInstanceMutexName is the Windows global named-mutex used to refuse
// a second moombox.exe launch. The "Local\\" prefix scopes to the current
// session — a different user logged into the same machine can run their
// own instance without conflict. Audit cmd-moombox.md TD-5 / W-single-
// instance.
const singleInstanceMutexName = `Local\Moombox-SingleInstance-{8C7F1A2D-3B4E-4D5F-9A6B-7C8D9E0F1A2B}`

// errAlreadyExists matches the Windows ERROR_ALREADY_EXISTS return code from
// CreateMutexW when the mutex name is already held by another process.
const errAlreadyExists = syscall.Errno(183)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutexW     = kernel32.NewProc("CreateMutexW")
	procCloseHandle      = kernel32.NewProc("CloseHandle")
	singleInstanceHandle uintptr // kept to release the mutex on graceful exit
)

// acquireSingleInstanceLock returns nil if this process is the first
// moombox.exe in the current Windows session. If another instance is
// already running, returns a clear error so main() can exit with a
// useful message instead of fighting for the port.
//
// The mutex is held for the lifetime of the process; releaseSingleInstanceLock
// closes it on graceful exit. A crashed process releases the mutex via
// kernel handle cleanup, so a stale lock is impossible.
//
// Safe to call multiple times — only the first call acquires; subsequent
// calls are no-ops.
func acquireSingleInstanceLock() error {
	if singleInstanceHandle != 0 {
		return nil // already acquired
	}

	namePtr, err := syscall.UTF16PtrFromString(singleInstanceMutexName)
	if err != nil {
		return fmt.Errorf("single-instance mutex name encode: %w", err)
	}

	// CreateMutexW(lpMutexAttributes=nil, bInitialOwner=TRUE, lpName=namePtr).
	// bInitialOwner=TRUE so we hold the mutex from the moment of creation.
	handle, _, callErr := procCreateMutexW.Call(
		0,
		1,
		uintptr(unsafe.Pointer(namePtr)),
	)
	if handle == 0 {
		return fmt.Errorf("CreateMutexW failed: %w", callErr)
	}

	// callErr is always set on Windows (Last-Error semantics); the actual
	// success path returns ERROR_ALREADY_EXISTS when the name was taken.
	if errno, ok := callErr.(syscall.Errno); ok && errno == errAlreadyExists {
		// Another process already holds the mutex. Close our handle to the
		// (existing) named mutex so we don't leak a kernel object, then
		// signal the conflict.
		procCloseHandle.Call(handle)
		return fmt.Errorf("another moombox instance is already running in this session")
	}

	singleInstanceHandle = handle

	// Cross-session guard: the Local\ mutex above is per-logon-session BY
	// DESIGN (different users may run their own instances), but that also
	// means a Task Scheduler instance ("run whether user is logged on or
	// not") and an interactive one in the same install dir don't see each
	// other's mutex — the second survives the port conflict in TUI mode
	// and double-writes the DB. Mirror the Unix flock with an exclusive
	// lock on a per-user file. Scope note: like the Unix flock, this is
	// PER-USER, not per-install — two independent installs run by the
	// same user across sessions now conflict (matching Linux behavior);
	// different users still coexist. FAIL-OPEN on anything unexpected (missing
	// env, ACLs, exotic filesystems): log and continue — the mutex stays
	// the primary guard and a startup regression would be worse than the
	// rare double-start this closes.
	if err := acquireCrossSessionLock(); err != nil {
		releaseSingleInstanceLock()
		return err
	}
	return nil
}

var (
	procLockFileEx         = kernel32.NewProc("LockFileEx")
	crossSessionLockHandle syscall.Handle
)

const (
	lockfileExclusiveLock   = 0x2
	lockfileFailImmediately = 0x1
	errLockViolation        = syscall.Errno(33)
)

// acquireCrossSessionLock takes an exclusive LockFileEx on
// %LOCALAPPDATA%\moombox\moombox.lock. Returns an error ONLY when another
// process (any session, same user) already holds it; every other failure
// is fail-open (nil).
func acquireCrossSessionLock() error {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return nil
	}
	lockDir := filepath.Join(base, "moombox")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cross-session lock dir unavailable (%v) — continuing with session mutex only\n", err)
		return nil
	}
	pathPtr, err := syscall.UTF16PtrFromString(filepath.Join(lockDir, "moombox.lock"))
	if err != nil {
		return nil
	}
	h, err := syscall.CreateFile(pathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cross-session lock file open failed (%v) — continuing with session mutex only\n", err)
		return nil
	}
	var ovl syscall.Overlapped
	r, _, callErr := procLockFileEx.Call(
		uintptr(h),
		lockfileExclusiveLock|lockfileFailImmediately,
		0, // reserved
		1, // lock 1 byte
		0,
		uintptr(unsafe.Pointer(&ovl)),
	)
	if r == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == errLockViolation {
			syscall.CloseHandle(h)
			return fmt.Errorf("another moombox instance is already running (cross-session lock held)")
		}
		fmt.Fprintf(os.Stderr, "warning: cross-session lock failed (%v) — continuing with session mutex only\n", callErr)
		syscall.CloseHandle(h)
		return nil
	}
	crossSessionLockHandle = h
	return nil
}

// releaseSingleInstanceLock closes the mutex handle and the cross-session
// lock file. Safe to call on a process that never acquired (no-op).
func releaseSingleInstanceLock() {
	if crossSessionLockHandle != 0 {
		syscall.CloseHandle(crossSessionLockHandle) // releases the LockFileEx region
		crossSessionLockHandle = 0
	}
	if singleInstanceHandle == 0 {
		return
	}
	procCloseHandle.Call(singleInstanceHandle)
	singleInstanceHandle = 0
}
