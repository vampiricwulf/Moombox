package goja

import (
	"context"
	"testing"
)

// Day-4 tests: Document / HTMLDocument with createElement returning
// real subclasses, querySelector with the minimal selector parser,
// initial document tree, and Window class.

func TestDocumentClassExists(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	cases := []struct{ expr, want string }{
		{`typeof Document`, "function"},
		{`typeof HTMLDocument`, "function"},
		{`typeof Window`, "function"},
		{`document instanceof HTMLDocument`, "true"},
		{`document instanceof Document`, "true"},
		{`document instanceof Node`, "true"},
		{`document instanceof EventTarget`, "true"},
		{`Object.prototype.toString.call(document)`, "[object HTMLDocument]"},
		{`document.constructor.name`, "HTMLDocument"},
		{`document.documentElement.tagName`, "HTML"},
		{`document.head.tagName`, "HEAD"},
		{`document.body.tagName`, "BODY"},
		{`document.body.parentNode === document.documentElement`, "true"},
		{`document.documentElement.parentNode === document`, "true"},
		{`document.URL`, "https://www.youtube.com/"},
		{`document.readyState`, "complete"},
		{`document.visibilityState`, "visible"},
		{`document.compatMode`, "CSS1Compat"},
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

// TestDocumentCreateElement — createElement returns the right subclass
// per tag name.
func TestDocumentCreateElement(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const div = document.createElement('div');
		const span = document.createElement('span');
		const a = document.createElement('a');
		const img = document.createElement('img');
		const unknown = document.createElement('foobar');
		return JSON.stringify({
			divClass: div.constructor.name,
			divIsDiv: div instanceof HTMLDivElement,
			divIsHTMLEl: div instanceof HTMLElement,
			divIsElement: div instanceof Element,
			spanClass: span.constructor.name,
			anchorClass: a.constructor.name,
			imgClass: img.constructor.name,
			unknownClass: unknown.constructor.name,
			unknownIsHTMLEl: unknown instanceof HTMLElement,
			divOwnerDoc: div.ownerDocument === document,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"divClass":"HTMLDivElement","divIsDiv":true,"divIsHTMLEl":true,"divIsElement":true,"spanClass":"HTMLSpanElement","anchorClass":"HTMLAnchorElement","imgClass":"HTMLImageElement","unknownClass":"HTMLUnknownElement","unknownIsHTMLEl":true,"divOwnerDoc":true}`
	if got := res.String(); got != want {
		t.Errorf("createElement:\n  got: %s\n  want: %s", got, want)
	}
}

// TestDocumentQuerySelector — basic selectors work.
func TestDocumentQuerySelector(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const main = document.createElement('div');
		main.id = 'main';
		main.className = 'wrap big';
		document.body.appendChild(main);
		const child = document.createElement('span');
		child.className = 'child';
		main.appendChild(child);
		const child2 = document.createElement('span');
		child2.className = 'child highlight';
		main.appendChild(child2);

		return JSON.stringify({
			byId: document.querySelector('#main').tagName,
			byTag: document.querySelector('span').tagName,
			byClass: document.querySelector('.wrap').tagName,
			byCompound: document.querySelector('div.wrap.big').id,
			descendantById: document.querySelector('#main span').tagName,
			highlight: document.querySelector('.highlight').className,
			missing: document.querySelector('.no-such-class') === null,
			allCount: document.querySelectorAll('span').length,
			allClassCount: document.querySelectorAll('.child').length,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"byId":"DIV","byTag":"SPAN","byClass":"DIV","byCompound":"main","descendantById":"SPAN","highlight":"child highlight","missing":true,"allCount":2,"allClassCount":2}`
	if got := res.String(); got != want {
		t.Errorf("querySelector:\n  got: %s\n  want: %s", got, want)
	}
}

// TestDocumentGetElementById — walks the tree.
func TestDocumentGetElementById(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const wrap = document.createElement('div');
		wrap.id = 'wrap';
		const inner = document.createElement('span');
		inner.id = 'inner';
		wrap.appendChild(inner);
		document.body.appendChild(wrap);
		return JSON.stringify({
			wrap: document.getElementById('wrap') === wrap,
			inner: document.getElementById('inner') === inner,
			missing: document.getElementById('nope') === null,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"wrap":true,"inner":true,"missing":true}`
	if got := res.String(); got != want {
		t.Errorf("getElementById:\n  got: %s\n  want: %s", got, want)
	}
}

// TestDocumentTitle — title getter/setter syncs with the title element
// inside head.
func TestDocumentTitle(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		document.title = 'Moombox';
		// Title element should now exist in head.
		const titleEl = document.head.getElementsByTagName('title')[0];
		return JSON.stringify({
			titleGet: document.title,
			titleEl: titleEl ? titleEl.tagName : null,
			titleText: titleEl ? titleEl.textContent : null,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"titleGet":"Moombox","titleEl":"TITLE","titleText":"Moombox"}`
	if got := res.String(); got != want {
		t.Errorf("title:\n  got: %s\n  want: %s", got, want)
	}
}

// TestDocumentCreateTextAndComment — factory functions.
func TestDocumentCreateTextAndComment(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const t = document.createTextNode('hi');
		const c = document.createComment('cmt');
		return JSON.stringify({
			tType: t.nodeType,
			tData: t.data,
			cType: c.nodeType,
			cData: c.data,
			tIsText: t instanceof Text,
			cIsComment: c instanceof Comment,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"tType":3,"tData":"hi","cType":8,"cData":"cmt","tIsText":true,"cIsComment":true}`
	if got := res.String(); got != want {
		t.Errorf("createTextNode/createComment:\n  got: %s\n  want: %s", got, want)
	}
}

// TestWindowConstructorIsClass — Window class exists and globalThis
// chains through it for instanceof.
func TestWindowConstructorIsClass(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	cases := []struct{ expr, want string }{
		{`typeof Window`, "function"},
		{`Window.name`, "Window"},
		{`new Window() instanceof Window`, "true"},
		{`new Window() instanceof EventTarget`, "true"},
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

// TestDocumentDispatchOnDescendant — full capture/bubble propagation
// works on the real document tree.
func TestDocumentDispatchOnDescendant(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const wrap = document.createElement('div');
		const target = document.createElement('span');
		wrap.appendChild(target);
		document.body.appendChild(wrap);

		const calls = [];
		document.body.addEventListener('go', () => calls.push('body-cap'), { capture: true });
		wrap.addEventListener('go', () => calls.push('wrap-cap'), { capture: true });
		target.addEventListener('go', () => calls.push('target'));
		wrap.addEventListener('go', () => calls.push('wrap-bub'));
		document.body.addEventListener('go', () => calls.push('body-bub'));
		document.documentElement.addEventListener('go', () => calls.push('html-bub'));

		target.dispatchEvent(new Event('go', { bubbles: true }));
		return calls.join(',');
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := "body-cap,wrap-cap,target,wrap-bub,body-bub,html-bub"
	if got := res.String(); got != want {
		t.Errorf("document tree dispatch:\n  got: %s\n  want: %s", got, want)
	}
}
