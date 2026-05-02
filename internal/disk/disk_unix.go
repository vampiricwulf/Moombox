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

	// Bsize is the optimal transfer block size; Bavail is blocks available
	// to a non-privileged user. uint64 cast handles 32-bit Bsize on some
	// platforms.
	bsize := uint64(stat.Bsize)
	free := stat.Bavail * bsize
	total := stat.Blocks * bsize

	var usedPct float64
	if total > 0 {
		usedPct = float64(total-free) / float64(total) * 100
	}

	return &DiskSpace{
		Free:    free,
		Total:   total,
		UsedPct: usedPct,
	}, nil
}
