package main

import (
	"context"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func consoleEndMarked(c *consoleEnd) bool {
	select {
	case <-c.marked():
		return true
	default:
		return false
	}
}

// TestOnlyAWindowsConsoleEndHurriesTheStop pins owner ruling R2-X4: the short
// shutdown budget is for the three events after which Windows ends the process
// itself (console close, logoff, system shutdown — the only things Go raises
// SIGTERM for on Windows). Ctrl+C keeps the ordinary timing everywhere, and so
// does SIGTERM on Linux, where Docker and systemd wait their own time.
//
// Mutants: mark on every stop signal (the Ctrl+C row, and SIGTERM on Linux);
// never mark (SIGTERM on Windows).
func TestOnlyAWindowsConsoleEndHurriesTheStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  os.Signal
		want bool
	}{
		{"SIGTERM", syscall.SIGTERM, runtime.GOOS == "windows"},
		{"Ctrl+C", os.Interrupt, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := &runState{ctx: ctx, cancel: cancel}
			s.stopOnSignal(tc.sig)
			if ctx.Err() == nil {
				t.Error("the run's context is still live: the signal stopped nothing")
			}
			if got := consoleEndMarked(&s.consoleEnd); got != tc.want {
				t.Errorf("console end marked = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConsoleEndExitsWhenItsBudgetRunsOut drives the watcher shutdown() runs:
// nothing happens while no console end is marked; once one is, the shutdown is
// told at once and the exit runs when what is left of the budget — counted
// from the event, not from now — has passed; a shutdown that finishes first is
// left alone.
//
// Mutants: count the budget from the watcher's start instead of the event (the
// late-marked case exits a whole budget later, past its wait); ignore done
// after the mark (the finished-in-time case exits).
func TestConsoleEndExitsWhenItsBudgetRunsOut(t *testing.T) {
	watch := func(c *consoleEnd, done chan struct{}) (hurried, exited, returned chan struct{}) {
		hurried, exited, returned = make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(returned)
			c.exitWhenConsoleEnds(done, func() { close(hurried) }, func() { close(exited) })
		}()
		return
	}
	// almostSpent is a console end marked long enough ago that only d of its
	// budget is left.
	almostSpent := func(d time.Duration) *consoleEnd {
		c := &consoleEnd{}
		c.mark()
		c.at = time.Now().Add(d - consoleEndBudget)
		return c
	}

	t.Run("not marked", func(t *testing.T) {
		c, done := &consoleEnd{}, make(chan struct{})
		hurried, exited, returned := watch(c, done)
		select {
		case <-hurried:
			t.Fatal("hurried with no console end")
		case <-exited:
			t.Fatal("exited with no console end")
		case <-time.After(100 * time.Millisecond):
		}
		close(done)
		<-returned
		select {
		case <-exited:
			t.Fatal("exited when the shutdown finished")
		default:
		}
	})

	t.Run("marked earlier, budget nearly spent", func(t *testing.T) {
		c, done := almostSpent(100*time.Millisecond), make(chan struct{})
		hurried, exited, _ := watch(c, done)
		<-hurried
		select {
		case <-exited:
		case <-time.After(consoleEndBudget / 2):
			t.Fatal("no exit although the budget, counted from the event, was spent")
		}
	})

	t.Run("marked during the shutdown", func(t *testing.T) {
		c, done := &consoleEnd{}, make(chan struct{})
		hurried, exited, returned := watch(c, done)
		c.mark()
		select {
		case <-hurried:
		case <-time.After(2 * time.Second):
			t.Fatal("the shutdown was never told to hurry")
		}
		close(done)
		<-returned
		select {
		case <-exited:
			t.Fatal("exited although the shutdown finished inside the budget")
		default:
		}
	})

	t.Run("budget left, shutdown finishes first", func(t *testing.T) {
		c, done := almostSpent(consoleEndBudget), make(chan struct{})
		hurried, exited, returned := watch(c, done)
		<-hurried
		close(done)
		<-returned
		select {
		case <-exited:
			t.Fatal("exited although the shutdown finished inside the budget")
		default:
		}
	})
}
