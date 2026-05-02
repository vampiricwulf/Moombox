//go:build !windows

package disk

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// GetDiskSpace returns disk space information for the volume containing path.
// On Unix, uses statfs(2). Bavail (blocks available to non-superuser) is
// reported as Free so per-user quotas are reflected, matching the Windows
// behaviour of using freeBytesAvailable.
func GetDiskSpace(path string) (*DiskSpace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("disk: resolve path: %w", err)
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(abs, &stat); err != nil {
		return nil, fmt.Errorf("disk: statfs %q: %w", abs, err)
	}

	// Bsize is int32 on 32-bit platforms (386, arm) and int64 on 64-bit
	// (amd64, arm64). Casting to uint64 is safe because Bsize is always
	// positive for a mounted filesystem. The multiplications below cannot
	// overflow on realistic volumes — even with 4 KB blocks, you'd need
	// volumes larger than ~75 ZB to wrap a uint64.
	bsize := uint64(stat.Bsize)
	free := stat.Bavail * bsize
	total := stat.Blocks * bsize

	var usedPct float64
	if total > 0 {
		used := uint64(0)
		if free < total {
			used = total - free
		}
		usedPct = float64(used) / float64(total) * 100
	}

	return &DiskSpace{
		Free:    free,
		Total:   total,
		UsedPct: usedPct,
	}, nil
}
