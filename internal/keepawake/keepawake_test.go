package keepawake

import (
	"os"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// TestMain runs the package's tests through treeguard.Run, which fails the
// run when a test leaves files in the package directory.
func TestMain(m *testing.M) {
	os.Exit(treeguard.Run(m))
}

// TestAHolderThatHoldsNothingAnswersWithoutAsking: releasing before anything
// was held, and anything after Close, must not start the holder's thread or
// report a hold that is not there.
//
// Mutant: answer true after Close (the last check).
func TestAHolderThatHoldsNothingAnswersWithoutAsking(t *testing.T) {
	h := New()
	if got := h.Hold(false); got != Supported() {
		t.Errorf("Hold(false) on a fresh holder = %v, want %v", got, Supported())
	}
	h.Close()
	h.Close() // twice is harmless
	if h.Hold(true) {
		t.Error("Hold reported success after Close")
	}
}
