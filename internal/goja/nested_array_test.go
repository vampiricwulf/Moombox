package goja

import (
	"context"
	"testing"

	"github.com/dop251/goja"
)

// TestNewArrayPreservesNestedObjectIdentity exercises a contract the BotGuard
// snapshot path depends on: when we build
//
//	argsArr := vm.NewArray(undefined, undefined, webPoSignalOutput, undefined)
//
// the `argsArr[2]` JS value must be the SAME object as `webPoSignalOutput`,
// not a copy. BotGuard writes its getMinter callback to args[2][0]; if the
// nesting copies the array we'd never see the write from the Go side.
//
// Symptom that motivated this test: in production the BotGuard snapshot
// returns a response but `webPoSignalOutput` stays length=0 / has0=false,
// and Google falls back to the websafe-only integrity-token path. Either
// (a) goja loses identity during nesting, or (b) BotGuard never writes.
// This test rules out (a).
func TestNewArrayPreservesNestedObjectIdentity(t *testing.T) {
	vm := NewRuntime()
	inner := vm.NewArray()
	outer := vm.NewArray(goja.Undefined(), inner, goja.Undefined())

	// Read outer[1] back via JS. If it's the same object as `inner`,
	// JS-side .push() should be observable on `inner`.
	if err := vm.Set("__outer", outer); err != nil {
		t.Fatalf("set __outer: %v", err)
	}
	if err := vm.Set("__inner", inner); err != nil {
		t.Fatalf("set __inner: %v", err)
	}

	res, err := vm.RunString(`(function() {
		// Push onto outer[1]; expect inner to observe the push.
		__outer[1].push("via-outer");
		return JSON.stringify({
			outerSlot1Length: __outer[1].length,
			innerLength: __inner.length,
			same: __outer[1] === __inner,
			innerVal: __inner[0],
		});
	})()`)
	if err != nil {
		t.Fatalf("diag JS: %v", err)
	}

	got := res.String()
	want := `{"outerSlot1Length":1,"innerLength":1,"same":true,"innerVal":"via-outer"}`
	if got != want {
		t.Errorf("nested-array identity check:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestArrayWriteFromCallableArg verifies that an array passed to a Callable
// retains writes done by JS code inside that Callable. Mimics the BotGuard
// snapshot contract more directly.
func TestArrayWriteFromCallableArg(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}

	// Define a JS function that takes an array and writes to [0].
	if _, err := vm.RunString(`function writeFirst(arr, val) { arr[0] = val; }`); err != nil {
		t.Fatalf("define writeFirst: %v", err)
	}

	writeFn, ok := goja.AssertFunction(vm.Get("writeFirst"))
	if !ok {
		t.Fatal("writeFirst not callable")
	}

	target := vm.NewArray()
	wrapper := vm.NewArray(goja.Undefined(), goja.Undefined(), target, goja.Undefined())

	// Call writeFirst(wrapper[2], "hello") from Go.
	wrapperSlot2 := wrapper.Get("2")
	if _, err := writeFn(goja.Undefined(), wrapperSlot2, vm.ToValue("hello")); err != nil {
		t.Fatalf("call writeFirst: %v", err)
	}

	// Verify target[0] is now "hello".
	got := target.Get("0")
	if got == nil || got.String() != "hello" {
		t.Errorf("target[0] after write: want \"hello\", got %v", got)
	}
	// And length should be 1.
	if l := target.Get("length"); l == nil || l.ToInteger() != 1 {
		t.Errorf("target.length after write: want 1, got %v", l)
	}
}

// TestArrayWriteFromAsyncCallback verifies an array write done inside a
// queueMicrotask / Promise.then chain settles before subsequent Go-side
// reads. If BotGuard's asyncSnapshot writes to webPoSignalOutput inside a
// promise chain and goja doesn't drain microtasks before our Go-side
// .Get("0") read, we'd see the empty array even though the write happened.
func TestArrayWriteFromAsyncCallback(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}

	// JS function that writes to arr[0] inside a microtask.
	if _, err := vm.RunString(`
		function writeAsync(arr, val) {
			Promise.resolve().then(function() { arr[0] = val; });
		}
	`); err != nil {
		t.Fatalf("define writeAsync: %v", err)
	}

	writeFn, ok := goja.AssertFunction(vm.Get("writeAsync"))
	if !ok {
		t.Fatal("writeAsync not callable")
	}

	target := vm.NewArray()
	if _, err := writeFn(goja.Undefined(), target, vm.ToValue("microtask-write")); err != nil {
		t.Fatalf("call writeAsync: %v", err)
	}

	// At this point, the microtask MIGHT not have drained yet.
	immediateGot := target.Get("0")

	// Force microtask drain by running an empty script (goja drains
	// microtasks at the end of each top-level script).
	if _, err := vm.RunString(""); err != nil {
		t.Fatalf("drain runstring: %v", err)
	}

	afterDrainGot := target.Get("0")

	// Either both are populated (goja drained on the writeFn return) or
	// only the second is. Either way, the post-drain read MUST see the
	// write — otherwise our snapshot path has a microtask-drain gap.
	if afterDrainGot == nil || afterDrainGot.String() != "microtask-write" {
		t.Errorf("target[0] after drain: want \"microtask-write\", got immediate=%v drained=%v", immediateGot, afterDrainGot)
	}
}
