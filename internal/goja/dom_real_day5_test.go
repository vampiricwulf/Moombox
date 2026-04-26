package goja

import (
	"context"
	"testing"
)

// Day-5 tests: DOMTokenList (classList), URL/URLSearchParams polyfill,
// real AbortController, dataset proxy.

// TestClassList — Element.classList round-trip.
func TestClassList(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const e = document.createElement('div');
		e.className = 'a b';
		e.classList.add('c');
		const beforeRemove = e.className;
		e.classList.remove('a');
		const afterRemove = e.className;
		const toggled1 = e.classList.toggle('d');
		const toggled2 = e.classList.toggle('b');
		const replaced = e.classList.replace('c', 'cc');
		return JSON.stringify({
			beforeRemove: beforeRemove,
			afterRemove: afterRemove,
			toggled1: toggled1,
			toggled2: toggled2,
			finalClass: e.className,
			contains_d: e.classList.contains('d'),
			length: e.classList.length,
			replaced: replaced,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"beforeRemove":"a b c","afterRemove":"b c","toggled1":true,"toggled2":false,"finalClass":"cc d","contains_d":true,"length":2,"replaced":true}`
	if got := res.String(); got != want {
		t.Errorf("classList:\n  got: %s\n  want: %s", got, want)
	}
}

// TestURLClass — URL parsing + serialisation.
func TestURLClass(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const u = new URL('https://www.youtube.com:443/watch?v=abc&t=10s#frag');
		return JSON.stringify({
			href: u.href,
			origin: u.origin,
			protocol: u.protocol,
			hostname: u.hostname,
			port: u.port,
			pathname: u.pathname,
			search: u.search,
			hash: u.hash,
			toStr: String(u),
			isURL: u instanceof URL,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"href":"https://www.youtube.com:443/watch?v=abc&t=10s#frag","origin":"https://www.youtube.com:443","protocol":"https:","hostname":"www.youtube.com","port":"443","pathname":"/watch","search":"?v=abc&t=10s","hash":"#frag","toStr":"https://www.youtube.com:443/watch?v=abc&t=10s#frag","isURL":true}`
	if got := res.String(); got != want {
		t.Errorf("URL:\n  got: %s\n  want: %s", got, want)
	}
}

// TestURLBaseRelative — URL constructor honours base parameter.
func TestURLBaseRelative(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const base = 'https://www.youtube.com/embed/';
		return JSON.stringify({
			abs: new URL('http://x.com/y', base).href,
			pathRel: new URL('/watch', base).href,
			rel: new URL('foo.html', base).href,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"abs":"http://x.com/y","pathRel":"https://www.youtube.com/watch","rel":"https://www.youtube.com/embed/foo.html"}`
	if got := res.String(); got != want {
		t.Errorf("URL base:\n  got: %s\n  want: %s", got, want)
	}
}

// TestURLSearchParamsClass — query-string round-trip.
func TestURLSearchParamsClass(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const sp = new URLSearchParams('a=1&b=2&a=3');
		const collected = [];
		sp.forEach((v, k) => { collected.push(k + '=' + v); });
		return JSON.stringify({
			get_a: sp.get('a'),
			getAll_a: sp.getAll('a'),
			has_b: sp.has('b'),
			has_x: sp.has('x'),
			collected: collected,
			toString: sp.toString(),
			size: sp.size,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"get_a":"1","getAll_a":["1","3"],"has_b":true,"has_x":false,"collected":["a=1","b=2","a=3"],"toString":"a=1&b=2&a=3","size":3}`
	if got := res.String(); got != want {
		t.Errorf("URLSearchParams:\n  got: %s\n  want: %s", got, want)
	}
}

// TestAbortControllerEventTarget — AbortSignal extends EventTarget,
// abort() fires the abort event.
func TestAbortControllerEventTarget(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const ctrl = new AbortController();
		let abortFiredCount = 0;
		ctrl.signal.addEventListener('abort', () => { abortFiredCount++; });
		ctrl.signal.onabort = () => { abortFiredCount += 10; };
		const beforeAbort = ctrl.signal.aborted;
		ctrl.abort('test reason');
		const afterAbort = ctrl.signal.aborted;
		ctrl.abort();   // second abort is a no-op
		return JSON.stringify({
			beforeAbort: beforeAbort,
			afterAbort: afterAbort,
			signalIsEventTarget: ctrl.signal instanceof EventTarget,
			signalIsAbortSignal: ctrl.signal instanceof AbortSignal,
			abortFiredCount: abortFiredCount,
			reason: String(ctrl.signal.reason),
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"beforeAbort":false,"afterAbort":true,"signalIsEventTarget":true,"signalIsAbortSignal":true,"abortFiredCount":11,"reason":"test reason"}`
	if got := res.String(); got != want {
		t.Errorf("AbortController:\n  got: %s\n  want: %s", got, want)
	}
}

// TestDatasetProxy — el.dataset.foo mirrors el.attributes['data-foo'].
func TestDatasetProxy(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const e = document.createElement('div');
		e.dataset.fooBar = 'hi';
		e.dataset.x = '1';
		return JSON.stringify({
			fooBar: e.dataset.fooBar,
			fooBarAttr: e.getAttribute('data-foo-bar'),
			x: e.dataset.x,
			xAttr: e.getAttribute('data-x'),
			has: 'fooBar' in e.dataset,
			hasMissing: 'missing' in e.dataset,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"fooBar":"hi","fooBarAttr":"hi","x":"1","xAttr":"1","has":true,"hasMissing":false}`
	if got := res.String(); got != want {
		t.Errorf("dataset:\n  got: %s\n  want: %s", got, want)
	}
}
