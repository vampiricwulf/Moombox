//go:build !windows && !linux

package main

import (
	"os"
	"os/exec"
)

// configureLauncherChild is a no-op on platforms without a parent-death
// primitive (documented no-op — Moombox officially supports Windows and
// Linux; here the child simply isn't tied to the launcher's lifetime).
func configureLauncherChild(cmd *exec.Cmd) {}

// newLauncherJob is Windows-only (Job Object).
func newLauncherJob() uintptr { return 0 }

// assignLauncherJob is Windows-only.
func assignLauncherJob(job uintptr, p *os.Process) {}
