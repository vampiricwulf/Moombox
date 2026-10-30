package httpxtest

import (
	"os"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// TestMain runs the package's tests through treeguard.Run, which fails the
// run when they leave a file in this directory.
func TestMain(m *testing.M) { os.Exit(treeguard.Run(m)) }
