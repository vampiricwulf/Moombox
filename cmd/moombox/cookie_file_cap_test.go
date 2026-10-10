package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// E I reads the file at a path the operator typed. The Web import caps a
// cookie file at routes.MaxCookieImportBytes; the TUI read whatever it was
// pointed at, so a mistyped path to a recording was read whole into memory.
// It is refused now, by size, without its bytes reaching the error.
//
// Mutant: the cap removed — the oversize file is returned.
func TestReadCookieFileCappedRefusesAnOversizeFile(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(ok, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readCookieFileCapped(ok); err != nil || !strings.HasPrefix(string(data), "# Netscape") {
		t.Fatalf("a cookie-sized file: %q, %v", data, err)
	}

	big := filepath.Join(dir, "stream.mp4")
	if err := os.WriteFile(big, []byte(strings.Repeat("SECRETBYTES", routes.MaxCookieImportBytes/10)), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readCookieFileCapped(big)
	if err == nil || data != nil {
		t.Fatalf("an oversize file was read: %d bytes, err %v", len(data), err)
	}
	if strings.Contains(err.Error(), "SECRETBYTES") {
		t.Errorf("the error carries the file's bytes: %v", err)
	}
	if _, err := readCookieFileCapped(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing file read without error")
	}
}
