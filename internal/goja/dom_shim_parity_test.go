package goja

import (
	"context"
	"testing"
	"time"
)

// TestShimBrowserParity pins the shim answers a browser gives where the stubs
// used to differ:
//
//   - Function.prototype.toString.call(stub) bypassed the stubs' own
//     toString and printed the Go module path for setTimeout (mutant: restore
//     the per-function own toString);
//   - a stub carried an own toString no real built-in has;
//   - Storage over a plain {} answered getItem("constructor") with Object's
//     constructor and could not store "__proto__" (mutant: var data = {}).
func TestShimBrowserParity(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	cases := []struct{ expr, want string }{
		{`Function.prototype.toString.call(setTimeout)`, "function setTimeout() { [native code] }"},
		{`String(setTimeout)`, "function setTimeout() { [native code] }"},
		{`Function.prototype.toString.call(addEventListener)`, "function addEventListener() { [native code] }"},
		{`Object.prototype.hasOwnProperty.call(setTimeout, 'toString')`, "false"},
		{`Function.prototype.toString.call(Function.prototype.toString)`, "function toString() { [native code] }"},
		{`(function f() { return 42; }).toString().indexOf('return 42') >= 0`, "true"},
		{`String(localStorage.getItem('constructor'))`, "null"},
		{`localStorage.setItem('__proto__', 'x'); localStorage.getItem('__proto__') + '/' + localStorage.length`, "x/1"},
		{`sessionStorage.setItem('', 'e'); JSON.stringify(sessionStorage.key(0))`, `""`},
		{`String(sessionStorage.key(5))`, "null"},
	}
	for _, tc := range cases {
		v, err := vm.RunString(tc.expr)
		if err != nil {
			t.Errorf("eval %q: %v", tc.expr, err)
			continue
		}
		if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.expr, got, tc.want)
		}
	}
}

// TestEncodingAndTimerBrowserParity pins three encoding answers and the timer
// argument rule the shims got wrong:
//
//   - atob refused ASCII whitespace a browser strips (mutant: decode the raw
//     argument);
//   - TextDecoder kept a leading BOM a browser consumes (mutant: drop the BOM
//     skip), and TextEncoder had no encoding property;
//   - setTimeout(cb, 0, a, b) called cb() without a and b (mutant: queue fn
//     itself in RegisterTimers).
func TestEncodingAndTimerBrowserParity(t *testing.T) {
	vm, tm, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	defer tm.CancelAll()
	cases := []struct{ expr, want string }{
		{`atob("aGVs bG8=")`, "hello"},
		{`atob("aGVs\tbG8=\n")`, "hello"},
		{`new TextDecoder().decode(new Uint8Array([0xEF, 0xBB, 0xBF, 0x41]))`, "A"},
		{`new TextDecoder('utf-8', { ignoreBOM: true }).decode(new Uint8Array([0xEF, 0xBB, 0xBF, 0x41])).length`, "2"},
		{`new TextEncoder().encoding`, "utf-8"},
	}
	for _, tc := range cases {
		v, err := vm.RunString(tc.expr)
		if err != nil {
			t.Errorf("eval %q: %v", tc.expr, err)
			continue
		}
		if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.expr, got, tc.want)
		}
	}

	if _, err := vm.RunString(`var got = ''; setTimeout(function(a, b) { got = a + b; }, 0, 'x', 'y');`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := tm.DrainCallbacks(); err != nil {
			t.Fatal(err)
		}
		if v := vm.Get("got"); v != nil && v.String() == "xy" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("setTimeout's extra arguments never reached the callback (got = %q)", vm.Get("got"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
