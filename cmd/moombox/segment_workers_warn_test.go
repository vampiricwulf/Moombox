package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/logger"
)

// warnSegmentWorkers is what boot and both save paths call; it warns above
// the threshold and stays quiet at or below it.
//
// Mutant: the threshold comparison inverted — the quiet case warns.
func TestWarnSegmentWorkersAboveTheThresholdOnly(t *testing.T) {
	lg, err := logger.New(filepath.Join(t.TempDir(), "moombox.log"), "INFO", 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	lines := lg.Subscribe()
	s := &runState{log: lg}

	// Reads up to this call's marker, so consecutive calls never see each
	// other's lines.
	warned := func(n int) bool {
		t.Helper()
		s.warnSegmentWorkers(n)
		lg.Info("marker")
		saw := false
		for {
			select {
			case l := <-lines:
				if strings.Contains(l, "segment_workers") {
					saw = true
				}
				if strings.Contains(l, "marker") {
					return saw
				}
			case <-time.After(time.Second):
				t.Fatal("no log line arrived")
			}
		}
	}
	if warned(config.SegmentWorkersWarnThreshold) {
		t.Error("warned at the threshold")
	}
	if !warned(config.SegmentWorkersWarnThreshold + 1) {
		t.Error("no warning above the threshold")
	}
	// The TUI save calls this on every save; only a value that moved warns.
	// Mutant: the segWorkersSeen gate removed — the unchanged save warns.
	if warned(config.SegmentWorkersWarnThreshold + 1) {
		t.Error("a save that did not move segment_workers warned again")
	}
	if !warned(config.SegmentWorkersWarnThreshold + 2) {
		t.Error("a changed high value did not warn")
	}
}
