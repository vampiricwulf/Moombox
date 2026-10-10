//go:build linux

package sidecar

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestSidecarRunsInItsOwnProcessGroup: a terminal's Ctrl+C reaches the whole
// foreground process group, so a sidecar sharing Moombox's group died at the
// same instant and every ordinary shutdown logged a stdout-EOF warning.
// Pdeathsig must survive alongside it — it is the crash-path cleanup.
//
// Mutant: drop Setpgid (or Pdeathsig) from configureCmdSysProcAttr.
func TestSidecarRunsInItsOwnProcessGroup(t *testing.T) {
	cmd := exec.Command("true")
	configureCmdSysProcAttr(cmd)
	a := cmd.SysProcAttr
	if a == nil || !a.Setpgid {
		t.Fatalf("SysProcAttr = %+v, want Setpgid", a)
	}
	if a.Pdeathsig != syscall.SIGKILL {
		t.Errorf("Pdeathsig = %v, want SIGKILL — the crash path still needs it", a.Pdeathsig)
	}
}
