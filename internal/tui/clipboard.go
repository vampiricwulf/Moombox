package tui

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// readClipboard reads text from the system clipboard.
// Returns empty string if clipboard is empty or reading fails.
func readClipboard() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", "Get-Clipboard")
	case "darwin":
		cmd = exec.CommandContext(ctx, "pbpaste")
	default:
		// Linux: try xclip first, then xsel
		cmd = exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-o")
		out, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
		cmd = exec.CommandContext(ctx, "xsel", "--clipboard", "--output")
	}

	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
