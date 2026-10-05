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

	debugReaches := func(marker string) bool {
		t.Helper()
		lg.Debug(marker)
		deadline := time.After(time.Second)
		for {
			select {
			case l := <-lines:
				if strings.Contains(l, marker) {
					return true
				}
			case <-deadline:
				return false
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
