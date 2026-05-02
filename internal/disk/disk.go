// Package disk provides disk space queries for Moombox.
package disk

// DiskSpace holds disk usage information for a volume.
type DiskSpace struct {
	Free    uint64  // bytes free for caller
	Total   uint64  // total bytes on volume
	UsedPct float64 // percentage used (0-100)
}
