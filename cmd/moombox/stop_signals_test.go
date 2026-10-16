package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/logger"
)

// TestSignalExitCode pins what the launcher exits with when a stop signal finds
// no child to hand it to (before the first spawn, in a crash backoff): the
// status a shell gives a process that signal ended. It was a literal 143 when
// SIGTERM was the only signal the launcher caught.
//
// Mutant: return 143 whatever the signal — the SIGINT and SIGHUP rows.
func TestSignalExitCode(t *testing.T) {
	for _, tc := range []struct {
		sig  os.Signal
		want int
	}{
		{os.Interrupt, 130},
		{syscall.SIGTERM, 143},
		{syscall.SIGHUP, 129},
	} {
		if got := signalExitCode(tc.sig); got != tc.want {
			t.Errorf("signalExitCode(%v) = %d, want %d", tc.sig, got, tc.want)
		}
	}
}

// newStdoutCapturingLogger builds a logger whose stdout sink is a pipe the
// test reads (logger.New takes os.Stdout as it stands when it is called), with
// its file in a temp dir. read closes both and returns what reached stdout.
func newStdoutCapturingLogger(t *testing.T) (log *logger.Logger, read func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	log, err = logger.New(filepath.Join(t.TempDir(), "moombox.log"), "INFO", 1<<20, 1)
	os.Stdout = saved
	if err != nil {
		w.Close()
		r.Close()
		t.Fatal(err)
	}
	return log, func() string {
		log.Close()
		w.Close()
		out, _ := io.ReadAll(r)
		r.Close()
		return string(out)
	}
}

// TestHungUpTerminalGetsNoMoreLines pins what a hangup does to the logger's
// stdout: once stdout's terminal is gone nothing more is written to it, so the
// shutdown makes no attempt on a terminal that may not drain — the logger
// writes stdout under the lock the force-exit backstop's own log line needs.
// Both ways in: shutdown's muteHungUpTerminal (a --headless run, or a hangup
// after the TUI had gone), and the TUI's exit, which no longer hands stdout
// back to a terminal that hung up (restoreStdoutAfterTUI). Stdout that did not
// hang up keeps getting every line, and gets it back after the TUI.
//
// Mutants: drop the stdoutHungUp check in muteHungUpTerminal (the
// live-terminal mute row loses its line); drop its SuppressStdout (the
// hung-up mute row still writes); drop the check in restoreStdoutAfterTUI
// (the hung-up TUI row writes again); drop its RestoreStdout (the live TUI
// row stays silent).
func TestHungUpTerminalGetsNoMoreLines(t *testing.T) {
	for _, tc := range []struct {
		name     string
		afterTUI bool // the TUI had stdout suppressed and has just quit
		hungUp   bool
		wantLine bool
	}{
		{"shutdown, stdout's terminal still there", false, false, true},
		{"shutdown, stdout's terminal hung up", false, true, false},
		{"the TUI quit, its terminal still there", true, false, true},
		{"the TUI quit on a hangup", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, read := newStdoutCapturingLogger(t)
			s := &runState{log: log}
			s.stdoutHungUp.Store(tc.hungUp)
			if tc.afterTUI {
				log.SuppressStdout() // runTUI, before tui.Run
				s.restoreStdoutAfterTUI()
			} else {
				s.muteHungUpTerminal()
			}
			log.Info("a line from the shutdown")
			if got := strings.Contains(read(), "a line from the shutdown"); got != tc.wantLine {
				t.Errorf("line on stdout = %v, want %v", got, tc.wantLine)
			}
		})
	}
}

// TestRunWiresTheStopSignals pins the call sites no behavioural test can
// reach: run() needs a whole service stack and runTUI a terminal. The pieces
// are pinned by behaviour elsewhere — catchStopSignals and the launcher by
// TestLauncherHandsStopSignalsToTheChild, stopOnSignal by TestStopOnSignal,
// the two stdout helpers by TestHungUpTerminalGetsNoMoreLines, tui.Run's quit
// by TestRunUntilDoneQuitsWhenTheContextEnds.
//
//   - run() arms the stop with catchStopSignals(stopSignals(), s.stopOnSignal)
//     and not with signal.NotifyContext, whose stop function — s.cancel,
//     which the TUI's exit and a restart call before shutdown begins —
//     unregistered the signals, so a second one killed the child
//     mid-shutdown; and which never caught a hangup.
//   - runTUI hands tui.Run the run's context, the only thing that quits the
//     TUI on a hangup, and gives stdout back through restoreStdoutAfterTUI,
//     never straight to a terminal that may have hung up.
//   - shutdown() mutes a hung-up terminal before its first log line.
//
// Mutants: signal.NotifyContext back in run() (or catchStopSignals given
// anything but stopSignals() and s.stopOnSignal); tui.Run given
// context.Background(); s.log.RestoreStdout() back in runTUI in place of the
// helper; the muteHungUpTerminal call deleted.
func TestRunWiresTheStopSignals(t *testing.T) {
	calls := func(file, fn string) []string {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		var out []string
		for _, decl := range f.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if !ok || d.Name.Name != fn || d.Body == nil {
				continue
			}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					out = append(out, callText(call))
				}
				return true
			})
		}
		if out == nil {
			t.Fatalf("%s not found in %s", fn, file)
		}
		return out
	}
	has := func(list []string, want string) bool {
		for _, c := range list {
			if c == want {
				return true
			}
		}
		return false
	}

	run := calls("main.go", "run")
	if !has(run, "catchStopSignals(stopSignals(), s.stopOnSignal)") {
		t.Errorf("run() does not arm its stop with catchStopSignals(stopSignals(), s.stopOnSignal); calls: %v", run)
	}
	for _, c := range run {
		if strings.HasPrefix(c, "signal.NotifyContext(") {
			t.Errorf("run() calls %s: its stop function unregisters the signals, and the TUI's exit calls it before shutdown", c)
		}
	}
	tuiCalls := calls("tui_wiring.go", "runTUI")
	if !has(tuiCalls, "tui.Run(s.ctx, app)") {
		t.Errorf("runTUI does not hand tui.Run the run's context: a hangup would leave the TUI running; calls: %v", tuiCalls)
	}
	if !has(tuiCalls, "s.restoreStdoutAfterTUI()") || has(tuiCalls, "s.log.RestoreStdout()") {
		t.Errorf("runTUI gives stdout back other than through restoreStdoutAfterTUI: a hung-up terminal would get the shutdown's lines; calls: %v", tuiCalls)
	}
	if sd := calls("shutdown.go", "shutdown"); !has(sd, "s.muteHungUpTerminal()") {
		t.Errorf("shutdown() does not mute a hung-up terminal; calls: %v", sd)
	}
}

// callText renders a call expression back to source-ish text: enough to compare a
// call site against the one the test expects.
func callText(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return callText(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		args := make([]string, len(e.Args))
		for i, a := range e.Args {
			args[i] = callText(a)
		}
		return callText(e.Fun) + "(" + strings.Join(args, ", ") + ")"
	default:
		return "…"
	}
}
