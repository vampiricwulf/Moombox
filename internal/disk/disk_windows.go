package disk

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceExW   = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// GetDiskSpace returns disk space information for the volume containing path.
func GetDiskSpace(path string) (*DiskSpace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("disk: resolve path: %w", err)
	}

	root := filepath.VolumeName(abs) + `\`
	rootPtr, err := syscall.UTF16PtrFromString(root)
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
		uintptr(unsafe.Pointer(rootPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("disk: GetDiskFreeSpaceExW: %w", callErr)
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
