//go:build !windows

package tui

import (
	"os/exec"
	"runtime"
)

// openBrowserCmd builds the platform browser-open command. See the
// Windows variant for the explorer.exe/quoting rationale.
func openBrowserCmd(url string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url)
	}
	return exec.Command("xdg-open", url)
}
