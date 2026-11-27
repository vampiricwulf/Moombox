package goja

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dop251/goja"
)

// MaxCallStackSize is the deepest JavaScript call stack any goja runtime in
// Moombox may reach (owner ruling RV-13). goja's own default is effectively
// unbounded: an endless recursion in a fetched player or BotGuard program grew
// to depth 985,087 and 306 MB in 5 s before anything stopped it. Past this depth
// goja aborts the run with a *goja.StackOverflowError.
//
// JavaScript try/catch does not see that abort: the whole run ends, unlike V8,
// which throws a catchable RangeError. A script that probes its own recursion
// depth is therefore aborted instead of answered.
//
// Measured on Windows x64, goja 5e92a7e2 (2026-11-26): the three testdata
// players compile and solve sig and n with a limit of 20 and fail at 15, so
// the cipher needs fewer than 20 frames. The BotGuard program could not be
// measured (its live test fails for another reason), hence the wide headroom.
// At this limit an endless recursion aborts in about 3 ms having allocated
// about 9 MB; recursing through a native call (Array.prototype.forEach) stops
// at depth 5,000, since each round costs two frames, in about 25 ms and 22 MB.
const MaxCallStackSize = 10000

// NewPlainRuntime creates a bare goja runtime that has the call-stack bound
// and nothing else. Every goja runtime in the tree is made here, test helpers
// included (TestEveryGojaRuntimeIsMadeByNewPlainRuntime holds that), so none
// can run without the bound.
func NewPlainRuntime() *goja.Runtime {
	vm := goja.New()
	vm.SetMaxCallStackSize(MaxCallStackSize)
	return vm
}

// NewRuntime creates a new Goja runtime with common setup (encoding, timers, etc.).
func NewRuntime() *goja.Runtime {
	vm := NewPlainRuntime()
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

// NewRuntimeForCipher creates a Goja runtime suitable for running YouTube
// player.js inside a cipher solver. Compared to NewRuntimeWithShims:
//   - Registers encoding (real TextEncoder/TextDecoder, btoa, atob).
//     The cipher previously hand-stubbed TextEncoder to return Uint8Array(0),
//     which would silently miscompute any signature touching TextEncoder
//     (audit reports/goja.md API6 + cipher.md D1/D4).
//   - Registers DOM shim (document, navigator, location, screen, performance,
//     storage, XMLHttpRequest, crypto.{getRandomValues,subtle,randomUUID},
//     plus AbortController / ReadableStream / CustomEvent / CSS / Intl).
//   - Does NOT register timers — cipher uses vm.Interrupt() externally for
//     timeout enforcement; setTimeout in player.js init paths becomes a
//     ReferenceError but those paths aren't hit during sig/n extraction.
//
// Audit reports/goja.md C4 + reports/cipher.md D1/D4.
func NewRuntimeForCipher(userAgent string) (*goja.Runtime, error) {
	vm := NewRuntime()

	if err := RegisterEncoding(vm); err != nil {
		return nil, fmt.Errorf("register encoding: %w", err)
	}
	if err := RegisterDOMShim(vm, userAgent); err != nil {
		return nil, fmt.Errorf("register DOM shim: %w", err)
	}

	return vm, nil
}

// ErrExecutionTimeout is what RunStringWithTimeout's error wraps when the
// bound ran out, so a caller can tell a timeout from a script error or a
// cancelled context with errors.Is. The message is unchanged:
// "execution timeout (<bound>)".
var ErrExecutionTimeout = errors.New("execution timeout")

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
//   - nil + an error wrapping ErrExecutionTimeout on timeout
//   - nil + ctx.Err() on context cancellation
//   - nil + the underlying error on RunString failure
//
// Audit reports/goja.md Q1. Replaces the inline goroutine+select+
// Interrupt+ClearInterrupt pattern that BotGuardClient construction
// (and any future caller that wants timeboxed JS execution) was
// hand-rolling. CallWithTimeout (timeout.go) is the ctx-less sibling
// with a different error contract — see its godoc for why the two
// stay separate; edits to the interrupt ordering go in both.
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
		return nil, fmt.Errorf("%w (%v)", ErrExecutionTimeout, timeout)
	case <-ctx.Done():
		vm.Interrupt("context cancelled")
		<-done
		vm.ClearInterrupt()
		return nil, ctx.Err()
	}
}
