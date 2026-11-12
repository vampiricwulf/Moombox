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
