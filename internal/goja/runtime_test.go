package goja

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

// --- RunStringWithTimeout (audit goja.md Q1) ---

func TestRunStringWithTimeoutSuccess(t *testing.T) {
	vm := NewRuntime()
	v, err := RunStringWithTimeout(context.Background(), vm, "1 + 2", 1*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v == nil || v.ToInteger() != 3 {
		t.Errorf("expected 3, got %v", v)
	}
}

func TestRunStringWithTimeoutTimeoutFires(t *testing.T) {
	vm := NewRuntime()
	// Infinite loop — must hit the timeout branch.
	_, err := RunStringWithTimeout(context.Background(), vm, "while (true) {}", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "execution timeout") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
}

func TestRunStringWithTimeoutContextCancel(t *testing.T) {
	vm := NewRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel almost immediately — JS loop forever.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := RunStringWithTimeout(ctx, vm, "while (true) {}", 5*time.Second)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

// TestRunStringWithTimeoutVMReusableAfterTimeout locks the post-timeout
// invariant: vm.ClearInterrupt is called after the goroutine settles
// so the runtime stays usable. Pre-Q1 the Interrupt flag persisted
// and the next RunString tripped immediately. This regression buffer
// runs JS that loops, hits the timeout, then runs JS that should
// succeed on the same VM.
func TestRunStringWithTimeoutVMReusableAfterTimeout(t *testing.T) {
	vm := NewRuntime()
	// Hit the timeout once.
	_, err := RunStringWithTimeout(context.Background(), vm, "while (true) {}", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout on first run")
	}
	// Now run something that should succeed — the runtime must not
	// be poisoned by the prior interrupt.
	v, err := RunStringWithTimeout(context.Background(), vm, "40 + 2", 1*time.Second)
	if err != nil {
		t.Fatalf("VM should be reusable after timeout, got: %v", err)
	}
	if v == nil || v.ToInteger() != 42 {
		t.Errorf("expected 42, got %v", v)
	}
}

// TestRunStringWithTimeoutPropagatesRunStringError covers the success-
// path-with-RunString-error case (e.g. ReferenceError / SyntaxError
// thrown by the JS code itself). Should return the RunString error
// untouched, not wrap it.
func TestRunStringWithTimeoutPropagatesRunStringError(t *testing.T) {
	vm := NewRuntime()
	_, err := RunStringWithTimeout(context.Background(), vm, "thisIsNotDefined.foo()", 1*time.Second)
	if err == nil {
		t.Fatal("expected RunString error")
	}
	// Goja surfaces the JS error as the underlying string; we just want
	// to confirm it's not the timeout / context-cancel sentinel.
	if strings.Contains(err.Error(), "execution timeout") {
		t.Errorf("error should not be timeout, got: %v", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error should not be context error, got: %v", err)
	}
}
