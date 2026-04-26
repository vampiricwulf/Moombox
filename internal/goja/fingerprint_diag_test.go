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
		// Function-call fingerprints
		checks.fn_getRandomValues_toString = (crypto && crypto.getRandomValues) ? crypto.getRandomValues.toString() : "n/a";
		checks.fn_setTimeout_toString = setTimeout.toString();
		checks.fn_addEventListener_toString = (typeof addEventListener === "function") ? addEventListener.toString() : "n/a";
		// instanceof checks (real browsers have prototype hierarchy)
		checks.instanceof_window_object = window instanceof Object;
		checks.instanceof_navigator_object = navigator instanceof Object;
		// Property descriptor — real browsers expose UA via getter
		var uaDesc = Object.getOwnPropertyDescriptor(navigator, 'userAgent');
		checks.uaDesc = uaDesc ? {
			hasGet: !!uaDesc.get,
			hasValue: 'value' in uaDesc,
			writable: uaDesc.writable,
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
		// Each Uint32 has 4 bytes; our shim might only fill 1 byte per element.
		// Real browsers fill all 16 bytes; our impl may only fill 4 bytes.
		// Test: a Uint32 value should typically exceed 255 (since 4 bytes random).
		checks.cryptoU32_anyHigh = u32[0] > 255 || u32[1] > 255 || u32[2] > 255 || u32[3] > 255;
		return JSON.stringify(checks, null, 2);
	})()`)
	if err != nil {
		t.Fatalf("diag: %v", err)
	}
	t.Logf("fingerprint dump:\n%s", res.String())
}
