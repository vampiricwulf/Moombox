//go:build !linux && !windows

package disk

import "syscall"

// blockUnit is the unit statfs(2) counts Blocks and Bavail in. These
// platforms' Statfs_t has no f_frsize; Bsize is the block size there.
func blockUnit(stat *syscall.Statfs_t) uint64 {
	return uint64(stat.Bsize)
}
