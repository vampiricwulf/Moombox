package disk

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// SameVolume reports whether a and b are on one volume: the volume mount
// point (GetVolumePathName) of each path's deepest existing directory is the
// same, or the two mount points name one volume (a volume mounted both as a
// drive letter and into a folder). A path that cannot be resolved answers
// false — the caller then reads both, and Worse still yields a single
// reading.
func SameVolume(a, b string) bool {
	ma, okA := mountPointOf(a)
	mb, okB := mountPointOf(b)
	if !okA || !okB {
		return false
	}
	if strings.EqualFold(ma, mb) {
		return true
	}
	ga, okA := volumeGUID(ma)
	gb, okB := volumeGUID(mb)
	return okA && okB && strings.EqualFold(ga, gb)
}

// mountPointOf is the volume mount point path's deepest existing ancestor is
// under, e.g. `D:\` or `C:\Recordings\`.
func mountPointOf(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(abs); err == nil {
			break
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", false
		}
		abs = parent
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return "", false
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", false
	}
	return windows.UTF16ToString(buf), true
}

// volumeGUID is the `\?\Volume{...}\` name of the volume mounted at
// mountPoint. Network shares have none.
func volumeGUID(mountPoint string) (string, bool) {
	p, err := windows.UTF16PtrFromString(mountPoint)
	if err != nil {
		return "", false
	}
	buf := make([]uint16, 64)
	if err := windows.GetVolumeNameForVolumeMountPoint(p, &buf[0], uint32(len(buf))); err != nil {
		return "", false
	}
	return windows.UTF16ToString(buf), true
}
