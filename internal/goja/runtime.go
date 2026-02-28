package goja

import (
	"fmt"

	"github.com/dop251/goja"
)

// NewRuntime creates a new Goja runtime with common setup (encoding, timers, etc.).
func NewRuntime() *goja.Runtime {
	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	return vm
}

// NewRuntimeWithShims creates a new Goja runtime with DOM shims, encoding, and timer support.
// Returns the runtime and a TimerManager for cleanup.
func NewRuntimeWithShims(userAgent string) (*goja.Runtime, *TimerManager, error) {
	vm := NewRuntime()
	tm := NewTimerManager(vm)

	if err := RegisterEncoding(vm); err != nil {
		return nil, nil, fmt.Errorf("register encoding: %w", err)
	}
	if err := RegisterTimers(vm, tm); err != nil {
		return nil, nil, fmt.Errorf("register timers: %w", err)
	}
	if err := RegisterDOMShim(vm, userAgent); err != nil {
		return nil, nil, fmt.Errorf("register DOM shim: %w", err)
	}

	return vm, tm, nil
}

// RunString executes JS code in a runtime and returns the result.
func RunString(vm *goja.Runtime, code string) (goja.Value, error) {
	v, err := vm.RunString(code)
	if err != nil {
		return nil, fmt.Errorf("goja eval: %w", err)
	}
	return v, nil
}

// CallFunction calls a JS function with the given arguments.
func CallFunction(vm *goja.Runtime, fn goja.Callable, this goja.Value, args ...goja.Value) (goja.Value, error) {
	if fn == nil {
		return nil, fmt.Errorf("function is nil")
	}
	v, err := fn(this, args...)
	if err != nil {
		return nil, fmt.Errorf("goja call: %w", err)
	}
	return v, nil
}
