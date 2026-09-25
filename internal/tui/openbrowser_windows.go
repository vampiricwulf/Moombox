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
//
// The trailing-backslash doubling (syscall.EscapeArg's rule: inside a quoted
// argument a run of backslashes before the closing quote escapes it) is
// UNREACHABLE here — a URL cannot end in a '\'. It is carried anyway because
// the web twin, forceQuoteCmdLine (internal/web/openpath_windows.go), composes
// directory paths where `D:\` is reachable, and the two must stay
// byte-identical: nothing mechanically ties them, so the only thing a reader
// can rely on is that checking one is checking both. Change one, change the
// other.
func openBrowserCmd(url string) *exec.Cmd {
	url = strings.ReplaceAll(url, `"`, "")
	url += strings.Repeat(`\`, len(url)-len(strings.TrimRight(url, `\`)))
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe "` + url + `"`}
	return cmd
}
