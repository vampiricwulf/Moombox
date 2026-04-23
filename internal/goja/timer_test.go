package goja

import (
	"sync/atomic"
	"testing"
	"time"

	gojalib "github.com/dop251/goja"
)

// --- NewTimerManager tests ---

func TestNewTimerManager(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	if tm == nil {
		t.Fatal("expected non-nil TimerManager")
	}
	if tm.ActiveCount() != 0 {
		t.Errorf("expected 0 active timers, got %d", tm.ActiveCount())
	}
}

// --- SetTimeout tests ---

func TestSetTimeoutEnqueuesCallback(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	var fired atomic.Int32
	fn := func(call gojalib.FunctionCall) gojalib.Value {
		fired.Store(1)
		return gojalib.Undefined()
	}
	callable, ok := gojalib.AssertFunction(vm.ToValue(fn))
	if !ok {
		t.Fatal("expected function assertion to succeed")
	}

	id := tm.SetTimeout(callable, 10)
	if id <= 0 {
		t.Errorf("expected positive timer ID, got %d", id)
	}

	// Wait for the timer to fire and enqueue the callback
	time.Sleep(100 * time.Millisecond)

	// Callback should NOT have been called yet (queued, not executed)
	if fired.Load() != 0 {
		t.Error("expected callback to NOT fire directly from goroutine")
	}

	// Drain callbacks on the VM thread (this test goroutine)
	n, err := tm.DrainCallbacks()
	if err != nil {
		t.Fatalf("unexpected error draining callbacks: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 callback drained, got %d", n)
	}
	if fired.Load() != 1 {
		t.Error("expected callback to fire after DrainCallbacks")
	}
}

