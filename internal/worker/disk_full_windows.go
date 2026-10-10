//go:build windows

package worker

import "syscall"

// The Win32 codes for a full volume. Neither is in package syscall, and Go
// does not map either onto syscall.ENOSPC, so errors.Is must name them.
const (
	errorHandleDiskFull syscall.Errno = 39  // ERROR_HANDLE_DISK_FULL
	errorDiskFull       syscall.Errno = 112 // ERROR_DISK_FULL
)

// diskFullErrnos are the errors a write returns on a full volume (isDiskFull).
var diskFullErrnos = []error{errorDiskFull, errorHandleDiskFull}
