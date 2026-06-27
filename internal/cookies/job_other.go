//go:build !windows && !linux

package cookies

import (
	"os"
	"os/exec"
)

// processJob is a no-op stub on platforms without a parent-death-cleanup
// primitive wired up (Windows uses Job Objects, Linux uses PR_SET_PDEATHSIG;
// everything else just needs to compile). Returns a non-nil no-op so the
// return contract is uniform with the Windows/Linux builds and the package's
// `job != nil` call-site guards stay meaningful only for real failures.
type processJob struct{}

func newProcessJob() (*processJob, error)        { return &processJob{}, nil }
func (j *processJob) assign(p *os.Process) error { return nil }
func (j *processJob) close()                     {}

func configureCmdSysProcAttr(*exec.Cmd) {}
