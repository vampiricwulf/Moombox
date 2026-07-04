//go:build windows

package tui

import (
	"os/exec"
	"strings"
	"syscall"
)

// openBrowserCmd builds the Windows browser-open command: explorer.exe
// (NOT `cmd /c start`) so a cold-started browser is re-parented by the
// shell OUTSIDE the launcher's kill-on-close Job Object — otherwise
// quitting Moombox would terminate the user's whole browser session.
//
// The command line is FORCED-QUOTED: Go only quotes args containing
// spaces/tabs/quotes, and explorer's legacy parser splits an unquoted
// '=' into separate arguments — every YouTube watch?v= URL — silently
// opening nothing (verified empirically). URLs cannot legally contain a
// literal '"'; strip defensively so the quoting can't be escaped.
func openBrowserCmd(url string) *exec.Cmd {
	url = strings.ReplaceAll(url, `"`, "")
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe "` + url + `"`}
	return cmd
}
