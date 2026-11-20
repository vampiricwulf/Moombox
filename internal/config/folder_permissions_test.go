package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// configDirFixture returns a real, existing directory holding exactly the named
// entries (a name ending in "/" becomes a subdirectory, anything else a file).
func configDirFixture(t *testing.T, entries ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "confdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, filepath.FromSlash(e))
		if strings.HasSuffix(e, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestSaveLeavesAnExistingFolderAlone is the owner ruling of 2026-11-19:
// Moombox never changes the permissions of a folder that already existed. A
// settings save into a folder shaped like a default install, and into one
// holding nothing else, leaves the folder reading back exactly as it did (the
// mode bits on POSIX, the whole security descriptor on Windows).
//
// Mutant: chmod or icacls the folder in Save, whatever it holds (the behaviour
// up to v2.9.9) or only when it is a dedicated one (the rule this replaced) ->
// the folder reads back changed.
func TestSaveLeavesAnExistingFolderAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		mode    os.FileMode
	}{
		{"an install folder open to everyone", []string{"output/", "staging/", "moombox.db", "moombox.log"}, 0o777},
		{"a folder holding only the config file, open to its group", nil, 0o750},
		{"a folder holding only the config file, open to everyone", nil, 0o777},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := configDirFixture(t, tc.entries...)
			// Explicit: the mode handed to mkdir is trimmed by the umask.
			// (On Windows this changes nothing; the descriptor is what is
			// compared there.)
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			before := folderPermissions(t, dir)

			if err := Save(Defaults(), filepath.Join(dir, "config.toml")); err != nil {
				t.Fatalf("Save: %v", err)
			}

			if after := folderPermissions(t, dir); after != before {
				t.Errorf("the folder's permissions changed:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// TestSaveCreatesItsFolderOwnerOnlyOnPOSIX: a folder Save has to create for
// config.toml is created 0700.
//
// Mutant: create it 0755 (what Save used to pass, relying on a chmod
// afterwards) -> group and others can list and enter the folder.
func TestSaveCreatesItsFolderOwnerOnlyOnPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are advisory on Windows; a created folder inherits its parent's permissions there")
	}
	dir := filepath.Join(t.TempDir(), "moombox")
	if err := Save(Defaults(), filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("the created config folder is %v, want -rwx------", got)
	}
}
