package disk

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// GetDiskSpace returns disk space information for the volume containing path.
//
// It asks about the deepest existing directory on the path, not the drive
// root: GetDiskFreeSpaceExW accepts any directory and answers for the volume
// that directory is on, so a recordings disk mounted into C:\Recordings
// reports its own space. Querying C:\ reported the C: volume's instead, and
// the low-space alert never fired for the disk actually filling up. A path
// that does not exist yet answers for the nearest existing ancestor, ending at
// the root as before.
func GetDiskSpace(path string) (*DiskSpace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("disk: resolve path: %w", err)
	}
	// An unrepresentable path is an error, not a reason to try its parent.
	if _, err := syscall.UTF16PtrFromString(abs); err != nil {
		return nil, fmt.Errorf("disk: utf16 convert: %w", err)
	}
	return queryNearestDirectory(abs, freeSpace)
}

// freeSpace queries the volume dir is on.
func freeSpace(dir string) (*DiskSpace, error) {
	// A UNC directory must end in a backslash, and any other may.
	if !strings.HasSuffix(dir, `\`) {
		dir += `\`
	}
	dirPtr, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return nil, fmt.Errorf("disk: utf16 convert: %w", err)
	}

	// freeBytesAvailable is the bytes available to the *caller* (respects per-
	// user quota when quotas are enabled — typically equal to totalFreeBytes
	// on Moombox's single-user Windows targets). totalFreeBytes is the
	// volume-wide free count. usedPct is computed against freeBytesAvailable
	// so the dashboard percentage reflects what the caller can actually use,
	// not the raw filesystem state. Audit reports/small-packages.md.
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	ret, _, callErr := getDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(dirPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("disk: GetDiskFreeSpaceExW %q: %w", dir, callErr)
	}

	var usedPct float64
	if totalBytes > 0 {
		usedPct = float64(totalBytes-freeBytesAvailable) / float64(totalBytes) * 100
	}

	return &DiskSpace{
		Free:    freeBytesAvailable,
		Total:   totalBytes,
		UsedPct: usedPct,
	}, nil
}
