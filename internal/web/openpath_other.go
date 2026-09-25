//go:build !windows

package web

import "os/exec"

// forceQuoteCmdLine is a no-op everywhere but Windows.
//
// Two reasons, and either alone would be enough: syscall.SysProcAttr carries
// a CmdLine field on no other platform (so the Windows body would not
// compile, and GOOS=linux go vet is a merge gate), and no other platform
// needs it (exec hands argv to the child directly — there is no legacy string
// parser to re-split an '='). See openpath_windows.go for what the Windows
// arm does and why.
func forceQuoteCmdLine(cmd *exec.Cmd, program, target string) {}
