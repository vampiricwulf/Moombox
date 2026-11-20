package cookies

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

// TestWriteFileAtomicRestrictsTheTempFileBeforeTheRename pins WHERE the
// per-file restriction runs: on the temp file, in the target's folder, while
// the target still holds its old bytes. Restricting the target after the rename
// would leave the new cookies readable through the folder's inherited
// permissions for as long as that takes, and for good when it fails.
//
// Mutants: call it after utils.ReplaceFile, or on `path` -> the path seen is
// the target and the target already holds the new bytes.
func TestWriteFileAtomicRestrictsTheTempFileBeforeTheRename(t *testing.T) {
	dir := cookieDirFixture(t, "output/")
	target := filepath.Join(dir, "cookies.txt")
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

	if err := writeFileAtomic(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("the file restriction ran on %v, want exactly one file", seen)
	}
	if seen[0] == target || filepath.Dir(seen[0]) != dir || !strings.HasSuffix(seen[0], ".tmp") {
		t.Errorf("the file restriction ran on %q, want the temp file beside %q", seen[0], target)
	}
	if targetAtCall != "old" {
		t.Errorf("the target held %q when the restriction ran, want the old bytes: it must run before the rename", targetAtCall)
	}
}

// TestWriteFileAtomicSurvivesAFailedFileRestriction: the restriction is
// hardening, the cookies are the session. A file system that refuses the DACL
// (a FAT or network volume) must still get its cookie file.
//
// Mutant: return the restriction's error -> the write fails and the target is
// never written.
func TestWriteFileAtomicSurvivesAFailedFileRestriction(t *testing.T) {
	dir := cookieDirFixture(t, "output/")
	target := filepath.Join(dir, "cookies.txt")
	fakeRestrictFileToOwner(t, func(string) error { return errors.New("this volume has no ACLs") })

	if err := writeFileAtomic(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic failed because the file restriction did: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Errorf("target = %q, %v; want the new bytes", got, err)
	}
}

// recordingHandler collects the records the package logs through slog's
// default logger.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

// restrictedLines returns the level of each "restricted folder" record logged
// for dir.
func (h *recordingHandler) restrictedLines(dir string) []slog.Level {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Level
	for _, r := range h.records {
		if r.Message != "restricted folder to the current user" {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "dir" && a.Value.String() == dir {
				out = append(out, r.Level)
			}
			return true
		})
	}
	return out
}

// captureSlog routes slog's default logger to a recorder for one test.
func captureSlog(t *testing.T) *recordingHandler {
	t.Helper()
	h := &recordingHandler{}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(h))
	return h
}

// TestCookieDirLockdownIsReportedOnce: changing a folder's permissions used to
// log nothing at all, which is how an install folder lost SYSTEM and
// Administrators without a trace. A lockdown that SUCCEEDS says so at Info,
// once per folder, however many cookie writes follow; one that fails says
// nothing at Info (it is retried, and it changed nothing).
//
// Mutants: log on the failure arm too -> a line after the first write; drop
// the line -> none at the end; log per write rather than per successful apply
// -> more than one at the end.
func TestCookieDirLockdownIsReportedOnce(t *testing.T) {
	dir := cookieDirFixture(t)
	target := filepath.Join(dir, "cookies.txt")
	logs := captureSlog(t)
	fail := true
	var mu sync.Mutex
	fakeApplyUserOnlyDACL(t, func(string) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return errors.New("access denied")
		}
		return nil
	})

	write := func() {
		t.Helper()
		if err := writeFileAtomic(target, []byte("x"), 0o600); err != nil {
			t.Fatalf("writeFileAtomic: %v", err)
		}
		awaitSettled(t, dir)
	}

	write()
	if got := logs.restrictedLines(dir); len(got) != 0 {
		t.Fatalf("a FAILED lockdown was reported as done: %v", got)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	write()
	write()
	write()

	got := logs.restrictedLines(dir)
	if len(got) != 1 {
		t.Fatalf("%d \"restricted folder\" lines for %s across three writes after the lockdown succeeded, want exactly 1", len(got), dir)
	}
	if got[0] != slog.LevelInfo {
		t.Errorf("the line was logged at %v, want Info", got[0])
	}
}
