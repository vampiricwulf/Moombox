//go:build !windows

package routes

import (
	"errors"
	"testing"
)

func TestRunElevatedNotSupported(t *testing.T) {
	_, err := runElevated("echo hi")
	if err == nil {
		t.Fatal("expected error for runElevated on non-Windows, got nil")
	}
	if !errors.Is(err, errElevationNotSupported) {
		t.Errorf("expected errElevationNotSupported, got: %v", err)
	}
}

func TestIsElevatedReportsRoot(t *testing.T) {
	// Just verify the function returns without panicking. The actual
	// boolean depends on test runner UID; we don't assert a specific
	// value because CI may run as either root or unprivileged.
	_ = isElevated()
}
