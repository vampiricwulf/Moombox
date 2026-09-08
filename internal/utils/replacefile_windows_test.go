//go:build windows

package utils

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// TestTransientReplaceErrorsOnWindows: the two refusals an antivirus or
// indexer produces while it briefly holds the target open are transient;
// a missing source is not.
func TestTransientReplaceErrorsOnWindows(t *testing.T) {
	transient := []error{
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: syscall.ERROR_ACCESS_DENIED},
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: errorSharingViolation},
	}
	for _, err := range transient {
		if !isTransientReplaceError(err) {
			t.Errorf("%v: want transient", err)
		}
	}
	permanent := []error{
		&os.LinkError{Op: "rename", Old: "a.tmp", New: "a", Err: syscall.ERROR_FILE_NOT_FOUND},
		errors.New("something else"),
		nil,
	}
	for _, err := range permanent {
		if isTransientReplaceError(err) {
			t.Errorf("%v: want permanent", err)
		}
	}
}
