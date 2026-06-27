package goja

import (
	"testing"
)

// runEncodingJS spins up a runtime with encoding registered and returns the
// string result of the supplied JS expression.
func runEncodingJS(t *testing.T, expr string) string {
	t.Helper()
	vm := NewRuntime()
	if err := RegisterEncoding(vm); err != nil {
		t.Fatalf("RegisterEncoding: %v", err)
	}
	v, err := vm.RunString(expr)
	if err != nil {
		t.Fatalf("RunString(%q): %v", expr, err)
	}
	return v.String()
}

func TestTextEncoderUTF8(t *testing.T) {
	tests := []struct {
		name string
		js   string // expression producing Array.from(encoder.encode(input)).join(",")
		want string
	}{
		{
			name: "ascii",
			js:   `Array.from(new TextEncoder().encode("AB")).join(",")`,
			want: "65,66",
		},
		{
			name: "two byte",
			js:   `Array.from(new TextEncoder().encode("é")).join(",")`, // é
			want: "195,169",
		},
		{
			name: "three byte",
			js:   `Array.from(new TextEncoder().encode("日")).join(",")`, // 日
			want: "230,151,165",
		},
		{
			name: "astral pair",
			js:   `Array.from(new TextEncoder().encode("😀")).join(",")`, // 😀
			want: "240,159,152,128",
		},
		{
			// Regression: the old shim consumed the unit after a lone lead
			// surrogate, silently dropping the "Y". WHATWG says lone
			// surrogates encode as U+FFFD (EF BF BD = 239,191,189).
			name: "lone lead surrogate keeps following char",
			js:   `Array.from(new TextEncoder().encode("X\ud800Y")).join(",")`,
			want: "88,239,191,189,89",
		},
		{
			name: "lone lead surrogate at end",
			js:   `Array.from(new TextEncoder().encode("X\ud800")).join(",")`,
			want: "88,239,191,189",
		},
		{
			name: "lone trail surrogate",
			js:   `Array.from(new TextEncoder().encode("\udc00Z")).join(",")`,
			want: "239,191,189,90",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runEncodingJS(t, tt.js); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestTextDecoderUTF8(t *testing.T) {
	// Round-trip through both shims: encode then decode.
	got := runEncodingJS(t, `new TextDecoder().decode(new TextEncoder().encode("aé日😀"))`)
	want := "aé日😀"
	if got != want {
		t.Errorf("round-trip = %q, want %q", got, want)
	}
}

func TestAtobBtoaBinaryString(t *testing.T) {
	// 0x00..0xFF must round-trip as one code unit per byte (Latin-1
	// "binary string" semantics, NOT UTF-8).
	got := runEncodingJS(t, `
		(function() {
			var s = "";
			for (var i = 0; i < 256; i++) s += String.fromCharCode(i);
			var rt = atob(btoa(s));
			if (rt.length !== 256) return "length " + rt.length;
			for (var i = 0; i < 256; i++) {
				if (rt.charCodeAt(i) !== i) return "mismatch at " + i + ": " + rt.charCodeAt(i);
			}
			return "ok";
		})()`)
	if got != "ok" {
		t.Errorf("binary round-trip failed: %s", got)
	}
}

func TestBtoaThrowsAboveFF(t *testing.T) {
	got := runEncodingJS(t, `
		(function() {
			try { btoa("Ā"); return "no throw"; }
			catch (e) { return "threw"; }
		})()`)
	if got != "threw" {
		t.Errorf("btoa(\\u0100) = %s, want throw (browser InvalidCharacterError parity)", got)
	}
}
