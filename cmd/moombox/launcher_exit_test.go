package main

import (
	"os"
	"path/filepath"
	"strings"
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

// TestSweptFailedReleaseNote: the boot that announces an auto-rollback marker
// has already swept <exe>.failed at its first-successful-boot milestone, so a
// marker saying the release was "KEPT at" that path needs a note saying it is
// gone — and only then.
//
// Mutant: drop the os.Stat check — the note is appended while the file still
// exists.
func TestSweptFailedReleaseNote(t *testing.T) {
	dir := t.TempDir()
	failed := filepath.Join(dir, "moombox.failed")
	marker := "The failed release was KEPT at:\n  " + failed + "\n"

	if note := sweptFailedReleaseNote(marker, failed); !strings.Contains(note, "has since been removed") {
		t.Errorf("note for a swept release = %q, want it to say the file was removed", note)
	}
	if err := os.WriteFile(failed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if note := sweptFailedReleaseNote(marker, failed); note != "" {
		t.Errorf("note while the release is still there = %q, want none", note)
	}
	if note := sweptFailedReleaseNote("The failed release could not be kept aside and was REMOVED.", filepath.Join(dir, "gone.failed")); note != "" {
		t.Errorf("note for a marker that names no kept release = %q, want none", note)
	}
}
