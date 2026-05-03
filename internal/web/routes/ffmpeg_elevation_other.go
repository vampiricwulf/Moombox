//go:build !windows

package routes

import (
	"errors"
	"os"
	"time"
)

// On non-Windows, these stubs exist purely so the package compiles.
// The runtime.GOOS != "windows" guards in ffmpeg.go prevent any install
// endpoints from invoking these functions. If those guards are ever
// removed, callers will see a clear "not supported" error instead of a
// panic.

// errElevationNotSupported is returned by runElevated on non-Windows.
// Defined as a package-level var so tests within the package can assert with errors.Is.
var errElevationNotSupported = errors.New("programmatic privilege elevation is not supported on this platform (Linux/macOS use sudo or pkexec at the user's discretion)")

// isElevated returns true when running as root on Unix-like systems.
// Used by ffmpeg.go's PrepareInstall, which already short-circuits on
// non-Windows; this implementation is a sensible default if that ever
// changes.
func isElevated() bool {
	return os.Geteuid() == 0
}

// runElevated is a stub on non-Windows; UAC has no equivalent here.
// Linux installs use sudo/pkexec which require user-driven invocation,
// not programmatic elevation from a long-running process.
func runElevated(script string) (uintptr, error) {
	return 0, errElevationNotSupported
}

// waitForProcess is a stub on non-Windows. The Windows implementation
// uses WaitForSingleObject; the Linux equivalent (waitpid) isn't needed
// because runElevated above never returns a real handle.
func waitForProcess(handle uintptr, timeout time.Duration) error {
	return errElevationNotSupported
}
