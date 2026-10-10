//go:build windows

package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCanonicalPathKeepsAJunction pins what CanonicalPath does with a
// Windows junction, which its doc used to say it resolved. Since Go 1.23
// filepath.EvalSymlinks returns a mount point as it is spelled and refuses a
// path that continues below one, so a path through a junction is spelled
// through it — the junction normalised with what is above it, what is below
// it appended — and the junction's target is a different spelling. The path
// guards canonicalise both sides through CanonicalPath, so a file named
// through the junction is inside the junction's spelling of a directory, and
// one named through the target is not.
//
// mklink is cmd.exe's builtin; where it cannot make a junction this is a
// skip (junctions need no Developer Mode, so on the CI runner it runs).
//
// Mutant: CanonicalPath resolving a junction (os.Readlink on the ancestor the
// walk stops at, and the remainder joined onto its target) — every row
// through the link reads as the target.
func TestCanonicalPathKeepsAJunction(t *testing.T) {
	tmp := t.TempDir()
	canonTmp, err := filepath.EvalSymlinks(tmp) // the runner's temp dir is an 8.3 name
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "file.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction on this host: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	for _, tc := range []struct{ name, path, want string }{
		{"the junction itself", link, filepath.Join(canonTmp, "link")},
		{"a file below it", filepath.Join(link, "file.mp4"), filepath.Join(canonTmp, "link", "file.mp4")},
		{"a missing file below it", filepath.Join(link, "missing", "x.mp4"), filepath.Join(canonTmp, "link", "missing", "x.mp4")},
		{"the same file through the target", filepath.Join(target, "file.mp4"), filepath.Join(canonTmp, "target", "file.mp4")},
	} {
		got, err := CanonicalPath(tc.path)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: CanonicalPath(%q) = %q, want %q", tc.name, tc.path, got, tc.want)
		}
	}
}
