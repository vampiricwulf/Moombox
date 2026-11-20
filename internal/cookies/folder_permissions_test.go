package cookies

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cookieDirFixture returns a real, existing directory holding exactly the named
// entries (a name ending in "/" becomes a subdirectory, anything else a file).
func cookieDirFixture(t *testing.T, entries ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cookiedir")
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

// TestCookieWriteLeavesAnExistingFolderAlone is the owner ruling of
// 2026-11-19: Moombox never changes the permissions of a folder that already
// existed. A cookie write and its sidecar's, into a folder shaped like a
// default install and into one holding nothing but the cookie file, leave the
// folder reading back exactly as it did (the mode bits on POSIX, the whole
// security descriptor on Windows).
//
// Mutant: chmod or icacls the parent after the write, whatever the folder
// holds (the behaviour up to v2.9.9) or only when it is a dedicated one (the
// rule this replaced) -> the folder reads back changed.
func TestCookieWriteLeavesAnExistingFolderAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		mode    os.FileMode
	}{
		{"an install folder open to everyone", []string{"output/", "staging/", "moombox.db", "moombox.log"}, 0o777},
		{"a folder holding only the cookie file, open to its group", nil, 0o750},
		{"a folder holding only the cookie file, open to everyone", nil, 0o777},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := cookieDirFixture(t, tc.entries...)
			// Explicit: the mode handed to mkdir is trimmed by the umask.
			// (On Windows this changes nothing; the descriptor is what is
			// compared there.)
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			before := folderPermissions(t, dir)

			cookiePath := filepath.Join(dir, "cookies.txt")
			if err := writeFileAtomic(cookiePath, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
				t.Fatalf("writeFileAtomic: %v", err)
			}
			if err := SaveMeta(cookiePath, CookieMeta{}); err != nil {
				t.Fatalf("SaveMeta: %v", err)
			}

			if after := folderPermissions(t, dir); after != before {
				t.Errorf("the folder's permissions changed:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// TestCookieFolderMoomboxCreatesIsOwnerOnlyOnPOSIX: a folder Moombox creates
// for the cookie file is created 0700. Every site that creates one goes through
// createCookieDir.
//
// Mutant: create it 0755 (the mode every site used to pass) -> group and others
// can list and enter the folder.
func TestCookieFolderMoomboxCreatesIsOwnerOnlyOnPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are advisory on Windows; a created folder inherits its parent's permissions there")
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := SaveMeta(filepath.Join(dir, "cookies.txt"), CookieMeta{}); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("the created cookie folder is %v, want -rwx------", got)
	}
}

// TestProfileFolderIsRestrictedOnlyWhenMoomboxCreatesIt: the browser profile
// folder is an operator-configurable path, and the browser writes the files in
// it, so the folder is the only thing Moombox can restrict. It does so when it
// created the folder in that call, and leaves one that already existed exactly
// as it was. Real permissions, read back.
//
// Mutants: restrict whatever MkdirAll returns nil for (the old behaviour) ->
// the pre-existing folder reads back changed; never restrict -> the created
// folder reads back open.
func TestProfileFolderIsRestrictedOnlyWhenMoomboxCreatesIt(t *testing.T) {
	t.Run("created by Moombox", func(t *testing.T) {
		// Under a parent that does not exist either, as on a first setup.
		dir := filepath.Join(t.TempDir(), "data", "browser-profile")
		log := &infoRecorder{}
		if err := prepareProfileDir(dir, log); err != nil {
			t.Fatalf("prepareProfileDir: %v", err)
		}
		if got := log.msgs; len(got) != 1 || got[0] != "restricted folder to the current user" {
			t.Errorf("Info lines = %q, want the one saying the folder was restricted", got)
		}
		if !folderIsOwnerOnly(t, dir) {
			t.Errorf("the created profile folder is not restricted: %s", folderPermissions(t, dir))
		}
	})

	t.Run("already there", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "browser-profile")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		before := folderPermissions(t, dir)
		log := &infoRecorder{}
		if err := prepareProfileDir(dir, log); err != nil {
			t.Fatalf("prepareProfileDir: %v", err)
		}
		if got := log.msgs; len(got) != 0 {
			t.Errorf("Info lines = %q for a folder that was left alone, want none", got)
		}
		if after := folderPermissions(t, dir); after != before {
			t.Errorf("the folder's permissions changed:\n before %s\n after  %s", before, after)
		}
	})

	t.Run("a file is in the way", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "browser-profile")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := prepareProfileDir(path, nopLogger{}); err == nil {
			t.Error("prepareProfileDir returned nil for a path that is a file: the browser would be launched against it")
		}
	})
}

// infoRecorder is a logger that keeps the Info messages it is given.
type infoRecorder struct {
	nopLogger
	msgs []string
}

func (r *infoRecorder) Info(msg string, args ...any) { r.msgs = append(r.msgs, msg) }
