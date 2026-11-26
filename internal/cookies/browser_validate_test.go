package cookies

import (
	"context"
	"errors"
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

// A file name an import can write is refused on every platform, before the
// file is looked at (owner ruling R3-5). On a filesystem without POSIX modes
// (CIFS, exFAT, ntfs-3g, a Docker Desktop bind mount) an imported file
// reports the executable bit, so the Unix mode check alone does not stop a
// planted upload. The rule is on the name, so it holds wherever the output
// folder is and whatever it is later moved to.
func TestValidateBrowserPathQuickRefusesNamesAnImportWrites(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"planted [abc].mp4",
		"planted [abc].MKV",
		"planted [abc].mKv", // KELVIN SIGN lower-cases to "k", as the import folds it
		"planted [abc].webm",
		"planted [abc].ts",
		"planted [abc] - part2.mp4",
		"planted [abc].chat.json",
		"moombox-import-123.partial",
		"moombox-import-123.zip",
	} {
		err := ValidateBrowserPathQuick(filepath.Join(root, name), "firefox")
		if !errors.Is(err, errImportWrittenName) {
			t.Errorf("%q: err = %v, want the import-name refusal", name, err)
		}
	}
	for _, name := range []string{"firefox", "firefox-bin", "Brave.AppImage", "chrome.exe", "vivaldi.sh", "start.json"} {
		err := ValidateBrowserPathQuick(filepath.Join(root, name), "firefox")
		if errors.Is(err, errImportWrittenName) {
			t.Errorf("%q: refused as an import name; no import writes it", name)
		}
	}
}

// The same rule on a file that passes every other check on Unix: executable
// bit set, as a filesystem without POSIX modes reports it.
func TestValidateBrowserPathQuickRefusesAnExecutableImportName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses any non-.exe; the name rule is pinned above")
	}
	planted := filepath.Join(t.TempDir(), "planted [abc].mp4")
	if err := os.WriteFile(planted, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBrowserPathQuick(planted, "firefox"); !errors.Is(err, errImportWrittenName) {
		t.Errorf("an executable file named like an import was accepted: %v", err)
	}
}
