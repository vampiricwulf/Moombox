package utils

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var errFakeTransient = errors.New("fake: the file is briefly held by another process")

// stubReplaceFile swaps the rename, classifier and sleep seams for one test.
func stubReplaceFile(t *testing.T, rename func(tmp, path string) error, transient func(error) bool) *[]time.Duration {
	t.Helper()
	prevRename, prevTransient, prevSleep := renameFile, isTransientReplaceError, replaceFileSleep
	slept := []time.Duration{}
	renameFile = rename
	isTransientReplaceError = transient
	replaceFileSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() {
		renameFile, isTransientReplaceError, replaceFileSleep = prevRename, prevTransient, prevSleep
	})
	return &slept
}

// TestReplaceFileRetriesATransientRefusalThenSucceeds: a rename refused a few
// times by a transient error (an antivirus holding the target open on
// Windows) is retried with a growing pause and succeeds without the caller
// ever seeing the refusal.
func TestReplaceFileRetriesATransientRefusalThenSucceeds(t *testing.T) {
	calls := 0
	slept := stubReplaceFile(t, func(string, string) error {
		calls++
		if calls < 4 {
			return errFakeTransient
		}
		return nil
	}, func(err error) bool { return errors.Is(err, errFakeTransient) })

	if err := ReplaceFile("a.tmp", "a"); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	if calls != 4 {
		t.Errorf("rename calls = %d, want 4 (three refusals, then success)", calls)
	}
	if len(*slept) != 3 {
		t.Fatalf("slept %d times, want 3 (once per refusal)", len(*slept))
	}
	for i := 1; i < len(*slept); i++ {
		if (*slept)[i] <= (*slept)[i-1] {
			t.Errorf("pause %d (%v) must grow over pause %d (%v)", i, (*slept)[i], i-1, (*slept)[i-1])
		}
	}
}

// TestReplaceFileGivesUpAfterTheBudget: a refusal that never clears is
// reported after the attempt budget, with the last error, and the total
// pause stays short enough for an interactive path (a cookie rollback).
func TestReplaceFileGivesUpAfterTheBudget(t *testing.T) {
	calls := 0
	slept := stubReplaceFile(t, func(string, string) error {
		calls++
		return errFakeTransient
	}, func(err error) bool { return errors.Is(err, errFakeTransient) })

	err := ReplaceFile("a.tmp", "a")
	if !errors.Is(err, errFakeTransient) {
		t.Fatalf("error = %v, want the last refusal", err)
	}
	if calls != replaceFileAttempts {
		t.Errorf("rename calls = %d, want the budget %d", calls, replaceFileAttempts)
	}
	var total time.Duration
	for _, d := range *slept {
		total += d
	}
	if total > 3*time.Second {
		t.Errorf("total pause %v exceeds 3 s — too long for a rollback the user is waiting on", total)
	}
}

// TestReplaceFileDoesNotRetryAPermanentError: anything the classifier does
// not call transient (a missing source, a read-only target dir) is returned
// on the first attempt.
func TestReplaceFileDoesNotRetryAPermanentError(t *testing.T) {
	calls := 0
	permanent := errors.New("fake: no such file")
	slept := stubReplaceFile(t, func(string, string) error {
		calls++
		return permanent
	}, func(error) bool { return false })

	if err := ReplaceFile("a.tmp", "a"); !errors.Is(err, permanent) {
		t.Fatalf("error = %v, want the permanent error", err)
	}
	if calls != 1 || len(*slept) != 0 {
		t.Errorf("calls = %d, pauses = %d; a permanent error must not be retried", calls, len(*slept))
	}
}

// TestReplaceFileRenamesForReal: the happy path against the filesystem — the
// temp file replaces the target and is gone afterwards.
func TestReplaceFileRenamesForReal(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "cookies.txt.123.tmp")
	target := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(tmp, target); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Fatalf("target = %q, %v; want the temp file's content", got, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp file still present after the replace (stat err %v)", err)
	}
}
