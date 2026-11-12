//go:build !windows

package disk

import (
	"path/filepath"
	"syscall"
)

// SameVolume reports whether a and b are on one filesystem: the device
// (st_dev) of each path's deepest existing directory is the same. A path
// that cannot be resolved or stat'ed at all answers false — the caller then
// reads both, and Worse still yields a single reading.
func SameVolume(a, b string) bool {
	da, okA := deviceOf(a)
	db, okB := deviceOf(b)
	return okA && okB && da == db
}

// deviceOf is the st_dev of path's deepest existing ancestor.
func deviceOf(path string) (uint64, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, false
	}
	for {
		var st syscall.Stat_t
		if err := syscall.Stat(abs, &st); err == nil {
			return uint64(st.Dev), true // Dev is narrower on some platforms
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return 0, false
		}
		abs = parent
	}
}
