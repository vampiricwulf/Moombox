//go:build windows

package tui

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// clipboardHelperTimeout bounds the wait on clip.exe. The helper normally
// returns in milliseconds, but O C runs on bubbletea's update goroutine, so
// an unbounded Wait on a wedged child would freeze the whole TUI. The bound
// is the feedback line's own lifetime: a helper still running after it could
// not have produced a message the operator would see attached to the press.
const clipboardHelperTimeout = 3 * time.Second

// clipboardHelperCmd builds the clip.exe invocation: the text goes in on
// STDIN, never as an argument. An argv element would be word-split by the
// Windows command line and would leak the URL into the process table, and
// clip.exe reads stdin by design.
func clipboardHelperCmd(ctx context.Context, text string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "clip.exe")
	cmd.Stdin = strings.NewReader(text)
	return cmd
}

// runClipboardHelper spawns clip.exe and reports whether it took the text.
//
// The child is TRACKED: Start hands back an *exec.Cmd holding this process's
// own PID and Wait reaps exactly that PID. Nothing here matches on an image
// name, and nothing kills a process it did not start — the context's own
// cancel (exec.CommandContext's default) signals this child and no other.
func runClipboardHelper(text string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardHelperTimeout)
	defer cancel()

	cmd := clipboardHelperCmd(ctx, text)
	if err := cmd.Start(); err != nil {
		// No clip.exe on PATH, or the spawn was refused. The caller falls
		// back to OSC 52 and says so; it never claims a copy.
		return false
	}
	return cmd.Wait() == nil
}

// clipboardHelper is the seam the tests replace. Spawning a real clip.exe in
// a test would write the developer's actual clipboard, so the structural
// checks assert on the command clipboardHelperCmd builds and the gating
// checks drive this variable instead.
var clipboardHelper = runClipboardHelper

// osClipboardFallback copies text with clip.exe when the terminal is NOT
// Windows Terminal. tea.SetClipboard speaks OSC 52, which conhost (and
// legacy consoles generally) silently drop — so on those terminals "Copied"
// was a promise nothing kept (CORE-15, O-W). WT_SESSION is Windows
// Terminal's own marker; when it is set OSC 52 works and this is skipped, so
// the terminal's own clipboard integration — which honours the remote end of
// an SSH session, where a local clip.exe would not — stays authoritative.
//
// Returns whether the text reached the system clipboard.
func osClipboardFallback(text string) bool {
	if os.Getenv("WT_SESSION") != "" {
		return false
	}
	return clipboardHelper(text)
}
