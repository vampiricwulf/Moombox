package goja

import (
	"strings"
	"testing"

	gojalib "github.com/dop251/goja"
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

// --- RunString tests ---

func TestRunStringBasicExpression(t *testing.T) {
	vm := NewRuntime()
	v, err := RunString(vm, "1 + 2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.ToInteger() != 3 {
		t.Errorf("expected 3, got %d", v.ToInteger())
	}
}

func TestRunStringReturnsString(t *testing.T) {
	vm := NewRuntime()
	v, err := RunString(vm, "'hello' + ' ' + 'world'")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "hello world" {
		t.Errorf("expected 'hello world', got %q", v.String())
	}
}

func TestRunStringSyntaxError(t *testing.T) {
	vm := NewRuntime()
	_, err := RunString(vm, "function(")
	if err == nil {
		t.Fatal("expected error for syntax error, got nil")
	}
	if !strings.Contains(err.Error(), "goja eval") {
		t.Errorf("expected error to contain 'goja eval', got %q", err.Error())
	}
}

func TestRunStringReferenceError(t *testing.T) {
	vm := NewRuntime()
	_, err := RunString(vm, "undefinedVariable.property")
	if err == nil {
		t.Fatal("expected error for undefined variable access, got nil")
	}
}

func TestRunStringReturnsUndefined(t *testing.T) {
	vm := NewRuntime()
	v, err := RunString(vm, "undefined")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != gojalib.Undefined() {
		t.Error("expected undefined value")
	}
}

func TestRunStringReturnsNull(t *testing.T) {
	vm := NewRuntime()
	v, err := RunString(vm, "null")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != gojalib.Null() {
		t.Error("expected null value")
	}
}

func TestRunStringObjectReturn(t *testing.T) {
	vm := NewRuntime()
	v, err := RunString(vm, "({a: 1, b: 'two'})")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	obj := v.ToObject(vm)
	a := obj.Get("a")
	if a.ToInteger() != 1 {
		t.Errorf("expected a=1, got %d", a.ToInteger())
	}
	b := obj.Get("b")
	if b.String() != "two" {
		t.Errorf("expected b='two', got %q", b.String())
	}
}

// --- CallFunction tests ---

func TestCallFunctionNilFunction(t *testing.T) {
	vm := NewRuntime()
	_, err := CallFunction(vm, nil, gojalib.Undefined())
	if err == nil {
		t.Fatal("expected error for nil function, got nil")
	}
	if !strings.Contains(err.Error(), "function is nil") {
		t.Errorf("expected 'function is nil' error, got %q", err.Error())
	}
}

func TestCallFunctionSimple(t *testing.T) {
	vm := NewRuntime()
	_, err := RunString(vm, "function add(a, b) { return a + b; }")
	if err != nil {
		t.Fatalf("unexpected error defining function: %v", err)
	}

	fnVal := vm.Get("add")
	fn, ok := gojalib.AssertFunction(fnVal)
	if !ok {
		t.Fatal("expected 'add' to be a function")
	}

	result, err := CallFunction(vm, fn, gojalib.Undefined(), vm.ToValue(3), vm.ToValue(4))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ToInteger() != 7 {
		t.Errorf("expected 7, got %d", result.ToInteger())
	}
}

func TestCallFunctionThrowsError(t *testing.T) {
	vm := NewRuntime()
	_, err := RunString(vm, "function throwErr() { throw new Error('test error'); }")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fnVal := vm.Get("throwErr")
	fn, ok := gojalib.AssertFunction(fnVal)
	if !ok {
		t.Fatal("expected 'throwErr' to be a function")
	}

	_, err = CallFunction(vm, fn, gojalib.Undefined())
	if err == nil {
		t.Fatal("expected error from throwing function, got nil")
	}
	if !strings.Contains(err.Error(), "goja call") {
		t.Errorf("expected 'goja call' in error, got %q", err.Error())
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
	v, err := RunString(vm, "atob(btoa('Hello, World!'))")
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
	v, err := RunString(vm, "typeof document")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "object" {
		t.Errorf("expected document to be object, got %q", v.String())
	}

	// Verify document.createElement works
	v, err = RunString(vm, "document.createElement('div').tagName")
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

	v, err := RunString(vm, "navigator.userAgent")
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
	v, err := RunString(vm, "typeof setTimeout")
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

	v, err := RunString(vm, "btoa('ABC')")
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

	v, err := RunString(vm, "atob('QUJD')")
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

	v, err := RunString(vm, "new TextEncoder().encode('AB').length")
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

	v, err := RunString(vm, "new TextDecoder().decode(new TextEncoder().encode('hello'))")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.String() != "hello" {
		t.Errorf("expected 'hello', got %q", v.String())
	}
}
