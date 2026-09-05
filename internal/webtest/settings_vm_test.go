package webtest

import (
	"testing"

	"github.com/dop251/goja"
)

// TestSettingsVMEvaluatesAndExposesUtilsHelpers pins the two things every
// caller of SettingsVM relies on: settings.js evaluates cleanly on top of
// utils.js, and utils.js's real exports — not stubs — are in scope by the
// time it does. snapshotRestartValues is the utils.js export
// internal/web/routes needed populateConfigForm's snapshotRestartValues call
// to resolve against the real helper rather than a ReferenceError.
func TestSettingsVMEvaluatesAndExposesUtilsHelpers(t *testing.T) {
	vm := SettingsVM(t)

	fn := vm.Get("snapshotRestartValues")
	if fn == nil {
		t.Fatal("snapshotRestartValues is not defined in the runtime — utils.js did not evaluate " +
			"or its export was not stripped correctly")
	}
	if _, ok := goja.AssertFunction(fn); !ok {
		t.Fatalf("snapshotRestartValues = %v, want a function", fn)
	}
}
