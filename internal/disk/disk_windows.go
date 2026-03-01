// Package disk provides disk space queries for Moombox.
package disk

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

// DiskSpace holds disk usage information for a volume.
type DiskSpace struct {
	Free    uint64  // bytes free for caller
	Total   uint64  // total bytes on volume
	UsedPct float64 // percentage used (0-100)
}

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
