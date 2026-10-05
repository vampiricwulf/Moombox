package main

import (
	"testing"
	"time"
)

// TestClassifyChildExit pins the launcher's exit policy, whose rule ORDER is
// the policy. The startup-error rows are the regression: the child waits for
// a keypress before exiting 3, so ranFor measures the operator, and an
// operator who took over a minute to press Enter used to be crash-respawned
// into the same prompt five times (and, after an update, lost the rollback
// artifact to the healthy-window rule).
//
// Mutants: drop the `code == exitCodeStartupError` rule (the slow-keypress
// row becomes a crash); drop the `|| code == exitCodeStartupError` clause of
// the post-update rule (the slow post-update row propagates and deletes the
// artifact instead of preserving it).
func TestClassifyChildExit(t *testing.T) {
	const quick, slow = 2 * time.Second, 5 * time.Minute
	for _, tc := range []struct {
		name               string
		code               int
		ranFor             time.Duration
		firstAfterUpdate   bool
		terminating        bool
		wasRespawn         bool
		consecutiveCrashes int
		want               childExitAction
	}{
		{"restart", exitCodeRestart, slow, false, false, false, 0, childRestart},
		{"clean quit", 0, slow, false, false, false, 0, childPropagate},
		{"forwarded stop beats a failed update", 1, quick, true, true, false, 0, childPropagate},
		{"fresh update dies quickly", 1, quick, true, false, false, 0, childPostUpdateFailure},
		{"fresh update dies late", 1, slow, true, false, false, 0, childCrash},
		{"fresh update startup error, slow keypress", exitCodeStartupError, slow, true, false, false, 0, childPostUpdateFailure},
		{"SIGINT convention", 130, slow, false, false, false, 0, childPropagate},
		{"startup error, quick keypress", exitCodeStartupError, quick, false, false, false, 0, childPropagate},
		{"startup error, slow keypress", exitCodeStartupError, slow, false, false, false, 0, childPropagate},
		{"startup error on a respawn", exitCodeStartupError, quick, false, false, true, 1, childPropagate},
		{"fresh launch fails fast", 1, quick, false, false, false, 0, childPropagate},
		{"respawned child dies quickly", 2, quick, false, false, true, 1, childCrash},
		{"healthy child crashes", 2, slow, false, false, false, 0, childCrash},
	} {
		got := classifyChildExit(tc.code, tc.ranFor, tc.firstAfterUpdate, tc.terminating, tc.wasRespawn, tc.consecutiveCrashes)
		if got != tc.want {
			t.Errorf("%s: classifyChildExit = %v, want %v", tc.name, got, tc.want)
		}
	}
}
