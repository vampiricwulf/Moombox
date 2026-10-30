package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/logger"
)

// A -log-level override lives on the running logger only. A save that leaves
// logs.log_level as it was must not end it — the TUI save used to re-apply
// the configured level on every save, so saving any setting from the
// terminal silently dropped a debug session — while a save that changes the
// level is the operator choosing one, and applies.
//
// Mutant: applyConfiguredLogLevel setting the level unconditionally — the
// debug line after the unrelated save is lost.
func TestASaveKeepsTheLogLevelOverrideUnlessTheLevelChanged(t *testing.T) {
	lg, err := logger.New(filepath.Join(t.TempDir(), "moombox.log"), "DEBUG", 1<<20, 1) // the override
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	lines := lg.Subscribe()
	s := &runState{log: lg, configuredLogLevel: "INFO"}

	// debugReaches logs marker at DEBUG, then a sentinel at ERROR, which
	// every level lets through, and reports whether the marker reached the
	// subscriber ahead of the sentinel. A subscriber gets lines in the order
	// they were logged, so the sentinel's arrival settles it: the check no
	// longer waits out a whole second for a line that must not come.
	debugReaches := func(marker string) bool {
		t.Helper()
		sentinel := "sentinel-after-" + marker
		lg.Debug(marker)
		lg.Error(sentinel)
		deadline := time.After(5 * time.Second)
		seen := false
		for {
			select {
			case l := <-lines:
				switch {
				case strings.Contains(l, sentinel):
					return seen
				case strings.Contains(l, marker):
					seen = true
				}
			case <-deadline:
				t.Fatalf("the ERROR line logged after %q never reached the subscriber", marker)
			}
		}
	}

	s.applyConfiguredLogLevel("INFO") // a save of some other setting
	if !debugReaches("after-unrelated-save") {
		t.Error("a save that left log_level unchanged ended the -log-level override")
	}
	s.applyConfiguredLogLevel("WARN") // the operator picks a level
	if debugReaches("after-level-change") {
		t.Error("a changed log_level did not reach the running logger")
	}
}
