//go:build linux

package disk

import (
	"syscall"
	"testing"
)

// TestBlockUnitIsTheFragmentSize: statfs counts Blocks and Bavail in f_frsize
// units. A CIFS mount reports a 1 MiB f_bsize over 4 KiB blocks, and
// multiplying by Bsize inflated every figure 256-fold.
//
// Mutant: returning Bsize — the CIFS case is 256 times too large.
func TestBlockUnitIsTheFragmentSize(t *testing.T) {
	if got := blockUnit(&syscall.Statfs_t{Bsize: 1 << 20, Frsize: 4096}); got != 4096 {
		t.Errorf("CIFS-shaped statfs: unit = %d, want the 4096-byte fragment size", got)
	}
	if got := blockUnit(&syscall.Statfs_t{Bsize: 4096}); got != 4096 {
		t.Errorf("no Frsize reported: unit = %d, want Bsize", got)
	}
}
