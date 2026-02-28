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

func TestSetTimeoutFires(t *testing.T) {
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

	// Wait for the timer to fire
	time.Sleep(100 * time.Millisecond)
	if fired.Load() != 1 {
		t.Error("expected setTimeout callback to fire")
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
	if fired.Load() != 0 {
		t.Error("expected timer created after CancelAll to not fire")
	}
}

// --- SetInterval tests ---

func TestSetIntervalFires(t *testing.T) {
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

	time.Sleep(150 * time.Millisecond)
	c := count.Load()
	if c < 2 {
		t.Errorf("expected interval to fire at least 2 times, got %d", c)
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
	tm.ClearTimer(id)

	countAtClear := count.Load()
	time.Sleep(100 * time.Millisecond)
	countAfter := count.Load()

	if countAfter != countAtClear {
		t.Errorf("expected no more ticks after clear, had %d at clear and %d after", countAtClear, countAfter)
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
