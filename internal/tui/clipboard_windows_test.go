//go:build windows

package tui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// The clip.exe spawn is checked by SHAPE, never by running it: a real spawn
// in a test would overwrite the developer's actual clipboard, and the
// interesting properties are all structural anyway — the program, an argv
// with nothing else in it, and the text on stdin.
//
// Mutant: passing the text as an argument (exec.Command("clip.exe", text)) —
// the argv check fails and the stdin check finds nothing.
func TestClipboardHelperCommandShape(t *testing.T) {
	const url = "https://example.test/x?a=1&b=2"
	cmd := clipboardHelperCmd(context.Background(), url)

	if got := filepath.Base(cmd.Path); !strings.EqualFold(got, "clip.exe") {
		t.Errorf("helper program = %q (path %q), want clip.exe", got, cmd.Path)
	}
	if len(cmd.Args) != 1 {
		t.Errorf("helper argv = %q, want the program alone — the URL goes on stdin", cmd.Args)
	}
	if cmd.Stdin == nil {
		t.Fatal("helper has no stdin, so the URL reaches clip.exe by no route at all")
	}
	b, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read helper stdin: %v", err)
	}
	if string(b) != url {
		t.Errorf("helper stdin = %q, want %q", b, url)
	}
}

// The context bound is what keeps a wedged child from freezing the update
// goroutine, and exec.CommandContext is what applies it — a plain
// exec.Command would ignore the deadline entirely.
//
// Mutant: building the command with exec.Command instead — Cancel is nil.
func TestClipboardHelperCommandIsContextBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cmd := clipboardHelperCmd(ctx, "x"); cmd.Cancel == nil {
		t.Error("the helper command is not context-bound: a hung clip.exe would never be reaped")
	}
	if clipboardHelperTimeout <= 0 {
		t.Errorf("clipboardHelperTimeout = %v, want a positive bound", clipboardHelperTimeout)
	}
	// cmd.Stdin is a strings.Reader, not an *os.File, so exec copies it
	// through an OS pipe in a goroutine and Wait waits for that goroutine
	// too. Killing the child normally breaks the pipe and unwinds the copy,
	// but WaitDelay is the documented guard for the case where it does not:
	// without it the bound is on the CHILD, not on Wait (B4).
	//
	// Mutant: leaving WaitDelay at its zero value (wait forever).
	if cmd := clipboardHelperCmd(ctx, "x"); cmd.WaitDelay <= 0 {
		t.Errorf("helper WaitDelay = %v, want a positive bound on Wait itself", cmd.WaitDelay)
	}
}

// clip.exe writes the clipboard of the machine Moombox is RUNNING on, which
// is only the operator's clipboard when the session is local. Two shapes are
// not local, and the helper stands down for both (CORE-15, O-W; B2):
//
//   - Windows Terminal (WT_SESSION) handles OSC 52 itself, including across
//     SSH, so its own integration is authoritative and a second write is at
//     best redundant.
//   - An SSH session (SSH_CONNECTION / SSH_TTY / SSH_CLIENT) is the operator
//     sitting at ANOTHER machine. WT_SESSION is not inherited through
//     OpenSSH — it is set only for Windows Terminal's own children — so
//     without this check an SSH login would fall straight through to
//     clip.exe and put the URL on the server's clipboard while telling the
//     operator it was copied. OSC 52, which the chord always sends, reaches
//     the terminal they are actually sitting at.
//
// Mutant: dropping the SSH check — the three SSH rows spawn a helper.
// Mutant: dropping the WT_SESSION check — the first row spawns a helper.
// Mutant: inverting either check — the "local console" row stops spawning,
// which is the one shape clip.exe exists for.
func TestOSClipboardFallbackStandsDownWhenClipExeIsTheWrongClipboard(t *testing.T) {
	orig := clipboardHelper
	t.Cleanup(func() { clipboardHelper = orig })

	// The four variables this gate reads, cleared for every row by t.Setenv
	// and restored when the test ends.
	env := []string{"WT_SESSION", "SSH_CONNECTION", "SSH_TTY", "SSH_CLIENT"}

	rows := []struct {
		name      string
		set       string // the one variable this row sets, "" for none
		wantSpawn bool
	}{
		{"local console (conhost)", "", true},
		{"Windows Terminal", "WT_SESSION", false},
		{"SSH_CONNECTION", "SSH_CONNECTION", false},
		{"SSH_TTY", "SSH_TTY", false},
		{"SSH_CLIENT", "SSH_CLIENT", false},
	}
	for _, row := range rows {
		calls := 0
		clipboardHelper = func(string) bool { calls++; return true }
		for _, name := range env {
			if name == row.set {
				t.Setenv(name, "1")
			} else {
				t.Setenv(name, "")
			}
		}
		got := osClipboardFallback("x")
		if got != row.wantSpawn {
			t.Errorf("%s: osClipboardFallback = %v, want %v", row.name, got, row.wantSpawn)
		}
		want := 0
		if row.wantSpawn {
			want = 1
		}
		if calls != want {
			t.Errorf("%s: the helper ran %d times, want %d", row.name, calls, want)
		}
	}
}
