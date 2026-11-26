package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestInstanceLockRefusalHintsOnlyWhenAnotherInstanceHoldsIt: the launcher
// printed "wait a few seconds and try again" after every failure to take the
// lock, including a home directory it could not create, where waiting changes
// nothing.
//
// Mutants: the hint printed unconditionally, or never; the error's own text
// replaced.
func TestInstanceLockRefusalHintsOnlyWhenAnotherInstanceHoldsIt(t *testing.T) {
	held := instanceRunningError("another moombox instance is already running in this session")
	env := fmt.Errorf("create lock dir %q: %w", "/home/moombox/.local/share/moombox", errors.New("permission denied"))

	got := instanceLockRefusal(held)
	if len(got) != 2 || got[0] != held.Error() || !strings.Contains(got[1], "wait a few seconds") {
		t.Errorf("held lock: %q, want its text then the wait hint", got)
	}
	got = instanceLockRefusal(env)
	if len(got) != 1 || got[0] != env.Error() {
		t.Errorf("environment failure: %q, want its text alone", got)
	}
}