func TestSetTimeoutReturnsUniqueIDs(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	fn := func(call gojalib.FunctionCall) gojalib.Value {
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	id1 := tm.SetTimeout(callable, 1000)
	id2 := tm.SetTimeout(callable, 1000)
	if id1 == id2 {
		t.Errorf("expected unique IDs, got %d and %d", id1, id2)
	}
}

func TestSetTimeoutNegativeDelay(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	var fired atomic.Int32
	fn := func(call gojalib.FunctionCall) gojalib.Value {
		fired.Store(1)
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	// Negative delay should be treated as 0 (fire immediately)
	tm.SetTimeout(callable, -100)
	time.Sleep(50 * time.Millisecond)

	// Drain and verify
	tm.DrainCallbacks()
	if fired.Load() != 1 {
		t.Error("expected setTimeout with negative delay to fire immediately")
	}
}

// --- ClearTimer tests ---

func TestClearTimerPreventsCallback(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	var fired atomic.Int32
	fn := func(call gojalib.FunctionCall) gojalib.Value {
		fired.Store(1)
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	id := tm.SetTimeout(callable, 200)
	tm.ClearTimer(id)

	time.Sleep(300 * time.Millisecond)
	tm.DrainCallbacks()
	if fired.Load() != 0 {
		t.Error("expected cleared timer to not fire")
	}
}

func TestClearTimerNonExistentID(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	// Should not panic when clearing a non-existent timer
	tm.ClearTimer(99999)
}

func TestClearTimerDecreasesActiveCount(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	fn := func(call gojalib.FunctionCall) gojalib.Value {
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	id := tm.SetTimeout(callable, 5000)
	if tm.ActiveCount() != 1 {
		t.Errorf("expected 1 active timer, got %d", tm.ActiveCount())
	}

	tm.ClearTimer(id)
	if tm.ActiveCount() != 0 {
		t.Errorf("expected 0 active timers after clear, got %d", tm.ActiveCount())
	}
}

// --- CancelAll tests ---

func TestCancelAllStopsAllTimers(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)

	var count atomic.Int32
	fn := func(call gojalib.FunctionCall) gojalib.Value {
		count.Add(1)
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	tm.SetTimeout(callable, 200)
	tm.SetTimeout(callable, 200)
	tm.SetTimeout(callable, 200)

	if tm.ActiveCount() != 3 {
		t.Errorf("expected 3 active timers, got %d", tm.ActiveCount())
	}

	tm.CancelAll()

	if tm.ActiveCount() != 0 {
		t.Errorf("expected 0 active timers after CancelAll, got %d", tm.ActiveCount())
	}

	time.Sleep(300 * time.Millisecond)
	tm.DrainCallbacks()
	if count.Load() != 0 {
		t.Errorf("expected no callbacks after CancelAll, got %d", count.Load())
	}
}

func TestCancelAllPreventsNewTimers(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)

	var fired atomic.Int32
	fn := func(call gojalib.FunctionCall) gojalib.Value {
		fired.Store(1)
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	tm.CancelAll()

	// After CancelAll, new timers should not fire (stopped=true)
	tm.SetTimeout(callable, 10)

	time.Sleep(100 * time.Millisecond)
	tm.DrainCallbacks()
	if fired.Load() != 0 {
		t.Error("expected timer created after CancelAll to not fire")
	}
}

func TestCancelAllClearsPendingCallbacks(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)

	var count atomic.Int32
	fn := func(call gojalib.FunctionCall) gojalib.Value {
		count.Add(1)
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	// Schedule timers that will fire quickly
	tm.SetTimeout(callable, 5)
	tm.SetTimeout(callable, 5)

	// Wait for them to enqueue
	time.Sleep(50 * time.Millisecond)

	// CancelAll should clear any pending callbacks
	tm.CancelAll()

	n, _ := tm.DrainCallbacks()
	if n != 0 {
		t.Errorf("expected 0 callbacks after CancelAll, drained %d", n)
	}
	if count.Load() != 0 {
		t.Errorf("expected count 0 after CancelAll, got %d", count.Load())
	}
}

// --- SetInterval tests ---

func TestSetIntervalEnqueuesCallbacks(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	var count atomic.Int32
	fn := func(call gojalib.FunctionCall) gojalib.Value {
		count.Add(1)
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	tm.SetInterval(callable, 20)

	// Wait for several ticks to enqueue
	time.Sleep(150 * time.Millisecond)

	// Drain all queued callbacks
	n, err := tm.DrainCallbacks()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n < 2 {
		t.Errorf("expected at least 2 callbacks drained, got %d", n)
	}
	if count.Load() < 2 {
		t.Errorf("expected interval to fire at least 2 times, got %d", count.Load())
	}
}

func TestSetIntervalClearedByID(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	var count atomic.Int32
	fn := func(call gojalib.FunctionCall) gojalib.Value {
		count.Add(1)
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	id := tm.SetInterval(callable, 20)
	time.Sleep(100 * time.Millisecond)
	// Clear before draining so we flush any in-flight callback that raced
	// with the clear — the contract is "no callbacks after clear+drain",
	// not "no callbacks after clear alone" (which is impossible to
	// guarantee without holding both tm.mu and callbackMu during Clear).
	tm.ClearTimer(id)
	tm.DrainCallbacks()

	countAtClear := count.Load()
	time.Sleep(100 * time.Millisecond)
	tm.DrainCallbacks()
	countAfter := count.Load()

	if countAfter != countAtClear {
		t.Errorf("expected no more ticks after clear, had %d at clear and %d after", countAtClear, countAfter)
	}
}

func TestSetIntervalZeroDelay(t *testing.T) {
	// Verify that 0ms delay is clamped to 1ms (not a tight loop)
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	var count atomic.Int32
	fn := func(call gojalib.FunctionCall) gojalib.Value {
		count.Add(1)
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	tm.SetInterval(callable, 0)

	// With 1ms clamping, should get many ticks but not an unbounded tight loop.
	// A tight loop would yield millions; 1ms clamping yields ~50 in 50ms.
	time.Sleep(50 * time.Millisecond)
	tm.DrainCallbacks()

	c := count.Load()
	if c < 5 {
		t.Errorf("expected at least 5 ticks with 0ms (clamped to 1ms) interval in 50ms, got %d", c)
	}
	// Sanity upper bound — if it were a true tight loop this would be millions
	if c > 1000 {
		t.Errorf("expected fewer than 1000 ticks (tight loop protection), got %d", c)
	}
}

// --- ActiveCount tests ---

func TestActiveCountTracking(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	fn := func(call gojalib.FunctionCall) gojalib.Value {
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	if tm.ActiveCount() != 0 {
		t.Errorf("expected 0 initially, got %d", tm.ActiveCount())
	}

	id1 := tm.SetTimeout(callable, 5000)
	if tm.ActiveCount() != 1 {
		t.Errorf("expected 1 after first timer, got %d", tm.ActiveCount())
	}

	id2 := tm.SetInterval(callable, 5000)
	if tm.ActiveCount() != 2 {
		t.Errorf("expected 2 after adding interval, got %d", tm.ActiveCount())
	}

	tm.ClearTimer(id1)
	if tm.ActiveCount() != 1 {
		t.Errorf("expected 1 after clearing one, got %d", tm.ActiveCount())
	}

	tm.ClearTimer(id2)
	if tm.ActiveCount() != 0 {
		t.Errorf("expected 0 after clearing all, got %d", tm.ActiveCount())
	}
}

// --- HasPendingCallbacks tests ---

func TestHasPendingCallbacks(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	if tm.HasPendingCallbacks() {
		t.Error("expected no pending callbacks initially")
	}

	fn := func(call gojalib.FunctionCall) gojalib.Value {
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	tm.SetTimeout(callable, 5)
	time.Sleep(50 * time.Millisecond)

	if !tm.HasPendingCallbacks() {
		t.Error("expected pending callbacks after timer fires")
	}

	tm.DrainCallbacks()
	if tm.HasPendingCallbacks() {
		t.Error("expected no pending callbacks after drain")
	}
}

// --- DrainCallbacks error handling ---

func TestDrainCallbacksReportsErrors(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)
	defer tm.CancelAll()

	// Register a JS function that throws
	_, err := vm.RunString("function throwingFn() { throw new Error('test error'); }")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	callable, ok := gojalib.AssertFunction(vm.Get("throwingFn"))
	if !ok {
		t.Fatal("expected throwingFn to be a function")
	}

	tm.SetTimeout(callable, 5)
	time.Sleep(50 * time.Millisecond)

	_, err = tm.DrainCallbacks()
	if err == nil {
		t.Error("expected error from DrainCallbacks when callback throws")
	}
}

// --- Concurrency safety ---

func TestTimerConcurrentCreateAndCancel(t *testing.T) {
	vm := gojalib.New()
	tm := NewTimerManager(vm)

	fn := func(call gojalib.FunctionCall) gojalib.Value {
		return gojalib.Undefined()
	}
	callable, _ := gojalib.AssertFunction(vm.ToValue(fn))

	// Create many timers concurrently
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic in concurrent timer creation: %v", r)
			}
		}()
		for range 100 {
			tm.SetTimeout(callable, 1000)
			tm.SetInterval(callable, 1000)
		}
		close(done)
	}()

	<-done
	tm.CancelAll()
}
