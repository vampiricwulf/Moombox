package cipher

import (
	"os"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// TestMain runs the package's tests through treeguard.Run, which fails the
// run when they leave a file in this directory, with the player fetch kept
// off the network (keepTestsOffTheNetwork).
func TestMain(m *testing.M) {
	keepTestsOffTheNetwork()
	os.Exit(treeguard.Run(m))
}
