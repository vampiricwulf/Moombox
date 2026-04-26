package goja

import (
	"context"
	"fmt"
	"time"

	"github.com/dop251/goja"
)

// NewRuntime creates a new Goja runtime with common setup (encoding, timers, etc.).
func NewRuntime() *goja.Runtime {
	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	return vm
}

// NewRuntimeWithShims creates a new Goja runtime with DOM shims, encoding,
// and timer support. The ctx is threaded into the TimerManager so that
// when ctx cancels, in-flight timer/interval goroutines exit promptly
// rather than waiting for an explicit CancelAll() call. Returns the
// runtime and the TimerManager (caller still owns Shutdown / CancelAll).
//
// Audit reports/goja.md TD-5 — ctx threading.
func NewRuntimeWithShims(ctx context.Context, userAgent string) (*goja.Runtime, *TimerManager, error) {
	vm := NewRuntime()
	tm := NewTimerManagerCtx(ctx, vm)

	if err := RegisterEncoding(vm); err != nil {
		return nil, nil, fmt.Errorf("register encoding: %w", err)
	}
	if err := RegisterTimers(vm, tm); err != nil {
		return nil, nil, fmt.Errorf("register timers: %w", err)
	}
	if err := RegisterDOMShim(vm, userAgent); err != nil {
		return nil, nil, fmt.Errorf("register DOM shim: %w", err)
	}

	return vm, tm, nil
}

// RunStringWithTimeout runs JavaScript source on the given runtime with
// a timeout AND context cancellation. On timeout or ctx-cancel,
// vm.Interrupt is fired to abort the in-flight execution; the helper
// then waits for the launching goroutine to settle and calls
// vm.ClearInterrupt so the runtime stays usable for subsequent
// operations on the same VM. Panics inside the VM are caught and
// surfaced as errors so the caller doesn't need its own
// defer/recover.
//
// Returns:
//   - the value from vm.RunString on success
//   - nil + a "execution timeout" error on timeout
//   - nil + ctx.Err() on context cancellation
//   - nil + the underlying error on RunString failure
//
// Audit reports/goja.md Q1. Replaces the inline goroutine+select+
// Interrupt+ClearInterrupt pattern that BotGuardClient construction
// (and any future caller that wants timeboxed JS execution) was
// hand-rolling.
func RunStringWithTimeout(ctx context.Context, vm *goja.Runtime, code string, timeout time.Duration) (goja.Value, error) {
	type result struct {
		v   goja.Value
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{nil, fmt.Errorf("goja panic: %v", r)}
			}
		}()
		v, runErr := vm.RunString(code)
		done <- result{v: v, err: runErr}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-done:
		return r.v, r.err
	case <-timer.C:
		vm.Interrupt(fmt.Sprintf("execution timeout (%v)", timeout))
		// Wait for the launching goroutine to settle (RunString
		// returns with an interrupt error) before ClearInterrupt so
		// no JS is mid-flight when the flag is dropped.
		<-done
		vm.ClearInterrupt()
		return nil, fmt.Errorf("execution timeout (%v)", timeout)
	case <-ctx.Done():
		vm.Interrupt("context cancelled")
		<-done
		vm.ClearInterrupt()
		return nil, ctx.Err()
	}
}
