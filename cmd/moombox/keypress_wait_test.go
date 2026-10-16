package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/types"
	"io"
	"strings"
	"testing"
	"time"
)

// awaitKeypressReturns runs awaitKeypress on in and reports whether it
// returned within d, with what it printed.
func awaitKeypressReturns(ctx context.Context, interactive bool, in io.Reader, d time.Duration) (bool, string) {
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer func() { _ = recover() }()
		defer close(done)
		awaitKeypress(ctx, interactive, in, &out)
	}()
	select {
	case <-done:
		return true, out.String()
	case <-time.After(d):
		return false, ""
	}
}

// A headless run never waits, whatever stdin is. In a container started with
// a TTY (`docker run -t`, compose `tty: true`) stdin IS a terminal, under
// MOOMBOX_NO_TUI, and nothing ever writes to it: the old isatty(stdin) check
// printed "Press Enter to exit..." and blocked a startup error's exit 3
// forever. The reader here never answers, as that terminal never does.
//
// Mutant: dropping the `!interactive` return — the wait blocks on the silent
// input and prints the prompt.
func TestAHeadlessStartupErrorDoesNotWaitForAKeypress(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	returned, printed := awaitKeypressReturns(context.Background(), false, r, 2*time.Second)
	if !returned {
		t.Fatal("a headless run waited for a keypress on an input nothing writes to")
	}
	if printed != "" {
		t.Errorf("a headless run printed %q; the prompt is a false line in a log nobody can answer from", printed)
	}
}

// An interactive wait ends with the signal context: SIGTERM (`docker stop`,
// the launcher's forwarded stop) and SIGINT cancel run()'s context, and a read
// blocked on the terminal used to swallow both until the stop's grace period
// ran out into a SIGKILL.
//
// Mutant: dropping the `<-ctx.Done()` case — the wait outlives the cancel.
func TestTheKeypressWaitEndsWithTheSignalContext(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer func() { _ = recover() }()
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	returned, printed := awaitKeypressReturns(ctx, true, r, 5*time.Second)
	if !returned {
		t.Fatal("a cancelled signal context did not end the keypress wait")
	}
	if !strings.Contains(printed, "Press Enter to exit...") {
		t.Errorf("an interactive wait printed %q, want the prompt", printed)
	}
}

// What the wait is for still works: on a console (a double-clicked Windows
// .exe) the error stays on screen until Enter, and Enter ends it.
//
// Mutant: returning before the read (or dropping the `<-pressed` case) — the
// wait does not hold before the Enter, or never ends on it.
func TestAnInteractiveStartupErrorWaitsForEnter(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer func() { _ = recover() }()
		defer close(done)
		awaitKeypress(context.Background(), true, r, &out)
	}()
	select {
	case <-done:
		t.Fatal("the interactive wait returned before any key was pressed")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enter did not end the interactive wait")
	}
}

// run() is where the two rules reach production, and only a failing boot
// reaches it there: so its call site is read from source. The one wait is
// the initServices failure's, handed the signal context and useTUI; the
// headless web-bind failure has none, since a headless run never waits.
//
// Mutants: `waitForKeypress(context.Background(), useTUI)` — SIGTERM no
// longer ends the wait; `waitForKeypress(ctx, true)` — a headless container
// with a TTY waits again; restoring a wait to the web-bind arm — two calls;
// run's ctx from context.Background() — not the one a stop signal ends.
func TestRunWaitsForAKeypressOnlyWithItsSignalContextAndUseTUI(t *testing.T) {
	const want = "waitForKeypress(ctx, useTUI)"
	fset, body := funcBody(t, "main.go", "run")
	var found []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "waitForKeypress" {
			got := types.ExprString(call)
			found = append(found, got)
			if got != want {
				t.Errorf("run() at %s calls %s, want %s", fset.Position(call.Pos()), got, want)
			}
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("run() calls waitForKeypress %d times (%v), want once — the initServices failure", len(found), found)
	}
	// And ctx is the one a stop signal ends: run's ctx comes from
	// context.WithCancel, its cancel is the runState's, and run arms
	// catchStopSignals with s.stopOnSignal, which calls that cancel
	// (TestRunWiresTheStopSignals pins the arming, the stop_signals tests
	// what stopOnSignal does).
	var withCancel, cancelHanded, armed bool
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == 2 && len(x.Rhs) == 1 {
				if id, ok := x.Lhs[0].(*ast.Ident); ok && id.Name == "ctx" &&
					strings.HasPrefix(types.ExprString(x.Rhs[0]), "context.WithCancel(") {
					withCancel = true
				}
			}
		case *ast.CompositeLit:
			if types.ExprString(x.Type) != "runState" {
				return true
			}
			for _, el := range x.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok && types.ExprString(kv.Key) == "cancel" && types.ExprString(kv.Value) == "cancel" {
					cancelHanded = true
				}
			}
		case *ast.CallExpr:
			if types.ExprString(x) == "catchStopSignals(stopSignals(), s.stopOnSignal)" {
				armed = true
			}
		}
		return true
	})
	if !withCancel || !cancelHanded || !armed {
		t.Errorf("run()'s ctx is not the one a stop signal ends (ctx from context.WithCancel: %v, its cancel on runState: %v, catchStopSignals armed with s.stopOnSignal: %v); the keypress wait would not end on SIGTERM",
			withCancel, cancelHanded, armed)
	}
}
