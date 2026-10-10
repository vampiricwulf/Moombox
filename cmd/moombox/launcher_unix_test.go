//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// On Linux .old is the only rollback artifact there has ever been: the
// restart that finds it names it as the update's artifact, and a restart
// that finds none is a config restart with no artifact at all.
//
// Mutants: handleUpdateRestart returning exePath+"~" for an update — the
// artifact names a file Linux never writes; returning oldPath whatever the
// stat says — a config restart arms the post-update window.
func TestHandleUpdateRestartNamesTheOldFile(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), "moombox")
	if got := handleUpdateRestart(exePath); got != "" {
		t.Errorf("a restart with no .old = %q, want no artifact (a config restart)", got)
	}
	if err := os.WriteFile(exePath+".old", []byte("previous release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := handleUpdateRestart(exePath), exePath+".old"; got != want {
		t.Errorf("a restart with a .old = %q, want %q", got, want)
	}
}
