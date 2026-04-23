package goja

import (
	"testing"
)

// --- NewRuntime tests ---

func TestNewRuntime(t *testing.T) {
	vm := NewRuntime()
	if vm == nil {
		t.Fatal("expected non-nil runtime")
	}
}

func TestNewRuntimeFieldMapper(t *testing.T) {
	// NewRuntime sets TagFieldNameMapper("json", true) — verify it works
	// by setting a Go struct with json tags and checking JS access
	vm := NewRuntime()

	type testStruct struct {
		MyField string `json:"myField"`
	}
	vm.Set("obj", testStruct{MyField: "hello"})
	v, err := vm.RunString("obj.myField")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "hello" {
		t.Errorf("expected 'hello', got %q", v.String())
	}
}

// --- NewRuntimeWithShims tests ---

func TestNewRuntimeWithShimsCreatesRuntime(t *testing.T) {
	vm, tm, err := NewRuntimeWithShims("TestAgent/1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vm == nil {
		t.Fatal("expected non-nil runtime")
	}
	if tm == nil {
		t.Fatal("expected non-nil timer manager")
	}
}

func TestNewRuntimeWithShimsBtoaAtob(t *testing.T) {
	vm, _, err := NewRuntimeWithShims("TestAgent/1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// btoa encodes, atob decodes — round trip
	v, err := vm.RunString("atob(btoa('Hello, World!'))")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "Hello, World!" {
		t.Errorf("expected 'Hello, World!', got %q", v.String())
	}
}

func TestNewRuntimeWithShimsDOMDocument(t *testing.T) {
	vm, _, err := NewRuntimeWithShims("TestAgent/1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify document exists
	v, err := vm.RunString("typeof document")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "object" {
		t.Errorf("expected document to be object, got %q", v.String())
	}

	// Verify document.createElement works
	v, err = vm.RunString("document.createElement('div').tagName")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "DIV" {
		t.Errorf("expected 'DIV', got %q", v.String())
	}
}

func TestNewRuntimeWithShimsNavigatorUserAgent(t *testing.T) {
	ua := "MoomboxTest/2.0"
	vm, _, err := NewRuntimeWithShims(ua)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, err := vm.RunString("navigator.userAgent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != ua {
		t.Errorf("expected user agent %q, got %q", ua, v.String())
	}
}

func TestNewRuntimeWithShimsSetTimeout(t *testing.T) {
	vm, tm, err := NewRuntimeWithShims("TestAgent/1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer tm.CancelAll()

	// Verify setTimeout is registered as a function
	v, err := vm.RunString("typeof setTimeout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "function" {
		t.Errorf("expected setTimeout to be function, got %q", v.String())
	}
}

// --- RegisterEncoding standalone tests ---

func TestRegisterEncodingBtoaBasic(t *testing.T) {
	vm := NewRuntime()
	if err := RegisterEncoding(vm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, err := vm.RunString("btoa('ABC')")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "QUJD" {
		t.Errorf("expected 'QUJD', got %q", v.String())
	}
}

func TestRegisterEncodingAtobBasic(t *testing.T) {
	vm := NewRuntime()
	if err := RegisterEncoding(vm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, err := vm.RunString("atob('QUJD')")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "ABC" {
		t.Errorf("expected 'ABC', got %q", v.String())
	}
}

func TestRegisterEncodingTextEncoder(t *testing.T) {
	vm := NewRuntime()
	if err := RegisterEncoding(vm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, err := vm.RunString("new TextEncoder().encode('AB').length")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.ToInteger() != 2 {
		t.Errorf("expected 2, got %d", v.ToInteger())
	}
}

func TestRegisterEncodingTextDecoder(t *testing.T) {
	vm := NewRuntime()
	if err := RegisterEncoding(vm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, err := vm.RunString("new TextDecoder().decode(new TextEncoder().encode('hello'))")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "hello" {
		t.Errorf("expected 'hello', got %q", v.String())
	}
}
