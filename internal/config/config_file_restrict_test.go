package config

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRestrictFileToOwner swaps the restrictFileToOwner seam for one test.
func fakeRestrictFileToOwner(t *testing.T, fn func(path string) error) {
	t.Helper()
	real := restrictFileToOwner
	t.Cleanup(func() { restrictFileToOwner = real })
	restrictFileToOwner = fn
}

// TestSaveRestrictsTheTempFileBeforeTheRename pins where Save's per-file
// restriction runs: on the temp file beside config.toml, while config.toml
// still holds what it held. config.toml carries the password hash; restricted
// only after the rename it would sit under the folder's inherited permissions
// in between, and for good when the restriction fails.
//
// Mutants: restrict `path` after utils.ReplaceFile -> the path seen is
// config.toml and it already holds the new settings; drop the call -> nothing
// is seen.
func TestSaveRestrictsTheTempFileBeforeTheRename(t *testing.T) {
	dir := configDirFixture(t, "output/")
	target := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	var targetAtCall string
	fakeRestrictFileToOwner(t, func(path string) error {
		seen = append(seen, path)
		b, _ := os.ReadFile(target)
		targetAtCall = string(b)
		return nil
	})

	if err := Save(Defaults(), target); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("the file restriction ran on %v, want exactly one file", seen)
	}
	if seen[0] == target || filepath.Dir(seen[0]) != dir || !strings.HasSuffix(seen[0], ".tmp") {
		t.Errorf("the file restriction ran on %q, want the temp file beside %q", seen[0], target)
	}
	if targetAtCall != "old" {
		t.Errorf("config.toml held %q when the restriction ran, want its old bytes: it must run before the rename", targetAtCall)
	}
}

// TestSaveSurvivesAFailedFileRestriction: a volume that refuses the DACL still
// gets its settings saved.
//
// Mutant: return the restriction's error -> Save fails and nothing is written.
func TestSaveSurvivesAFailedFileRestriction(t *testing.T) {
	dir := configDirFixture(t, "output/")
	target := filepath.Join(dir, "config.toml")
	fakeRestrictFileToOwner(t, func(string) error { return errors.New("this volume has no ACLs") })

	if err := Save(Defaults(), target); err != nil {
		t.Fatalf("Save failed because the file restriction did: %v", err)
	}
	if _, err := Load(target); err != nil {
		t.Errorf("the saved config does not load: %v", err)
	}
}

// restrictedLineRecorder counts the "restricted folder" records logged through
// slog's default logger, by level, for one folder.
type restrictedLineRecorder struct {
	mu     sync.Mutex
	dir    string
	levels []slog.Level
}

func (h *restrictedLineRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *restrictedLineRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *restrictedLineRecorder) WithGroup(string) slog.Handler            { return h }
func (h *restrictedLineRecorder) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "restricted folder to the current user" {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "dir" && a.Value.String() == h.dir {
			h.mu.Lock()
			h.levels = append(h.levels, r.Level)
			h.mu.Unlock()
		}
		return true
	})
	return nil
}

func (h *restrictedLineRecorder) seen() []slog.Level {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Level(nil), h.levels...)
}

func captureRestrictedLines(t *testing.T, dir string) *restrictedLineRecorder {
	t.Helper()
	h := &restrictedLineRecorder{dir: dir}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(h))
	return h
}

// TestSaveReportsADirLockdownOnce: a lockdown that succeeded changes who can
// reach the config folder, and it used to log nothing. It is said at Info,
// once, however many saves follow.
//
// Mutants: drop the line -> none; log outside the once-per-directory block ->
// three.
func TestSaveReportsADirLockdownOnce(t *testing.T) {
	dir := configDirFixture(t, "cookies.txt")
	logs := captureRestrictedLines(t, dir)
	fakeApplyUserOnlyDACL(t, func(string) error { return nil })

	for range 3 {
		if err := Save(Defaults(), filepath.Join(dir, "config.toml")); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	got := logs.seen()
	if len(got) != 1 {
		t.Fatalf("%d \"restricted folder\" lines across three saves, want exactly 1", len(got))
	}
	if got[0] != slog.LevelInfo {
		t.Errorf("the line was logged at %v, want Info", got[0])
	}
}

// TestSaveDoesNotReportALockdownThatFailed: the line states a fact about the
// folder. A refused icacls changed nothing.
//
// Mutant: log whatever the apply returned -> one line here.
func TestSaveDoesNotReportALockdownThatFailed(t *testing.T) {
	dir := configDirFixture(t, "cookies.txt")
	logs := captureRestrictedLines(t, dir)
	fakeApplyUserOnlyDACL(t, func(string) error { return errors.New("access denied") })

	if err := Save(Defaults(), filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got := logs.seen(); len(got) != 0 {
		t.Errorf("a failed lockdown was reported as done: %v", got)
	}
}
