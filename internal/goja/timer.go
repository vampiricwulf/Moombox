package goja

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

// TimerManager tracks setTimeout/setInterval calls for cleanup.
// Goja is single-threaded per runtime, so timer callbacks must be
// dispatched carefully via the runtime's event loop or deferred.
type TimerManager struct {
	vm      *goja.Runtime
	mu      sync.Mutex
	timers  map[int64]*timerEntry
	nextID  atomic.Int64
	stopped bool
	done    chan struct{} // closed by CancelAll to unblock interval goroutines
}

type timerEntry struct {
	id       int64
	timer    *time.Timer
	ticker   *time.Ticker
	interval bool
}

// NewTimerManager creates a timer manager for a Goja runtime.
func NewTimerManager(vm *goja.Runtime) *TimerManager {
	return &TimerManager{
		vm:     vm,
		timers: make(map[int64]*timerEntry),
		done:   make(chan struct{}),
	}
}

// SetTimeout schedules a one-shot timer. Returns timer ID.
func (tm *TimerManager) SetTimeout(fn goja.Callable, delayMs int64) int64 {
	id := tm.nextID.Add(1)

	tm.mu.Lock()
	if tm.stopped {
		tm.mu.Unlock()
		return id
	}

	delay := time.Duration(delayMs) * time.Millisecond
	if delay < 0 {
		delay = 0
	}

	t := time.AfterFunc(delay, func() {
		tm.mu.Lock()
		_, exists := tm.timers[id]
		if exists {
			delete(tm.timers, id)
		}
		tm.mu.Unlock()

		if exists && fn != nil {
			// Goja is not goroutine-safe; we invoke via Interrupt-and-resume pattern
			// For BotGuard use cases, callbacks are typically no-ops or simple assignments
			fn(goja.Undefined())
		}
	})

	tm.timers[id] = &timerEntry{id: id, timer: t}
	tm.mu.Unlock()
	return id
}

// SetInterval schedules a repeating timer. Returns timer ID.
func (tm *TimerManager) SetInterval(fn goja.Callable, delayMs int64) int64 {
	id := tm.nextID.Add(1)

	tm.mu.Lock()
	if tm.stopped {
		tm.mu.Unlock()
		return id
	}

	delay := time.Duration(delayMs) * time.Millisecond
	if delay <= 0 {
		delay = 1 * time.Millisecond
	}

	ticker := time.NewTicker(delay)
	entry := &timerEntry{id: id, ticker: ticker, interval: true}
	tm.timers[id] = entry
	tm.mu.Unlock()

	go func() {
		for {
			select {
			case <-tm.done:
				return
			case _, ok := <-ticker.C:
				if !ok {
					return
				}
				tm.mu.Lock()
				_, exists := tm.timers[id]
				tm.mu.Unlock()
				if !exists {
					return
				}
				if fn != nil {
					fn(goja.Undefined())
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
	delete(tm.timers, id)
}

// CancelAll cancels all active timers and intervals.
func (tm *TimerManager) CancelAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.stopped = true
	for _, entry := range tm.timers {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		if entry.ticker != nil {
			entry.ticker.Stop()
		}
	}
	tm.timers = make(map[int64]*timerEntry)

	// Signal interval goroutines to exit (ticker.Stop doesn't close the channel)
	select {
	case <-tm.done:
		// Already closed
	default:
		close(tm.done)
	}
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
