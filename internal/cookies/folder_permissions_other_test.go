//go:build !windows

package cookies

import (
	"os"
	"testing"
)

// folderPermissions reads a folder's permissions back from the file system in
// this platform's own terms: the mode bits.
func folderPermissions(t *testing.T, dir string) string {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm().String()
}

// folderIsOwnerOnly reports whether dir reads back as mode 0700.
func folderIsOwnerOnly(t *testing.T, dir string) bool {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm() == 0o700
}
