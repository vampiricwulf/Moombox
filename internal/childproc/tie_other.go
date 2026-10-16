//go:build !linux && !windows

package childproc

import (
	"os"
	"os/exec"
)

// No parent-death primitive is wired up here (Moombox supports Windows and
// Linux; everything else only has to build): the programs run untied, as
// exec.Cmd runs them.
const tieLocksThread = false

func prepare(*exec.Cmd)       {}
func adopt(*os.Process) error { return nil }
