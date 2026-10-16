//go:build !windows

package engine

import (
	"errors"
	"os"
	"syscall"
)

// createMuxTempFile creates a temporary output for its writer, refusing a
// name that exists. 0o666 under the umask is the mode FFmpeg gave the file
// when it created the output itself, and the archive keeps it: the rename
// moves this inode onto the final name.
func createMuxTempFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
}

// openMuxTempForSweep opens a temporary output the boot sweep found, to try
// its writer's lock: flock does not care how the file was opened.
func openMuxTempForSweep(path string) (*os.File, error) {
	return os.Open(path)
}

// lockMuxTempFile takes f's lock without waiting: flock(2), exclusive, on
// the open file. The kernel drops it when the last descriptor on that open
// file closes — when the process holding it exits, however it exits — and
// another open of the same file conflicts with it, in this process or any
// other on the machine (an NFS mount carries it to the server's lock manager).
// errMuxTempHeld when someone else holds it; any other error says the lock
// cannot be tested here.
func lockMuxTempFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errMuxTempHeld
		case errors.Is(err, syscall.EINTR):
			continue
		default:
			return err
		}
	}
}
