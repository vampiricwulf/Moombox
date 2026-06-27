//go:build !windows

package disk

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
)

// GetDiskSpace returns disk space information for the volume containing path.
// On Unix, uses statfs(2). Bavail (blocks available to non-superuser) is
// reported as Free so per-user quotas are reflected, matching the Windows
// behaviour of using freeBytesAvailable.
//
// Like the Windows implementation (which queries the volume root and so
// succeeds for paths that don't exist yet), a missing path falls back to the
// nearest existing ancestor: a configured output directory that hasn't been
// created reports the space of the volume it would be created on, keeping
// disk monitoring and low-space notifications alive.
func GetDiskSpace(path string) (*DiskSpace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("disk: resolve path: %w", err)
	}

	var stat syscall.Statfs_t
	target := abs
	for {
		err := syscall.Statfs(target, &stat)
		if err == nil {
			break
		}
		parent := filepath.Dir(target)
		if parent == target || (!errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENOTDIR)) {
			return nil, fmt.Errorf("disk: statfs %q: %w", abs, err)
		}
		target = parent
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
