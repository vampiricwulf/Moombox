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
// The composing itself lives in forceQuoteCmdLine below — a byte-for-byte
// copy of the web twin, held there by a mechanical pin.
func openBrowserCmd(url string) *exec.Cmd {
	cmd := exec.Command("explorer.exe")
	forceQuoteCmdLine(cmd, "explorer.exe", url)
	return cmd
}

// forceQuoteCmdLine is a BYTE-FOR-BYTE COPY of the function of the same name
// in internal/web/openpath_windows.go, where the behaviour is documented in
// full: the defensive quote strip, and the trailing-backslash doubling
// (syscall.EscapeArg's rule — inside a quoted argument a run of backslashes
// immediately before the closing quote escapes it, so `D:\` would compose to
// `explorer.exe "D:\"` and parse as the single argument `D:"`).
//
// THE COPY IS FORCED, NOT ACCIDENTAL. internal/tui may not import
// internal/web (the import fence), and SysProcAttr.CmdLine exists only in the
// Windows syscall package, so neither side can be hoisted into a shared
// untagged file. What ties them is no longer only a comment:
// TestWindowsCmdLineComposersAreByteIdentical
// (internal/web/openpath_parity_test.go — UNTAGGED, so ubuntu runs it where
// neither _windows_test.go table compiles) reads both files and fails the
// moment the two bodies drift. Change one and change the other.
//
// The trailing-backslash doubling is UNREACHABLE through openBrowserCmd — a
// URL cannot end in a '\' — and is carried anyway, because the whole value of
// the copy is that checking one composer is checking both.
func forceQuoteCmdLine(cmd *exec.Cmd, program, target string) {
	q := strings.ReplaceAll(target, `"`, "")
	q += strings.Repeat(`\`, len(q)-len(strings.TrimRight(q, `\`)))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: program + ` "` + q + `"`,
	}
}
