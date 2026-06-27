//go:build !windows

package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyUserOnlyDACLDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := ApplyUserOnlyDACL(dir); err != nil {
		t.Fatalf("ApplyUserOnlyDACL(dir): %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 0700", got)
	}
}

func TestApplyUserOnlyDACLFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.toml")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := ApplyUserOnlyDACL(path); err != nil {
		t.Fatalf("ApplyUserOnlyDACL(file): %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0600", got)
	}
}

func TestApplyUserOnlyDACLMissingPath(t *testing.T) {
	if err := ApplyUserOnlyDACL(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("expected error for missing path, got nil")
	}
}
