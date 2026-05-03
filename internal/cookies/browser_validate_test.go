package cookies

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateBrowserPathRejectsEmpty(t *testing.T) {
	if err := ValidateBrowserPath("", "firefox"); err == nil {
		t.Error("expected error for empty path, got nil")
	}
}

func TestValidateBrowserPathRejectsNonexistent(t *testing.T) {
	if err := ValidateBrowserPath("/this/does/not/exist/anywhere", "firefox"); err == nil {
		t.Error("expected error for nonexistent path, got nil")
	}
}

func TestValidateBrowserPathRejectsUnknownType(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	if err := ValidateBrowserPath(exe, "not-a-real-browser"); err == nil {
		t.Error("expected error for unknown browser type, got nil")
	}
}

func TestValidateBrowserPathRejectsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows treats all files as potentially executable")
	}
	tmp := t.TempDir()
	plain := filepath.Join(tmp, "plain.txt")
	if err := os.WriteFile(plain, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBrowserPath(plain, "firefox"); err == nil {
		t.Error("expected error for non-executable file, got nil")
	}
}
