package cookies

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// panicOnDebugLogger panics on Debug — the first thing a tick with no browser
// profile does — and counts Error, where the per-tick recover reports.
type panicOnDebugLogger struct{ errors atomic.Int32 }

func (l *panicOnDebugLogger) Debug(string, ...any) { panic("synthetic panic inside a periodic tick") }
func (l *panicOnDebugLogger) Info(string, ...any)  {}
func (l *panicOnDebugLogger) Warn(string, ...any)  {}
func (l *panicOnDebugLogger) Error(string, ...any) { l.errors.Add(1) }

// TestPeriodicTickPanicCostsOneTick: StartPeriodicRefresh's goroutine recover
// sits outside its loop, so a panic reaching it ends the 30-minute browser
// refresh for the life of the process. runPeriodicTick gives each tick its
// own recover.
//
// Mutant: call periodicTick bare in runPeriodicTick — the panic escapes this
// call and fails the test.
func TestPeriodicTickPanicCostsOneTick(t *testing.T) {
	log := &panicOnDebugLogger{}
	s := NewAutoCookieService("", "", nil, log)
	s.runPeriodicTick(context.Background(), time.Hour)
	s.runPeriodicTick(context.Background(), time.Hour)
	if got := log.errors.Load(); got != 2 {
		t.Errorf("Error calls = %d, want 2 — each panicking tick is recovered and reported", got)
	}
}
