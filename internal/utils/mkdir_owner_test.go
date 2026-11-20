package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Mutants this kills: MkdirAll(dir, 0o700) for the whole chain (the parent is
// then 0700 on POSIX); a chmod of an existing folder; a file at the path
// accepted as the folder.
func TestMkdirOwnerOnly(t *testing.T) {
	t.Run("a missing folder is created, its missing parent with the ordinary mode", func(t *testing.T) {
		root := t.TempDir()
		parent := filepath.Join(root, "moombox")
		dir := filepath.Join(parent, "secrets")
		if err := MkdirOwnerOnly(dir); err != nil {
			t.Fatalf("MkdirOwnerOnly: %v", err)
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("the folder was not created: %v", err)
		}
		if runtime.GOOS == "windows" {
			return // modes mean nothing there; the folder inherits
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("the folder's mode = %o, want 700", got)
		}
		pinfo, err := os.Stat(parent)
		if err != nil {
			t.Fatal(err)
		}
		if got := pinfo.Mode().Perm(); got&0o055 == 0 {
			t.Errorf("the parent's mode = %o: it was created for its owner only, and an output tree placed under it would be closed to everyone else", got)
		}
	})

	t.Run("a folder that is already there is left as it is", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "shared")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil { // past the umask
			t.Fatal(err)
		}
		before, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := MkdirOwnerOnly(dir); err != nil {
			t.Fatalf("MkdirOwnerOnly on an existing folder: %v", err)
		}
		after, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if before.Mode() != after.Mode() {
			t.Errorf("an existing folder's mode went from %v to %v", before.Mode(), after.Mode())
		}
	})

	t.Run("a file at the path is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cookies")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := MkdirOwnerOnly(path); err == nil {
			t.Fatal("a file at the folder's path was accepted as the folder")
		}
	})
}
