//go:build !windows

package worker

import "syscall"

// diskFullErrnos are the errors a write returns on a full volume (isDiskFull).
var diskFullErrnos = []error{syscall.ENOSPC}
