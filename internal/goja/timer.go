package goja

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

// maxTimerDelayMs caps the JS-side setTimeout/setInterval delay argument so
// that delayMs * time.Millisecond (1e6 ns) can't overflow time.Duration
// (int64 nanoseconds). A malicious/bugged script passing math.MaxInt64 would
// otherwise produce a near-zero or negative duration and fire immediately.
const maxTimerDelayMs = math.MaxInt64 / int64(time.Millisecond)

// TimerManager tracks setTimeout/setInterval calls for cleanup.
//
// IMPORTANT: Goja runtimes are NOT goroutine-safe. Timer callbacks are queued
// and must be drained on the same goroutine that owns the VM via DrainCallbacks().
type TimerManager struct {
	vm      *goja.Runtime
	logger  timerLogger // may be nil
	mu      sync.Mutex
	timers  map[int64]*timerEntry
	nextID  atomic.Int64
	stopped bool
	done    chan struct{} // closed by CancelAll to unblock interval goroutines

	// Callback queue — timer/interval callbacks are enqueued here instead of
	// being called directly from goroutines (Goja is single-threaded).
	callbackMu sync.Mutex
	callbacks  []goja.Callable
}

// timerLogger captures the minimal logging surface used by TimerManager for
// goroutine-boundary diagnostics. Same shape as the loggers plumbed elsewhere
// in the package; nil-safe.
type timerLogger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type timerEntry struct {
	id     int64
	timer  *time.Timer
	ticker *time.Ticker
	done   chan struct{} // closed on ClearTimer to stop the interval goroutine
}

// clampDelayMs bounds delayMs to [0, maxTimerDelayMs]. Caller then multiplies
// by time.Millisecond safely.
func clampDelayMs(delayMs int64) int64 {
	if delayMs < 0 {
		return 0
	}
	if delayMs > maxTimerDelayMs {
		return maxTimerDelayMs
	}
	return delayMs
}

// NewTimerManager creates a timer manager for a Goja runtime.
func NewTimerManager(vm *goja.Runtime) *TimerManager {
	return &TimerManager{
		vm:     vm,
		timers: make(map[int64]*timerEntry),
		done:   make(chan struct{}),
	}
}

// SetLogger wires an optional logger used for goroutine-boundary diagnostics
// (timer panics, interval-callback errors). nil disables logging.
func (tm *TimerManager) SetLogger(l timerLogger) {
	tm.logger = l
}

func (tm *TimerManager) logWarn(msg string, args ...any) {
	if tm.logger != nil {
		tm.logger.Warn(msg, args...)
	}
}

// enqueueCallback safely queues a callback for later execution on the VM thread.
func (tm *TimerManager) enqueueCallback(fn goja.Callable) {
	if fn == nil {
		return
	}
	tm.callbackMu.Lock()
	tm.callbacks = append(tm.callbacks, fn)
	tm.callbackMu.Unlock()
}

// DrainCallbacks executes all queued timer callbacks on the calling goroutine.
// This MUST be called from the goroutine that owns the Goja runtime.
// Returns the number of callbacks executed and the first error encountered (if any).
func (tm *TimerManager) DrainCallbacks() (int, error) {
	tm.callbackMu.Lock()
	if len(tm.callbacks) == 0 {
		tm.callbackMu.Unlock()
		return 0, nil
	}
	// Swap out the queue so we don't hold the lock during execution
	cbs := tm.callbacks
	tm.callbacks = nil
	tm.callbackMu.Unlock()

	var firstErr error
	for _, fn := range cbs {
		// Per-callback panic recovery: one buggy callback must not abort the
		// drain pass, otherwise any remaining callbacks are lost.
		func() {
			defer func() {
				if r := recover(); r != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("timer callback panic: %v", r)
					}
				}
			}()
			if _, err := fn(goja.Undefined()); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("timer callback: %w", err)
			}
		}()
	}
	return len(cbs), firstErr
}

// HasPendingCallbacks returns true if there are queued callbacks waiting to be drained.
func (tm *TimerManager) HasPendingCallbacks() bool {
	tm.callbackMu.Lock()
	defer tm.callbackMu.Unlock()
	return len(tm.callbacks) > 0
}

