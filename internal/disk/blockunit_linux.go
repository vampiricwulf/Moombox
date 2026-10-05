//go:build linux

package disk

import "syscall"

// blockUnit is the unit statfs(2) counts Blocks and Bavail in: f_frsize, the
// fundamental block size — what coreutils df multiplies by. f_bsize is only
// the preferred I/O size and can differ: a CIFS mount advertises 1 MiB there
// by default on 5.1+ kernels against 4 KiB blocks, which inflated Free and
// Total 256-fold in the status bar, the Stats tab and the low-disk alert
// text. Falls back to Bsize when a filesystem reports no Frsize.
//
// Both fields are int32 on 32-bit platforms and int64 on 64-bit ones, and
// positive for a mounted filesystem, so the conversion is safe.
func blockUnit(stat *syscall.Statfs_t) uint64 {
	if stat.Frsize > 0 {
		return uint64(stat.Frsize)
	}
	return uint64(stat.Bsize)
}
