package web

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isAddrInUse: Winsock reports a taken port as WSAEADDRINUSE, which
// syscall.EADDRINUSE (an invented Errno on Windows) never matches.
func isAddrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE)
}
