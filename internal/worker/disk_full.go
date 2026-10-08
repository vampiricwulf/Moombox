package worker

import (
	"errors"
	"strings"
)

// diskFullTexts are the out-of-space messages, lowercased, as they reach the
// worker once a chain has been flattened to text: FFmpeg's stderr tail (the
// muxer wraps it with %s, engine runFFmpeg), and the engine's HLS init-write
// error, which wraps the os error with %v.
var diskFullTexts = []string{
	// ENOSPC's strerror: Go's own text for it on unix, and FFmpeg's on every
	// platform — the Windows builds' C runtime maps a full disk to ENOSPC.
	"no space left on device",
	// FormatMessage's texts for ERROR_DISK_FULL and ERROR_HANDLE_DISK_FULL,
	// which is what Go's syscall.Errno prints for them on Windows.
	"there is not enough space on the disk",
	"the disk is full",
}

// isDiskFull reports whether err is a failure to write for want of disk
// space: the platform's own error (diskFullErrnos — ENOSPC on unix,
// ERROR_DISK_FULL and ERROR_HANDLE_DISK_FULL on Windows) anywhere in the
// chain, which is how the engine's writes reach the worker (engine.ErrLocalWrite
// wraps the *os.PathError with %w), or one of diskFullTexts in the message,
// which is how FFmpeg's do.
func isDiskFull(err error) bool {
	if err == nil {
		return false
	}
	for _, errno := range diskFullErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	for _, text := range diskFullTexts {
		if strings.Contains(msg, text) {
			return true
		}
	}
	return false
}
