//go:build !windows && !linux

package sidecar

import (
	"os"
	"os/exec"
)

// processJob is a no-op stub on platforms without a parent-death-cleanup
// primitive wired up (Moombox supports Windows via Job Objects and Linux
// via PR_SET_PDEATHSIG; everything else just needs to compile). A crashed
// Moombox on these platforms can leave the sidecar running until it
// notices stdin EOF.
type processJob struct{}

func newProcessJob() (*processJob, error)      { return &processJob{}, nil }
func (j *processJob) assign(*os.Process) error { return nil }
func (j *processJob) close()                   {}

func configureCmdSysProcAttr(*exec.Cmd) {}
