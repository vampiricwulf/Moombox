package goja

import (
	"context"
	"testing"
)

// Day-3 tests: CSSStyleDeclaration + getComputedStyle.

func TestCSSStyleDeclarationClassExists(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	cases := []struct{ expr, want string }{
		{`typeof CSSStyleDeclaration`, "function"},
		{`typeof getComputedStyle`, "function"},
		{`new HTMLDivElement().style.constructor.name`, "CSSStyleDeclaration"},
		{`Object.prototype.toString.call(new HTMLDivElement().style)`, "[object CSSStyleDeclaration]"},
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

// TestCSSStyleInlineRoundTrip — assigning to el.style.color updates the
// internal store AND the element's `style` attribute.
func TestCSSStyleInlineRoundTrip(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const e = new HTMLDivElement();
		e.style.color = 'red';
		e.style.backgroundColor = 'blue';
		e.style['margin-top'] = '4px';
		return JSON.stringify({
			color: e.style.color,
			bg: e.style.backgroundColor,
			bgGet: e.style.getPropertyValue('background-color'),
			marginTop: e.style.marginTop,
			marginViaGetter: e.style.getPropertyValue('margin-top'),
			cssText: e.style.cssText,
			attr: e.getAttribute('style'),
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"color":"red","bg":"blue","bgGet":"blue","marginTop":"4px","marginViaGetter":"4px","cssText":"color: red; background-color: blue; margin-top: 4px;","attr":"color: red; background-color: blue; margin-top: 4px;"}`
	if got := res.String(); got != want {
		t.Errorf("inline-style round-trip:\n  got: %s\n  want: %s", got, want)
	}
}

// TestCSSStyleSetProperty — setProperty / removeProperty round-trip
// matches the dot/bracket access path.
func TestCSSStyleSetProperty(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const e = new HTMLDivElement();
		e.style.setProperty('color', 'red');
		e.style.setProperty('font-size', '14px');
		const had = e.style.removeProperty('color');
		return JSON.stringify({
			afterRemove: e.style.color,
			removed: had,
			fontSize: e.style.fontSize,
			length: e.style.length,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"afterRemove":"","removed":"red","fontSize":"14px","length":1}`
	if got := res.String(); got != want {
		t.Errorf("setProperty:\n  got: %s\n  want: %s", got, want)
	}
}

// TestCSSStyleCssTextSetter — setting cssText replaces the whole map.
func TestCSSStyleCssTextSetter(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const e = new HTMLDivElement();
		e.style.color = 'red';
		e.style.cssText = 'display: none; opacity: 0.5';
		return JSON.stringify({
			color: e.style.color,
			display: e.style.display,
			opacity: e.style.opacity,
			length: e.style.length,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"color":"","display":"none","opacity":"0.5","length":2}`
	if got := res.String(); got != want {
		t.Errorf("cssText setter:\n  got: %s\n  want: %s", got, want)
	}
}

// TestGetComputedStyleDefaults — getComputedStyle returns spec defaults
// for properties not set inline.
func TestGetComputedStyleDefaults(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const div = new HTMLDivElement();
		const span = new HTMLSpanElement();
		const computedDiv = getComputedStyle(div);
		const computedSpan = getComputedStyle(span);
		return JSON.stringify({
			divDisplay: computedDiv.display,
			spanDisplay: computedSpan.display,
			divPosition: computedDiv.position,
			divVisibility: computedDiv.visibility,
			divOpacity: computedDiv.opacity,
			divColor: computedDiv.color,
			divFontFamily: computedDiv.fontFamily,
			divLineHeight: computedDiv.lineHeight,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"divDisplay":"block","spanDisplay":"inline","divPosition":"static","divVisibility":"visible","divOpacity":"1","divColor":"rgb(0, 0, 0)","divFontFamily":"sans-serif","divLineHeight":"normal"}`
	if got := res.String(); got != want {
		t.Errorf("getComputedStyle defaults:\n  got: %s\n  want: %s", got, want)
	}
}

// TestGetComputedStyleInlineOverridesDefault — inline style wins over
// the spec default.
func TestGetComputedStyleInlineOverridesDefault(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const div = new HTMLDivElement();
		div.style.display = 'flex';
		div.style.color = 'red';
		const c = getComputedStyle(div);
		return JSON.stringify({
			display: c.display,
			color: c.color,
			// non-set property still returns default
			position: c.position,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"display":"flex","color":"red","position":"static"}`
	if got := res.String(); got != want {
		t.Errorf("getComputedStyle override:\n  got: %s\n  want: %s", got, want)
	}
}

// TestGetComputedStyleReadOnly — writes to the result are no-ops.
func TestGetComputedStyleReadOnly(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const div = new HTMLDivElement();
		const c = getComputedStyle(div);
		c.color = 'green';
		return c.color === 'green' ? 'mutable' : 'readonly';
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := res.String(); got != "readonly" {
		t.Errorf("getComputedStyle readOnly: got %q, want \"readonly\"", got)
	}
}

// TestCSSStyleStyleAttributeSeed — when an element already has a
// `style` attribute (set via setAttribute), reading element.style
// reflects the seeded values.
func TestCSSStyleStyleAttributeSeed(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}
	res, err := vm.RunString(`(function() {
		const e = new HTMLDivElement();
		e.setAttribute('style', 'color: red; padding: 4px');
		return JSON.stringify({
			color: e.style.color,
			padding: e.style.padding,
		});
	})()`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := `{"color":"red","padding":"4px"}`
	if got := res.String(); got != want {
		t.Errorf("style attribute seed:\n  got: %s\n  want: %s", got, want)
	}
}
