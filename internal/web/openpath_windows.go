//go:build windows

package web

import (
	"os/exec"
	"strings"
	"syscall"
)

// forceQuoteCmdLine replaces the command line Go would build for cmd with an
// explicitly quoted one: `explorer.exe "<target>"`.
//
// Go's os/exec quotes an argument only when it contains a space, a tab or a
// quote. A directory path like C:\Moombox\output\Stream=Archive therefore
// reaches explorer.exe bare, and explorer's legacy parser splits an unquoted
// '=' into separate arguments and silently opens nothing — no error, no
// window, a button that appears to do nothing (W R-3). The TUI's browser
// launch has carried this same quoting since the behaviour was verified
// empirically on YouTube watch?v= URLs; the folder-open path never got it.
//
// THE DUPLICATION WITH openBrowserCmd (internal/tui/openbrowser_windows.go)
// IS FORCED, NOT ACCIDENTAL. internal/tui may not import internal/web (the
// import fence), and SysProcAttr.CmdLine exists only in the Windows syscall
// package, so neither copy can be hoisted into a shared untagged file.
// Change one and change the other.
//
// cmd.Args is deliberately left alone. Windows ignores argv entirely once
// CmdLine is set (syscall.StartProcess prefers sys.CmdLine over
// makeCmdLine(argv)), and TestOpenPathCommandIsTheSameOnBothHosts asserts the
// argv is byte-identical on every platform — that pin is what lets a Linux
// runner check the Windows spawn at all, so it must not be weakened here.
//
// The quote strip is defensive: a Windows path cannot legally contain a '"',
// but stripping it means no target can ever escape the quoting.
func forceQuoteCmdLine(cmd *exec.Cmd, program, target string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: program + ` "` + strings.ReplaceAll(target, `"`, "") + `"`,
	}
}
