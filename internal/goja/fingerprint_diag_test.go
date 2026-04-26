package goja

import (
	"context"
	"testing"
)

// TestFingerprintShape dumps fingerprint-relevant details of our shim so we
// can spot what BotGuard would notice as "non-browser". The test isn't
// asserting specific values; it's diagnostic. Run with `go test -v -run
// TestFingerprintShape ./internal/goja/...` to print the dump.
func TestFingerprintShape(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}

	res, err := vm.RunString(`(function() {
		var checks = {};
		// toString tags — real browsers return "[object Window]", "[object Navigator]", etc.
		checks.toString_globalThis = Object.prototype.toString.call(globalThis);
		checks.toString_window = Object.prototype.toString.call(window);
		checks.toString_navigator = Object.prototype.toString.call(navigator);
		checks.toString_document = Object.prototype.toString.call(document);
		checks.toString_location = Object.prototype.toString.call(location);
		checks.toString_screen = Object.prototype.toString.call(screen);
		checks.toString_history = Object.prototype.toString.call(history);
		checks.toString_localStorage = Object.prototype.toString.call(localStorage);
		checks.toString_performance = Object.prototype.toString.call(performance);
		// constructor.name — real browsers return "Window", "Navigator", etc.
		checks.constructor_navigator = navigator.constructor && navigator.constructor.name;
		checks.constructor_document = document.constructor && document.constructor.name;
		checks.constructor_location = location.constructor && location.constructor.name;
		checks.constructor_screen = screen.constructor && screen.constructor.name;
		checks.constructor_history = history.constructor && history.constructor.name;
		checks.constructor_performance = performance.constructor && performance.constructor.name;
		checks.constructor_localStorage = localStorage.constructor && localStorage.constructor.name;
		checks.constructor_crypto = crypto.constructor && crypto.constructor.name;
		// instanceof checks (real browsers have prototype hierarchy)
		checks.instanceof_navigator_class = (typeof Navigator === "function") ? (navigator instanceof Navigator) : "Navigator-class-missing";
		checks.instanceof_document_class = (typeof Document === "function") ? (document instanceof Document) : "Document-class-missing";
		checks.instanceof_htmldocument_class = (typeof HTMLDocument === "function") ? (document instanceof HTMLDocument) : "HTMLDocument-class-missing";
		checks.instanceof_location_class = (typeof Location === "function") ? (location instanceof Location) : "Location-class-missing";
		checks.instanceof_screen_class = (typeof Screen === "function") ? (screen instanceof Screen) : "Screen-class-missing";
		checks.instanceof_window_class = (typeof Window === "function") ? (window instanceof Window) : "Window-class-missing";
		checks.instanceof_history_class = (typeof History === "function") ? (history instanceof History) : "History-class-missing";
		checks.instanceof_storage_class = (typeof Storage === "function") ? (localStorage instanceof Storage) : "Storage-class-missing";
		checks.instanceof_performance_class = (typeof Performance === "function") ? (performance instanceof Performance) : "Performance-class-missing";
		checks.instanceof_crypto_class = (typeof Crypto === "function") ? (crypto instanceof Crypto) : "Crypto-class-missing";
		checks.instanceof_eventtarget = (typeof EventTarget === "function") ? (document instanceof EventTarget) : "EventTarget-class-missing";
		checks.instanceof_node = (typeof Node === "function") ? (document instanceof Node) : "Node-class-missing";
		// Function-call fingerprints
		checks.fn_getRandomValues_toString = (crypto && crypto.getRandomValues) ? crypto.getRandomValues.toString() : "n/a";
		checks.fn_setTimeout_toString = setTimeout.toString();
		checks.fn_addEventListener_toString = (typeof addEventListener === "function") ? addEventListener.toString() : "n/a";
		// Property descriptor — real browsers expose UA via getter
		var uaDesc = Object.getOwnPropertyDescriptor(navigator, 'userAgent');
		checks.uaDesc = uaDesc ? {
			hasGet: !!uaDesc.get,
			hasValue: 'value' in uaDesc,
			enumerable: uaDesc.enumerable
		} : "absent";
		// Frame chain
		checks.window_self_eq = window === self;
		checks.window_top_eq = window === top;
		checks.window_globalThis_eq = window === globalThis;
		// crypto.getRandomValues with different typed arrays
		var u8 = new Uint8Array(8);
		crypto.getRandomValues(u8);
		var u8sum = 0; for (var i = 0; i < 8; i++) u8sum += u8[i];
		checks.cryptoU8_filled = u8sum > 0;
		var u32 = new Uint32Array(4);
		crypto.getRandomValues(u32);
		var u32sum = 0; for (var i = 0; i < 4; i++) u32sum += u32[i];
		checks.cryptoU32_filled = u32sum > 0;
		checks.cryptoU32_anyHigh = u32[0] > 255 || u32[1] > 255 || u32[2] > 255 || u32[3] > 255;
		return JSON.stringify(checks, null, 2);
	})()`)
	if err != nil {
		t.Fatalf("diag: %v", err)
	}
	t.Logf("fingerprint dump:\n%s", res.String())
}

