package goja

import (
	"context"
	"testing"
)

// Day-2 tests for the Option-2 real-DOM rollout: Node tree + Element +
// HTMLElement subclasses + tree-aware dispatchEvent capture/bubble.

// TestNodeClassExists — sanity that the Node hierarchy is in scope.
func TestNodeClassExists(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	cases := []struct{ expr, want string }{
		{`typeof Node`, "function"},
		{`typeof Text`, "function"},
		{`typeof Comment`, "function"},
		{`typeof DocumentFragment`, "function"},
		{`typeof Element`, "function"},
		{`typeof HTMLElement`, "function"},
		{`typeof HTMLDivElement`, "function"},
		{`typeof HTMLBodyElement`, "function"},
		{`Node.ELEMENT_NODE`, "1"},
		{`Node.TEXT_NODE`, "3"},
		{`Node.COMMENT_NODE`, "8"},
		{`Node.DOCUMENT_NODE`, "9"},

		// instanceof chain
		{`new Element('div') instanceof Node`, "true"},
		{`new Element('div') instanceof EventTarget`, "true"},
		{`new HTMLElement('div') instanceof Element`, "true"},
		{`new HTMLElement('div') instanceof Node`, "true"},
		{`new HTMLDivElement() instanceof HTMLElement`, "true"},
		{`new HTMLDivElement() instanceof Element`, "true"},
		{`new HTMLDivElement() instanceof Node`, "true"},
		{`new HTMLDivElement() instanceof EventTarget`, "true"},

		// tagName / nodeName / nodeType
		{`new HTMLDivElement().tagName`, "DIV"},
		{`new HTMLDivElement().nodeName`, "DIV"},
		{`new HTMLDivElement().localName`, "div"},
		{`new HTMLDivElement().nodeType`, "1"},
		{`new Text('hi').nodeType`, "3"},
		{`new Text('hi').data`, "hi"},
		{`new Comment('c').nodeType`, "8"},

		// toString tags
		{`Object.prototype.toString.call(new HTMLDivElement())`, "[object HTMLDivElement]"},
		{`Object.prototype.toString.call(new Text(''))`, "[object Text]"},
		{`Object.prototype.toString.call(new Comment(''))`, "[object Comment]"},
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

// TestNodeAppendChildAndSiblings — appendChild updates parent + childNodes,
// and computes firstChild/lastChild/nextSibling/previousSibling correctly.
func TestNodeAppendChildAndSiblings(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const root = new HTMLDivElement();
		const a = new HTMLDivElement();
		const b = new HTMLSpanElement();
		const c = new HTMLDivElement();
		root.appendChild(a);
		root.appendChild(b);
		root.appendChild(c);
		return JSON.stringify({
			rootHasA: a.parentNode === root,
			rootHasB: b.parentNode === root,
			rootChildCount: root.childNodes.length,
			firstChildIsA: root.firstChild === a,
			lastChildIsC: root.lastChild === c,
			aNextIsB: a.nextSibling === b,
			bPrevIsA: b.previousSibling === a,
			bNextIsC: b.nextSibling === c,
			cNextNull: c.nextSibling === null,
			aPrevNull: a.previousSibling === null,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"rootHasA":true,"rootHasB":true,"rootChildCount":3,"firstChildIsA":true,"lastChildIsC":true,"aNextIsB":true,"bPrevIsA":true,"bNextIsC":true,"cNextNull":true,"aPrevNull":true}`
	if got := res.String(); got != want {
		t.Errorf("siblings:\n  got: %s\n  want: %s", got, want)
	}
}

// TestNodeRemoveChild — removeChild detaches; subsequent appendChild
// somewhere else moves the node.
func TestNodeRemoveChild(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const a = new HTMLDivElement();
		const b = new HTMLDivElement();
		const c = new HTMLDivElement();
		a.appendChild(c);
		const beforeMove = c.parentNode === a;
		// move c into b
		b.appendChild(c);
		return JSON.stringify({
			beforeMove: beforeMove,
			cParentIsB: c.parentNode === b,
			aChildCount: a.childNodes.length,
			bChildCount: b.childNodes.length,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"beforeMove":true,"cParentIsB":true,"aChildCount":0,"bChildCount":1}`
	if got := res.String(); got != want {
		t.Errorf("removeChild:\n  got: %s\n  want: %s", got, want)
	}
}

// TestElementAttributes — setAttribute / getAttribute / hasAttribute /
// removeAttribute round-trip; id/className delegate to attributes.
func TestElementAttributes(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const e = new HTMLDivElement();
		e.setAttribute('data-foo', 'bar');
		e.id = 'main';
		e.className = 'big red';
		return JSON.stringify({
			dataFoo: e.getAttribute('data-foo'),
			id: e.id,
			idAttr: e.getAttribute('id'),
			className: e.className,
			classAttr: e.getAttribute('class'),
			hasFoo: e.hasAttribute('data-foo'),
			hasMissing: e.hasAttribute('missing'),
			missingAttr: e.getAttribute('missing'),
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"dataFoo":"bar","id":"main","idAttr":"main","className":"big red","classAttr":"big red","hasFoo":true,"hasMissing":false,"missingAttr":null}`
	if got := res.String(); got != want {
		t.Errorf("attributes:\n  got: %s\n  want: %s", got, want)
	}
}

// TestElementInnerHTMLOuterHTML — outer/innerHTML round-trip on an
// element with attributes + child nodes.
func TestElementInnerHTMLOuterHTML(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const wrap = new HTMLDivElement();
		wrap.id = 'w';
		const inner = new HTMLSpanElement();
		inner.appendChild(new Text('hello'));
		wrap.appendChild(inner);
		return JSON.stringify({
			outer: wrap.outerHTML,
			inner: wrap.innerHTML,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"outer":"<div id=\"w\"><span>hello</span></div>","inner":"<span>hello</span>"}`
	if got := res.String(); got != want {
		t.Errorf("inner/outer HTML:\n  got: %s\n  want: %s", got, want)
	}
}

// TestNodeTextContent — textContent walks the tree.
func TestNodeTextContent(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const wrap = new HTMLDivElement();
		const a = new HTMLSpanElement();
		a.appendChild(new Text('he'));
		const b = new HTMLSpanElement();
		b.appendChild(new Text('llo'));
		wrap.appendChild(a);
		wrap.appendChild(b);
		return wrap.textContent;
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := res.String(); got != "hello" {
		t.Errorf("textContent = %q, want \"hello\"", got)
	}
}

// TestNodeContains — contains walks ancestors.
func TestNodeContains(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const a = new HTMLDivElement();
		const b = new HTMLDivElement();
		const c = new HTMLDivElement();
		a.appendChild(b);
		b.appendChild(c);
		return JSON.stringify({
			aContainsC: a.contains(c),
			cContainsA: c.contains(a),
			aContainsA: a.contains(a),
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"aContainsC":true,"cContainsA":false,"aContainsA":true}`
	if got := res.String(); got != want {
		t.Errorf("contains:\n  got: %s\n  want: %s", got, want)
	}
}

// TestEventTreeCapturBubble — dispatch on a child fires capture
// listeners on ancestors, target listener, then bubble listeners.
func TestEventTreeCaptureBubble(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const root = new HTMLDivElement();
		const mid = new HTMLDivElement();
		const target = new HTMLDivElement();
		root.appendChild(mid);
		mid.appendChild(target);

		const calls = [];
		// capture listeners on ancestors
		root.addEventListener('go', () => calls.push('root-cap'), { capture: true });
		mid.addEventListener('go', () => calls.push('mid-cap'), { capture: true });
		// at-target listener
		target.addEventListener('go', () => calls.push('target'));
		// bubble listeners on ancestors
		mid.addEventListener('go', () => calls.push('mid-bub'));
		root.addEventListener('go', () => calls.push('root-bub'));

		target.dispatchEvent(new Event('go', { bubbles: true }));
		return calls.join(',');
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := "root-cap,mid-cap,target,mid-bub,root-bub"
	if got := res.String(); got != want {
		t.Errorf("dispatch order:\n  got: %s\n  want: %s", got, want)
	}
}

// TestEventTreeStopPropagation — stopPropagation skips the bubble path.
func TestEventTreeStopPropagation(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const root = new HTMLDivElement();
		const target = new HTMLDivElement();
		root.appendChild(target);
		const calls = [];
		root.addEventListener('go', () => calls.push('root-bub'));
		target.addEventListener('go', (e) => { calls.push('target'); e.stopPropagation(); });
		target.dispatchEvent(new Event('go', { bubbles: true }));
		return calls.join(',');
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := res.String(); got != "target" {
		t.Errorf("stopPropagation: got %q, want \"target\"", got)
	}
}

// TestElementGetElementsByTagName — descendants of given tag.
func TestElementGetElementsByTagName(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const root = new HTMLBodyElement();
		const a = new HTMLDivElement();
		const b = new HTMLSpanElement();
		const c = new HTMLDivElement();
		root.appendChild(a);
		a.appendChild(b);
		root.appendChild(c);
		return JSON.stringify({
			divs: root.getElementsByTagName('div').length,
			spans: root.getElementsByTagName('span').length,
			all: root.getElementsByTagName('*').length,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"divs":2,"spans":1,"all":3}`
	if got := res.String(); got != want {
		t.Errorf("getElementsByTagName:\n  got: %s\n  want: %s", got, want)
	}
}

// TestComposedPathReflectsChain — composedPath now walks the parent
// chain.
func TestComposedPathReflectsChain(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const root = new HTMLDivElement();
		const target = new HTMLDivElement();
		root.appendChild(target);
		let path = [];
		target.addEventListener('go', (e) => { path = e.composedPath(); });
		target.dispatchEvent(new Event('go', { bubbles: true }));
		return JSON.stringify({
			pathLen: path.length,
			pathStart: path[0] === target,
			pathEnd: path[1] === root,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"pathLen":2,"pathStart":true,"pathEnd":true}`
	if got := res.String(); got != want {
		t.Errorf("composedPath:\n  got: %s\n  want: %s", got, want)
	}
}
