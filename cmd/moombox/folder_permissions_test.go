package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// infoRecorder is a logger that keeps what was logged at Info.
type infoRecorder struct {
	lines []string
}

func (r *infoRecorder) Info(msg string, args ...any) {
	r.lines = append(r.lines, fmt.Sprint(append([]any{msg}, args...)...))
}

// TestUninheritedSharedFoldersNamesOnlySharedAndProtected is the rule of the
// startup notice: a folder is named only when it BOTH holds shared data and no
// longer inherits its permissions. A folder holding only the secrets may be
// one its owner restricted on purpose, and is not what the notice is about;
// a shared folder that still inherits was never touched.
//
// Mutants: drop either condition -> the "dedicated, protected" or the "shared,
// inheriting" file's folder is named; treat an unreadable ACL as protected ->
// "unreadable" is named; drop the dedupe -> the default install, with both
// files in one folder, is told twice.
func TestUninheritedSharedFoldersNamesOnlySharedAndProtected(t *testing.T) {
	root := t.TempDir()
	dir := func(name string) string { return filepath.Join(root, name) }
	shared := map[string]bool{dir("install"): true, dir("inheriting"): true, dir("unreadable"): true}
	protected := map[string]bool{dir("install"): true, dir("secrets"): true}
	isShared := func(d string) bool { return shared[d] }
	isProtected := func(d string) (bool, error) {
		if d == dir("unreadable") {
			return true, errors.New("access denied")
		}
		return protected[d], nil
	}

	got := uninheritedSharedFolders([]string{
		filepath.Join(dir("install"), "cookies.txt"),
		filepath.Join(dir("install"), "config.toml"),
		filepath.Join(dir("secrets"), "cookies.txt"),
		filepath.Join(dir("inheriting"), "config.toml"),
		filepath.Join(dir("unreadable"), "config.toml"),
		filepath.Join(dir("plain"), "config.toml"),
		"",
	}, isShared, isProtected)

	if want := []string{dir("install")}; !reflect.DeepEqual(got, want) {
		t.Errorf("folders named = %q, want %q", got, want)
	}
}

// TestUninheritedSharedFoldersResolvesARelativeCookieFile: the default cookie
// file is "./cookies.txt", and the restore command printed for "." would run
// against whatever folder the operator's shell happens to be in.
func TestUninheritedSharedFoldersResolvesARelativeCookieFile(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	always := func(string) bool { return true }
	protected := func(string) (bool, error) { return true, nil }

	got := uninheritedSharedFolders([]string{"./cookies.txt", filepath.Join(cwd, "config.toml")}, always, protected)

	if len(got) != 1 || !filepath.IsAbs(got[0]) || !sameFolder(got[0], cwd) {
		t.Errorf("folders named = %q, want the working directory %q once, absolute", got, cwd)
	}
}

// TestUninheritedSharedFoldersSkipsADriveRoot: a root has no parent to inherit
// from, so "does not inherit" is no finding there, and the restore command for
// one (`icacls "D:\" /inheritance:e`) would not even parse: `\"` escapes the
// quote.
func TestUninheritedSharedFoldersSkipsADriveRoot(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	always := func(string) bool { return true }
	protected := func(string) (bool, error) { return true, nil }

	got := uninheritedSharedFolders([]string{filepath.Join(root, "cookies.txt")}, always, protected)

	if len(got) != 0 {
		t.Errorf("folders named = %q, want none for a file in the root %q", got, root)
	}
}

// sameFolder compares two spellings of a folder the way the file system does
// (t.TempDir and os.Getwd can differ in a short name or a symlink).
func sameFolder(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && filepath.Clean(ra) == filepath.Clean(rb)
}

// TestLogUninheritedFoldersSaysWhichFolderAndHowToRestoreIt pins the line the
// operator reads: the folder, and a command they can paste.
func TestLogUninheritedFoldersSaysWhichFolderAndHowToRestoreIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Moombox")
	rec := &infoRecorder{}

	logUninheritedFolders(rec, []string{dir})

	want := "this folder does not inherit permissions from its parent; an earlier Moombox version may have restricted it to one account" +
		"dir" + dir + "restore" + `icacls "` + dir + `" /inheritance:e`
	if len(rec.lines) != 1 || rec.lines[0] != want {
		t.Errorf("logged %q\n want one line %q", rec.lines, want)
	}
}
