package cookies

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateBrowserPathRejectsEmpty(t *testing.T) {
	if err := ValidateBrowserPath(context.Background(), "", "firefox"); err == nil {
		t.Error("expected error for empty path, got nil")
	}
}

func TestValidateBrowserPathRejectsNonexistent(t *testing.T) {
	if err := ValidateBrowserPath(context.Background(), "/this/does/not/exist/anywhere", "firefox"); err == nil {
		t.Error("expected error for nonexistent path, got nil")
	}
}

func TestValidateBrowserPathRejectsUnknownType(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	if err := ValidateBrowserPath(context.Background(), exe, "not-a-real-browser"); err == nil {
		t.Error("expected error for unknown browser type, got nil")
	}
}

// On Unix a file without the executable bit is refused. Windows has no such
// bit and CreateProcess runs a PE whatever its extension, so there the rule
// is the .exe extension — an imported upload ("<title> [<id>].mp4") a LAN
// client planted must not be runnable as a "browser".
func TestValidateBrowserPathRejectsNonExecutable(t *testing.T) {
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		planted := filepath.Join(tmp, "planted [abc].mp4")
		if err := os.WriteFile(planted, []byte("MZ"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateBrowserPathQuick(planted, "chrome"); err == nil {
			t.Error("a non-.exe file was accepted as a browser on Windows")
		}
		return
	}
	plain := filepath.Join(tmp, "plain.txt")
	if err := os.WriteFile(plain, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBrowserPath(context.Background(), plain, "firefox"); err == nil {
		t.Error("expected error for non-executable file, got nil")
	}
}

// --- ValidateBrowserPathQuick ---

func TestValidateBrowserPathQuickRejectsEmpty(t *testing.T) {
	if err := ValidateBrowserPathQuick("", "firefox"); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestValidateBrowserPathQuickRejectsRelativePath(t *testing.T) {
	if err := ValidateBrowserPathQuick("./firefox", "firefox"); err == nil {
		t.Error("expected error for relative path, got nil")
	}
	if err := ValidateBrowserPathQuick("../bin/firefox", "firefox"); err == nil {
		t.Error("expected error for relative path, got nil")
	}
}

func TestValidateBrowserPathQuickAcceptsValidExe(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	// Should pass static checks but does NOT call --version
	if err := ValidateBrowserPathQuick(exe, "firefox"); err != nil {
		t.Errorf("expected nil error for the test binary: %v", err)
	}
}
