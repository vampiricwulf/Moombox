// Package disk provides disk space queries for Moombox.
package disk

import "path/filepath"

// DiskSpace holds disk usage information for a volume.
type DiskSpace struct {
	Free    uint64  // bytes free for caller
	Total   uint64  // total bytes on volume
	UsedPct float64 // percentage used (0-100)
}

// queryNearestDirectory asks query about dir and, while that fails, about each
// ancestor in turn up to the root — so a path that does not exist yet answers
// for the volume it would be created on, and an existing one for the volume it
// is ON, which may be a different one mounted into a folder of its drive.
// Returns the root's error when even the root fails.
func queryNearestDirectory(dir string, query func(string) (*DiskSpace, error)) (*DiskSpace, error) {
	for {
		space, err := query(dir)
		if err == nil {
			return space, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, err
		}
		dir = parent
	}
}
