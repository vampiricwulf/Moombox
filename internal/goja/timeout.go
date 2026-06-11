package goja

import (
	"fmt"
	"time"

	"github.com/dop251/goja"
)

// CallWithTimeout invokes fn — which must execute on vm — and bounds it via
// vm.Interrupt after timeout. Shared by the cipher solver and the BotGuard
// minter so the subtle ordering contract lives in exactly one place:
//
// The invocation runs in a dedicated goroutine so that vm.Interrupt (which
// must be called from a non-VM goroutine) can race the completion. On
// timeout, the interrupt is fired and we BLOCK on the result channel until
// the goroutine actually exits — only then is ClearInterrupt called. This
// ordering is what makes the next call on this VM start with a clean slate;
// a fire-and-forget interrupt (the old time.AfterFunc shape) could leave a
// pending interrupt that aborts the NEXT call.
//
// The caller must hold whatever mutex serializes access to vm for the full
// duration of this function — there is no concurrent VM access here, only
// the interrupt flag, which is the one goja API safe to touch from another
// goroutine.
//
// RunStringWithTimeout (runtime.go) is the ctx-aware sibling implementing
// the same interrupt contract. They stay separate deliberately: it needs a
// third select arm for ctx cancellation and returns ctx.Err() / a
// synthesized "execution timeout" error, while this returns fn's own
// interrupt error so callers (cipher, minter) can match on the label.
// Editing the contract in one means editing it in both.
func CallWithTimeout(vm *goja.Runtime, timeout time.Duration, label string, fn func() (goja.Value, error)) (goja.Value, error) {
	type callResult struct {
		val goja.Value
		err error
	}

	done := make(chan callResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- callResult{nil, fmt.Errorf("%s panic inside goroutine: %v", label, r)}
			}
		}()
		v, e := fn()
		done <- callResult{v, e}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var r callResult
	select {
	case r = <-done:
		// Normal path — no interrupt fired. ClearInterrupt below is a no-op
		// but keeps the cleanup symmetric.
	case <-timer.C:
		vm.Interrupt(fmt.Sprintf("%s timeout (%v)", label, timeout))
		r = <-done // wait for the goroutine to observe the interrupt and return
	}
	vm.ClearInterrupt()

	return r.val, r.err
}
