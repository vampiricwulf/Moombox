package chat

import (
	"os"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/treeguard"
)

// TestMain runs the package's tests through treeguard.Run, which fails the
// run when they leave a file in this directory. The cookie directory
// hardening is off first: the tests that write a cookie file into a
// t.TempDir would each leave its unjoined goroutine (a real icacls on
// Windows) walking a directory the test's cleanup is removing.
func TestMain(m *testing.M) {
	cookies.DisableDirTighteningForTesting()
	os.Exit(treeguard.Run(m))
}
