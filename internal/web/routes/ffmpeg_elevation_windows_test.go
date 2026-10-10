//go:build windows

package routes

import (
	"reflect"
	"testing"
	"unsafe"
)

// TestShellExecuteInfoLayout pins SHELLEXECUTEINFOW's x64 layout and that the
// string fields stay pointers. Stored as uintptr, runElevated's UTF-16
// buffers had no reference the garbage collector could see once the struct
// was built, so a collection during ShellExecuteExW could free them.
//
// Mutant: declare lpVerb as uintptr again — the kind check fails.
func TestShellExecuteInfoLayout(t *testing.T) {
	if got := unsafe.Sizeof(shellExecuteInfo{}); got != 112 {
		t.Fatalf("sizeof(SHELLEXECUTEINFOW) = %d, want 112", got)
	}
	typ := reflect.TypeOf(shellExecuteInfo{})
	for name, offset := range map[string]uintptr{
		"lpVerb":       16,
		"lpFile":       24,
		"lpParameters": 32,
		"lpDirectory":  40,
		"nShow":        48,
		"lpClass":      72,
		"hProcess":     104,
	} {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Fatalf("no field %s", name)
		}
		if f.Offset != offset {
			t.Errorf("%s at offset %d, want %d", name, f.Offset, offset)
		}
	}
	for _, name := range []string{"lpVerb", "lpFile", "lpParameters", "lpDirectory", "lpClass"} {
		f, _ := typ.FieldByName(name)
		if f.Type.Kind() != reflect.Pointer {
			t.Errorf("%s is %s; a string the API reads must be a pointer the garbage collector traces", name, f.Type)
		}
	}
}