// TestPrototypeChainContract enforces the post-fix browser-class hierarchy.
// Failures here mean BotGuard-style fingerprinters that do
//
//	document instanceof HTMLDocument
//
// (which was historically the next class of probe to fail after the
// Symbol.toStringTag pass in test.46) will reject the env. Once the
// hierarchy is wired in, any regression to dom_shim.go will trip these.
func TestPrototypeChainContract(t *testing.T) {
	vm, _, err := NewRuntimeWithShims(context.Background(), "")
	if err != nil {
		t.Fatalf("NewRuntimeWithShims: %v", err)
	}

	cases := []struct {
		name string
		expr string
		want string
	}{
		{"Navigator class exists", `typeof Navigator`, "function"},
		{"Document class exists", `typeof Document`, "function"},
		{"HTMLDocument class exists", `typeof HTMLDocument`, "function"},
		{"Location class exists", `typeof Location`, "function"},
		{"Screen class exists", `typeof Screen`, "function"},
		{"Window class exists", `typeof Window`, "function"},
		{"History class exists", `typeof History`, "function"},
		{"Storage class exists", `typeof Storage`, "function"},
		{"Performance class exists", `typeof Performance`, "function"},
		{"Crypto class exists", `typeof Crypto`, "function"},
		{"EventTarget class exists", `typeof EventTarget`, "function"},
		{"Node class exists", `typeof Node`, "function"},

		{"navigator instanceof Navigator", `navigator instanceof Navigator`, "true"},
		{"document instanceof HTMLDocument", `document instanceof HTMLDocument`, "true"},
		{"document instanceof Document", `document instanceof Document`, "true"},
		{"document instanceof Node", `document instanceof Node`, "true"},
		{"document instanceof EventTarget", `document instanceof EventTarget`, "true"},
		{"location instanceof Location", `location instanceof Location`, "true"},
		{"screen instanceof Screen", `screen instanceof Screen`, "true"},
		{"history instanceof History", `history instanceof History`, "true"},
		{"localStorage instanceof Storage", `localStorage instanceof Storage`, "true"},
		{"sessionStorage instanceof Storage", `sessionStorage instanceof Storage`, "true"},
		{"performance instanceof Performance", `performance instanceof Performance`, "true"},
		{"crypto instanceof Crypto", `crypto instanceof Crypto`, "true"},
		// `globalThis instanceof Window` is the standard probe; goja's
		// global object can't actually have its prototype changed so we
		// can't make this true without a wrapper. Verify the class
		// exists at least.

		// constructor.name should reflect the right class
		{"navigator.constructor.name", `navigator.constructor.name`, "Navigator"},
		{"document.constructor.name", `document.constructor.name`, "HTMLDocument"},
		{"location.constructor.name", `location.constructor.name`, "Location"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := vm.RunString(tc.expr)
			if err != nil {
				t.Fatalf("eval %q: %v", tc.expr, err)
			}
			got := v.String()
			if got != tc.want {
				t.Errorf("%s = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}
}
