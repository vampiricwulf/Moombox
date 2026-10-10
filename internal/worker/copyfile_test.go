package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noTempLeftIn fails the test when dir holds a copyFile temp — the
// "<base>.<random>.tmp" os.CreateTemp shape.
func noTempLeftIn(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// TestCopyFileLeavesNoTruncatedDestination pins copyFile's atomicity: a copy
// that fails part-way leaves dst exactly as it was and no temp beside it.
// copyFile used to os.Create(dst) first, so a failed copy left a TRUNCATED
// dst — and copyKeptChatSidecar, which skips the copy when dst exists, then
// kept that corrupt chat file beside the archive for good.
//
// The failing source is a directory: it opens, but the read fails on every
// platform (EISDIR on Linux, "Incorrect function" on Windows), which is the
// mid-stream failure the old code turned into an empty dst.
//
// MUTANT: os.Create(dst) + io.Copy — dst reads "" after the failed copy.
func TestCopyFileLeavesNoTruncatedDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "a.chat.json")
	if err := os.WriteFile(dst, []byte("OLD"), 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	badSrc := filepath.Join(dir, "srcdir")
	if err := os.Mkdir(badSrc, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := copyFile(badSrc, dst); err == nil {
		t.Fatal("copyFile from a directory = nil, want an error")
	}
	if got, _ := os.ReadFile(dst); string(got) != "OLD" {
		t.Errorf("dst after a failed copy = %q, want the previous \"OLD\" untouched", got)
	}
	noTempLeftIn(t, dir)

	// A copy that succeeds replaces the old content whole, and still leaves
	// no temp behind.
	src := filepath.Join(dir, "src.json")
	if err := os.WriteFile(src, []byte("NEW CONTENT"), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile = %v, want nil", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "NEW CONTENT" {
		t.Errorf("dst after a good copy = %q, want \"NEW CONTENT\"", got)
	}
	noTempLeftIn(t, dir)
}

// TestCopyFileMissingSourceCreatesNothing pins the first-step failure: an
// unreadable source must not create dst or a temp at all.
func TestCopyFileMissingSourceCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.json")
	if err := copyFile(filepath.Join(dir, "missing"), dst); err == nil {
		t.Fatal("copyFile from a missing source = nil, want an error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst exists after a failed open (stat err = %v), want absent", err)
	}
	noTempLeftIn(t, dir)
}
