package goja

import (
	"context"
	"testing"
)

// Day-1 tests for the Option-2 real-DOM rollout: EventTarget + Event +
// dispatch with capture/bubble flags.
//
// These don't yet exercise tree propagation (no Node arrives until
// Day 2); they verify the listener-registry contract on a single
// EventTarget instance.

// TestEventTargetClassExists — sanity that the new class is in scope.
func TestEventTargetClassExists(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	cases := []struct{ expr, want string }{
		{`typeof EventTarget`, "function"},
		{`typeof Event`, "function"},
		{`typeof CustomEvent`, "function"},
		{`typeof MessageEvent`, "function"},
		{`typeof ErrorEvent`, "function"},
		{`new EventTarget() instanceof EventTarget`, "true"},
		{`new Event('x') instanceof Event`, "true"},
		{`new CustomEvent('x') instanceof Event`, "true"},
		{`new MessageEvent('x') instanceof Event`, "true"},
		{`new ErrorEvent('x') instanceof Event`, "true"},
		{`Object.prototype.toString.call(new EventTarget())`, "[object EventTarget]"},
		{`Object.prototype.toString.call(new Event('x'))`, "[object Event]"},
		{`Object.prototype.toString.call(new CustomEvent('x'))`, "[object CustomEvent]"},
		{`Event.AT_TARGET`, "2"},
		{`Event.BUBBLING_PHASE`, "3"},
		{`Event.CAPTURING_PHASE`, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			v, err := vm.RunString(tc.expr)
			if err != nil {
				t.Fatalf("eval %q: %v", tc.expr, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}
}

// TestEventTargetDispatchAndCustomDetail — listener fires, target is
// set, custom event detail propagates.
func TestEventTargetDispatchAndCustomDetail(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const t = new EventTarget();
		const calls = [];
		t.addEventListener('foo', function(e) {
			calls.push({
				type: e.type,
				detail: e.detail,
				targetIsT: e.target === t,
				currentIsT: e.currentTarget === t,
				phase: e.eventPhase,
			});
		});
		t.dispatchEvent(new CustomEvent('foo', { detail: { x: 42 } }));
		return JSON.stringify(calls);
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `[{"type":"foo","detail":{"x":42},"targetIsT":true,"currentIsT":true,"phase":2}]`
	if got := res.String(); got != want {
		t.Errorf("dispatch result\n  got: %s\n  want: %s", got, want)
	}
}

// TestEventTargetOnce — once:true listener fires once and doesn't fire
// on subsequent dispatch.
func TestEventTargetOnce(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const t = new EventTarget();
		let n = 0;
		t.addEventListener('go', function() { n++; }, { once: true });
		t.dispatchEvent(new Event('go'));
		t.dispatchEvent(new Event('go'));
		t.dispatchEvent(new Event('go'));
		return n;
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := res.ToInteger(); got != 1 {
		t.Errorf("once listener fire count = %d, want 1", got)
	}
}

// TestEventTargetRemoveDuringDispatch — listeners removed mid-dispatch
// don't fire; the snapshot taken at dispatch start is what's iterated.
func TestEventTargetRemoveDuringDispatch(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const t = new EventTarget();
		const calls = [];
		const a = function() { calls.push('a'); t.removeEventListener('go', b); };
		const b = function() { calls.push('b'); };
		const c = function() { calls.push('c'); };
		t.addEventListener('go', a);
		t.addEventListener('go', b);
		t.addEventListener('go', c);
		t.dispatchEvent(new Event('go'));
		return calls.join(',');
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	// b registered before dispatch; a removes it during, but the
	// pre-dispatch snapshot still lists b -- BUT b's `removed` flag was
	// set, so the loop skips it. c follows normally.
	want := "a,c"
	if got := res.String(); got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// TestEventTargetStopImmediatePropagation — stops further listener
// invocation on the same target.
func TestEventTargetStopImmediatePropagation(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const t = new EventTarget();
		const calls = [];
		t.addEventListener('go', function(e) { calls.push('a'); e.stopImmediatePropagation(); });
		t.addEventListener('go', function() { calls.push('b'); });
		t.dispatchEvent(new Event('go'));
		return calls.join(',');
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := res.String(); got != "a" {
		t.Errorf("calls after stopImmediatePropagation = %q, want \"a\"", got)
	}
}

// TestEventTargetPreventDefault — defaultPrevented + dispatch return.
func TestEventTargetPreventDefault(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const t = new EventTarget();
		t.addEventListener('go', function(e) { e.preventDefault(); });
		const ret = t.dispatchEvent(new Event('go', { cancelable: true }));
		// preventDefault on a non-cancelable event is a no-op:
		t.addEventListener('nope', function(e) { e.preventDefault(); });
		const ret2 = t.dispatchEvent(new Event('nope'));
		return JSON.stringify({ cancelableRet: ret, nonCancelableRet: ret2 });
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"cancelableRet":false,"nonCancelableRet":true}`
	if got := res.String(); got != want {
		t.Errorf("preventDefault behaviour = %s, want %s", got, want)
	}
}

// TestEventTargetDuplicateListenerIgnored — same (callback, capture) pair
// added twice = single registration per spec.
func TestEventTargetDuplicateListenerIgnored(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const t = new EventTarget();
		let n = 0;
		const fn = function() { n++; };
		t.addEventListener('go', fn);
		t.addEventListener('go', fn);
		t.addEventListener('go', fn, { capture: true });   // distinct capture flag = different entry
		t.dispatchEvent(new Event('go'));
		return n;
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	// First two collapse to one entry. Capture-flag variant is separate.
	// Both fire on AT_TARGET dispatch (capture flag doesn't matter at
	// AT_TARGET phase, both listeners run).
	if got := res.ToInteger(); got != 2 {
		t.Errorf("listener fire count = %d, want 2", got)
	}
}

// TestEventTargetSignalAbortRemovesListener — AbortSignal-based listener
// removal removes the listener when the signal aborts.
//
// Day-1 shipped this with the documented limitation that the test.49
// hand-stub AbortController didn't wire signal-abort, so listeners
// stayed registered. Day 5 replaced AbortController with a real
// EventTarget-based class, so this test now verifies the WHATWG
// signal contract: aborting the controller removes any listener
// registered with {signal: ...}.
func TestEventTargetSignalAbortRemovesListener(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const t = new EventTarget();
		const ctrl = new AbortController();
		let n = 0;
		t.addEventListener('go', function() { n++; }, { signal: ctrl.signal });
		t.dispatchEvent(new Event('go'));    // listener registered, fires
		ctrl.abort();                         // signal abort -> remove listener
		t.dispatchEvent(new Event('go'));    // no listener, no fire
		return JSON.stringify({
			firstFires: n,
			signalIsAborted: ctrl.signal.aborted,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"firstFires":1,"signalIsAborted":true}`
	if got := res.String(); got != want {
		t.Errorf("signal-abort fires = %s, want %s", got, want)
	}
}
