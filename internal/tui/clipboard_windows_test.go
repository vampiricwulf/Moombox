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
}

// Windows Terminal handles OSC 52 itself — including across SSH, where a
// local clip.exe would write the wrong machine's clipboard — so the fallback
// stands down whenever WT_SESSION is set (CORE-15, O-W).
//
// Mutant: dropping the WT_SESSION check — the helper is spawned in Windows
// Terminal too and the first case fails.
func TestOSClipboardFallbackStandsDownInWindowsTerminal(t *testing.T) {
	calls := 0
	orig := clipboardHelper
	t.Cleanup(func() { clipboardHelper = orig })
	clipboardHelper = func(string) bool { calls++; return true }

	t.Setenv("WT_SESSION", "1")
	if osClipboardFallback("x") {
		t.Error("in Windows Terminal the fallback must decline and leave OSC 52 to the terminal")
	}
	if calls != 0 {
		t.Errorf("the helper was spawned %d times inside Windows Terminal, want 0", calls)
	}

	t.Setenv("WT_SESSION", "")
	if !osClipboardFallback("x") {
		t.Error("outside Windows Terminal a helper that took the text must report the copy")
	}
	if calls != 1 {
		t.Errorf("the helper ran %d times outside Windows Terminal, want 1", calls)
	}
}
