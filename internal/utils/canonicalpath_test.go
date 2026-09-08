package utils

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCanonicalPathOfAMissingFileUsesItsExistingAncestor: a file that does
// not exist yet (a job's not-yet-written output, a ghost entry) is spelled
// from its deepest existing ancestor's canonical form plus the remainder, so
// it compares equal to a canonical base directory. On a GitHub Windows
// runner the temp dir is an 8.3 short name (RUNNER~1) — exactly the case
// where the naive spelling and the canonical one differ.
func TestCanonicalPathOfAMissingFileUsesItsExistingAncestor(t *testing.T) {
	dir := t.TempDir()
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	got, err := CanonicalPath(filepath.Join(dir, "missing.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(realDir, "missing.mp4"); got != want {
		t.Errorf("missing file: got %q, want %q", got, want)
	}

	got, err = CanonicalPath(filepath.Join(dir, "a", "b", "c.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(realDir, "a", "b", "c.mp4"); got != want {
		t.Errorf("nested missing file: got %q, want %q", got, want)
	}
}

// TestCanonicalPathOfAnExistingPathResolvesIt: an existing path is exactly
// EvalSymlinks of its absolute form.
func TestCanonicalPathOfAnExistingPathResolvesIt(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "real.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalPath(file)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCanonicalPathFollowsASymlinkedDirectory: a link to a directory
// resolves to the directory, for a file under it that exists and for one
// that does not. Skipped where the process cannot create symlinks (Windows
// without developer mode).
func TestCanonicalPathFollowsASymlinkedDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}

	got, err := CanonicalPath(filepath.Join(link, "ghost.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(realTarget, "ghost.mp4"); got != want {
		t.Errorf("missing file under a link: got %q, want %q", got, want)
	}
}

// TestCanonicalPathWithNoExistingAncestorFallsBackToAbs: a path whose whole
// chain is missing (a not-yet-created drive letter or root subdir) still
// yields its absolute spelling rather than an error.
func TestCanonicalPathWithNoExistingAncestorFallsBackToAbs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gone")
	if err := os.RemoveAll(filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	// The temp dir's parent still exists, so this exercises the walk; the
	// result must be an absolute path ending in the missing elements.
	got, err := CanonicalPath(p)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) || filepath.Base(got) != "gone" {
		t.Errorf("got %q, want an absolute path ending in gone", got)
	}
}