// SetTimeout schedules a one-shot timer. Returns timer ID.
// The callback is queued for execution via DrainCallbacks() — it is NOT called
// directly from the timer goroutine (Goja is not goroutine-safe).
func (tm *TimerManager) SetTimeout(fn goja.Callable, delayMs int64) int64 {
	id := tm.nextID.Add(1)

	tm.mu.Lock()
	if tm.stopped {
		tm.mu.Unlock()
		return id
	}

	delay := time.Duration(clampDelayMs(delayMs)) * time.Millisecond

	t := time.AfterFunc(delay, func() {
		defer func() {
			if r := recover(); r != nil {
				// Timer goroutine panic recovery — route through the logger
				// if one is set (nil-safe), since we can't access the Goja
				// VM from this goroutine.
				tm.logWarn("goja: panic in setTimeout cleanup", "id", id, "panic", r)
			}
		}()

		tm.mu.Lock()
		_, exists := tm.timers[id]
		if exists {
			delete(tm.timers, id)
		}
		tm.mu.Unlock()

		if exists {
			tm.enqueueCallback(fn)
		}
	})

	tm.timers[id] = &timerEntry{id: id, timer: t}
	tm.mu.Unlock()
	return id
}

// SetInterval schedules a repeating timer. Returns timer ID.
// Callbacks are queued for execution via DrainCallbacks() — they are NOT called
// directly from the ticker goroutine (Goja is not goroutine-safe).
func (tm *TimerManager) SetInterval(fn goja.Callable, delayMs int64) int64 {
	id := tm.nextID.Add(1)

	tm.mu.Lock()
	if tm.stopped {
		tm.mu.Unlock()
		return id
	}

	delay := time.Duration(clampDelayMs(delayMs)) * time.Millisecond
	if delay <= 0 {
		delay = 1 * time.Millisecond
	}

	ticker := time.NewTicker(delay)
	entryDone := make(chan struct{})
	entry := &timerEntry{id: id, ticker: ticker, done: entryDone}
	tm.timers[id] = entry
	tm.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				tm.logWarn("goja: panic in setInterval goroutine", "id", id, "panic", r)
			}
		}()

		for {
			select {
			case <-tm.done:
				return
			case <-entryDone:
				return
			case _, ok := <-ticker.C:
				if !ok {
					return
				}
				// Check-and-enqueue under tm.mu so ClearTimer can't fire
				// between the existence check and the enqueue and leave a
				// callback in the queue for a timer that was just cleared.
				// Lock order (tm.mu → callbackMu) matches CancelAll.
				tm.mu.Lock()
				_, exists := tm.timers[id]
				if exists && fn != nil {
					tm.callbackMu.Lock()
					tm.callbacks = append(tm.callbacks, fn)
					tm.callbackMu.Unlock()
				}
				tm.mu.Unlock()
				if !exists {
					return
				}
			}
		}
	}()

	return id
}

// ClearTimer cancels a timer or interval by ID.
func (tm *TimerManager) ClearTimer(id int64) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	entry, ok := tm.timers[id]
	if !ok {
		return
	}

	if entry.timer != nil {
		entry.timer.Stop()
	}
	if entry.ticker != nil {
		entry.ticker.Stop()
	}
	if entry.done != nil {
		close(entry.done)
	}
	delete(tm.timers, id)
}

// CancelAll cancels all active timers and intervals.
func (tm *TimerManager) CancelAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Signal interval goroutines to exit FIRST — this prevents any in-flight
	// tick from enqueuing a new callback after we've cleared the timer map.
	select {
	case <-tm.done:
		// Already closed
	default:
		close(tm.done)
	}

	tm.stopped = true
	for _, entry := range tm.timers {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		if entry.ticker != nil {
			entry.ticker.Stop()
		}
		if entry.done != nil {
			close(entry.done)
		}
	}
	tm.timers = make(map[int64]*timerEntry)

	// Clear any pending callbacks that were queued before cancellation
	tm.callbackMu.Lock()
	tm.callbacks = nil
	tm.callbackMu.Unlock()
}

// ActiveCount returns the number of active timers.
func (tm *TimerManager) ActiveCount() int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return len(tm.timers)
}

// RegisterTimers registers setTimeout/setInterval/clearTimeout/clearInterval on a Goja runtime.
func RegisterTimers(vm *goja.Runtime, tm *TimerManager) error {
	err := vm.Set("setTimeout", func(call goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			return goja.Undefined()
		}
		delay := call.Argument(1).ToInteger()
		id := tm.SetTimeout(fn, delay)
		return vm.ToValue(id)
	})
	if err != nil {
		return err
	}

	err = vm.Set("setInterval", func(call goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			return goja.Undefined()
		}
		delay := call.Argument(1).ToInteger()
		id := tm.SetInterval(fn, delay)
		return vm.ToValue(id)
	})
	if err != nil {
		return err
	}

	err = vm.Set("clearTimeout", func(call goja.FunctionCall) goja.Value {
		id := call.Argument(0).ToInteger()
		tm.ClearTimer(id)
		return goja.Undefined()
	})
	if err != nil {
		return err
	}

	err = vm.Set("clearInterval", func(call goja.FunctionCall) goja.Value {
		id := call.Argument(0).ToInteger()
		tm.ClearTimer(id)
		return goja.Undefined()
	})
	if err != nil {
		return err
	}

	return nil
}
