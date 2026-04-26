//go:build windows

package main

import (
	"fmt"
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
	return nil
}

// releaseSingleInstanceLock closes the mutex handle. Safe to call on a
// process that never acquired (no-op).
func releaseSingleInstanceLock() {
	if singleInstanceHandle == 0 {
		return
	}
	procCloseHandle.Call(singleInstanceHandle)
	singleInstanceHandle = 0
}
