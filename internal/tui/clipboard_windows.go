//go:build windows

package tui

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// clipboardHelperTimeout bounds the wait on clip.exe, and clipboardWaitDelay
// bounds the Wait that follows the kill. The helper normally returns in
// milliseconds; the bound exists for a wedged console or a clipboard another
// process is holding open. O C runs this inside a tea.Cmd, off the update
// goroutine, so what the bound protects is the feedback line rather than the
// frame rate: the timeout is that line's own lifetime, because a helper
// still running after it could not produce a message the operator would
// still connect to the press.
const (
	clipboardHelperTimeout = 3 * time.Second
	clipboardWaitDelay     = time.Second
)

// clipboardHelperCmd builds the clip.exe invocation: the text goes in on
// STDIN, never as an argument. An argv element would be word-split by the
// Windows command line and would leak the URL into the process table, and
// clip.exe reads stdin by design.
//
// WaitDelay bounds Wait itself. Stdin is a strings.Reader rather than an
// *os.File, so os/exec copies it through an OS pipe on a goroutine and Wait
// waits for that copy as well as for the process; killing the child normally
// breaks the pipe and unwinds it, but WaitDelay is the documented guard for
// when it does not, and without it the context bounds the CHILD rather than
// the call.
func clipboardHelperCmd(ctx context.Context, text string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "clip.exe")
	cmd.Stdin = strings.NewReader(text)
	cmd.WaitDelay = clipboardWaitDelay
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

// osClipboardFallback copies text with clip.exe on a LOCAL Windows console.
// It is a backup for the OSC 52 write the chord always sends, not a
// replacement for it: conhost (and legacy consoles generally) drop OSC 52 in
// silence, so on those terminals clip.exe is the only thing that can put the
// URL anywhere at all (CORE-15, O-W).
//
// It stands down in two shapes, because clip.exe writes the clipboard of the
// machine MOOMBOX RUNS ON, which is the operator's clipboard only when the
// session is local:
//
//   - WT_SESSION set — Windows Terminal handles OSC 52 itself, including
//     across SSH, so its own integration is authoritative and a second write
//     would at best duplicate it.
//   - SSH_CONNECTION / SSH_TTY / SSH_CLIENT set — the operator is sitting at
//     another machine. WT_SESSION is NOT inherited through OpenSSH (Windows
//     Terminal sets it only for its own children), so without this check an
//     SSH login falls straight through to clip.exe and writes the server's
//     clipboard while reporting a copy (B2).
//
// Returns whether the text reached THIS machine's system clipboard.
func osClipboardFallback(text string) bool {
	if os.Getenv("WT_SESSION") != "" {
		return false
	}
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CLIENT") != "" {
		return false
	}
	return clipboardHelper(text)
}
