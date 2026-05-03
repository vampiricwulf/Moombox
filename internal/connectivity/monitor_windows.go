package connectivity

import (
	"syscall"
	"unsafe"
)

var (
	wininet                  = syscall.NewLazyDLL("wininet.dll")
	procInternetGetConnected = wininet.NewProc("InternetGetConnectedState")
)

// checkInternetConnected uses Windows' InternetGetConnectedState to
// query the system's notion of connectivity. This is a heuristic check
// that doesn't actually verify reachability — it just reports whether
// Windows believes there's an active connection. The Monitor's
// two-poll debounce smooths over transient flapping.
func checkInternetConnected() bool {
	var flags uint32
	ret, _, _ := procInternetGetConnected.Call(
		uintptr(unsafe.Pointer(&flags)),
		0,
	)
	return ret != 0
}
